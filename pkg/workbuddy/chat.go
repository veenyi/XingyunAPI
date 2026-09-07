package workbuddy

// 聊天协议：请求头（chatHeaders/commonHeaders/refreshHeaders）、请求体
// 规范化（PrepareBody/normalizeToolCallChoice/normalizeModelName）、
// SSE 归一（Stream/mergeToolCallDelta）与令牌刷新（RefreshToken）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// headerUserAgent 未从二进制还原，取浏览器惯例值（见推断清单）。
const headerUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) CodeBuddy/1.0.0 Chrome/133.0.0.0 Safari/537.36"

// modelsUserAgent 模型目录接口的客户端 UA（workbuddy2api 权威值，活体探测验证）。
const modelsUserAgent = "CLI/2.63.2 CodeBuddy/2.63.2"

// commonHeaders 所有上游请求共用的基础头。
func commonHeaders() http.Header {
	return http.Header{
		"Content-Type":   {"application/json"},
		"Accept":         {"text/event-stream, application/json"},
		"User-Agent":     {headerUserAgent},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
	}
}

// chatHeaders 聊天/目录/计费请求头（Bearer 访问令牌 + 企业上下文）。
func chatHeaders(acct *WBAccount) http.Header {
	h := commonHeaders()
	if acct != nil {
		if acct.AccessToken != "" {
			h.Set("Authorization", "Bearer "+acct.AccessToken)
		}
		if acct.EnterpriseID != "" {
			h.Set("X-Enterprise-Id", acct.EnterpriseID)
		}
		if acct.Domain != "" {
			h.Set("X-Domain", acct.Domain)
		}
	}
	return h
}

// refreshHeaders 刷新请求头（以 refresh_token 鉴权）。
func refreshHeaders(acct *WBAccount) http.Header {
	h := commonHeaders()
	h.Set("Accept", "application/json")
	if acct != nil {
		if acct.RefreshToken != "" {
			h.Set("Authorization", "Bearer "+acct.RefreshToken)
			h.Set("X-Refresh-Token", acct.RefreshToken)
		}
		if acct.EnterpriseID != "" {
			h.Set("X-Enterprise-Id", acct.EnterpriseID)
		}
	}
	return h
}

// normalizeModelName 规范化模型名：去渠道前缀（"workbuddy/xxx"）、去空白、转小写。
func normalizeModelName(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(n))
}

// normalizeToolCallChoice 修复 tool_choice 形态：字符串函数名转为
// {"type":"function","function":{"name":...}}，nil 视为 "auto"。
func normalizeToolCallChoice(choice interface{}) interface{} {
	switch v := choice.(type) {
	case nil:
		return "auto"
	case string:
		s := strings.TrimSpace(v)
		if s == "" || s == "auto" || s == "none" || s == "required" {
			return s
		}
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": s},
		}
	default:
		return choice
	}
}

