package compat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
)

// 说明：生产环境里 DefaultBaseURL 恒为公网 https，地址校验发生在设置读取那一层
// （cfg.BaseURL 返回值）。测试直接把 httptest 的地址放进 DefaultBaseURL，
// 绕开的是"管理员填的地址"这道门，没有放松任何线上约束。

// stub 起一个假上游，返回地址与累计请求数。
type stub struct {
	baseURL string
	hits    *atomic.Int64
}

func newStub(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *stub {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return &stub{baseURL: srv.URL, hits: &hits}
}

func (s *stub) count() int64 { return s.hits.Load() }

func jsonBody(w http.ResponseWriter, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func catalogHandler(ids ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		jsonBody(w, map[string]interface{}{"data": data})
	}
}

// ── 地址与鉴权头 ───────────────────────────────────────────────────────

func TestBaseURLResolution(t *testing.T) {
	cases := []struct {
		name    string
		setting string
		want    string
	}{
		{"未填时回落到内置地址", "", "https://default.example/v1"},
		{"只有空白也算未填", "   ", "https://default.example/v1"},
		{"尾部斜杠被去掉", "https://api.example/v1/", "https://api.example/v1"},
		{"内网地址被拒后回落到内置地址", "https://127.0.0.1:9999/v1", "https://default.example/v1"},
		{"明文 http 被拒后回落到内置地址", "http://api.example.com/v1", "https://default.example/v1"},
		{"坏得解析不了的地址同样回落", "https://%zz/v1", "https://default.example/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.setting
			c := New(Config{DefaultBaseURL: "https://default.example/v1", BaseURL: func() string { return v }})
			if got := c.baseURL(); got != tc.want {
				t.Fatalf("baseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAuthorizationHeaderOnlyWithKey(t *testing.T) {
	var gotAuth []string
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		jsonBody(w, map[string]interface{}{"data": []map[string]string{{"id": "a"}}})
	})

	keyless := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	if _, err := keyless.fetchCatalog(); err != nil {
		t.Fatalf("匿名目录应能拉通: %v", err)
	}
	if got := keyless.userAgent(); got != "XingyunAPI" {
		t.Fatalf("缺版本号时 UA 应退化为产品名，得到 %q", got)
	}

	withKey := New(Config{Name: "p", DefaultBaseURL: s.baseURL,
		APIKey: func() string { return "  sk-demo  " }, Version: "9.9.9"})
	if _, err := withKey.fetchCatalog(); err != nil {
		t.Fatalf("带 Key 目录应能拉通: %v", err)
	}
	if got := withKey.userAgent(); got != "XingyunAPI/9.9.9" {
		t.Fatalf("UA 应带版本号，得到 %q", got)
	}

	if len(gotAuth) != 2 || gotAuth[0] != "" || gotAuth[1] != "Bearer sk-demo" {
		t.Fatalf("匿名请求不该带 Authorization，带 Key 请求应去空白后加 Bearer 前缀，得到 %#v", gotAuth)
	}
}

// ── 目录拉取与缓存 ─────────────────────────────────────────────────────

func TestFetchCatalogSkipsBlankIDs(t *testing.T) {
	s := newStub(t, catalogHandler("a", "  ", "b"))
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	ids, err := c.fetchCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("空白模型名应被丢掉，得到 %#v", ids)
	}
}

func TestFetchCatalogEmptyIsError(t *testing.T) {
	// "目录为空"必须当成失败：否则渠道会从看板上静默消失，而不是显示降级名单。
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonBody(w, map[string]interface{}{"data": []interface{}{}})
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	if _, err := c.fetchCatalog(); err == nil {
		t.Fatal("空目录应报错")
	}

	bad := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	})
	failing := New(Config{Name: "p", DefaultBaseURL: bad.baseURL})
	if _, err := failing.fetchCatalog(); err == nil {
		t.Fatal("非 200 目录应报错")
	}
}

