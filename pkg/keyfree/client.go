// Package keyfree 接入不需要任何凭据的公共 OpenAI 兼容渠道。默认关闭，
// 由设置页的 keyfree_enabled 打开；它是虚拟渠道，不占账号、不参与保活。
// 传输与目录缓存复用 pkg/compat，本包只保留"哪些模型被实测过"这份知识。
package keyfree

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
	SettingEnabled = "keyfree_enabled"
	SettingBaseURL = "keyfree_base_url"

	DefaultBaseURL = "https://opencode.ai/zen/v1"
	ProviderName   = "opencode-free"
)

// verifiedModels 是 2026-08-29 国内网络实测可用的匿名模型地板名单；上游会随时
// 下架或限流，所以对外可见名单还要与运行时匿名目录求交，并被失败冷却收缩。
var verifiedModels = []string{
	"hy3-free",
	"laguna-s-2.1-free",
	"nemotron-3.5-lightning-free",
	"nemotron-3-ultra-free",
	"ling-3.0-flash-fin-free",
	"muse-spark-1.2-contributor-free",
}

// excludedModels 实测不可用：区域封锁 / 已下架 / 共享池常年限流 / 上游不认。
// 不在此名单里的新 free 模型不会自动放行，避免把 401/429 端点推给用户。
var excludedModels = map[string]string{
	"deepseek-v4-flash-free":   "delisted",
	"mimo-v2.5-free":                  "rate_limited",
	"big-pickle":                      "rate_limited",
}

type Settings interface {
	GetSetting(key string) string
}

// Client 包装 compat.Client，对外接口保持不变。
type Client struct {
	*compat.Client
}

func New(settings Settings, version string, reg *health.Registry) *Client {
	return &Client{compat.New(compat.Config{
		Name:           ProviderName,
		Version:        version,
		DefaultBaseURL: DefaultBaseURL,
		Enabled:        func() bool { return common.SettingEnabled(settings, SettingEnabled) },
		BaseURL:        func() string { return setting(settings, SettingBaseURL) },
		Allow:          func(modelID string) bool { return visible(modelID) },
		Floor:          func() []string { return append([]string(nil), verifiedModels...) },
		Health:         reg,
	})}
}

func setting(settings Settings, key string) string {
	if settings == nil {
		return ""
	}
	return strings.TrimSpace(settings.GetSetting(key))
}

// Tier 声明本渠道花的是公共免费额度：点名免费模型的请求只在免费渠道之间切，
// 绝不漂到用户订阅账号或他自己的 Key 上。
func (c *Client) Tier() string { return provider.TierFree }

func visible(modelID string) bool {
	if _, excluded := excludedModels[modelID]; excluded {
		return false
	}
	return true
}

func isVerified(id string) bool {
	for _, v := range verifiedModels {
		if strings.EqualFold(v, id) {
			return true
		}
	}
	return false
}
