// Package keyfree serves the credential-free public model pool: community
// OpenAI-compatible endpoints that answer without any API key. Catalog ids
// are filtered through excludedModels; verifiedModels are surfaced even when
// every catalog fetch is in backoff.
package keyfree

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/common"
	"github.com/veenyi/XingyunAPI/pkg/compat"
	"github.com/veenyi/XingyunAPI/pkg/provider"
)

const (
	poolName    = "keyfree"
	tierKeyfree = 30 // dispatches with the other free pools
)

// Settings is keyfree's view of the settings store.
type Settings interface {
	GetSetting(key string) string
}

// excludedModels are ids dropped from public catalogs (broken or useless
// entries that some community endpoints keep serving).
var excludedModels = map[string]bool{
	"api":     true,
	"default": true,
	"none":    true,
	"test":    true,
}

// verifiedModels are known-good ids surfaced even when every catalog fetch
// fails (they are also the static seed of the pool).
var verifiedModels = map[string]bool{
	"openai":      true,
	"openai-fast": true,
	"mistral":     true,
}

// endpoint is one keyless public upstream.
type endpoint struct {
	name    string
	baseURL string
	models  []string
	// noStream: 上游对 stream=true 返回错误（如 pollinations 2026-09 起对
	// legacy API 的流式请求回 402），ChatStream 走非流式请求 + 本地合成 SSE。
	noStream bool
}

// endpoints are the credential-free public endpoints polled by the pool.
// opencode-zen 于 2026-09-07 移除：其免费档全面收紧为「仅限 OpenCode 客户端
// 会话」（MissingSessionID），匿名请求全部被拒。
var endpoints = []endpoint{
	{name: "pollinations", baseURL: "https://text.pollinations.ai/openai", models: []string{"openai", "openai-fast", "mistral"}, noStream: true},
}

// setting reads a setting with a nil-safe receiver.
func setting(s Settings, key string) string {
	if s == nil {
		return ""
	}
	return s.GetSetting(key)
}

// Client is the keyfree pool (provider.Keyless + Tier/RefreshNow).
type Client struct {
	st   Settings
	subs []*compat.Client
}

// New builds the pool over every configured public endpoint.
func New(st Settings) *Client {
	subs := make([]*compat.Client, 0, len(endpoints))
	for _, ep := range endpoints {
		subs = append(subs, compat.New(compat.Options{
			Name:    poolName,
			BaseURL: ep.baseURL,
			Models:  append([]string(nil), ep.models...),
			Tier:    tierKeyfree,
		}))
	}
	return &Client{st: st, subs: subs}
}

// Name implements provider.Keyless.
func (c *Client) Name() string { return poolName }

// Enabled implements provider.Keyless (keyfree_enabled, default off —
// 免费公共渠道须用户在渠道设置里显式开启).
func (c *Client) Enabled() bool {
	return common.SettingEnabledOr(c.st, "keyfree_enabled", false)
}

// Supports implements provider.Keyless: verified ids or any sub catalog.
func (c *Client) Supports(model string) bool {
	if model == "" || excludedModels[model] {
		return false
	}
	if verifiedModels[model] {
		return true
	}
	for _, sc := range c.subs {
		if sc.Supports(model) {
			return true
		}
	}
	return false
}

// ListModels implements provider.Keyless: the union of verified ids and all
// sub catalogs minus the excluded ones.
func (c *Client) ListModels() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(m string) {
		if m == "" || excludedModels[m] || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	for m := range verifiedModels {
		add(m)
	}
	for _, sc := range c.subs {
		for _, m := range sc.ListModels() {
			add(m)
		}
	}
	sort.Strings(out)
	return out
}

// Chat implements provider.Keyless: first endpoint that answers wins.
// 402（pollinations 匿名预算波动）自动重试一次。
func (c *Client) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	var lastErr error
	for _, sc := range c.subs {
		for attempt := 0; attempt < 2; attempt++ {
			out, err := sc.Chat(ctx, body)
			if err == nil {
				if lookErr := compat.LooksLikeError(out); lookErr != nil {
					lastErr = lookErr
					break
				}
				return out, nil
			}
			lastErr = err
			if attempt == 0 && strings.Contains(err.Error(), "402") {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(2 * time.Second):
				}
				continue
			}
			break
		}
	}
	return nil, lastErr
}

// ChatStream implements provider.Keyless: first endpoint that answers wins.
// noStream 端点走非流式请求，再把完整响应合成为单帧 SSE 流（pollinations
// 对流式请求回 402，非流式匿名可用）。
func (c *Client) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	var lastErr error
	for i, sc := range c.subs {
		for attempt := 0; attempt < 2; attempt++ {
			if i < len(endpoints) && endpoints[i].noStream {
				out, err := sc.Chat(ctx, body)
				if err != nil {
					lastErr = err
					if attempt == 0 && strings.Contains(err.Error(), "402") {
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-time.After(2 * time.Second):
						}
						continue
					}
					break
				}
				if lookErr := compat.LooksLikeError(out); lookErr != nil {
					lastErr = lookErr
					break
				}
				return synthSSE(out), nil
			}
			rc, err := sc.ChatStream(ctx, body)
			if err == nil {
				return rc, nil
			}
			lastErr = err
			break
		}
	}
	return nil, lastErr
}

// synthSSE 把非流式 completion 包装成单帧 delta 形状的 SSE 流（含 usage 与
// finish_reason:stop），下游中继按普通上游 SSE 原样转发。
func synthSSE(out map[string]interface{}) io.ReadCloser {
	id, _ := out["id"].(string)
	content := ""
	usage := map[string]interface{}{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if u, ok := out["usage"].(map[string]interface{}); ok {
		usage = u
	}
	if choices, ok := out["choices"].([]interface{}); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := ch["message"].(map[string]interface{}); ok {
				content, _ = msg["content"].(string)
			}
		}
	}
	first := map[string]interface{}{
		"id": id, "object": "chat.completion.chunk",
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{"role": "assistant", "content": content}, "finish_reason": nil}},
	}
	final := map[string]interface{}{
		"id": id, "object": "chat.completion.chunk",
		"choices": []map[string]interface{}{{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"}},
		"usage":   usage,
	}
	b1, _ := json.Marshal(first)
	b2, _ := json.Marshal(final)
	return io.NopCloser(strings.NewReader("data: " + string(b1) + "\n\ndata: " + string(b2) + "\n\ndata: [DONE]\n\n"))
}

// Tier returns the dispatch tier of the pool.
func (c *Client) Tier() int { return tierKeyfree }

// RefreshNow forces a catalog re-fetch on every endpoint.
func (c *Client) RefreshNow(ctx context.Context) {
	for _, sc := range c.subs {
		sc.RefreshNow(ctx)
	}
}

// Compile-time surface checks.
var _ provider.Keyless = (*Client)(nil)
