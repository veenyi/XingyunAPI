package dashboard

// 聊天页后端：/api/chat（SSE 事件流）与 /api/chat-history。
// 事件协议（对齐前端契约 §7.8）：{type:'reasoning'|'text'|'tool_start'|
// 'tool_result'|'error', content?, tool?, query?, message?}。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/joycode"
	"github.com/veenyi/XingyunAPI/pkg/store"
)

// chatClientFor resolves the JoyCode client for the selected account
// (user_id 为空或不存在时回落到默认账号).
func (h *Handler) chatClientFor(userID string) (*joycode.Client, error) {
	var account *store.Account
	if userID != "" {
		if a, err := h.store.GetAccount(userID); err == nil && a != nil {
			account = a
		}
	}
	if account == nil {
		a, err := h.store.GetDefaultAccount()
		if err != nil {
			return nil, err
		}
		if a == nil {
			return nil, fmt.Errorf("暂无账号，请先添加 JoyCode 账号")
		}
		account = a
	}
	return joycode.NewClient(account.PtKey, account.UserID), nil
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		Messages  []map[string]interface{} `json:"messages"`
		Model     string                   `json:"model"`
		Mode      string                   `json:"mode"`
		WebSearch bool                     `json:"web_search"`
		UserID    string                   `json:"user_id"`
	}
	if !readJSONBody(w, r, &body) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var wmu sync.Mutex
	emit := func(v map[string]interface{}) {
		data, _ := json.Marshal(v)
		wmu.Lock()
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		wmu.Unlock()
	}
	fail := func(msg string) {
		slog.Warn("chat", "user_id", body.UserID, "error", msg)
		emit(map[string]interface{}{"type": "error", "message": msg})
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				wmu.Lock()
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
				wmu.Unlock()
			}
		}
	}()

	model := body.Model
	if model == "" {
		model = h.store.GetSetting("default_model")
	}
	if model == "" {
		model = joycode.DefaultModel
	}

	msgs := make([]interface{}, 0, len(body.Messages))
	for _, m := range body.Messages {
		msgs = append(msgs, m)
	}

	// 联网搜索：先跑一次搜索端点，把结果作为系统上下文注入本轮对话。
	// 搜索依赖 JoyCode 账号；无账号（纯渠道模型）时静默跳过。
	if body.WebSearch {
		if q := lastUserText(body.Messages); q != "" {
			emit(map[string]interface{}{"type": "tool_start", "tool": "web_search", "query": q})
			var results []interface{}
			if client, err := h.chatClientFor(body.UserID); err == nil {
				results, err = client.WebSearch(q)
				if err != nil {
					slog.Warn("chat web search failed", "error", err)
				}
			} else {
				slog.Warn("chat web search skipped: no joycode account", "error", err)
			}
			emit(map[string]interface{}{"type": "tool_result", "tool": "web_search", "query": q})
			if len(results) > 0 {
				if ctxText := searchContext(results); ctxText != "" {
					msgs = append(msgs, map[string]interface{}{
						"role":    "system",
						"content": "以下是与用户问题相关的联网搜索结果：\n" + ctxText,
					})
				}
			}
		}
	}

	chatBody := map[string]interface{}{
		"model":    model,
		"messages": msgs,
		"stream":   true,
	}
	if mt := h.store.GetIntSetting("default_max_tokens", 0); mt > 0 {
		chatBody["max_tokens"] = mt
	}

	// 渠道模型（非 JoyCode 内置模型，如 xingyun 本地行云AI）走路由分发到
	// keyless 渠道（custom 等）；返回的流与 JoyCode 同为 OpenAI chunk SSE，
	// 下面的解析逻辑两路通用。
	isJoyModel := false
	for _, m := range joycode.Models {
		if m == model {
			isJoyModel = true
			break
		}
	}
	var rc io.ReadCloser
	if isJoyModel || h.router == nil {
		// JoyCode 内置模型需要账号；渠道模型（如 xingyun）无账号也可用
		// 行云面板身份：裸模型自我介绍时要说清"底层模型身份 + 行云面板服务"，
		// 避免问"你是谁"时自称 JoyAI 本尊而只字不提行云（实测踩坑）。
		// 注意 msgs append 后必须重新赋值回 chatBody，否则扩容后注入丢失。
		msgs = append(msgs, map[string]interface{}{
			"role":    "system",
			"content": "本对话通过「行云」面板（用户自托管服务 xingyun-api）进行。当用户问你是谁：如实说明你的底层模型身份，并说明你正通过行云面板与用户对话；不要冒充行云或其他产品。一律用简体中文回答。",
		})
		chatBody["messages"] = msgs
		client, err := h.chatClientFor(body.UserID)
		if err != nil {
			fail(err.Error())
			return
		}
		rc, err = client.ChatStream(r.Context(), chatBody)
		if err != nil {
			fail("上游服务返回错误: " + err.Error())
			return
		}
	} else {
		var err error
		rc, _, err = h.router.ChatStream(r.Context(), chatBody)
		if err != nil {
			fail("渠道服务返回错误: " + err.Error())
			return
		}
	}
	defer rc.Close()

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			fail("上游在流里返回错误: " + chunk.Error.Message)
			return
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != "" {
				emit(map[string]interface{}{"type": "reasoning", "content": ch.Delta.ReasoningContent})
			}
			if ch.Delta.Content != "" {
				emit(map[string]interface{}{"type": "text", "content": ch.Delta.Content})
			}
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF && r.Context().Err() == nil {
		fail("上游在返回结束标记前断开了流式响应，本次回复可能不完整，请重试")
	}
}

func lastUserText(msgs []map[string]interface{}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			if strings.TrimSpace(c) != "" {
				return strings.TrimSpace(c)
			}
		case []interface{}:
			var parts []string
			for _, p := range c {
				if pm, ok := p.(map[string]interface{}); ok {
					if t, ok := pm["text"].(string); ok && t != "" {
						parts = append(parts, t)
					}
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
		}
	}
	return ""
}

func searchContext(results []interface{}) string {
	n := len(results)
	if n > 5 {
		n = 5
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		raw, err := json.Marshal(results[i])
		if err != nil {
			continue
		}
		text := string(raw)
		if len(text) > 1600 {
			text = text[:1600]
		}
		fmt.Fprintf(&b, "[%d] %s\n", i+1, text)
	}
	out := b.String()
	if len(out) > 8000 {
		out = out[:8000]
	}
	return out
}

// handleChatHistory GET ?user_id= 读取 / POST {messages, user_id} 全量保存
// （前端每轮只回传最近 200 条）。
func (h *Handler) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		uid := r.URL.Query().Get("user_id")
		if uid == "" {
			uid = "root"
		}
		data, err := h.store.GetChatHistory(uid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var msgs []interface{}
		if data != "" {
			if err := json.Unmarshal([]byte(data), &msgs); err != nil {
				slog.Warn("chat history decode failed", "user_id", uid, "error", err)
			}
		}
		if msgs == nil {
			msgs = []interface{}{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"messages": msgs})
	case http.MethodPost:
		var body struct {
			Messages []interface{} `json:"messages"`
			UserID   string        `json:"user_id"`
		}
		if !readJSONBody(w, r, &body) {
			return
		}
		uid := body.UserID
		if uid == "" {
			uid = "root"
		}
		raw, _ := json.Marshal(body.Messages)
		if err := h.store.SaveChatHistory(uid, string(raw)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
