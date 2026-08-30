// Package keyed 接入"用户自带一个 Key"的 OpenAI 兼容上游（智谱开放平台、
// NVIDIA build 免费端点，或任何用户自己填地址的站点）。
// 一个 Key 换取该站点全部模型，与 keyfree 共用 pkg/compat 的传输与目录缓存。
// 默认关闭；密钥只经 store 的加密设置读写，任何对外输出都只出现掩码。
package keyed

import (
	"strings"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/common"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/compat"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

var (
	_ provider.Keyless = (*Client)(nil)
	_ provider.Tiered  = (*Client)(nil)
)

const (
	SettingEnabled   = "keyed_enabled"
	SettingBaseURL   = "keyed_base_url"
	SettingAPIKey    = "keyed_api_key"
	SettingExtraMode = "keyed_extra_models"
	SettingPreset    = "keyed_preset"

	// DefaultBaseURL 预置智谱开放平台；实测：无鉴权返回 401 code 1001，
	// 带 Bearer Key 时 /models 能列出该 Key 可用的全部模型。
	DefaultBaseURL = "https://open.bigmodel.cn/api/paas/v4"
	// NvidiaBaseURL 预置 build.nvidia.com 免费端点；实测 /v1/models 不需要任何
	// 凭据就返回标准 {object,data} 共 83 个模型，/v1/chat/completions 无 Key 时
	// 401 "Header of type authorization was missing"，带上 Bearer nvapi-… 即可调用。
	NvidiaBaseURL = "https://integrate.api.nvidia.com/v1"

	PresetBigmodel = "bigmodel"
	PresetNvidia   = "nvidia"
	// BaiBaseURL 预置 b.ai OpenAI 兼容端点；用用户自己的 sk-… key 调用，
	// 一个 key 拿到该平台全部模型（GPT、DeepSeek、Qwen、GLM 等）。
	BaiBaseURL = "https://api.b.ai/v1"
	PresetBai  = "bai"

	ProviderName = "B.AI"
)

type Settings interface {
	GetSetting(key string) string
	// GetSecretSetting 读取解密后的敏感设置；缺失或解不开时返回空串。
	GetSecretSetting(key string) string
}

type Client struct {
	*compat.Client
	settings Settings
}

func New(settings Settings, version string, reg *health.Registry) *Client {
	c := &Client{settings: settings}
	c.Client = compat.New(compat.Config{
		Name:           ProviderName,
		Version:        version,
		DefaultBaseURL: DefaultBaseURL,
		Enabled:        func() bool { return common.SettingEnabled(settings, SettingEnabled) && c.apiKey() != "" },
		BaseURL:        func() string { return resolveBaseURL(settings) },
		APIKey:         c.apiKey,
		Floor:          func() []string { return extraModels(settings) },
		Health:         reg,
	})
	return c
}

// resolveBaseURL 手填的地址优先，其次按预设走；每次调用都重读设置，
// 所以换预设不需要重启进程。
func resolveBaseURL(settings Settings) string {
	if raw := setting(settings, SettingBaseURL); raw != "" {
		return raw
	}
	return presetBaseURL(setting(settings, SettingPreset))
}

func presetBaseURL(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case PresetNvidia:
		return NvidiaBaseURL
	case PresetBai:
		return BaiBaseURL
	default:
		return DefaultBaseURL
	}
}

// Tier 声明这里花的是用户自己的 Key：点名本渠道模型的请求不会被切到
// 免登录公共渠道或行云订阅账号上，反之亦然。
func (c *Client) Tier() string { return provider.TierKeyed }

func (c *Client) apiKey() string {
	if c == nil || c.settings == nil {
		return ""
	}
	return strings.TrimSpace(c.settings.GetSecretSetting(SettingAPIKey))
}

// extraModels 允许补录 /models 不展示但仍可调用的模型名，逗号或空格分隔。
func extraModels(settings Settings) []string {
	raw := setting(settings, SettingExtraMode)
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == ';'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func setting(settings Settings, key string) string {
	if settings == nil {
		return ""
	}
	return strings.TrimSpace(settings.GetSetting(key))
}
