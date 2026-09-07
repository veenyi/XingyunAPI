package qoder

// 账号与错误分类：QoderAccount、poolEntry、UserResourceF64、Classify、
// hardMarkers 及小工具（hexShort/truncateRunes/uuid4/buildAgentBody/aggregate）。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/health"
)

// QoderAccount 是账号池中的一个 Qoder CN 账号。
// 令牌仅驻留内存（json:"-"），持久化时以 AES 加密为 enc_access/enc_refresh。
type QoderAccount struct {
	UserID        string  `json:"user_id"`
	Nickname      string  `json:"nickname,omitempty"`
	Remark        string  `json:"remark,omitempty"`
	AccessToken   string  `json:"-"`
	RefreshToken  string  `json:"-"`
	MachineID     string  `json:"machine_id,omitempty"`
	MachineType   string  `json:"machine_type,omitempty"`
	EnterpriseID  string  `json:"enterprise_id,omitempty"`
	Region        string  `json:"region,omitempty"`
	Credits       float64 `json:"credits,omitempty"`
	CreditsTotal  float64 `json:"credits_total,omitempty"`
	Disabled      bool    `json:"disabled,omitempty"`
	Note          string  `json:"note,omitempty"`
	LastRefreshAt string  `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool    `json:"last_refresh_ok,omitempty"`
}

// NeedsRefresh 报告账号令牌是否需要刷新（对齐「行云会定期刷新 token 保持在线」）。
func (a *QoderAccount) NeedsRefresh() bool {
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
	acct      *QoderAccount
	healthy   bool
	disabled  bool
	cooldown  time.Time
	lastErr   string
	failClass string
}

// hardMarkers 命中即硬性禁用账号（需要重新登录/充值才恢复）。
var hardMarkers = []string{
	"login pending", "out of credit", "quota exhaust",
	"登录失效", "登录已过期", "token expired", "unauthorized", "forbidden",
}

// classifyMarkers 把错误文本映射到 pkg/health 的 class。
var classifyMarkers = []struct {
	markers []string
	class   string
}{
	{[]string{"429", "rate limit", "ratelimit", "too many requests", "被限流", "限流", "frequent"}, health.ClassRateLimited},
	{[]string{"out of credit", "quota exhaust", "no_credit", "no credit", "insufficient", "额度不足", "额度用尽", "余额不足", "配额用尽", "积分不足", "积分用完", "每日上限", "402", "payment"}, health.ClassNoCredit},
	{[]string{"401", "403", "unauthorized", "forbidden", "auth_failed", "login pending", "token expired", "登录失效", "登录已过期", "invalid token", "invalid_token", "session dead"}, health.ClassAuthFailed},
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

// containsAnyFold 报告小写化后的 s 是否包含任一子串。
func containsAnyFold(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// UserResourceF64 是 quota/usage 响应数值字段的宽容视图：
// 上游可能把数值编码为字符串或嵌套对象，这里统一取 float64。
type UserResourceF64 struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
}

// parseUserResource 从解码后的 JSON 对象宽容提取额度字段（含一层嵌套）。
func parseUserResource(data map[string]interface{}) UserResourceF64 {
	var out UserResourceF64
	if data == nil {
		return out
	}
	for k, v := range data {
		switch strings.ToLower(k) {
		case "total", "total_credits", "credits_total", "totalquota":
			out.Total = anyF64(v)
		case "used", "used_credits", "consumed":
			out.Used = anyF64(v)
		case "remaining", "left", "credits", "credit", "balance", "quota":
			if out.Remaining == 0 {
				out.Remaining = anyF64(v)
			}
		}
	}
	if out.Remaining == 0 {
		for _, key := range []string{"data", "quota", "usage", "resource"} {
			if nested, ok := data[key].(map[string]interface{}); ok {
				if n := parseUserResource(nested); n.Remaining != 0 || n.Total != 0 {
					if out.Total == 0 {
						out.Total = n.Total
					}
					return UserResourceF64{Total: out.Total, Used: n.Used, Remaining: n.Remaining}
				}
			}
		}
	}
	if out.Total > 0 && out.Remaining == 0 {
		out.Remaining = out.Total - out.Used
	}
	return out
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

// hexShort 返回 8 位短 hex 随机 id。
func hexShort() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// truncateRunes 按字符（而非字节）截断，避免打断多字节字符。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// uuid4 生成随机 UUID v4（不引入外部依赖）。
func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildAgentBody 构造 gateway 聊天请求体（shape 未从二进制还原，见推断清单）：
// model 经 stdToCustom 映射，附带设备指纹与透传的采样参数。
func buildAgentBody(acct *QoderAccount, body map[string]interface{}, stream bool) map[string]interface{} {
	model := NormalizeModelName(fmt.Sprint(body["model"]))
	out := map[string]interface{}{
		"model":        customModelName(model),
		"messages":     body["messages"],
		"stream":       stream,
		"session_id":   uuid4(),
		"machine_id":   acct.MachineID,
		"machine_type": acct.MachineType,
		"user_id":      acct.UserID,
	}
	for _, k := range []string{"tools", "tool_choice", "temperature", "top_p", "max_tokens", "stop", "images"} {
		if v, ok := body[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// customModelName 把标准名映射为上游自定义名（缺省回退原名）。
func customModelName(std string) string {
	if v, ok := stdToCustom[std]; ok {
		return v
	}
	return std
}

// stdModelName 把上游自定义名映射回标准名（缺省回退原名）。
func stdModelName(custom string) string {
	if v, ok := customToStd[custom]; ok {
		return v
	}
	return custom
}

// aggregate 汇总账号池的额度（remaining/total）。
func aggregate(entries []*poolEntry) (remaining, total float64) {
	for _, e := range entries {
		if e == nil || e.acct == nil {
			continue
		}
		remaining += e.acct.Credits
		total += e.acct.CreditsTotal
	}
	return remaining, total
}

// toAnySlice 把 []map[string]interface{} 转成 []interface{}（合并工具增量用）。
func toAnySlice(in []map[string]interface{}) []interface{} {
	out := make([]interface{}, 0, len(in))
	for _, m := range in {
		out = append(out, m)
	}
	return out
}

// mergeToolCallDelta 把 tool_calls 增量按 index 合并进累计列表：
// id/type/function.name 首见生效，function.arguments 追加。
func mergeToolCallDelta(acc []map[string]interface{}, deltas []interface{}) []map[string]interface{} {
	for _, d := range deltas {
		tc, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		idx := int(anyF64(tc["index"]))
		if idx < 0 {
			idx = len(acc)
		}
		for len(acc) <= idx {
			acc = append(acc, map[string]interface{}{
				"index":    len(acc),
				"type":     "function",
				"function": map[string]interface{}{"name": "", "arguments": ""},
			})
		}
		slot := acc[idx]
		if id, _ := tc["id"].(string); id != "" {
			slot["id"] = id
		}
		if t, _ := tc["type"].(string); t != "" {
			slot["type"] = t
		}
		fn, _ := slot["function"].(map[string]interface{})
		if fn == nil {
			fn = map[string]interface{}{"name": "", "arguments": ""}
			slot["function"] = fn
		}
		if dfn, ok := tc["function"].(map[string]interface{}); ok {
			if n, _ := dfn["name"].(string); n != "" {
				if cur, _ := fn["name"].(string); cur == "" {
					fn["name"] = n
				}
			}
			if a, _ := dfn["arguments"].(string); a != "" {
				cur, _ := fn["arguments"].(string)
				fn["arguments"] = cur + a
			}
		}
	}
	return acc
}
