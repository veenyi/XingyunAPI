// Package keyed implements the BYO-key preset channels: official
// OpenAI-compatible free tiers (OpenAI、OpenRouter、Groq、Cerebras、智谱 等)
// unlocked by pasting the user's own API key. The preset registry backs the
// /api/channel-presets payload; each channel is a thin wrapper over
// pkg/compat.
package keyed

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/veenyi/XingyunAPI/pkg/common"
	"github.com/veenyi/XingyunAPI/pkg/compat"
	"github.com/veenyi/XingyunAPI/pkg/provider"
)

// tierKeyed is the dispatch tier of keyed channels (free pools dispatch
// first, custom channels last; see route.tierOf).
const tierKeyed = 40

// Settings is keyed's view of the store: plain setting reads plus the
// api_key encryption pair (store.Encrypt/Decrypt).
type Settings interface {
	GetSetting(key string) string
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// Preset is one official preset channel (element of /api/channel-presets).
type Preset struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	BaseURL   string `json:"base_url"`
	KeyPrefix string `json:"key_prefix,omitempty"` // e.g. nvapi- for NVIDIA
	FreeTier  bool   `json:"free_tier,omitempty"`  // has an official free tier
}

// presetRegistry is the official free preset registry (strings-notes §4/§5).
var presetRegistry = []Preset{
	{ID: "openai", Name: "OpenAI", Note: "GPT 系列官方接口", BaseURL: "https://api.openai.com/v1"},
	{ID: "openrouter", Name: "OpenRouter", Note: "聚合多家，带 :free 后缀的模型免费", BaseURL: "https://openrouter.ai/api/v1"},
	{ID: "groq", Name: "Groq", Note: "免费层高速推理（Llama/Qwen 等）", BaseURL: "https://api.groq.com/openai/v1", FreeTier: true},
	{ID: "mistral", Name: "Mistral", Note: "Mistral 官方接口", BaseURL: "https://api.mistral.ai/v1"},
	{ID: "cerebras", Name: "Cerebras", Note: "免费层超低延迟", BaseURL: "https://api.cerebras.ai/v1", FreeTier: true},
	{ID: "siliconflow", Name: "SiliconFlow", Note: "一个 Key 拿全站模型", BaseURL: "https://api.siliconflow.cn/v1"},
	{ID: "modelscope", Name: "ModelScope", Note: "阿里魔搭免费推理", BaseURL: "https://api-inference.modelscope.cn/v1", FreeTier: true},
	{ID: "bigmodel", Name: "BigModel", Note: "国内直连，L0 免费层", BaseURL: "https://open.bigmodel.cn/api/paas/v4", FreeTier: true},
	{ID: "nvidia", Name: "NVIDIA", Note: "免费端点，nvapi-… Key", BaseURL: "https://integrate.api.nvidia.com/v1", KeyPrefix: "nvapi-", FreeTier: true},
	{ID: "bai", Name: "B.AI", Note: "免费实验层", BaseURL: "https://api.b.ai/v1", FreeTier: true},
	{ID: "zai", Name: "Z.AI", Note: "GLM 国际版免费层", BaseURL: "https://api.z.ai/api/paas/v4", FreeTier: true},
}

// Presets returns every registered preset channel.
func Presets() []Preset { return append([]Preset(nil), presetRegistry...) }

