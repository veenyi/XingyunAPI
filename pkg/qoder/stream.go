package qoder

// 流式处理：上游 SSE 可能是标准 OpenAI chunk，也可能是「嵌套 SSE」事件
// （data 载荷内再嵌一层 SSE 文本）。normalizeQoderStream 统一转成标准
// OpenAI SSE（`data: {...}\n\n` + `data: [DONE]`），passthroughWriter 负责
// 透传写入并在转发时观察内容（错误标记检测）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// passthroughWriter 把写入的字节原样转发到底层 io.Writer；
// onWrite 钩子用于在透传时观察内容（流内错误标记检测）。
type passthroughWriter struct {
	w       io.Writer
	onWrite func(p []byte)
}

// Write 实现 io.Writer。
func (p *passthroughWriter) Write(b []byte) (int, error) {
	if p.onWrite != nil {
		p.onWrite(b)
	}
	return p.w.Write(b)
}

// normalizeQoderStream 将上游响应体包装成标准 OpenAI SSE 流。
// 返回的 ReadCloser 关闭时会同时关闭上游 rc。
func normalizeQoderStream(rc io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		var streamErr error
		w := &passthroughWriter{w: pw, onWrite: func(p []byte) {
			if streamErr == nil && hitsAny(string(p), hardMarkers) {
				streamErr = fmt.Errorf("上游在流里返回错误: %s", truncateRunes(string(p), 200))
			}
		}}
		err := pumpStream(rc, w)
		_ = rc.Close()
		if err == nil {
			err = streamErr
		}
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// pumpStream 逐行读取上游 SSE 并写入规范化后的 SSE。
func pumpStream(rc io.ReadCloser, w io.Writer) error {
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(line[len("data:"):])
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
					return err
				}
				continue
			}
			// 嵌套 SSE：外层载荷里内嵌了一层 data: 行
			if inner := parseNestedSSE(payload); len(inner) > 0 {
				for _, p := range inner {
					if err := writeChunk(w, p); err != nil {
						return err
					}
				}
				continue
			}
			if err := writeChunk(w, payload); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// SSE 注释/心跳，透传保持连接语义
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		case line == "":
			// 空行由 writeChunk 自带 \n\n 生成，跳过
		default:
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// writeChunk 校验并写出单个 data 载荷。
func writeChunk(w io.Writer, payload string) error {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" || trimmed == "[DONE]" {
		return nil
	}
	var probe interface{}
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		// 非 JSON 载荷按纯文本包装成 delta，避免下游解析中断
		trimmed = deltaTextJSON(trimmed)
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", trimmed)
	return err
}

// deltaTextJSON 把纯文本包装成最小 OpenAI delta chunk。
func deltaTextJSON(text string) string {
	b, _ := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{"content": text}},
		},
	})
	return string(b)
}

// decodeChatResponse 读取非流式响应：JSON 直解；SSE 则聚合为 OpenAI 形状。
func decodeChatResponse(resp *http.Response, model string) (map[string]interface{}, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var out map[string]interface{}
		if err := json.Unmarshal(trimmed, &out); err != nil {
			return nil, fmt.Errorf("qoder: 响应解析失败: %w", err)
		}
		return out, nil
	}
	return aggregateSSE(string(raw), model), nil
}

// aggregateSSE 把上游 SSE（含嵌套 SSE）聚合为 OpenAI 形状的非流式响应。
func aggregateSSE(raw, model string) map[string]interface{} {
	var content strings.Builder
	var toolArgs strings.Builder
	var toolName string
	finish := ""
	usage := map[string]interface{}{}
	id := ""
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		chunks := []string{payload}
		if inner := parseNestedSSE(payload); len(inner) > 0 {
			chunks = inner
		}
		for _, c := range chunks {
			var chunk struct {
				ID      string `json:"id"`
				Choices []struct {
					Delta struct {
						Content   string                 `json:"content"`
						Reasoning string                 `json:"reasoning"`
						ToolCalls []map[string]interface{} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason interface{} `json:"finish_reason"`
				} `json:"choices"`
				Usage map[string]interface{} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(c), &chunk); err != nil {
				continue
			}
			if chunk.ID != "" && id == "" {
				id = chunk.ID
			}
			for _, ch := range chunk.Choices {
				content.WriteString(ch.Delta.Content)
				if ch.Delta.ToolCalls != nil {
					merged := mergeToolCallDelta(nil, toAnySlice(ch.Delta.ToolCalls))
					for _, tc := range merged {
						if fn, ok := tc["function"].(map[string]interface{}); ok {
							if n, _ := fn["name"].(string); n != "" {
								toolName = n
							}
							if a, _ := fn["arguments"].(string); a != "" {
								toolArgs.WriteString(a)
							}
						}
					}
				}
				if s, ok := ch.FinishReason.(string); ok && s != "" {
					finish = s
				}
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
		}
	}
	message := map[string]interface{}{"role": "assistant", "content": content.String()}
	if toolName != "" {
		message["tool_calls"] = []map[string]interface{}{{
			"id":   "call_" + hexShort(),
			"type": "function",
			"function": map[string]interface{}{
				"name":      toolName,
				"arguments": toolArgs.String(),
			},
		}}
		if finish == "" {
			finish = "tool_calls"
		}
	}
	choice := map[string]interface{}{"index": 0, "message": message, "finish_reason": finish}
	out := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"model":   model,
		"choices": []interface{}{choice},
	}
	if len(usage) > 0 {
		out["usage"] = usage
	}
	return out
}

// hitsAny 报告 s（不区分大小写）是否命中任一标记。
func hitsAny(s string, markers []string) bool {
	t := strings.ToLower(s)
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}
