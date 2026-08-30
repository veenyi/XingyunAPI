package joycode

import (
	"net/http"
	"regexp"
	"strconv"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
)

// provider.Chat 适配：把 JoyCode 的端点字面量与"失败即自动激活重试"语义收在这里，
// handler 侧只需面对 Chat/ChatStream 两个动作。
// 这里不 import pkg/provider（provider 已依赖本包），接口靠结构匹配。
const chatEndpoint = "/api/saas/openai/v1/chat/completions"

func (c *Client) Name() string { return "joycode" }

func (c *Client) Chat(body map[string]interface{}) (map[string]interface{}, error) {
	return c.PostWithActivation(chatEndpoint, body)
}

func (c *Client) ChatStream(body map[string]interface{}) (*http.Response, error) {
	return c.PostStreamWithActivation(chatEndpoint, body)
}

// apiErrorPattern 匹配 Post* 生成的 "API error 429: {...}" 文案。
// 状态码丢在字符串里，路由层要选对冷却档位就得自己捞回来。
var apiErrorPattern = regexp.MustCompile(`API error (\d{3})`)

func (c *Client) ClassifyError(err error) (int, health.Class, string) {
	if err == nil {
		return 0, health.ClassOK, ""
	}
	msg := err.Error()
	status := 0
	if m := apiErrorPattern.FindStringSubmatch(msg); m != nil {
		status, _ = strconv.Atoi(m[1])
	}
	return status, health.Classify(status, msg), msg
}
