package traework

// SOLO 自定义 SSE 解析 → OpenAI SSE（流式转换 + 非流式聚合）。
// 事件序列（wild-work SPEC §4.6 实测）：
//
//	event:metadata  data:{"model":"","session_id":"...","prompt_completion_id":0,...}
//	event:output    data:{"response":"<增量>","reasoning_content":"<思考增量>","tool_calls":...}
//	event:token_usage data:{"prompt_tokens":..,"completion_tokens":..,"total_tokens":..}
//	event:done      data:{"finish_reason":"stop"}
//	event:error     data:{"code":1005,"message":"..."}

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// soloError 上游 SSE 流内业务错误（event:error）。
type soloError struct {
	Code int64
	Msg  string
}

func (e *soloError) Error() string { return fmt.Sprintf("solo error code=%d msg=%s", e.Code, e.Msg) }

// soloEvent 单条归一化事件。
type soloEvent struct {
	Event        string
	Response     string
	Reasoning    string
	ToolCalls    json.RawMessage
	Usage        map[string]interface{}
	FinishReason string
	ErrCode      int64
	ErrMsg       string
}

// parseSoloLine 解析一条 event/data 对。
func parseSoloLine(eventName, dataLine string) (*soloEvent, error) {
	ev := &soloEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrMsg = v
		}
	}
	return ev, nil
}

// sseState 维护跨行 event/data 累积。
type sseState struct {
	event string
	data  strings.Builder
}

func (s *sseState) reset() { s.event = ""; s.data.Reset() }

// scanLine 处理一行，事件边界返回解析结果。
func scanLine(st *sseState, line string) *soloEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.reset()
			return nil
		}
		ev, err := parseSoloLine(st.event, st.data.String())
		st.reset()
		if err != nil {
			return nil
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, ":"):
		// 注释行忽略
	}
	return nil
}

// --- 非流式聚合 ---

// aggregateSolo 读完整 SOLO SSE 聚合为 OpenAI chat.completion。
func aggregateSolo(r io.Reader) (map[string]interface{}, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		content      strings.Builder
		reasoning    strings.Builder
		finishReason = "stop"
		usage        map[string]interface{}
		toolCalls    = map[int]map[string]interface{}{}
		toolOrder    []int
		upstreamErr  error
	)
	st := &sseState{}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				content.WriteString(ev.Response)
				reasoning.WriteString(ev.Reasoning)
				mergeToolCall(toolCalls, &toolOrder, ev.ToolCalls)
			case "token_usage":
				usage = ev.Usage
			case "done":
				if ev.FinishReason != "" {
					finishReason = ev.FinishReason
				}
			case "error":
				upstreamErr = &soloError{Code: ev.ErrCode, Msg: ev.ErrMsg}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if upstreamErr != nil {
		return nil, upstreamErr
	}
	message := map[string]interface{}{
		"role":    "assistant",
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]interface{}, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "",
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCall 合并 SOLO output.tool_calls（null/对象/数组）到按 index 累计表。
func mergeToolCall(toolCalls map[int]map[string]interface{}, toolOrder *[]int, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]interface{}
		if json.Unmarshal(raw, &one) != nil {
			return
		}
		arr = []map[string]interface{}{one}
	}
	for _, call := range arr {
		if call == nil {
			continue
		}
		idx := 0
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		}
		merged, seen := toolCalls[idx]
		if !seen {
			merged = map[string]interface{}{"index": idx}
			toolCalls[idx] = merged
			*toolOrder = append(*toolOrder, idx)
		}
		mergeToolCallDelta(merged, call)
	}
}

// mergeToolCallDelta 合并单个 tool_call 片段：id/type 直覆盖，arguments 拼接。
// SOLO 用 function_call 字段名（兼容 OpenAI 的 function）。
func mergeToolCallDelta(merged, delta map[string]interface{}) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]interface{})
	if df == nil {
		df, _ = delta["function_call"].(map[string]interface{})
	}
	if df == nil {
		return
	}
	delete(df, "namespace")
	delete(df, "partial_arguments")
	mf, _ := merged["function"].(map[string]interface{})
	if mf == nil {
		mf = map[string]interface{}{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// --- 流式转换 ---

// newSoloSSE 把 SOLO SSE 流转成标准 OpenAI SSE（data: {...}\n\n + [DONE]）。
func newSoloSSE(r io.ReadCloser, model string) io.ReadCloser {
	return &soloSSEReader{src: r, br: bufio.NewReaderSize(r, 64*1024), model: model}
}

type soloSSEReader struct {
	src     io.ReadCloser
	br      *bufio.Reader
	out     bytes.Buffer
	model   string
	st      sseState
	written bool
	done    bool
}

// writeChunk 追加一个 OpenAI SSE chunk。
func (r *soloSSEReader) writeChunk(delta map[string]interface{}, finish string) {
	chunk := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   r.model,
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"delta": delta,
			},
		},
	}
	choice := chunk["choices"].([]interface{})[0].(map[string]interface{})
	if finish != "" {
		choice["finish_reason"] = finish
	}
	raw, _ := json.Marshal(chunk)
	fmt.Fprintf(&r.out, "data: %s\n\n", raw)
}

func (r *soloSSEReader) writeDone() {
	r.out.WriteString("data: [DONE]\n\n")
}

// finish 流结束时兜底写收尾（done 缺失也保证 [DONE]）。
func (r *soloSSEReader) finish() {
	if r.done {
		return
	}
	r.done = true
	if !r.written {
		// 上游空流：给一个空 delta chunk，保证客户端拿到合法响应
		r.writeChunk(map[string]interface{}{}, "stop")
		r.written = true
	}
	r.writeDone()
}

func (r *soloSSEReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 && !r.done {
		line, err := r.br.ReadString('\n')
		if ev := scanLine(&r.st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				delta := map[string]interface{}{}
				if ev.Response != "" {
					delta["content"] = ev.Response
				}
				if ev.Reasoning != "" {
					delta["reasoning_content"] = ev.Reasoning
				}
				if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
					var tc []map[string]interface{}
					if json.Unmarshal(ev.ToolCalls, &tc) == nil {
						// SOLO tool_call 条目 function_call 字段 → OpenAI function；清理专属字段
						for _, call := range tc {
							if fc, ok := call["function_call"].(map[string]interface{}); ok {
								call["function"] = fc
								delete(call, "function_call")
							}
							if fn, ok := call["function"].(map[string]interface{}); ok {
								delete(fn, "namespace")
								delete(fn, "partial_arguments")
							}
						}
						delta["tool_calls"] = tc
					}
				}
				if len(delta) > 0 {
					r.writeChunk(delta, "")
					r.written = true
				}
			case "done":
				r.writeChunk(map[string]interface{}{}, ev.FinishReason)
				r.written = true
				r.done = true
				r.writeDone()
			case "error":
				// 流已开始，以 OpenAI chunk 形态注入错误描述，finish_reason=stop 收口
				r.writeChunk(map[string]interface{}{
					"content": fmt.Sprintf("traework 上游错误 code=%d msg=%s", ev.ErrCode, ev.ErrMsg),
				}, "stop")
				r.written = true
				r.done = true
				r.writeDone()
			}
		}
		if err != nil {
			if err != io.EOF {
				return 0, err
			}
			r.finish()
			break
		}
	}
	if r.out.Len() == 0 {
		return 0, io.EOF
	}
	n, _ := r.out.Read(p)
	return n, nil
}

func (r *soloSSEReader) Close() error { return r.src.Close() }