// lookupPreset finds one preset by id.
func lookupPreset(id string) (Preset, bool) {
	for _, p := range presetRegistry {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// presetBaseURL returns the registered base URL of a preset id.
func presetBaseURL(id string) (string, bool) {
	if p, ok := lookupPreset(id); ok {
		return p.BaseURL, true
	}
	return "", false
}

// extraCatalog holds static catalog additions for presets whose /models
// omits their free entries.
var extraCatalog = map[string][]string{
	"https://open.bigmodel.cn/api/paas/v4": {"glm-4-flash"},
	"https://api.z.ai/api/paas/v4":         {"glm-4.5-flash"},
}

// extraModels returns the static catalog additions for a preset base URL.
func extraModels(baseURL string) []string {
	return append([]string(nil), extraCatalog[baseURL]...)
}

// keySettingKey is the settings key holding the encrypted api_key of a
// preset channel (masked in the UI as a secret setting).
func keySettingKey(presetID string) string { return "keyed_" + presetID + "_key" }

// setting reads a setting with a nil-safe receiver.
func setting(s Settings, key string) string {
	if s == nil {
		return ""
	}
	return s.GetSetting(key)
}

// storedKey decrypts the api_key persisted for a preset channel. Failure
// degrades gracefully: the channel runs without a key.
func storedKey(st Settings, presetID string) string {
	if st == nil || presetID == "" {
		return ""
	}
	enc := strings.TrimSpace(setting(st, keySettingKey(presetID)))
	if enc == "" {
		return ""
	}
	plain, err := st.Decrypt(enc)
	if err != nil {
		slog.Warn("解密 api_key 失败，渠道将无密钥", "preset", presetID, "error", err)
		return ""
	}
	return strings.TrimSpace(plain)
}

// Options configures one keyed preset channel.
type Options struct {
	Name     string   // channel name (defaults to the preset name)
	Preset   string   // preset id (empty when BaseURL is explicit)
	BaseURL  string   // explicit base URL, wins over the preset table
	APIKey   string   // plaintext key (wins over the encrypted setting)
	Models   []string // extra static models merged into the catalog
	Tier     int      // dispatch tier (defaults to tierKeyed)
	Disabled bool     // disable the channel (default: enabled)
	Settings Settings // store access (key lookup + api_key decryption)
}

// resolveBaseURL picks the preset base URL or normalizes an explicit one.
// Preset channels are public https endpoints only (GuardPublicHTTPS); an
// invalid explicit URL falls back to the built-in preset address.
func resolveBaseURL(opts Options) string {
	if raw := strings.TrimSpace(opts.BaseURL); raw != "" {
		u, err := common.GuardPublicHTTPS(raw)
		if err != nil {
			slog.Warn("预设渠道地址无效，使用内置地址", "raw", raw, "error", err)
		} else {
			return strings.TrimSuffix(u.String(), "/")
		}
	}
	if base, ok := presetBaseURL(strings.TrimSpace(opts.Preset)); ok {
		return base
	}
	return ""
}

// Client is one keyed preset channel (provider.Keyless + provider.Catalog).
type Client struct {
	name    string
	baseURL string
	preset  string
	st      Settings
	enabled bool
	sc      *compat.Client
}

// New builds a keyed channel from options.
func New(opts Options) *Client {
	st := opts.Settings
	presetID := strings.TrimSpace(opts.Preset)
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		if p, ok := lookupPreset(presetID); ok {
			name = p.Name
		} else {
			name = presetID
		}
	}
	baseURL := resolveBaseURL(opts)
	apiKey := strings.TrimSpace(opts.APIKey)
	if apiKey == "" {
		apiKey = storedKey(st, presetID)
	}
	tier := opts.Tier
	if tier == 0 {
		tier = tierKeyed
	}
	models := dedupe(append(append([]string(nil), opts.Models...), extraModels(baseURL)...))
	return &Client{
		name:    name,
		baseURL: baseURL,
		preset:  presetID,
		st:      st,
		enabled: !opts.Disabled && baseURL != "",
		sc: compat.New(compat.Options{
			Name:    name,
			BaseURL: baseURL,
			APIKey:  apiKey,
			Models:  models,
			Tier:    tier,
		}),
	}
}

// dedupe drops empty strings and duplicates, preserving order.
func dedupe(models []string) []string {
	seen := map[string]bool{}
	out := models[:0:0]
	for _, m := range models {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// Name implements provider.Keyless.
func (c *Client) Name() string { return c.name }

// Enabled implements provider.Keyless: the channel must resolve to a preset
// base URL, not be disabled, and the global keyed_enabled switch (when set)
// must be on.
func (c *Client) Enabled() bool {
	if !c.enabled {
		return false
	}
	if v := strings.TrimSpace(setting(c.st, "keyed_enabled")); v != "" {
		return common.SettingEnabled(c.st, "keyed_enabled")
	}
	return true
}

// Supports implements provider.Keyless.
func (c *Client) Supports(model string) bool { return c.sc.Supports(model) }

// ListModels implements provider.Keyless.
func (c *Client) ListModels() []string { return c.sc.ListModels() }

// Chat implements provider.Keyless.
func (c *Client) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	return c.sc.Chat(ctx, body)
}

// ChatStream implements provider.Keyless.
func (c *Client) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	return c.sc.ChatStream(ctx, body)
}

// Tier implements provider.Catalog.
func (c *Client) Tier() int { return c.sc.Tier() }

// ModelIDs returns the raw catalog ids (unfiltered diagnostics view).
func (c *Client) ModelIDs() []string { return c.sc.ModelIDs() }

// CatalogIDs is an alias kept for the provider.Catalog surface.
func (c *Client) CatalogIDs() []string { return c.sc.CatalogIDs() }

// ClassifyError implements provider.Catalog.
func (c *Client) ClassifyError(err error) string { return c.sc.ClassifyError(err) }

// RefreshNow implements provider.Catalog: forces a catalog re-fetch.
func (c *Client) RefreshNow(ctx context.Context) { c.sc.RefreshNow(ctx) }

// Compile-time surface checks.
var (
	_ provider.Keyless = (*Client)(nil)
	_ provider.Catalog = (*Client)(nil)
)
