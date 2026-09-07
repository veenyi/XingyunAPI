package checkin

// QwenWork（千问办公）平台实现：
//   - 无签到动作，只刷积分：GET https://gateway.qwenwork.cn/api/v2/quota/usage
//     （Bearer auth-v2 JWT + X-QwenWork-* 头，UA "QoderWork"；响应与 Qoder
//     quota/usage 同构：user_quota/add_on_quota/dedicated_resource_packages，
//     解析复用 parseQuotaUsagePayload，qoder.go 共用）。
//   - 凭据来源：checkin blob 的 access_token/refresh_token 字段（PC 端从
//     auth-v2.dat 解密导出后经 accounts API 导入，与 workbuddy 同路径）。
//   - 刷新：POST /api/v1/deviceToken/refresh body {"refresh_token":...}
//     → device_token；成功后经 persistCredentials 写回 checkin blob。
//     刷新会派生新设备身份，故采用「查询失败才刷」的被动策略，尽量低频。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// QwenWork 上游端点（constants chunk + 客户端实测）。
const (
	qwBase         = "https://gateway.qwenwork.cn"
	qwUsagePath    = "/api/v2/quota/usage"
	qwRefreshPath  = "/api/v1/deviceToken/refresh"
	qwUserInfoPath = "/api/v1/userinfo"

	// 客户端身份头（探测实测 200 的组合；服务端不校验版本新鲜度）。
	qwAppVersion = "1.0.3"
	qwBuild      = "26090304"
)

const errNoQwCreds = "缺少 QwenWork 凭据，请从 PC 端导出并导入 access_token"

// QWCredential 是导出给聊天渠道的账号凭据视图。
type QWCredential struct {
	ID           string
	Nickname     string
	AccessToken  string
	RefreshToken string
}

// QwenWorkCredentials 返回全部启用 qwenwork 账号的解密凭据
// （pkg/qwenwork 聊天池经回调消费；无凭据账号跳过）。
func (m *Manager) QwenWorkCredentials() []QWCredential {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]QWCredential, 0, len(m.accounts))
	for _, a := range m.accounts {
		if a == nil || a.Platform != platformQwenWork || !a.Enabled {
			continue
		}
		if a.AccessToken == "" && a.RefreshToken == "" {
			continue
		}
		out = append(out, QWCredential{
			ID:           a.IDKey(),
			Nickname:     a.displayName(),
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
		})
	}
	return out
}

// qwApplyHeaders 写入 QwenWork 网关请求头（token 为空不下发 Authorization）。
func qwApplyHeaders(h http.Header, token string) {
	if h == nil {
		return
	}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "QoderWork")
	h.Set("X-QwenWork-Version", qwAppVersion)
	h.Set("X-QwenWork-Release-Version", qwAppVersion+"-"+qwBuild)
	h.Set("X-QwenWork-Build", qwBuild)
	h.Set("X-QwenWork-Platform", "win32")
	h.Set("X-QwenWork-Arch", "x64")
	h.Set("X-QwenWork-Channel", "stable")
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
}

// qwCreditsF64 查询 QwenWork 额度并宽容换算为 (剩余, 总量)。
// 查询 401/令牌失效且备有 refresh_token 时被动刷新一次后重试。
func (m *Manager) qwCreditsF64(ctx context.Context, a *Account) (credits, total float64, err error) {
	if a == nil || a.AccessToken == "" {
		return 0, 0, errors.New(errNoQwCreds)
	}
	data, err := m.qwUsage(ctx, a.AccessToken)
	if err != nil && a.RefreshToken != "" {
		slog.Warn("checkin: QwenWork 查询失败，尝试刷新令牌", "id", a.IDKey(), "error", err)
		if rerr := m.qwRefresh(ctx, a); rerr != nil {
			return 0, 0, err
		}
		if data, err = m.qwUsage(ctx, a.AccessToken); err != nil {
			return 0, 0, err
		}
	}
	if err != nil {
		return 0, 0, err
	}
	credits, total = m.creditsFrom(data)
	return credits, total, nil
}

// qwUsage 请求 quota/usage 并展平为 {remaining,total} 视图（与 Qoder 同构）。
func (m *Manager) qwUsage(ctx context.Context, token string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qwBase+qwUsagePath, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	qwApplyHeaders(req.Header, token)
	resp, err := m.hcOr(checkinHTTP).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errors.New("QwenWork 登录已过期，请重新导出凭据（HTTP " + fmt.Sprint(resp.StatusCode) + "）")
	}
	return parseQuotaUsagePayload(raw)
}

