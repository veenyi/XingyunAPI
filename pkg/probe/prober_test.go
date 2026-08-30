package probe

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

type fakeSettings map[string]string

func (f fakeSettings) GetSetting(key string) string { return f[key] }

// fakeUp 只实现探针用到的 Chat；计数带锁，一轮探测是并发跑的。
type fakeUp struct {
	name   string
	err    error
	status int

	mu     sync.Mutex
	calls  int
	bodies []map[string]interface{}
}

func (f *fakeUp) Name() string { return f.name }

func (f *fakeUp) ListModels() ([]joycode.ModelInfo, error) { return nil, nil }

func (f *fakeUp) Chat(body map[string]interface{}) (map[string]interface{}, error) {
	f.mu.Lock()
	f.calls++
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return map[string]interface{}{"choices": []interface{}{
		map[string]interface{}{"message": map[string]interface{}{"content": "pong"}},
	}}, nil
}

func (f *fakeUp) ChatStream(map[string]interface{}) (*http.Response, error) {
	return nil, errors.New("探针不走流式")
}

func (f *fakeUp) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// classified 让假上游能上报状态码，探针必须按状态码而不是错误文案选冷却档位。
type classified struct {
	*fakeUp
}

func (c *classified) ClassifyError(err error) (int, health.Class, string) {
	return c.status, health.ClassOK, err.Error()
}

var (
	_ provider.Chat            = (*fakeUp)(nil)
	_ provider.ErrorClassifier = (*classified)(nil)
)

func newRegistry() *health.Registry {
	return health.NewRegistry(health.DefaultPolicy(), nil)
}

func TestBodyIsMinimal(t *testing.T) {
	body := Body("m1")
	if body["model"] != "m1" {
		t.Fatalf("model 应原样带上，得到 %v", body["model"])
	}
	if body["max_tokens"] != 1 || body["stream"] != false {
		t.Fatalf("探针必须最短且非流式，得到 %v/%v", body["max_tokens"], body["stream"])
	}
	msgs, ok := body["messages"].([]map[string]string)
	if !ok || len(msgs) != 1 || msgs[0]["content"] != "ping" {
		t.Fatalf("探针提示词应只有一句 ping，得到 %#v", body["messages"])
	}
}

func TestDefaults(t *testing.T) {
	p := New(fakeSettings{}, newRegistry(), nil)
	if p.Enabled() {
		t.Fatal("探针默认必须关闭：它会消耗真实额度")
	}
	if got := p.Interval(); got != 5*time.Minute {
		t.Fatalf("默认间隔应为 5 分钟，得到 %s", got)
	}

	// 开关认 "1"/"true"，与设置页存的字符串一致。
	on := New(fakeSettings{SettingEnabled: "true"}, newRegistry(), nil)
	if !on.Enabled() {
		t.Fatal("显式打开后应为开")
	}
	// 间隔有下限，防止用户填 0 把上游打成 DDOS 目标。
	fast := New(fakeSettings{SettingInterval: "1"}, newRegistry(), nil)
	if got := fast.Interval(); got != 30*time.Second {
		t.Fatalf("间隔应被抬到下限 30s，得到 %s", got)
	}
	junk := New(fakeSettings{SettingInterval: "abc"}, newRegistry(), nil)
	if got := junk.Interval(); got != 5*time.Minute {
		t.Fatalf("非法间隔应回到默认值，得到 %s", got)
	}
}

func TestRunRecordsOKAndFailure(t *testing.T) {
	ok := &fakeUp{name: "good"}
	rate := &classified{&fakeUp{name: "rate", err: errors.New("too many requests"), status: http.StatusTooManyRequests}}
	reg := newRegistry()
	p := New(fakeSettings{}, reg, func() []Candidate {
		return []Candidate{
			{Provider: "good", Model: "a", Upstream: ok},
			{Provider: "rate", Model: "b", Upstream: rate},
		}
	})

	if got := p.Run(); got != 2 {
		t.Fatalf("本轮应探测 2 个，得到 %d", got)
	}
	if ok.callCount() != 1 || rate.callCount() != 1 {
		t.Fatalf("每个候选应恰好敲一次，得到 %d/%d", ok.callCount(), rate.callCount())
	}
	if !reg.Available("good", "a") {
		t.Fatal("探通的模型不该被冷却")
	}
	if reg.Available("rate", "b") {
		t.Fatal("429 的模型应立刻进入冷却")
	}
	st := reg.Snapshot()[health.Key("rate", "b")]
	if st.Class != health.ClassRate {
		t.Fatalf("应按状态码归类为限流，得到 %v", st.Class)
	}
}

