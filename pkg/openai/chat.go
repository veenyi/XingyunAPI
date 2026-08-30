package openai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/common"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/route"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// plainRouter 用于没接入路由的构造方式（老测试、store 打不开）：
// 没有健康度记录、自动切换恒关闭，候选永远只有一个。
var plainRouter = route.New(nil, nil)

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
	client := s.getClient(r)
	home, bodyFor := s.planFor(client, r, &req)
	// 先按用户点名的模型登记，自动切换成功后再改成实际使用的模型。
	store.SetModel(r, home.Model)
	cands := s.plan(client, home)
	if req.Stream {
		s.handleStreamChat(w, r, cands, bodyFor)
	} else {
		s.handleNonStreamChat(w, r, cands, bodyFor)
	}
}

// planFor 决定用户点名的模型该派给谁。
// 免登录 / 自带 Key 渠道按原模型名走，不参与默认模型兜底：
// 把 hy3-free 静默换成付费模型，比直接返回错误更糟。
func (s *Server) planFor(client *joycode.Client, r *http.Request, req *ChatRequest) (route.Candidate, bodyFor) {
	bodyFor := func(c route.Candidate) map[string]interface{} {
		return TranslateRequestModel(req, c.Model)
	}
	for _, p := range s.extras() {
		if p.Supports(req.Model) {
			return route.Candidate{Upstream: p, Provider: p.Name(), Model: req.Model}, bodyFor
		}
	}
	model := ResolveModel(req.Model, store.GetAccountDefaultModel(r), s.systemDefault())
	return route.Candidate{Upstream: client, Provider: route.JoyCodeProvider, Model: model}, bodyFor
}

// plan 在自动切换开启时，按用户排好的顺序补齐候选。
func (s *Server) plan(client *joycode.Client, home route.Candidate) []route.Candidate {
	if s.Route == nil {
		return []route.Candidate{home}
	}
	sources := append([]route.Source{route.JoyCodeSource(client)}, route.KeylessSources(s.extras())...)
	return s.Route.Plan(home, sources)
}

func (s *Server) router() *route.Router {
	if s.Route == nil {
		return plainRouter
	}
	return s.Route
}

func (s *Server) systemDefault() string {
	if s.store == nil {
		return ""
	}
	return s.store.GetSetting("default_model")
}

// bodyFor 按候选生成上游请求体：消息内容不变，只换模型。
type bodyFor func(route.Candidate) map[string]interface{}

func (s *Server) handleNonStreamChat(w http.ResponseWriter, r *http.Request, cands []route.Candidate, build bodyFor) {
	resp, used, err := s.router().Chat(cands, build)
	if err != nil {
		slog.Error("chat non-stream upstream error", "model", wantedModel(cands), "error", err)
		msg := err.Error()
		code := 500
		if isTimeoutError(msg) {
			code = 504
			msg = "上游服务响应超时，请稍后重试。原始错误: " + msg
		}
		writeError(w, code, msg)
		return
	}
	store.SetModel(r, used.Model)
	if usage, ok := resp["usage"].(map[string]interface{}); ok {
		inTk, _ := usage["prompt_tokens"].(float64)
		outTk, _ := usage["completion_tokens"].(float64)
		store.SetTokenUsage(r, int(inTk), int(outTk))
	}
	writeJSON(w, 200, TranslateResponse(resp, used.Model))
}

