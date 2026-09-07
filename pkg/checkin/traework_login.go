package checkin

// TraeWork（TRAE SOLO CN）浏览器 PKCE 快捷登录 + 令牌刷新：
//   - Start：生成 PKCE(code_verifier/code_challenge=S256) + RSA 设备密钥对 + 会话，
//     auth_callback_url 指回本服务 /api/checkin/trae_login/callback?session=…；
//   - Callback：浏览器登录完成后 302 携 code 回来，只暂存（白名单路径，无鉴权）；
//   - Poll：前端轮询触发一次性 ExchangeToken 式A → GetUserInfo → 自动入库；
//   - Refresh：ExchangeToken 式B（RefreshToken + DeviceProof RSA-SHA256 签名），
//     无设备私钥时退化为有效性探测。
// 逆向依据 rebuild/spec/traework-cn.md（TRAE SOLO CN v0.1.62 运行时实测）。

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	twClientID     = "en1oxy7wnw8j9n" // TRAE SOLO CN authConfig.SOLO 硬编码
	twIDEVersion   = "0.1.62"
	twAuthzPath    = "/trae/api/v3/oauth/ExchangeToken"
	twUserInfoPath = "/cloudide/api/v3/trae/GetUserInfo"
	twLoginHost    = "https://www.trae.cn"
	twPluginVer    = "2.3.79943"

	twLoginTTL = 30 * time.Minute
)

var errTwNoSession = errors.New("登录会话不存在或已过期")

// twLoginSession 是一次浏览器 PKCE 登录会话（device 密钥对与式B 刷新共用）。
type twLoginSession struct {
	ID        string
	Verifier  string
	Challenge string
	DeviceID  string
	MachineID string
	PrivPEM   string
	AuthURL   string
	CreatedAt time.Time
	ExpiresAt time.Time

	mu      sync.Mutex
	code    string
	account *Account
	err     error
}

// TraeLoginStart 发起 TRAE 浏览器登录：返回 {session_id, auth_url}。
// auth_callback_url 必须是回环地址（TRAE 授权页服务端白名单，实测非回环直接
// 报"登录失败 网络错误"）；登录完成后浏览器 302 到用户本机回环（打不开），
// code 由用户从地址栏复制回面板提交（TraeLoginSubmit）。
func (m *Manager) TraeLoginStart(_ context.Context) (*QoderLoginResult, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("生成 PKCE 失败: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	privPEM, err := twGenerateDeviceKey()
	if err != nil {
		return nil, fmt.Errorf("生成设备密钥失败: %w", err)
	}
	deviceID, machineID := twUUID(), twUUID()

	s := &twLoginSession{
		ID:        randHex(12),
		Verifier:  verifier,
		Challenge: challenge,
		DeviceID:  deviceID,
		MachineID: machineID,
		PrivPEM:   privPEM,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(twLoginTTL),
	}

	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", twPluginVer)
	q.Set("auth_type", "local")
	q.Set("client_id", twClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", twUUID())
	q.Set("auth_callback_url", "http://127.0.0.1:"+twEphemeralPort()+"/authorize")
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	q.Set("x_device_brand", "windows")
	q.Set("x_device_type", "pc")
	q.Set("x_os_version", "10.0.26100")
	q.Set("x_app_version", twIDEVersion)
	q.Set("x_app_type", "ide")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("hide_saas_login", "true")
	s.AuthURL = twLoginHost + "/authorization?" + q.Encode()

	m.mu.Lock()
	for id, old := range m.twSessions {
		if time.Now().After(old.ExpiresAt) {
			delete(m.twSessions, id)
		}
	}
	m.twSessions[s.ID] = s
	m.mu.Unlock()
	slog.Info("checkin: TraeWork 浏览器登录会话已创建", "session_id", s.ID)
	return &QoderLoginResult{
		SessionID: s.ID,
		AuthURL:   s.AuthURL,
		Message:   "请在浏览器打开链接登录 TRAE，登录后把跳转地址粘贴回面板",
	}, nil
}

// TraeLoginSubmit 接收用户粘贴的登录后跳转地址（或裸 code），解析出 AuthCode 暂存。
func (m *Manager) TraeLoginSubmit(sessionID, raw string) error {
	code := twExtractCode(raw)
	if code == "" {
		return errors.New("粘贴内容里没有找到 code 参数，请复制浏览器地址栏的完整链接")
	}
	return m.TraeLoginCallback(sessionID, code)
}

// twExtractCode 从粘贴内容提取授权码：完整 URL 取 query 的 code，否则整段当裸 code。
func twExtractCode(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "\"'<>()")
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Query().Get("code") != "" {
		return u.Query().Get("code")
	}
	if i := strings.Index(raw, "code="); i >= 0 {
		if v, err := url.ParseQuery(raw[i:]); err == nil {
			return v.Get("code")
		}
	}
	if strings.ContainsAny(raw, " \t\r\n?#&=") {
		return ""
	}
	return raw
}

// twEphemeralPort 随机动态端口（回环回调仅用于满足 TRAE 白名单，不需要真实监听）。
func twEphemeralPort() string {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "49152"
	}
	n := 49152 + (int(b[0])<<8|int(b[1]))%16384
	return fmt.Sprintf("%d", n)
}

