// Package custom 实现用户自定义 OpenAI 兼容上游渠道。
// 每个渠道由 name/base_url/api_key 描述，api_key 以 AES-GCM 加密后存于 settings.custom_providers JSON。
package custom

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/compat"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

const settingKey = "custom_providers"

// ProviderJSON 是 settings 里的序列化形式：api_key 以 hex 编码的 AES-GCM 密文存放。
type ProviderJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"` // 密文（存库）或明文（运行时）
	Enabled bool   `json:"enabled"`
}

// Provider 是运行时的自定义渠道实例，包装一个 compat.Client 并实现 provider.Keyless。
type Provider struct {
	id        string
	name      string
	baseURL   string
	apiKey    string // 已解密
	enabled   bool
	client    *compat.Client
	tier      string
	healthReg *health.Registry
}

// Manager 持有所有自定义渠道的运行时状态，从 store 加载、向 store 写入。
type Manager struct {
	mu        sync.RWMutex
	store     *store.Store
	version   string
	reg       *health.Registry
	providers []*Provider
}

// New 创建管理器但不立即加载；调用 Load() 后才可用。
func New(s *store.Store, version string) *Manager {
	return &Manager{store: s, version: version}
}

// SetRegistry 注入健康度注册表（在 serve.go 启动时调用）。
func (m *Manager) SetRegistry(reg *health.Registry) {
	m.reg = reg
}

// ProviderEntry 是前端表单的读写结构。
type ProviderEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	// APIKey 为空或 SecretMask 时不修改；否则视为新凭据。
	APIKey  string `json:"api_key,omitempty"`
	Enabled bool   `json:"enabled"`
}

// ListEntries 返回前端所需的全部渠道条目（含占位密钥）。
func (m *Manager) ListEntries() []ProviderEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ProviderEntry, len(m.providers))
	for i, p := range m.providers {
		out[i] = ProviderEntry{
			ID:      p.id,
			Name:    p.name,
			BaseURL: p.baseURL,
			APIKey:  store.SecretMask,
			Enabled: p.enabled,
		}
	}
	return out
}

func (m *Manager) Providers() []*Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Provider, len(m.providers))
	copy(out, m.providers)
	return out
}

func (m *Manager) Load() error {
	if m.store == nil {
		return nil
	}
	raw := m.store.GetSetting(settingKey)
	if raw == "" {
		m.mu.Lock()
		m.providers = nil
		m.mu.Unlock()
		return nil
	}
	var entries []ProviderJSON
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		slog.Error("custom: 解析 custom_providers 失败", "error", err)
		m.mu.Lock()
		m.providers = nil
		m.mu.Unlock()
		return nil
	}
	providers := make([]*Provider, 0, len(entries))
	for _, ej := range entries {
		p, err := m.buildProvider(ej)
		if err != nil {
			slog.Warn("custom: 构建渠道失败，跳过", "id", ej.ID, "error", err)
			continue
		}
		providers = append(providers, p)
	}
	m.mu.Lock()
	m.providers = providers
	m.mu.Unlock()
	return nil
}

func (m *Manager) buildProvider(ej ProviderJSON) (*Provider, error) {
	if strings.TrimSpace(ej.Name) == "" {
		return nil, fmt.Errorf("渠道名称不能为空")
	}
	name := strings.TrimSpace(ej.Name)
	baseURL := strings.TrimSpace(ej.BaseURL)

	p := &Provider{
		id:      ej.ID,
		name:    name,
		baseURL: baseURL,
		enabled: ej.Enabled,
		tier:    provider.TierKeyed,
	}
	if ej.APIKey != "" {
		dec, err := m.store.Decrypt(ej.APIKey)
		if err != nil {
			slog.Warn("custom: 解密 api_key 失败，渠道将无密钥", "id", ej.ID, "error", err)
		} else {
			p.apiKey = dec
		}
	}
	p.client = compat.New(compat.Config{
		Name:           name,
		Version:        m.version,
		Enabled:        func() bool { return p.enabled },
		BaseURL:        func() string { return baseURL },
		APIKey:         func() string { return p.apiKey },
		Health:         m.reg,
		DefaultBaseURL: baseURL,
	})
	return p, nil
}

// Save 将当前 providers 序列化并写入 store。
// incoming 是前端提交的新列表：若 entry.APIKey 非空且非 SecretMask，则加密后覆盖旧值；
// 否则保留现有密文（即用户未改）。
func (m *Manager) Save(incoming []ProviderEntry) error {
	if m.store == nil {
		return fmt.Errorf("store not available")
	}
	// 先拿到现有密文映射，方便未改动时回填
	m.mu.RLock()
	oldEntries := m.loadRawEntries()
	m.mu.RUnlock()
	oldByKey := make(map[string]string, len(oldEntries))
	for _, ej := range oldEntries {
		oldByKey[ej.ID] = ej.APIKey
	}

	out := make([]ProviderJSON, 0, len(incoming))
	for _, entry := range incoming {
		if strings.TrimSpace(entry.Name) == "" {
			continue
		}
		ej := ProviderJSON{
			ID:      entry.ID,
			Name:    strings.TrimSpace(entry.Name),
			BaseURL: strings.TrimSpace(entry.BaseURL),
			Enabled: entry.Enabled,
		}
		switch {
		case entry.APIKey == "" || entry.APIKey == store.SecretMask:
			// 未改：保留旧密文
			ej.APIKey = oldByKey[entry.ID]
		default:
			enc, err := m.store.Encrypt(entry.APIKey)
			if err != nil {
				slog.Error("custom: 加密 api_key 失败", "id", entry.ID, "error", err)
				continue
			}
			ej.APIKey = enc
		}
		out = append(out, ej)
	}

	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return m.store.SetSetting(settingKey, string(data))
}

func (m *Manager) loadRawEntries() []ProviderJSON {
	raw := m.store.GetSetting(settingKey)
	if raw == "" {
		return nil
	}
	var entries []ProviderJSON
	if json.Unmarshal([]byte(raw), &entries) != nil {
		return nil
	}
	return entries
}

// KeylessList 返回所有已启用的 Provider，供 serve.go 注册到路由。
func (m *Manager) KeylessList() []provider.Keyless {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]provider.Keyless, 0, len(m.providers))
	for _, p := range m.providers {
		if p.enabled {
			out = append(out, p)
		}
	}
	return out
}

// --- provider.Keyless / provider.Chat 实现 ---

func (p *Provider) Name() string        { return p.name }
func (p *Provider) Enabled() bool       { return p.enabled }
func (p *Provider) Tier() string        { return p.tier }
func (p *Provider) Supports(model string) bool {
	return p.client.Supports(model)
}
func (p *Provider) ListModels() ([]joycode.ModelInfo, error) {
	return p.client.ListModels()
}
func (p *Provider) Chat(body map[string]interface{}) (map[string]interface{}, error) {
	return p.client.Chat(body)
}
func (p *Provider) ChatStream(body map[string]interface{}) (*http.Response, error) {
	return p.client.ChatStream(body)
}

// CatalogIDs 供 route.KeylessSource 包装为 Source 时使用（通过 catalogLister 接口）。
func (p *Provider) CatalogIDs() []string {
	return p.client.CatalogIDs()
}
