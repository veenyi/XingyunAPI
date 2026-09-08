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
	"strconv"
	"strings"
	"time"
)

const (
	BaseURL       = "https://joycode-api.jd.com"
	SaasBaseURL   = "http://joycode-api-saas.jd.com"
	DefaultModel  = "JoyAI-Code-1.5"
	ClientVersion = "2.7.5"
	UserAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"JoyCode/2.7.5 Chrome/133.0.0.0 Electron/35.2.0 Safari/537.36"

	// color gateway 签名（逆向自 JoyCode 2.7.5 / joycoder-editor 3.8.57）
	DefaultColorBaseURL = "https://api-ai.jd.com"
	colorGatewayAppID   = "joycode_ide"
	colorGatewayPath    = "/api"
	colorHMACKey        = "0691a3f0b37b4a85aeb63ad0fc7db3ed"
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
}

// Models 是 JoyCode 渠道模型表（顺序对齐 IDE 下拉）。2026-09-08 补齐 IDE
// 现役模型：GLM-5.1 / GLM-5 / MiniMax-M2.7（三个实测零扣分=免费）。
var Models = []string{
	"JoyAI-Code-1.5",
	"MiniMax-M2.7",
	"Kimi-K2.6",
	"GLM-5.1",
	"GLM-5",
	"Doubao-Seed-2.0-pro",
	"GLM-5.3",
	"MiniMax-M3",
}

// APIModelNames 把模型显示名映射为上游 chatApiModel（modelList 实测
// 2026-09-04；未列出的名字原样透传，上游对别名宽容）。
//
// 只映射付费的 -agent 变体：GLM-5.1 / GLM-5 / MiniMax-M2.7 **必须保持裸名透传**，
// 2026-09-08 积分实测裸名零扣分（免费），而 -agent 变体每次扣 3~11 分。
var APIModelNames = map[string]string{
	"GLM-5.3":             "GLM-5.3-agent",
	"Kimi-K2.6":           "Kimi-K2.6-agent",
	"MiniMax-M3":          "MiniMax-M3-agent",
	"Doubao-Seed-2.0-pro": "Doubao-Seed-2.0-pro-agent",
}

// ModelNotes 模型备注（/api/models 的 note 字段）。免费为 2026-09-08
// 单账号积分实测（每次 max_tokens 300~400，比对 /api/accounts/{id}/point
// 的 remain 前后值）：JoyAI-Code-1.5 / GLM-5.1 / GLM-5 / MiniMax-M2.7
// 裸名零扣分；GLM-5.3 -4、Doubao-Seed-2.0-pro -3、MiniMax-M3 -9、
// Kimi-K2.6 -10（每次）。
var ModelNotes = map[string]string{
	"JoyAI-Code-1.5": "免费",
	"GLM-5.1":        "免费",
	"GLM-5":          "免费",
	"MiniMax-M2.7":   "免费",
}

// APIModelName 返回上游 chatApiModel（无映射时原样返回）。
func APIModelName(model string) string {
	if mapped, ok := APIModelNames[model]; ok {
		return mapped
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

func NewClient(ptKey, userID string) *Client {
	return &Client{
		PtKey:        ptKey,
		UserID:       userID,
		SessionID:    newHexID(),
		ColorBaseURL: DefaultColorBaseURL,
		httpClient:   &http.Client{Timeout: 30 * time.Minute},
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
			query, sign := colorSign(ep.functionID)
			return u.Scheme + "://" + u.Host + colorGatewayPath + "?" + query + "&sign=" + sign
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
	return http.Header{
		"Content-Type":    {"application/json; charset=UTF-8"},
		"source-type":     {"joycoder-ide"},
		"ptKey":           {c.PtKey},
		"loginType":       {loginType},
		"User-Agent":      {UserAgent},
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate"},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
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
	return http.Header{
		"Content-Type":    {"application/json; charset=utf-8"},
		"source-type":     {"joycoder-ide"},
		"ptKey":           {ptKey},
		"loginType":       {loginType},
		"User-Agent":      {UserAgent},
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate"},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
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
	if m, ok := body["model"].(string); ok {
		body["model"] = APIModelName(m)
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
	if m, ok := body["model"].(string); ok {
		body["model"] = APIModelName(m)
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

// guardSSEHead turns HTTP-200 JSON error bodies (which carry no SSE events)
// into errors so stream callers fail over, instead of relaying a non-SSE
// payload that OpenAI clients would parse as an empty stream.
func guardSSEHead(resp *http.Response) error {
	br := bufio.NewReader(resp.Body)
	head, _ := br.Peek(256)
	if len(head) == 0 {
		return nil
	}
	if _, err := br.Discard(len(head)); err != nil {
		return nil
	}
	resp.Body = &peekedBody{Reader: io.MultiReader(bytes.NewReader(head), br), Closer: resp.Body}
	t := bytes.TrimLeft(head, " \t\r\n")
	if len(t) == 0 || t[0] != '{' {
		return nil
	}
	rest, _ := io.ReadAll(io.LimitReader(br, 64<<10))
	resp.Body.Close()
	payload := append(append([]byte{}, t...), rest...)
	if msg := jsonErrMessage(payload); msg != "" {
		return fmt.Errorf("upstream error: %s", msg)
	}
	return fmt.Errorf("upstream error: %s", truncate(string(payload), 500))
}

// jsonErrMessage extracts error.message from a JSON body, if present.
func jsonErrMessage(payload []byte) string {
	var obj struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &obj) != nil || obj.Error.Message == "" {
		return ""
	}
	if obj.Error.Code != "" {
		return obj.Error.Code + ": " + obj.Error.Message
	}
	return obj.Error.Message
}

type peekedBody struct {
	io.Reader
	io.Closer
}

func (c *Client) Post(endpoint string, body map[string]interface{}) (map[string]interface{}, error) {
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
	resp, err := c.doPost(endpoint, c.prepareBody(body))
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
	if err := guardSSEHead(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func (c *Client) PostAnthropicStream(endpoint string, body map[string]interface{}) (*http.Response, error) {
	resp, err := c.doAnthropicPost(endpoint, c.prepareAnthropicBody(body))
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
	if err := guardSSEHead(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
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
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
