package joycode

import "testing"

func TestIsActivationError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"AI_GRAY_ACCESS_DENIED: account not in beta", true},
		{"ai_gray_access_denied 账号不在内测范围内", true},
		{"账号不在内测范围, 请先使用官方客户端激活", true},
		{"gray access denied", true},
		{"权限不足: 灰度未开放", true},
		{"account not activated", true},
		{"access denied", true},
		{"API error 500: internal server error", false},
		{"context length exceeded", false},
		{"network timeout", false},
	}
	for _, c := range cases {
		if got := IsActivationError(c.msg); got != c.want {
			t.Errorf("IsActivationError(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestTryActivate_Guard(t *testing.T) {
	// 空 PtKey 直接短路，不应触发网络请求
	c := &Client{}
	if c.TryActivate() {
		t.Error("TryActivate with empty PtKey should return false")
	}
}

func TestIsErrorBody(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`{"error":{"code":"AI_GRAY_ACCESS_DENIED","message":"访问受限"}}`, true},
		{`{"code":429,"msg":"rate limited"}`, true},
		{`{"status":"error","msg":"boom"}`, true},
		{`data: {"choices":[{"delta":{"content":"hi"}}]}`, false},
		{`data: [DONE]`, false},
		{`not json at all`, false},
	}
	for _, c := range cases {
		if got := IsErrorBody(c.line); got != c.want {
			t.Errorf("IsErrorBody(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}