func TestCachedIDsUsesPositiveCache(t *testing.T) {
	s := newStub(t, catalogHandler("a", "b"))
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	if _, err := c.cachedIDs(); err != nil {
		t.Fatal(err)
	}
	ids, err := c.cachedIDs()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("第二次应命中缓存并返回同一份名单，得到 %#v", ids)
	}
	if s.count() != 1 {
		t.Fatalf("TTL 内不该重复拉目录，请求数 %d", s.count())
	}
}

func TestCachedIDsRefetchesAfterTTL(t *testing.T) {
	s := newStub(t, catalogHandler("a"))
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	if _, err := c.cachedIDs(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.catalogAt = time.Now().Add(-catalogTTL - time.Minute)
	c.mu.Unlock()

	ids, err := c.cachedIDs()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "a" || s.count() != 2 {
		t.Fatalf("TTL 过期后应重新拉取，得到 %#v / 请求数 %d", ids, s.count())
	}
}

func TestCachedIDsFallsBackToFloorDuringBackoff(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL,
		Floor: func() []string { return []string{"floor-a", "floor-b"} }})

	ids, err := c.cachedIDs()
	if err == nil {
		t.Fatal("目录失败必须带上错误，上层才知道这是降级名单")
	}
	if strings.Join(ids, ",") != "floor-a,floor-b" {
		t.Fatalf("拉不到目录时应给出地板名单，避免渠道整体消失，得到 %#v", ids)
	}
	// 退避窗口内不再重试，否则会把抖动的上游打得更死。
	if _, err := c.cachedIDs(); err == nil {
		t.Fatal("退避窗口内仍应视为降级")
	}
	if s.count() != 1 {
		t.Fatalf("退避窗口内不该再打上游，请求数 %d", s.count())
	}
}

func TestCachedIDsKeepsStaleCatalogOnRefreshError(t *testing.T) {
	var fail atomic.Bool
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		catalogHandler("a", "b")(w, nil)
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL, Floor: func() []string { return []string{"floor"} }})
	if _, err := c.cachedIDs(); err != nil {
		t.Fatal(err)
	}

	fail.Store(true)
	c.mu.Lock()
	c.catalogAt = time.Now().Add(-catalogTTL - time.Minute)
	c.mu.Unlock()

	ids, err := c.cachedIDs()
	if err == nil {
		t.Fatal("刷新失败要如实上报")
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("刷新失败应继续用上一次成功的目录，而不是退回地板名单，得到 %#v", ids)
	}
}

func TestAllowFiltersBothLiveAndFloorCatalog(t *testing.T) {
	// Allow 是唯一一道"哪些模型允许对外露出"的门，地板名单也必须过它。
	s := newStub(t, catalogHandler("keep", "drop"))
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL,
		Allow: func(id string) bool { return id == "keep" },
		Floor: func() []string { return []string{"keep", "drop", "unknown"} }})

	ids, err := c.cachedIDs()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "keep" {
		t.Fatalf("目录应按 Allow 过滤，得到 %#v", ids)
	}
	if got := c.floorIDs(); strings.Join(got, ",") != "keep" {
		t.Fatalf("地板名单也要按 Allow 过滤，得到 %#v", got)
	}
}

// ── 可见名单与健康度 ───────────────────────────────────────────────────

func TestVisibleCatalogDropsCoolingModels(t *testing.T) {
	reg := health.NewRegistry(health.DefaultPolicy(), nil)
	s := newStub(t, catalogHandler("a", "b"))
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL, Health: reg,
		Enabled: func() bool { return true }})

	if got := idsOf(c.visibleCatalog()); strings.Join(got, ",") != "a,b" {
		t.Fatalf("初始应全部可见，得到 %#v", got)
	}
	reg.MarkFailure("p", "a", health.ClassRate, "429")
	if got := idsOf(c.visibleCatalog()); strings.Join(got, ",") != "b" {
		t.Fatalf("冷却中的模型不该继续对外露出，得到 %#v", got)
	}
	// Supports 只回答"归不归本渠道"，绕不绕开冷却由 pkg/route 决定。
	if !c.Supports("A") {
		t.Fatal("Supports 不该看冷却，也不该区分大小写")
	}
}

