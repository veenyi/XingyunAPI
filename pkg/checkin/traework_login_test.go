package checkin

import "testing"

func TestTwExtractCode(t *testing.T) {
	cases := []struct {
		in       string
		wantCode string
		wantHost string
	}{
		{"http://127.0.0.1:18561/authorize?code=abc123&state=x", "abc123", ""},
		{"https://www.trae.cn/login?next=http%3A%2F%2F127.0.0.1%2Fauthorize%3Fcode%3Dinner45", "", ""},
		{"127.0.0.1:18561/authorize?code=abc123", "abc123", ""},
		{"barecodeXYZ789", "barecodeXYZ789", ""},
		{"  http://127.0.0.1/authorize?code=sp  ", "sp", ""},
		{"", "", ""},
		{"no code here", "", ""},
		// 2026-09-07 实测回跳形态：authCodeInfo JSON + host
		{
			`http://127.0.0.1:51284/authorize?isRedirect=true&scope=solo&authCodeInfo=%7B%22AuthCode%22%3A%22P2c6gS_4YX3MjA2oCxA7wEeM6pt2F2oY92dsrNLUaVQ%22%2C%22ExpireAt%22%3A1788784971837%2C%22ExpireDuration%22%3A600000%7D&loginTraceID=3ca38547-fe57-4b68-b9ac-8846643bd279&host=https%3A%2F%2Fapi.trae.com`,
			"P2c6gS_4YX3MjA2oCxA7wEeM6pt2F2oY92dsrNLUaVQ", "https://api.trae.com",
		},
		{"http://127.0.0.1:51284/authorize?authCodeInfo=%7B%22AuthCode%22%3A%22plain%22%7D", "plain", ""},
	}
	for _, c := range cases {
		got, gotHost := twExtractCode(c.in)
		if got != c.wantCode {
			t.Errorf("twExtractCode(%q) code = %q, want %q", c.in, got, c.wantCode)
		}
		if gotHost != c.wantHost {
			t.Errorf("twExtractCode(%q) host = %q, want %q", c.in, gotHost, c.wantHost)
		}
	}
}
