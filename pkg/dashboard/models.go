package dashboard

// 模型与渠道页的后端：/api/model-status、/api/model-ranking、
// /api/model-blocklist、/api/channel-presets、/api/channels/refresh、
// /api/rotate-aggregate-key。

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/joycode"
)

type modelItem struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Ranked   bool   `json:"ranked"`
	Cooling  bool   `json:"cooling"`
	Class    string `json:"class,omitempty"`
	Label    string `json:"class_label,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Until    string `json:"until,omitempty"`
}

// handleModelStatus 枚举所有渠道的全部模型行，叠加冷却状态与排序标记。
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

	cooling := map[string]health.State{}
	if h.router != nil && h.router.Health != nil {
		for _, st := range h.router.Health.Snapshot() {
			cooling[st.Provider+"|"+st.Model] = st
		}
	}

	items := []modelItem{}
	seen := map[string]bool{}
	add := func(providerName, model string) {
		if providerName == "" || model == "" {
			return
		}
		key := providerName + "|" + model
		if seen[key] {
			return
		}
		seen[key] = true
		it := modelItem{Provider: providerName, Model: model}
		if h.router != nil {
			it.Ranked = h.router.RankingOr(providerName, model) >= 0
		}
		if st, ok := cooling[key]; ok && st.Status == "cooling" {
			it.Cooling = true
			it.Class = st.Class
			it.Label = health.ClassLabels[st.Class]
			it.Reason = st.Reason
			it.Until = st.Until
		}
		items = append(items, it)
	}

	if h.router != nil {
		if p := h.router.Primary; p != nil {
			for _, m := range p.ListModels() {
				add(p.Name(), m)
			}
		} else {
			// Primary 每请求生成，聚合时可能为 nil：直接用 JoyCode 静态模型表
			for _, m := range joycode.Models {
				add("joycode", m)
			}
		}
		for _, p := range h.router.KeylessChannels() {
			if !p.Enabled() {
				continue
			}
			for _, m := range p.ListModels() {
				add(p.Name(), m)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"models": items})
}

// handleModelRanking GET 返回当前排序，PUT 全量替换（[] 表示恢复默认）。
func (h *Handler) handleModelRanking(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ranking": h.routerRanking(),
		})
	case http.MethodPut:
		var body struct {
			Ranking []routeRank `json:"ranking"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		raw, _ := json.Marshal(body.Ranking)
		if err := h.store.SetSetting("model_ranking", string(raw)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type routeRank struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (h *Handler) routerRanking() []routeRank {
	if h.router == nil {
		return nil
	}
	rk := h.router.Ranking()
	out := make([]routeRank, 0, len(rk))
	for _, x := range rk {
		out = append(out, routeRank{Provider: x.Provider, Model: x.Model})
	}
	return out
}

// handleModelBlocklist 屏蔽名单：GET 返回列表；PUT 接受 {blocked:[...]}
// 全量替换或 {key, hidden} 增删单条（前端「隐藏」按钮）。
func (h *Handler) handleModelBlocklist(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"blocked": h.blocklist()})
	case http.MethodPut:
		var body struct {
			Blocked []string `json:"blocked"`
			Key     string   `json:"key"`
			Hidden  *bool    `json:"hidden"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		list := h.blocklist()
		if body.Hidden != nil && body.Key != "" {
			found := false
			for _, b := range list {
				if b == body.Key {
					found = true
					break
				}
			}
			if *body.Hidden && !found {
				list = append(list, body.Key)
			}
			if !*body.Hidden && found {
				out := list[:0]
				for _, b := range list {
					if b != body.Key {
						out = append(out, b)
					}
				}
				list = out
			}
		} else {
			list = body.Blocked
		}
		raw, _ := json.Marshal(list)
		if err := h.store.SetSetting("model_blocklist", string(raw)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) blocklist() []string {
	raw := h.store.GetSetting("model_blocklist")
	if raw == "" {
		return []string{}
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return []string{}
	}
	return list
}

// handleChannelPresets 返回 keyed 预设渠道注册表（快速添加下拉）。
func (h *Handler) handleChannelPresets(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.channelPresets == nil {
		h.channelPresets = []map[string]string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"presets": h.channelPresets})
}

// handleChannelsRefresh 强制重拉各渠道模型目录。
func (h *Handler) handleChannelsRefresh(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.refreshChannels != nil {
		h.refreshChannels()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleRotateAggregateKey 重新生成聚合 Key（旧 Key 立即失效）。
func (h *Handler) handleRotateAggregateKey(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	key := "sk-joy-aggregate-" + generateRandomHex(12)
	if err := h.store.SetSecretSetting("aggregate_key", key); err != nil {
		slog.Error("rotate aggregate key failed", "error", err)
		writeError(w, http.StatusInternalServerError, "生成密钥失败")
		return
	}
	slog.Info("aggregate key rotated")
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "key": key})
}
