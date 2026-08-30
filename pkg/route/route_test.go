package route

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

// ── 测试替身 ───────────────────────────────────────────────────────────

type fakeSettings map[string]string

func (s fakeSettings) GetSetting(key string) string { return s[key] }

// fakeUp 是一个可编程上游：给定模型名单与固定成败，并记录被调用次数。
type fakeUp struct {
	name   string
	models []string
	err    error
	calls  int
	// status 模拟"上游告诉我们它回了哪个状态码"。0 表示连接层就失败了。
	status int
}

func (f *fakeUp) Name() string { return f.name }

func (f *fakeUp) ListModels() ([]joycode.ModelInfo, error) {
	out := make([]joycode.ModelInfo, 0, len(f.models))
	for _, m := range f.models {
		out = append(out, joycode.ModelInfo{ModelID: m, Label: m})
	}
	return out, nil
}

func (f *fakeUp) Chat(map[string]interface{}) (map[string]interface{}, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return map[string]interface{}{"provider": f.name}, nil
}

func (f *fakeUp) ChatStream(map[string]interface{}) (*http.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
	}, nil
}

// ClassifyError 只上报状态码，归类仍交给 health.Classify：
// 路由层判断"要不要冷却"必须同时看状态码和文本，缺一个都会把坏请求当成渠道故障。
func (f *fakeUp) ClassifyError(err error) (int, health.Class, string) {
	return f.status, health.ClassOK, err.Error()
}

// fakeChannel 给 fakeUp 补上"免凭据渠道"那一面，可直接喂给 KeylessSource。
type fakeChannel struct {
	*fakeUp
	enabled bool
	tier    string
}

func (f *fakeChannel) Enabled() bool { return f.enabled }

// Tier 让测试能表达"这两个渠道花的是不是同一份钱"。
func (f *fakeChannel) Tier() string { return f.tier }

