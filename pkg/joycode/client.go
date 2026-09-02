package joycode

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/common"
)

const (
	DefaultModel  = "JoyAI-Code-1.5"
	ClientVersion = "2.7.5"
	UserAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/133.0.0.0 Electron/35.2.0 Safari/537.36"

	// color gateway 签名（逆向自 JoyCode 2.7.5 / joycoder-editor 3.8.57）
	colorGatewayAppID = "joycode_ide"
	colorGatewayPath  = "/api"
	colorHMACKey      = "0691a3f0b37b4a85aeb63ad0fc7db3ed"
)

var (
	BaseURL             = envOr("JOYCODE_BASE_URL", "https://joycode-api.jd.com")
	SaasBaseURL         = envOr("JOYCODE_SAAS_BASE_URL", "http://joycode-api-saas.jd.com")
	DefaultColorBaseURL = envOr("JOYCODE_COLOR_BASE_URL", "https://api-ai.jd.com")
)

// colorEndpoint 把旧 v1 路径映射到 (functionId, v2 路径)。
// gateway 模式靠 query 的 functionId 路由；direct 模式用 v2 路径。
type colorEndpoint struct {
	functionID string
	v2Path     string
}

var colorEndpoints = map[string]colorEndpoint{
	"/api/saas/openai/v1/chat/completions": {"chat_completions", "/api/saas/openai/v2/chat/completions"},
	"/api/saas/models/v1/modelList":        {"joycode_modelList", "/api/saas/models/v2/modelList"},
	"/api/saas/openai/v1/web-search":       {"web_search", "/api/saas/openai/v2/web-search"},
	"/api/saas/user/v1/userInfo":           {"joycode_userInfo", "/api/saas/user/v2/userInfo"},
	"/api/saas/anthropic/v1/messages":      {"anthropic_completions", "/api/saas/anthropic/v1/messages"},
	"/api/saas/point/v1/getNewIdePoint":    {"get_newpoint_ide", "/api/saas/point/v1/getNewIdePoint"},
	"/api/saas/model-runtime/v1/models/prepare": {"model_runtime_prepare", "/api/saas/model-runtime/v1/models/prepare"},
}

// Models 为 fallback 模型列表（上游 modelList 失败时使用），与官方模型管理一致。
var Models = []string{
	"JoyAI-Code-1.5",
	"MiniMax-M3",
	"MiniMax-M2.7",
	"Kimi-K2.6",
	"GLM-5.1",
	"GLM-5",
	"DeepSeek-V4-Pro",
	"Doubao-Seed-2.0-pro",
	"Claude-Opus-4.7",
}

// ChatModelMapping 把用户可见的模型 label 映射为上游实际接受的 chatApiModel。
// 上游 modelList 返回的 chatApiModel 才是 chat 接口真正接受的模型名；
// 用 label（如 MiniMax-M3）直接调用会得到 9003 "模型不在套餐"。
// 官方客户端 createMessage 中 model: m.chatApiModel，此处保持一致。
var ChatModelMapping = map[string]string{
	"JoyAI-Code-1.5":     "JoyAI-Code-1.5",
	"MiniMax-M3":         "MiniMax-M3-agent",
	"MiniMax-M2.7":       "MiniMax-M2.7-agent",
	"Kimi-K2.6":          "Kimi-K2.6-agent",
	"GLM-5.3":            "GLM-5.3-agent",
	"GLM-5.1":            "GLM-5.1-agent",
	"GLM-5":              "GLM-5-agent",
	"DeepSeek-V4-Pro":    "DeepSeek-V4-Pro-agent",
	"Doubao-Seed-2.0-pro": "Doubao-Seed-2.0-pro-agent",
}

// ResolveChatModel 把用户请求的模型名映射为上游 chatApiModel。
// 已知 label 返回映射值；未知模型原样透传（可能是自定义/新模型）。
func ResolveChatModel(model string) string {
	if v, ok := ChatModelMapping[model]; ok {
		return v
	}
	return model
}

