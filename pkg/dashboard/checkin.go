// 签到中心 API：账号 CRUD / 手动签到 / 配置。凭据一律以掩码回显。
package dashboard

import (
	"net/http"
	"strings"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/checkin"
)

// CheckinManager 由 serve.go 注入；为 nil 时签到接口返回 503。
var _ = strings.TrimSpace

func (h *Handler) registerCheckinRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/checkin/accounts", h.handleCheckinAccounts)
	mux.HandleFunc("/api/checkin/accounts/", h.handleCheckinAccountAction)
	mux.HandleFunc("/api/checkin/run", h.handleCheckinRun)
	mux.HandleFunc("/api/checkin/config", h.handleCheckinConfig)
}

func (h *Handler) checkin() *checkin.Manager { return h.CheckinManager }

// handleCheckinAccounts GET 列出 / PUT 保存签到账号。
func (h *Handler) handleCheckinAccounts(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m := h.checkin()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "签到功能未启用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"accounts": m.List(),
			"times":    m.Times(),
		})
	case http.MethodPut:
		var body struct {
			Accounts []checkin.AccountInput `json:"accounts"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if err := m.Save(body.Accounts); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleCheckinAccountAction DELETE /api/checkin/accounts/{id}。
func (h *Handler) handleCheckinAccountAction(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m := h.checkin()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "签到功能未启用")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/checkin/accounts/")
	if id == "" || r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := m.Remove(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleCheckinRun POST /api/checkin/run {id} 或 {all:true}。
func (h *Handler) handleCheckinRun(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m := h.checkin()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "签到功能未启用")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if !readJSONBody(w, r, &body) {
		return
	}
	if body.All {
		writeJSON(w, http.StatusOK, map[string]interface{}{"results": m.RunAll()})
		return
	}
	if body.ID == "" {
		writeError(w, http.StatusBadRequest, "缺少 id 或 all")
		return
	}
	res := m.RunOne(body.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": []checkin.Result{res}})
}

// handleCheckinConfig GET/PUT 每日签到时刻。
func (h *Handler) handleCheckinConfig(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m := h.checkin()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "签到功能未启用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"times": m.Times()})
	case http.MethodPut:
		var body struct {
			Times []string `json:"times"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if err := m.SetTimes(body.Times); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "times": m.Times()})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
