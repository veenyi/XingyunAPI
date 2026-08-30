package route

import (
	"fmt"
	"testing"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

// hintError 模拟"渠道自家错误类型带上了 Retry-After"的上游（pkg/compat 就是这种）。
type hintError struct {
	msg    string
	hint   time.Duration
	called bool
}

func (e *hintError) Error() string { return e.msg }

// RetryAfter 用 called 记录路由到底有没有来取过这个建议。
func (e *hintError) RetryAfter() time.Duration {
	e.called = true
	return e.hint
}

func failingUp(name, model string, err error, status int) *fakeChannel {
	return &fakeChannel{
		fakeUp:  &fakeUp{name: name, models: []string{model}, err: err, status: status},
		enabled: true, tier: provider.TierFree,
	}
}

func coolingFor(t *testing.T, reg *health.Registry, prov, model string) time.Duration {
	t.Helper()
	st, ok := reg.Snapshot()[health.Key(prov, model)]
	if !ok {
		t.Fatalf("健康表里没有 %s/%s 的记录", prov, model)
	}
	if st.Status != health.StatusCooling {
		t.Fatalf("状态 = %s，期望 cooling", st.Status)
	}
	return time.Until(st.Until)
}

// 上游说了"5 分钟后再来"，冷却就得是 5 分钟，不能继续按本地档位猜。
func TestNoteFailureHonorsUpstreamRetryHint(t *testing.T) {
	rt, reg := newRouterWithSettings(fakeSettings{SettingFailover: "1"})
	hint := &hintError{msg: "HTTP 429: too many requests", hint: 5 * time.Minute}
	up := failingUp("kf", "a", hint, 429)

	if _, _, err := rt.Chat([]Candidate{{Upstream: up, Provider: "kf", Model: "a"}},
		func(c Candidate) map[string]interface{} { return map[string]interface{}{"model": c.Model} }); err == nil {
		t.Fatal("上游失败时 Chat 必须返回错误")
	}
	if !hint.called {
		t.Fatal("路由没有读取上游的重试建议")
	}
	if got := coolingFor(t, reg, "kf", "a"); got < 4*time.Minute || got > 5*time.Minute {
		t.Fatalf("冷却 %v，期望约 5m（按 Retry-After）", got)
	}
}

// 没给建议的渠道行为必须和改动前一致：按类别档位冷却（这里 longPolicy 统一为 1h）。
func TestNoteFailureWithoutHintUnchanged(t *testing.T) {
	rt, reg := newRouterWithSettings(fakeSettings{SettingFailover: "1"})
	up := failingUp("kf", "a", fmt.Errorf("HTTP 429: too many requests"), 429)

	if _, _, err := rt.Chat([]Candidate{{Upstream: up, Provider: "kf", Model: "a"}},
		func(c Candidate) map[string]interface{} { return map[string]interface{}{"model": c.Model} }); err == nil {
		t.Fatal("上游失败时 Chat 必须返回错误")
	}
	if got := coolingFor(t, reg, "kf", "a"); got < 55*time.Minute {
		t.Fatalf("冷却 %v，期望按档位约 1h", got)
	}
}
