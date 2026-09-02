// sse.go 解析 Qoder 嵌套 SSE 并转为 OpenAI 格式。
package qoder

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// parseNestedSSE 解析嵌套 SSE：每行 data:{"body":"<OpenAI chunk JSON>"},
// body=="[DONE]" 结束。
func parseNestedSSE(r io.Reader, onChunk func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			var env struct {
				Body string `json:"body"`
			}
			if json.Unmarshal([]byte(payload), &env) == nil && env.Body != "" {
				if env.Body == "[DONE]" {
					return nil
				}
				var chunk map[string]any
				if json.Unmarshal([]byte(env.Body), &chunk) == nil {
					if err := onChunk(chunk); err != nil {
						return err
					}
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// Aggregate 将上游 SSE 流聚合为单个 OpenAI chat.completion 对象。
func Aggregate(r io.Reader, model string) (map[string]any, error) {
	return aggregate(r, model)
}

func aggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id      string
		created int64
		content strings.Builder
		reason  strings.Builder
		usage   map[string]any
		tools   = map[int]map[string]any{}
		toolOrd []int
	)

	err := parseNestedSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && v != "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && v > 0 {
			created = int64(v)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}

		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			if cm == nil {
				continue
			}
			delta, _ := cm["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if txt, ok := delta["content"].(string); ok && txt != "" {
				content.WriteString(txt)
			}
			if txt, ok := delta["reasoning_content"].(string); ok && txt != "" {
				reason.WriteString(txt)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					if tcm == nil {
						continue
					}
					idx := 0
					if i, ok := tcm["index"].(float64); ok {
						idx = int(i)
					}
					existing, has := tools[idx]
					if !has {
						existing = map[string]any{
							"id":   "",
							"type": "function",
							"function": map[string]any{
								"name":      "",
								"arguments": "",
							},
						}
						tools[idx] = existing
						toolOrd = append(toolOrd, idx)
					}
					if v, ok := tcm["id"].(string); ok {
						existing["id"] = v
					}
					if v, ok := tcm["type"].(string); ok {
						existing["type"] = v
					}
					if fn, ok := tcm["function"].(map[string]any); ok {
						efn := existing["function"].(map[string]any)
						if v, ok := fn["name"].(string); ok {
							efn["name"] = v
						}
						if v, ok := fn["arguments"].(string); ok {
							efn["arguments"] = efn["arguments"].(string) + v
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = time.Now().Unix()
	}

	msg := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if reason.Len() > 0 {
		msg["reasoning_content"] = reason.String()
	}
	if len(tools) > 0 {
		sort.Ints(toolOrd)
		arr := make([]any, 0, len(toolOrd))
		for _, idx := range toolOrd {
			arr = append(arr, tools[idx])
		}
		msg["tool_calls"] = arr
	}

	result := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": msg,
				"finish_reason": "stop",
			},
		},
	}
	if usage != nil {
		result["usage"] = usage
	}
	return result, nil
}

// Stream 将上游 SSE 流转写为 OpenAI 格式 SSE 响应。
func Stream(w io.Writer, r io.Reader, model string) error {
	return parseNestedSSE(r, func(chunk map[string]any) error {
		chunk["model"] = model
		data, err := json.Marshal(chunk)
		if err != nil {
			return nil
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		return nil
	})
}