func (s *Server) handleStreamChat(w http.ResponseWriter, r *http.Request, cands []route.Candidate, build bodyFor) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("streaming not supported by response writer")
		return
	}

	// 响应头只在"上游握手超过宽限期"或"拿到确定结果"时提交一次。
	// 宽限期内失败还能换渠道；一旦提交就只能把错误写进流里。
	var (
		committed bool
		hbStop    chan struct{}
		hbDone    chan struct{}
	)
	commit := func() {
		if committed {
			return
		}
		committed = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "close")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(200)
		// 上游可能先把整段回答攒完才开始吐字节（推理模型 TTFB 10–30s）。
		// 没有心跳时下游客户端（Claude Code、OpenAI SDK）会超时或显示"无响应"。
		// SSE 注释行按规范会被所有合规客户端忽略。
		hbStop, hbDone = startHeartbeat(w, flusher)
	}
	stopHeartbeat := func() {
		if hbStop != nil {
			close(hbStop)
			<-hbDone
		}
	}

	streamStart := time.Now()
	out := s.router().Open(cands, build, commit)
	commit()
	defer stopHeartbeat()

	if out.Err != nil {
		slog.Error("chat stream upstream error", "model", wantedModel(cands), "error", out.Err)
		msg := out.Err.Error()
		if isTimeoutError(msg) {
			msg = "上游服务响应超时，请稍后重试。原始错误: " + msg
		}
		fmt.Fprintf(w, "data: {\"error\":{\"message\":\"%s\"}}\n\n", msg)
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	defer out.Resp.Body.Close()
	store.SetModel(r, out.Candidate.Model)
	slog.Info("stream: connected to upstream", "provider", out.Candidate.Provider,
		"model", out.Candidate.Model, "ttfb_ms", time.Since(streamStart).Milliseconds())

	// 逐行管道转发上游 SSE —— 本身已是 OpenAI 兼容格式。
	// 用 bufio.Scanner（而不是裸 Read）确保每个 SSE 事件到达就立刻转发，
	// 不会把多个事件攒进同一次写入。同时从最后一个 chunk 里取用量供看板统计。
	scanner := bufio.NewScanner(out.Resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var inTk, outTk int
	sawDone := false
	var problem string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// 空行分隔 SSE 事件，原样转发。
			w.Write([]byte("\n"))
			flusher.Flush()
			continue
		}
		if strings.Contains(line, "[DONE]") {
			sawDone = true
		}
		if msg := health.StreamProblem(line); msg != "" && problem == "" {
			problem = msg
		}
		line = normalizeSSE(line)
		w.Write([]byte(line))
		w.Write([]byte("\n"))
		flusher.Flush()
		if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
			var chunk struct {
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) == nil && chunk.Usage != nil {
				inTk = chunk.Usage.PromptTokens
				outTk = chunk.Usage.CompletionTokens
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Error("chat stream read error", "model", out.Candidate.Model, "error", err)
	}
	if problem != "" {
		slog.Warn("stream: 上游在流内报错，该模型计入冷却", "provider", out.Candidate.Provider,
			"model", out.Candidate.Model, "error", problem)
		s.router().RecordFailure(out.Candidate, errors.New(problem))
	}
	// 有些上游响应会漏掉 [DONE]（提前关闭、某些错误分支），
	// 让 CherryStudio 这类客户端一直卡在"生成中"。始终补一个终止帧。
	if !sawDone {
		slog.Warn("stream ended without [DONE], sending terminator", "model", out.Candidate.Model)
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}
	if inTk > 0 || outTk > 0 {
		store.SetTokenUsage(r, inTk, outTk)
	}
}

func startHeartbeat(w http.ResponseWriter, flusher http.Flusher) (chan struct{}, chan struct{}) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()
	return stop, done
}

// wantedModel 用于错误日志：用户最初点名的模型就是首个候选。
func wantedModel(cands []route.Candidate) string {
	if len(cands) == 0 {
		return ""
	}
	return cands[0].Model
}

func isTimeoutError(msg string) bool {
	return common.IsTimeoutError(errors.New(msg))
}

// normalizeSSE 规范化上游 SSE 帧：去掉空 content、空 tool_calls，保留 OpenAI 规范字段。
// 借鉴 workbuddy2api normalizeFrame()，防止上游噪声导致客户端解析错误。
func normalizeSSE(line string) string {
	if !strings.HasPrefix(line, "data: ") {
		return line
	}
	data := strings.TrimPrefix(line, "data: ")
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return line
	}
	cleaned := false
	// Strip empty/null content in choices
	if choices, ok := obj["choices"].([]interface{}); ok {
		for _, ch := range choices {
			if chm, ok := ch.(map[string]interface{}); ok {
				if msg, ok := chm["message"].(map[string]interface{}); ok {
					if msg["content"] == nil || msg["content"] == "" {
						msg["content"] = ""
					}
				}
				// Strip null/empty tool_calls
				if tc, ok := chm["tool_calls"]; ok && tc != nil {
					if tca, ok := tc.([]interface{}); ok && len(tca) == 0 {
						delete(chm, "tool_calls")
						cleaned = true
					}
				}
			}
		}
	}
	// Strip extra fields not in OpenAI spec
	speckey := map[string]bool{"id": true, "object": true, "created": true, "model": true,
		"choices": true, "usage": true, "system_fingerprint": true}
	for k := range obj {
		if !speckey[k] {
			delete(obj, k)
			cleaned = true
		}
	}
	if cleaned {
		if b, err := json.Marshal(obj); err == nil {
			return "data: " + string(b)
		}
	}
	return line
}
