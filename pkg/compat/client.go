// Package compat 实现 OpenAI 兼容上游的通用客户端：目录缓存、鉴权头注入、
// 流式首行错误探测，以及按"渠道+模型"粒度的冷却联动。
// keyfree（匿名）与 keyed（用户自带 Key）都基于它，两条渠道的差异收敛到 Config 里。
package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/common"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

var (
	_ provider.Chat            = (*Client)(nil)
	_ provider.ErrorClassifier = (*Client)(nil)
)

const (
	catalogTTL     = 6 * time.Hour
	catalogFailTTL = 5 * time.Minute
	// catalogTimeout 只管目录拉取。聊天流可能持续数分钟，共享 client 的
	// 10min 超时没问题；但目录请求要是也挂 10 分钟，看板刷新就会一直转圈。
	catalogTimeout = 30 * time.Second
)

// Config 描述一个 OpenAI 兼容上游。除 Name 外必须给出 Enabled：
// 免登录渠道会自发真实流量，没写开关就按关闭处理。
type Config struct {
	Name    string // 渠道名，同时是健康度与 owned_by 的取值
	Version string // 仅用于 User-Agent

	Enabled func() bool // nil 表示关闭（失败关闭）
	// BaseURL 返回规范化前的地址；留空则用 DefaultBaseURL。
	BaseURL        func() string
	DefaultBaseURL string
	// APIKey 返回上游密钥；返回空串则不带 Authorization。
	APIKey func() string
	// Allow 判定目录里的模型是否对外可见；nil 表示全部可见。
	// 作用于完整目录（CatalogIDs / ModelsAll）与可见名单，用于"永久不该出现"的
	// 模型（如手动屏蔽名单）——这些模型探针也看不到。
	Allow func(modelID string) bool
	// Visible 只收缩"对外可见名单"（ListModels / ModelIDs / Supports），
	// 不影响 CatalogIDs / ModelsAll。用于"暂时不可用但仍需被探针回采复活"的
	// 过滤维度（如 free_only 按健康度隐藏付费墙模型）：这类模型从派单名单消失，
	// 但完整目录里仍在，探针能确认它复活后自动回到可见名单。
	Visible func(modelID string) bool
	// Floor 在目录拉不到且无缓存时兜底，避免上游目录端点抖动导致渠道整体消失。
	Floor func() []string

	// Health 只用于"读"：可见名单会过滤掉冷却中的模型。
	// 失败与恢复由 pkg/route 统一记录，避免一次请求被计两次而把冷却越拉越长。
	Health *health.Registry

	// AllowLocal 放行 http 与 loopback/内网地址：freepool 这类渠道目的就是
	// 连管理员主动配置的本地聚合代理（9Router/FreeLLMAPI），公网 SSRF 闸门不适用。
	AllowLocal bool
}

// Client 实现 provider.Chat 与 provider.Keyless 所需的方法面。
type Client struct {
	cfg        Config
	httpClient *http.Client

	mu          sync.Mutex
	catalog     []joycode.ModelInfo
	catalogAt   time.Time
	catalogFail time.Time
}

func New(cfg Config) *Client {
	redirectGuard := func(req *http.Request, _ []*http.Request) error { return common.GuardPublicHTTPS(req.URL) }
	if cfg.AllowLocal {
		redirectGuard = func(*http.Request, []*http.Request) error { return nil }
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:       10 * time.Minute,
			CheckRedirect: redirectGuard,
		},
	}
}

func (c *Client) Name() string { return c.cfg.Name }

// Enabled 每次调用都读设置，因此开关不需要重启进程。
func (c *Client) Enabled() bool {
	if c == nil || c.cfg.Enabled == nil {
		return false
	}
	return c.cfg.Enabled()
}

func (c *Client) baseURL() string {
	if c.cfg.BaseURL != nil {
		if v := strings.TrimSpace(c.cfg.BaseURL()); v != "" {
			norm := common.NormalizeBaseURL
			if c.cfg.AllowLocal {
				norm = common.NormalizeBaseURLLocal
			}
			normalized, err := norm(v)
			if err != nil {
				slog.Warn("compat: base_url 无效，使用内置地址", "provider", c.cfg.Name, "error", err)
				return c.cfg.DefaultBaseURL
			}
			return normalized
		}
	}
	return c.cfg.DefaultBaseURL
}

