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
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/compat"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

const (
	settingKey = "custom_providers"
	// SettingBlocklist 是手动屏蔽的模型清单：JSON 数组 ["provider|model", ...]。
	// 看板"隐藏"按钮写这里，被屏蔽的模型从目录与路由里同时消失。
	SettingBlocklist = "model_blocklist"
)

// ProviderJSON 是 settings 里的序列化形式：api_key 以 hex 编码的 AES-GCM 密文存放。
type ProviderJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"` // 密文（存库）或明文（运行时）
	Enabled bool   `json:"enabled"`
	// FreeOnly 只保留免费模型：探针判定为欠费/登录失效（Deposit required、
	// premium 付费墙）的模型从目录里隐去。官方免费策略随时会变，配合
	// 定期目录刷新自动跟进，不需要手动维护名单。
	FreeOnly bool `json:"free_only,omitempty"`
}

// Provider 是运行时的自定义渠道实例，包装一个 compat.Client 并实现 provider.Keyless。
type Provider struct {
	id        string
	name      string
	baseURL   string
	apiKey    string // 已解密
	enabled   bool
	freeOnly  bool
	client    *compat.Client
	tier      string
	healthReg *health.Registry
	mgr       *Manager

	// hidden 缓存 freeOnly 模式下被隐藏的模型集合，30s 刷新一次，
	// 避免每个请求都去拷贝整张健康表。
	hiddenMu sync.Mutex
	hiddenAt time.Time
	hidden   map[string]bool

	// blockMu/blockList 缓存手动屏蔽名单，10s 刷新一次。
	blockMu   sync.Mutex
	blockAt   time.Time
	blockList map[string]bool
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
	APIKey   string `json:"api_key,omitempty"`
	Enabled  bool   `json:"enabled"`
	FreeOnly bool   `json:"free_only,omitempty"`
}

// ListEntries 返回前端所需的全部渠道条目（含占位密钥）。
func (m *Manager) ListEntries() []ProviderEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ProviderEntry, len(m.providers))
	for i, p := range m.providers {
		out[i] = ProviderEntry{
			ID:       p.id,
			Name:     p.name,
			BaseURL:  p.baseURL,
			APIKey:   store.SecretMask,
			Enabled:  p.enabled,
			FreeOnly: p.freeOnly,
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
		freeOnly: ej.FreeOnly,
		tier:    provider.TierKeyed,
		mgr:     m,
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
		Allow:          p.blocked,       // 手动屏蔽：完整目录与可见名单都消失
		Visible:        p.visibleFilter, // free_only 付费墙：只从可见名单收缩，探针仍可复活
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
			ID:       entry.ID,
			Name:     strings.TrimSpace(entry.Name),
			BaseURL:  strings.TrimSpace(entry.BaseURL),
			Enabled:  entry.Enabled,
			FreeOnly: entry.FreeOnly,
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
// Tier 计费边界：仅免费模式加入公共免费池（与 opencode-free 互相轮询兜底），
// 常规模式与其他自带 Key 渠道同一边界。
func (p *Provider) Tier() string {
	if p.freeOnly {
		return provider.TierFree
	}
	return p.tier
}
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

// RefreshNow 强制重拉该渠道的模型目录。
func (p *Provider) RefreshNow() ([]string, error) {
	return p.client.RefreshNow()
}

// RefreshAll 强制重拉所有已启用自定义渠道的目录，返回每个渠道的结果。
func (m *Manager) RefreshAll() map[string]error {
	out := map[string]error{}
	for _, p := range m.Providers() {
		if !p.enabled {
			continue
		}
		if _, err := p.RefreshNow(); err != nil {
			out[p.name] = err
		} else {
			out[p.name] = nil
		}
	}
	return out
}

// blocked 是 compat.Config.Allow：手动屏蔽名单，作用于完整目录与可见名单。
// 被屏蔽的模型从目录、路由、看板、探针同时消失（用户主动永久排除的语义）。
func (p *Provider) blocked(id string) bool {
	return !p.isBlocked(id)
}

// visibleFilter 是 compat.Config.Visible：free_only 模式下把踩在付费墙上的模型
// 从"对外可见名单"里收缩掉，但完整目录（ModelsAll）仍保留，探针能回采复活。
func (p *Provider) visibleFilter(id string) bool {
	if p.freeOnly && p.isPaidModel(id) {
		return false
	}
	return true
}

// isBlocked 查手动屏蔽名单（settings.model_blocklist，10s 缓存）。
func (p *Provider) isBlocked(id string) bool {
	p.blockMu.Lock()
	defer p.blockMu.Unlock()
	if p.blockList == nil || time.Since(p.blockAt) > 10*time.Second {
		p.blockList = map[string]bool{}
		if p.mgr != nil && p.mgr.store != nil {
			raw := strings.TrimSpace(p.mgr.store.GetSetting(SettingBlocklist))
			if raw != "" {
				var keys []string
				if json.Unmarshal([]byte(raw), &keys) == nil {
					for _, k := range keys {
						p.blockList[strings.TrimSpace(k)] = true
					}
				}
			}
		}
		p.blockAt = time.Now()
	}
	return p.blockList[strings.ToLower(p.name+"|"+id)] ||
		p.blockList[strings.ToLower(health.Key(p.name, id))]
}

// isPaidModel 判定模型当前是否踩在付费墙上：探针/真实流量把该模型标记为
// 欠费（no_credit）或登录失效（auth_failed，B.AI 的 "Deposit required to
// unlock premium models" 归这类）时视为付费。30s 缓存健康表快照。
func (p *Provider) isPaidModel(id string) bool {
	if p.healthReg == nil {
		return false
	}
	p.hiddenMu.Lock()
	defer p.hiddenMu.Unlock()
	if p.hidden == nil || time.Since(p.hiddenAt) > 30*time.Second {
		p.hidden = map[string]bool{}
		for key, st := range p.healthReg.Snapshot() {
			if st.Class == health.ClassNoCredit || st.Class == health.ClassAuth {
				p.hidden[strings.ToLower(key)] = true
			}
		}
		p.hiddenAt = time.Now()
	}
	return p.hidden[strings.ToLower(health.Key(p.name, id))]
}