type Client struct {
	PtKey          string
	AnthropicPtKey string
	UserID         string
	SessionID      string
	ColorBaseURL   string
	MasterBaseURL  string
	Tenant         string
	LoginType      string
	OrgFullName    string
	ModelQueueToken string
	httpClient     *http.Client
}

type gzipReadCloser struct {
	io.Reader
	body io.Closer
	gzip io.Closer
}

func (r *gzipReadCloser) Close() error {
	gzipErr := r.gzip.Close()
	bodyErr := r.body.Close()
	if gzipErr != nil {
		return gzipErr
	}
	return bodyErr
}

// defaultTransport is a shared transport with sane connection-pool defaults
// so that clients created without an explicit transport still reuse TCP
// connections instead of dialing anew for every request.
var defaultTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     90 * time.Second,
}

// envOr 读取环境变量，为空则返回 fallback
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func NewClient(ptKey, userID string) *Client {
	return &Client{
		PtKey:        ptKey,
		UserID:       userID,
		SessionID:    newHexID(),
		ColorBaseURL: DefaultColorBaseURL,
		httpClient: &http.Client{
			Timeout:   30 * time.Minute,
			Transport: defaultTransport,
		},
	}
}

// SetHTTPClient replaces the internal HTTP client. Intended for testing.
func (c *Client) SetHTTPClient(hc *http.Client) {
	c.httpClient = hc
}

func (c *Client) SetTimeout(d time.Duration) {
	c.httpClient.Timeout = d
}

func (c *Client) SetTransport(transport http.RoundTripper) {
	c.httpClient.Transport = transport
}

func (c *Client) SetAnthropicPtKey(ptKey string) {
	c.AnthropicPtKey = ptKey
}

// SetColorContext sets the color-gateway routing context from login credentials.
// Empty colorBaseURL keeps the default gateway origin.
func (c *Client) SetColorContext(colorBaseURL, masterBaseURL, tenant, loginType, orgFullName string) {
	if colorBaseURL != "" {
		c.ColorBaseURL = colorBaseURL
	}
	c.MasterBaseURL = masterBaseURL
	c.Tenant = tenant
	c.LoginType = loginType
	c.OrgFullName = orgFullName
}

func newHexID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// colorSign 构造 color gateway 的 query 串与 HMAC 签名。
// 规范串 = 参数按 key 排序后的 value 拼接（appid < functionId < t）。
func colorSign(functionID string) (query, sign string) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	signStr := colorGatewayAppID + "&" + functionID + "&" + ts
	mac := hmac.New(sha256.New, []byte(colorHMACKey))
	mac.Write([]byte(signStr))
	sign = hex.EncodeToString(mac.Sum(nil))
	query = "appid=" + colorGatewayAppID + "&functionId=" + functionID + "&t=" + ts
	return query, sign
}

// requestURL 根据登录态把端点解析为最终请求 URL。
// 有 colorBaseURL → gateway 模式（带签名，functionId 路由）；否则 direct v2（无签名）。
func (c *Client) requestURL(endpoint string) string {
	ep, ok := colorEndpoints[endpoint]
	if !ok {
		// 未在 color 端点表中的旧端点（如已下线的 rerank），保持 direct 行为
		return BaseURL + endpoint
	}
	if c.ColorBaseURL != "" {
		if u, err := url.Parse(c.ColorBaseURL); err == nil && u.Host != "" {
			basePath := strings.TrimRight(u.Path, "/")
			query, sign := colorSign(ep.functionID)
			return u.Scheme + "://" + u.Host + basePath + colorGatewayPath + "?" + query + "&sign=" + sign
		}
	}
	base := c.MasterBaseURL
	if base == "" {
		base = BaseURL
	}
	return strings.TrimRight(base, "/") + ep.v2Path
}