func (c *Client) apiKey() string {
	if c.cfg.APIKey == nil {
		return ""
	}
	return strings.TrimSpace(c.cfg.APIKey())
}

func (c *Client) userAgent() string {
	if c.cfg.Version != "" {
		return "XingyunAPI/" + c.cfg.Version
	}
	return "XingyunAPI"
}

func (c *Client) newRequest(method, url string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	// 401/403 时分不清是地址填错还是 Key 不对，出站 URL 是唯一能定位的线索。
	slog.Debug("compat: 出站请求", "provider", c.cfg.Name, "method", method, "url", url)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
	if key := c.apiKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

// Supports 只回答"这个模型归不归本渠道"，不看冷却：绕不绕开冷却中的模型由 pkg/route 决定。
// 若在这里返回 false，上层会把它当成未知模型静默换成默认付费模型，比报错更糟。
func (c *Client) Supports(model string) bool {
	if !c.Enabled() || model == "" {
		return false
	}
	ids, _ := c.cachedIDs()
	for _, m := range c.filterAllowed(ids) {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}

// ModelIDs 返回此刻对外可见（含健康度过滤）的模型名。
func (c *Client) ModelIDs() []string {
	models, _ := c.ListModels()
	return idsOf(models)
}

// CatalogIDs 返回目录里的全部模型名，不做健康度过滤。
// 派单用可见名单即可，但看板的"谁在冷却、冷却到几点"和探针的复活确认
// 都必须看得见被冷却挡住的那些模型。
func (c *Client) CatalogIDs() []string {
	if c == nil {
		return nil
	}
	ids, _ := c.cachedIDs()
	return c.filterAllowed(ids)
}

// ListModels 返回对外可见的模型；渠道关闭时返回空名单而不是 nil，便于调用方直接 append。
func (c *Client) ListModels() ([]joycode.ModelInfo, error) {
	return c.visibleCatalog(), nil
}

func (c *Client) Chat(body map[string]interface{}) (map[string]interface{}, error) {
	model, _ := body["model"].(string)
	compressMessages(body)
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(http.MethodPost, c.baseURL()+"/chat/completions", data)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Error("compat: request failed", "provider", c.cfg.Name, "model", model, "error", err)
		return nil, c.fail(err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, c.failResp(resp, "读取响应失败: "+err.Error())
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, c.failResp(resp, Snippet(raw))
	}
	if msg, bad := UpstreamError(resp.StatusCode, result); bad {
		return nil, c.failResp(resp, msg)
	}
	return result, nil
}

// upstreamError 保留状态码与上游原文，供 ClassifyError 还原档位。
// retryAfter 是上游 Retry-After 头的建议时长（0 表示没给），路由冷却时优先信它。
type upstreamError struct {
	providerName string
	status       int
	detail       string
	retryAfter   time.Duration
}

func (e *upstreamError) Error() string {
	if e.status <= 0 {
		return fmt.Sprintf("%s 渠道: %s", e.providerName, e.detail)
	}
	return fmt.Sprintf("%s 渠道 HTTP %d: %s", e.providerName, e.status, e.detail)
}

// RetryAfter 实现 health.RetryAfter 读取的接口。
func (e *upstreamError) RetryAfter() time.Duration {
	if e.retryAfter < 0 {
		return 0
	}
	return e.retryAfter
}

func (c *Client) fail(detail string) error {
	return &upstreamError{providerName: c.cfg.Name, detail: detail}
}

// failResp 按响应构造带状态码的错误，顺手把 Retry-After 读走：
// 不少上游在 429 里明确写了配额何时恢复，用它比按固定档位猜准。
func (c *Client) failResp(resp *http.Response, detail string) error {
	return &upstreamError{
		providerName: c.cfg.Name,
		status:       resp.StatusCode,
		detail:       detail,
		retryAfter:   health.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

// ClassifyError 实现 provider.ErrorClassifier。不是自家错误时按文本兜底，
// 保证目录/网络类失败也能选对冷却档位。
func (c *Client) ClassifyError(err error) (int, health.Class, string) {
	if err == nil {
		return 0, health.ClassOK, ""
	}
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue.status, health.Classify(ue.status, ue.detail), ue.detail
	}
	msg := err.Error()
	return 0, health.Classify(0, msg), msg
}

// ChatStream 返回可直接逐行转发的 SSE 响应；非 200 与"200 带 JSON 错误体"都转成 error，
// 这样上层还能在 HTTP 头提交之前换渠道重试。
func (c *Client) ChatStream(body map[string]interface{}) (*http.Response, error) {
	model, _ := body["model"].(string)
	compressMessages(body)
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(http.MethodPost, c.baseURL()+"/chat/completions", data)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Error("compat: stream connect failed", "provider", c.cfg.Name, "model", model, "error", err)
		return nil, c.fail(err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return nil, c.failResp(resp, Snippet(raw))
	}
	// 有些兼容上游会把错误包成 HTTP 200 + JSON body，先探一行再决定能不能当流用。
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	first, lerr := br.ReadString('\n')
	trimmed := strings.TrimSpace(first)
	if trimmed != "" && !strings.HasPrefix(trimmed, "data:") && !strings.HasPrefix(trimmed, ":") && LooksLikeError(trimmed) {
		rest, _ := io.ReadAll(br)
		resp.Body.Close()
		return nil, c.failResp(resp, Snippet([]byte(trimmed+string(rest))))
	}
	if lerr != nil && lerr != io.EOF {
		resp.Body.Close()
		return nil, c.fail("读取首行失败: " + lerr.Error())
	}
	resp.Body = &replayReader{first: []byte(first), source: br, closer: resp.Body}
	return resp, nil
}

type replayReader struct {
	first  []byte
	source *bufio.Reader
	closer io.Closer
	done   bool
}

func (r *replayReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		if len(r.first) > 0 {
			return copy(p, r.first), nil
		}
	}
	return r.source.Read(p)
}

func (r *replayReader) Close() error { return r.closer.Close() }

// visibleCatalog = 上游实时目录经 Allow + Visible 过滤，再扣掉冷却中的模型。
// Visible 只影响这里（对外可见），不影响 CatalogIDs / ModelsAll——后者保留
// free_only 暂时隐藏的模型，供探针回采复活。
func (c *Client) visibleCatalog() []joycode.ModelInfo {
	ids, err := c.cachedIDs()
	if err != nil {
		slog.Warn("compat: 目录不可用", "provider", c.cfg.Name, "error", err)
	}
	out := make([]joycode.ModelInfo, 0, len(ids))
	for _, id := range ids {
		if c.cfg.Visible != nil && !c.cfg.Visible(id) {
			continue
		}
		if !c.available(id) {
			continue
		}
		out = append(out, joycode.ModelInfo{
			Label: id, ModelID: id, ChatAPIModel: id,
			SupportStream: true, Provider: c.cfg.Name, VerificationStatus: "verified",
		})
	}
	return out
}

// cachedIDs 带正/负两级缓存：目录成功 6h 内不重复拉，失败 5min 内不重试，
// 避免上游目录端点抖动时被我们自己的轮询打死。退避期内有旧目录回旧目录、
// 没有才回地板名单——两种情况都如实带错误，让上层知道这是降级名单。
func (c *Client) cachedIDs() ([]string, error) {
	c.mu.Lock()
	now := time.Now()
	if c.catalog != nil && now.Sub(c.catalogAt) < catalogTTL {
		ids := idsOf(c.catalog)
		c.mu.Unlock()
		return ids, nil
	}
	if !c.catalogFail.IsZero() && now.Sub(c.catalogFail) < catalogFailTTL {
		if c.catalog != nil {
			ids := idsOf(c.catalog)
			c.mu.Unlock()
			return ids, fmt.Errorf("目录刷新处于退避窗口，使用上一次成功的名单")
		}
		ids := c.floorIDs()
		c.mu.Unlock()
		return ids, fmt.Errorf("目录刷新处于退避窗口")
	}
	c.mu.Unlock()

	fetched, err := c.fetchCatalog()
	c.mu.Lock()
	if err != nil {
		c.catalogFail = time.Now()
		c.mu.Unlock()
		if c.catalog != nil {
			return idsOf(c.catalog), err
		}
		return c.floorIDs(), err
	}
	allowed := c.filterAllowed(fetched)
	c.catalog = buildInfos(allowed, c.cfg.Name)
	c.catalogAt = time.Now()
	c.catalogFail = time.Time{}
	ids := idsOf(c.catalog)
	c.mu.Unlock()
	return ids, nil
}

func (c *Client) filterAllowed(ids []string) []string {
	if c.cfg.Allow == nil {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if c.cfg.Allow(id) {
			out = append(out, id)
		}
	}
	return out
}

// floorIDs 用于目录拉不到的降级路径：只暴露本地实测过的名单。
func (c *Client) floorIDs() []string {
	if c.cfg.Floor == nil {
		return nil
	}
	return c.filterAllowed(c.cfg.Floor())
}

func (c *Client) fetchCatalog() ([]string, error) {
	ids, err := c.fetchCatalogAt(c.baseURL() + "/models")
	if err == nil {
		return ids, nil
	}
	// base_url 忘写 /v1 是最常见配错：仅在主路径明确 404（路径不存在）时
	// 才补试 {base}/v1/models；5xx/网络错误不重试，避免把抖动的上游打得更死。
	var nf *notFoundError
	if !errors.As(err, &nf) {
		return nil, err
	}
	alt := strings.TrimRight(c.baseURL(), "/") + "/v1/models"
	if ids2, err2 := c.fetchCatalogAt(alt); err2 == nil {
		slog.Info("compat: 目录在 /v1/models 上取得，建议直接把 /v1 写进 base_url", "provider", c.cfg.Name)
		return ids2, nil
	}
	return nil, err
}

// notFoundError 标记"主路径 404"，供 /v1 兜底判断。
type notFoundError struct{ detail string }

func (e *notFoundError) Error() string { return e.detail }

func (c *Client) fetchCatalogAt(url string) ([]string, error) {
	req, err := c.newRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := *c.httpClient
	client.Timeout = catalogTimeout
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, Snippet(raw))
		if resp.StatusCode == http.StatusNotFound {
			return nil, &notFoundError{detail: msg}
		}
		return nil, fmt.Errorf("%s", msg)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if strings.TrimSpace(d.ID) != "" {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("目录为空")
	}
	return ids, nil
}

// RefreshNow 强制作废目录缓存并重拉一次。看板的"刷新渠道"按钮与
// 后台定期刷新都走这里：免费渠道的名单随官方策略变动，不能等满 6h TTL。
func (c *Client) RefreshNow() ([]string, error) {
	c.mu.Lock()
	c.catalog = nil
	c.catalogAt = time.Time{}
	c.catalogFail = time.Time{}
	c.mu.Unlock()
	return c.cachedIDs()
}


// compressMessages 压缩请求体中的 tool_result 内容，减少输入 token。
// 借鉴 9Router RTK：对 git-diff、grep、ls、tree 等输出做去重/截断/折叠。
// 任何错误都 fail-open（原样返回），绝不污染正常请求。
func compressMessages(body map[string]interface{}) {
	msgs, _ := body["messages"].([]interface{})
	if len(msgs) == 0 {
		return
	}
	for _, m := range msgs {
		obj, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := obj["role"].(string)
		if role != "user" {
			continue
		}
		compressContent(obj)
	}
}

func compressContent(obj map[string]interface{}) {
	switch v := obj["content"].(type) {
	case string:
		if compressed := compressText(v); compressed != v {
			obj["content"] = compressed
		}
	case []interface{}:
		for _, item := range v {
			if obj2, ok := item.(map[string]interface{}); ok {
				if typ, _ := obj2["type"].(string); typ == "tool_result" {
					if text, ok := obj2["content"].(string); ok {
						if compressed := compressText(text); compressed != text {
							obj2["content"] = compressed
						}
					}
				}
			}
		}
	}
}

// compressText 对文本内容应用压缩策略，优先匹配输出模式类型。
func compressText(s string) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < 200 {
		return s
	}
	switch {
	case isGitDiff(trimmed):
		return compressGitDiff(trimmed)
	case isGrepOutput(trimmed):
		return compressGrep(trimmed)
	case isLsOutput(trimmed):
		return compressLs(trimmed)
	case isTreeOutput(trimmed):
		return compressTree(trimmed)
	case isDedupLog(trimmed):
		return compressDedupLog(trimmed)
	}
	return smartTruncate(trimmed, 4000)
}

