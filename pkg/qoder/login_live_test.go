package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 活体测试：直连 Qoder 真实上游，验证设备流四条链路。
// 默认跳过，QODER_LIVE=1 时才跑（避免 CI/离线环境误打网络）。
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("QODER_LIVE") != "1" {
		t.Skip("set QODER_LIVE=1 to hit the real upstream")
	}
}

func liveManager() *LoginManager {
	return NewLoginManager(&http.Client{Timeout: 20 * time.Second}, nil)
}

func TestLiveStartBuildsAuthURL(t *testing.T) {
	requireLive(t)
	m := liveManager()
	st, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	for _, want := range []string{
		DeviceAuthBase + "/device/selectAccounts?",
		"challenge=", "challenge_method=S256", "nonce=", "machine_id=",
		"client_id=" + DeviceClientID, "redirect_uri=" + urlEscape(DeviceRedirectURI),
	} {
		if !strings.Contains(st.AuthURL, want) {
			t.Errorf("auth_url 缺少 %q\n got: %s", want, st.AuthURL)
		}
	}
	t.Logf("verifier_len=%d nonce=%s", len(st.Verifier), st.Nonce)
}

func TestLivePollBeforeAuthorizeIsPending(t *testing.T) {
	requireLive(t)
	m := liveManager()
	st, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// nonce 未经授权页注册：上游回 404 NotFound，必须判为等待而非报错。
	if _, err := m.pollOnce(context.Background(), st); !errors.Is(err, ErrPending) {
		t.Fatalf("期望 ErrPending，实际 %v (%T)", err, err)
	}
	if _, err := m.Poll(context.Background(), st.SessionID); !errors.Is(err, ErrPending) {
		t.Fatalf("Poll 期望 ErrPending，实际 %v", err)
	}
}

func TestLiveUserInfoRejectsBogusToken(t *testing.T) {
	requireLive(t)
	m := liveManager()
	_, _, err := m.fetchUser(context.Background(), "bogus-token-for-probe")
	if err == nil {
		t.Fatal("伪令牌竟然通过，鉴权规格可疑")
	}
	t.Logf("userinfo 拒绝伪令牌: %v", err)
}

func TestLiveRefreshRejectsBogusToken(t *testing.T) {
	requireLive(t)
	acct := &QoderAccount{UserID: "probe", RefreshToken: "bogus-refresh-for-probe"}
	err := RefreshToken(context.Background(), &http.Client{Timeout: 20 * time.Second}, acct)
	if err == nil {
		t.Fatal("伪 refreshToken 竟然刷新成功")
	}
	t.Logf("refresh 拒绝伪令牌: %v", err)
}

func TestLiveQuotaUsageNeedsAuth(t *testing.T) {
	requireLive(t)
	req, err := http.NewRequest(http.MethodGet, OpenAPIBase+quotaUsagePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	ApplyOpenAPIHeaders(req.Header, "bogus-token-for-probe")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var env map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("期望 401，实际 %d %v", resp.StatusCode, env)
	}
	if got := pollField(env, "code", "errorCode"); got == "" {
		t.Errorf("未识别到错误信封字段: %v", env)
	} else {
		t.Logf("quota/usage 鉴权规格回包 code=%s msg=%s", got, pollField(env, "message", "errorMessage"))
	}
}

func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}