// TestCatalogIDsKeepsCoolingModels 冷却过滤只该作用于对外可见名单：
// 看板要展示"谁在冷却"、探针要重新敲到它确认复活，两者都靠这份完整目录。
func TestCatalogIDsKeepsCoolingModels(t *testing.T) {
	reg := health.NewRegistry(health.DefaultPolicy(), nil)
	s := newStub(t, catalogHandler("a", "b"))
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL, Health: reg,
		Enabled: func() bool { return true }})
	reg.MarkFailure("p", "a", health.ClassAuth, "401 unauthorized")

	if got := strings.Join(c.ModelIDs(), ","); got != "b" {
		t.Fatalf("可见名单应剔除冷却中的模型，得到 %q", got)
	}
	if got := strings.Join(c.CatalogIDs(), ","); got != "a,b" {
		t.Fatalf("完整目录不该被健康度裁剪，得到 %q", got)
	}
	var nilClient *Client
	if got := nilClient.CatalogIDs(); got != nil {
		t.Fatalf("nil 客户端应给空名单，得到 %#v", got)
	}
}

func TestSupportsRequiresEnabled(t *testing.T) {
	on := false
	s := newStub(t, catalogHandler("a"))
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL, Enabled: func() bool { return on }})
	if c.Supports("a") {
		t.Fatal("渠道关闭时不该认领任何模型")
	}
	on = true
	if !c.Supports("a") || c.Supports("") || c.Supports("nope") {
		t.Fatal("开启后应只认领目录里的模型")
	}
}

func TestNilClientIsDisabled(t *testing.T) {
	var c *Client
	if c.Enabled() {
		t.Fatal("nil 客户端必须视为关闭")
	}
}

// TestMissingEnabledClosureMeansClosed 契约：漏配 Enabled 一律当关闭，
// 不能因为"忘了配"就替用户去敲真实上游。
func TestMissingEnabledClosureMeansClosed(t *testing.T) {
	s := newStub(t, catalogHandler("a"))
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	if c.Enabled() {
		t.Fatal("没给 Enabled 应视为关闭")
	}
	if c.Supports("a") {
		t.Fatal("关闭的渠道不该认领模型")
	}
}

// ── 对话请求与错误归类 ─────────────────────────────────────────────────

func TestChatRejectsNonOKUpstream(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		jsonBody(w, map[string]interface{}{"error": map[string]interface{}{"message": "too many requests"}})
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	_, err := c.Chat(map[string]interface{}{"model": "a"})
	if err == nil {
		t.Fatal("429 必须转成 error，让路由层换渠道")
	}
	status, class, detail := c.ClassifyError(err)
	if status != http.StatusTooManyRequests || class != health.ClassRate {
		t.Fatalf("应还原状态码并归类为限流，得到 %d/%v", status, class)
	}
	if !strings.Contains(detail, "too many requests") {
		t.Fatalf("detail 应带上游原文，得到 %q", detail)
	}
	if strings.Contains(detail, "p 渠道") {
		t.Fatalf("detail 是给健康表用的原文，不该混进包装文案: %q", detail)
	}
}

func TestChatRejectsOKResponseWithoutChoices(t *testing.T) {
	// 不少兼容端点出错时仍回 200 + {"error":...}；直接透传会让上层拿到空回答。
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonBody(w, map[string]interface{}{"error": "insufficient credit balance"})
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	_, err := c.Chat(map[string]interface{}{"model": "a"})
	if err == nil {
		t.Fatal("HTTP 200 的错误体必须转成 error")
	}
	if _, class, _ := c.ClassifyError(err); class != health.ClassNoCredit {
		t.Fatalf("200 里的错误也要按文本归类，得到 %v", class)
	}
}

