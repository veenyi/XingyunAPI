package checkin

// Qoder 平台实现：
//   - 无签到动作，只刷积分：GET {qoder.OpenAPIBase}/api/v2/quota/usage。
//   - 登录流整体委托 qoder.LoginManager（设备流，禁止重复实现）：
//     QoderLoginStart → qoderLoginSession（Start 返回 session_id/auth_url）、
//     QoderLoginPoll → LoginManager.Poll（成功账号自动写入 qoder_accounts blob）。
//   - 凭据不落在 checkin blob：按 uid 从 qoder_accounts blob 解析
//     （密文格式与 store.Encrypt/Decrypt 一致：nonce 前置 + hex），
//     刷新经 qoder.RefreshToken 后写回（仅在 NeedsRefresh 时，低频）。

import (
	"context"
	"encoding/json"
	"io"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/veenyi/XingyunAPI/pkg/qoder"
)

// qoderBlobAccount 是 qoder_accounts blob 单条账号的读取视图
// （字段标签对齐 pkg/qoder/blob.go；enc_* 由 store.Decrypt 解密）。
type qoderBlobAccount struct {
	UserID        string `json:"user_id"`
	Nickname      string `json:"nickname,omitempty"`
	MachineID     string `json:"machine_id,omitempty"`
	MachineType   string `json:"machine_type,omitempty"`
	EnterpriseID  string `json:"enterprise_id,omitempty"`
	Region        string `json:"region,omitempty"`
	EncAccess     string `json:"enc_access,omitempty"`
	EncRefresh    string `json:"enc_refresh,omitempty"`
	LastRefreshAt string `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool   `json:"last_refresh_ok,omitempty"`

	// 明文暂存（不落盘）：刷新成功后由 saveQoderAccounts 加密回填 enc_*
	AccessTokenPlain  string `json:"-"`
	RefreshTokenPlain string `json:"-"`
}

// QoderAccounts 返回 Qoder 平台的签到账号（打码令牌）。
func (m *Manager) QoderAccounts() []Account {
	return m.accountsByPlatform(platformQoder)
}

// QoderLoginStart 发起 Qoder 设备流登录（委托 qoder.LoginManager.Start）。
func (m *Manager) QoderLoginStart(ctx context.Context) (*QoderLoginResult, error) {
	res, err := m.qoderLoginSession(ctx)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// QoderLoginPoll 轮询设备流授权结果：status ∈ ok/expired/error/其他=等待；
// 成功时账号自动加入签到列表并写入 qoder_accounts blob。
func (m *Manager) QoderLoginPoll(ctx context.Context, sessionID string) (*QoderLoginResult, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()
	m.mu.Lock()
	lm := m.qoderLogin
	m.mu.Unlock()
	if lm == nil {
		return &QoderLoginResult{Status: "expired", Message: "登录会话不存在或已过期"}, nil
	}
	acct, err := lm.Poll(ctx, sessionID)
	switch {
	case err == nil:
		saved, aerr := m.addQoderAccount(acct)
		if aerr != nil {
			return &QoderLoginResult{Status: "error", Message: aerr.Error()}, nil
		}
		nick := saved.Nickname
		if nick == "" {
			nick = saved.Name
		}
		return &QoderLoginResult{
			Status:   "ok",
			Message:  qoder.TextAccountAdded,
			Nickname: nick,
			UID:      saved.UID,
		}, nil
	case errors.Is(err, qoder.ErrPending):
		return &QoderLoginResult{Status: "waiting", Message: "请在浏览器中打开授权链接，等待授权完成"}, nil
	default:
		if containsFold(err.Error(), []string{"过期", "expired", "不存在", "失效"}) {
			return &QoderLoginResult{Status: "expired", Message: err.Error()}, nil
		}
		return &QoderLoginResult{Status: "error", Message: err.Error()}, nil
	}
}

// qoderLoginSession 创建设备流登录会话（登录管理器懒初始化；
// 「checkin: Qoder 设备流登录会话已创建」由 qoder.LoginManager.Start 记录）。
func (m *Manager) qoderLoginSession(ctx context.Context) (*QoderLoginResult, error) {
	m.mu.Lock()
	if m.qoderLogin == nil {
		if m.store != nil {
			m.qoderLogin = qoder.NewLoginManager(m.hc, m.store)
		} else {
			m.qoderLogin = qoder.NewLoginManager(m.hc, nil)
		}
	}
	lm := m.qoderLogin
	m.mu.Unlock()
	st, err := lm.Start(ctx)
	if err != nil {
		return nil, err
	}
	return &QoderLoginResult{
		SessionID: st.SessionID,
		AuthURL:   st.AuthURL,
		Message:   qoder.TextLoginHint,
	}, nil
}

// addQoderAccount 把设备流登录成功的账号加入签到列表（按 UID 去重更新）。
// 凭据已由 qoder.LoginManager 持久化到 qoder_accounts blob，此处只登记元数据。
func (m *Manager) addQoderAccount(acct *qoder.QoderAccount) (*Account, error) {
	if acct == nil || acct.UserID == "" {
		return nil, errors.New(errNoQoderCreds)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var saved *Account
	for _, cur := range m.accounts {
		if cur != nil && cur.Platform == platformQoder && cur.UID == acct.UserID {
			if acct.Nickname != "" {
				cur.Nickname = acct.Nickname
			}
			if cur.Name == "" {
				cur.Name = acct.Nickname
			}
			saved = cur
			break
		}
	}
	if saved == nil {
		saved = &Account{
			ID:       "ci_" + randHex(8),
			Platform: platformQoder,
			Name:     acct.Nickname,
			Nickname: acct.Nickname,
			UID:      acct.UserID,
			Enabled:  true,
		}
		m.accounts = append(m.accounts, saved)
	}
	if err := m.saveStored(); err != nil {
		return nil, fmt.Errorf("保存 Qoder 登录账号失败: %w", err)
	}
	slog.Info("checkin: Qoder 设备流登录成功", "uid", saved.UID)
	return saved.clone(), nil
}

// loadQoderAccounts 读取 qoder_accounts blob 并解密令牌（容忍缺省）。
func (m *Manager) loadQoderAccounts() []*qoderBlobAccount {
	raw := m.getSetting(qoder.SettingsKey)
	if raw == "" {
		return nil
	}
	var items []*qoderBlobAccount
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		slog.Warn("checkin: 解析 Qoder 账号失败", "error", err)
		return nil
	}
	return items
}

// saveQoderAccounts 把 qoder_accounts blob 写回（令牌经 store 加密，
// 与 pkg/qoder 的密文格式一致）。
func (m *Manager) saveQoderAccounts(items []*qoderBlobAccount) error {
	for _, it := range items {
		if it == nil {
			continue
		}
		if enc, err := m.encryptField(it.AccessTokenPlain); err == nil && enc != "" {
			it.EncAccess = enc
		}
		if enc, err := m.encryptField(it.RefreshTokenPlain); err == nil && enc != "" {
			it.EncRefresh = enc
		}
	}
	return m.setSetting(qoder.SettingsKey, mustJSON(items))
}

// resolveQoderAccount 按 uid 定位 qoder blob 账号并还原为 QoderAccount
// （令牌解密失败容忍为空，由上层报错）。
func (m *Manager) resolveQoderAccount(uid string) (*qoder.QoderAccount, []*qoderBlobAccount, error) {
	items := m.loadQoderAccounts()
	for _, it := range items {
		if it == nil || it.UserID != uid {
			continue
		}
		if it.EncAccess == "" && it.EncRefresh == "" {
			return nil, items, errors.New(errNoQoderCreds)
		}
		acct := &qoder.QoderAccount{
			UserID:        it.UserID,
			Nickname:      it.Nickname,
			AccessToken:   m.decryptField(it.EncAccess),
			RefreshToken:  m.decryptField(it.EncRefresh),
			MachineID:     it.MachineID,
			MachineType:   it.MachineType,
			EnterpriseID:  it.EnterpriseID,
			Region:        it.Region,
			LastRefreshAt: it.LastRefreshAt,
			LastRefreshOK: it.LastRefreshOK,
		}
		qoder.EnsureFingerprint(acct)
		return acct, items, nil
	}
	return nil, items, errors.New(errNoQoderCreds)
}

// qoderCredits 查询 quota/usage（先按需刷新令牌），返回宽容解析的 usage 数据。
func (m *Manager) qoderCredits(ctx context.Context, a *Account) (map[string]interface{}, error) {
	if a.UID == "" {
		return nil, errors.New(errNoQoderCreds)
	}
	acct, _, err := m.resolveQoderAccount(a.UID)
	if err != nil {
		return nil, err
	}
	if acct.NeedsRefresh() {
		if err := m.qoderRefresh(ctx, a); err == nil {
			if refreshed, _, rerr := m.resolveQoderAccount(a.UID); rerr == nil {
				acct = refreshed
			}
		} else {
			slog.Warn("checkin: token 刷新失败，改用现有 access token 继续签到", "uid", a.UID, "error", err)
		}
	}
	if acct.AccessToken == "" {
		return nil, errors.New(errNoQoderCreds)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qoder.OpenAPIBase+qoderUsagePath, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	qoder.ApplyOpenAPIHeaders(req.Header, acct.AccessToken)
	hc := m.hc
	if hc == nil {
		hc = checkinHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// Qoder usage 响应两种形态（客户端 HIe 实证 0.1.8）：
	// 1) 顶层字段：{user_quota:{total,used,remaining}, add_on_quota:{...}}（无信封）
	// 2) 错误：{code:"TOKEN_INVALID", message:...}
	var top map[string]interface{}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("%s: %w", errCreditsParse, err)
	}
	if code, _ := top["code"].(string); code == "TOKEN_INVALID" || code == "UNAUTHORIZED" {
		return nil, errors.New("Qoder 登录已过期，请重新登录（" + code + "）")
	}
	uq := firstMap(top, "userQuota", "user_quota")
	if uq != nil {
		// 顶层形态：展平 user_quota + add_on_quota 为 {remaining,total} 视图
		rem, tot := 0.0, 0.0
		addQ := firstMap(top, "addOnQuota", "add_on_quota")
		// dedicated_resource_packages：活动/专属积分包（如 Qwen 专属），逐包累加
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

// qoderCreditsF64 查询 Qoder 额度并宽容换算为 (剩余, 总量)。
func (m *Manager) qoderCreditsF64(ctx context.Context, a *Account) (credits, total float64, err error) {
	data, err := m.qoderCredits(ctx, a)
	if err != nil {
		return 0, 0, err
	}
	credits, total = m.creditsFrom(data)
	return credits, total, nil
}

// qoderRefresh 刷新 Qoder 令牌（委托 qoder.RefreshToken；仅在
// NeedsRefresh 时执行）并写回 qoder_accounts blob。注意：与在线 qoder.Pool
// 并存时存在低概率双写竞态，Pool.SyncAccounts 会重新加载。
func (m *Manager) qoderRefresh(ctx context.Context, a *Account) error {
	if a.UID == "" {
		return errors.New(errNoQoderCreds)
	}
	acct, items, err := m.resolveQoderAccount(a.UID)
	if err != nil {
		return err
	}
	if !acct.NeedsRefresh() {
		return nil
	}
	if acct.RefreshToken == "" {
		return errors.New("缺少 refreshToken，请重新登录")
	}
	if err := qoder.RefreshToken(ctx, m.hc, acct); err != nil {
		return err
	}
	for _, it := range items {
		if it != nil && it.UserID == acct.UserID {
			it.AccessTokenPlain = acct.AccessToken
			it.RefreshTokenPlain = acct.RefreshToken
			it.LastRefreshAt = nowStr()
			it.LastRefreshOK = true
			break
		}
	}
	if err := m.saveQoderAccounts(items); err != nil {
		return err
	}
	slog.Info("checkin: token 刷新成功", "uid", acct.UserID, "platform", platformQoder)
	return nil
}

// firstMap 返回 top 中第一个存在且为 map 的键（驼峰/蛇形兼容）。
func firstMap(top map[string]interface{}, keys ...string) map[string]interface{} {
	for _, k := range keys {
		if v, ok := top[k].(map[string]interface{}); ok {
			return v
		}
	}
	return nil
}