func isGitDiff(s string) bool {
	return strings.Contains(s, "diff --git") ||
		(strings.Contains(s, "@@") && strings.Count(s, "+") > strings.Count(s, "-")*3)
}
func compressGitDiff(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	skipCtx := 0
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git"), strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "@@"):
			skipCtx = 0
			out = append(out, line)
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			skipCtx = 0
			out = append(out, line)
		case strings.HasPrefix(line, "-"):
			skipCtx = 0
			out = append(out, line)
		case strings.HasPrefix(line, " "):
			if skipCtx > 3 {
				continue
			}
			skipCtx++
			out = append(out, line)
		default:
			skipCtx = 0
			out = append(out, line)
		}
	}
	result := strings.Join(out, "\n")
	if len(result) >= len(s) {
		return s
	}
	return result
}

func isGrepOutput(s string) bool {
	return strings.Contains(s, ":") && strings.Count(s, "\n") > 20
}
func compressGrep(s string) string {
	lines := strings.Split(s, "\n")
	seen := make(map[string]int)
	var out []string
	for _, line := range lines {
		seen[line]++
		if seen[line] <= 2 {
			out = append(out, line)
		}
	}
	result := strings.Join(out, "\n")
	if len(result) >= len(s) {
		return s
	}
	return result
}

func isLsOutput(s string) bool {
	lines := strings.Split(s, "\n")
	if len(lines) < 10 {
		return false
	}
	match := 0
	for _, line := range lines {
		if strings.Contains(line, "->") || strings.HasPrefix(line, "total ") {
			match++
		}
	}
	return match > int(float64(len(lines))*0.3)
}
func compressLs(s string) string { return smartTruncate(s, 2000) }

