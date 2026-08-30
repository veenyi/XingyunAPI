package health

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Classify 把一次上游失败归到冷却档位。status<=0 表示没拿到 HTTP 响应（连接层失败）。
// 文本判定必须同时看状态码，很多上游会把 429 包在 200 的 JSON 里。
func Classify(status int, body string) Class {
	switch {
	case status == http.StatusTooManyRequests:
		return ClassRate
	case status == http.StatusPaymentRequired, status == http.StatusInsufficientStorage:
		return ClassNoCredit
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ClassAuth
	case status == http.StatusNotFound:
		return ClassNotFound
	case status >= 500:
		return ClassServer
	case status > 0:
		// 4xx 里没归类的（400/422 等）多为请求本身的问题，不是渠道坏了
		if class := classifyText(body); class != ClassOK {
			return class
		}
		return ClassOK
	default:
		// 没有状态码（连接层失败）时也要先读文本，否则"限流"会被当成网络抖动只冷却 30s。
		if class := classifyText(body); class != ClassOK {
			return class
		}
		return ClassNetwork
	}
}

// ClassifyBody 用于"HTTP 200 但 body 是错误"的上游。
func ClassifyBody(body string) Class {
	return classifyText(body)
}

// StreamProblem 从一行 SSE 里挑出"上游在流内报错"的文本。不少兼容上游把
// 429/5xx 裹进已经建好的流里（HTTP 头还是 200），只看握手状态会漏判。
// 返回空串表示这一行不是错误帧。
func StreamProblem(line string) string {
	if strings.HasPrefix(line, "event:") {
		if strings.Contains(strings.ToLower(line), "error") {
			// 带上 event: error 字样，让 classifyText 把它归到上游故障而不是网络抖动。
			return "上游在流里返回错误: event: error"
		}
		return ""
	}
	if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
		return ""
	}
	var frame struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) != nil {
		return ""
	}
	msg := strings.TrimSpace(frame.Error.Message)
	if msg == "" {
		msg = strings.TrimSpace(frame.Error.Type)
	}
	if msg == "" && strings.EqualFold(strings.TrimSpace(frame.Type), "error") {
		msg = "上游在流里返回错误"
	}
	return msg
}

// classifyText 覆盖中英文上游的常见措辞。宁可归为 server_error 也不要判成 ok。
func classifyText(body string) Class {
	s := strings.ToLower(body)
	switch {
	case containsAny(s, "429", "rate limit", "rate_limit", "ratelimit", "too many requests",
		"qps", "并发超限", "请求过于频繁", "限流", "频率"):
		return ClassRate
	case containsAny(s, "insufficient", "no_credit", "credit balance", "balance is not sufficient",
		"余额不足", "额度不足", "配额用尽", "quota exceeded", "out of quota", "每日上限", "用尽"):
		return ClassNoCredit
	case containsAny(s, "401", "403", "unauthorized", "un-auth", "invalid api key", "invalid_api_key",
		"authentication", "apikey-error", "region", "地区", "区域不可用", "forbidden"):
		return ClassAuth
	case containsAny(s, "model not found", "not in the plan", "模型不在套餐", "不支持的模型",
		"unknown model", "no such model", "model_not_found"):
		return ClassNotFound
	case containsAny(s, "500", "502", "503", "504", "internal error", "bad gateway",
		"service unavailable", "upstream", "服务器繁忙", "服务不可用", "上游在流里返回错误"):
		return ClassServer
	}
	return ClassOK
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// ClassifyErr 给拿不到 HTTP 状态的失败用（连接重置、上下文取消等）。
func ClassifyErr(err error) Class {
	if err == nil {
		return ClassOK
	}
	if IsTimeout(err) {
		return ClassNetwork
	}
	return ClassifyBody(err.Error())
}

// IsTimeout 判定连接层超时/截止，沿用字符串兜底以覆盖包装过的错误。
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var t interface{ Timeout() bool }
	if errors.As(err, &t) {
		return t.Timeout()
	}
	s := err.Error()
	return strings.Contains(s, "timeout") || strings.Contains(s, "Timeout") ||
		strings.Contains(s, "deadline exceeded") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF")
}
