package workbuddy

// Client 是 WorkBuddy 上游客户端：模型目录抓取 + 单账号聊天执行。
// 账号轮换的重试语义在 Pool.DoChatWithRetry。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// dynModelsCache 是动态模型目录的进程内缓存。
type dynModelsCache struct {
	mu    sync.Mutex
	items []string
	at    time.Time
}

// get 返回缓存副本。
func (c *dynModelsCache) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.items...)
}

// set 覆盖缓存。
func (c *dynModelsCache) set(items []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append([]string(nil), items...)
	c.at = time.Now()
}

// stale 报告缓存是否超过 ttl。
func (c *dynModelsCache) stale(ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items == nil || time.Since(c.at) > ttl
}

// dynModels 是包级动态模型缓存。
var dynModels = &dynModelsCache{}

// Client 是 WorkBuddy 客户端。
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

// ListModels 实现 provider.Keyless（动态目录，懒刷新）。
func (c *Client) ListModels() []string {
	c.ensureModels()
	return dynModels.get()
}

// Supports 实现 provider.Keyless。
func (c *Client) Supports(model string) bool {
	k := normalizeModelName(model)
	for _, m := range dynModels.get() {
		if normalizeModelName(m) == k {
			return true
		}
	}
	return false
}

// SyncAccounts 重载账号池。
func (c *Client) SyncAccounts() { c.pool.SyncAccounts() }

// ensureModels 在缓存过期时刷新动态目录（失败沿用上一次名单）。
func (c *Client) ensureModels() {
	if !dynModels.stale(dynModelsTTL) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	go func() {
		defer cancel()
		_ = c.FetchModels(ctx)
	}()
}

// FetchModels 拉取上游动态模型目录并写入缓存。协议对齐 workbuddy2api：
// GET console/enterprises/personal/models，取 agents[name=cli].models 与
// data.models（剔除 disabled）的交集作为可见清单。
func (c *Client) FetchModels(ctx context.Context) error {
	acct := c.pool.Pick()
	if acct == nil {
		return errors.New("workbuddy: 没有可用账号，无法拉取模型目录")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", BaseURL+modelsPath, nil)
	if err != nil {
		return err
	}
	h := http.Header{
		"Accept":     {"application/json"},
		"User-Agent": {modelsUserAgent},
		"Origin":     {"https://www.codebuddy.cn"},
		"Referer":    {"https://www.codebuddy.cn/"},
	}
	h.Set("Authorization", "Bearer "+acct.AccessToken)
	if acct.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", acct.EnterpriseID)
	}
	req.Header = h
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("workbuddy: 模型目录拉取失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("workbuddy: 模型目录拉取失败: HTTP %d: %s", resp.StatusCode, truncateDetail(string(raw)))
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Models []struct {
				ID       string `json:"id"`
				Disabled bool   `json:"disabled"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("workbuddy: 模型目录解析失败: %w", err)
	}
	if parsed.Code != 0 {
		return fmt.Errorf("workbuddy: 模型目录 code=%d: %s", parsed.Code, truncateDetail(parsed.Msg))
	}
	disabled := map[string]bool{}
	for _, m := range parsed.Data.Models {
		if m.Disabled {
			disabled[m.ID] = true
		}
	}
	items := make([]string, 0, len(parsed.Data.Agents))
	for _, ag := range parsed.Data.Agents {
		if ag.Name != "cli" {
			continue
		}
		for _, id := range ag.Models {
			if id != "" && !disabled[id] {
				items = append(items, id)
			}
		}
		break
	}
	if len(items) == 0 {
		return errors.New("workbuddy: 模型目录无 cli 可用模型")
	}
	dynModels.set(items)
	return nil
}

// doChatForAccount 单账号非流式聊天（Pool 轮换循环的每步调用）。
// 上游 v2/chat/completions 只支持流式（非流式回 11101），故固定 stream=true
// 再把 SSE 聚合为完整响应。
func (c *Client) doChatForAccount(ctx context.Context, acct *WBAccount, body map[string]interface{}) (map[string]interface{}, error) {
	payload, err := json.Marshal(PrepareBody(body, true))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", BaseURL+chatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = chatHeaders(acct)
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: 聊天请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workbuddy: HTTP %d: %s", resp.StatusCode, truncateDetail(string(raw)))
	}
	if out, err := parseJSONBody(raw); err == nil {
		if e, ok := out["error"]; ok {
			return nil, fmt.Errorf("upstream error: %v", e)
		}
		return out, nil
	}
	return aggregateSSE(string(raw)), nil
}

// parseJSONBody 尝试把响应体解析为 JSON 对象。
func parseJSONBody(raw []byte) (map[string]interface{}, error) {
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// doChatStreamForAccount 单账号流式聊天，返回上游 SSE 原始响应体。
func (c *Client) doChatStreamForAccount(ctx context.Context, acct *WBAccount, body map[string]interface{}) (io.ReadCloser, error) {
	payload, err := json.Marshal(PrepareBody(body, true))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", BaseURL+chatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = chatHeaders(acct)
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: 聊天请求失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("workbuddy: HTTP %d: %s", resp.StatusCode, truncateDetail(string(raw)))
	}
	return resp.Body, nil
}

// aggregateSSE 把上游 SSE 聚合为 OpenAI 形状的非流式响应。
func aggregateSSE(raw string) map[string]interface{} {
	var content strings.Builder
	var toolAcc []map[string]interface{}
	finish := ""
	usage := map[string]interface{}{}
	id := ""
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content   string        `json:"content"`
					Reasoning string        `json:"reasoning"`
					ToolCalls []interface{} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason interface{} `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]interface{} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.ID != "" && id == "" {
			id = chunk.ID
		}
		for _, ch := range chunk.Choices {
			content.WriteString(ch.Delta.Content)
			if ch.Delta.ToolCalls != nil {
				toolAcc = mergeToolCallDelta(toolAcc, ch.Delta.ToolCalls)
			}
			if s, ok := ch.FinishReason.(string); ok && s != "" {
				finish = s
			}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	message := map[string]interface{}{"role": "assistant", "content": content.String()}
	if len(toolAcc) > 0 {
		cleaned := make([]interface{}, 0, len(toolAcc))
		for _, tc := range toolAcc {
			cleaned = append(cleaned, tc)
		}
		message["tool_calls"] = cleaned
		if finish == "" {
			finish = "tool_calls"
		}
	}
	choice := map[string]interface{}{"index": 0, "message": message, "finish_reason": finish}
	out := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"model":   "",
		"choices": []interface{}{choice},
	}
	if len(usage) > 0 {
		out["usage"] = usage
	}
	return out
}