func isTreeOutput(s string) bool {
	return strings.Contains(s, "├") || strings.Contains(s, "│") ||
		(strings.Contains(s, "-") && strings.Contains(s, ">"))
}
func compressTree(s string) string { return smartTruncate(s, 3000) }

func isDedupLog(s string) bool {
	lines := strings.Split(s, "\n")
	if len(lines) < 10 {
		return false
	}
	dups := 0
	for i := 1; i < len(lines); i++ {
		if lines[i] == lines[i-1] {
			dups++
		}
	}
	return dups > int(float64(len(lines))*0.3)
}
func compressDedupLog(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	prev := ""
	for _, line := range lines {
		if line == prev {
			continue
		}
		out = append(out, line)
		prev = line
	}
	result := strings.Join(out, "\n")
	if len(result) >= len(s) {
		return s
	}
	return result
}

func smartTruncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	trunc := s[:maxBytes]
	if idx := strings.LastIndex(trunc, "\n"); idx > maxBytes/2 {
		trunc = trunc[:idx+1]
	}
	return trunc + "\n... [truncated]"
}

func (c *Client) available(model string) bool {
	if c.cfg.Health == nil {
		return true
	}
	return c.cfg.Health.Available(c.cfg.Name, model)
}

func buildInfos(ids []string, providerName string) []joycode.ModelInfo {
	out := make([]joycode.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, joycode.ModelInfo{
			Label: id, ModelID: id, ChatAPIModel: id,
			SupportStream: true, Provider: providerName, VerificationStatus: "verified",
		})
	}
	return out
}