func TestRunSkipsCooldownThatIsNotExpiring(t *testing.T) {
	up := &fakeUp{name: "kf"}
	reg := newRegistry()
	// 欠费冷却 6 小时，远长于探测间隔，没必要每轮去撞。
	reg.MarkFailure("kf", "long", health.ClassNoCredit, "余额不足")
	// 限流冷却 60 秒，短于间隔：探针要在用户之前先确认它复活。
	reg.MarkFailure("kf", "short", health.ClassRate, "429")

	p := New(fakeSettings{}, reg, func() []Candidate {
		return []Candidate{
			{Provider: "kf", Model: "long", Upstream: up},
			{Provider: "kf", Model: "short", Upstream: up},
		}
	})
	if got := p.Run(); got != 1 {
		t.Fatalf("长冷却候选应被跳过，只探测 1 个，得到 %d", got)
	}
	if up.callCount() != 1 {
		t.Fatalf("被跳过的候选一次都不该敲，得到 %d 次", up.callCount())
	}
}

func TestRunIgnoresBlanksAndMissingUpstream(t *testing.T) {
	up := &fakeUp{name: "kf"}
	p := New(fakeSettings{}, newRegistry(), func() []Candidate {
		return []Candidate{
			{Provider: "kf", Model: "", Upstream: up},
			{Provider: "kf", Model: "a", Upstream: nil},
		}
	})
	if got := p.Run(); got != 0 {
		t.Fatalf("空模型名与空上游都不该计入，得到 %d", got)
	}
	if up.callCount() != 0 {
		t.Fatalf("不该发出任何请求，得到 %d 次", up.callCount())
	}

	// list 为 nil / 表为空时也不能 panic。
	if New(fakeSettings{}, newRegistry(), nil).Run() != 0 {
		t.Fatal("没有候选名单时应返回 0")
	}
	if New(fakeSettings{}, nil, func() []Candidate { return []Candidate{{Provider: "kf", Model: "a", Upstream: up}} }).Run() != 0 {
		t.Fatal("没有健康表时应直接跳过而不是崩溃")
	}
}

func TestProbeOneTreatsRequestProblemAsAlive(t *testing.T) {
	// 400 是探针姿势问题，不是渠道故障；判成失败会让用户白白失去一个可用模型。
	up := &classified{&fakeUp{name: "kf", err: errors.New("bad request: temperature must be >= 0"), status: http.StatusBadRequest}}
	reg := newRegistry()
	New(fakeSettings{}, reg, nil).probeOne(Candidate{Provider: "kf", Model: "a", Upstream: up})

	if !reg.Available("kf", "a") {
		t.Fatal("4xx 不该把模型打入冷却")
	}
	if st := reg.Snapshot()[health.Key("kf", "a")]; st.Status != health.StatusOK {
		t.Fatalf("应记为可用，得到 %+v", st)
	}
}

func TestStartNeverProbesWhenDisabled(t *testing.T) {
	up := &fakeUp{name: "kf"}
	p := New(fakeSettings{}, newRegistry(), func() []Candidate {
		return []Candidate{{Provider: "kf", Model: "a", Upstream: up}}
	})
	p.Start()
	time.Sleep(250 * time.Millisecond)
	p.Start() // 重复启动应无副作用
	p.Stop()

	if up.callCount() != 0 {
		t.Fatalf("开关没打开时一个请求都不能发，得到 %d 次", up.callCount())
	}
	p.Stop() // 未运行时再停一次不该卡住或 panic
}

func TestStartProbesOnceImmediatelyWhenEnabled(t *testing.T) {
	up := &fakeUp{name: "kf"}
	p := New(fakeSettings{SettingEnabled: "1", SettingInterval: "30"}, newRegistry(), func() []Candidate {
		return []Candidate{{Provider: "kf", Model: "a", Upstream: up}}
	})
	p.Start()
	deadline := time.Now().Add(2 * time.Second)
	for up.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	p.Stop()

	if up.callCount() == 0 {
		t.Fatal("打开开关后启动应先跑一轮，否则重启后健康表是空的")
	}
}
