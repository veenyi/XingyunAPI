package health

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestClassifyByStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Class
	}{
		{http.StatusTooManyRequests, "", ClassRate},
		{http.StatusPaymentRequired, "", ClassNoCredit},
		{http.StatusInsufficientStorage, "", ClassNoCredit},
		{http.StatusUnauthorized, "", ClassAuth},
		{http.StatusForbidden, "", ClassAuth},
		{http.StatusNotFound, "", ClassNotFound},
		{http.StatusInternalServerError, "", ClassServer},
		{http.StatusBadGateway, "", ClassServer},
		// 400/422 是请求本身的问题，不该把整条渠道拖进冷却
		{http.StatusBadRequest, "max_tokens must be positive", ClassOK},
		// 200 包错误体：只看文本
		{http.StatusOK, `{"error":{"message":"Too Many Requests"}}`, ClassRate},
		{http.StatusOK, `{"error":{"message":"余额不足"}}`, ClassNoCredit},
		{0, "dial tcp: connection refused", ClassNetwork},
		{0, "429 Too Many Requests", ClassRate},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %s, want %s", c.status, c.body, got, c.want)
		}
	}
}

func TestClassifyErr(t *testing.T) {
	if got := ClassifyErr(nil); got != ClassOK {
		t.Fatalf("ClassifyErr(nil) = %s, want ok", got)
	}
	if got := ClassifyErr(errors.New("context deadline exceeded")); got != ClassNetwork {
		t.Fatalf("timeout should be network, got %s", got)
	}
	if got := ClassifyErr(errors.New("upstream: 限流")); got != ClassRate {
		t.Fatalf("rate wording should be rate_limited, got %s", got)
	}
}

// fakeClock 让冷却到期在测试里可控，不用真等 60 秒。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestRegistry(policy Policy) (*Registry, *fakeClock, *string) {
	clock := &fakeClock{t: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
	var saved string
	reg := NewRegistry(policy, func(raw string) { saved = raw })
	reg.now = clock.now
	return reg, clock, &saved
}

func TestCooldownScalesWithConsecutiveFailures(t *testing.T) {
	reg, _, _ := newTestRegistry(DefaultPolicy())

	if got := reg.MarkFailure("p", "m", ClassRate, "429"); got != DefaultPolicy().Rate {
		t.Fatalf("first cooldown = %v, want %v", got, DefaultPolicy().Rate)
	}
	if got := reg.MarkFailure("p", "m", ClassRate, "429"); got != 2*DefaultPolicy().Rate {
		t.Fatalf("second cooldown = %v, want %v", got, 2*DefaultPolicy().Rate)
	}
	// 递增要封顶，否则一个长期坏掉的模型会被关到不可用
	for i := 0; i < 10; i++ {
		reg.MarkFailure("p", "m", ClassRate, "429")
	}
	if got := reg.MarkFailure("p", "m", ClassRate, "429"); got != 6*DefaultPolicy().Rate {
		t.Fatalf("cooldown should cap at 6x, got %v", got)
	}
	if reg.Available("p", "m") {
		t.Fatal("model in cooldown must not be available")
	}
}

func TestRequestProblemDoesNotCooldown(t *testing.T) {
	reg, _, _ := newTestRegistry(DefaultPolicy())
	if got := reg.MarkFailure("p", "m", ClassOK, "bad request"); got != 0 {
		t.Fatalf("ClassOK must not cool down, got %v", got)
	}
	if !reg.Available("p", "m") {
		t.Fatal("model should stay available after a non-channel failure")
	}
}

func TestCooldownExpiresBackToAvailable(t *testing.T) {
	reg, clock, _ := newTestRegistry(DefaultPolicy())
	reg.MarkFailure("p", "m", ClassRate, "429")
	clock.advance(DefaultPolicy().Rate - time.Second)
	if reg.Available("p", "m") {
		t.Fatal("still inside cooldown window")
	}
	clock.advance(2 * time.Second)
	if !reg.Available("p", "m") {
		t.Fatal("cooldown must expire on its own")
	}
	// 到期后失败计数清零：下一次冷却从最短档重新开始，而不是继续累加
	if got := reg.MarkFailure("p", "m", ClassRate, "429"); got != DefaultPolicy().Rate {
		t.Fatalf("cooldown should restart at base after expiry, got %v", got)
	}
}