func TestChatAcceptsNormalResponse(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("请求体应是合法 JSON: %v", err)
		}
		jsonBody(w, map[string]interface{}{"choices": []interface{}{
			map[string]interface{}{"message": map[string]interface{}{"content": "pong"}},
		}})
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	out, err := c.Chat(map[string]interface{}{"model": "a", "messages": []interface{}{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["choices"]; !ok {
		t.Fatalf("响应应原样返回，得到 %#v", out)
	}
}

func TestChatStreamDetectsErrorWrappedInOK(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`+"\n")
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	resp, err := c.ChatStream(map[string]interface{}{"model": "ghost"})
	if err == nil {
		resp.Body.Close()
		t.Fatal("200 但首行是错误体时必须转成 error，让上层还能换渠道")
	}
	if _, class, _ := c.ClassifyError(err); class != health.ClassNotFound {
		t.Fatalf("应归类为模型不存在，得到 %v", class)
	}
}

func TestChatStreamKeepsSSEPayloadIntact(t *testing.T) {
	const first = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n"
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first+"data: [DONE]\n\n")
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	resp, err := c.ChatStream(map[string]interface{}{"model": "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// 探首行判断"是不是 200 假错误"之后，首行必须原样回放，否则用户丢掉第一个 token。
	if string(raw) != first+"data: [DONE]\n\n" {
		t.Fatalf("SSE 内容被改动了: %q", string(raw))
	}
}

func TestChatStreamNonOKIsError(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"region blocked"}}`)
	})
	c := New(Config{Name: "p", DefaultBaseURL: s.baseURL})
	if _, err := c.ChatStream(map[string]interface{}{"model": "a"}); err == nil {
		t.Fatal("非 200 的流式响应应转成 error")
	} else if _, class, _ := c.ClassifyError(err); class != health.ClassAuth {
		t.Fatalf("403 应归类为鉴权失败，得到 %v", class)
	}
}

// ── 纯函数 ─────────────────────────────────────────────────────────────

func TestErrTextHandlesShapes(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{"boom", "boom"},
		{map[string]interface{}{"message": "m"}, "m"},
		{map[string]interface{}{"code": "c"}, "c"},
		{map[string]interface{}{"message": "", "type": "t"}, "t"},
		{nil, "<nil>"},
	}
	for _, tc := range cases {
		if got := ErrText(tc.in); tc.want != "" && got != tc.want {
			t.Fatalf("ErrText(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLooksLikeErrorOnlyForPureError(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`{"error":{"message":"x"}}`, true},
		{`{"error":"x","choices":[]}`, false},
		{`{"choices":[]}`, false},
		{`data: {"error":"x"}`, false},
		{`not json`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := LooksLikeError(tc.line); got != tc.want {
			t.Fatalf("LooksLikeError(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestUpstreamErrorNeedsChoices(t *testing.T) {
	if msg, bad := UpstreamError(http.StatusOK, map[string]interface{}{"choices": []interface{}{1}}); bad || msg != "" {
		t.Fatalf("正常响应不该判成错误: %q/%v", msg, bad)
	}
	if msg, bad := UpstreamError(http.StatusOK, map[string]interface{}{}); !bad || msg == "" {
		t.Fatalf("200 但缺 choices 要报错并给出原因，得到 %q/%v", msg, bad)
	}
	if msg, bad := UpstreamError(http.StatusTeapot, map[string]interface{}{}); !bad || !strings.Contains(fmt.Sprint(msg), "418") {
		t.Fatalf("非 200 且无 error 字段时应给出状态码，得到 %q/%v", msg, bad)
	}
}

func TestSnippetCollapsesWhitespaceAndTruncates(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := Snippet([]byte("  a\n\t b  " + long))
	if strings.Contains(got, "\n") {
		t.Fatalf("应把换行折成空格，得到 %q", got)
	}
	if len(got) != 203 || !strings.HasSuffix(got, "...") {
		t.Fatalf("超长内容应截到 200 字符加省略号，得到 %d 字符", len(got))
	}
}
