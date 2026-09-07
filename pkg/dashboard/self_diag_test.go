package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSelfDiagLoopbackOnly(t *testing.T) {
	h, _ := setupTestHandler(t)

	// 非本机地址必须 401
	req := makeRequest(t, "GET", "/api/self/diag", nil)
	w := httptest.NewRecorder()
	h.handleSelfDiag(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("remote addr: got %d, want 401", w.Code)
	}

	// 本机请求 200 且含核心区块
	req = makeRequest(t, "GET", "/api/self/diag", nil)
	req.RemoteAddr = "127.0.0.1:45678"
	w = httptest.NewRecorder()
	h.handleSelfDiag(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("loopback: got %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, key := range []string{"accounts", "keepalive", "channels", "checkin", "stats"} {
		if !strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("diag response missing %q: %s", key, body)
		}
	}

	// 不得泄露任何凭据字段
	for _, secret := range []string{"api_token", "pt_key", "ptKey", "access_token", "refresh_token"} {
		if strings.Contains(body, `"`+secret+`"`) {
			t.Fatalf("diag response leaked credential field %q", secret)
		}
	}
}
