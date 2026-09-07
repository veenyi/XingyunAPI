package traework

import (
	"strings"
	"testing"
)

func TestPrepareBodyBasic(t *testing.T) {
	src := map[string]interface{}{
		"model": "traework/glm-5.2",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "你是助手"},
			map[string]interface{}{"role": "developer", "content": "dev 提示"},
			map[string]interface{}{"role": "user", "content": "你好"},
		},
		"stream": false,
	}
	out := prepareBody(src)
	if out["stream"] != true {
		t.Fatalf("stream 应强制为 true，got %v", out["stream"])
	}
	if out["function"] != function {
		t.Fatalf("function 应为 %s，got %v", function, out["function"])
	}
	if out["config_name"] != "glm-5.2" || out["model"] != "glm-5.2" {
		t.Fatalf("模型名应去前缀：config_name=%v model=%v", out["config_name"], out["model"])
	}
	msgs := out["messages"].([]interface{})
	if msgs[1].(map[string]interface{})["role"] != "system" {
		t.Fatalf("developer 应映射为 system")
	}
	if c, ok := msgs[2].(map[string]interface{})["content"].([]interface{}); !ok || c[0].(map[string]interface{})["text"] != "你好" {
		t.Fatalf("字符串 content 应转为 parts 数组: %v", msgs[2])
	}
}

func TestPrepareBodyToolCalls(t *testing.T) {
	src := map[string]interface{}{
		"model": "glm-5.2",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":       "call1",
						"type":     "function",
						"function": map[string]interface{}{"name": "get_weather", "arguments": "{}"},
					},
					map[string]interface{}{
						"id":   "call2",
						"type": "function",
					},
				},
			},
			map[string]interface{}{"role": "tool", "content": "晴"},
		},
		"tools": []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "get_weather",
					"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
				},
			},
		},
		"tool_choice": map[string]interface{}{"type": "auto"},
	}
	out := prepareBody(src)
	msgs := out["messages"].([]interface{})
	tcs := msgs[0].(map[string]interface{})["tool_calls"].([]interface{})
	if len(tcs) != 2 {
		t.Fatalf("tool_call 应保留（wild-work 原语义仅剔除有名空/无名空函数），got %d", len(tcs))
	}
	tc := tcs[0].(map[string]interface{})
	if _, has := tc["function"]; has {
		t.Fatalf("function 应改名 function_call")
	}
	if _, has := tc["function_call"]; !has {
		t.Fatalf("应保留 function_call")
	}
	tools := out["tools"].([]interface{})
	fn := tools[0].(map[string]interface{})["function"].(map[string]interface{})
	if _, ok := fn["parameters"].(string); !ok {
		t.Fatalf("parameters 应序列化为字符串: %T", fn["parameters"])
	}
	if out["tool_choice"] != "auto" {
		t.Fatalf("tool_choice 应归一为 auto，got %v", out["tool_choice"])
	}
}

func TestPrepareBodyToolChoiceNone(t *testing.T) {
	out := prepareBody(map[string]interface{}{
		"model":       "glm-5.2",
		"tool_choice": "none",
		"tools":       []interface{}{},
	})
	if _, has := out["tool_choice"]; has {
		t.Fatalf("none 应删除 tool_choice")
	}
	if _, has := out["tools"]; has {
		t.Fatalf("none 应删除 tools")
	}
}

func soloFeed(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	return b.String()
}

func TestAggregateSolo(t *testing.T) {
	feed := soloFeed(
		"event: metadata\ndata: {\"session_id\":\"s1\",\"model\":\"glm-5.2\"}",
		"event: output\ndata: {\"response\":\"你好\",\"reasoning_content\":\"思考A\"}",
		"event: output\ndata: {\"response\":\"！\"}",
		"event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}",
		"event: done\ndata: {\"finish_reason\":\"stop\"}",
	)
	out, err := aggregateSolo(strings.NewReader(feed))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	choice := out["choices"].([]interface{})[0].(map[string]interface{})
	msg := choice["message"].(map[string]interface{})
	if msg["content"] != "你好！" {
		t.Fatalf("content 聚合错误: %v", msg["content"])
	}
	if msg["reasoning_content"] != "思考A" {
		t.Fatalf("reasoning 聚合错误: %v", msg["reasoning_content"])
	}
	if out["usage"].(map[string]interface{})["total_tokens"] != float64(8) {
		t.Fatalf("usage 缺失: %v", out["usage"])
	}
}

func TestAggregateSoloStreamError(t *testing.T) {
	feed := soloFeed(
		"event: output\ndata: {\"response\":\"部分\"}",
		"event: error\ndata: {\"code\":1005,\"message\":\"plan limit\"}",
	)
	_, err := aggregateSolo(strings.NewReader(feed))
	if err == nil {
		t.Fatalf("event:error 应上抛")
	}
	if !strings.Contains(err.Error(), "1005") {
		t.Fatalf("错误应含 code: %v", err)
	}
}

func TestStreamSoloSSE(t *testing.T) {
	feed := soloFeed(
		"event: output\ndata: {\"response\":\"A\"}",
		"event: output\ndata: {\"response\":\"B\"}",
		"event: done\ndata: {\"finish_reason\":\"stop\"}",
	)
	rc := newSoloSSE(&nopRC{strings.NewReader(feed)}, "glm-5.2")
	defer rc.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	out := sb.String()
	if !strings.Contains(out, `"content":"A"`) || !strings.Contains(out, `"content":"B"`) {
		t.Fatalf("流缺 content 增量: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("流缺 [DONE]: %s", out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] 应恰好一次")
	}
}

func TestStreamSoloEmpty(t *testing.T) {
	rc := newSoloSSE(&nopRC{strings.NewReader("")}, "m")
	defer rc.Close()
	buf := make([]byte, 4096)
	var sb strings.Builder
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "[DONE]") {
		t.Fatalf("空流也应保证 [DONE]: %s", sb.String())
	}
}

type nopRC struct{ *strings.Reader }

func (n *nopRC) Close() error { return nil }
