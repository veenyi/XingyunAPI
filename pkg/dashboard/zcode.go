package dashboard

// ZCode 登录卡片后端：把 zcode-api（custom 渠道 zcode，NAS 本机 8080）的
// admin 登录端点代理给面板。凭据始终留在代理侧，行云只转发状态与流程。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type zcodeUpstream struct {
	Base string
	Key  string
}

// zcodeUpstream 从 custom_providers 设置里取 zcode 渠道的 base_url 与 api_key
// （即 zcode-api 的 proxyApiKey）。
func (h *Handler) zcodeUpstream() (zcodeUpstream, error) {
	if h.store == nil {
		return zcodeUpstream{}, fmt.Errorf("存储不可用")
	}
	raw := h.store.GetSetting("custom_providers")
	if strings.TrimSpace(raw) == "" {
		return zcodeUpstream{}, fmt.Errorf("未配置 custom 渠道")
	}
	var provs []struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.Unmarshal([]byte(raw), &provs); err != nil {
		return zcodeUpstream{}, fmt.Errorf("custom 渠道配置解析失败: %w", err)
	}
	for _, p := range provs {
		if p.Name == "zcode" {
			if strings.TrimSpace(p.BaseURL) == "" || strings.TrimSpace(p.APIKey) == "" {
				return zcodeUpstream{}, fmt.Errorf("zcode 渠道缺少 base_url 或 api_key")
			}
			return zcodeUpstream{Base: strings.TrimRight(p.BaseURL, "/"), Key: p.APIKey}, nil
		}
	}
	return zcodeUpstream{}, fmt.Errorf("未找到名为 zcode 的 custom 渠道")
}

func (z zcodeUpstream) call(method, path string, body any) (map[string]interface{}, int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, z.Base+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+z.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cli := &http.Client{Timeout: 30 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	var out map[string]interface{}
	if json.Unmarshal(data, &out) != nil {
		out = map[string]interface{}{"raw": string(data)}
	}
	return out, resp.StatusCode, nil
}

func (h *Handler) zcodeProxy(w http.ResponseWriter, r *http.Request, path string, body any) {
	up, err := h.zcodeUpstream()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	out, code, err := up.call(r.Method, path, body)
	if err != nil {
		slog.Warn("zcode login proxy failed", "path", path, "error", err)
		writeError(w, http.StatusBadGateway, "zcode-api 不可达: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(out)
	w.Write(b)
}

// handleZcodeLoginStatus GET /api/zcode/login/status
func (h *Handler) handleZcodeLoginStatus(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.zcodeProxy(w, r, "/admin/login/status", nil)
}

// handleZcodeLoginInit POST /api/zcode/login/init
func (h *Handler) handleZcodeLoginInit(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	h.zcodeProxy(w, r, "/admin/login/init", map[string]interface{}{})
}

// handleZcodeLoginComplete POST /api/zcode/login/complete — body {flowId, callbackUrl}
func (h *Handler) handleZcodeLoginComplete(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad body")
		return
	}
	h.zcodeProxy(w, r, "/admin/login/complete", body)
	h.writeAudit(map[string]interface{}{
		"ts": time.Now().Format("2006-01-02T15:04:05.000-07:00"), "method": r.Method,
		"path": r.URL.Path, "action": "zcode_login_complete", "remote": r.RemoteAddr,
	})
}

// handleZcodeLoginLogout POST /api/zcode/login/logout
func (h *Handler) handleZcodeLoginLogout(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	h.zcodeProxy(w, r, "/admin/login/logout", map[string]interface{}{})
	h.writeAudit(map[string]interface{}{
		"ts": time.Now().Format("2006-01-02T15:04:05.000-07:00"), "method": r.Method,
		"path": r.URL.Path, "action": "zcode_login_logout", "remote": r.RemoteAddr,
	})
}
