package compat

import (
	"net/http"
	"testing"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
)

// 上游在 429 里写了 Retry-After，就必须把它随错误带上去：
// 路由冷却只有拿到这个建议，才不会在配额恢复前反复撞同一堵墙。
func TestRetryAfterHeaderTravelsWithError(t *testing.T) {
	cases := []struct {
		name string
		hdr  string
		want time.Duration
	}{
		{"秒数形式", "300", 5 * time.Minute},
		{"没有该头", "", 0},
		{"乱填的负数", "-1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.hdr != "" {
					w.Header().Set("Retry-After", tc.hdr)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
			})
			c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})

			_, err := c.Chat(map[string]interface{}{"model": "a"})
			if err == nil {
				t.Fatal("429 应返回错误")
			}
			if got := health.RetryAfter(err); got != tc.want {
				t.Fatalf("RetryAfter = %v, want %v", got, tc.want)
			}
			status, class, _ := c.ClassifyError(err)
			if status != http.StatusTooManyRequests || class != health.ClassRate {
				t.Fatalf("归类 = %d/%s，期望 429/rate_limited", status, class)
			}
		})
	}
}

// 流式建连阶段的 429 同样要带上建议：这条路径是自动切换真正会走的路径。
func TestStreamHandshakeRetryAfter(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})

	_, err := c.ChatStream(map[string]interface{}{"model": "a", "stream": true})
	if err == nil {
		t.Fatal("流式 429 应返回错误")
	}
	if got := health.RetryAfter(err); got != 45*time.Second {
		t.Fatalf("RetryAfter = %v, want 45s", got)
	}
}
