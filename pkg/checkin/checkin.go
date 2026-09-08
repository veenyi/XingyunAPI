// Package checkin implements the multi-platform auto check-in manager
// (CodeBuddy/WorkBuddy, TraeWork, Qoder) added in v0.7.2:
//
//   - WorkBuddy/CodeBuddy: POST /v2/billing/meter/daily-checkin (签到),
//     /v2/billing/meter/get-user-resource (积分), WeChat-scan login via
//     copilot.tencent.com /v2/plugin/auth/state + /v2/plugin/login/account.
//   - TraeWork: /trae/api/v2/ug/checkin_credits/status + /claim with a strict
//     device fingerprint (x-device-id); random device ids are rejected with
//     risk code 9074.
//   - Qoder: no check-in action, only quota refresh via /api/v2/quota/usage;
//     login delegates to qoder.LoginManager (device flow).
//
// Accounts live in the settings blob (key checkin_accounts) with tokens
// encrypted at rest (enc_access/enc_refresh/enc_machine_token); per-account
// runtime state (last_checkin_at/last_checkin_ok/credits) lives in the
// checkin_state settings key; the schedule lives in checkin_times
// (default ["09:00"], each time fires once per day).
package checkin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Platform identifiers (frontend CheckinAccount.platform values).
const (
	platformWorkBuddy = "workbuddy"
	platformTraeWork  = "traework"
	platformQoder     = "qoder"
	platformQwenWork  = "qwenwork"
)

// Settings keys (endpoints-and-schema §3).
const (
	timesKey    = "checkin_times"    // JSON []string of HH:mm
	stateKey    = "checkin_state"    // JSON map[id]checkinState
	accountsKey = "checkin_accounts" // JSON []storedAccount blob
)

// Upstream bases & endpoints (endpoints-and-schema §1.3, binary-confirmed).
const (
	wbBase = "https://www.codebuddy.cn"
	// wbCheckinPath 每日签到；wbUsagePath 积分查询。
	wbCheckinPath = "/v2/billing/meter/daily-checkin"
	wbUsagePath   = "/v2/billing/meter/get-user-resource"

	wbAuthBase      = "https://copilot.tencent.com"
	wbAuthStatePath = "/v2/plugin/auth/state"
	wbAuthTokenPath = "/v2/plugin/auth/token"
	wbLoginPath     = "/v2/plugin/login/account"

	twBase = "https://api.trae.cn"
	// twStatusPath 签到状态；twClaimPath 领取签到积分。
	twStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	twEntUsagePath = "/trae/api/v2/pay/web_user_ent_usage"
	twClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"

	// qoderUsagePath 只刷积分，无签到动作（qoder.OpenAPIBase 前缀）。
	qoderUsagePath = "/api/v2/quota/usage"

	// QwenWork（千问办公）同 qoder：无签到动作，只刷积分。
	// 端点常量见 qwenwork.go。
)

// User-visible copy（strings-notes §1 checkin 段，一字不差）。
const (
	textCheckinOK         = "签到成功"
	textCheckedIn         = "已签到"
	textCheckedToday      = "今日已签到"
	textNotCheckedIn      = "未签到"
	textCheckinFailed     = "签到请求失败"
	textScheduleFired     = "到达定时时刻，开始自动签到"
	textDisabled          = "签到功能未启用"
	textCheckinUnfinished = "签到未完成"

	errRateLimited9074   = "签到被限流（9074），请稍后重试"
	errDeviceFingerprint = "TraeWork 签到强校验设备指纹：device_id 必须是账号真实注册的设备 ID，随机值会以 9074 被拒"

	errStateQuery      = "查询签到状态失败"
	errStateParse      = "签到状态解析失败"
	errCheckinParse    = "签到响应解析失败"
	errCreditsParse    = "积分响应解析失败"
	errRefreshFallback = "刷新失败，改用现有 access token 继续签到"

	errNoAccessToken = "缺少 access_token，请编辑签到账号补充凭据"
	errNoQoderCreds  = "未找到 Qoder 账号凭据，请先通过设备流登录添加"
)

// tokenRefreshInterval 是「行云定期刷新 token」的周期（对齐 qoder/workbuddy）。
const tokenRefreshInterval = 6 * time.Hour

// defaultTimes 是 checkin_times 的缺省时刻表。
var defaultTimes = []string{"09:00"}

// checkinHTTP 是包级默认 HTTP 客户端。
var checkinHTTP = &http.Client{Timeout: 30 * time.Second}

// errAlreadyCheckedIn 哨兵错误：上游回报当日已签到（run 据此转为
// ok=false + 「今日已签到」提示，对齐前端 toast 的 info 分支）。
var errAlreadyCheckedIn = errors.New(textCheckedToday)

// errWBPending 哨兵错误：WorkBuddy 扫码会话尚未被确认（继续轮询）。
var errWBPending = errors.New("login pending")

// creditMarkers 命中说明上游回报额度类问题（积分不足/每日上限等），
// 原样透传给前端展示（strings-notes §1 checkin 段）。
var creditMarkers = []string{
	"积分不足", "积分用完", "额度用尽", "余额不足", "配额用尽", "每日上限",
	"insufficient", "out of credit", "quota exhaust",
}

// alreadyCheckedMarkers 命中说明上游回报当日已签到。
var alreadyCheckedMarkers = []string{
	"已签到", "今日已签", "重复签到", "already", "checked_in", "checked in",
	"duplicate",
}

// randHex 返回 n 字节的随机 hex 字符串（2n 个字符）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 随机源失败时退化为时间戳，保证 id 唯一性
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// nowStr 是状态字段的统一时间戳格式（RFC3339，前端 dayjs 可直接解析）。
func nowStr() string { return time.Now().Format(time.RFC3339) }

// todayStr 返回本地日期（每时刻每天一次的守卫键）。
func todayStr(t time.Time) string { return t.Format("2006-01-02") }

// normalizeTimes 校验/去重/排序 HH:mm 时刻列表（非法项丢弃）。
func normalizeTimes(times []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(times))
	for _, t := range times {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, err := time.ParseInLocation("15:04", t, time.Local); err != nil {
			slog.Warn("checkin: 忽略非法签到时刻", "time", t)
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// containsFold 报告 s 是否包含任一子串（大小写不敏感）。
func containsFold(s string, subs []string) bool {
	low := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(low, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
