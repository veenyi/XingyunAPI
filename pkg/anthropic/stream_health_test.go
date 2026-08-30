package anthropic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/route"
)

type rankSettings map[string]string

func (m rankSettings) GetSetting(key string) string { return m[key] }

// /v1/messages 的流里冒出错误帧时，同样要把这个模型打进冷却：
// 握手成功只说明连上了，不代表这一轮能正常结束。
func TestAnthropicStreamErrorFrameCoolsModel(t *testing.T) {
	reg := health.NewRegistry(health.DefaultPolicy(), nil)
	client := joycode.NewClient("pt-test", "user-test")
	client.SetHTTPClient(&http.Client{
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"!\"},\"finish_reason\":\"stop\"}]}\n" +
				"data: {\"error\":{\"message\":\"Rate limit exceeded (429) from upstream\"}}\n"
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		}),
	})
	h := NewHandler(client, nil)
	h.Route = route.New(rankSettings{route.SettingFailover: "1"}, reg)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"GLM-5.1","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	state, ok := reg.Snapshot()[health.Key(route.JoyCodeProvider, "GLM-5.1")]
	if !ok {
		t.Fatalf("流内错误没有记进健康表，响应:\n%s", w.Body.String())
	}
	if state.Status != health.StatusCooling || state.Class != health.ClassRate {
		t.Fatalf("状态 = %s / %s，期望 cooling / rate_limited（明细 %q）", state.Status, state.Class, state.Reason)
	}
	// 已经吐出去的增量照常到达调用方，不能因为后端记了冷却就改写响应。
	if !strings.Contains(w.Body.String(), "Hello") {
		t.Fatalf("正常增量被吞掉了:\n%s", w.Body.String())
	}
}
