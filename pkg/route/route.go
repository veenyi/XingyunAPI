// Package route 决定一次对话派给哪个上游，并在被限流、欠费、掉线时按用户
// 排好的顺序换下一个。自动切换关闭时永远只有一个候选，行为与之前完全一致。
package route

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/common"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

const (
	SettingFailover = "route_failover_enabled"
	SettingRanking  = "model_ranking"
	SettingHealth   = "model_health"

	JoyCodeProvider = "joycode"

	defaultMaxRotate = 3

	// streamGrace 是"先不发 HTTP 头，等一等上游握手"的窗口。429/401/404 这类
	// 失败通常几百毫秒内就能拿到，所以还来得及换渠道；慢但正常的上游（JoyCode
	// TTFB 可达 10–30s）超过窗口后照常提交响应头，由心跳继续兜着。
	streamGrace = 3 * time.Second
)

// Candidate 是一张派单：某个上游 + 它对用户可见的模型名。
type Candidate struct {
	Upstream provider.Chat
	Provider string
	Model    string
	// Rotated 表示这不是用户点名那个候选，而是自动切换换出来的。
	Rotated bool
}

func (c Candidate) Key() string { return health.Key(c.Provider, c.Model) }

// Pick 只给一个候选：自动切换开启且首选正在冷却时，返回排序里第一个可用的。
// 用于无法在一次请求内重试的路径（例如响应头必须提前提交的 SSE）。
func (rt *Router) Pick(home Candidate, sources []Source) Candidate {
	cands := rt.Plan(home, sources)
	if len(cands) == 0 {
		return home
	}
	if cands[0].Key() != home.Key() {
		cands[0].Rotated = true
	}
	return cands[0]
}

// Rank 是用户排序里的一项，持久化在 model_ranking 里。
type Rank struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Source 是路由可见的一个上游渠道及它此刻可见的模型名单。
type Source struct {
	Name     string
	Upstream provider.Chat
	// Tier 是计费边界（provider.Tier*）：自动切换只在同边界内进行。
	// 留空表示该渠道自成一边界（按 Name），只和同名渠道互换。
	Tier string
	// Models 是扣掉冷却中模型后的对外可见名单，派单只认它。
	Models func() []string
	// ModelsAll 是不过滤健康度的完整名单；看板的冷却展示与主动探测用它，
	// 否则一个模型刚冷却就会从这两处凭空消失，永远等不到探针确认它复活。
	ModelsAll func() []string
}

type Settings interface {
	GetSetting(key string) string
}

type Router struct {
	settings  Settings
	registry  *health.Registry
	maxRotate int
}

func New(settings Settings, reg *health.Registry) *Router {
	return &Router{settings: settings, registry: reg, maxRotate: defaultMaxRotate}
}

func (rt *Router) Health() *health.Registry { return rt.registry }

// RecordSuccess / RecordFailure 给无法走 Chat/Open 的路径用（Anthropic 的原生
// Claude 分支、必须提前提交响应头的保活流）。只在一次请求的终局结果处调一次，
// 避免同一个失败被两处重复计入冷却。
// 转发路径也用它上报"流已经建好、错误才从流里冒出来"的情况：握手时的
// MarkOK 会让健康表以为模型没问题，下一轮派单还会挑它。
func (rt *Router) RecordSuccess(c Candidate) { rt.noteOK(c) }

func (rt *Router) RecordFailure(c Candidate, err error) { rt.noteFailure(c, err) }

// FailoverEnabled 关掉时不做任何自动切换，只按用户点名的模型走一次。
func (rt *Router) FailoverEnabled() bool {
	return common.SettingEnabled(rt.settings, SettingFailover)
}

// Ranking 读用户排好的顺序；解析失败当作没排序，而不是让请求失败。
func (rt *Router) Ranking() []Rank {
	raw := strings.TrimSpace(rt.get(SettingRanking))
	if raw == "" {
		return nil
	}
	var ranks []Rank
	if err := json.Unmarshal([]byte(raw), &ranks); err != nil {
		slog.Warn("route: model_ranking 解析失败，忽略自动切换顺序", "error", err)
		return nil
	}
	return ranks
}

// rankingOr 用户没排过序时按渠道名单原序兜底：开了自动切换就能立刻生效，
// 一旦在看板里排好顺序，这里完全以他的顺序为准。
func (rt *Router) rankingOr(sources []Source) []Rank {
	if ranks := rt.Ranking(); len(ranks) > 0 {
		return ranks
	}
	ranks := make([]Rank, 0, 16)
	for _, s := range sources {
		if s.Models == nil {
			continue
		}
		for _, m := range s.Models() {
			ranks = append(ranks, Rank{Provider: s.Name, Model: m})
		}
	}
	return ranks
}

