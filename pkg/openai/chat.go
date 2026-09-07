package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/veenyi/XingyunAPI/pkg/joycode"
	"github.com/veenyi/XingyunAPI/pkg/store"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Error("decode chat request", "error", err)
		writeError(w, 400, fmt.Sprintf("请求体解析失败: %s。请检查请求是否完整，或尝试开启新对话减少上下文长度。", err.Error()))
		return
	}
	systemDefault := ""
	if s.store != nil {
		systemDefault = s.store.GetSetting("default_model")
	}
	model := ResolveModel(req.Model, store.GetAccountDefaultModel(r), systemDefault)
		store.SetModel(r, model)
		jcBody := TranslateRequest(&req)
	client := s.getClient(r)
	if req.Stream {
		s.handleStreamChat(w, r, client, jcBody, model)
	} else {
		s.handleNonStreamChat(w, r, client, jcBody, model)
	}
}

func (s *Server) handleNonStreamChat(w http.ResponseWriter, r *http.Request, client *joycode.Client, jcBody map[string]interface{}, model string) {
	var (
		resp map[string]interface{}
		err  error
	)
	if rt := s.routerFor(r); rt != nil {
		resp, err = rt.Chat(r.Context(), jcBody)
	} else {
		resp, err = client.Post("/api/saas/openai/v1/chat/completions", jcBody)
	}
	if err != nil {
		slog.Error("chat non-stream upstream error", "model", model, "error", err)
		msg := err.Error()
		code := 500
		if isTimeoutError(msg) {
			code = 504
			msg = "上游服务响应超时，请稍后重试。原始错误: " + msg
		}
		writeError(w, code, msg)
		return
	}
	if usage, ok := resp["usage"].(map[string]interface{}); ok {
		inTk, _ := usage["prompt_tokens"].(float64)
		outTk, _ := usage["completion_tokens"].(float64)
		store.SetTokenUsage(r, int(inTk), int(outTk))
	}
	writeJSON(w, 200, TranslateResponse(resp, model))
}

func (s *Server) handleStreamChat(w http.ResponseWriter, r *http.Request, client *joycode.Client, jcBody map[string]interface{}, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("streaming not supported by response writer")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(200)

	var (
		body io.ReadCloser
		err  error
	)
	if rt := s.routerFor(r); rt != nil {
		body, _, err = rt.ChatStream(r.Context(), jcBody)
	} else {
		var resp *http.Response
		resp, err = client.PostStream("/api/saas/openai/v1/chat/completions", jcBody)
		if err == nil {
			body = resp.Body
		}
	}
	if err != nil {
		slog.Error("chat stream upstream error", "model", model, "error", err)
		msg := err.Error()
		if isTimeoutError(msg) {
			msg = "上游服务响应超时，请稍后重试。原始错误: " + msg
		}
		fmt.Fprintf(w, "data: {\"error\":{\"message\":\"%s\"}}\n\n", msg)
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	defer body.Close()

	// Pipe the upstream SSE response directly — already OpenAI-compatible format.
	// First-chunk guard: an upstream 200 with a JSON (non-SSE) body would be
	// parsed by clients as a stream with zero events; re-emit it as SSE.
	buf := make([]byte, 4096)
	n, _ := body.Read(buf)
	if n > 0 {
		if t := bytes.TrimLeft(buf[:n], " \t\r\n"); len(t) > 0 && t[0] == '{' {
			rest, _ := io.ReadAll(io.LimitReader(body, 1<<20))
			payload := append(append([]byte{}, t...), rest...)
			slog.Error("chat stream: 200 响应非 SSE，转 SSE 事件输出", "model", model, "body", snippetOf(payload, 300))
			if json.Valid(payload) {
				fmt.Fprintf(w, "data: %s\n\n", payload)
			} else {
				emsg, _ := json.Marshal(map[string]interface{}{
					"error": map[string]string{"message": snippetOf(payload, 300), "type": "upstream_error"},
				})
				fmt.Fprintf(w, "data: %s\n\n", emsg)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		w.Write(buf[:n])
		flusher.Flush()
	}
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			flusher.Flush()
		}
		if readErr != nil {
				if readErr.Error() != "EOF" {
					slog.Error("chat stream read error", "model", model, "error", readErr)
				}
			break
		}
	}
}

func snippetOf(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

func isTimeoutError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "context deadline exceeded") ||
		strings.Contains(lower, "client.timeout exceeded") ||
		strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "i/o timeout")
}
