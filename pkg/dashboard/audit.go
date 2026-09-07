package dashboard

// 管理面操作审计：所有 /api/* 变更类请求（非 GET/OPTIONS/HEAD）落 JSONL
// 审计文件（logs/audit.log，超 10MB 截断保留后半），供人类与 keeper 追溯
// 「谁在什么时候动了什么」。敏感字段值统一打码。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const auditMaxSize = 10 << 20 // 超过即截断保留后半

var (
	auditMaskMu sync.Mutex
	// 命中 password/secret/token/*key*/access/refresh 等键名的字符串值打码。
	sensitiveValueRe = regexp.MustCompile(
		`(?i)"([^"]*(password|passwd|secret|token|api_key|apikey|pt_key|access|refresh)[^"]*)"\s*:\s*"([^"]*)"`)
)

type auditStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditStatusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// SetAuditFile sets the audit JSONL sink; empty path disables auditing.
func (h *Handler) SetAuditFile(path string) {
	h.auditMu.Lock()
	h.auditPath = path
	h.auditMu.Unlock()
}

// AuditMiddleware wraps next with management-operation auditing.
func (h *Handler) AuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodOptions ||
			r.Method == http.MethodHead || !strings.HasPrefix(r.URL.Path, "/api/") ||
			r.URL.Path == "/api/audit" {
			next.ServeHTTP(w, r)
			return
		}

		// 摘要：读一小段 body 再放回，不打断后续处理
		summary := ""
		if r.Body != nil {
			head, _ := io.ReadAll(io.LimitReader(r.Body, 32<<10))
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
			trimmed := strings.TrimSpace(string(head))
			if trimmed != "" {
				if r.URL.Path == "/api/chat" {
					// 聊天内容不入审计，只记规模
					var probe struct {
						Messages []json.RawMessage `json:"messages"`
					}
					if json.Unmarshal([]byte(trimmed), &probe) == nil {
						summary = "chat messages=" + strconv.Itoa(len(probe.Messages))
					} else {
						summary = "(binary)"
					}
				} else {
					summary = maskSensitive(trimmed)
					if len(summary) > 300 {
						summary = summary[:300] + "…"
					}
				}
			}
		}

		sw := &auditStatusWriter{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(sw, r)

		h.writeAudit(map[string]interface{}{
			"ts":     time.Now().Format("2006-01-02T15:04:05.000-07:00"),
			"method": r.Method,
			"path":   r.URL.Path,
			"status": sw.status,
			"remote": r.RemoteAddr,
			"ms":     time.Since(start).Milliseconds(),
			"body":   summary,
		})
	})
}

func (h *Handler) writeAudit(entry map[string]interface{}) {
	h.auditMu.Lock()
	path := h.auditPath
	h.auditMu.Unlock()
	if path == "" {
		return
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size() > auditMaxSize {
		if data, err := os.ReadFile(path); err == nil {
			keep := data
			if len(data) > auditMaxSize/2 {
				keep = data[len(data)-auditMaxSize/2:]
				if i := bytes.IndexByte(keep, '\n'); i >= 0 {
					keep = keep[i+1:]
				}
			}
			_ = os.WriteFile(path, keep, 0o600)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

// handleAudit GET /api/audit?limit=200 返回最近的审计记录。
func (h *Handler) handleAudit(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 || limit > 2000 {
		limit = 200
	}
	h.auditMu.Lock()
	path := h.auditPath
	h.auditMu.Unlock()
	empty := map[string]interface{}{"entries": []interface{}{}, "path": path}
	if path == "" {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	entries := make([]json.RawMessage, 0, len(lines))
	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if len(ln) > 0 && json.Valid(ln) {
			entries = append(entries, json.RawMessage(ln))
		}
	}
	raw, _ := json.Marshal(entries)
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": path, "entries": json.RawMessage(raw)})
}

func maskSensitive(s string) string {
	auditMaskMu.Lock()
	defer auditMaskMu.Unlock()
	return sensitiveValueRe.ReplaceAllString(s, `"$1":"***"`)
}
