package qoder

// 设备流登录：LoginManager（Start/Poll/cleanup）+ RefreshToken + EnsureFingerprint。
// 端点（v0.7.2 反汇编实证）：Start 纯本地生成授权链接（不发请求）；
// Poll = GET {OpenAPIBase}/api/v1/deviceToken/poll?nonce=…&verifier=…&challenge_method=S256
// （nonce 需先经授权页注册，未注册时上游回 404 NotFound → 视为等待）；
// 刷新 = POST {OpenAPIBase}/api/v1/deviceToken/refresh。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrPending 表示用户尚未完成授权（前端据此继续轮询）。
var ErrPending = errors.New("login pending")

// loginState 是一个设备流登录会话（字段名对齐上游 query）。
type loginState struct {
	SessionID string    `json:"session_id"`
	Verifier  string    `json:"-"`
	Nonce     string    `json:"nonce"`
	MachineID string    `json:"machine_id"`
	AuthURL   string    `json:"auth_url"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	mu      sync.Mutex
	done    bool
	account *QoderAccount
	err     error
}

// Status 返回前端轮询所需的状态视图（对齐 LoginSessionStatus 契约）：
// status ∈ ok / expired / error / 其他值 = 等待中。
func (s *loginState) Status() (status, message string, account *QoderAccount) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.account != nil:
		nick := s.account.Nickname
		if nick == "" {
			nick = s.account.UserID
		}
		return "ok", TextAccountAdded, s.account
	case s.err != nil:
		return "error", s.err.Error(), nil
	case time.Now().After(s.ExpiresAt):
		return "expired", errLoginExpired, nil
	default:
		return "pending", TextLoginHint, nil
	}
}

// LoginManager 管理设备流登录会话。
type LoginManager struct {
	mu       sync.Mutex
	sessions map[string]*loginState
	http     *http.Client
	store    settingsStore // 登录成功即入库；nil 则仅返回账号
}

// NewLoginManager 构造登录管理器（hc 为 nil 时用默认客户端）。
func NewLoginManager(hc *http.Client, st settingsStore) *LoginManager {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &LoginManager{sessions: map[string]*loginState{}, http: hc, store: st}
}

// Start 创建设备流登录会话：本地生成 PKCE + nonce/machine_id 并拼授权链接。
// 授权页加载时由上游注册 nonce，Poll 才能查到结果（Start 不发任何请求）。
func (m *LoginManager) Start(_ context.Context) (*loginState, error) {
	verifier, challenge := makePKCE()
	nonce := uuid4()
	machineID := uuid4()
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("machine_id", machineID)
	q.Set("client_id", DeviceClientID)
	q.Set("redirect_uri", DeviceRedirectURI)
	st := &loginState{
		SessionID: uuid4(),
		Verifier:  verifier,
		Nonce:     nonce,
		MachineID: machineID,
		AuthURL:   DeviceAuthBase + "/device/selectAccounts?" + q.Encode(),
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(sessionTTL),
	}
	m.mu.Lock()
	m.sessions[st.SessionID] = st
	m.mu.Unlock()
	slog.Info("checkin: Qoder 设备流登录会话已创建", "session_id", st.SessionID)
	return st, nil
}

// Poll 轮询授权结果：pending → ErrPending；成功 → 账号（并 cleanup）。
func (m *LoginManager) Poll(ctx context.Context, sessionID string) (*QoderAccount, error) {
	st := m.get(sessionID)
	if st == nil {
		return nil, errors.New(errSessionGone)
	}
	if st.expired() {
		m.cleanup(st)
		return nil, errors.New(errLoginExpired)
	}
	data, err := m.pollOnce(ctx, st)
	if err != nil {
		if errors.Is(err, ErrPending) {
			return nil, ErrPending
		}
		return nil, err
	}
	access := pollField(data, "device_token", "token")
	acct := &QoderAccount{
		UserID:        pollField(data, "id", "user_id", "uid"),
		Nickname:      pollField(data, "name", "username", "user_name", "email"),
		AccessToken:   access,
		RefreshToken:  pollField(data, "refresh_token"),
		MachineID:     st.MachineID,
		Note:          TextAccountAdded,
		LastRefreshAt: nowRFC3339(),
		LastRefreshOK: true,
	}
	if acct.AccessToken == "" {
		return nil, errors.New(errNoTokenInResp)
	}
	// poll 响应不带身份，账号 UID/昵称需另取 /api/v1/userinfo（客户端同款流程）。
	if acct.UserID == "" || acct.Nickname == "" {
		if uid, name, err := m.fetchUser(ctx, acct.AccessToken); err == nil {
			if acct.UserID == "" {
				acct.UserID = uid
			}
			if acct.Nickname == "" {
				acct.Nickname = name
			}
		} else {
			slog.Warn("qoder: userinfo 拉取失败，UID 退化为随机值", "error", err)
		}
	}
	if acct.UserID == "" {
		acct.UserID = "qoder_" + hexShort()
	}
	EnsureFingerprint(acct)
	if err := m.persist(acct); err != nil {
		m.fail(st, err)
		return nil, err
	}
	st.mu.Lock()
	st.account = acct
	st.mu.Unlock()
	m.cleanup(st)
	slog.Info("checkin: Qoder 设备流登录成功", "user_id", acct.UserID)
	return acct, nil
}

// fetchUser 用访问令牌取账号身份（poll 响应不含 UID/昵称）。
func (m *LoginManager) fetchUser(ctx context.Context, token string) (userID, nickname string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenAPIBase+userInfoPath, nil)
	if err != nil {
		return "", "", err
	}
	ApplyOpenAPIHeaders(req.Header, token)
	resp, err := m.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("qoder: userinfo 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", "", errors.New(errInvalidCreds)
	}
	if resp.StatusCode != http.StatusOK {
		var parsed map[string]interface{}
		_ = json.Unmarshal(raw, &parsed)
		return "", "", fmt.Errorf("qoder: userinfo HTTP %d: %s", resp.StatusCode,
			pollFieldWithFallback(parsed, truncateRunes(string(raw), 120), "errorMessage", "error_message", "message"))
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", "", fmt.Errorf("qoder: userinfo 解析失败: %w", err)
	}
	data = flattenPollData(data)
	userID = pollField(data, "id", "user_id", "uid")
	nickname = pollField(data, "name", "username", "user_name")
	if nickname == "" {
		nickname = pollField(data, "email")
	}
	if userID == "" {
		return "", "", errors.New(errNoUserID)
	}
	return userID, nickname, nil
}

// cleanup 终结会话（成功/过期后从注册表移除）。
func (m *LoginManager) cleanup(st *loginState) {
	if st == nil {
		return
	}
	st.mu.Lock()
	st.done = true
	st.mu.Unlock()
	m.mu.Lock()
	delete(m.sessions, st.SessionID)
	m.mu.Unlock()
}

func (m *LoginManager) get(sessionID string) *loginState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID]
}

func (m *LoginManager) fail(st *loginState, err error) {
	st.mu.Lock()
	st.err = err
	st.mu.Unlock()
	m.cleanup(st)
}

func (s *loginState) expired() bool {
	return time.Now().After(s.ExpiresAt)
}

// persist 把新账号追加进账号池 blob（保存失败即登录失败）。
func (m *LoginManager) persist(acct *QoderAccount) error {
	if m.store == nil {
		return nil
	}
	existing, err := loadAccounts(m.store)
	if err != nil {
		existing = nil
	}
	replaced := false
	for i, a := range existing {
		if a.UserID == acct.UserID {
			existing[i] = acct
			replaced = true
		}
	}
	if !replaced {
		existing = append(existing, acct)
	}
	return saveAccounts(m.store, existing)
}

// pollOnce 调一次 deviceToken/poll（GET，参数全在 query）。
// 返回归一化后的响应 map；等待态（404/NotFound/无令牌）→ ErrPending。
func (m *LoginManager) pollOnce(ctx context.Context, st *loginState) (map[string]interface{}, error) {
	q := url.Values{}
	q.Set("nonce", st.Nonce)
	q.Set("verifier", st.Verifier)
	q.Set("challenge_method", "S256")
	pollURL := OpenAPIBase + deviceTokenPollPath + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, err
	}
	ApplyOpenAPIHeaders(req.Header, "")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qoder: deviceToken poll 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed map[string]interface{}
	if json.Unmarshal(raw, &parsed) != nil {
		parsed = map[string]interface{}{}
	}
	if resp.StatusCode == http.StatusNotFound {
		// nonce 尚未在授权页注册（或已过期）→ 继续等待
		return nil, ErrPending
	}
	if resp.StatusCode != http.StatusOK {
		msg := pollField(parsed, "errorMessage", "error_message", "msg")
		if msg == "" {
			msg = truncateRunes(string(raw), 200)
		}
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, msg)
	}
	if pollField(parsed, "device_token", "token") != "" {
		return flattenPollData(parsed), nil
	}
	// 200 但无令牌：可能是 pending 信封（{code,msg}）或错误码
	if code := pollField(parsed, "errorCode", "error_code"); code != "" && !strings.EqualFold(code, "NotFound") {
		return nil, errors.New(pollFieldWithFallback(parsed, code, "errorMessage", "error_message", "msg"))
	}
	return nil, ErrPending
}

// ApplyOpenAPIHeaders 给 OpenAPI 家族请求（deviceToken/userinfo/quota/models）
// 下发客户端实证规格的鉴权头：裸 Bearer 令牌（不含 COSY. 前缀）。
// token 为空时不下发 Authorization（用于 poll/refresh 这类未鉴权请求）。
func ApplyOpenAPIHeaders(h http.Header, token string) {
	if h == nil {
		return
	}
	h.Set("Accept", "application/json")
	h.Set("User-Agent", openapiUserAgent)
	h.Set("Cosy-ClientType", openapiClientType)
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
}

// flattenPollData 把 {code,msg,data:{…}} 信封展平：data 层键优先并入顶层。
func flattenPollData(parsed map[string]interface{}) map[string]interface{} {
	data, ok := parsed["data"].(map[string]interface{})
	if !ok {
		return parsed
	}
	out := make(map[string]interface{}, len(parsed)+len(data))
	for k, v := range parsed {
		out[k] = v
	}
	for k, v := range data {
		out[k] = v
	}
	return out
}

// pollField 按候选键顺序取第一个非空字符串值。
func pollField(data map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if s, ok := data[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// pollFieldWithFallback 取候选键，全空时用兜底文案。
func pollFieldWithFallback(data map[string]interface{}, fallback string, keys ...string) string {
	if s := pollField(data, keys...); s != "" {
		return s
	}
	return fallback
}

// EnsureFingerprint 确保账号具备设备指纹（machine_id/machine_type/region）。
func EnsureFingerprint(acct *QoderAccount) {
	if acct == nil {
		return
	}
	if acct.MachineType == "" {
		acct.MachineType = defaultMachineType
	}
	if acct.MachineID == "" {
		acct.MachineID = uuid4()
	}
	if acct.Region == "" {
		acct.Region = defaultRegion
	}
}

// RefreshToken 用 refresh_token 换新 access_token（设备流刷新端点）。
func RefreshToken(ctx context.Context, hc *http.Client, acct *QoderAccount) error {
	if acct == nil {
		return errors.New(errNoRefreshToken)
	}
	if acct.RefreshToken == "" {
		return errors.New(errNoRefreshToken)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	body, err := json.Marshal(map[string]interface{}{
		"refresh_token": acct.RefreshToken,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, OpenAPIBase+deviceTokenRefreshPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// 刷新以 refresh_token 自身为凭据，不下发 Authorization（客户端同款）。
	ApplyOpenAPIHeaders(req.Header, "")
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("qoder: 刷新失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errors.New(errLoginExpired)
	case resp.StatusCode != http.StatusOK:
		var errEnv map[string]interface{}
		_ = json.Unmarshal(raw, &errEnv)
		return fmt.Errorf("qoder: 刷新失败: HTTP %d: %s", resp.StatusCode,
			pollFieldWithFallback(errEnv, truncateRunes(string(raw), 200), "errorMessage", "error_message", "message"))
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("qoder: 刷新失败: %w", err)
	}
	parsed = flattenPollData(parsed)
	access := pollField(parsed, "device_token", "token")
	refresh := pollField(parsed, "refresh_token")
	if access == "" || refresh == "" {
		return errors.New(errNoTokenInResp)
	}
	acct.AccessToken = access
	acct.RefreshToken = refresh
	acct.LastRefreshAt = nowRFC3339()
	acct.LastRefreshOK = true
	return nil
}