func (rt *Router) get(key string) string {
	if rt.settings == nil {
		return ""
	}
	return rt.settings.GetSetting(key)
}

// Plan 生成按序候选，home 来自用户点名的模型。
// 自动切换开启时，正在冷却的 home 会被挪到最后：先试活着的，最后才回去撞墙。
func (rt *Router) Plan(home Candidate, sources []Source) []Candidate {
	cands := []Candidate{home}
	if !rt.FailoverEnabled() {
		return cands
	}
	tier := tierOf(sources, home)
	seen := map[string]bool{home.Key(): true}
	for _, r := range rt.rankingOr(sources) {
		if len(cands) >= rt.maxRotate {
			break
		}
		cand, ok := resolve(sources, r)
		if !ok || seen[cand.Key()] {
			continue
		}
		// 跨计费边界的候选一律不采纳：点名付费模型时不能被换成免费模型，反之亦然。
		if tierOf(sources, cand) != tier {
			continue
		}
		if !rt.available(cand) {
			continue
		}
		seen[cand.Key()] = true
		cands = append(cands, cand)
	}
	homeAvailable := rt.available(home)
	if !homeAvailable {
		if len(cands) == 1 {
			slog.Info("route: 首选在冷却且同计费边界内无其他可用候选，只按点名模型走一次",
				"provider", home.Provider, "model", home.Model, "tier", tier)
			return cands
		}
		slog.Info("route: 首选模型冷却中，先按用户排序尝试其他候选",
			"provider", home.Provider, "model", home.Model)
		cands = append(cands[1:], cands[0])
	}
	return cands
}

// tierOf 找出候选所属渠道的计费边界。名单里没有这个渠道时按渠道名自成一边界，
// 结果是它谁都合不上——宁可不切换，也不把请求换到另一个钱包的渠道上。
func tierOf(sources []Source, c Candidate) string {
	for _, s := range sources {
		if !strings.EqualFold(s.Name, c.Provider) {
			continue
		}
		if s.Tier != "" {
			return s.Tier
		}
		return s.Name
	}
	return c.Provider
}

// JoyCodeSource 用内置白名单而不是实时 modelList：路由每个请求都要枚举候选，
// 每次都打一次上游目录接口太贵。
func JoyCodeSource(up provider.Chat) Source {
	models := append([]string(nil), joycode.Models...)
	return Source{Name: JoyCodeProvider, Upstream: up, Tier: provider.TierAccount,
		Models: func() []string { return models }, ModelsAll: func() []string { return models }}
}

// catalogLister 由能拿到完整目录的渠道实现：可见名单被健康度过滤过，
// 冷却中的模型只剩这一份名单里还在。
type catalogLister interface {
	CatalogIDs() []string
}

// KeylessSource 包装免登录 / 自带 Key 渠道：名单由渠道自己维护（含冷却过滤），
// 计费边界由渠道自己声明（provider.Tiered）；没声明的渠道独占一边界，不会和别的渠道互切。
func KeylessSource(p provider.Keyless) Source {
	s := Source{Name: p.Name(), Upstream: p, Models: func() []string { return ModelNames(p) }}
	if cl, ok := p.(catalogLister); ok {
		s.ModelsAll = func() []string { return CleanNames(cl.CatalogIDs()) }
	} else {
		s.ModelsAll = s.Models
	}
	if t, ok := p.(provider.Tiered); ok {
		s.Tier = t.Tier()
	}
	return s
}

// KeylessSources 过滤掉未启用的渠道后批量包装。
func KeylessSources(ps []provider.Keyless) []Source {
	out := make([]Source, 0, len(ps))
	for _, p := range ps {
		if p == nil || !p.Enabled() {
			continue
		}
		out = append(out, KeylessSource(p))
	}
	return out
}

// ModelNames 取某个上游当前可见的模型名；上游不可用时返回空名单而不是报错。
func ModelNames(up provider.Chat) []string {
	if up == nil {
		return nil
	}
	models, err := up.ListModels()
	if err != nil {
		return nil
	}
	return namesOf(models)
}

