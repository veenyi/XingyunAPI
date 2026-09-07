package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/veenyi/XingyunAPI/pkg/checkin"
)

// SetCheckinDeps 注入签到管理器与账号池同步回调（qoder/wb 登录成功后触发）。
func (h *Handler) SetCheckinDeps(cm *checkin.Manager, syncQoder, syncWB func()) {
	h.checkin = cm
	h.syncQoder = syncQoder
	h.syncWB = syncWB
}

func (h *Handler) registerCheckinRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/checkin/accounts", h.handleCheckinAccounts)
	mux.HandleFunc("/api/checkin/accounts/", h.handleCheckinAccountAction)
	mux.HandleFunc("/api/checkin/config", h.handleCheckinConfig)
	mux.HandleFunc("/api/checkin/points", h.handleCheckinPoints)
	mux.HandleFunc("/api/checkin/run", h.handleCheckinRun)
	mux.HandleFunc("/api/checkin/qoder_login/init", h.handleQoderLoginInit)
	mux.HandleFunc("/api/checkin/qoder_login/status", h.handleQoderLoginStatus)
	mux.HandleFunc("/api/checkin/wb_login/init", h.handleWBLoginInit)
	mux.HandleFunc("/api/checkin/wb_login/status", h.handleWBLoginStatus)
	mux.HandleFunc("/api/checkin/trae_login/init", h.handleTraeLoginInit)
	mux.HandleFunc("/api/checkin/trae_login/status", h.handleTraeLoginStatus)
	mux.HandleFunc("/api/checkin/trae_login/callback", h.handleTraeLoginCallback)
}

func (h *Handler) checkinReady(w http.ResponseWriter) bool {
	if h.checkin == nil {
		writeError(w, http.StatusNotFound, "签到功能未启用")
		return false
	}
	return true
}

// handleCheckinAccounts: GET → {accounts, times}；PUT {accounts: [...]} 全量替换。
func (h *Handler) handleCheckinAccounts(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"accounts": h.checkin.List(),
			"times":    h.checkin.Times(),
		})
	case http.MethodPut:
		var body struct {
			Accounts []checkin.AccountInput `json:"accounts"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if err := h.checkin.Save(body.Accounts); err != nil {
			slog.Warn("checkin: 解析账号失败", "error", err)
			writeError(w, http.StatusBadRequest, "checkin: 解析账号失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleCheckinAccountAction: DELETE /api/checkin/accounts/{id}。
func (h *Handler) handleCheckinAccountAction(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/checkin/accounts/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少账号 id")
		return
	}
	if plain, err := url.PathUnescape(id); err == nil {
		id = plain
	}
	if err := h.checkin.Remove(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleCheckinConfig: GET → {times}；PUT {times: ['HH:mm', …]} → {times}。
func (h *Handler) handleCheckinConfig(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"times": h.checkin.Times()})
	case http.MethodPut:
		var body struct {
			Times []string `json:"times"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		if err := h.checkin.SetTimes(body.Times); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"times": h.checkin.Times()})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleCheckinPoints: GET → {points: [{id, name, platform, credits, total}]}。
func (h *Handler) handleCheckinPoints(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"points": h.checkin.ListPoints(r.Context())})
}

// handleCheckinRun: POST {all: true} 全部签到，或 {id} 单账号（qoder 平台=刷新积分）。
// 契约响应 {results: [{id, name, ok, message, credits?}]}；message 含「已签到」前端按 info 展示。
func (h *Handler) handleCheckinRun(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		All bool   `json:"all"`
		ID  string `json:"id"`
	}
	if !readJSONBody(w, r, &body) {
		return
	}
	if body.All {
		writeJSON(w, http.StatusOK, map[string]interface{}{"results": h.checkin.RunAll(r.Context())})
		return
	}
	if body.ID == "" {
		writeError(w, http.StatusBadRequest, "缺少账号 id")
		return
	}
	res, err := h.checkin.RunOne(r.Context(), body.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": []checkin.Result{*res}})
}

func (h *Handler) handleQoderLoginInit(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	res, err := h.checkin.QoderLoginStart(r.Context())
	if err != nil {
		slog.Error("checkin: Qoder 设备流登录会话创建失败", "error", err)
		writeError(w, http.StatusInternalServerError, "发起 Qoder 登录失败: "+err.Error())
		return
	}
	slog.Info("checkin: Qoder 设备流登录会话已创建")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": res.SessionID,
		"auth_url":   res.AuthURL,
	})
}

func (h *Handler) handleWBLoginInit(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	res, err := h.checkin.WBLoginStart(r.Context())
	if err != nil {
		slog.Error("checkin: WorkBuddy 扫码登录会话创建失败", "error", err)
		writeError(w, http.StatusInternalServerError, "发起 WorkBuddy 登录失败: "+err.Error())
		return
	}
	slog.Info("checkin: WorkBuddy 扫码登录会话已创建")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": res.SessionID,
		"auth_url":   res.AuthURL,
	})
}

