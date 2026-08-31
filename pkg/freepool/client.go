// Package freepool 接入"免 Key 的本地/远程 OpenAI 兼容聚合代理"作为免费模型池，
// 典型如 9Router、FreeLLMAPI——这类工具自身已经把几十上百个免费上游聚合好、
// 在本地暴露一个 OpenAI 兼容端点，行云只需把它当成一个免鉴权渠道挂上，
// 即可把它的整个免费目录并进统一入口，并加入免费池轮询。
//
// 与 keyfree（opencode 匿名端点，带实测地板名单）不同：freepool 完全跟随代理
// 自己的 /models 目录动态发现，不做本地白名单，代理上新/下架即时反映。
// 传输与目录缓存复用 pkg/compat。默认关闭，需用户在渠道设置里打开并填代理地址。
package freepool

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

// Settings 只读取普通设置（这些渠道免 Key，不涉及密文设置）。
type Settings interface {
	GetSetting(key string) string
}

// Client 是一个免 Key 聚合代理渠道。
type Client struct{ *compat.Client }

// New 构造一个免费池代理渠道。enabledKey/baseKey 是各自的设置开关，
// defaultURL 是留空时使用的地址（本地代理默认 http，GuardPublicHTTPS 只拦重定向）。
func New(name, enabledKey, baseKey, defaultURL, version string, s Settings, reg *health.Registry) *Client {
	return &Client{compat.New(compat.Config{
		Name:           name,
		Version:        version,
		DefaultBaseURL: defaultURL,
		Enabled:        func() bool { return common.SettingEnabled(s, enabledKey) },
		BaseURL:        func() string { return strings.TrimSpace(s.GetSetting(baseKey)) },
		Health:         reg,
	})}
}

// Tier 声明这是公共免费额度：点名本渠道模型的请求只在免费池内轮转，
// 绝不漂到用户的行云订阅账号或自带 Key 上。
func (c *Client) Tier() string { return provider.TierFree }

// 内置两个常见免 Key 聚合代理的默认地址与设置键。
const (
	Router9Name       = "9router"
	Router9EnabledKey = "router9_enabled"
	Router9BaseKey    = "router9_base_url"
	Router9DefaultURL = "http://localhost:20128/v1"

	FreeLLMName       = "freellmapi"
	FreeLLMEnabledKey = "freellm_enabled"
	FreeLLMBaseKey    = "freellm_base_url"
	FreeLLMDefaultURL = "http://localhost:8787/v1"
)

// NewRouter9 接入本地 9Router（默认 http://localhost:20128/v1）。
func NewRouter9(version string, s Settings, reg *health.Registry) *Client {
	return New(Router9Name, Router9EnabledKey, Router9BaseKey, Router9DefaultURL, version, s, reg)
}

// NewFreeLLM 接入本地 FreeLLMAPI（默认 http://localhost:8787/v1）。
func NewFreeLLM(version string, s Settings, reg *health.Registry) *Client {
	return New(FreeLLMName, FreeLLMEnabledKey, FreeLLMBaseKey, FreeLLMDefaultURL, version, s, reg)
}
