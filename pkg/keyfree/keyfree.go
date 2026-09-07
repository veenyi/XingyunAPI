// Package keyfree serves the credential-free public model pool: community
// OpenAI-compatible endpoints that answer without any API key. Catalog ids
// are filtered through excludedModels; verifiedModels are surfaced even when
// every catalog fetch is in backoff.
package keyfree

import (
	"context"
	"io"
	"sort"

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
}

// endpoints are the credential-free public endpoints polled by the pool.
var endpoints = []endpoint{
	{name: "opencode-zen", baseURL: "https://opencode.ai/zen/v1"},
	{name: "pollinations", baseURL: "https://text.pollinations.ai/openai", models: []string{"openai", "openai-fast", "mistral"}},
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
func (c *Client) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	var lastErr error
	for _, sc := range c.subs {
		out, err := sc.Chat(ctx, body)
		if err == nil {
			if lookErr := compat.LooksLikeError(out); lookErr != nil {
				lastErr = lookErr
				continue
			}
			return out, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// ChatStream implements provider.Keyless: first endpoint that answers wins.
func (c *Client) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	var lastErr error
	for _, sc := range c.subs {
		rc, err := sc.ChatStream(ctx, body)
		if err == nil {
			return rc, nil
		}
		lastErr = err
	}
	return nil, lastErr
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
