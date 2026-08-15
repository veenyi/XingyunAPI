package dashboard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// ── Web 聊天代理（前端「聊天」页）：POST /api/chat（SSE） ─────────────

const (
	chatModeQA     = "qa"
	chatModeCoding = "coding"

	webSearchToolName = "util_web_search_online"
	maxToolRounds     = 3
)

// 问答模式 system prompt（参考 JoyCode 客户端问答代理）
const qaSystemPrompt = "你是一个AI编程助手，使用的是京东公司开发的JoyCoder模型，善于回答计算机与编程相关的问题。对于政治敏感问题、安全和隐私问题，你将拒绝回答。请尽可能使用中文回答。\n\n***You should answer user questions directly and concisely.***"

// 编程模式 system prompt（简化版）
const codingSystemPrompt = "你是一个AI编程助手，使用的是京东公司开发的JoyCoder模型。你擅长代码编写、调试、重构、项目分析等各种开发任务。请尽可能使用中文回答。"

type chatRequest struct {
	Messages  []chatMessage `json:"messages"`
	Model     string        `json:"model"`
	Mode      string        `json:"mode"`       // qa | coding
	WebSearch bool          `json:"web_search"` // 是否启用联网搜索
	UserID    string        `json:"user_id"`    // 指定账号（空则用默认账号）
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Images  []string `json:"images,omitempty"` // 图片 data URL 列表（多模态输入）
}

