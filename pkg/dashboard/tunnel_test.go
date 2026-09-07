package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/veenyi/XingyunAPI/pkg/devcloud"
)

func TestTunnelStatusAndInvalidAction(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/devcloud/tunnel", nil))
	if rec.Code != 200 {
		t.Fatalf("GET status: code = %d", rec.Code)
	}
	var st map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["state"] != string(tunnelStopped) {
		t.Fatalf("initial state = %v, want stopped", st["state"])
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/devcloud/tunnel", nil)
	req.Body = http.NoBody
	mux.ServeHTTP(rec, req)
	// 空 body → 解码失败或 action 非法，均不得 500
	if rec.Code >= 500 {
		t.Fatalf("empty body: code = %d", rec.Code)
	}
}

func TestTunnelStartStopIdempotent(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	// 不拨号的启动器：返回错误让 run goroutine 走 backoff 分支，不触碰真实 SSH。
	persisted := ""
	h.tm.Start(func() (*devcloud.Client, error) { return nil, errFakeDial }, func(on bool) {
		if on {
			persisted = "true"
		} else {
			persisted = "false"
		}
	})
	h.tm.Stop(nil)
	if st := h.tm.Status(); st["state"] != string(tunnelStopped) {
		t.Fatalf("after stop state = %v", st["state"])
	}
	if persisted != "true" {
		t.Fatalf("persist flag = %q, want true", persisted)
	}
}

var errFakeDial = &tunnelTestErr{}

type tunnelTestErr struct{}

func (*tunnelTestErr) Error() string { return "fake dial" }
