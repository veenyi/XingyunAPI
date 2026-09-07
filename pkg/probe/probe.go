// Package probe 主动探测模型可用性：周期性对冷却中的免费渠道模型发送
// 1-token 探测请求，恢复即提前出冷却（「probe: 模型不可用，转入冷却」/
// 「health: 已恢复冷却中的模型状态」）。探测会消耗真实额度，默认关闭。
package probe

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/common"
	"github.com/veenyi/XingyunAPI/pkg/compat"
	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/provider"
	"github.com/veenyi/XingyunAPI/pkg/route"
)

// Settings is the prober's view of the settings store.
type Settings interface {
	GetSetting(key string) string
}

// Candidate is one probe target.
type Candidate struct {
	Provider string
	Model    string
	Cl       provider.Keyless
}

// Prober runs the periodic availability probe loop.
type Prober struct {
	router *route.Router
	store  Settings

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func New(rt *route.Router, st Settings) *Prober {
	return &Prober{router: rt, store: st, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start launches the background loop (no-op when probing is disabled).
func (p *Prober) Start() {
	if !common.SettingEnabled(p.store, "health_probe_enabled") {
		slog.Info("probe: 主动探测未启用，跳过后台探测")
		close(p.done)
		return
	}
	go p.loop()
}

func (p *Prober) Stop() {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
}

func (p *Prober) interval() time.Duration {
	sec := common.SettingInt(p.store, "health_probe_interval_seconds", 300)
	if sec < 30 {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}

func (p *Prober) loop() {
	defer close(p.done)
	for {
		p.Run()
		select {
		case <-p.stop:
			return
		case <-time.After(p.interval()):
		}
	}
}

// Run performs one probing pass over cooling candidates.
func (p *Prober) Run() {
	for _, c := range p.candidates() {
		if p.skippable(c) {
			continue
		}
		p.probeOne(c)
	}
}

// candidates collects models currently in cooldown on enabled keyless
// channels (恢复探测只针对冷却中的行，正常模型不消耗额度).
func (p *Prober) candidates() []Candidate {
	var out []Candidate
	if p.router == nil || p.router.Health == nil {
		return nil
	}
	cooling := map[string]bool{}
	for _, st := range p.router.Health.Snapshot() {
		if st.Status == "cooling" {
			cooling[st.Provider+"|"+st.Model] = true
		}
	}
	for _, ch := range p.router.KeylessChannels() {
		if !ch.Enabled() {
			continue
		}
		for _, m := range ch.ListModels() {
			if cooling[ch.Name()+"|"+m] {
				out = append(out, Candidate{Provider: ch.Name(), Model: m, Cl: ch})
			}
		}
	}
	return out
}

func (p *Prober) skippable(c Candidate) bool {
	if c.Cl == nil || c.Model == "" {
		return true
	}
	if p.router.Blocked(c.Provider, c.Model) {
		return true
	}
	return false
}

// probeOne sends a 1-token chat probe and updates the health registry.
func (p *Prober) probeOne(c Candidate) {
	body := map[string]interface{}{
		"model":      c.Model,
		"messages":   []map[string]string{{"role": "user", "content": "1"}},
		"max_tokens": 1,
		"stream":     false,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := c.Cl.Chat(ctx, body)
	if err == nil {
		err = compat.LooksLikeError(out)
	}
	if err != nil {
		class := health.ClassServerError
		if cf, ok := c.Cl.(interface{ ClassifyError(error) string }); ok {
			class = cf.ClassifyError(err)
		} else {
			class = health.Classify(err)
		}
		slog.Warn("probe: 模型不可用，转入冷却", "provider", c.Provider, "model", c.Model, "class", class, "error", err)
		p.router.Health.MarkFailure(c.Provider, c.Model, err)
		return
	}
	// candidates() 只收集冷却中的行，这里恢复即出冷却。
	p.router.Health.MarkOK(c.Provider, c.Model)
	slog.Info("health: 已恢复冷却中的模型状态", "provider", c.Provider, "model", c.Model)
}
