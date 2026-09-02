// body.go 构造 Qoder 上游 agent 协议请求体。
package qoder

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// buildAgentBody 将 OpenAI 格式消息转为 Qoder agent 协议体。
// 三条不变量：developer→system、stream=true、tool_choice 不在此层处理。
func buildAgentBody(messages []map[string]any, modelKey string, tools []any, enableReasoning bool) ([]byte, error) {
	msgs := make([]map[string]any, len(messages))
	for i, m := range messages {
		cp := make(map[string]any, len(m)+1)
		for k, v := range m {
			cp[k] = v
		}
		if r, _ := cp["role"].(string); r == "developer" {
			cp["role"] = "system"
		}
		msgs[i] = cp
	}

	prompt := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i]["role"] == "user" {
			if c, ok := msgs[i]["content"].(string); ok && c != "" {
				prompt = c
				break
			}
		}
	}

	now := time.Now()
	newUUID := uuid4()
	base := map[string]any{
		"request_id":       newUUID,
		"chat_record_id":   newUUID,
		"request_set_id":   uuid4(),
		"session_id":       uuid4(),
		"stream":           true,
		"aliyun_user_type": "personal_professional_trial",
		"agent_id":         "agent_common",
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"image_urls":       nil,
		"session_type":     "qodercli",
		"model_config":     map[string]any{"key": modelKey, "is_reasoning": enableReasoning},
		"chat_context": map[string]any{
			"chatPrompt": "",
			"text":       map[string]any{"type": "text", "text": prompt},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": modelKey, "is_reasoning": enableReasoning},
				"originalContent": map[string]any{"type": "text", "text": prompt},
			},
			"features":  []any{},
			"imageUrls": nil,
		},
		"messages": msgs,
		"business": map[string]any{
			"id":       uuid4(),
			"begin_at": now.UnixMilli(),
			"name":     truncateRunes(prompt, 30),
		},
	}
	if len(tools) > 0 {
		base["tools"] = tools
	}
	return json.Marshal(base)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	var sb strings.Builder
	for i, r := range s {
		if i >= n {
			break
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