// qwRefresh 用 refresh_token 换新 access_token（deviceToken/refresh 端点）。
// 响应未携带新 refresh_token 时保留原值（会话可复用）。
func (m *Manager) qwRefresh(ctx context.Context, a *Account) error {
	if a == nil || a.RefreshToken == "" {
		return errors.New("缺少 refresh_token，请从 PC 端重新导出凭据")
	}
	body, err := json.Marshal(map[string]interface{}{"refresh_token": a.RefreshToken})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qwBase+qwRefreshPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	qwApplyHeaders(req.Header, "")
	resp, err := m.hcOr(checkinHTTP).Do(req)
	if err != nil {
		return fmt.Errorf("QwenWork 刷新失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden:
		return errors.New("QwenWork refresh_token 已失效，请重新导出凭据")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("QwenWork 刷新失败: HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 200))
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("QwenWork 刷新失败: %w", err)
	}
	if d, ok := parsed["data"].(map[string]interface{}); ok {
		parsed = d
	}
	access := strField(parsed, "device_token", "token", "access_token")
	if access == "" {
		return errors.New("QwenWork 刷新响应未含令牌")
	}
	a.AccessToken = access
	if r := strField(parsed, "refresh_token"); r != "" {
		a.RefreshToken = r
	}
	a.LastRefreshAt = nowStr()
	a.LastRefreshOK = true
	m.persistCredentials(a)
	slog.Info("checkin: token 刷新成功", "id", a.IDKey(), "platform", platformQwenWork)
	return nil
}

// qwUserInfo 查询用户信息（昵称/套餐，导入展示用）。
func (m *Manager) qwUserInfo(ctx context.Context, token string) (name, plan string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qwBase+qwUserInfoPath, nil)
	if err != nil {
		return "", "", err
	}
	qwApplyHeaders(req.Header, token)
	resp, err := m.hcOr(checkinHTTP).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var top struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
		Plan  struct {
			PID  string `json:"pid"`
			Name string `json:"name"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", "", fmt.Errorf("userinfo 解析失败: %w", err)
	}
	display := top.Name
	if display == "" {
		display = top.Email
	}
	return display, top.Plan.Name, nil
}

// truncateRunes 按字符数截断（错误消息防超长）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// strField 按候选键顺序取第一个非空字符串值。
func strField(data map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if s, ok := data[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// parseQuotaUsagePayload 解析 Qoder/QwenWork quota/usage 响应：
// 顶层形态 {user_quota:{total,used,remaining}, add_on_quota:{...},
// dedicated_resource_packages:[...]}；错误形态 {code:"TOKEN_INVALID"}；
// 旧信封 {code,msg,data}。返回 creditsFrom 可消费的视图。
func parseQuotaUsagePayload(raw []byte) (map[string]interface{}, error) {
	var top map[string]interface{}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("%s: %w", errCreditsParse, err)
	}
	if code, _ := top["code"].(string); code == "TOKEN_INVALID" || code == "UNAUTHORIZED" {
		return nil, errors.New("登录已过期，请重新登录（" + code + "）")
	}
	uq := firstMap(top, "userQuota", "user_quota")
	if uq != nil {
		rem, tot := 0.0, 0.0
		addQ := firstMap(top, "addOnQuota", "add_on_quota")
		// dedicated_resource_packages：活动/专属积分包，逐包累加
		if packs, ok := top["dedicatedResourcePackages"].([]interface{}); ok {
			for _, pk := range packs {
				if pm, ok := pk.(map[string]interface{}); ok {
					if r, ok := pm["remaining"].(float64); ok {
						rem += r
					}
					if t, ok := pm["total"].(float64); ok {
						tot += t
					}
				}
			}
		}
		for _, q := range []map[string]interface{}{uq, addQ} {
			if q == nil {
				continue
			}
			if r, ok := q["remaining"].(float64); ok {
				rem += r
			}
			if t, ok := q["total"].(float64); ok {
				tot += t
			}
		}
		return map[string]interface{}{"remaining": rem, "total": tot}, nil
	}
	// 旧信封形态
	var parsed struct {
		Code interface{}            `json:"code"`
		Msg  string                 `json:"msg"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%s: %w", errCreditsParse, err)
	}
	if parsed.Data == nil && parsed.Msg == "" {
		return nil, errors.New(errCreditsParse)
	}
	if parsed.Msg != "" && parsed.Data == nil {
		return nil, errors.New(parsed.Msg)
	}
	return parsed.Data, nil
}