func (h *Handler) handleWBLoginStatus(w http.ResponseWriter, r *http.Request) {
	h.handleLoginStatus(w, r, loginKindWB)
}

func (h *Handler) handleQoderLoginStatus(w http.ResponseWriter, r *http.Request) {
	h.handleLoginStatus(w, r, loginKindQoder)
}

func (h *Handler) handleTraeLoginStatus(w http.ResponseWriter, r *http.Request) {
	h.handleLoginStatus(w, r, loginKindTrae)
}

// handleTraeLoginInit 发起 TraeWork 浏览器 PKCE 登录：
// 回调地址按本次请求的 Host 构造（保证浏览器可达），返回 {session_id, auth_url}。
func (h *Handler) handleTraeLoginInit(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	res, err := h.checkin.TraeLoginStart(r.Context())
	if err != nil {
		slog.Error("checkin: TraeWork 浏览器登录会话创建失败", "error", err)
		writeError(w, http.StatusInternalServerError, "发起 TraeWork 登录失败: "+err.Error())
		return
	}
	slog.Info("checkin: TraeWork 浏览器登录会话已创建")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": res.SessionID,
		"auth_url":   res.AuthURL,
	})
}

// handleTraeLoginCallback 是白名单路径：GET 为浏览器 302 落点（code 在 query）；
// POST 为面板粘贴提交（body JSON {url}，回环回调打不开时用户复制地址栏回传）。
func (h *Handler) handleTraeLoginCallback(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q := r.URL.Query()
	session := q.Get("session")
	code := q.Get("code")
	if r.Method == http.MethodPost {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "请求体无效")
			return
		}
		if !h.checkinReady(w) {
			return
		}
		if err := h.checkin.TraeLoginSubmit(session, body.URL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if session == "" || code == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(traeCallbackPage("登录回调参数不完整", "请回到面板重新发起 TraeWork 登录。")))
		return
	}
	if err := h.checkin.TraeLoginCallback(session, code); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(traeCallbackPage("登录回调处理失败", err.Error())))
		return
	}
	w.Write([]byte(traeCallbackPage("TRAE 登录成功", "授权已完成，请回到面板查看账号（若未立即出现请稍候片刻）。")))
}

// traeCallbackPage 是回调提示页（无外部资源，直接内联样式）。
func traeCallbackPage(title, detail string) string {
	return `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>` + title + `</title><style>` +
		`body{font-family:system-ui,-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;` +
		`display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f5f6f8;color:#1f2329}` +
		`.card{background:#fff;border-radius:12px;padding:40px 48px;max-width:420px;text-align:center;` +
		`box-shadow:0 4px 16px rgba(0,0,0,.08)}` +
		`h1{font-size:20px;margin:0 0 12px}p{font-size:14px;color:#646a73;margin:0;line-height:1.6}` +
		`</style></head><body><div class="card"><h1>` + title + `</h1><p>` + detail + `</p>` +
		`<p style="margin-top:16px;color:#8f959e;font-size:12px">本页面由行云 API 生成，可安全关闭</p>` +
		`</div></body></html>`
}

type loginKind int

const (
	loginKindQoder loginKind = iota
	loginKindWB
	loginKindTrae
)

// handleLoginStatus: GET ?session= 轮询登录状态；契约 {status, message?, account?:{nickname, uid}}。
func (h *Handler) handleLoginStatus(w http.ResponseWriter, r *http.Request, kind loginKind) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !h.checkinReady(w) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "缺少 session 参数")
		return
	}
	var status, message, nickname, uid string
	if kind == loginKindQoder {
		qr, err := h.checkin.QoderLoginPoll(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		status, message, nickname, uid = qr.Status, qr.Message, qr.Nickname, qr.UID
	} else if kind == loginKindTrae {
		tr, err := h.checkin.TraeLoginPoll(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		status, message, nickname, uid = tr.Status, tr.Message, tr.Nickname, tr.UID
	} else {
		wr, err := h.checkin.WBLoginPoll(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		status, message, nickname, uid = wr.Status, wr.Message, wr.Nickname, wr.UID
	}
	if status == "ok" {
		if kind == loginKindQoder {
			slog.Info("checkin: Qoder 设备流登录成功", "uid", uid)
			if h.syncQoder != nil {
				h.syncQoder()
			}
		} else if kind == loginKindWB {
			slog.Info("checkin: WorkBuddy 扫码登录成功", "uid", uid)
			if h.syncWB != nil {
				h.syncWB()
			}
		}
	}
	resp := map[string]interface{}{
		"status":  status,
		"message": message,
	}
	if status == "ok" {
		resp["account"] = map[string]string{"nickname": nickname, "uid": uid}
	}
	writeJSON(w, http.StatusOK, resp)
}
