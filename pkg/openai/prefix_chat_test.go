package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/route"
)

// fakeKeyless 是一个"当前不提供任何模型"的渠道，用于验证前缀未命中的路由行为。
type fakeKeyless struct{ name string }

func (f fakeKeyless) Name() string                          { return f.name }
func (f fakeKeyless) Enabled() bool                         { return true }
func (f fakeKeyless) Supports(string) bool                  { return false }
func (f fakeKeyless) ListModels() ([]joycode.ModelInfo, error) { return nil, nil }
func (f fakeKeyless) Chat(map[string]interface{}) (map[string]interface{}, error) {
	return nil, io.EOF
}
func (f fakeKeyless) ChatStream(map[string]interface{}) (*http.Response, error) {
	return nil, io.EOF
}

// TestPrefixedModelMissDoesNotBillJoyCode 锁定回归：点名一个带渠道前缀、
// 但没有任何渠道能命中的模型时，必须直接 404，绝不能静默落到付费 JoyCode 账号计费。
func TestPrefixedModelMissDoesNotBillJoyCode(t *testing.T) {
	var joycodeHits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&joycodeHits, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": "should-not-be-used"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer backend.Close()

	st, cleanup, err := newTempStore()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	client := newMockClient(backend)
	srv := NewServer(client, st)
	// 只挂一个"不提供任何模型"的渠道；前缀渠道名也对不上。
	srv.Extras = func() []provider.Keyless { return []provider.Keyless{fakeKeyless{name: "opencode-free"}} }
	srv.Route = route.New(st, nil) // 自动切换路由已接线，验证它不会把请求补回付费池

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":       "nosuchchannel/whatever",
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens":  8,
	})
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("前缀未命中应 404，实际 %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&joycodeHits); n != 0 {
		t.Fatalf("绝不能打到付费 JoyCode 上游，实际命中 %d 次", n)
	}
}
