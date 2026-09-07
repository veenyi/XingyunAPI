package checkin

// TraeWork 平台实现：
//   - 签到状态：GET /trae/api/v2/ug/checkin_credits/status
//   - 领取积分：POST /trae/api/v2/ug/checkin_credits/claim
//   - 强校验设备指纹：x-device-id 必须是账号真实注册设备，随机值返回
//     风险码 9074（文案「签到被限流（9074），请稍后重试」）。
//   - 令牌刷新/浏览器登录在 traework_login.go（ExchangeToken 式A/式B）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"io"
)

// twHeaderUserAgent 未从二进制还原，取浏览器惯例值。
const twHeaderUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

// twRiskRateLimit 是 TraeWork 风控限流码（随机设备指纹被拒）。
const twRiskRateLimit = 9074

// twUgHeaders 构造 ug 端点请求头——对齐 wild-work 的 SOLOHeaders + UgHeaders：
// Authorization 用 Cloud-IDE-JWT 前缀（不是 Bearer），需全套设备指纹头。
func twUgHeaders(a *Account) http.Header {
	h := http.Header{
		"Content-Type":    {"application/json"},
		"Accept":          {"application/json"},
		"User-Agent":      {twHeaderUserAgent},
		"X-User-Region":   {"CN"},
		"X-Device-Type":   {"windows"},
		"X-OS-Version":    {"Windows 10 Pro"},
		"X-Device-Brand":  {"20Y5A002XX"},
		"X-App-Version":   {twIDEVersion},
		"Request-Traffic-Type": {"prod"},
	}
	if a != nil {
		if a.AccessToken != "" {
			h.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
			h.Set("X-Cloudide-Token", a.AccessToken)
			h.Set("X-Ide-Token", a.AccessToken)
		}
		if a.UID != "" {
			h.Set("X-Uid", a.UID)
		}
		if a.DeviceID != "" {
			h.Set("x-device-id", a.DeviceID)
			h.Set("X-Device-Id", a.DeviceID)
		}
		if a.MachineID != "" {
			h.Set("X-Machine-Id", a.MachineID)
		}
	}
	return h
}

// twMsg 从宽容解析的响应对象提取业务消息（msg/message/error）。
func twMsg(env map[string]interface{}) string {
	for _, k := range []string{"msg", "message", "error", "error_message"} {
		if v, ok := env[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			default:
				if v != nil {
					return fmt.Sprint(v)
				}
			}
		}
	}
	return ""
}

// twCode 从响应提取业务码（code/risk_code，宽容数字/字符串）。
func twCode(env map[string]interface{}) (int, bool) {
	for _, k := range []string{"risk_code", "code", "status_code"} {
		v, ok := env[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			return int(t), true
		case string:
			if n, err := strconv.Atoi(t); err == nil {
				return n, true
			}
		case json.Number:
			n, _ := t.Int64()
			return int(n), true
		}
	}
	return 0, false
}

// twEnvelope 解码 TraeWork 包裹层（code/msg/data）。
func twEnvelope(raw map[string]interface{}) (code int, msg string, data map[string]interface{}) {
	code, _ = twCode(raw)
	msg = twMsg(raw)
	if d, ok := raw["data"].(map[string]interface{}); ok {
		data = d
	}
	return code, msg, data
}

// twCheckin 执行每日签到（claim），返回结果文案。
// 设备指纹缺失直接报错（9074 前置提示）；风控码 9074 转专用文案。
func (m *Manager) twCheckin(ctx context.Context, a *Account) (string, error) {
	if a.DeviceID == "" {
		return "", errors.New(errDeviceFingerprint)
	}
	if a.AccessToken == "" {
		return "", fmt.Errorf("%s：%s", textCheckinUnfinished, errNoAccessToken)
	}
	body := map[string]interface{}{
		"device_id":    a.DeviceID,
		"machine_id":   a.MachineID,
		"machine_type": a.MachineType,
	}
	var out map[string]interface{}
	if err := doJSON(ctx, m.hc, http.MethodPost, a.twHost()+twClaimPath, twUgHeaders(a), body, &out); err != nil {
		return "", err
	}
	code, msg, data := twEnvelope(out)
	if code == twRiskRateLimit || containsFold(msg, []string{"9074"}) {
		twRiskRateLimited()
		return "", errors.New(errRateLimited9074)
	}
	if !twOKCode(code) {
		if containsFold(msg, alreadyCheckedMarkers) {
			return "", errAlreadyCheckedIn
		}
		if containsFold(msg, creditMarkers) {
			return "", errors.New(msg)
		}
		if msg == "" {
			msg = textCheckinFailed
		}
		return "", errors.New(msg)
	}
	if data != nil && truthy(data["checked_in"]) {
		return "", errAlreadyCheckedIn
	}
	if data != nil {
		if v, ok := data["checked_in"]; ok && !truthy(v) {
			return "", errors.New(textNotCheckedIn)
		}
	}
	if containsFold(msg, alreadyCheckedMarkers) {
		return "", errAlreadyCheckedIn
	}
	if msg != "" {
		return msg, nil
	}
	return textCheckinOK, nil
}

