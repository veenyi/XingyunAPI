package auth

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── JD 扫码登录（jdhgpt 官方流程，服务器端轮询，适配 NAS） ──────────
//
// 流程（与 JoyCode 客户端 extension.js 一致）：
//   1. 生成 uuid → 登录 URL http://jdhgpt.jd.com/login/pluginlogin?uuid=..&userName=&source=joyCoderFe&ideAppName=JoyCode
//   2. 浏览器打开 URL，用户在京东页面扫码/授权
//   3. 后台 goroutine 轮询 POST http://jdhgpt.jd.com/es/bigdata/pollLoginInfo
//      （SSE 文本，data:{...}，code=="0000" → userToken）
//   4. POST http://jdhgpt.jd.com/login/loginResultCheck → {erp, cookiesStr}
//      ptKey = cookiesStr 中 sso.jd.com cookie 的值
//   5. 返回 ptKey / erp / userToken

const (
	jdhgptPollTimeout  = 300 * time.Second
	jdhgptSessionTTL   = 6 * time.Minute
	jdhgptPollInterval = 2 * time.Second
)

type JdHptResult struct {
	PtKey      string `json:"pt_key"`
	ERP        string `json:"erp"`
	UserToken  string `json:"user_token"`
	CookiesStr string `json:"-"`
}

type JdHptSession struct {
	UUID      string
	LoginURL  string
	Status    string // waiting | confirmed | error | timeout
	Result    *JdHptResult
	Err       string
	CreatedAt time.Time
	done      chan struct{}
}

var (
	jdSessionsMu sync.Mutex
	jdSessions   = make(map[string]*JdHptSession)
	jdJanitor    sync.Once
)

func jdSessionJanitor() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			jdSessionsMu.Lock()
			for id, s := range jdSessions {
				if time.Since(s.CreatedAt) > jdhgptSessionTTL {
					delete(jdSessions, id)
				}
			}
			jdSessionsMu.Unlock()
		}
	}()
}

func newUUIDv4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:32]
}

// JdHptInit 创建扫码登录会话，返回 sessionID 与登录 URL，并启动后台轮询。
func JdHptInit() (sessionID, loginURL string, err error) {
	jdJanitor.Do(jdSessionJanitor)
	uuid := newUUIDv4()
	loginURL = fmt.Sprintf(
		"http://jdhgpt.jd.com/login/pluginlogin?uuid=%s&userName=&source=joyCoderFe&ideAppName=JoyCode",
		uuid,
	)
	sessionID = "jdh_" + uuid

	s := &JdHptSession{
		UUID:      uuid,
		LoginURL:  loginURL,
		Status:    "waiting",
		CreatedAt: time.Now(),
		done:      make(chan struct{}),
	}

	jdSessionsMu.Lock()
	jdSessions[sessionID] = s
	jdSessionsMu.Unlock()

	go jdHptPollLoop(s)
	return sessionID, loginURL, nil
}

// jdHptPollLoop 后台轮询 pollLoginInfo 直到成功/超时。
func jdHptPollLoop(s *JdHptSession) {
	defer close(s.done)

	userToken, err := jdHptPollLoginInfo(s.UUID)
	if err != nil {
		s.Status = "timeout"
		s.Err = err.Error()
		slog.Error("jdhgpt poll login info", "uuid", s.UUID, "error", err)
		return
	}

	// loginResultCheck 换 erp / cookiesStr
	erp, cookiesStr, err := jdHptLoginResultCheck(userToken)
	if err != nil {
		s.Status = "error"
		s.Err = err.Error()
		slog.Error("jdhgpt login result check", "uuid", s.UUID, "error", err)
		return
	}

	ptKey := extractSsoPtKey(cookiesStr)
	if ptKey == "" {
		s.Status = "error"
		s.Err = "登录成功但未获取到 pt_key，请重试"
		slog.Error("jdhgpt pt_key not found", "uuid", s.UUID)
		return
	}

	s.Status = "confirmed"
	s.Result = &JdHptResult{PtKey: ptKey, ERP: erp, UserToken: userToken, CookiesStr: cookiesStr}
	slog.Info("jdhgpt login confirmed", "uuid", s.UUID, "erp", erp, "pt_key_len", len(ptKey))
}

// JdHptStatus 查询会话状态；等待时最多阻塞 pollInterval 返回。
func JdHptStatus(sessionID string) (*JdHptSession, bool) {
	jdSessionsMu.Lock()
	s, ok := jdSessions[sessionID]
	jdSessionsMu.Unlock()
	if !ok {
		return nil, false
	}
	return s, true
}

// jdHptPollLoginInfo 轮询 pollLoginInfo（阻塞直到成功或超时）。
func jdHptPollLoginInfo(uuid string) (string, error) {
	client := &http.Client{Timeout: jdhgptPollTimeout}
	body := fmt.Sprintf(`{"uuid":%q,"sourceType":"encrypt","userName":"","source":"joyCoderFe"}`, uuid)

	deadline := time.Now().Add(jdhgptPollTimeout)
	for time.Now().Before(deadline) {
		resp, err := client.Post(
			"http://jdhgpt.jd.com/es/bigdata/pollLoginInfo",
			"application/json; charset=UTF-8",
			strings.NewReader(body),
		)
		if err != nil {
			// 长连接被服务端断开属正常，重试
			time.Sleep(jdhgptPollInterval)
			continue
		}
		// SSE 流式读取
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			var evt struct {
				Code string          `json:"code"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				continue
			}
			if evt.Code == "0000" {
				resp.Body.Close()
				var token string
				if err := json.Unmarshal(evt.Data, &token); err == nil && token != "" {
					return token, nil
				}
				return string(evt.Data), nil
			}
			// 非 0000：等待用户扫码，继续读流
		}
		resp.Body.Close()
		time.Sleep(jdhgptPollInterval)
	}
	return "", fmt.Errorf("登录超时（5 分钟），请重新扫码")
}

// jdHptLoginResultCheck 用 userToken 换 erp/cookiesStr。
func jdHptLoginResultCheck(userToken string) (erp, cookiesStr string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	body := fmt.Sprintf(`{"userToken":%q,"sourceType":"joyCoderFe","userName":""}`, userToken)
	resp, err := client.Post(
		"http://jdhgpt.jd.com/login/loginResultCheck",
		"application/json; charset=UTF-8",
		strings.NewReader(body),
	)
	if err != nil {
		return "", "", fmt.Errorf("loginResultCheck 请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("loginResultCheck 读取失败: %w", err)
	}
	var result struct {
		Data struct {
			ERP        string `json:"erp"`
			CookiesStr string `json:"cookiesStr"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", "", fmt.Errorf("loginResultCheck 解析失败: %s", string(data[:min(len(data), 200)]))
	}
	return result.Data.ERP, result.Data.CookiesStr, nil
}

// extractSsoPtKey 从 cookiesStr 提取 sso.jd.com cookie 值（pt_key）。
func extractSsoPtKey(cookiesStr string) string {
	// cookiesStr 形如 "sso.jd.com=xxxx; pt_key=...; ..."
	idx := strings.Index(cookiesStr, "sso.jd.com=")
	if idx < 0 {
		return ""
	}
	rest := cookiesStr[idx+len("sso.jd.com="):]
	if semi := strings.Index(rest, ";"); semi >= 0 {
		rest = rest[:semi]
	}
	return strings.TrimSpace(rest)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
