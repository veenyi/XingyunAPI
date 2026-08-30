// 渠道与模型排序接口：看板要能看见"每个渠道都有哪些模型、现在能不能用"，
// 并让用户把想要的顺序存回去，供 pkg/route 做自动切换。
package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/route"
)

// maxRankingEntries 防止一个超大 body 把 model_ranking 撑成无用的巨型字符串。
const maxRankingEntries = 500

// modelItem 是排序页里的一行：渠道 + 模型 + 当前健康度。
type modelItem struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Status   string `json:"status"`
	Class    string `json:"class,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Failures int    `json:"failures,omitempty"`
	// Until 是冷却结束时间；空表示当前可用。
	Until string `json:"until,omitempty"`
	// Ranked 表示这条已经在用户排序里（用于区分"未排序"和"排到最后"）。
	Ranked bool `json:"ranked"`
}

// handleModelStatus GET /api/model-status — 所有渠道的模型健康一览。
func (h *Handler) handleModelStatus(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": h.modelItems()})
}

// handleModelRanking GET/PUT /api/model-ranking — 自动切换时的候选顺序。
func (h *Handler) handleModelRanking(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"ranking": h.ranking()})
	case http.MethodPut:
		var body struct {
			Ranking []route.Rank `json:"ranking"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if len(body.Ranking) > maxRankingEntries {
			writeError(w, http.StatusBadRequest, "模型顺序条目过多")
			return
		}
		cleaned := make([]route.Rank, 0, len(body.Ranking))
		for _, item := range body.Ranking {
			p := strings.TrimSpace(item.Provider)
			m := strings.TrimSpace(item.Model)
			if p == "" || m == "" {
				continue
			}
			cleaned = append(cleaned, route.Rank{Provider: p, Model: m})
		}
		raw, err := json.Marshal(cleaned)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := h.store.SetSetting(route.SettingRanking, string(raw)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "count": len(cleaned)})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) ranking() []route.Rank {
	raw := strings.TrimSpace(h.store.GetSetting(route.SettingRanking))
	if raw == "" {
		return []route.Rank{}
	}
	var ranks []route.Rank
	if err := json.Unmarshal([]byte(raw), &ranks); err != nil {
		return []route.Rank{}
	}
	return ranks
}

// modelItems 列出当前所有渠道的模型，并按用户排序标记先后。
// 渠道名单来自路由的同一份数据源，看板看到的顺序就是实际派单顺序。
func (h *Handler) modelItems() []modelItem {
	sources := h.channels()
	states := map[string]health.State{}
	if h.Health != nil {
		states = h.Health.Snapshot()
	}
	ranked := map[string]int{}
	for i, r := range h.ranking() {
		ranked[health.Key(r.Provider, r.Model)] = i
	}

	var items []modelItem
	seen := map[string]bool{}
	for _, s := range sources {
		// 冷却中的模型恰恰是最需要展示状态的那一个，必须用不过滤健康度的完整名单。
		list := s.ModelsAll
		if list == nil {
			list = s.Models
		}
		if list == nil {
			continue
		}
		for _, m := range list() {
			key := health.Key(s.Name, m)
			if seen[key] {
				continue
			}
			seen[key] = true
			item := modelItem{Provider: s.Name, Model: m, Status: health.StatusOK}
			if st, ok := states[key]; ok {
				item.Status = st.Status
				item.Class = string(st.Class)
				item.Reason = st.Reason
				item.Failures = st.Failures
				if !st.Until.IsZero() && st.Status == health.StatusCooling {
					item.Until = st.Until.Format(time.RFC3339)
				}
			}
			_, item.Ranked = ranked[key]
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		ri, oki := ranked[health.Key(items[i].Provider, items[i].Model)]
		rj, okj := ranked[health.Key(items[j].Provider, items[j].Model)]
		if oki && okj {
			return ri < rj
		}
		if oki != okj {
			return oki
		}
		if items[i].Provider != items[j].Provider {
			return items[i].Provider < items[j].Provider
		}
		return items[i].Model < items[j].Model
	})
	return items
}

// channels 取当前参与路由的上游渠道名单；没接线时返回空而不是报错。
func (h *Handler) channels() []route.Source {
	if h.Channels == nil {
		return nil
	}
	return h.Channels()
}
