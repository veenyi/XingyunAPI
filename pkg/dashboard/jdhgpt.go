package dashboard

import (
	"log/slog"
	"net/http"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/auth"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
)

// ── JD 扫码登录（jdhgpt 官方流程） API ────────────────────────────

// handleJdHptInit POST /api/jdhgpt-login/init — 创建扫码会话，返回登录 URL
func (h *Handler) handleJdHptInit(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessionID, loginURL, err := auth.JdHptInit()
	if err != nil {
		slog.Error("jdhgpt init", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":         true,
		"session_id": sessionID,
		"login_url":  loginURL,
	})
}

// handleJdHptStatus GET /api/jdhgpt-login/status?session=xxx
// confirmed 时自动验证并添加账号。
func (h *Handler) handleJdHptStatus(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "missing session parameter")
		return
	}

	s, ok := auth.JdHptStatus(sessionID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "expired"})
		return
	}

	if s.Status == "waiting" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "waiting"})
		return
	}
	if s.Status != "confirmed" || s.Result == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  s.Status,
			"message": s.Err,
		})
		return
	}

	// 验证 pt_key 并保存账号
	userID, nickname, err := h.validateAndSavePtKey(s.Result.PtKey)
	if err != nil {
		slog.Error("jdhgpt save account failed", "error", err)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "error",
			"ok":      false,
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "confirmed",
		"ok":       true,
		"user_id":  userID,
		"nickname": nickname,
		"erp":      s.Result.ERP,
	})
}

var _ = joycode.Models // keep import if unused in future edits
