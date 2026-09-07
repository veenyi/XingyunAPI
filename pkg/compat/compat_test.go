package compat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChatSamplingRetry 上游对 temperature 限制报 400 → 剔除采样参数重试成功。
func TestChatSamplingRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"temperature"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"field Temperature invalid, only 1 is allowed for this model"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := New(Options{Name: "t", BaseURL: srv.URL, Models: []string{"m"}})
	out, err := c.Chat(context.Background(), map[string]interface{}{
		"model":       "m",
		"messages":    []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"temperature": 0.7,
	})
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("应请求两次（首次 400 + 重试），got %d", attempts)
	}
	if out["choices"] == nil {
		t.Fatalf("响应异常: %v", out)
	}
}

// TestChatSamplingRetryDisabled 无采样参数时不重试。
func TestChatSamplingRetryDisabled(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"field Temperature invalid, only 1 is allowed for this model"}}`))
	}))
	defer srv.Close()

	c := New(Options{Name: "t", BaseURL: srv.URL})
	_, err := c.Chat(context.Background(), map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatalf("无参数可剔除，应保留错误")
	}
	if attempts != 1 {
		t.Fatalf("不应重试，got %d", attempts)
	}
}

// TestVendorHintSensenova 日日新旧域名 403 时追加官方端点提示（纯函数测试）。
func TestVendorHintSensenova(t *testing.T) {
	got := vendorHint("https://api.sensenova.cn/v1", http.StatusForbidden, "denied")
	if !strings.Contains(got, "token.sensenova.cn") {
		t.Fatalf("403 应附官方端点提示: %s", got)
	}
	got = vendorHint("https://api.sensenova.cn/v1", http.StatusUnauthorized, "unauthorized")
	if !strings.Contains(got, "token.sensenova.cn") {
		t.Fatalf("401 也应附提示: %s", got)
	}
}

func TestVendorHintOther(t *testing.T) {
	if got := vendorHint("https://api.example.com/v1", 403, "denied"); got != "denied" {
		t.Fatalf("非 sensenova 域名不应附加提示: %s", got)
	}
	if got := vendorHint("https://token.sensenova.cn/v1", 403, "denied"); got != "denied" {
		t.Fatalf("官方域名 403 不应附加提示: %s", got)
	}
}
