// Package freepool aggregates the credential-free OpenCode Zen catalog into
// one keyless pool whose models carry the `-free` suffix (e.g.
// nemotron-3.5-lightning-free、mimo-v2.5-free). The toggle lives in settings:
// opencode_enabled.
//
// 历史源 9Router（本地软件官网，云端无 API）与 FreeLLM（域名已失效）
// 于 2026-09 实测后移除；它们的 -free 模型全部来自 OpenCode Zen 同一目录。
package freepool

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/common"
	"github.com/veenyi/XingyunAPI/pkg/compat"
	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/provider"
)

const (
	poolName      = "freepool"
	tierFree      = 30              // dispatches with the other free pools
	refreshMinGap = 5 * time.Minute // backoff window between pool refreshes
)

// Settings is freepool's view of the settings store.
type Settings interface {
	GetSetting(key string) string
}

// source is one aggregated free catalog.
type source struct {
	id      string
	baseURL string
	onKey   string   // settings switch (default off)
	models  []string // static free-model seed
}

// poolSources are the aggregated keyless free catalogs.
// 2026-09-07：OpenCode Zen 免费档全面收紧为「仅限 OpenCode 客户端会话」
// （所有 -free 模型匿名请求回 MissingSessionID），源整体下线；免费档改由
// keyfree（pollinations，非流式合成）承担。若上游放开，把源加回即可。
var poolSources = []source{}

// isFreeModel reports whether a catalog id carries the free suffix.
func isFreeModel(id string) bool {
	return strings.HasSuffix(id, "-free") || strings.HasSuffix(id, ":free")
}

// freeName normalizes ":free" ids to the pool's "-free" spelling.
func freeName(id string) string {
	if strings.HasSuffix(id, ":free") {
		return strings.TrimSuffix(id, ":free") + "-free"
	}
	return id
}

// Client is the aggregated free-model pool
// (provider.Keyless + provider.Catalog).
type Client struct {
	st   Settings
	subs []*compat.Client   // per-source clients, dispatch order
	wrap []provider.Keyless // extra keyless sources folded into the pool

	mu          sync.Mutex
	lastRefresh time.Time
	lastBackoff time.Time
}

// New builds the pool; every enabled source gets a compat client and any
// extra keyless sources given are wrapped into the rotation.
func New(st Settings, wrap ...provider.Keyless) *Client {
	c := &Client{st: st}
	for _, s := range poolSources {
		if !common.SettingEnabledOr(st, s.onKey, false) {
			continue
		}
		c.subs = append(c.subs, compat.New(compat.Options{
			Name:    poolName,
			BaseURL: s.baseURL,
			Models:  append([]string(nil), s.models...),
			Tier:    tierFree,
		}))
	}
	c.wrap = append([]provider.Keyless(nil), wrap...)
	return c
}

// Name implements provider.Keyless.
func (c *Client) Name() string { return poolName }

// Enabled implements provider.Keyless: at least one source is active.
func (c *Client) Enabled() bool { return len(c.subs) > 0 || len(c.wrap) > 0 }

// Supports implements provider.Keyless.
func (c *Client) Supports(model string) bool {
	for _, m := range c.ListModels() {
		if m == model {
			return true
		}
	}
	return false
}

// ListModels implements provider.Keyless: the deduped union of every
// source's free-suffixed models.
func (c *Client) ListModels() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(id string) {
		id = freeName(id)
		if id == "" || seen[id] || !isFreeModel(id) {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, sc := range c.subs {
		for _, id := range sc.ListModels() {
			add(id)
		}
	}
	for _, w := range c.wrap {
		for _, id := range w.ListModels() {
			add(id)
		}
	}
	return out
}

// ModelIDs returns the normalized free ids of every source (diagnostics
// view for the model-status tooling).
func (c *Client) ModelIDs() []string { return c.ListModels() }

// CatalogIDs is an alias kept for the provider.Catalog surface.
func (c *Client) CatalogIDs() []string { return c.ModelIDs() }

// upstreamModel maps a pool-side "-free" name back to the id the source
// actually serves (its ":free" spelling) when needed.
func upstreamModel(sc *compat.Client, model string) string {
	if sc.Supports(model) {
		return model
	}
	if strings.HasSuffix(model, "-free") {
		if alt := strings.TrimSuffix(model, "-free") + ":free"; sc.Supports(alt) {
			return alt
		}
	}
	return model
}

// Chat implements provider.Keyless: first source that answers wins.
func (c *Client) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	c.maybeRefresh()
	requested, _ := body["model"].(string)
	var lastErr error
	for _, sc := range c.subs {
		body["model"] = upstreamModel(sc, requested)
		out, err := sc.Chat(ctx, body)
		body["model"] = requested
		if err == nil {
			if lookErr := compat.LooksLikeError(out); lookErr != nil {
				lastErr = lookErr
				continue
			}
			return out, nil
		}
		lastErr = err
	}
	for _, w := range c.wrap {
		body["model"] = requested
		out, err := w.Chat(ctx, body)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// ChatStream implements provider.Keyless: first source that answers wins.
func (c *Client) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	c.maybeRefresh()
	requested, _ := body["model"].(string)
	var lastErr error
	for _, sc := range c.subs {
		body["model"] = upstreamModel(sc, requested)
		rc, err := sc.ChatStream(ctx, body)
		body["model"] = requested
		if err == nil {
			return rc, nil
		}
		lastErr = err
	}
	for _, w := range c.wrap {
		body["model"] = requested
		rc, err := w.ChatStream(ctx, body)
		if err == nil {
			return rc, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Tier implements provider.Catalog.
func (c *Client) Tier() int { return tierFree }

// ClassifyError implements provider.Catalog.
func (c *Client) ClassifyError(err error) string { return health.Classify(err) }

// RefreshNow implements provider.Catalog: forces a re-fetch on every source.
func (c *Client) RefreshNow(ctx context.Context) {
	for _, sc := range c.subs {
		sc.RefreshNow(ctx)
	}
	c.mu.Lock()
	c.lastRefresh = time.Now()
	c.mu.Unlock()
	slog.Info("免费渠道目录定期刷新完成", "sources", len(c.subs))
}

// maybeRefresh lazily pulls the pool catalogs when empty, honouring the
// backoff window (uses the last good list while backing off).
func (c *Client) maybeRefresh() {
	if len(c.ListModels()) > 0 {
		return
	}
	c.mu.Lock()
	now := time.Now()
	inWindow := now.Sub(c.lastRefresh) < refreshMinGap
	logStale := inWindow && now.Sub(c.lastBackoff) >= refreshMinGap
	if logStale {
		c.lastBackoff = now
	}
	if !inWindow {
		c.lastRefresh = now
	}
	c.mu.Unlock()
	if inWindow {
		if logStale {
			slog.Info("目录刷新处于退避窗口，使用上一次成功的名单")
		}
		return
	}
	for _, sc := range c.subs {
		sc.RefreshNow(context.Background())
	}
}

// Compile-time surface checks.
var (
	_ provider.Keyless = (*Client)(nil)
	_ provider.Catalog = (*Client)(nil)
)