// defaultAccount 返回当前默认账号（聊天用账号）
func (h *Handler) defaultAccount() *store.AccountInfo {
	accounts, err := h.store.ListAccounts()
	if err != nil {
		return nil
	}
	for i := range accounts {
		if accounts[i].IsDefault {
			return &accounts[i]
		}
	}
	if len(accounts) > 0 {
		return &accounts[0]
	}
	return nil
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req chatRequest
	if !readJSONBody(w, r, &req) {
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages cannot be empty")
		return
	}
	if req.Mode == "" {
		req.Mode = chatModeQA
	}

	account := h.defaultAccount()
	var fullAccount *store.Account
	var err error
	if req.UserID != "" {
		if a, gerr := h.store.GetAccount(req.UserID); gerr == nil && a != nil {
			fullAccount = a
		} else {
			writeError(w, http.StatusBadRequest, "指定账号不存在")
			return
		}
	} else {
		if account == nil {
			writeError(w, http.StatusBadRequest, "暂无账号，请先添加 JoyCode 账号")
			return
		}
		fullAccount, err = h.store.GetAccount(account.UserID)
		if err != nil || fullAccount == nil {
			writeError(w, http.StatusBadRequest, "获取账号凭据失败")
			return
		}
	}

	model := req.Model
	if model == "" {
		model = fullAccount.DefaultModel
		if model == "" {
			model = "GLM-5.1"
		}
	}

	client := joycode.NewClient(fullAccount.PtKey, fullAccount.UserID)
	// 账号专属网关上下文（企业账号 tenant/loginType 与默认不同，写死会导致 401）
	if fullAccount.Tenant != "" || fullAccount.LoginType != "" || fullAccount.ColorBaseURL != "" || fullAccount.MasterBaseURL != "" || fullAccount.OrgFullName != "" {
		client.SetColorContext(fullAccount.ColorBaseURL, fullAccount.MasterBaseURL, fullAccount.Tenant, fullAccount.LoginType, fullAccount.OrgFullName)
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.WriteHeader(200)

	// 心跳，防止长时间思考时前端超时
	stopHB := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		defer close(hbDone)
		for {
			select {
			case <-stopHB:
				return
			case <-ticker.C:
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()

	sysPrompt := qaSystemPrompt
	if req.Mode == chatModeCoding {
		sysPrompt = codingSystemPrompt
	}

	h.chatLoop(w, flusher, client, sysPrompt, req.Messages, model, req.WebSearch, 0)

	close(stopHB)
	<-hbDone
}

// chatLoop 执行一轮上游请求；若模型请求联网搜索则回填工具结果并递归续跑。
func (h *Handler) chatLoop(
	w http.ResponseWriter, flusher http.Flusher,
	client *joycode.Client, sysPrompt string,
	history []chatMessage, model string, webSearch bool, depth int,
) {
	// 组装 messages：[system, ...history]
	messages := make([]map[string]interface{}, 0, len(history)+1)
	messages = append(messages, map[string]interface{}{"role": "system", "content": sysPrompt})
	for _, m := range history {
		if len(m.Images) > 0 {
			parts := []map[string]interface{}{}
			if m.Content != "" {
				parts = append(parts, map[string]interface{}{"type": "text", "text": m.Content})
			}
			for _, img := range m.Images {
				parts = append(parts, map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]string{"url": img},
				})
			}
			messages = append(messages, map[string]interface{}{"role": m.Role, "content": parts})
		} else {
			messages = append(messages, map[string]interface{}{"role": m.Role, "content": m.Content})
		}
	}

	body := map[string]interface{}{
		"model":          model,
		"max_tokens":     8192,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]interface{}{"include_usage": true},
		"temperature":    0.7,
		"sendSource":     "user",
		"thinking":       map[string]interface{}{"type": "disabled"},
	}
	if webSearch {
		body["tools"] = []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        webSearchToolName,
					"description": "Search the web for real-time information on a specific query. Use this tool when you need current, factual information.",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"query": map[string]interface{}{"type": "string", "description": "The search query"},
						},
						"required": []string{"query"},
					},
				},
			},
		}
		body["tool_choice"] = "auto"
	}

	resp, err := client.PostStreamWithActivation("/api/saas/openai/v1/chat/completions", body)
	if err != nil {
		slog.Error("chat loop upstream error", "model", model, "depth", depth, "error", err)
		writeChatEvent(w, flusher, map[string]interface{}{"type": "error", "message": err.Error()})
		writeChatEvent(w, flusher, map[string]interface{}{"type": "done"})
		return
	}
	defer resp.Body.Close()

	// 解析上游 SSE：转发 text/reasoning，收集 tool_calls
	var toolCalls []map[string]interface{}
	var lastAssistantContent string

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			// 上游偶发返回 200 + 纯 JSON 错误体（非 SSE 格式），如
			// {"error":{"code":"AI_GRAY_ACCESS_DENIED",...}}——必须透传提示，
			// 否则前端只收到 done 渲染成空白气泡。
			if joycode.IsErrorBody(line) {
				msg := line
				var e struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(line), &e) == nil && e.Error.Message != "" {
					msg = e.Error.Message
					if e.Error.Code != "" {
						msg = e.Error.Code + ": " + msg
					}
				}
				slog.Error("chat upstream error body", "model", model, "depth", depth, "error", msg)
				writeChatEvent(w, flusher, map[string]interface{}{"type": "error", "message": msg})
				writeChatEvent(w, flusher, map[string]interface{}{"type": "done"})
				return
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		// 上游返回错误（如 AI_GRAY_ACCESS_DENIED）→ 透传给前端，避免"没回应"
		if errObj, ok := chunk["error"].(map[string]interface{}); ok {
			msg, _ := errObj["message"].(string)
			if msg == "" {
				msg = "上游服务返回错误"
			}
			if code, _ := errObj["code"].(string); code != "" {
				msg = code + ": " + msg
			}
			slog.Error("chat upstream error response", "model", model, "depth", depth, "error", msg)
			writeChatEvent(w, flusher, map[string]interface{}{"type": "error", "message": msg})
			writeChatEvent(w, flusher, map[string]interface{}{"type": "done"})
			return
		}
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		if delta == nil {
			continue
		}
		if c, _ := delta["content"].(string); c != "" {
			lastAssistantContent += c
			writeChatEvent(w, flusher, map[string]interface{}{"type": "text", "content": c})
		}
		if rc, _ := delta["reasoning_content"].(string); rc != "" {
			writeChatEvent(w, flusher, map[string]interface{}{"type": "reasoning", "content": rc})
		}
		if tcs, ok := delta["tool_calls"].([]interface{}); ok {
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]interface{})
				fn, _ := tcm["function"].(map[string]interface{})
				args, _ := fn["arguments"].(string)
				idx := 0
				if i, ok := tcm["index"].(float64); ok {
					idx = int(i)
				}
				for len(toolCalls) <= idx {
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":        "",
						"name":      "",
						"arguments": "",
					})
				}
				if id, _ := tcm["id"].(string); id != "" {
					toolCalls[idx]["id"] = id
				}
				if n, _ := fn["name"].(string); n != "" {
					toolCalls[idx]["name"] = n
				}
				toolCalls[idx]["arguments"] = toolCalls[idx]["arguments"].(string) + args
			}
		}
	}

	// 是否有联网搜索工具调用
	hasSearch := false
	for _, tc := range toolCalls {
		if name, _ := tc["name"].(string); name == webSearchToolName {
			hasSearch = true
			break
		}
	}

	if hasSearch && webSearch && depth < maxToolRounds {
		// 追加 assistant 消息（含 tool_calls）
		assistantMsg := chatMessage{Role: "assistant", Content: lastAssistantContent}
		history = append(history, assistantMsg)

		// 执行每个搜索工具，把结果作为 tool 消息追加
		for _, tc := range toolCalls {
			name, _ := tc["name"].(string)
			if name != webSearchToolName {
				continue
			}
			toolCallID, _ := tc["id"].(string)
			query := ""
			if argsStr, ok := tc["arguments"].(string); ok {
				var args struct {
					Query string `json:"query"`
				}
				_ = json.Unmarshal([]byte(argsStr), &args)
				query = args.Query
			}
			if query == "" {
				continue
			}
			writeChatEvent(w, flusher, map[string]interface{}{"type": "tool_start", "tool": "web_search", "query": query})

			resultJSON := "[]"
			searchResults, serr := client.WebSearch(query)
			if serr != nil {
				slog.Error("web search failed", "query", query, "error", serr)
				resultJSON = fmt.Sprintf("{\"error\":%q}", serr.Error())
			} else if b, jerr := json.Marshal(searchResults); jerr == nil {
				resultJSON = string(b)
			}
			writeChatEvent(w, flusher, map[string]interface{}{"type": "tool_result", "tool": "web_search", "query": query})

			// tool 消息（携带 tool_call_id 与工具名）
			toolMsg := chatMessage{Role: "tool", Content: resultJSON}
			_ = toolCallID
			history = append(history, toolMsg)
		}

		// 递归续跑（最多 maxToolRounds 轮）
		h.chatLoop(w, flusher, client, sysPrompt, history, model, webSearch, depth+1)
		return
	}

	writeChatEvent(w, flusher, map[string]interface{}{"type": "done"})
}

// writeChatEvent 向前端输出一个 SSE 事件（自定义事件格式）
func writeChatEvent(w http.ResponseWriter, flusher http.Flusher, evt map[string]interface{}) {
	b, err := json.Marshal(evt)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	flusher.Flush()
}
