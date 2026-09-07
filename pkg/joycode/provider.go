package joycode

// provider.Chat 适配层：route.Router 的 Primary 渠道与 dashboard 聊天页
// 都通过这组方法访问 JoyCode SaaS。Chat/ChatStream 走与 Post/PostStream
// 相同的 color gateway 路由，但接受 ctx 以支持取消/超时传播。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"

	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/provider"
)

var _ provider.Chat = (*ChatProvider)(nil)

const chatCompletionsEndpoint = "/api/saas/openai/v1/chat/completions"

func (c *Client) doPostCtx(ctx context.Context, headersFn func() http.Header, endpoint string, body map[string]interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.requestURL(endpoint), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header = headersFn()
	return c.httpClient.Do(req)
}

// Chat 发起一次非流式对话补全。
func (c *Client) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	resp, err := c.doPostCtx(ctx, c.headers, chatCompletionsEndpoint, c.prepareBody(body))
	if err != nil {
		return nil, err
	}
	data, err := decodeBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, truncate(string(data), 500))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid JSON response: %s", truncate(string(data), 500))
	}
	return result, nil
}

// ChatStream 发起流式对话补全，返回已解压的 SSE 响应体。
func (c *Client) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	resp, err := c.doPostCtx(ctx, c.headers, chatCompletionsEndpoint, c.prepareBody(body))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}
	if err := decodeStreamBody(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	if err := guardSSEHead(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// Name 渠道名（路由日志与故障提示里展示）。
func (c *Client) Name() string { return "joycode" }

// ClassifyError 把上游错误归类到 health 错误类别。
func (c *Client) ClassifyError(err error) string { return health.Classify(err) }

// ChatProvider 把 Client 适配成 provider.Chat（Client 自身的
// ListModels 返回 ([]ModelInfo, error)，与接口的 []string 签名不同）。
type ChatProvider struct{ *Client }

func (c *Client) AsProvider() *ChatProvider { return &ChatProvider{c} }

func (p *ChatProvider) ListModels() []string {
	names := make([]string, 0)
	if ms, err := p.Client.ListModels(); err == nil {
		for _, m := range ms {
			if m.Label != "" {
				names = append(names, m.Label)
			}
		}
	}
	return names
}

// GetPoint 查询账号剩余 IDE 积分。
func (c *Client) GetPoint() (map[string]interface{}, error) {
	return c.Post("/api/saas/point/v1/getNewIdePoint", map[string]interface{}{})
}

var keepaliveMessages = []string{"1", "hi", "ping", "ok", "在吗", "hello"}

func randomKeepaliveMessage() string {
	return keepaliveMessages[rand.Intn(len(keepaliveMessages))]
}

// SendKeepalive 发送一条超短对话，模拟客户端活跃防止上游冻结账号。
func (c *Client) SendKeepalive() error {
	body := map[string]interface{}{
		"model":      DefaultModel,
		"messages":   []map[string]string{{"role": "user", "content": randomKeepaliveMessage()}},
		"stream":     false,
		"max_tokens": 1,
	}
	_, err := c.Post(chatCompletionsEndpoint, body)
	return err
}
