package workbuddy

// 账号与错误分类：WBAccount、poolEntry、hardMarkers、sessionDeadMarkers、
// Classify、Aggregate 及宽容数值解析。

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/health"
)

// WBAccount 是账号池中的一个 WorkBuddy（腾讯 CodeBuddy）账号。
// 令牌仅驻留内存（json:"-"），持久化时以 AES 加密为 enc_access/enc_refresh。
type WBAccount struct {
	UserID        string  `json:"user_id"`
	Nickname      string  `json:"nickname,omitempty"`
	Remark        string  `json:"remark,omitempty"`
	AccessToken   string  `json:"-"`
	RefreshToken  string  `json:"-"`
	EnterpriseID  string  `json:"enterprise_id,omitempty"`
	Domain        string  `json:"domain,omitempty"`
	APIHost       string  `json:"api_host,omitempty"`
	Credits       float64 `json:"credits,omitempty"`
	CreditsTotal  float64 `json:"credits_total,omitempty"`
	Disabled      bool    `json:"disabled,omitempty"`
	Note          string  `json:"note,omitempty"`
	LastRefreshAt string  `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool    `json:"last_refresh_ok,omitempty"`
}

// NeedsRefresh 报告账号令牌是否需要刷新（定期刷新保持在线）。
func (a *WBAccount) NeedsRefresh() bool {
	if a == nil {
		return false
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return true
	}
	if !a.LastRefreshOK || a.LastRefreshAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, a.LastRefreshAt)
	if err != nil {
		return true
	}
	return time.Since(t) > refreshInterval
}

// poolEntry 是账号池的一条运行态（healthy 为健康位）。
type poolEntry struct {
	acct      *WBAccount
	healthy   bool
	disabled  bool
	cooldown  time.Time
	lastErr   string
	failClass string
}

// hardMarkers 命中即硬性禁用账号（需要重新扫码/充值才恢复）。
var hardMarkers = []string{
	"out of credit", "quota exhaust", "insufficient",
	"额度不足", "额度用尽", "余额不足", "配额用尽", "积分不足", "每日上限",
}

// sessionDeadMarkers 命中说明登录会话已失效（需重新扫码）。
// 注意：只放文本标记，不放裸状态码（"401"），避免对 chunk JSON 的
// 数值字段（如 prompt_tokens:401）误判。
var sessionDeadMarkers = []string{
	"session dead", "会话已失效", "登录已过期", "登录失效",
	"unauthorized", "forbidden", "token expired",
}

// classifyMarkers 把错误文本映射到 pkg/health 的 class。
var classifyMarkers = []struct {
	markers []string
	class   string
}{
	{[]string{"429", "rate limit", "ratelimit", "too many requests", "被限流", "限流", "frequent"}, health.ClassRateLimited},
	{[]string{"out of credit", "quota exhaust", "no_credit", "no credit", "insufficient", "额度不足", "额度用尽", "余额不足", "配额用尽", "积分不足", "每日上限", "402", "payment"}, health.ClassNoCredit},
	{[]string{"401", "403", "unauthorized", "forbidden", "auth_failed", "login pending", "token expired", "登录失效", "登录已过期", "session dead", "invalid token"}, health.ClassAuthFailed},
	{[]string{"404", "not found", "not_found", "模型不在套餐", "不支持的模型", "模型不存在", "已下架", "model_not_found"}, health.ClassNotFound},
	{[]string{"timeout", "connection refused", "connection reset", "eof", "tls", "dns", "network", "网络不通", "网络"}, health.ClassNetwork},
}

// Classify 将错误对齐到 pkg/health 的 class
// （rate_limited/no_credit/auth_failed/not_found/server_error/network）。
func Classify(err error) string {
	if err == nil {
		return ""
	}
	t := strings.ToLower(err.Error())
	for _, cm := range classifyMarkers {
		if containsAnyFold(t, cm.markers) {
			return cm.class
		}
	}
	return health.ClassServerError
}

// containsAnyFold 报告 s 是否包含任一子串（大小写不敏感）。
func containsAnyFold(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// hitsAny 报告 s 是否命中任一标记（大小写不敏感）。
func hitsAny(s string, markers []string) bool {
	return containsAnyFold(s, markers)
}

// Aggregate 汇总账号池的额度（remaining/total）。
func Aggregate(accounts []*WBAccount) (remaining, total float64) {
	for _, a := range accounts {
		if a == nil {
			continue
		}
		remaining += a.Credits
		total += a.CreditsTotal
	}
	return remaining, total
}

// anyF64 把任意解码值宽容转成 float64。
func anyF64(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

// parseUsage 从 get-user-resource 响应宽容提取额度。
func parseUsage(data map[string]interface{}) (credits, total float64) {
	if data == nil {
		return 0, 0
	}
	for k, v := range data {
		switch strings.ToLower(k) {
		case "credits", "credit", "remaining", "balance", "left":
			if credits == 0 {
				credits = anyF64(v)
			}
		case "credits_total", "total", "total_credits", "quota":
			if total == 0 {
				total = anyF64(v)
			}
		}
	}
	if credits == 0 {
		for _, key := range []string{"data", "resource", "usage", "billing"} {
			if nested, ok := data[key].(map[string]interface{}); ok {
				c, t := parseUsage(nested)
				if c != 0 || t != 0 {
					return c, t
				}
			}
		}
	}
	return credits, total
}

// errSessionDead 是会话失效的规范化错误（供 NoteError 识别）。
func errSessionDead(detail string) error {
	if detail == "" {
		detail = "session dead"
	}
	return errors.New("session dead: " + detail)
}