func namesOf(models []joycode.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ModelID)
		if id == "" {
			id = strings.TrimSpace(m.Label)
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// CleanNames 去掉名单里的空白项，大小写原样保留：模型名要按上游给的写法展示，
// 比较时再由路由按 EqualFold 处理。
func CleanNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func resolve(sources []Source, r Rank) (Candidate, bool) {
	providerName := strings.TrimSpace(r.Provider)
	model := strings.TrimSpace(r.Model)
	if providerName == "" || model == "" {
		return Candidate{}, false
	}
	for _, s := range sources {
		if !strings.EqualFold(s.Name, providerName) || s.Upstream == nil || s.Models == nil {
			continue
		}
		for _, m := range s.Models() {
			if strings.EqualFold(m, model) {
				return Candidate{Upstream: s.Upstream, Provider: s.Name, Model: m}, true
			}
		}
	}
	return Candidate{}, false
}

func (rt *Router) available(c Candidate) bool {
	if rt.registry == nil {
		return true
	}
	return rt.registry.Available(c.Provider, c.Model)
}

// Chat 依次尝试候选直到成功。返回的 Candidate 才是实际使用的上游，
// 调用方要用它回填响应里的 model 与用量统计，不能继续报用户点名的那个。
func (rt *Router) Chat(cands []Candidate, bodyFor func(Candidate) map[string]interface{}) (map[string]interface{}, Candidate, error) {
	var lastErr error
	for i, c := range cands {
		rt.logAttempt(i, c)
		payload, err := c.Upstream.Chat(bodyFor(c))
		if err == nil {
			rt.noteOK(c)
			return payload, c, nil
		}
		lastErr = err
		rt.noteFailure(c, err)
	}
	return nil, Candidate{}, orNoUpstream(lastErr)
}

// StreamOutcome 是 Open 的结果。Committed 为真表示响应头已发出，
// 这时不可能再改状态码或换渠道，只能把错误写进 SSE 流里。
type StreamOutcome struct {
	Resp      *http.Response
	Candidate Candidate
	Err       error
	Committed bool
}

// Open 建立流式连接：宽限期内失败就继续试下一个候选，超过宽限期就先提交响应头。
func (rt *Router) Open(cands []Candidate, bodyFor func(Candidate) map[string]interface{}, commit func()) StreamOutcome {
	var lastErr error
	for i, c := range cands {
		rt.logAttempt(i, c)
		ch := make(chan streamAttempt, 1)
		go func(c Candidate) {
			resp, err := c.Upstream.ChatStream(bodyFor(c))
			ch <- streamAttempt{resp: resp, err: err}
		}(c)

		timer := time.NewTimer(streamGrace)
		select {
		case a := <-ch:
			timer.Stop()
			if a.err == nil {
				rt.noteOK(c)
				return StreamOutcome{Resp: a.resp, Candidate: c}
			}
			rt.noteFailure(c, a.err)
			lastErr = a.err
		case <-timer.C:
			// 上游还在思考：先给客户端 200 + 心跳，此后只能把错误留在流里。
			if commit != nil {
				commit()
			}
			a := <-ch
			if a.err != nil {
				rt.noteFailure(c, a.err)
				return StreamOutcome{Err: a.err, Committed: true}
			}
			rt.noteOK(c)
			return StreamOutcome{Resp: a.resp, Candidate: c, Committed: true}
		}
	}
	return StreamOutcome{Err: orNoUpstream(lastErr)}
}

type streamAttempt struct {
	resp *http.Response
	err  error
}

func orNoUpstream(err error) error {
	if err != nil {
		return err
	}
	return errors.New("没有可用上游渠道")
}

func (rt *Router) logAttempt(i int, c Candidate) {
	if i > 0 {
		slog.Warn("route: 自动切换到下一个候选", "attempt", i+1, "provider", c.Provider, "model", c.Model)
	}
}

func (rt *Router) noteOK(c Candidate) {
	if rt.registry == nil || c.Provider == "" {
		return
	}
	rt.registry.MarkOK(c.Provider, c.Model)
}

// noteFailure 把上游失败归到冷却档位。400/422 这类请求本身的问题不记账，
// 否则一个坏请求会把整条渠道拖进冷却。
func (rt *Router) noteFailure(c Candidate, err error) {
	if rt.registry == nil || c.Provider == "" || err == nil {
		return
	}
	status := 0
	class := health.ClassOK
	detail := err.Error()
	if ec, ok := c.Upstream.(provider.ErrorClassifier); ok {
		status, class, detail = ec.ClassifyError(err)
	}
	if class == health.ClassOK {
		class = health.Classify(status, detail)
	}
	if class == health.ClassOK {
		return
	}
	// 上游给了 Retry-After 就按它冷却；没给时 hint=0，仍走原有的类别档位递增。
	hint := health.RetryAfter(err)
	if cooldown := rt.registry.MarkFailureAfter(c.Provider, c.Model, class, detail, hint); cooldown > 0 {
		attrs := []any{"provider", c.Provider, "model", c.Model,
			"class", string(class), "cooldown", cooldown.String()}
		if hint > 0 {
			attrs = append(attrs, "retry_after", hint.String())
		}
		slog.Info("route: 候选进入冷却", attrs...)
	}
}