// TraeLoginCallback 接收浏览器回调的 AuthCode（仅暂存，交换由 Poll 触发）。
func (m *Manager) TraeLoginCallback(sessionID, code string) error {
	if strings.TrimSpace(code) == "" {
		return errors.New("回调缺少 code 参数")
	}
	m.mu.Lock()
	s := m.twSessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return errTwNoSession
	}
	s.mu.Lock()
	if s.account == nil && s.err == nil {
		s.code = code
	}
	s.mu.Unlock()
	return nil
}

// TraeLoginPoll 轮询登录结果：status ∈ ok/waiting/expired/error；
// 暂存有 AuthCode 时执行一次性交换 + GetUserInfo + 自动入库。
func (m *Manager) TraeLoginPoll(ctx context.Context, sessionID string) (*QoderLoginResult, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()
	m.mu.Lock()
	s := m.twSessions[sessionID]
	m.mu.Unlock()
	if s == nil {
		return &QoderLoginResult{Status: "expired", Message: "登录会话不存在或已过期"}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.account != nil {
		return &QoderLoginResult{
			Status:   "ok",
			Message:  "登录成功，账号已加入签到列表",
			Nickname: s.account.displayName(),
			UID:      s.account.UID,
		}, nil
	}
	if s.err != nil {
		return &QoderLoginResult{Status: "error", Message: s.err.Error()}, nil
	}
	if s.code == "" {
		if time.Now().After(s.ExpiresAt) {
			return &QoderLoginResult{Status: "expired", Message: "登录已超时，请重新发起"}, nil
		}
		return &QoderLoginResult{Status: "waiting", Message: "等待浏览器完成登录…"}, nil
	}
	code := s.code
	s.code = ""
	tok, err := m.twExchangeAuthCode(ctx, s, code)
	if err != nil {
		s.err = fmt.Errorf("交换令牌失败: %w", err)
		return &QoderLoginResult{Status: "error", Message: s.err.Error()}, nil
	}
	nick, uid := m.twUserInfo(ctx, tok.access)
	acct := &Account{
		Platform:     platformTraeWork,
		UID:          uid,
		Nickname:     nick,
		AccessToken:  tok.access,
		RefreshToken: tok.refresh,
		DeviceKey:    s.PrivPEM,
		DeviceID:     s.DeviceID,
		MachineID:    s.MachineID,
		Enabled:      true,
	}
	saved, err := m.addTraeAccount(acct)
	if err != nil {
		s.err = err
		return &QoderLoginResult{Status: "error", Message: err.Error()}, nil
	}
	s.account = saved
	slog.Info("checkin: TraeWork 浏览器登录成功", "uid", saved.UID)
	return &QoderLoginResult{
		Status:   "ok",
		Message:  "登录成功，账号已加入签到列表",
		Nickname: saved.displayName(),
		UID:      saved.UID,
	}, nil
}

// twTokens 是交换得到的令牌对。
type twTokens struct {
	access  string
	refresh string
}

// twExchangeAuthCode 用授权码一次性交换令牌（式A）。
func (m *Manager) twExchangeAuthCode(ctx context.Context, s *twLoginSession, code string) (*twTokens, error) {
	body := map[string]interface{}{
		"ClientID":     twClientID,
		"AuthCode":     code,
		"CodeVerifier": s.Verifier,
		"DeviceInfo":   twDeviceInfo(s.DeviceID, s.MachineID, twPubFromPriv(s.PrivPEM)),
		"IDEVersion":   twIDEVersion,
	}
	return m.twExchange(ctx, body)
}

// twExchangeRefresh 用 RefreshToken + DeviceProof 续期（式B）。
func (m *Manager) twExchangeRefresh(ctx context.Context, a *Account) (*twTokens, error) {
	sig, ts, nonce, err := twSignDeviceProof(a.DeviceKey, a.RefreshToken)
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{
		"ClientID":     twClientID,
		"ClientSecret": "",
		"RefreshToken": a.RefreshToken,
		"DeviceInfo":   twDeviceInfo(a.DeviceID, a.MachineID, twPubFromPriv(a.DeviceKey)),
		"DeviceProof":  map[string]interface{}{"Signature": sig, "Timestamp": ts, "Nonce": nonce},
		"IDEVersion":   twIDEVersion,
	}
	return m.twExchange(ctx, body)
}

// twExchange 调 ExchangeToken 并宽容解析 {Token, RefreshToken}（平铺或 data/Result 包裹）。
func (m *Manager) twExchange(ctx context.Context, body map[string]interface{}) (*twTokens, error) {
	env, err := doEnvelope(ctx, m.hc, http.MethodPost, twBase+twAuthzPath, twLoginHeaders(), body)
	if err != nil {
		return nil, err
	}
	if !env.okCode() {
		text := env.text()
		if text == "" {
			text = "上游未返回成功状态"
		}
		return nil, errors.New(text)
	}
	candidates := twResultFields(env)
	access := strAny(candidates, "Token", "token", "AccessToken", "access_token")
	refresh := strAny(candidates, "RefreshToken", "refresh_token")
	if access == "" {
		return nil, errors.New("响应缺少 Token 字段")
	}
	return &twTokens{access: access, refresh: refresh}, nil
}

// twUserInfo 查询 TRAE 用户信息（尽力而为：失败不阻断入库，uid 兜底随机）。
func (m *Manager) twUserInfo(ctx context.Context, token string) (nick, uid string) {
	if token == "" {
		return "", ""
	}
	h := twLoginHeaders()
	h.Set("x-cloudide-token", token)
	env, err := doEnvelope(ctx, m.hc, http.MethodPost, twBase+twUserInfoPath, h,
		map[string]interface{}{"ReqSource": "Lite", "IDEVersion": twIDEVersion})
	if err != nil {
		slog.Warn("checkin: TraeWork 用户信息查询失败（不阻断入库）", "error", err)
		return "", ""
	}
	candidates := twResultFields(env)
	nick = strAny(candidates, "Nickname", "nickname", "DisplayName", "displayName", "UserName", "userName", "Name", "name", "Email", "email")
	uid = strAny(candidates, "UserID", "userId", "userID", "Uid", "uid", "UID", "Email", "email")
	return nick, uid
}

// twResultFields 合并顶层/data/Result 三层字段（TRAE 双信封宽容视图）。
func twResultFields(env *wbEnvelope) map[string]interface{} {
	out := map[string]interface{}{}
	merge := func(src map[string]interface{}) {
		for k, v := range src {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	if env == nil {
		return out
	}
	merge(env.Raw)
	for _, key := range []string{"Result", "result", "Data"} {
		if nested, ok := env.Raw[key].(map[string]interface{}); ok {
			merge(nested)
		}
	}
	merge(env.Data)
	return out
}

// twLoginHeaders 登录/用户信息请求头（无需鉴权）。
func twLoginHeaders() http.Header {
	return http.Header{
		"Content-Type":    {"application/json"},
		"Accept":          {"application/json"},
		"User-Agent":      {twHeaderUserAgent},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
}

// twDeviceInfo 构造设备指纹（DeviceName 由 DeviceID 确定性派生，前后一致）。
func twDeviceInfo(deviceID, machineID, pubPEM string) map[string]interface{} {
	short := strings.ToUpper(strings.ReplaceAll(deviceID, "-", ""))
	if len(short) > 8 {
		short = short[:8]
	}
	return map[string]interface{}{
		"DeviceID":        deviceID,
		"MachineID":       machineID,
		"PlatformCode":    "SOLO_PC",
		"DeviceType":      "PC",
		"DeviceName":      "DESKTOP-" + short,
		"DeviceModel":     "PC",
		"ClientVersion":   twIDEVersion,
		"DevicePublicKey": pubPEM,
		"DeviceBrand":     "windows",
		"DeviceCPU":       "AMD64",
		"OSInfo":          "Windows 11 Pro",
		"OSVersion":       "10.0.26100",
	}
}

// twGenerateDeviceKey 生成 RSA-2048 设备密钥对（PKCS1 私钥 PEM）。
func twGenerateDeviceKey() (privPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(priv), nil
}

// twPubFromPriv 从设备私钥重导出 SPKI 公钥 PEM（式B 复用，Account 只存私钥）。
func twPubFromPriv(privPEM string) string {
	key, err := twParsePrivateKey(privPEM)
	if err != nil {
		return ""
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// twSignDeviceProof 生成式B 设备签名：
// base64(RSA-SHA256(priv, "POST\n/trae/api/v3/oauth/ExchangeToken\n"+clientID+"\n"+refreshToken+"\n"+秒级ts+"\n"+16字节hex nonce))。
func twSignDeviceProof(privPEM, refreshToken string) (sig, ts, nonce string, err error) {
	key, err := twParsePrivateKey(privPEM)
	if err != nil {
		return "", "", "", err
	}
	ts = fmt.Sprintf("%d", time.Now().Unix())
	nb := make([]byte, 16)
	if _, err = rand.Read(nb); err != nil {
		return "", "", "", err
	}
	nonce = hex.EncodeToString(nb)
	payload := "POST\n" + twAuthzPath + "\n" + twClientID + "\n" + refreshToken + "\n" + ts + "\n" + nonce
	h := sha256.Sum256([]byte(payload))
	d, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", "", "", err
	}
	return base64.StdEncoding.EncodeToString(d), ts, nonce, nil
}

// twParsePrivateKey 解析设备私钥（PKCS1 优先，PKCS8 兼容）。
func twParsePrivateKey(privPEM string) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(privPEM))
	if blk == nil {
		return nil, errors.New("设备私钥 PEM 无效")
	}
	if key, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return key, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("设备私钥解析失败: %w", err)
	}
	key, ok := k8.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("设备私钥不是 RSA 密钥")
	}
	return key, nil
}

// twUUID 生成 uuid v4 形态的设备标识。
func twUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strings.ReplaceAll(randHex(8), "", "-")[0:36]
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// addTraeAccount 把浏览器登录成功的账号加入签到列表（按 UID 去重更新）。
func (m *Manager) addTraeAccount(acct *Account) (*Account, error) {
	if acct.UID == "" {
		acct.UID = "trae_" + randHex(4)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var saved *Account
	for _, cur := range m.accounts {
		if cur != nil && cur.Platform == platformTraeWork && cur.UID == acct.UID {
			cur.AccessToken = acct.AccessToken
			cur.RefreshToken = acct.RefreshToken
			if acct.DeviceKey != "" {
				cur.DeviceKey = acct.DeviceKey
			}
			if acct.Nickname != "" {
				cur.Nickname = acct.Nickname
			}
			if acct.DeviceID != "" {
				cur.DeviceID = acct.DeviceID
			}
			if acct.MachineID != "" {
				cur.MachineID = acct.MachineID
			}
			cur.LastRefreshAt = nowStr()
			cur.LastRefreshOK = true
			saved = cur
			break
		}
	}
	if saved == nil {
		acct.ID = "ci_" + randHex(8)
		if acct.Name == "" {
			acct.Name = acct.Nickname
		}
		saved = acct
		m.accounts = append(m.accounts, saved)
	}
	if err := m.saveStored(); err != nil {
		return nil, fmt.Errorf("保存 TraeWork 登录账号失败: %w", err)
	}
	return saved.clone(), nil
}

// twRefresh 静默刷新 TRAE 令牌：有 RefreshToken+DeviceKey 时走式B 轮换；
// 缺设备私钥（旧数据/手工录入）时退化为有效性探测。
func (m *Manager) twRefresh(ctx context.Context, a *Account) error {
	if a.RefreshToken == "" || a.DeviceKey == "" {
		return m.twProbeKeepalive(ctx, a)
	}
	tok, err := m.twExchangeRefresh(ctx, a)
	if err != nil {
		return err
	}
	a.AccessToken = tok.access
	if tok.refresh != "" {
		a.RefreshToken = tok.refresh
	}
	a.LastRefreshAt = nowStr()
	a.LastRefreshOK = true
	return nil
}
