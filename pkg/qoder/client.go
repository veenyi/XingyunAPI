// client.go Qoder CN 核心客户端：HTTP 连接、ChatStream、token 刷新、配额查询。
package qoder

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// QoderAccount 是运行时账号表示（从 checkin.Account 同步而来）。
type QoderAccount struct {
	UID          string
	Nickname     string
	AccessToken  string // dt-
	RefreshToken string // drt-
	ExpiresAt    int64  // unix 秒
	MachineID    string
	MachineToken string
	MachineType  string
}

// NeedsRefresh 判断 token 是否需要刷新。
func (a *QoderAccount) NeedsRefresh(skew time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(skew).Unix() >= a.ExpiresAt
}

// Client 是 Qoder CN 渠道客户端，实现 provider.Keyless。
type Client struct {
	HTTP    *http.Client
	Base    string // OpenAPIBase
	Gateway string // GatewayBase

	mu       sync.RWMutex
	modelMap map[string]string // 动态 name→key 缓存

	dynCache *dynamicModelsCache

	accountsMu sync.RWMutex
	accounts   []*QoderAccount
}

// New 创建 Qoder 客户端。强制 HTTP/1.1（网关 HTTP/2 有 INTERNAL_ERROR bug）。
func New() *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 180 * time.Second,
			Transport: &http.Transport{
				TLSNextProto:        map[string]func(authority string, c *tls.Conn) http.RoundTripper{},
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		Base:     OpenAPIBase,
		Gateway:  GatewayBase,
		dynCache: &dynamicModelsCache{},
	}
}

func (c *Client) gatewayBase() string { return c.Gateway }
func (c *Client) openapiBase() string { return c.Base }

// Name 返回渠道名（用于模型前缀 "qoder/model"）。
func (c *Client) Name() string { return "qoder" }

// Enabled 当有至少一个活跃账号时启用。
func (c *Client) Enabled() bool {
	c.accountsMu.RLock()
	defer c.accountsMu.RUnlock()
	for _, a := range c.accounts {
		if a.AccessToken != "" {
			return true
		}
	}
	return false
}

// SyncAccounts 从签到中心账号列表同步 Qoder 账号。
func (c *Client) SyncAccounts(accs []*QoderAccount) {
	c.accountsMu.Lock()
	defer c.accountsMu.Unlock()
	c.accounts = accs
}

// ListModels 返回可用模型列表：动态优先，静态兜底。
func (c *Client) ListModels() ([]ModelEntry, error) {
	if entries, ok := c.dynCache.get(); ok {
		return c.entriesToModels(entries), nil
	}

	c.accountsMu.RLock()
	var acct *QoderAccount
	for _, a := range c.accounts {
		if a.AccessToken != "" {
			acct = a
			break
		}
	}
	c.accountsMu.RUnlock()

	if acct == nil {
		return c.staticEntries(), nil
	}

	entries, err := c.FetchModels(acct)
	c.dynCache.set(entries, err)
	if err != nil {
		slog.Warn("qoder: dynamic model fetch failed, using static", "error", err)
		return c.staticEntries(), nil
	}
	return c.entriesToModels(entries), nil
}

// ModelEntry 是 ListModels 返回的模型条目。
type ModelEntry struct {
	ID            string
	ContextWindow int
}

func (c *Client) entriesToModels(entries []DynamicModelEntry) []ModelEntry {
	out := make([]ModelEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, ModelEntry{ID: e.Name, ContextWindow: e.ContextWindow})
	}
	return out
}

func (c *Client) staticEntries() []ModelEntry {
	sm := StaticModels()
	out := make([]ModelEntry, len(sm))
	for i, m := range sm {
		out[i] = ModelEntry{ID: m.ChatAPIModel, ContextWindow: m.MaxTotalTokens}
	}
	return out
}

// Supports 判定该模型是否由 Qoder 渠道提供。
func (c *Client) Supports(model string) bool {
	if k := c.modelKey(model); k != "" {
		return true
	}
	entries, ok := c.dynCache.get()
	if !ok {
		return false
	}
	for _, e := range entries {
		if e.Name == model {
			return true
		}
	}
	return false
}

