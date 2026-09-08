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
	"time"
)

// twHeaderUserAgent ug 端点 UA——对齐 wild-work clientUA="Trae/"+IdeVersion。
const twHeaderUserAgent = "Trae/" + twIDEVersion

// twRiskRateLimit 是 TraeWork 风控限流码（wild-work CheckinClaim 同款处理：
// 等 8s 重试一次）。
const twRiskRateLimit = 9074

// twCheckinRetryDelay 是 9074 限流后的重试等待（wild-work CheckinRetryDelay 默认 8s）。
const twCheckinRetryDelay = 8 * time.Second

// twUgHeaders 构造 ug 端点请求头——逐项对齐 wild-work UgHeaders：
// Authorization 用 Cloud-IDE-JWT 前缀（不是 Bearer）+ 小写设备指纹头集合。
// 注意 X-Uid/X-Machine-Id 属 SOLOHeaders（聊天），UgHeaders 不带。
func twUgHeaders(a *Account) http.Header {
	h := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
		"User-Agent":   {twHeaderUserAgent},
		"Authorization": {"Cloud-IDE-JWT " + a.AccessToken},
		"X-User-Region": {"CN"},
		// 设备指纹头（官方客户端 bb() 注入；UG 签到/积分接口校验，
		// 缺任一环节会以 9074 拒绝）——wild-work 用小写 key。
		"X-Device-Brand": {"20Y5A002XX"},
		"X-Device-Type":  {"windows"},
		"X-OS-Version":   {"Windows 10 Pro"},
		"X-App-Version":  {twIDEVersion},
	}
	if a != nil {
		if a.AccessToken != "" {
			h.Set("X-Cloudide-Token", a.AccessToken)
			h.Set("X-Ide-Token", a.AccessToken)
		}
		if a.DeviceID != "" {
			h.Set("x-device-id", a.DeviceID)
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

// twNumericDeviceID 判断 device_id 是否为官方客户端的 15 位纯数字形态。
// 旧版登录生成的 UUID 形态会被 claim 风控以 9074 拒掉，需迁移。
func twNumericDeviceID(id string) bool {
	if len(id) != 15 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// twCheckin 执行每日签到（claim），返回结果文案。
// 对齐 wild-work CheckinClaim：body 恒为空 JSON {}（不发设备信息），
// 9074 限流等 8s 重试一次（CheckinRetryDelay），两试仍限流才报错。
func (m *Manager) twCheckin(ctx context.Context, a *Account) (string, error) {
	if a.DeviceID == "" {
		return "", errors.New(errDeviceFingerprint)
	}
	if !twNumericDeviceID(a.DeviceID) {
		// 存量 UUID 设备 ID 迁移为 15 位纯数字（9074 根治，2026-09-08 实测）
		a.DeviceID = twNumericID()
		m.mu.Lock()
		err := m.saveStored()
		m.mu.Unlock()
		if err != nil {
			slog.Warn("checkin: TraeWork device_id 迁移持久化失败", "uid", a.UID, "error", err)
		}
		slog.Info("checkin: TraeWork device_id 已迁移为数字形态", "uid", a.UID)
	}
	if a.AccessToken == "" {
		return "", fmt.Errorf("%s：%s", textCheckinUnfinished, errNoAccessToken)
	}
	for attempt := 0; attempt < 2; attempt++ {
		var out map[string]interface{}
		if err := doJSON(ctx, m.hc, http.MethodPost, a.twHost()+twClaimPath, twUgHeaders(a), map[string]interface{}{}, &out); err != nil {
			return "", err
		}
		code, msg, data := twEnvelope(out)
		if code == twRiskRateLimit || containsFold(msg, []string{"9074"}) {
			if attempt == 0 {
				twRiskRateLimited()
				slog.Warn("checkin: 9074 限流，等待后重试", "uid", a.UID, "retry_after", twCheckinRetryDelay.String())
				select {
				case <-time.After(twCheckinRetryDelay):
				case <-ctx.Done():
					return "", ctx.Err()
				}
				continue
			}
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
	return "", errors.New(errRateLimited9074)
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
