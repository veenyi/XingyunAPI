// login.go Qoder CN 设备流 OAuth（PKCE）。
package qoder

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	OAuthWebsiteCN   = "https://qoder.com.cn"
	OAuthOpenapiCN   = "https://openapi.qoder.com.cn"
	OAuthClientID    = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
	OAuthRedirect    = "qoder-work-cn://"
)

// ErrPending 表示授权尚未完成。
var ErrPending = fmt.Errorf("login pending")

// LoginResult 登录成功后的凭证。
type LoginResult struct {
	AccessToken  string // dt-
	RefreshToken string // drt-
	ExpiresIn    int64  // 秒
	UID          string
	Nickname     string
}

type loginState struct {
	Verifier  string
	Nonce     string
	MachineID string
	AuthURL   string
	CreatedAt time.Time
}

// LoginManager 管理设备流登录会话。
type LoginManager struct {
	mu       sync.Mutex
	sessions map[string]*loginState
}

// NewLoginManager 创建登录管理器。
func NewLoginManager() *LoginManager {
	return &LoginManager{
		sessions: make(map[string]*loginState),
	}
}

// Start 发起登录：生成 PKCE 状态，返回 sessionID + 授权 URL。
func (lm *LoginManager) Start() (sessionID, authURL string, err error) {
	verifier, challenge := makePKCE()
	nonce := uuid4()
	machineID := uuid4()
	authURL = fmt.Sprintf("%s/device/selectAccounts?challenge=%s&challenge_method=S256&nonce=%s&machine_id=%s&client_id=%s&redirect_uri=%s",
		OAuthWebsiteCN, challenge, nonce, machineID, OAuthClientID, OAuthRedirect)

	sessionID = uuid4()
	lm.mu.Lock()
	lm.sessions[sessionID] = &loginState{
		Verifier:  verifier,
		Nonce:     nonce,
		MachineID: machineID,
		AuthURL:   authURL,
		CreatedAt: time.Now(),
	}
	lm.mu.Unlock()

	go lm.cleanup()
	return sessionID, authURL, nil
}

// Poll 单次轮询设备令牌；未完成返回 ErrPending。
func (lm *LoginManager) Poll(sessionID string) (LoginResult, string, error) {
	lm.mu.Lock()
	st, ok := lm.sessions[sessionID]
	lm.mu.Unlock()
	if !ok {
		return LoginResult{}, "", fmt.Errorf("session not found")
	}

	url := fmt.Sprintf("%s/api/v1/deviceToken/poll?nonce=%s&verifier=%s&challenge_method=S256",
		OAuthOpenapiCN, st.Nonce, st.Verifier)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return LoginResult{}, "", err
	}
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return LoginResult{}, "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return LoginResult{}, "", ErrPending
	}
	if resp.StatusCode >= 400 {
		return LoginResult{}, "", fmt.Errorf("http %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}

	var tok map[string]any
	if err := json.Unmarshal(raw, &tok); err != nil {
		return LoginResult{}, "", fmt.Errorf("parse: %w", err)
	}

	token := getStr(tok, "token")
	if token == "" {
		token = getStr(tok, "device_token")
	}
	if token == "" {
		return LoginResult{}, "", ErrPending
	}

	refresh := getStr(tok, "refresh_token")
	uid := getStr(tok, "user_id")
	if uid == "" {
		uid = getStr(tok, "uid")
	}
	expiresIn := int64(0)
	if v, ok := tok["expires_in"].(float64); ok {
		expiresIn = int64(v)
	}

	lm.mu.Lock()
	delete(lm.sessions, sessionID)
	lm.mu.Unlock()

	return LoginResult{
		AccessToken:  token,
		RefreshToken: refresh,
		ExpiresIn:    expiresIn,
		UID:          uid,
	}, st.MachineID, nil
}

func (lm *LoginManager) cleanup() {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, st := range lm.sessions {
		if st.CreatedAt.Before(cutoff) {
			delete(lm.sessions, id)
		}
	}
}

func makePKCE() (verifier, challenge string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	buf := make([]byte, 64)
	_, _ = rand.Read(buf)
	var sb strings.Builder
	sb.Grow(64)
	for _, b := range buf {
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	verifier = sb.String()
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}
