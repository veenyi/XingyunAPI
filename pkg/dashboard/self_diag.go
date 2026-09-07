package dashboard

import (
	"net"
	"net/http"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/checkin"
	"github.com/veenyi/XingyunAPI/pkg/store"
)

// diagStart 用于 uptime 计算（进程级）。
var diagStart = time.Now()

var diagChannelKeys = []string{
	"keyfree_enabled", "keyed_enabled", "router9_enabled",
	"freellm_enabled", "opencode_enabled", "route_failover_enabled",
}

// diagAccount 是账号的精简视图：不含 api_token 等任何凭据字段。
type diagAccount struct {
	UserID              string `json:"user_id"`
	Display             string `json:"display"`
	IsDefault           bool   `json:"is_default"`
	DefaultModel        string `json:"default_model"`
	CredentialValid     int    `json:"credential_valid"`
	CredentialRefreshAt string `json:"credential_refreshed_at,omitempty"`
	CredentialError     string `json:"credential_error,omitempty"`
}

// handleSelfDiag: GET /api/self/diag —— 结构化自诊断快照，供常驻运维
// keeper 与兜底巡检拉取。JWT 白名单放行 + loopback 校验双保险：
// 仅本机进程可读，响应不含任何凭据字段。
func (h *Handler) handleSelfDiag(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !isLoopback(r) {
		writeError(w, http.StatusUnauthorized, "self/diag is restricted to localhost")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	accounts, err := h.store.ListAccounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	diagAccs := make([]diagAccount, 0, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		diagAccs = append(diagAccs, diagAccount{
			UserID:              a.UserID,
			Display:             a.DisplayName(),
			IsDefault:           a.IsDefault,
			DefaultModel:        a.DefaultModel,
			CredentialValid:     a.CredentialValid,
			CredentialRefreshAt: a.CredentialRefreshAt,
			CredentialError:     a.CredentialError,
		})
	}

	channels := map[string]string{}
	for _, key := range diagChannelKeys {
		channels[key] = h.store.GetSetting(key)
	}
	channels["keepalive_interval_minutes"] = h.store.GetSetting("keepalive_interval_minutes")

	checkinSnap := map[string]interface{}{"available": false}
	if h.checkin != nil {
		accs := h.checkin.List()
		if accs == nil {
			accs = []checkin.Account{}
		}
		checkinSnap = map[string]interface{}{
			"available": true,
			"times":     h.checkin.Times(),
			"accounts":  accs,
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version":        h.Version,
		"uptime_seconds": int64(time.Since(diagStart).Seconds()),
		"generated_at":   time.Now().Format("2006-01-02 15:04:05"),
		"accounts":       diagAccs,
		"keepalive":      h.keeperStatuses(),
		"channels":       channels,
		"checkin":        checkinSnap,
		"recent_errors":  h.recentErrors(),
		"stats":          h.statsSummary(),
	})
}

func (h *Handler) keeperStatuses() map[string]interface{} {
	if h.keeper == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{"statuses": h.keeper.GetAllStatuses()}
}

func (h *Handler) recentErrors() []store.RequestLog {
	logs, err := h.store.GetRecentErrors(20)
	if err != nil || logs == nil {
		return []store.RequestLog{}
	}
	return logs
}

func (h *Handler) statsSummary() map[string]interface{} {
	st, err := h.store.GetStats()
	if err != nil || st == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"total_requests": st.TotalRequests,
		"success_count":  st.SuccessCount,
		"error_count":    st.ErrorCount,
		"avg_latency_ms": st.AvgLatencyMs,
	}
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
