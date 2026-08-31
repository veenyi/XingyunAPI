// WorkBuddy 扫码/设备码登录：官方 CLI 设备流，无需用户手动抓 token。
// 流程：POST auth/state 拿 state+授权 URL（前端渲染成二维码，手机扫码授权）
// → 轮询 auth/token?state= 直到返回 accessToken → 用 accessToken 调 login/account
// 拿 uid/昵称 → 加密入库为一条签到账号。协议与请求头对齐 wild-work 实测实现。
package checkin

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	wbEndpointAuthState = wbChatBaseCN + "/v2/plugin/auth/state?platform=CLI"
	wbEndpointAuthToken = wbChatBaseCN + "/v2/plugin/auth/token?state="
	wbEndpointLoginAcct = wbChatBaseCN + "/v2/plugin/login/account?state="
	wbLoginSessionTTL   = 5 * time.Minute
)

// wbLoginSession 是一次进行中的扫码登录会话。
type wbLoginSession struct {
	state     string
	createdAt time.Time
}

// WBLoginResult 是登录成功后的凭据与账号信息。
type WBLoginResult struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterprise_id"`
	Domain       string `json:"domain"`
}

// wbLoginEnvelope 上游统一信封 {code,msg,data}。
type wbLoginEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func wbLoginDo(client *http.Client, method, fullURL string, body []byte, bearer string) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, rd)
	if err != nil {
		return nil, err
	}
	wbCommonHeaders(req)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream http %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env wbLoginEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))
	}
	return env.Data, nil
}

// WBLoginStart 发起设备码登录，返回会话 ID 与授权 URL（前端渲染二维码）。
func (m *Manager) WBLoginStart() (sessionID, authURL string, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	data, err := wbLoginDo(client, http.MethodPost, wbEndpointAuthState, []byte("{}"), "")
	if err != nil {
		return "", "", fmt.Errorf("auth state 失败: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state 响应缺少 state/authUrl")
	}
	sid := randHex(8)
	m.mu.Lock()
	m.pruneWBSessionsLocked()
	m.wbSessions[sid] = &wbLoginSession{state: st.State, createdAt: time.Now()}
	m.mu.Unlock()
	slog.Info("checkin: WorkBuddy 扫码登录会话已创建", "session", sid)
	return sid, st.AuthURL, nil
}

// WBLoginPoll 轮询一次登录状态。返回 status ∈ {pending, ok, expired, error}。
// ok 时把凭据加密入库并返回账号信息（不含 token）。
func (m *Manager) WBLoginPoll(sessionID string) (status string, acct *WBLoginResult, msg string) {
	m.mu.Lock()
	sess := m.wbSessions[sessionID]
	m.mu.Unlock()
	if sess == nil {
		return "expired", nil, "登录会话不存在或已过期"
	}
	if time.Since(sess.createdAt) > wbLoginSessionTTL {
		m.mu.Lock()
		delete(m.wbSessions, sessionID)
		m.mu.Unlock()
		return "expired", nil, "二维码已过期，请重新获取"
	}

	client := &http.Client{Timeout: 30 * time.Second}
	data, err := wbLoginDo(client, http.MethodGet, wbEndpointAuthToken+sess.state, nil, "")
	if err != nil {
		// 业务 code 非 0 且含 login → 尚未授权，继续等；其它错误也当 pending 但带信息。
		if isWBPending(err) {
			return "pending", nil, ""
		}
		return "pending", nil, err.Error()
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return "pending", nil, ""
	}

	// 拿 uid / 昵称 / 企业 ID（带 Bearer）。
	res := &WBLoginResult{Domain: tok.Domain}
	if acctData, aerr := wbLoginDo(client, http.MethodGet, wbEndpointLoginAcct+sess.state, nil, tok.AccessToken); aerr == nil {
		var a struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		_ = json.Unmarshal(acctData, &a)
		res.UID = a.UID
		res.EnterpriseID = a.EnterpriseID
		res.Nickname = a.Nickname
	}
	if res.UID == "" {
		return "pending", nil, "已授权但暂未取到账号信息，请稍候再试"
	}

	// 入库（加密 token），成功即清理会话。
	if err := m.addWBAccount(res, tok.AccessToken, tok.RefreshToken); err != nil {
		slog.Error("checkin: 保存 WorkBuddy 扫码账号失败", "uid", res.UID, "error", err)
		return "error", nil, "保存账号失败: " + err.Error()
	}
	m.mu.Lock()
	delete(m.wbSessions, sessionID)
	m.mu.Unlock()
	slog.Info("checkin: WorkBuddy 扫码登录成功", "uid", res.UID, "nickname", res.Nickname)
	return "ok", res, "登录成功"
}

// addWBAccount 把扫码拿到的凭据加密入库；同 UID 的 WorkBuddy 账号则更新凭据。
func (m *Manager) addWBAccount(res *WBLoginResult, access, refresh string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.loadStored()
	encA, err := m.store.Encrypt(access)
	if err != nil {
		return fmt.Errorf("加密 access token: %w", err)
	}
	encR := ""
	if refresh != "" {
		if encR, err = m.store.Encrypt(refresh); err != nil {
			return fmt.Errorf("加密 refresh token: %w", err)
		}
	}
	name := res.Nickname
	if name == "" {
		name = res.UID
	}
	// 命中同平台同 UID → 更新凭据。
	for i := range stored {
		if stored[i].Account.Platform == PlatformWorkBuddy && stored[i].Account.UID == res.UID {
			stored[i].EncAccess = encA
			if encR != "" {
				stored[i].EncRefresh = encR
			}
			if res.EnterpriseID != "" {
				stored[i].Account.EnterpriseID = res.EnterpriseID
			}
			if res.Domain != "" {
				stored[i].Account.Domain = res.Domain
			}
			if name != "" {
				stored[i].Account.Name = name
			}
			stored[i].Account.AccessToken = ""
			stored[i].Account.RefreshToken = ""
			return m.saveStored(stored)
		}
	}
	// 新增。
	acc := Account{
		ID:           fmt.Sprintf("ci_%d", time.Now().UnixNano()),
		Platform:     PlatformWorkBuddy,
		Name:         name,
		UID:          res.UID,
		EnterpriseID: res.EnterpriseID,
		Domain:       res.Domain,
		Enabled:      true,
	}
	sa := storedAccount{Account: acc, EncAccess: encA, EncRefresh: encR}
	stored = append(stored, sa)
	return m.saveStored(stored)
}

func (m *Manager) pruneWBSessionsLocked() {
	for id, s := range m.wbSessions {
		if time.Since(s.createdAt) > wbLoginSessionTTL {
			delete(m.wbSessions, id)
		}
	}
}

func isWBPending(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return (strings.Contains(s, "login") || strings.Contains(s, "code=")) &&
		!strings.Contains(s, "http ") && !strings.Contains(s, "parse")
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