func idsOf(models []joycode.ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ModelID)
	}
	return ids
}

// UpstreamError 识别"HTTP 200 但 body 是错误"的上游，以及常规非 200。
func UpstreamError(status int, result map[string]interface{}) (string, bool) {
	if status != http.StatusOK {
		if e, ok := result["error"]; ok {
			return ErrText(e), true
		}
		return fmt.Sprintf("HTTP %d", status), true
	}
	if _, hasChoices := result["choices"]; hasChoices {
		return "", false
	}
	if e, ok := result["error"]; ok {
		return ErrText(e), true
	}
	return "响应缺少 choices", true
}

func ErrText(e interface{}) string {
	switch v := e.(type) {
	case string:
		return v
	case map[string]interface{}:
		for _, k := range []string{"message", "error_message", "type", "code"} {
			if m, ok := v[k].(string); ok && m != "" {
				return m
			}
		}
	}
	return fmt.Sprint(e)
}

// LooksLikeError 判定一行 JSON 是否是纯错误体。
func LooksLikeError(line string) bool {
	if !strings.HasPrefix(line, "{") {
		return false
	}
	var probe map[string]interface{}
	if json.Unmarshal([]byte(line), &probe) != nil {
		return false
	}
	_, hasError := probe["error"]
	_, hasChoices := probe["choices"]
	return hasError && !hasChoices
}

func Snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
