package checkin

// WorkBuddy/CodeBuddy 平台实现：
//   - 签到/积分：www.codebuddy.cn /v2/billing/meter/daily-checkin、
//     /v2/billing/meter/get-user-resource（Bearer 访问令牌）。
//   - 扫码登录（微信）：copilot.tencent.com /v2/plugin/auth/state 建会话、
//     /v2/plugin/login/account 轮询确认（workbuddy 包本身无登录流，
//     由本包实现 wbLoginSession/wbLoginDo）。
//   - 令牌刷新委托 workbuddy.RefreshToken（chat.go）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/workbuddy"
)

// wbHeaderUserAgent 未从二进制还原，取浏览器惯例值（对齐 workbuddy 包）。
const wbHeaderUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) CodeBuddy/1.0.0 Chrome/133.0.0.0 Safari/537.36"

// wbLoginTTL 是扫码登录会话有效期（对齐 qoder sessionTTL）。
const wbLoginTTL = 5 * time.Minute

// wbCommonHeaders 所有上游请求共用的基础头。
func wbCommonHeaders() http.Header {
	return http.Header{
		"Content-Type":    {"application/json"},
		"Accept":          {"application/json"},
		"User-Agent":      {wbHeaderUserAgent},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
}

// wbBillingHeaders 签到/积分请求头（Bearer 访问令牌 + 企业上下文）。
func wbBillingHeaders(a *Account) http.Header {
	h := wbCommonHeaders()
	if a != nil {
		if a.AccessToken != "" {
			h.Set("Authorization", "Bearer "+a.AccessToken)
		}
		if a.EnterpriseID != "" {
			h.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		if a.Domain != "" {
			h.Set("X-Domain", a.Domain)
		}
	}
	return h
}

// wbBillingDo 调用计费端点（签到/积分共用），返回宽容包裹层。
func (m *Manager) wbBillingDo(ctx context.Context, a *Account, path string, body interface{}) (*wbEnvelope, error) {
	if a.AccessToken == "" {
		return nil, fmt.Errorf("%s：%s", textCheckinUnfinished, errNoAccessToken)
	}
	if body == nil {
		body = wbBillingBody(a)
	}
	return doEnvelope(ctx, m.hc, http.MethodPost, wbBase+path, wbBillingHeaders(a), body)
}

// wbBillingBody 构造计费请求体（企业上下文透传，其余留空）。
func wbBillingBody(a *Account) map[string]interface{} {
	body := map[string]interface{}{}
	if a.EnterpriseID != "" {
		body["enterprise_id"] = a.EnterpriseID
	}
	if a.Domain != "" {
		body["domain"] = a.Domain
	}
	return body
}

// wbCheckin 执行每日签到，返回结果文案（签到成功）。
// 上游回报已签到 → errAlreadyCheckedIn；额度类问题原样透传。
func (m *Manager) wbCheckin(ctx context.Context, a *Account) (string, error) {
	env, err := m.wbBillingDo(ctx, a, wbCheckinPath, nil)
	if err != nil {
		// 上游对“已签到”回报 HTTP 400（body code=10001“今天已签到”），
		// doEnvelope 在非 2xx 直接抛错，这里补识别避免幂等场景被记失败。
		if containsFold(err.Error(), alreadyCheckedMarkers) {
			return "", errAlreadyCheckedIn
		}
		return "", err
	}
	text := env.text()
	if !env.okCode() {
		if containsFold(text, alreadyCheckedMarkers) {
			return "", errAlreadyCheckedIn
		}
		if containsFold(text, creditMarkers) {
			return "", errors.New(text)
		}
		if text == "" {
			text = "签到请求失败"
		}
		return "", errors.New(text)
	}
	if data := env.dataOrSelf(); truthy(data["checked_in"]) || truthy(data["checked"]) {
		return "", errAlreadyCheckedIn
	}
	if data := env.dataOrSelf(); data != nil {
		if v, ok := data["checked_in"]; ok && !truthy(v) {
			return "", errors.New(textNotCheckedIn)
		}
	}
	if containsFold(text, alreadyCheckedMarkers) {
		return "", errAlreadyCheckedIn
	}
	if text != "" {
		return text, nil
	}
	return textCheckinOK, nil
}

// wbCredits 查询积分（get-user-resource），返回宽容包裹层。
func (m *Manager) wbCredits(ctx context.Context, a *Account) (*wbEnvelope, error) {
	env, err := m.wbBillingDo(ctx, a, wbUsagePath, nil)
	if err != nil {
		return nil, err
	}
	if !env.okCode() {
		text := env.text()
		if text == "" {
			text = errCreditsParse
		}
		return nil, errors.New(text)
	}
	return env, nil
}

// wbCreditsF64 查询积分并宽容换算为 (剩余, 总量)。
func (m *Manager) wbCreditsF64(ctx context.Context, a *Account) (credits, total float64, err error) {
	env, err := m.wbCredits(ctx, a)
	if err != nil {
		return 0, 0, err
	}
	credits, total = m.creditsFrom(env.dataOrSelf())
	return credits, total, nil
}

// wbRefresh 用 refresh_token 换新访问令牌（委托 workbuddy.RefreshToken）。
func (m *Manager) wbRefresh(ctx context.Context, a *Account) error {
	if a.RefreshToken == "" {
		return errors.New("缺少 refreshToken，请重新登录")
	}
	wba := &workbuddy.WBAccount{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		EnterpriseID: a.EnterpriseID,
		Domain:       a.Domain,
	}
	if err := workbuddy.RefreshToken(ctx, m.hc, wba); err != nil {
		return err
	}
	a.AccessToken = wba.AccessToken
	a.RefreshToken = wba.RefreshToken
	a.LastRefreshAt = nowStr()
	a.LastRefreshOK = true
	return nil
}

// ---- WorkBuddy 微信扫码登录 ----

// wbLoginSession 是一个扫码登录会话（auth/state → login/account 轮询）。
type wbLoginSession struct {
	ID        string
	State     string
	AuthURL   string
	CreatedAt time.Time
	ExpiresAt time.Time

	mu      sync.Mutex
	account *Account
	err     error
}

// wbLoginSession 向 auth/state 申请扫码会话（state + 授权链接）。
func (m *Manager) wbLoginSession(ctx context.Context) (*wbLoginSession, error) {
	env, err := doEnvelope(ctx, m.hc, http.MethodPost, wbAuthBase+wbAuthStatePath, wbCommonHeaders(), map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	candidates := mergedFields(env)
	state := strAny(candidates, "state", "auth_state", "ticket")
	authURL := strAny(candidates, "auth_url", "authUrl", "url", "login_url")
	if state == "" || authURL == "" {
		return nil, errors.New("auth state 响应缺少 state/authUrl")
	}
	s := &wbLoginSession{
		ID:        randHex(12),
		State:     state,
		AuthURL:   authURL,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(wbLoginTTL),
	}
	m.mu.Lock()
	// 顺带清理过期会话
	for id, old := range m.wbSessions {
		if time.Now().After(old.ExpiresAt) {
			delete(m.wbSessions, id)
		}
	}
	m.wbSessions[s.ID] = s
	m.mu.Unlock()
	slog.Info("checkin: WorkBuddy 扫码登录会话已创建", "session_id", s.ID)
	return s, nil
}

// wbLoginDo 轮询一次登录确认：扫码确认 → 账号（自动入库）；
// 未确认 → errWBPending；会话失效 → 明确错误。
func (m *Manager) wbLoginDo(ctx context.Context, s *wbLoginSession) (*Account, error) {
	env, err := doEnvelope(ctx, m.hc, http.MethodPost, wbAuthBase+wbLoginPath, wbCommonHeaders(), map[string]interface{}{"state": s.State})
	if err != nil {
		return nil, err
	}
	candidates := mergedFields(env)
	access := strAny(candidates, "access_token", "accessToken")
	if access == "" {
		text := env.text()
		if containsFold(text, []string{"过期", "expired", "失效", "invalid", "不存在"}) {
			if text == "" {
				text = "登录已过期，请重新扫码"
			}
			return nil, errors.New(text)
		}
		return nil, errWBPending
	}
	acct := &Account{
		Platform:     platformWorkBuddy,
		UID:          strAny(candidates, "user_id", "uid", "userid"),
		Nickname:     strAny(candidates, "nickname", "display_name", "username"),
		AccessToken:  access,
		RefreshToken: strAny(candidates, "refresh_token", "refreshToken"),
		EnterpriseID: strAny(candidates, "enterprise_id", "enterpriseId"),
		Domain:       strAny(candidates, "domain"),
		APIHost:      strAny(candidates, "api_host", "apiHost"),
		Enabled:      true,
	}
	saved, err := m.addWBAccount(acct)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.account = saved
	s.mu.Unlock()
	slog.Info("checkin: WorkBuddy 扫码登录成功", "uid", saved.UID)
	return saved, nil
}

// addWBAccount 把扫码登录得到的账号写入签到账号列表（按 UID 去重更新）。
func (m *Manager) addWBAccount(acct *Account) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var saved *Account
	for _, cur := range m.accounts {
		if cur != nil && cur.Platform == platformWorkBuddy && cur.UID == acct.UID {
			cur.AccessToken = acct.AccessToken
			cur.RefreshToken = acct.RefreshToken
			if acct.Nickname != "" {
				cur.Nickname = acct.Nickname
			}
			if acct.EnterpriseID != "" {
				cur.EnterpriseID = acct.EnterpriseID
			}
			if acct.Domain != "" {
				cur.Domain = acct.Domain
			}
			if acct.APIHost != "" {
				cur.APIHost = acct.APIHost
			}
			cur.LastRefreshAt = nowStr()
			cur.LastRefreshOK = true
			saved = cur
			break
		}
	}
	if saved == nil {
		acct.ID = "ci_" + randHex(8)
		m.accounts = append(m.accounts, acct)
		saved = acct
	}
	if err := m.saveStored(); err != nil {
		return nil, fmt.Errorf("保存 WorkBuddy 扫码账号失败: %w", err)
	}
	return saved.clone(), nil
}

// isWBPending 报告错误是否为「扫码尚未确认」（前端继续轮询）。
func isWBPending(err error) bool { return errors.Is(err, errWBPending) }

// WBAccounts 返回 WorkBuddy 平台的签到账号（打码令牌）。
func (m *Manager) WBAccounts() []Account {
	return m.accountsByPlatform(platformWorkBuddy)
}

// WBLoginStart 发起 WorkBuddy 扫码登录（auth_url 前端渲染二维码）。
func (m *Manager) WBLoginStart(ctx context.Context) (*WBLoginResult, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()
	s, err := m.wbLoginSession(ctx)
	if err != nil {
		return nil, err
	}
	return &WBLoginResult{
		SessionID: s.ID,
		AuthURL:   s.AuthURL,
		Message:   "用手机微信扫码，或在已登录 WorkBuddy 的浏览器中打开下方链接完成授权",
	}, nil
}

// WBLoginPoll 轮询扫码结果：status ∈ ok/expired/error/其他=等待中；
// 成功时账号已自动加入签到列表。
func (m *Manager) WBLoginPoll(ctx context.Context, sessionID string) (*WBLoginResult, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()
	m.mu.Lock()
	s := m.wbSessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return &WBLoginResult{Status: "expired", Message: "登录会话不存在或已过期"}, nil
	}
	if time.Now().After(s.ExpiresAt) {
		m.mu.Lock()
		delete(m.wbSessions, sessionID)
		m.mu.Unlock()
		return &WBLoginResult{Status: "expired", Message: "二维码已过期，请重新获取"}, nil
	}
	s.mu.Lock()
	acct, persistedErr := s.account, s.err
	s.mu.Unlock()
	switch {
	case acct != nil:
		return &WBLoginResult{
			Status:   "ok",
			Message:  "登录成功，账号已添加",
			Nickname: acct.Nickname,
			UID:      acct.UID,
		}, nil
	case persistedErr != nil:
		return &WBLoginResult{Status: "error", Message: persistedErr.Error()}, nil
	}
	saved, err := m.wbLoginDo(ctx, s)
	switch {
	case err == nil:
		return &WBLoginResult{
			Status:   "ok",
			Message:  "登录成功，账号已添加",
			Nickname: saved.Nickname,
			UID:      saved.UID,
		}, nil
	case isWBPending(err):
		return &WBLoginResult{Status: "waiting", Message: "等待扫码确认"}, nil
	default:
		if containsFold(err.Error(), []string{"过期", "expired", "不存在", "失效"}) {
			m.mu.Lock()
			delete(m.wbSessions, sessionID)
			m.mu.Unlock()
			return &WBLoginResult{Status: "expired", Message: err.Error()}, nil
		}
		// 瞬时错误不打断轮询，保持等待态
		return &WBLoginResult{Status: "waiting", Message: err.Error()}, nil
	}
}

// accountsByPlatform 返回指定平台的打码账号列表。
func (m *Manager) accountsByPlatform(platform string) []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		if a != nil && a.Platform == platform {
			out = append(out, a.Masked())
		}
	}
	return out
}

// strAny 依次取候选键的第一个非空字符串值。
func strAny(data map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := data[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// mergedFields 合并包裹层的顶层字段与 data 字段（data 优先），
// 供登录/授权响应的宽容字段提取。
func mergedFields(env *wbEnvelope) map[string]interface{} {
	candidates := map[string]interface{}{}
	if env == nil {
		return candidates
	}
	for k, v := range env.Raw {
		candidates[k] = v
	}
	for k, v := range env.Data {
		candidates[k] = v
	}
	return candidates
}