// Chat 非流式聊天：强制上游流式，本地聚合。
func (c *Client) Chat(body map[string]any) (map[string]any, error) {
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	rc, status, respBody, err := c.doChat(rawBody)
	if err != nil {
		return nil, err
	}
	if rc != nil {
		defer rc.Close()
		model, _ := body["model"].(string)
		return aggregate(rc, model)
	}
	if respBody != nil {
		var result map[string]any
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("upstream http %d: %s", status, truncateStr(string(respBody), 200))
		}
		return result, nil
	}
	return nil, fmt.Errorf("upstream http %d", status)
}

// ChatStream 流式聊天：返回上游 SSE 响应。
func (c *Client) ChatStream(body map[string]any) (*http.Response, error) {
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	rc, status, respBody, err := c.doChat(rawBody)
	if err != nil {
		return nil, err
	}
	if rc != nil {
		return &http.Response{
			StatusCode: status,
			Body:       rc,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
		}, nil
	}
	if respBody != nil {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(string(respBody))),
			Header:     http.Header{"Content-Type": {"application/json"}},
		}, nil
	}
	return nil, fmt.Errorf("upstream http %d", status)
}

// doChat 选一个账号然后发请求（Client 直接调用，无粘性路由）。
func (c *Client) doChat(rawBody []byte) (io.ReadCloser, int, []byte, error) {
	acct := c.pickAccount()
	if acct == nil {
		return nil, 0, nil, fmt.Errorf("no available qoder account")
	}
	return c.doChatForAccount(acct, rawBody)
}

