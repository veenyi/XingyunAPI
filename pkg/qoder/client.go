package qoder

// Client 是 Qoder 上游聊天客户端：单次请求执行 + 模型目录抓取。
// 账号轮换的重试语义在 Pool.DoChatWithRetry。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// dynModelsTTL 是动态模型目录的刷新周期。
const dynModelsTTL = 10 * time.Minute

// Client 是 Qoder 网关客户端。
type Client struct {
	pool *Pool
	http *http.Client
}

func newClient(p *Pool, hc *http.Client) *Client {
	return &Client{pool: p, http: hc}
}

// Name 实现 provider.Keyless。
func (c *Client) Name() string { return Name }

// Enabled 透传池开关。
func (c *Client) Enabled() bool { return c.pool.Enabled() }

// ListModels 实现 provider.Keyless。
func (c *Client) ListModels() []string { return c.pool.ListModels() }

// Supports 实现 provider.Keyless。
func (c *Client) Supports(model string) bool { return c.pool.Supports(model) }

// SyncAccounts 重载账号池。
func (c *Client) SyncAccounts() { c.pool.SyncAccounts() }

// staticEntries 返回静态模型表（含 std↔custom 映射）。
func (c *Client) staticEntries() []ModelEntry {
	out := make([]ModelEntry, 0, len(StaticModels))
	for _, m := range StaticModels {
		std := NormalizeModelName(m)
		out = append(out, ModelEntry{Name: std, Upstream: customModelName(std)})
	}
	return out
}

// modelKey 返回模型的规范键（健康键/动态缓存查找用）。
func (c *Client) modelKey(model string) string { return NormalizeModelName(model) }

// pickAccount 按排除表挑一个账号（单次，无重试）。
func (c *Client) pickAccount(exclude map[string]bool) *QoderAccount {
	return c.pool.PickExcluding(exclude)
}

// FetchModels 拉取上游动态模型目录并写入缓存。
func (c *Client) FetchModels(ctx context.Context) error {
	acct := c.pickAccount(nil)
	if acct == nil {
		return errors.New("qoder: 没有可用账号，无法拉取模型目录")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", OpenAPIBase+modelsPath, nil)
	if err != nil {
		return err
	}
	ApplyOpenAPIHeaders(req.Header, acct.AccessToken)
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("qoder: 模型目录拉取失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qoder: 模型目录拉取失败: HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 200))
	}
	var parsed struct {
		Data []DynamicModelEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("qoder: 模型目录解析失败: %w", err)
	}
	items := make([]DynamicModelEntry, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			items = append(items, m)
		}
	}
	dynModels.set(items)
	return nil
}

// ensureModels 在缓存过期时后台刷新动态目录。
func (c *Client) ensureModels() {
	if !dynModels.stale(dynModelsTTL) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	go func() {
		defer cancel()
		if err := c.FetchModels(ctx); err != nil {
			// 退避内沿用上一次名单
			return
		}
	}()
}

// Chat 单次非流式聊天（无账号重试）。
func (c *Client) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	acct := c.pickAccount(nil)
	if acct == nil {
		return nil, errors.New("qoder: 没有可用账号")
	}
	_, out, err := c.doChat(ctx, acct, body, false)
	return out, err
}

// ChatStream 单次流式聊天（无账号重试），返回规范化后的 OpenAI SSE 流。
func (c *Client) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	acct := c.pickAccount(nil)
	if acct == nil {
		return nil, errors.New("qoder: 没有可用账号")
	}
	resp, _, err := c.doChat(ctx, acct, body, true)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// doChatForAccount 单账号非流式聊天（Pool 轮换循环的每步调用）。
func (c *Client) doChatForAccount(ctx context.Context, acct *QoderAccount, body map[string]interface{}) (map[string]interface{}, error) {
	_, out, err := c.doChat(ctx, acct, body, false)
	return out, err
}

// doChat 执行一次网关聊天请求：stream=true 返回规范化 SSE 流，
// 否则返回聚合后的 OpenAI 形状响应（两个返回值二选一）。
func (c *Client) doChat(ctx context.Context, acct *QoderAccount, body map[string]interface{}, stream bool) (io.ReadCloser, map[string]interface{}, error) {
	if acct.AccessToken == "" && acct.RefreshToken == "" {
		return nil, nil, errors.New("qoder: 账号缺少令牌，请重新登录")
	}
	payload, err := json.Marshal(buildAgentBody(acct, body, stream))
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", GatewayBase+chatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	sessionFor(acct).ApplyHeaders(req.Header)
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("qoder: 聊天请求失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, nil, fmt.Errorf("qoder: HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 300))
	}
	if stream {
		return normalizeQoderStream(resp.Body), nil, nil
	}
	model := NormalizeModelName(fmt.Sprint(body["model"]))
	out, err := decodeChatResponse(resp, model)
	if err != nil {
		return nil, nil, err
	}
	if msg := errorMessage(out); msg != "" {
		return nil, nil, errors.New(msg)
	}
	return nil, out, nil
}

// errorMessage 提取 200 响应体内嵌的错误对象文本。
func errorMessage(out map[string]interface{}) string {
	e, ok := out["error"]
	if !ok {
		return ""
	}
	switch v := e.(type) {
	case map[string]interface{}:
		if m, _ := v["message"].(string); m != "" {
			return m
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	case string:
		return v
	}
	return fmt.Sprintf("upstream error: %v", e)
}

// sessionFor 用账号字段构造 Cosy 会话。
func sessionFor(acct *QoderAccount) *CosySession {
	s := NewCosySession(acct.AccessToken, acct.RefreshToken, acct.MachineID)
	s.MachineID = acct.MachineID
	s.MachineType = acct.MachineType
	s.Region = acct.Region
	s.EnterpriseID = acct.EnterpriseID
	return s
}