// twOKCode 判断 TraeWork 业务码是否成功（0/200/负数兼容）。
func twOKCode(code int) bool {
	return code == 0 || code == 200
}

// twCredits 查询签到状态端点，返回宽容解析的响应对象。
func (m *Manager) twCredits(ctx context.Context, a *Account) (map[string]interface{}, error) {
	if a.AccessToken == "" {
		return nil, fmt.Errorf("%s：%s", textCheckinUnfinished, errNoAccessToken)
	}
	var out map[string]interface{}
	if err := doJSON(ctx, m.hc, http.MethodGet, a.twHost()+twStatusPath, twUgHeaders(a), nil, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", errStateQuery, err)
	}
	if out == nil {
		return nil, errors.New(errStateParse)
	}
	if code, msg, _ := twEnvelope(out); !twOKCode(code) {
		if msg == "" {
			msg = errStateParse
		}
		return nil, errors.New(msg)
	}
	return out, nil
}

// twCreditsF64 查询签到状态并宽容换算为 (剩余, 总量)。
func (m *Manager) twCreditsF64(ctx context.Context, a *Account) (credits, total float64, err error) {
	return m.twEntUsage(ctx, a)
}

// twProbeKeepalive 探测当前 access token 有效性（无设备私钥时的降级保活）。
func (m *Manager) twProbeKeepalive(ctx context.Context, a *Account) error {
	_, err := m.twCredits(ctx, a)
	if err != nil {
		return err
	}
	a.LastRefreshAt = nowStr()
	a.LastRefreshOK = true
	return nil
}

// logRateLimited9074 是限流日志（文案一字不差）。
const logRateLimited9074 = "checkin: 签到被限流（9074），请稍后重试"

// twRiskRateLimited 记录限流日志。
func twRiskRateLimited() {
	slog.Warn(logRateLimited9074)
}

// twHost 返回该账号的 API host（登录时按授权页回跳记录，空则 CN 默认）。
func (a *Account) twHost() string {
	if a != nil && strings.TrimSpace(a.APIHost) != "" {
		return strings.TrimSpace(a.APIHost)
	}
	return twBase
}

// TWCredential 是导出给聊天渠道的账号凭据视图。
type TWCredential struct {
	ID           string
	Nickname     string
	AccessToken  string
	RefreshToken string
	UID          string
	DeviceID     string
	MachineID    string
}

// TraeWorkCredentials 返回全部启用 traework 账号的解密凭据
// （聊天池 credsFn 回调实时取；签到侧刷新令牌后自动生效）。
func (m *Manager) TraeWorkCredentials() []TWCredential {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TWCredential, 0, len(m.accounts))
	for _, a := range m.accounts {
		if a == nil || a.Platform != platformTraeWork || !a.Enabled {
			continue
		}
		if a.AccessToken == "" {
			continue
		}
		out = append(out, TWCredential{
			ID:           a.IDKey(),
			Nickname:     a.displayName(),
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
			UID:          a.UID,
			DeviceID:     a.DeviceID,
			MachineID:    a.MachineID,
		})
	}
	return out
}

// twEntUsage 查询 TRAE 账号实际剩余积分（对齐 wild-work UserEntUsage）。
// POST {host}/trae/api/v2/pay/web_user_ent_usage body {"require_usage":true}
// 剩余 = Σ(credits_limit - credits_amount)。
func (m *Manager) twEntUsage(ctx context.Context, a *Account) (credits, total float64, err error) {
	req, err := http.NewRequest(http.MethodPost, a.twHost()+twEntUsagePath, strings.NewReader(`{"require_usage":true}`))
	if err != nil {
		return 0, 0, err
	}
	for k, vals := range twUgHeaders(a) {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	hc := m.hc
	if hc == nil {
		hc = checkinHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("ent_usage 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("ent_usage HTTP %d: %s", resp.StatusCode, string(raw)[:min(200,len(raw))])
	}
	var result struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit float64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, 0, fmt.Errorf("ent_usage 解析失败: %w", err)
	}
	for _, p := range result.UserEntitlementPackList {
		total += p.EntitlementBaseInfo.Quota.CreditsLimit
		credits += p.EntitlementBaseInfo.Quota.CreditsLimit - p.Usage.CreditsAmount
	}
	return credits, total, nil
}