// doChatForAccount 对指定账号执行一次上游请求。
func (c *Client) doChatForAccount(acct *QoderAccount, rawBody []byte) (io.ReadCloser, int, []byte, error) {
	var peek struct {
		Model           string           `json:"model"`
		Messages        []map[string]any `json:"messages"`
		Tools           []any            `json:"tools"`
		ReasoningEffort string           `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(rawBody, &peek); err != nil {
		return nil, 0, nil, err
	}

	modelKey := c.modelKey(peek.Model)
	if modelKey == "" {
		modelKey = peek.Model
	}

	enableReasoning := peek.ReasoningEffort != ""

	agentBody, err := buildAgentBody(peek.Messages, modelKey, peek.Tools, enableReasoning)
	if err != nil {
		return nil, 0, nil, err
	}
	encoded := qoderEncode(agentBody)
	url := c.gatewayBase() + EpChat

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("cosy-user", acct.UID)
	sess, err := NewCosySession(acct.MachineID, acct.MachineToken, acct.MachineType, acct.Nickname, acct.UID, acct.AccessToken, acct.RefreshToken)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, encoded, url, true, modelKey); err != nil {
		return nil, 0, nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}

	return resp.Body, resp.StatusCode, nil, nil
}

// pickAccount 从池中选一个可用账号（简单轮询，pool.go 提供粘性路由）。
func (c *Client) pickAccount() *QoderAccount {
	c.accountsMu.RLock()
	defer c.accountsMu.RUnlock()
	for _, a := range c.accounts {
		if a.AccessToken != "" {
			return a
		}
	}
	return nil
}

// RefreshToken 刷新账号 token。POST /api/v1/deviceToken/refresh。
func RefreshToken(a *QoderAccount) error {
	if a.RefreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	url := OpenAPIBase + EpDTRefresh
	payload := fmt.Sprintf(`{"refresh_token":%q}`, a.RefreshToken)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("session dead: http %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("refresh http %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}

	var tok map[string]any
	if err := json.Unmarshal(raw, &tok); err != nil {
		return err
	}

	newToken := getStr(tok, "token")
	if newToken == "" {
		newToken = getStr(tok, "device_token")
	}
	if newToken == "" {
		return fmt.Errorf("no token in refresh response")
	}
	newRefresh := getStr(tok, "refresh_token")
	if newRefresh != "" {
		a.RefreshToken = newRefresh
	}
	a.AccessToken = newToken

	if ea, ok := tok["expires_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, ea); err == nil {
			a.ExpiresAt = t.Unix()
		}
	} else if ei, ok := tok["expires_in"].(float64); ok {
		// expires_in 单位是毫秒
		a.ExpiresAt = time.Now().Add(time.Duration(ei) * time.Millisecond).Unix()
	} else {
		a.ExpiresAt = time.Now().Add(30 * 24 * time.Hour).Unix()
	}

	slog.Info("qoder: token refreshed", "uid", a.UID)
	return nil
}

// UserResource 查询账号余额。返回 (剩余积分, 总积分, error)。
func UserResource(a *QoderAccount) (remain, total int64, err error) {
	url := OpenAPIBase + EpQuotaUsage
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return 0, 0, fmt.Errorf("http %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	slog.Info("qoder: UserResource raw response", "uid", a.UID, "status", resp.StatusCode, "body", truncateStr(string(raw), 500))

	var data struct {
		UserQuota struct {
			Remaining float64 `json:"remaining"`
			Total     float64 `json:"total"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Remaining float64 `json:"remaining"`
			Total     float64 `json:"total"`
		} `json:"addOnQuota"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return 0, 0, err
	}
	remain = int64(data.UserQuota.Remaining) + int64(data.AddOnQuota.Remaining)
	total = int64(data.UserQuota.Total) + int64(data.AddOnQuota.Total)
	return remain, total, nil
}

// UserResourceF64 查询账号余额，保留小数精度。
func UserResourceF64(a *QoderAccount) (remain, total float64, err error) {
	url := OpenAPIBase + EpQuotaUsage
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return 0, 0, fmt.Errorf("http %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var data struct {
		UserQuota struct {
			Remaining float64 `json:"remaining"`
			Total     float64 `json:"total"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Remaining float64 `json:"remaining"`
			Total     float64 `json:"total"`
		} `json:"addOnQuota"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return 0, 0, err
	}
	remain = data.UserQuota.Remaining + data.AddOnQuota.Remaining
	total = data.UserQuota.Total + data.AddOnQuota.Total
	return remain, total, nil
}

// EnsureFingerprint 为账号生成持久机器指纹（幂等）。
func EnsureFingerprint(a *QoderAccount) {
	if a.MachineID == "" {
		a.MachineID = uuid4()
	}
	if a.MachineToken == "" {
		raw := uuid4() + uuid4()
		a.MachineToken = base64.RawURLEncoding.EncodeToString([]byte(raw))[:50]
	}
	if a.MachineType == "" {
		id := uuid4()
		id = strings.ReplaceAll(id, "-", "")
		if len(id) > 18 {
			id = id[:18]
		}
		a.MachineType = id
	}
}

// Classify 将上游 HTTP 状态码 + 响应体分类为错误类型。
func Classify(status int, body string) ErrKind {
	if status == 402 {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, m) {
			return ErrHardCredit
		}
	}
	if status == 401 {
		return ErrSessionDead
	}
	if status == 429 {
		return ErrSoftRate
	}
	if status == 404 {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// ErrKind 错误分类。
type ErrKind int

const (
	ErrNone        ErrKind = iota
	ErrHardCredit          // 余额/权益不足 → 长冷却
	ErrSoftRate            // 429 → 短冷却
	ErrSessionDead         // 登录态失效 → 禁用
	ErrNotFound            // 404 → 短冷却
	ErrServer              // 5xx
	ErrClient              // 其他 4xx
)

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "isquotaexceeded\":true",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// StreamUpstream 将上游 SSE 流直接转写为 OpenAI SSE 响应。
func StreamUpstream(w http.ResponseWriter, r io.Reader, model string) error {
	return Stream(w, r, model)
}

// AggregateUpstream 将上游 SSE 流聚合为 OpenAI completion。
func AggregateUpstream(r io.Reader, model string) (map[string]any, error) {
	return aggregate(r, model)
}

func getStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