func (c *Client) headers() http.Header {
	loginType := c.LoginType
	if loginType == "" {
		loginType = "N_PIN_PC"
	}
	h := http.Header{
		"Content-Type":    {"application/json; charset=UTF-8"},
		"source-type":     {"joycoder-ide"},
		"ptKey":           {c.PtKey},
		"loginType":       {loginType},
		"User-Agent":      {UserAgent},
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate"},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
	// 官方客户端会在请求头携带 tenant（URL 编码）。
	// 注意：X-Model-Token 在"bypass"模式下（token 为 mt_ready_bypass.* 前缀，
	// 即模型免排队直接放行）官方客户端【不发送】该头，发送反而会被上游拒绝
	// （MODEL_TOKEN_INVALID）。实测只有真正排队获取的 token 才需要带。
	if c.Tenant != "" {
		h.Set("tenant", url.QueryEscape(c.Tenant))
	}
	if c.ModelQueueToken != "" && !strings.HasPrefix(strings.ToLower(c.ModelQueueToken), "mt_ready_bypass") {
		h.Set("X-Model-Token", c.ModelQueueToken)
	}
	if c.SessionID != "" {
		// 官方客户端 header 名为 x-ms-client-request-id，格式 task-{id}_session-{id}_{timestamp}
		reqID := "task-" + c.SessionID + "_session-" + c.SessionID + "_" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		h.Set("x-ms-client-request-id", reqID)
	}
	return h
}

func (c *Client) anthropicHeaders() http.Header {
	ptKey := c.PtKey
	if c.AnthropicPtKey != "" {
		ptKey = c.AnthropicPtKey
	}
	loginType := c.LoginType
	if loginType == "" {
		loginType = "PIN_JD_CLOUD"
	}
	h := http.Header{
		"Content-Type":    {"application/json; charset=utf-8"},
		"source-type":     {"joycoder-ide"},
		"ptKey":           {ptKey},
		"loginType":       {loginType},
		"User-Agent":      {UserAgent},
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate"},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
	if c.Tenant != "" {
		h.Set("tenant", url.QueryEscape(c.Tenant))
	}
	if c.ModelQueueToken != "" && !strings.HasPrefix(strings.ToLower(c.ModelQueueToken), "mt_ready_bypass") {
		h.Set("X-Model-Token", c.ModelQueueToken)
	}
	if c.SessionID != "" {
		// 官方客户端 header 名为 x-ms-client-request-id，格式 task-{id}_session-{id}_{timestamp}
		reqID := "task-" + c.SessionID + "_session-" + c.SessionID + "_" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		h.Set("x-ms-client-request-id", reqID)
	}
	return h
}

func (c *Client) prepareBody(extra map[string]interface{}) map[string]interface{} {
	tenant := c.Tenant
	if tenant == "" {
		tenant = "JOYCODE"
	}
	body := map[string]interface{}{
		"tenant":        tenant,
		"orgFullName":   c.OrgFullName,
		"userId":        c.UserID,
		"client":        "JoyCode",
		"clientVersion": ClientVersion,
		"language":      "UNKNOWN",
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (c *Client) prepareAnthropicBody(extra map[string]interface{}) map[string]interface{} {
	tenant := c.Tenant
	if tenant == "" {
		tenant = "JD"
	}
	body := map[string]interface{}{
		"tenant":        tenant,
		"orgFullName":   c.OrgFullName,
		"userId":        c.UserID,
		"client":        "JoyCode",
		"clientVersion": ClientVersion,
		"language":      "UNKNOWN",
		"stream":        true,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (c *Client) doPost(endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("marshal request body", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req, err := http.NewRequest("POST", c.requestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		slog.Error("create request", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req.Header = c.headers()
	return c.httpClient.Do(req)
}

// doPostStream is like doPost but disables Accept-Encoding: gzip so the
// upstream returns raw (uncompressed) SSE. gzip.Reader buffers an entire
// gzip block before yielding any bytes, which breaks chunk-by-chunk
// streaming — the client sees all data arrive at once after a long delay.
func (c *Client) doPostStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("marshal stream request body", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req, err := http.NewRequest("POST", c.requestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		slog.Error("create stream request", "endpoint", endpoint, "error", err)
		return nil, err
	}
	h := c.headers()
	h.Set("Accept-Encoding", "identity") // no gzip for streaming
	req.Header = h
	return c.httpClient.Do(req)
}

func (c *Client) doAnthropicPost(endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("marshal anthropic request body", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req, err := http.NewRequest("POST", c.requestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		slog.Error("create anthropic request", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req.Header = c.anthropicHeaders()
	return c.httpClient.Do(req)
}

// doAnthropicPostStream is like doAnthropicPost but disables gzip for the
// same reason as doPostStream — see its comment for details.
func (c *Client) doAnthropicPostStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("marshal anthropic stream request body", "endpoint", endpoint, "error", err)
		return nil, err
	}
	req, err := http.NewRequest("POST", c.requestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		slog.Error("create anthropic stream request", "endpoint", endpoint, "error", err)
		return nil, err
	}
	h := c.anthropicHeaders()
	h.Set("Accept-Encoding", "identity")
	req.Header = h
	return c.httpClient.Do(req)
}

func decodeBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var r io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	return io.ReadAll(r)
}

func decodeStreamBody(resp *http.Response) error {
	if resp.Header.Get("Content-Encoding") != "gzip" {
		return nil
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	resp.Body = &gzipReadCloser{Reader: gz, body: resp.Body, gzip: gz}
	resp.Header.Del("Content-Encoding")
	return nil
}

func (c *Client) Post(endpoint string, body map[string]interface{}) (map[string]interface{}, error) {
	// 发送聊天前准备模型运行时（与 PostStream 一致）。跳过 prepare 端点自身。
	// 模型名需映射为 chatApiModel（label 如 MiniMax-M3 → MiniMax-M3-agent）。
	if endpoint != "/api/saas/model-runtime/v1/models/prepare" {
		if model, ok := body["model"].(string); ok && model != "" {
			body["model"] = ResolveChatModel(model)
			_ = c.EnsureModelReady(body["model"].(string))
		}
	}
	resp, err := c.doPost(endpoint, c.prepareBody(body))
	if err != nil {
		slog.Error("upstream request failed", "endpoint", endpoint, "error", err)
		return nil, err
	}
	data, err := decodeBody(resp)
	if err != nil {
		slog.Error("decode upstream response", "endpoint", endpoint, "status", resp.StatusCode, "error", err)
		return nil, err
	}
	if resp.StatusCode != 200 {
		slog.Error("upstream non-200", "endpoint", endpoint, "status", resp.StatusCode, "body", truncate(string(data), 500))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		slog.Error("unmarshal upstream response", "endpoint", endpoint, "error", err)
		return nil, fmt.Errorf("invalid JSON response (parse error: %s): %s", err.Error(), truncate(string(data), 500))
	}
	return result, nil
}

func (c *Client) PostStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	// 发送聊天前准备模型运行时（与官方客户端一致）。跳过 prepare 端点自身。
	// 模型名需映射为 chatApiModel；PrepareModel 返回的 mt_ready_bypass token
	// 按官方逻辑不随请求头发送（见 headers()），因此这里拿 token 主要用于保活判断。
	if endpoint != "/api/saas/model-runtime/v1/models/prepare" {
		if model, ok := body["model"].(string); ok && model != "" {
			body["model"] = ResolveChatModel(model)
			_ = c.EnsureModelReady(body["model"].(string))
		}
	}
	resp, err := c.doPostStream(endpoint, c.prepareBody(body))
	if err != nil {
		slog.Error("upstream stream connect", "endpoint", endpoint, "error", err)
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		slog.Error("upstream stream non-200", "endpoint", endpoint, "status", resp.StatusCode, "body", truncate(string(data), 500))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	if err := decodeStreamBody(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	// 上游偶发返回 200 + 纯 JSON 错误体（非 SSE data: 行），如
	// {"error":{"code":"AI_GRAY_ACCESS_DENIED",...}}。此时流式解析会把它当
	// 普通行跳过，导致请求"静默返回空"。这里 peek 首行识别并转为 error，
	// 让上层（PostStreamWithActivation 等）能触发自动激活与重试。
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	firstLine, lerr := br.ReadString('\n')
	if lerr != nil && lerr != io.EOF {
		resp.Body.Close()
		return nil, fmt.Errorf("read stream first line: %w", lerr)
	}
	trimmed := strings.TrimSpace(firstLine)
	if trimmed != "" && !strings.HasPrefix(trimmed, "data:") && IsErrorBody(trimmed) {
		rest, _ := io.ReadAll(br)
		resp.Body.Close()
		return nil, fmt.Errorf("upstream error: %s", truncate(trimmed+string(rest), 500))
	}
	// 正常：回放首行，保持流式逐行读取
	resp.Body = &replayReader{first: []byte(firstLine), source: br, body: resp.Body}
	return resp, nil
}

func (c *Client) PostAnthropicStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	// 与 PostStream 一致：模型名映射为 chatApiModel + 发送前 prepare。
	if endpoint != "/api/saas/model-runtime/v1/models/prepare" {
		if model, ok := body["model"].(string); ok && model != "" {
			body["model"] = ResolveChatModel(model)
			_ = c.EnsureModelReady(body["model"].(string))
		}
	}
	resp, err := c.doAnthropicPostStream(endpoint, c.prepareAnthropicBody(body))
	if err != nil {
		slog.Error("upstream anthropic stream connect", "endpoint", endpoint, "error", err)
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		slog.Error("upstream anthropic stream non-200", "endpoint", endpoint, "status", resp.StatusCode, "body", truncate(string(data), 500))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	if err := decodeStreamBody(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	// 同 PostStream：识别 200 + 纯 JSON 错误体（非 SSE data: 行）
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	firstLine, lerr := br.ReadString('\n')
	if lerr != nil && lerr != io.EOF {
		resp.Body.Close()
		return nil, fmt.Errorf("read anthropic stream first line: %w", lerr)
	}
	trimmed := strings.TrimSpace(firstLine)
	if trimmed != "" && !strings.HasPrefix(trimmed, "data:") && IsErrorBody(trimmed) {
		rest, _ := io.ReadAll(br)
		resp.Body.Close()
		return nil, fmt.Errorf("upstream error: %s", truncate(trimmed+string(rest), 500))
	}
	resp.Body = &replayReader{first: []byte(firstLine), source: br, body: resp.Body}
	return resp, nil
}

func (c *Client) ListModels() ([]ModelInfo, error) {
	resp, err := c.Post("/api/saas/models/v1/modelList", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	data, ok := resp["data"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected models response format: missing data array")
	}
	models := make([]ModelInfo, 0, len(data))
	for _, item := range data {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var m ModelInfo
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		models = append(models, m)
	}
	return models, nil
}

func (c *Client) WebSearch(query string) ([]interface{}, error) {
	body := map[string]interface{}{
		"messages": []map[string]string{{"role": "user", "content": query}},
		"stream":   false, "model": "search_pro_jina", "language": "UNKNOWN",
	}
	resp, err := c.Post("/api/saas/openai/v1/web-search", body)
	if err != nil {
		return nil, err
	}
	results, _ := resp["search_result"].([]interface{})
	return results, nil
}

func (c *Client) Rerank(query string, documents []string, topN int) (map[string]interface{}, error) {
	return c.Post("/api/saas/openai/v1/rerank", map[string]interface{}{
		"model": "Qwen3-Reranker-8B", "query": query,
		"documents": documents, "top_n": topN,
	})
}

func (c *Client) UserInfo() (map[string]interface{}, error) {
	return c.Post("/api/saas/user/v1/userInfo", map[string]interface{}{})
}

// GetPoint 获取账号积分/套餐用量信息（getNewIdePoint）。
// 返回上游原始响应，data.usageItems 含 used/total/remain 等字段。
func (c *Client) GetPoint() (map[string]interface{}, error) {
	return c.Post("/api/saas/point/v1/getNewIdePoint", map[string]interface{}{})
}

func (c *Client) Validate() error {
	resp, err := c.UserInfo()
	if err != nil {
		return fmt.Errorf("credential validation failed: %w", err)
	}
	code, ok := resp["code"].(float64)
	if !ok || code != 0 {
		msg, _ := resp["msg"].(string)
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("credential validation failed (code=%.0f): %s", code, msg)
	}
	return nil
}

// PrepareModel 调用 model_runtime_prepare 接口"准备模型运行时"。
// 官方客户端在发送聊天消息前会先调用此接口申请/轮询模型运行时令牌，
// 缺失这一步时，部分账号（尤其企业账号 PIN_JD_CLOUD）会被上游直接拒绝
// （AI_GRAY_ACCESS_DENIED）。chatID 为会话标识（官方客户端用它关联历史会话）。
func (c *Client) PrepareModel(model, chatID string) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"model":      model,
		"chatId":     chatID,
		"stream":     true,
		"client":     "JoyCode",
		"language":   "UNKNOWN",
		"orgFullName": c.OrgFullName,
	}
	return c.Post("/api/saas/model-runtime/v1/models/prepare", body)
}

// EnsureModelReady 发送聊天前"准备模型运行时"：调用 model_runtime_prepare 拿令牌，
// 写入 ModelQueueToken 供 chat 请求头 X-Model-Token 使用。官方客户端每个会话都会先
// prepare，缺失时企业账号（PIN_JD_CLOUD）会被上游拒绝（AI_GRAY_ACCESS_DENIED）。
func (c *Client) EnsureModelReady(model string) error {
	resp, err := c.PrepareModel(model, c.SessionID)
	if err != nil {
		return err
	}
	data, _ := resp["data"].(map[string]interface{})
	if data == nil {
		return nil
	}
	if token, ok := data["token"].(string); ok && token != "" {
		c.ModelQueueToken = token
	}
	return nil
}

// UserInfoWithRefresh calls the UserInfo API and returns the refreshed ptKey
// from the response data, if present. Returns (refreshedPtKey, nil) on success.
func (c *Client) UserInfoWithRefresh() (string, error) {
	resp, err := c.UserInfo()
	if err != nil {
		return "", fmt.Errorf("user info request failed: %w", err)
	}
	code, ok := resp["code"].(float64)
	if !ok || code != 0 {
		msg, _ := resp["msg"].(string)
		if msg == "" {
			msg = "unknown error"
		}
		return "", fmt.Errorf("user info failed (code=%.0f): %s", code, msg)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", nil
	}
	if ptKey, ok := data["ptKey"].(string); ok && ptKey != "" {
		return ptKey, nil
	}
	return "", nil
}

func truncate(s string, maxLen int) string {
	return common.Truncate(s, maxLen)
}

// IsErrorBody 判断一行文本是否为上游错误体（含 error/code/status/msg 且无 choices）。
// 上游偶发返回 200 + 纯 JSON 错误（非 SSE data: 行），流式解析会跳过它导致"静默空回复"，
// 需要在此识别并转为 error 供上层处理（自动激活/错误提示）。
func IsErrorBody(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "data:") || trimmed == "[DONE]" {
		return false
	}
	var parsed struct {
		Choices []interface{} `json:"choices"`
		Error   interface{}   `json:"error"`
		Code    interface{}   `json:"code"`
		Status  string        `json:"status"`
		Msg     string        `json:"msg"`
	}
	if json.Unmarshal([]byte(trimmed), &parsed) != nil {
		return false
	}
	if len(parsed.Choices) > 0 {
		return false
	}
	return parsed.Error != nil || parsed.Code != nil || parsed.Status != "" || parsed.Msg != ""
}

// replayReader 回放已读入的首行，再继续读取底层流（用于 PostStream 的 peek 检测）。
type replayReader struct {
	first  []byte
	offset int
	source io.Reader
	body   io.ReadCloser
}

func (r *replayReader) Read(p []byte) (int, error) {
	if r.offset < len(r.first) {
		n := copy(p, r.first[r.offset:])
		r.offset += n
		return n, nil
	}
	return r.source.Read(p)
}

func (r *replayReader) Close() error {
	return r.body.Close()
}

// ── 账号自动激活 ─────────────────────────────────────────────
//
// JoyCode 上游对新账号（新 ptKey）有灰度白名单限制：直接调用 AI 接口会返回
// AI_GRAY_ACCESS_DENIED / "不在内测范围" 之类的错误。官方客户端在首次发送一条
// 聊天消息后，账号即被激活放行。这里模拟该行为：
//
//  1. 检测到激活类错误时，自动发送一条最小聊天请求（等价于客户端首聊）；
//  2. 短等待让上游放行，然后重试原始请求。
//
// 激活是账号级一次性动作，带冷却期（同 ptKey 2 分钟内最多触发一次），
// 避免上游被高频激活请求刷到。

var (
	activateMu      sync.Mutex
	activateHistory = map[string]time.Time{} // ptKey → 上次激活触发时间
)

// activateCooldown 同一账号两次自动激活的最小间隔。
const activateCooldown = 2 * time.Minute

// activateWait 激活请求发出后到重试原始请求前的等待时间（上游放行需要时间）。
const activateWait = 5 * time.Second

// IsActivationError 判断错误信息是否属于"账号未激活/灰度受限"。
// 匹配 JoyCode 上游的 AI_GRAY_ACCESS_DENIED 及各类中英文提示。
func IsActivationError(msg string) bool {
	lower := strings.ToLower(msg)
	keywords := []string{
		"ai_gray_access_denied", "gray_access_denied", "不在内测", "灰度",
		"内测范围", "未激活", "账号受限", "access denied", "beta access",
		"not in beta", "activated",
	}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// minChatBody 构造保活/激活用的最小流式聊天请求体。
// 模型使用官方默认 JoyAI-Code-1.5——账号套餐允许的模型；MiniMax-M3 会报
// 9003「当前模型不在套餐允许的模型列表中」。
const keepaliveModel = DefaultModel

func minChatBody() map[string]interface{} {
	return map[string]interface{}{
		"model":      keepaliveModel,
		"messages": []map[string]string{
			{"role": "system", "content": OfficialSystemPrompt},
			{"role": "user", "content": randomKeepaliveMessage()},
		},
		"stream":         true,
		"max_tokens":     16,
		"sendSource":     "user",
		"thinking":       map[string]interface{}{"type": "disabled"},
		"temperature":    0.7,
	}
}

// TryActivate 解冻账号：上报客户端遥测（functionId=telemetry + IDE 设备指纹），
// 上游据此判定"真实客户端使用"并撤销 AI_GRAY_ACCESS_DENIED，随后用一条流式聊天验证。
// 关键：向已冻结账号发 chat_completions 永远只会再次拿到 AI_GRAY_ACCESS_DENIED，
// 解冻只能由遥测上报触发（实测 pk0377 / 法罗力中国 一次生效）。
func (c *Client) TryActivate() bool {
	if c == nil || c.PtKey == "" {
		return false
	}
	activateMu.Lock()
	if last, ok := activateHistory[c.PtKey]; ok && time.Since(last) < activateCooldown {
		activateMu.Unlock()
		return false // 冷却期内不再重复触发
	}
	activateHistory[c.PtKey] = time.Now()
	activateMu.Unlock()

	if err := c.ReportClientActivity(); err != nil {
		slog.Warn("joycode: activation telemetry report failed", "user_id", c.UserID, "error", err)
	}
	_ = c.ReportUsageMetrics()
	time.Sleep(activateWait) // 等上游放行

	// 验证：再发一条流式消息，成功即账号已解冻
	resp2, err2 := c.PostStream("/api/saas/openai/v1/chat/completions", minChatBody())
	if err2 != nil {
		if resp2 != nil {
			resp2.Body.Close()
		}
		slog.Warn("joycode: activation verify failed", "user_id", c.UserID, "error", err2)
		return false
	}
	resp2.Body.Close()
	slog.Info("joycode: account activation verified", "user_id", c.UserID)
	return true
}

// SendKeepalive 发送一条随机极短流式消息，模拟 JoyCode 客户端对话，保持账号"存活"。
// 关键：先调用 EnsureModelReady 获取 X-Model-Token，否则 PIN_JD_CLOUD 企业账号
// 会被上游直接拒绝（AI_GRAY_ACCESS_DENIED）；同时用始终放行的 MiniMax-M3 模型。
// 若消息仍被拒绝，自动触发激活解冻。返回 true 表示账号当前可用。
func (c *Client) SendKeepalive() bool {
	if c == nil || c.PtKey == "" {
		return false
	}
	// 准备模型运行时：与官方客户端每个会话前 prepare 的行为一致
	_ = c.EnsureModelReady(keepaliveModel)
	resp, err := c.PostStream("/api/saas/openai/v1/chat/completions", minChatBody())
	if err != nil {
		// 冻结/受限 → 触发激活解冻
		slog.Warn("joycode: keepalive message blocked, trying activation", "user_id", c.UserID, "error", err)
		return c.TryActivate()
	}
	resp.Body.Close()
	slog.Info("joycode: keepalive message sent", "user_id", c.UserID)
	return true
}

// randomKeepaliveMessage 生成一条随机极短消息（几个字节~20 字节）。
// 内容在固定词库 + 随机后缀间变化，避免被上游识别为固定心跳而忽略。
func randomKeepaliveMessage() string {
	words := []string{"hi", "hello", "ping", "test", "ok", "in", "1", "ah", "yo"}
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return words[0] + " " + strconv.FormatInt(time.Now().UnixNano()%10000, 10)
	}
	n := int(b[0])<<8 | int(b[1])
	return words[n%len(words)] + " " + strconv.Itoa(n%100000)
}

// PostWithActivation 与 Post 相同，但遇到激活类错误时自动激活账号并重试一次。
func (c *Client) PostWithActivation(endpoint string, body map[string]interface{}) (map[string]interface{}, error) {
	resp, err := c.Post(endpoint, body)
	if err != nil && IsActivationError(err.Error()) && c.TryActivate() {
		slog.Info("joycode: activation triggered, retrying request", "endpoint", endpoint, "user_id", c.UserID)
		time.Sleep(activateWait)
		return c.Post(endpoint, body)
	}
	return resp, err
}

// PostStreamWithActivation 与 PostStream 相同，但遇到激活类错误时自动激活账号并重试一次。
func (c *Client) PostStreamWithActivation(endpoint string, body map[string]interface{}) (*http.Response, error) {
	resp, err := c.PostStream(endpoint, body)
	if err != nil && IsActivationError(err.Error()) && c.TryActivate() {
		slog.Info("joycode: activation triggered, retrying stream", "endpoint", endpoint, "user_id", c.UserID)
		time.Sleep(activateWait)
		return c.PostStream(endpoint, body)
	}
	return resp, err
}

// PostAnthropicStreamWithActivation 与 PostAnthropicStream 相同，遇到激活类错误时自动激活并重试一次。
func (c *Client) PostAnthropicStreamWithActivation(endpoint string, body map[string]interface{}) (*http.Response, error) {
	resp, err := c.PostAnthropicStream(endpoint, body)
	if err != nil && IsActivationError(err.Error()) && c.TryActivate() {
		slog.Info("joycode: activation triggered, retrying anthropic stream", "endpoint", endpoint, "user_id", c.UserID)
		time.Sleep(activateWait)
		return c.PostAnthropicStream(endpoint, body)
	}
	return resp, err
}