// PrepareBody 规范化上游聊天请求体：模型名归一、tool_choice 修复、
// 透传采样参数并强制 stream 位。
func PrepareBody(body map[string]interface{}, stream bool) map[string]interface{} {
	if body == nil {
		body = map[string]interface{}{}
	}
	out := map[string]interface{}{
		"model":    normalizeModelName(fmt.Sprint(body["model"])),
		"messages": body["messages"],
		"stream":   stream,
	}
	if tools, ok := body["tools"]; ok && tools != nil {
		out["tools"] = tools
	}
	if tc, ok := body["tool_choice"]; ok {
		out["tool_choice"] = normalizeToolCallChoice(tc)
	} else if _, has := body["tools"]; has {
		out["tool_choice"] = "auto"
	}
	for _, k := range []string{"temperature", "top_p", "max_tokens", "stop", "user"} {
		if v, ok := body[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// mergeToolCallDelta 把 tool_calls 增量按 index 合并进累计列表：
// id/type/function.name 首见生效，function.arguments 追加。
func mergeToolCallDelta(acc []map[string]interface{}, deltas []interface{}) []map[string]interface{} {
	for _, d := range deltas {
		tc, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		idx := int(anyF64(tc["index"]))
		if idx < 0 {
			idx = len(acc)
		}
		for len(acc) <= idx {
			acc = append(acc, map[string]interface{}{
				"index":    len(acc),
				"type":     "function",
				"function": map[string]interface{}{"name": "", "arguments": ""},
			})
		}
		slot := acc[idx]
		if id, _ := tc["id"].(string); id != "" {
			slot["id"] = id
		}
		if t, _ := tc["type"].(string); t != "" {
			slot["type"] = t
		}
		fn, _ := slot["function"].(map[string]interface{})
		if fn == nil {
			fn = map[string]interface{}{"name": "", "arguments": ""}
			slot["function"] = fn
		}
		if dfn, ok := tc["function"].(map[string]interface{}); ok {
			if n, _ := dfn["name"].(string); n != "" {
				if cur, _ := fn["name"].(string); cur == "" {
					fn["name"] = n
				}
			}
			if a, _ := dfn["arguments"].(string); a != "" {
				cur, _ := fn["arguments"].(string)
				fn["arguments"] = cur + a
			}
		}
	}
	return acc
}

// repairToolCallDeltas 修复单个 chunk 内的 tool_calls 增量：
// 缺 index 的按出现顺序补齐，形态异常的归一。
func repairToolCallDeltas(raw []interface{}) []interface{} {
	var acc []map[string]interface{}
	acc = mergeToolCallDelta(acc, raw)
	out := make([]interface{}, 0, len(acc))
	for _, m := range acc {
		out = append(out, m)
	}
	return out
}

// Stream 将上游 SSE 归一为标准 OpenAI chunk 流写入 w：
// 修复 tool_calls 增量形态、透传 [DONE]，检测 session-dead/错误标记。
// rc 由 Stream 关闭。
func Stream(ctx context.Context, rc io.ReadCloser, w io.Writer) error {
	if rc == nil {
		return nil
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			if strings.HasPrefix(line, ":") {
				if _, err := fmt.Fprintln(w, line); err != nil {
					return err
				}
			}
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
				return err
			}
			continue
		}
		normalized, err := normalizeChunk(payload)
		if err != nil {
			return err
		}
		if normalized == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", normalized); err != nil {
			return err
		}
	}
	return sc.Err()
}

// normalizeChunk 校验并修复单个 chunk 载荷；错误标记返回 error，空载荷返回 ""。
func normalizeChunk(payload string) (string, error) {
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		// 非 JSON 载荷包装成最小 delta，避免下游解析中断
		b, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]interface{}{"content": payload}},
			},
		})
		return string(b), nil
	}
	text := payload
	if hitsAny(text, sessionDeadMarkers) {
		return "", errSessionDead(truncateDetail(text))
	}
	if hitsAny(text, hardMarkers) {
		return "", fmt.Errorf("out of credit: %s", truncateDetail(text))
	}
	if e, ok := chunk["error"]; ok {
		return "", fmt.Errorf("upstream error: %v", e)
	}
	if choices, ok := chunk["choices"].([]interface{}); ok {
		for _, c := range choices {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			if delta, ok := cm["delta"].(map[string]interface{}); ok {
				if raw, ok := delta["tool_calls"].([]interface{}); ok {
					delta["tool_calls"] = repairToolCallDeltas(raw)
				}
			}
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return payload, nil
		}
		return string(b), nil
	}
	return payload, nil
}

// truncateDetail 截断错误细节。
func truncateDetail(s string) string {
	r := []rune(strings.TrimSpace(strings.ReplaceAll(s, "\n", " ")))
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return string(r)
}

// RefreshToken 用 refresh_token 换新访问令牌（copilot.tencent.com
// v2/plugin/auth/token/refresh，Keycloak 风格：体只有 refresh_token，
// 响应 data 为 camelCase accessToken/refreshToken）。
func RefreshToken(ctx context.Context, hc *http.Client, acct *WBAccount) error {
	if acct == nil || acct.RefreshToken == "" {
		return errors.New(errNoRefreshToken)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	body, err := json.Marshal(map[string]interface{}{
		"refresh_token": acct.RefreshToken,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", AuthBase+refreshPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = refreshHeaders(acct)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("刷新失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("刷新失败: HTTP %d: %s", resp.StatusCode, truncateDetail(string(raw)))
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("刷新失败: %w", err)
	}
	if parsed.Data.AccessToken == "" {
		return errors.New(errNoTokenInResp)
	}
	acct.AccessToken = parsed.Data.AccessToken
	if parsed.Data.RefreshToken != "" {
		acct.RefreshToken = parsed.Data.RefreshToken
	}
	acct.LastRefreshAt = nowRFC3339()
	acct.LastRefreshOK = true
	return nil
}
