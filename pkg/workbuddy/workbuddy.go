// Package workbuddy implements the WorkBuddy (Tencent CodeBuddy) account
// pool: OpenAI-compatible chat over www.codebuddy.cn with per-account
// rotation, session-dead detection, dynamic model catalog and settings-blob
// persistence. It is one of the provider.Keyless channels.
package workbuddy

import "time"

// Upstream bases（strings-notes §5）。
const (
	// BaseURL 是聊天 + 计费端点。
	BaseURL = "https://www.codebuddy.cn"
	// AuthBase 是认证态/扫码登录。
	AuthBase = "https://copilot.tencent.com"
)

// Upstream endpoints（auth/token/refresh、chat_completions、
// console/enterprises/personal/models、get-user-resource/daily-checkin 经
// v0.7.2 二进制字符串 + workbuddy2api 权威实现 + 上游活体探测三重确认）。
const (
	chatPath         = "/v2/chat/completions"
	modelsPath       = "/console/enterprises/personal/models"
	billingUsagePath = "/v2/billing/meter/get-user-resource"
	authStatePath    = "/v2/plugin/auth/state"
	loginAccountPath = "/v2/plugin/login/account"
	refreshPath      = "/v2/plugin/auth/token/refresh"
)

// Channel identity & settings keys.
const (
	// Name 是渠道名（route 前缀 / 排序 / 健康键的 provider 段）。
	Name = "workbuddy"
	// SettingsKey 是账号池在 settings 表中的 blob 键。
	SettingsKey = "workbuddy_accounts"
	// EnabledKey 是渠道开关的 settings 键（缺省 = 有账号即启用）。
	EnabledKey = "workbuddy_enabled"
)

// User-visible copy（逐字，strings-notes §1）。
const (
	errSaveAccounts   = "保存 WorkBuddy 扫码账号失败"
	errNoRefreshToken = "缺少 refreshToken，请重新登录"
	errNoTokenInResp  = "刷新失败：响应中没有 Token，请重新登录"
)

// maxAccountAttempts 是单次请求内账号轮换的上限（对齐 route.maxCandidates）。
const maxAccountAttempts = 3

// refreshInterval 是「定期刷新 token」的周期。
const refreshInterval = 6 * time.Hour

// dynModelsTTL 是动态模型目录的刷新周期。
const dynModelsTTL = 10 * time.Minute
