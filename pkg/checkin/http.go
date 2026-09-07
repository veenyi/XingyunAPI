package checkin

// HTTP 与宽容解析：doJSON（统一 JSON 请求）、wbEnvelope（上游包裹层）、
// 片段截断与任意值→float64 转换。上游真实响应体拿不到，全部按候选字段
// 宽容解析（credits/credits_total/remaining/checked_in/data 包裹层等）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// doJSON 发起 JSON 请求并解码响应：
//   - in 非 nil 时序列化为请求体（自动补 Content-Type: application/json）；
//   - out 非 nil 时解码响应体（空体容忍）；
//   - 非 2xx 返回携带状态码与原始片段的错误（供上层文案拼接）。
func doJSON(ctx context.Context, hc *http.Client, method, url string, headers http.Header, in, out interface{}) error {
	if hc == nil {
		hc = checkinHTTP
	}
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s: %w", textCheckinFailed, err)
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if in != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: HTTP %d: %s", textCheckinFailed, resp.StatusCode, snippet(string(raw)))
	}
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: %w", errCheckinParse, err)
	}
	return nil
}

// wbEnvelope 是 codebuddy/copilot 上游的宽容包裹层：
// code 可能是数字或字符串，业务数据在 data（可能缺省）。
// Raw 保存完整顶层字段（data 缺省时兜底，见 dataOrSelf）。
type wbEnvelope struct {
	Code    interface{}            `json:"code"`
	Msg     string                 `json:"msg"`
	Message string                 `json:"message"`
	Error   interface{}            `json:"error"`
	Status  string                 `json:"status"`
	Success interface{}            `json:"success"`
	Data    map[string]interface{} `json:"data"`

	Raw map[string]interface{} `json:"-"`
}

// doEnvelope 发起 JSON 请求并把响应同时解码为 wbEnvelope 与原始 map
// （上游业务字段可能在顶层平铺，data 缺省时用 Raw 兜底）。
func doEnvelope(ctx context.Context, hc *http.Client, method, url string, headers http.Header, in interface{}) (*wbEnvelope, error) {
	if hc == nil {
		hc = checkinHTTP
	}
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", textCheckinFailed, err)
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if in != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textCheckinFailed, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s: HTTP %d: %s", textCheckinFailed, resp.StatusCode, snippet(string(raw)))
	}
	env := &wbEnvelope{Raw: map[string]interface{}{}}
	if len(bytes.TrimSpace(raw)) == 0 {
		return env, nil
	}
	if err := json.Unmarshal(raw, env); err != nil {
		return nil, fmt.Errorf("%s: %w", errCheckinParse, err)
	}
	_ = json.Unmarshal(raw, &env.Raw)
	return env, nil
}

// okCode 报告包裹层是否表示成功（0/200/"0"/"200"/"success"/true）。
func (e *wbEnvelope) okCode() bool {
	switch v := e.Code.(type) {
	case nil:
	case float64:
		if v != 0 && v != 200 {
			return false
		}
	case string:
		if v != "" && v != "0" && v != "200" && !strings.EqualFold(v, "success") && !strings.EqualFold(v, "ok") {
			return false
		}
	}
	switch v := e.Success.(type) {
	case bool:
		if !v {
			return false
		}
	case string:
		if strings.EqualFold(v, "false") {
			return false
		}
	}
	return true
}

// text 返回上游业务消息（msg 优先，message 兜底，error 兜底）。
func (e *wbEnvelope) text() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Message != "" {
		return e.Message
	}
	if s, ok := e.Error.(string); ok && s != "" {
		return s
	}
	if e.Error != nil {
		return fmt.Sprint(e.Error)
	}
	if e.Status != "" {
		return e.Status
	}
	return ""
}

// dataOrSelf 在 data 缺省时返回顶层字段的宽容视图。
func (e *wbEnvelope) dataOrSelf() map[string]interface{} {
	if len(e.Data) > 0 {
		return e.Data
	}
	return map[string]interface{}(nil)
}

// creditsFloat64 把任意解码值宽容转成 float64（数值/字符串/布尔）。
func (m *Manager) creditsFloat64(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

// creditsFrom 从宽容解析出的数据对象提取 (剩余, 总量)，
// 先试顶层候选字段，再下钻一层 data/resource/usage/billing 包裹层。
func (m *Manager) creditsFrom(data map[string]interface{}) (credits, total float64) {
	if len(data) == 0 {
		return 0, 0
	}
	if c, t := m.creditsFromAccounts(data); c > 0 || t > 0 {
		return c, t
	}
	for k, v := range data {
		switch strings.ToLower(k) {
		case "credits", "credit", "remaining", "balance", "left", "credits_amount", "quota":
			if credits == 0 {
				credits = m.creditsFloat64(v)
			}
		case "credits_total", "total", "total_credits", "credits_limit", "limit":
			if total == 0 {
				total = m.creditsFloat64(v)
			}
		}
	}
	if credits == 0 && total == 0 {
		for k, v := range data {
			lk := strings.ToLower(k)
			if lk != "data" && lk != "response" && lk != "resource" && lk != "usage" && lk != "billing" && lk != "checkin" && lk != "meter" {
				continue
			}
			if nested, ok := v.(map[string]interface{}); ok {
				c, t := m.creditsFrom(nested)
				if c != 0 || t != 0 {
					return c, t
				}
			}
		}
	}
	if total > 0 && credits == 0 {
		if used := m.creditsFromUsed(data); used > 0 && used <= total {
			credits = total - used
		}
	}
	return credits, total
}

// creditsFromAccounts 处理 WorkBuddy 计费结构：data.Response.Data.Accounts
// 是资源包数组，credits=ΣCapacityRemain，total=ΣCapacitySize。
func (m *Manager) creditsFromAccounts(data map[string]interface{}) (credits, total float64) {
	var accounts []interface{}
	for k, v := range data {
		if strings.EqualFold(k, "accounts") {
			if arr, ok := v.([]interface{}); ok {
				accounts = arr
				break
			}
		}
	}
	for _, item := range accounts {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		for k, v := range entry {
			switch strings.ToLower(k) {
			case "capacityremain":
				credits += m.creditsFloat64(v)
			case "capacitysize":
				total += m.creditsFloat64(v)
			}
		}
	}
	return credits, total
}

// creditsFromUsed 提取已用量（total-used 推剩余用）。
func (m *Manager) creditsFromUsed(data map[string]interface{}) float64 {
	for k, v := range data {
		switch strings.ToLower(k) {
		case "used", "used_credits", "consumed", "cost":
			return m.creditsFloat64(v)
		}
	}
	return 0
}

// truthy 宽容判断布尔语义字段（checked_in/enabled/success 等）。
func truthy(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return strings.EqualFold(t, "true") || t == "1" || strings.EqualFold(t, "yes")
	}
	return false
}

// snippet 截断原始响应片段（错误消息用，避免刷屏）。
func snippet(s string) string {
	r := []rune(strings.TrimSpace(strings.ReplaceAll(s, "\n", " ")))
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return string(r)
}

// ensureTimeout 保障 ctx 带 60s 超时（后台调度用的裸 ctx）。
func ensureTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), 60*time.Second)
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 60*time.Second)
}
