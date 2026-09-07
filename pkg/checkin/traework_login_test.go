package checkin

import "testing"

func TestTwExtractCode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://127.0.0.1:18561/authorize?code=abc123&state=x", "abc123"},
		{"https://www.trae.cn/login?next=http%3A%2F%2F127.0.0.1%2Fauthorize%3Fcode%3Dinner45", ""},
		{"127.0.0.1:18561/authorize?code=abc123", "abc123"},
		{"barecodeXYZ789", "barecodeXYZ789"},
		{"  http://127.0.0.1/authorize?code=sp  ", "sp"},
		{"", ""},
		{"no code here", ""},
	}
	for _, c := range cases {
		if got := twExtractCode(c.in); got != c.want {
			t.Errorf("twExtractCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
