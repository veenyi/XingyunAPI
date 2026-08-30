package health

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"300", 5 * time.Minute},
		{"  45 ", 45 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"", 0},
		{"not-a-number", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		// 已经过期的时间戳不能让模型冷却到未来某个奇怪的时刻，交回默认档位。
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
	}
	for _, c := range cases {
		if got := ParseRetryAfter(c.raw, now); got != c.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// 上游说了等多久就等多久：不再叠加本地"连续失败递增"猜测。
func TestMarkFailureAfterPrefersUpstreamHint(t *testing.T) {
	reg, clock, _ := newTestRegistry(DefaultPolicy())

	if got := reg.MarkFailureAfter("p", "m", ClassRate, "429", 300*time.Second); got != 300*time.Second {
		t.Fatalf("cooldown = %v, want 300s", got)
	}
	st := reg.Snapshot()[Key("p", "m")]
	if st.Status != StatusCooling {
		t.Fatalf("status = %s, want cooling", st.Status)
	}
	if !st.Until.Equal(clock.now().Add(300 * time.Second)) {
		t.Fatalf("until = %s, want %s", st.Until, clock.now().Add(300*time.Second))
	}
}

// Retry-After 可能被上游乱填：下界保证不出现"冷却 0 秒等于没冷却"，
// 上界不能比"额度用尽"更久，否则一个坏头能把模型永久关出门外。
func TestMarkFailureAfterClampsHint(t *testing.T) {
	reg, _, _ := newTestRegistry(DefaultPolicy())

	if got := reg.MarkFailureAfter("p", "tiny", ClassRate, "429", 500*time.Millisecond); got != time.Second {
		t.Fatalf("tiny hint cooldown = %v, want 1s", got)
	}
	if got := reg.MarkFailureAfter("p", "huge", ClassRate, "429", 9999*time.Hour); got != DefaultPolicy().NoCredit {
		t.Fatalf("huge hint cooldown = %v, want %v", got, DefaultPolicy().NoCredit)
	}
}

// 没有建议时行为必须和改动前完全一致：按类别档位随连续失败递增。
func TestMarkFailureWithoutHintUnchanged(t *testing.T) {
	reg, _, _ := newTestRegistry(DefaultPolicy())

	if got := reg.MarkFailureAfter("p", "m", ClassRate, "429", 0); got != DefaultPolicy().Rate {
		t.Fatalf("first cooldown = %v, want %v", got, DefaultPolicy().Rate)
	}
	if got := reg.MarkFailureAfter("p", "m", ClassRate, "429", 0); got != 2*DefaultPolicy().Rate {
		t.Fatalf("second cooldown = %v, want %v", got, 2*DefaultPolicy().Rate)
	}
}

type hintedError struct{ d time.Duration }

func (e *hintedError) Error() string             { return fmt.Sprintf("HTTP 429: 限流, retry after %v", e.d) }
func (e *hintedError) RetryAfter() time.Duration { return e.d }

func TestRetryAfterUnwrapsHinter(t *testing.T) {
	if got := RetryAfter(nil); got != 0 {
		t.Fatalf("RetryAfter(nil) = %v, want 0", got)
	}
	if got := RetryAfter(errors.New("plain failure")); got != 0 {
		t.Fatalf("plain error hint = %v, want 0", got)
	}
	// 渠道错误常被层层包装，路由层还得能把建议捞回来。
	err := fmt.Errorf("渠道调用失败: %w", &hintedError{d: 4 * time.Minute})
	if got := RetryAfter(err); got != 4*time.Minute {
		t.Fatalf("wrapped hint = %v, want 4m", got)
	}
}
