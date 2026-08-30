package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/route"
)

func decodeInto(t *testing.T, w *httptest.ResponseRecorder, dst interface{}) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
}

func statusRows(t *testing.T, h *Handler) []modelItem {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleModelStatus(w, httptest.NewRequest("GET", "/api/model-status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Models []modelItem `json:"models"`
	}
	decodeInto(t, w, &payload)
	return payload.Models
}

func rowOf(rows []modelItem, provider, model string) (modelItem, bool) {
	for _, r := range rows {
		if r.Provider == provider && r.Model == model {
			return r, true
		}
	}
	return modelItem{}, false
}

// TestModelStatusKeepsCoolingModels 刚被限流的模型正是用户最想看的那一个：
// 派单用的可见名单已经把它剔除了，状态页必须改用完整名单，否则它整行消失。
func TestModelStatusKeepsCoolingModels(t *testing.T) {
	h, _ := setupTestHandler(t)
	reg := health.NewRegistry(health.DefaultPolicy(), nil)
	h.Health = reg
	h.Channels = func() []route.Source {
		return []route.Source{{
			Name:      "kf",
			Models:    func() []string { return []string{"alive"} },
			ModelsAll: func() []string { return []string{"alive", "cooling-one"} },
		}}
	}
	reg.MarkFailure("kf", "cooling-one", health.ClassRate, "429 too many requests")

	rows := statusRows(t, h)
	if len(rows) != 2 {
		t.Fatalf("完整名单里两个模型都该出现，得到 %+v", rows)
	}
	alive, ok := rowOf(rows, "kf", "alive")
	if !ok || alive.Status != health.StatusOK || alive.Until != "" {
		t.Fatalf("可用模型应标为 ok 且没有截止时间，得到 %+v", alive)
	}
	cooling, ok := rowOf(rows, "kf", "cooling-one")
	if !ok {
		t.Fatalf("冷却中的模型不见了，得到 %+v", rows)
	}
	if cooling.Status != health.StatusCooling || cooling.Class != string(health.ClassRate) {
		t.Fatalf("冷却状态与归类要对得上，得到 %+v", cooling)
	}
	if cooling.Until == "" || cooling.Failures != 1 {
		t.Fatalf("要给出冷却截止时间与失败次数，得到 %+v", cooling)
	}
}

// TestModelStatusFallsBackToVisibleList 没实现完整名单的渠道（退回可见名单）也不能漏行或 panic。
func TestModelStatusFallsBackToVisibleList(t *testing.T) {
	h, _ := setupTestHandler(t)
	h.Health = health.NewRegistry(health.DefaultPolicy(), nil)
	h.Channels = func() []route.Source {
		return []route.Source{
			{Name: "kf", Models: func() []string { return []string{"a"} }},
			{Name: "broken"},
		}
	}
	rows := statusRows(t, h)
	if len(rows) != 1 || rows[0].Model != "a" {
		t.Fatalf("应只列出有名单的渠道，得到 %+v", rows)
	}
}

func TestModelRankingRoundTrip(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	put := map[string]interface{}{"ranking": []route.Rank{
		{Provider: " opencode-free ", Model: " hy3-free "},
		{Provider: "", Model: "skip-me"},
		{Provider: "joycode", Model: ""},
		{Provider: "joycode", Model: "GLM-5.3"},
	}}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, makeRequest(t, "PUT", "/api/model-ranking", put))
	if w.Code != http.StatusOK {
		t.Fatalf("put status = %d, body=%s", w.Code, w.Body.String())
	}
	if got := decodeJSON(t, w)["count"]; got != float64(2) {
		t.Fatalf("空白项应被丢掉，count = %v", got)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/model-ranking", nil))
	var payload struct {
		Ranking []route.Rank `json:"ranking"`
	}
	decodeInto(t, w, &payload)
	if len(payload.Ranking) != 2 ||
		payload.Ranking[0].Provider != "opencode-free" || payload.Ranking[0].Model != "hy3-free" ||
		payload.Ranking[1].Model != "GLM-5.3" {
		t.Fatalf("读回的顺序应与保存的一致且已去空白，得到 %+v", payload.Ranking)
	}
}

func TestModelRankingRejectsOversizedList(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// 一个超大 body 就能把 model_ranking 撑成无用的巨型字符串，必须挡住。
	ranks := make([]route.Rank, maxRankingEntries+1)
	for i := range ranks {
		ranks[i] = route.Rank{Provider: "kf", Model: fmt.Sprintf("m%d", i)}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, makeRequest(t, "PUT", "/api/model-ranking", map[string]interface{}{"ranking": ranks}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超出上限的顺序应被拒绝，得到 %d", w.Code)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("PUT", "/api/model-ranking", nil))
	if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusBadRequest {
		t.Fatalf("非法请求应被拒绝，得到 %d", w.Code)
	}
}

// TestModelStatusShowsRankingFlags 状态页要能区分"没排过序"和"排到了最后"。
func TestModelStatusShowsRankingFlags(t *testing.T) {
	h, s := setupTestHandler(t)
	h.Health = health.NewRegistry(health.DefaultPolicy(), nil)
	h.Channels = func() []route.Source {
		return []route.Source{{Name: "kf", Models: func() []string { return []string{"a", "b"} }}}
	}
	if err := s.SetSetting(route.SettingRanking, `[{"provider":"kf","model":"b"}]`); err != nil {
		t.Fatal(err)
	}

	rows := statusRows(t, h)
	if len(rows) != 2 {
		t.Fatalf("应有两行，得到 %+v", rows)
	}
	// 排过序的排在前，且只有它带 ranked 标记。
	if rows[0].Model != "b" || !rows[0].Ranked || rows[1].Model != "a" || rows[1].Ranked {
		t.Fatalf("排序标记与先后不对，得到 %+v", rows)
	}
}