func (f *fakeChannel) Supports(model string) bool {
	if !f.enabled {
		return false
	}
	for _, m := range f.models {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}

var (
	_ provider.Chat    = (*fakeUp)(nil)
	_ provider.Keyless = (*fakeChannel)(nil)
	_ provider.Tiered  = (*fakeChannel)(nil)
)

func channel(t *testing.T, name string, models ...string) *fakeChannel {
	t.Helper()
	return &fakeChannel{fakeUp: &fakeUp{name: name, models: models}, enabled: true, tier: provider.TierFree}
}

// untiered 把渠道的计费边界声明抹掉，用来验证"没声明的渠道谁也不跟谁换"。
func untiered(c *fakeChannel) *fakeChannel {
	c.tier = ""
	return c
}

// inTier 改换渠道归属的计费边界。
func inTier(c *fakeChannel, tier string) *fakeChannel {
	c.tier = tier
	return c
}

func broken(name, model, reason string) *fakeChannel {
	return brokenWith(name, model, 0, reason)
}

// brokenWith 造一个固定失败的上游；status=0 表示连 HTTP 响应都没拿到。
func brokenWith(name, model string, status int, reason string) *fakeChannel {
	return &fakeChannel{
		fakeUp:  &fakeUp{name: name, models: []string{model}, err: errors.New(reason), status: status},
		enabled: true, tier: provider.TierFree,
	}
}

func sourcesOf(cs ...*fakeChannel) []Source {
	out := make([]Source, 0, len(cs))
	for _, c := range cs {
		out = append(out, KeylessSource(c))
	}
	return out
}

// longPolicy 把冷却档位统一放大到 1 小时，测试期间不会因为时间流逝自动复活。
func longPolicy() health.Policy {
	return health.Policy{
		Rate: time.Hour, NoCredit: time.Hour, Auth: time.Hour,
		NotFound: time.Hour, Server: time.Hour, Network: time.Hour,
		ServerFails: 3,
	}
}

func newRouterWithSettings(st fakeSettings) (*Router, *health.Registry) {
	reg := health.NewRegistry(longPolicy(), nil)
	return New(st, reg), reg
}

func newTestRouter(st fakeSettings) (*Router, *health.Registry) {
	return newRouterWithSettings(st)
}

func setRanking(t *testing.T, st fakeSettings, ranks ...Rank) {
	t.Helper()
	raw, err := json.Marshal(ranks)
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	st[SettingRanking] = string(raw)
}

func keys(cands []Candidate) string {
	parts := make([]string, 0, len(cands))
	for _, c := range cands {
		parts = append(parts, c.Provider+"/"+c.Model)
	}
	return strings.Join(parts, ",")
}

// ── 候选生成 ───────────────────────────────────────────────────────────

func TestPlanKeepsSingleCandidateWhenFailoverOff(t *testing.T) {
	kf := channel(t, "kf", "a", "b")
	rt, _ := newTestRouter(fakeSettings{}) // 没写过 route_failover_enabled：默认关

	home := Candidate{Upstream: kf, Provider: "kf", Model: "a"}
	if got := keys(rt.Plan(home, sourcesOf(kf))); got != "kf/a" {
		t.Fatalf("开关关闭时必须只按点名的那个走，得到 %s", got)
	}
}

func TestPlanUsesSourceOrderWhenUnranked(t *testing.T) {
	kf := channel(t, "kf", "a", "b", "c", "d")
	rt, _ := newTestRouter(fakeSettings{SettingFailover: "1"})

	home := Candidate{Upstream: kf, Provider: "kf", Model: "a"}
	got := keys(rt.Plan(home, sourcesOf(kf)))
	if got != "kf/a,kf/b,kf/c" {
		t.Fatalf("未排序时按渠道原序补齐并封顶 %d 个，得到 %s", defaultMaxRotate, got)
	}
}

func TestPlanRespectsUserRanking(t *testing.T) {
	kf := channel(t, "kf", "a", "b", "c")
	st := fakeSettings{SettingFailover: "1"}
	setRanking(t, st, Rank{Provider: "kf", Model: "c"}, Rank{Provider: "kf", Model: "b"})
	rt, _ := newTestRouter(st)

	home := Candidate{Upstream: kf, Provider: "kf", Model: "a"}
	if got := keys(rt.Plan(home, sourcesOf(kf))); got != "kf/a,kf/c,kf/b" {
		t.Fatalf("用户排序必须决定后备顺序，得到 %s", got)
	}
}

func TestPlanSkipsCoolingBackup(t *testing.T) {
	kf := channel(t, "kf", "a", "b", "c")
	st := fakeSettings{SettingFailover: "1"}
	setRanking(t, st, Rank{Provider: "kf", Model: "b"}, Rank{Provider: "kf", Model: "c"})
	rt, reg := newRouterWithSettings(st)
	reg.MarkFailure("kf", "b", health.ClassRate, "429")

	home := Candidate{Upstream: kf, Provider: "kf", Model: "a"}
	if got := keys(rt.Plan(home, sourcesOf(kf))); got != "kf/a,kf/c" {
		t.Fatalf("冷却中的后备不该进名单，得到 %s", got)
	}
}

func TestPlanMovesCoolingHomeLast(t *testing.T) {
	kf := channel(t, "kf", "a", "b")
	st := fakeSettings{SettingFailover: "1"}
	setRanking(t, st, Rank{Provider: "kf", Model: "a"}, Rank{Provider: "kf", Model: "b"})
	rt, reg := newRouterWithSettings(st)
	reg.MarkFailure("kf", "a", health.ClassRate, "429")

	home := Candidate{Upstream: kf, Provider: "kf", Model: "a"}
	// 用户点名那个已经在冷却：先试活着的，最后才回去撞墙，而不是直接放弃。
	if got := keys(rt.Plan(home, sourcesOf(kf))); got != "kf/b,kf/a" {
		t.Fatalf("期望 kf/b,kf/a，得到 %s", got)
	}
}

// TestPlanNeverRotatesAcrossBillingTiers 锁住"不跨钱包切换"：
// 实测过点名付费模型、该渠道 Key 失效被冷却，路由却换成免费模型并成功返回，
// 用户以为花的是自己的钱。反向同理，免费请求也不该被推到付费渠道上。
func TestPlanNeverRotatesAcrossBillingTiers(t *testing.T) {
	free := channel(t, "kf", "a", "b")
	keyed := inTier(channel(t, "kd", "z"), provider.TierKeyed)
	st := fakeSettings{SettingFailover: "1"}
	setRanking(t, st, Rank{Provider: "kd", Model: "z"}, Rank{Provider: "kf", Model: "b"})
	rt, _ := newTestRouter(st)

	srcs := sourcesOf(free, keyed)
	home := Candidate{Upstream: free, Provider: "kf", Model: "a"}
	if got := keys(rt.Plan(home, srcs)); got != "kf/a,kf/b" {
		t.Fatalf("免费候选不能切到自带 Key 渠道，得到 %s", got)
	}
	paid := Candidate{Upstream: keyed, Provider: "kd", Model: "z"}
	if got := keys(rt.Plan(paid, srcs)); got != "kd/z" {
		t.Fatalf("付费候选只能留在自己的边界里，哪怕同边界内没有后备，得到 %s", got)
	}
}

// TestPlanUntieredChannelsStayAlone 未声明边界的渠道宁可切不动，也不猜它花谁的钱。
func TestPlanUntieredChannelsStayAlone(t *testing.T) {
	a := untiered(channel(t, "ax", "m"))
	b := untiered(channel(t, "bx", "n"))
	rt, _ := newTestRouter(fakeSettings{SettingFailover: "1"})

	home := Candidate{Upstream: a, Provider: "ax", Model: "m"}
	if got := keys(rt.Plan(home, sourcesOf(a, b))); got != "ax/m" {
		t.Fatalf("没声明边界的渠道只认自己，得到 %s", got)
	}
}

func TestSourceBuildersCarryTier(t *testing.T) {
	if got := KeylessSource(channel(t, "kf", "a")).Tier; got != provider.TierFree {
		t.Fatalf("免登录渠道必须声明 free，得到 %q", got)
	}
	if got := KeylessSource(untiered(channel(t, "kd", "b"))).Tier; got != "" {
		t.Fatalf("未声明边界的渠道该留空，得到 %q", got)
	}
	if got := JoyCodeSource(channel(t, "joycode", "x")).Tier; got != provider.TierAccount {
		t.Fatalf("行云账号渠道必须声明 account，得到 %q", got)
	}
}

func TestTierOfUnknownProvider(t *testing.T) {
	// 名单里查不到的渠道按渠道名自成一边界：结果是谁都合不上，只可能原地试一次。
	if got := tierOf(sourcesOf(channel(t, "kf", "a")), Candidate{Provider: "ghost"}); got != "ghost" {
		t.Fatalf("未知渠道应回落到自己的名字，得到 %q", got)
	}
}

func TestPlanIgnoresGarbageRanking(t *testing.T) {
	kf := channel(t, "kf", "a", "b")
	st := fakeSettings{SettingFailover: "1", SettingRanking: "{not json"}
	rt, _ := newTestRouter(st)
	if len(rt.Ranking()) != 0 {
		t.Fatal("坏数据应当作没排序")
	}
	home := Candidate{Upstream: kf, Provider: "kf", Model: "a"}
	if got := keys(rt.Plan(home, sourcesOf(kf))); got != "kf/a,kf/b" {
		t.Fatalf("坏排序不能让请求失败，得到 %s", got)
	}
}

func TestPlanIgnoresUnknownRankedModels(t *testing.T) {
	kf := channel(t, "kf", "a", "b")
	gone := channel(t, "gone", "x")
	st := fakeSettings{SettingFailover: "1"}
	setRanking(t, st,
		Rank{Provider: "gone", Model: "stale"}, // 渠道在，模型已下架
		Rank{Provider: "missing", Model: "y"},  // 渠道整个没启用
		Rank{Provider: "kf", Model: "b"})
	rt, _ := newTestRouter(st)

	home := Candidate{Upstream: kf, Provider: "kf", Model: "a"}
	got := keys(rt.Plan(home, append(sourcesOf(kf), KeylessSource(gone))))
	if got != "kf/a,kf/b" {
		t.Fatalf("只该补齐真实可解析的候选，得到 %s", got)
	}
}

func TestPickMarksRotatedCandidate(t *testing.T) {
	kf := channel(t, "kf", "a", "b")
	st := fakeSettings{SettingFailover: "1"}
	setRanking(t, st, Rank{Provider: "kf", Model: "a"}, Rank{Provider: "kf", Model: "b"})
	rt, reg := newRouterWithSettings(st)
	reg.MarkFailure("kf", "a", health.ClassRate, "429")

	got := rt.Pick(Candidate{Upstream: kf, Provider: "kf", Model: "a"}, sourcesOf(kf))
	if got.Model != "b" || !got.Rotated {
		t.Fatalf("首选冷却时应换到 b 并标记 Rotated，得到 %+v", got)
	}
	// 没换人时不能打上 Rotated，否则回给客户端的模型名会凭空变化
	plain := rt.Pick(Candidate{Upstream: kf, Provider: "kf", Model: "b"}, sourcesOf(kf))
	if plain.Rotated {
		t.Fatalf("未切换的候选不该带 Rotated: %+v", plain)
	}
}

// ── 失败重试与记账 ─────────────────────────────────────────────────────

func TestChatRotatesUntilSuccess(t *testing.T) {
	bad := broken("kf", "a", "429 Too Many Requests")
	good := channel(t, "kb", "b")
	st := fakeSettings{SettingFailover: "1"}
	setRanking(t, st, Rank{Provider: "kf", Model: "a"}, Rank{Provider: "kb", Model: "b"})
	rt, reg := newRouterWithSettings(st)

	home := Candidate{Upstream: bad, Provider: "kf", Model: "a"}
	cands := rt.Plan(home, sourcesOf(bad, good))
	payload, used, err := rt.Chat(cands, func(c Candidate) map[string]interface{} {
		if c.Model == "" {
			t.Fatal("请求体构造拿不到模型名")
		}
		return map[string]interface{}{"model": c.Model}
	})
	if err != nil {
		t.Fatalf("第二个候选可用就不该报错: %v", err)
	}
	if used.Provider != "kb" || used.Model != "b" {
		t.Fatalf("应返回真正回答的那个上游，得到 %+v", used)
	}
	if payload["provider"] != "kb" {
		t.Fatalf("payload 来自错误上游: %v", payload)
	}
	if bad.calls != 1 || good.calls != 1 {
		t.Fatalf("每个候选各试一次，得到 kf=%d kb=%d", bad.calls, good.calls)
	}
	if reg.Available("kf", "a") {
		t.Fatal("失败的候选必须进入冷却")
	}
	if st := reg.Snapshot()[health.Key("kb", "b")]; st.Status != health.StatusOK {
		t.Fatalf("成功的候选应记为 ok，得到 %+v", st)
	}
}

func TestChatFailsWhenEveryCandidateFails(t *testing.T) {
	bad := broken("kf", "a", "upstream 503 service unavailable")
	st := fakeSettings{SettingFailover: "1"}
	rt, reg := newRouterWithSettings(st)

	home := Candidate{Upstream: bad, Provider: "kf", Model: "a"}
	_, _, err := rt.Chat([]Candidate{home}, func(Candidate) map[string]interface{} { return nil })
	if err == nil {
		t.Fatal("全部候选失败必须把错误透传出去")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("错误里应保留上游原文: %v", err)
	}
	if reg.Available("kf", "a") {
		t.Fatal("5xx 也要进入冷却，否则下一个请求继续撞墙")
	}
}

func TestChatWithoutCandidatesReturnsError(t *testing.T) {
	rt, _ := newTestRouter(fakeSettings{})
	_, _, err := rt.Chat(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "没有可用上游") {
		t.Fatalf("空候选集要给出明确错误，得到 %v", err)
	}
}

func TestOpenRotatesOnImmediateFailure(t *testing.T) {
	bad := broken("kf", "a", "401 unauthorized")
	good := channel(t, "kb", "b")
	st := fakeSettings{SettingFailover: "1"}
	setRanking(t, st, Rank{Provider: "kf", Model: "a"}, Rank{Provider: "kb", Model: "b"})
	rt, reg := newRouterWithSettings(st)

	home := Candidate{Upstream: bad, Provider: "kf", Model: "a"}
	cands := rt.Plan(home, sourcesOf(bad, good))
	committed := false
	out := rt.Open(cands, func(c Candidate) map[string]interface{} {
		return map[string]interface{}{"model": c.Model}
	}, func() { committed = true })
	if out.Err != nil {
		t.Fatalf("换到可用渠道后不该失败: %v", out.Err)
	}
	if out.Candidate.Provider != "kb" {
		t.Fatalf("应连到 kb，得到 %+v", out.Candidate)
	}
	// 快速失败发生在宽限期内，响应头还来得及留给调用方决定，不能提前提交
	if committed || out.Committed {
		t.Fatalf("宽限期内切换不该提交响应头: committed=%v out.Committed=%v", committed, out.Committed)
	}
	defer out.Resp.Body.Close()
	if reg.Available("kf", "a") {
		t.Fatal("401 的候选必须进入冷却")
	}
}

func TestOpenReportsAllFailed(t *testing.T) {
	bad := broken("kf", "a", "connection reset by peer")
	rt, _ := newTestRouter(fakeSettings{})
	out := rt.Open([]Candidate{{Upstream: bad, Provider: "kf", Model: "a"}},
		func(Candidate) map[string]interface{} { return nil }, nil)
	if out.Err == nil {
		t.Fatal("没有可用上游时必须返回错误")
	}
	if out.Resp != nil {
		t.Fatal("失败时不该带响应")
	}
}

func TestRecordHelpersFeedCooldown(t *testing.T) {
	kf := channel(t, "kf", "a")
	rt, reg := newTestRouter(fakeSettings{})
	cand := Candidate{Upstream: kf, Provider: "kf", Model: "a"}

	rt.RecordFailure(cand, errors.New("429 rate limited"))
	if reg.Available("kf", "a") {
		t.Fatal("手工记账的失败也要生效")
	}
	rt.RecordSuccess(cand)
	if !reg.Available("kf", "a") {
		t.Fatal("成功应立刻解除冷却")
	}
	// 请求本身的问题不该拖垮整条渠道
	kf.status = http.StatusBadRequest
	rt.RecordFailure(cand, errors.New("bad request: temperature must be >= 0"))
	if !reg.Available("kf", "a") {
		t.Fatal("4xx 请求错误不该进入冷却")
	}
	for _, st := range reg.Snapshot() {
		if st.Status == health.StatusCooling {
			t.Fatalf("4xx 不该留下冷却记录，得到 %+v", st)
		}
	}
}

// ── 渠道名单 ───────────────────────────────────────────────────────────

func TestKeylessSourcesFiltersDisabledChannels(t *testing.T) {
	on := channel(t, "on", "a")
	off := &fakeChannel{fakeUp: &fakeUp{name: "off", models: []string{"b"}}}

	got := KeylessSources([]provider.Keyless{on, off})
	if len(got) != 1 || got[0].Name != "on" {
		t.Fatalf("只该留下已启用的渠道，得到 %d 个", len(got))
	}
	if models := got[0].Models(); len(models) != 1 || models[0] != "a" {
		t.Fatalf("渠道名单应由渠道自己给出，得到 %v", models)
	}
}

// fullCatalog 模拟 pkg/compat：除可见名单外还能给出未过滤健康度的完整目录。
type fullCatalog struct {
	*fakeChannel
	all []string
}

func (f *fullCatalog) CatalogIDs() []string { return f.all }

func TestKeylessSourceExposesFullCatalog(t *testing.T) {
	src := KeylessSource(&fullCatalog{
		fakeChannel: channel(t, "kf", "a"),
		all:         []string{"a", " B ", "", "  "},
	})
	if got := ModelNames(src.Upstream); strings.Join(got, ",") != "a" {
		t.Fatalf("可见名单由渠道给出，得到 %#v", got)
	}
	if got := src.ModelsAll(); strings.Join(got, ",") != "a,B" {
		t.Fatalf("完整名单应去掉空白但保留原样大小写，得到 %#v", got)
	}

	plain := KeylessSource(channel(t, "kp", "z"))
	if plain.ModelsAll == nil {
		t.Fatal("没实现完整目录的渠道也要有 ModelsAll，不能让上层到处判空")
	}
	if got := plain.ModelsAll(); strings.Join(got, ",") != "z" {
		t.Fatalf("应退回可见名单，得到 %#v", got)
	}
}

func TestJoyCodeSourceUsesBuiltinWhitelist(t *testing.T) {
	src := JoyCodeSource(&fakeUp{name: "joycode", models: []string{"only-this"}})
	if src.Name != JoyCodeProvider {
		t.Fatalf("渠道名应为 %s，得到 %s", JoyCodeProvider, src.Name)
	}
	got := src.Models()
	if len(got) != len(joycode.Models) {
		t.Fatalf("JoyCode 候选应来自内置白名单，得到 %d 个", len(got))
	}
	// 内置名单必须含 JoyAI-Code，否则保活与路由会互相看不到对方
	if !strings.Contains(strings.Join(got, ","), "JoyAI-Code") {
		t.Fatalf("内置白名单里没有 JoyAI-Code: %v", got)
	}
}

func TestModelNamesSkipsBlankIDs(t *testing.T) {
	if got := ModelNames(nil); got != nil {
		t.Fatalf("nil 上游应给空名单，得到 %v", got)
	}
	got := ModelNames(&fakeUp{name: "kf", models: []string{"a", "  "}})
	for _, m := range got {
		if strings.TrimSpace(m) == "" {
			t.Fatalf("名单里混进了空模型名: %#v", got)
		}
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("期望只剩 a，得到 %#v", got)
	}
}
