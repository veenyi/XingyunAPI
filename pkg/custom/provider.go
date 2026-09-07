// Provider is the runtime half of pkg/custom: one user-defined channel as a
// chat-capable keyless provider, with paywall/blocklist-aware catalog
// filtering. Pointer receiver only (matches the 0.7.2 symbol table).
package custom

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/compat"
	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/provider"
)

const (
	tierCustom     = 50 // plain custom channels dispatch last
	tierCustomFree = 35 // free_only channels dispatch with the free pools
	paidMarkTTL    = 30 * time.Minute
)

// paidMark remembers when a model last answered with a paywall-classified
// failure (no_credit / auth_failed).
type paidMark struct {
	class string
	at    time.Time
}

// Provider is one user-defined channel (provider.Keyless + Tier/RefreshNow).
type Provider struct {
	entry   ProviderJSON
	baseURL string
	sc      *compat.Client

	mu     sync.Mutex
	health *health.Registry
	paid   map[string]paidMark
}

// setHealth swaps the health registry (Manager.SetRegistry).
func (p *Provider) setHealth(h *health.Registry) {
	p.mu.Lock()
	p.health = h
	p.mu.Unlock()
}

// Entry returns the stored row backing this provider.
func (p *Provider) Entry() ProviderEntry { return entryOf(p.entry) }

// entryOf converts the stored row into its API shape.
func entryOf(row ProviderJSON) ProviderEntry {
	return ProviderEntry{
		ID:          row.ID,
		Name:        row.Name,
		BaseURL:     row.BaseURL,
		APIKey:      row.APIKey,
		Models:      row.Models,
		Allowed:     row.Allowed,
		Blocked:     row.Blocked,
		FreeOnly:    row.FreeOnly,
		PriceFactor: row.PriceFactor,
		Enabled:     row.Enabled,
	}
}

// Name implements provider.Keyless.
func (p *Provider) Name() string { return p.entry.Name }

// Enabled implements provider.Keyless: the row must be enabled and carry a
// usable name + base URL.
func (p *Provider) Enabled() bool {
	return p.entry.Enabled && p.entry.Name != "" && p.baseURL != ""
}

// Tier is the dispatch tier (free_only channels join the free-pool tier).
func (p *Provider) Tier() int {
	if p.entry.FreeOnly {
		return tierCustomFree
	}
	return tierCustom
}

// isBlocked reports whether the model sits on the provider's manual
// blocklist (JSON field blocked).
func (p *Provider) isBlocked(model string) bool {
	for _, b := range p.entry.Blocked {
		if b == model {
			return true
		}
	}
	return false
}

// isPaidModel reports whether chat/probe history says the model sits behind
// a paywall (欠费/登录失效/401); such models auto-hide from free_only
// channels until the mark expires.
func (p *Provider) isPaidModel(model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.paid[model]
	if !ok {
		return false
	}
	if time.Since(m.at) > paidMarkTTL {
		delete(p.paid, model)
		return false
	}
	return true
}

// visibleFilter is the catalog allow-filter handed to compat: blocked models
// never show, paywalled models hide on free_only channels, and a configured
// allowlist prunes everything not listed.
func (p *Provider) visibleFilter(model string) bool {
	if p.isBlocked(model) {
		return false
	}
	if allow := p.entry.Allowed; len(allow) > 0 && !containsModel(allow, model) {
		return false
	}
	if p.entry.FreeOnly && p.isPaidModel(model) {
		return false
	}
	return true
}

// containsModel is a tiny membership helper (avoids importing slices).
func containsModel(list []string, model string) bool {
	for _, m := range list {
		if m == model {
			return true
		}
	}
	return false
}

// record folds a chat outcome into the paywall marks and the health registry.
func (p *Provider) record(model string, err error) {
	if model == "" {
		return
	}
	p.mu.Lock()
	if err == nil {
		delete(p.paid, model)
		h := p.health
		p.mu.Unlock()
		if h != nil {
			h.MarkOK(p.entry.Name, model)
		}
		return
	}
	class := health.Classify(err)
	if class == health.ClassNoCredit || class == health.ClassAuthFailed {
		p.paid[model] = paidMark{class: class, at: time.Now()}
	}
	h := p.health
	p.mu.Unlock()
	if h != nil {
		h.MarkFailure(p.entry.Name, model, err)
	}
}

// Chat implements provider.Keyless.
func (p *Provider) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	model, _ := body["model"].(string)
	out, err := p.sc.Chat(ctx, body)
	p.record(model, err)
	if err == nil {
		if lookErr := compat.LooksLikeError(out); lookErr != nil {
			p.record(model, lookErr)
		}
	}
	return out, err
}

// ChatStream implements provider.Keyless.
func (p *Provider) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	model, _ := body["model"].(string)
	rc, err := p.sc.ChatStream(ctx, body)
	p.record(model, err)
	return rc, err
}

// ListModels implements provider.Keyless (visibleFilter applied by compat).
func (p *Provider) ListModels() []string { return p.sc.ListModels() }

// Supports implements provider.Keyless.
func (p *Provider) Supports(model string) bool { return p.sc.Supports(model) }

// RefreshNow forces a catalog re-fetch for this channel.
func (p *Provider) RefreshNow(ctx context.Context) { p.sc.RefreshNow(ctx) }

// Compile-time surface checks.
var _ provider.Keyless = (*Provider)(nil)
