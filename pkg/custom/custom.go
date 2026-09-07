// Package custom implements user-defined OpenAI-compatible channels: the
// custom_providers settings blob (one JSON array), the channel manager that
// persists and rebuilds it, and the Provider wrapper that joins the free
// pool rotation when free_only is set. 探针判定为付费墙（欠费/登录失效）的
// 模型在「仅免费」渠道里自动隐藏。
package custom

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/common"
	"github.com/veenyi/XingyunAPI/pkg/compat"
	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/provider"
)

const (
	settingKey      = "custom_providers"
	maxModelEntries = 200 // 模型顺序条目上限
)

// errNoStore is returned when the manager has no settings backend.
var errNoStore = errors.New("自定义渠道存储不可用")

// Settings is the custom package's view of the store.
type Settings interface {
	GetSetting(key string) string
	SetSetting(key, value string) error
}

// ProviderJSON is one stored row of the custom_providers blob.
type ProviderJSON struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	BaseURL     string   `json:"base_url,omitempty"`
	APIKey      string   `json:"api_key,omitempty"`
	Models      []string `json:"models,omitempty"`
	Allowed     []string `json:"allowed,omitempty"`
	Blocked     []string `json:"blocked,omitempty"`
	FreeOnly    bool     `json:"free_only,omitempty"`
	PriceFactor float64  `json:"price_factor,omitempty"`
	Enabled     bool     `json:"enabled"`
}

// ProviderEntry is one channel as surfaced by ListEntries / consumed by Save.
type ProviderEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	BaseURL     string   `json:"base_url,omitempty"`
	APIKey      string   `json:"api_key,omitempty"`
	Models      []string `json:"models,omitempty"`
	Allowed     []string `json:"allowed,omitempty"`
	Blocked     []string `json:"blocked,omitempty"`
	FreeOnly    bool     `json:"free_only,omitempty"`
	PriceFactor float64  `json:"price_factor,omitempty"`
	Enabled     bool     `json:"enabled"`
}

// Manager persists and serves the user-defined channels.
type Manager struct {
	st     Settings
	health *health.Registry

	mu   sync.Mutex
	list []*Provider
}

// New creates an empty manager; call Load to hydrate it from settings.
func New(st Settings) *Manager {
	return &Manager{st: st}
}

// SetRegistry attaches the health registry used for cooldown marking.
func (m *Manager) SetRegistry(h *health.Registry) {
	m.mu.Lock()
	m.health = h
	list := append([]*Provider(nil), m.list...)
	m.mu.Unlock()
	for _, p := range list {
		p.setHealth(h)
	}
}

// loadRawEntries reads and parses the raw custom_providers blob.
func (m *Manager) loadRawEntries() ([]ProviderJSON, error) {
	if m.st == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(m.st.GetSetting(settingKey))
	if raw == "" {
		return nil, nil
	}
	var rows []ProviderJSON
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, fmt.Errorf("解析 custom_providers 失败: %w", err)
	}
	return rows, nil
}

// Load re-reads custom_providers and rebuilds every provider.
func (m *Manager) Load() error {
	rows, err := m.loadRawEntries()
	if err != nil {
		return fmt.Errorf("加载自定义渠道失败: %w", err)
	}
	m.mu.Lock()
	h := m.health
	m.mu.Unlock()
	list := make([]*Provider, 0, len(rows))
	for _, row := range rows {
		list = append(list, m.buildProvider(row, h))
	}
	m.mu.Lock()
	m.list = list
	m.mu.Unlock()
	return nil
}

// Save validates, persists and reloads the channel list (full replace).
func (m *Manager) Save(entries []ProviderEntry) error {
	if m.st == nil {
		return errNoStore
	}
	rows := make([]ProviderJSON, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("渠道名称不能为空")
		}
		if len(e.Models) > maxModelEntries {
			return fmt.Errorf("模型顺序条目过多")
		}
		id := strings.TrimSpace(e.ID)
		if id == "" {
			id = fmt.Sprintf("cp_%d", time.Now().UnixNano())
		}
		rows = append(rows, ProviderJSON{
			ID:          id,
			Name:        strings.TrimSpace(e.Name),
			BaseURL:     strings.TrimSpace(e.BaseURL),
			APIKey:      e.APIKey,
			Models:      e.Models,
			Allowed:     e.Allowed,
			Blocked:     e.Blocked,
			FreeOnly:    e.FreeOnly,
			PriceFactor: e.PriceFactor,
			Enabled:     e.Enabled,
		})
	}
	blob, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if err := m.st.SetSetting(settingKey, string(blob)); err != nil {
		return err
	}
	return m.Load()
}

// ListEntries returns every stored channel entry.
func (m *Manager) ListEntries() []ProviderEntry {
	m.mu.Lock()
	list := append([]*Provider(nil), m.list...)
	m.mu.Unlock()
	out := make([]ProviderEntry, 0, len(list))
	for _, p := range list {
		out = append(out, p.Entry())
	}
	return out
}

// KeylessList returns the channels as routing candidates (route.Router
// filters by Enabled/Supports itself).
func (m *Manager) KeylessList() []provider.Keyless {
	m.mu.Lock()
	list := append([]*Provider(nil), m.list...)
	m.mu.Unlock()
	out := make([]provider.Keyless, 0, len(list))
	for _, p := range list {
		out = append(out, p)
	}
	return out
}

// Providers returns the live providers (diagnostics / status pages).
func (m *Manager) Providers() []*Provider {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Provider(nil), m.list...)
}

// buildProvider wires one stored row into a chat-capable provider.
func (m *Manager) buildProvider(row ProviderJSON, h *health.Registry) *Provider {
	p := &Provider{entry: row, health: h, paid: map[string]paidMark{}}
	p.baseURL = normalizeCustomBaseURL(row.BaseURL)
	p.sc = compat.New(compat.Options{
		Name:    row.Name,
		BaseURL: p.baseURL,
		APIKey:  row.APIKey,
		Models:  append([]string(nil), row.Models...),
		Allowed: p.visibleFilter,
		Tier:    p.Tier(),
	})
	return p
}

// normalizeCustomBaseURL accepts a root URL and defaults the path to /v1
// (根地址即可，自动尝试 /v1/models)。Local http targets stay allowed for
// user-defined upstreams.
func normalizeCustomBaseURL(raw string) string {
	u, err := common.NormalizeBaseURLLocal(raw)
	if err != nil {
		return strings.TrimSuffix(strings.TrimSpace(raw), "/")
	}
	if u.Path == "" {
		u.Path += "/v1"
	}
	return strings.TrimSuffix(u.String(), "/")
}
