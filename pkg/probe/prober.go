// Package probe 主动嗅探模型可用性：定期用一条最短对话敲一遍每个候选，
// 把结果写进健康度登记表。这样"被限流的模型"不用等用户撞墙才被发现，
// 冷却到期的模型也先由探针确认复活，再把真实请求放过去。
//
// 探针会消耗真实额度，所以默认关闭，且建议只挂免费 / 自带 Key 渠道。
package probe

import (
	"log/slog"
	"sync"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/common"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

const (
	SettingEnabled  = "health_probe_enabled"
	SettingInterval = "health_probe_interval_seconds"

	defaultIntervalSeconds = 300
	minIntervalSeconds     = 30
	// probeConcurrency 一次探测打穿整个名单太久，小并发把一轮压在可接受时间内。
	probeConcurrency = 3
	// probeContent 探针只发一个词、只要一个词的回答，把消耗压到最低。
	probeContent = "ping"
)

// Candidate 是一个待探测的上游 + 模型。
type Candidate struct {
	Provider string
	Model    string
	Upstream provider.Chat
}

type Settings interface {
	GetSetting(key string) string
}

type Prober struct {
	settings Settings
	registry *health.Registry
	list     func() []Candidate

	stop    chan struct{}
	done    chan struct{}
	running bool
	mu      sync.Mutex
}

func New(settings Settings, reg *health.Registry, list func() []Candidate) *Prober {
	return &Prober{settings: settings, registry: reg, list: list, done: make(chan struct{})}
}

// Enabled 探针默认关闭：它会真实消耗额度，必须由用户在看板里主动打开。
func (p *Prober) Enabled() bool {
	return common.SettingEnabled(p.settings, SettingEnabled)
}

func (p *Prober) Interval() time.Duration {
	sec := common.SettingInt(p.settings, SettingInterval, defaultIntervalSeconds)
	if sec < minIntervalSeconds {
		sec = minIntervalSeconds
	}
	return time.Duration(sec) * time.Second
}

// Start 拉起后台探测循环；已开启时重复调用无副作用。
func (p *Prober) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return
	}
	p.running = true
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	go p.loop(p.stop, p.done)
}

func (p *Prober) loop(stop chan struct{}, done chan struct{}) {
	defer close(done)
	// 启动先跑一轮：进程重启后健康表是空的，用户第一眼就该看到真实状态。
	// 但开关没打开时一个请求都不能发——探针是花真金白银的。
	if p.Enabled() {
		p.Run()
	}
	for {
		timer := time.NewTimer(p.Interval())
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			if p.Enabled() {
				p.Run()
			}
		}
	}
}

// Stop 结束后台探测并等待当前一轮退出。
func (p *Prober) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return
	}
	close(p.stop)
	<-p.done
	p.running = false
}

// Run 执行一轮探测：并发把名单里每个候选敲一次，结果写回健康表。
func (p *Prober) Run() int {
	if p.list == nil || p.registry == nil {
		return 0
	}
	cands := p.list()
	interval := p.Interval()
	skip := p.skippable(interval)
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	var probed int
	var countMu sync.Mutex
	for _, c := range cands {
		if c.Upstream == nil || c.Model == "" {
			continue
		}
		if skip[health.Key(c.Provider, c.Model)] {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(c Candidate) {
			defer wg.Done()
			defer func() { <-sem }()
			p.probeOne(c)
			countMu.Lock()
			probed++
			countMu.Unlock()
		}(c)
	}
	wg.Wait()
	if probed > 0 {
		slog.Debug("probe: round finished", "probed", probed)
	}
	return probed
}

// skippable 返回"冷却还没接近到期、这次不必敲"的候选：
// 剩余冷却明显长于探测间隔的（例如欠费 6 小时）没必要每 5 分钟去撞一次。
func (p *Prober) skippable(interval time.Duration) map[string]bool {
	out := map[string]bool{}
	now := time.Now()
	for k, st := range p.registry.Snapshot() {
		if st.Status != health.StatusCooling || st.Until.IsZero() {
			continue
		}
		if st.Until.Sub(now) > interval {
			out[k] = true
		}
	}
	return out
}

func (p *Prober) probeOne(c Candidate) {
	_, err := c.Upstream.Chat(Body(c.Model))
	if err == nil {
		p.registry.MarkOK(c.Provider, c.Model)
		slog.Debug("probe: 模型可用", "provider", c.Provider, "model", c.Model)
		return
	}
	class, detail := classify(c.Upstream, err)
	if class == health.ClassOK {
		// 请求本身被拒（400 一类）说明上游活着，只是探针姿势不对——不记失败。
		p.registry.MarkOK(c.Provider, c.Model)
		return
	}
	// 探针没有真实用户在场，更要把上游的重试建议当真：按 60s 档冷却会让每轮探测
	// 都去撞同一个明确说了"一小时后再来"的端点。
	hint := health.RetryAfter(err)
	if cooldown := p.registry.MarkFailureAfter(c.Provider, c.Model, class, "主动探测: "+detail, hint); cooldown > 0 {
		attrs := []any{"provider", c.Provider, "model", c.Model,
			"class", string(class), "cooldown", cooldown.String()}
		if hint > 0 {
			attrs = append(attrs, "retry_after", hint.String())
		}
		slog.Info("probe: 模型不可用，转入冷却", attrs...)
	}
}

func classify(up provider.Chat, err error) (health.Class, string) {
	status := 0
	class := health.ClassOK
	detail := err.Error()
	if ec, ok := up.(provider.ErrorClassifier); ok {
		status, class, detail = ec.ClassifyError(err)
	}
	if class == health.ClassOK {
		class = health.Classify(status, detail)
	}
	return class, detail
}

// Body 构造一条最省的对话请求：一个词的问题、一个词的回答。
func Body(model string) map[string]interface{} {
	return map[string]interface{}{
		"model":       model,
		"messages":    []map[string]string{{"role": "user", "content": probeContent}},
		"max_tokens":  1,
		"stream":      false,
		"temperature": 0,
	}
}
