// Package qoder implements the Qoder CN account pool: device-flow login
// (deviceToken poll/refresh with PKCE + Cosy protocol), multi-account
// picking with sticky routing, quota-gated re-enable and model-name
// normalization. It is one of the provider.Keyless channels.
package qoder

import "time"

// Upstream bases (strings-notes §5).
const (
	// OpenAPIBase 是 Qoder 开放平台（设备流登录 + 用量）。
	OpenAPIBase = "https://openapi.qoder.com.cn"
	// GatewayBase 是聊天网关。
	GatewayBase = "https://gateway.qoder.com.cn"
)

// Upstream endpoints.
const (
	deviceTokenPollPath   = "/api/v1/deviceToken/poll"
	deviceTokenRefreshPath = "/api/v1/deviceToken/refresh"
	quotaUsagePath         = "/api/v2/quota/usage"
	chatPath               = "/api/v1/chat/completions"
	modelsPath             = "/api/v1/models"
)

// Channel identity & settings keys.
const (
	// Name 是渠道名（route 前缀 / 排序 / 健康键的 provider 段）。
	Name = "qoder"
	// SettingsKey 是账号池在 settings 表中的 blob 键。
	SettingsKey = "qoder_accounts"
	// EnabledKey 是渠道开关的 settings 键（缺省 = 有账号即启用）。
	EnabledKey = "qoder_enabled"
)

// 设备流授权页常量（Qoder CN 客户端 app.asar + v0.7.2 反汇编双向实证，非推断）。
const (
	// DeviceAuthBase 是设备流授权页 host（environments.prod.authBaseUrl；
	// 浏览器打开，加载时由上游注册 nonce）。
	DeviceAuthBase = "https://qoder.cn"
	// DeviceClientID 是 Qoder CN IDE 设备流 client_id（IDE 硬编码）。
	DeviceClientID = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
	// DeviceRedirectURI 是授权完成后的深链协议。
	DeviceRedirectURI = "qoder-work-cn://"
)

// Cosy protocol constants（strings-notes §7）。
const (
	// authScheme 是聊天网关 Authorization 头的 token 前缀：Bearer COSY.<token>。
	// 注意：仅网关使用，OpenAPI 家族用裸 Bearer（见下方 openapi* 常量）。
	authScheme = "COSY."

	// 以下头取值未从二进制还原，取行业惯例值（见重建报告推断清单）。
	headerCosyClient   = "qoder-ide"
	headerLoginVersion = "1"
	defaultAppVersion  = "2.0.0"
	defaultRegion      = "CN"
	defaultMachineType = "PC"
)

// OpenAPI 鉴权头（Qoder CN 客户端 app.asar 实证：
// {"Accept":"application/json","Authorization":`Bearer ${token}`,
//  "Cosy-ClientType":String(qt.clientType),"User-Agent":"Qoder"}）。
const (
	userInfoPath      = "/api/v1/userinfo"
	openapiClientType = "10"
	openapiUserAgent  = "Qoder"
)

// User-visible copy（逐字，strings-notes §1）。
const (
	// TextAccountAdded 是设备流登录成功后写入账号备注的文案。
	TextAccountAdded = "账号通过设备流登录添加"
	// TextLoginHint 是登录初始化时返回给前端的提示文案。
	TextLoginHint = "授权完成后将自动添加账号；行云会定期刷新 token 保持在线"

	errSaveAccounts   = "保存 Qoder 登录账号失败"
	errNoRefreshToken = "缺少 refreshToken，请重新登录"
	errNoTokenInResp  = "刷新失败：响应中没有 Token，请重新登录"
	errSessionGone    = "登录会话不存在或已过期"
	errLoginExpired   = "登录已过期，请重新登录"
	errInvalidCreds   = "登录凭证无效，请重新登录"
	errNoUserID       = "账号信息缺少用户标识"
)

// maxAccountAttempts 是单次请求内账号轮换的上限（对齐 route.maxCandidates）。
const maxAccountAttempts = 3

// refreshInterval 是「行云定期刷新 token 保持在线」的周期。
const refreshInterval = 6 * time.Hour

// sessionTTL 是设备流登录会话的有效期。
const sessionTTL = 5 * time.Minute
