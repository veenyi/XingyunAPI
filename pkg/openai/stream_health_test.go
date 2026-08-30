package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/compat"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/route"
)

// mapSettings 是最小的 route.Settings / common 开关读取实现。
type mapSettings map[string]string

func (m mapSettings) GetSetting(key string) string { return m[key] }

// 上游把 429 裹在已经建好的 SSE 流里（HTTP 头是 200）时，握手时记的"可用"
// 必须被改回来：否则下一轮派单还会挑同一个模型，用户每次都撞同一堵墙。
func TestStreamInboundErrorCoolsModel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"fake-free-model","object":"model"}]}`)
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial-\"}}]}\n\n")
			io.WriteString(w, "data: {\"error\":{\"message\":\"Rate limit exceeded (429) from upstream\"}}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	reg := health.NewRegistry(health.DefaultPolicy(), nil)
	upstream := compat.New(compat.Config{
		Name:           "fake-free",
		DefaultBaseURL: backend.URL,
		Enabled:        func() bool { return true },
		Health:         reg,
	})
	srv := &Server{
		Keyfree: upstream,
		Route:   route.New(mapSettings{route.SettingFailover: "1"}, reg),
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"fake-free-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("流应该照常以 200 转发，得到 %d: %s", resp.StatusCode, body)
	}
	// 已经吐出去的增量不能撤回，必须原样到达调用方。
	if !strings.Contains(string(body), "partial-") {
		t.Fatalf("正常增量被吞掉了: %s", body)
	}

	state, ok := reg.Snapshot()[health.Key("fake-free", "fake-free-model")]
	if !ok {
		t.Fatal("流内错误没有记进健康表")
	}
	if state.Status != health.StatusCooling {
		t.Fatalf("状态 = %s，期望 cooling（流内 429 必须冷却）", state.Status)
	}
	if state.Class != health.ClassRate {
		t.Fatalf("归类 = %s，期望 rate_limited，明细 %q", state.Class, state.Reason)
	}
	if state.Until.Before(time.Now()) {
		t.Fatalf("冷却截止时间异常: %s", state.Until)
	}
	// 冷却中的模型从对外目录里消失，自动切换不会再挑到它；
	// 但用户点名要它时 Supports 仍然认（故意不静默换成别的模型）。
	for _, id := range upstream.ModelIDs() {
		if strings.EqualFold(id, "fake-free-model") {
			t.Fatal("冷却中的模型仍然对外可见")
		}
	}
	if !upstream.Supports("fake-free-model") {
		t.Fatal("点名的冷却模型应当仍可直连，不能被悄悄换掉")
	}
}

// 没有错误帧的正常流不该被误判：MarkOK 之后仍然可用。
func TestStreamWithoutErrorStaysHealthy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"fake-free-model","object":"model"}]}`)
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fine\"}}]}\n\n")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"!\"},\"finish_reason\":\"stop\"}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	reg := health.NewRegistry(health.DefaultPolicy(), nil)
	upstream := compat.New(compat.Config{
		Name:           "fake-free",
		DefaultBaseURL: backend.URL,
		Enabled:        func() bool { return true },
		Health:         reg,
	})
	srv := &Server{
		Keyfree: upstream,
		Route:   route.New(mapSettings{route.SettingFailover: "1"}, reg),
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"fake-free-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("得到 %d: %s", resp.StatusCode, body)
	}
	if state, ok := reg.Snapshot()[health.Key("fake-free", "fake-free-model")]; ok && state.Status != health.StatusOK {
		t.Fatalf("正常流之后状态 = %s，期望 ok", state.Status)
	}
	if !upstream.Supports("fake-free-model") {
		t.Fatal("正常流之后模型应当仍可用")
	}
}
