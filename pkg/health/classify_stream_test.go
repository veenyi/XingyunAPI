package health

import "testing"

func TestStreamProblemRecognizesInStreamErrors(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"普通增量帧", `data: {"choices":[{"delta":{"content":"hi"}}]}`, ""},
		{"结束帧", `data: [DONE]`, ""},
		{"心跳注释", `: keepalive`, ""},
		{"error 事件", `event: error`, "上游在流里返回错误: event: error"},
		{"带 message 的错误帧", `data: {"error":{"message":"Rate limit reached for gpt (429)"}}`, "Rate limit reached for gpt (429)"},
		{"只有 error.type", `data: {"error":{"type":"server_error"}}`, "server_error"},
		{"只有 type:error", `data: {"type":"error"}`, "上游在流里返回错误"},
		{"error 为空对象", `data: {"error":{},"choices":[]}`, ""},
		{"不是 JSON", `data: 上游炸了`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StreamProblem(c.line); got != c.want {
				t.Fatalf("得到 %q，期望 %q", got, c.want)
			}
		})
	}
}

func TestStreamProblemClassifiesIntoCooldownBuckets(t *testing.T) {
	cases := []struct {
		line string
		want Class
	}{
		{`data: {"error":{"message":"Rate limit exceeded, retry in 120 seconds"}}`, ClassRate},
		{`data: {"error":{"message":"insufficient quota"}}`, ClassNoCredit},
		{`data: {"error":{"message":"Upstream request failed: [502] service temporarily overloaded"}}`, ClassServer},
		{`event: error`, ClassServer},
		{`data: {"type":"error"}`, ClassServer},
	}
	for _, c := range cases {
		msg := StreamProblem(c.line)
		if msg == "" {
			t.Fatalf("%s 没被识别成错误帧", c.line)
		}
		if got := Classify(0, msg); got != c.want {
			t.Fatalf("%s 归类为 %s，期望 %s（文本 %q）", c.line, got, c.want, msg)
		}
	}
}