func TestMarkOKClearsCooldown(t *testing.T) {
	reg, _, _ := newTestRegistry(DefaultPolicy())
	reg.MarkFailure("p", "m", ClassNoCredit, "欠费")
	if reg.Available("p", "m") {
		t.Fatal("expected cooling")
	}
	reg.MarkOK("p", "m")
	if !reg.Available("p", "m") {
		t.Fatal("MarkOK must clear cooldown")
	}
	if reg.Failures("p", "m") != 0 {
		t.Fatalf("MarkOK must reset failures, got %d", reg.Failures("p", "m"))
	}
}

func TestKeyNormalizesModelName(t *testing.T) {
	reg, _, _ := newTestRegistry(DefaultPolicy())
	reg.MarkFailure("p", "GLM-5.1", ClassRate, "429")
	if reg.Available("p", "glm-5.1") {
		t.Fatal("model name lookup must be case-insensitive")
	}
	if reg.Available("p", " GLM-5.1 ") {
		t.Fatal("model name lookup must ignore surrounding spaces")
	}
}

func TestPersistAndRestoreRoundTrip(t *testing.T) {
	reg, clock, saved := newTestRegistry(DefaultPolicy())
	reg.MarkFailure("kf", "hy3-free", ClassRate, "429")
	if *saved == "" {
		t.Fatal("registry must emit a snapshot on every state change")
	}
	var dump map[string]State
	if err := json.Unmarshal([]byte(*saved), &dump); err != nil {
		t.Fatalf("persisted snapshot is not valid JSON: %v", err)
	}
	st, ok := dump[Key("kf", "hy3-free")]
	if !ok || st.Status != StatusCooling {
		t.Fatalf("persisted state missing or wrong: %+v", dump)
	}

	// 新进程（新表）读回未到期冷却；已到期与 ok 状态一律不恢复
	restored, _, _ := newTestRegistry(DefaultPolicy())
	restored.now = clock.now
	restored.Restore(*saved)
	if restored.Available("kf", "hy3-free") {
		t.Fatal("cooldown must survive a restart")
	}

	expired, _, _ := newTestRegistry(DefaultPolicy())
	expired.now = func() time.Time { return clock.now().Add(24 * time.Hour) }
	expired.Restore(*saved)
	if !expired.Available("kf", "hy3-free") {
		t.Fatal("expired cooldown must not be restored")
	}

	broken, _, _ := newTestRegistry(DefaultPolicy())
	broken.Restore("{not json")
	if len(broken.Snapshot()) != 0 {
		t.Fatal("garbage input must be ignored, not loaded")
	}
}

func TestSnapshotReportsOnlyLiveCooldown(t *testing.T) {
	reg, clock, _ := newTestRegistry(DefaultPolicy())
	reg.MarkFailure("p", "cooling", ClassServer, "502")
	reg.MarkFailure("p", "expired", ClassNetwork, "eof")
	clock.advance(time.Minute)

	snap := reg.Snapshot()
	if got := snap[Key("p", "expired")].Status; got != StatusOK {
		t.Fatalf("expired entry should report ok, got %s", got)
	}
	st := snap[Key("p", "cooling")]
	if st.Status != StatusCooling || st.Until.IsZero() {
		t.Fatalf("cooling entry should carry an until time, got %+v", st)
	}
}

func TestAvailableOnNilRegistryIsOptimistic(t *testing.T) {
	var reg *Registry
	if !reg.Available("p", "m") {
		t.Fatal("nil registry must allow every model through")
	}
}

func TestTrimKeepsReasonShort(t *testing.T) {
	long := ""
	for i := 0; i < 400; i++ {
		long += "x "
	}
	if got := len(trim(long)); got > 170 {
		t.Fatalf("reason should be truncated, got %d chars", got)
	}
}
