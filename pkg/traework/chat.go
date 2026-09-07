package traework

// TraeWork (TRAE SOLO CN) 聊天上游协议——对齐 wild-work 实测实现：
//   端点  POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat
//   鉴权  Cloud-IDE-JWT <token>（三头同值：Authorization/X-Cloudide-Token/X-Ide-Token）
//   请求  OpenAI 风格改写：强制 stream=true、function=solo_work_lite、
//         model→config_name、content 字符串→[{type:text,text}]、
//         developer→system、assistant tool_calls→function_call、
//         tools[].function.parameters 对象→JSON 字符串
//   响应  SOLO 自定义 SSE（event: metadata/timing_cost/output/token_usage/done/error）
//         → 本地转标准 OpenAI SSE 或聚合为完整响应

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	agentHost  = "https://trae-api-cn.mchost.guru"
	epChat     = "/api/agent/v3/llm_utils_chat"
	appID      = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	function   = "solo_work_lite"
	ideVersion = "0.1.52" // 聊天链路对齐 wild-work 验证组合（签到侧用行云自己的 0.1.62 不受影响）
	verCode    = "20260811"
	osVersion  = "Windows 10 Pro"
	devBrand   = "20Y5A002XX"
	clientUA   = "Trae/" + ideVersion

	defaultModel = "glm-5.2"
)

// soLOHeaders 构造 llm_utils_chat 请求头（wild-work SOLOHeaders 同构）。
func soloHeaders(req *http.Request, cred *Credential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", clientUA)
	at := cred.AccessToken
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if cred.UID != "" {
		req.Header.Set("X-Uid", cred.UID)
	}
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", ideVersion)
	req.Header.Set("X-Ide-Version-Code", verCode)
	req.Header.Set("X-App-Version-Code", verCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", osVersion)
	req.Header.Set("X-Device-Brand", devBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if cred.MachineID != "" {
		req.Header.Set("X-Machine-Id", cred.MachineID)
	}
	if cred.DeviceID != "" {
		req.Header.Set("x-device-id", cred.DeviceID)
	}
}

// prepareBody 把 OpenAI chat/completions 请求改写为 Trae SOLO 请求体。
func prepareBody(src map[string]interface{}) map[string]interface{} {
	obj := make(map[string]interface{}, len(src)+3)
	for k, v := range src {
		obj[k] = v
	}
	obj["stream"] = true
	obj["function"] = function
	if msgs, ok := obj["messages"].([]interface{}); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]interface{})
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)
			if role == "developer" {
				m["role"] = "system"
				role = "system"
			}
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]interface{}); ok {
					kept := make([]interface{}, 0, len(tcs))
					for _, tci := range tcs {
						tc, ok := tci.(map[string]interface{})
						if !ok {
							continue
						}
						if fn, ok := tc["function"].(map[string]interface{}); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
						if fc, ok := tc["function_call"].(map[string]interface{}); ok {
							name, _ := fc["name"].(string)
							if strings.TrimSpace(name) == "" {
								continue
							}
						}
						kept = append(kept, tc)
					}
					if len(kept) == 0 {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = kept
					}
				}
			}
			if !present || content == nil {
				continue
			}
			if s, ok := content.(string); ok {
				m["content"] = []interface{}{map[string]interface{}{"type": "text", "text": s}}
			}
		}
	}
	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if i := strings.Index(model, "/"); i >= 0 { // 去渠道前缀 "traework/xxx"
		model = model[i+1:]
	}
	if model == "" {
		model = defaultModel
	}
	obj["config_name"] = model
	obj["model"] = model
	normalizeToolChoice(obj)
	normalizeTools(obj)
	return obj
}

// normalizeToolChoice 归一 tool_choice（SOLO 只认字符串形态）。
func normalizeToolChoice(obj map[string]interface{}) {
	suppress := func() { delete(obj, "tools"); delete(obj, "functions") }
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]interface{}:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]interface{}); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools 把 tools[].function.parameters 对象序列化为字符串（SOLO 要求）。
func normalizeTools(obj map[string]interface{}) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]interface{})
	if !ok || len(list) == 0 {
		return
	}
	out := make([]interface{}, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]interface{})
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]interface{}); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}

// chatOnce 单账号聊天（stream=true 返回 OpenAI SSE 流，否则聚合完整响应）。
func chatOnce(ctx context.Context, hc *http.Client, cred *Credential, body map[string]interface{}, stream bool) (io.ReadCloser, map[string]interface{}, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, nil, fmt.Errorf("traework: 账号缺少 access_token")
	}
	payload, err := json.Marshal(prepareBody(body))
	if err != nil {
		return nil, nil, fmt.Errorf("traework: 请求体序列化失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentHost+epChat, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	soloHeaders(req, cred)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, nil, classifyStatus(resp.StatusCode, string(raw))
	}
	if stream {
		return newSoloSSE(resp.Body, modelOf(body)), nil, nil
	}
	defer resp.Body.Close()
	out, err := aggregateSolo(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if out != nil {
		out["model"] = modelOf(body)
	}
	return nil, out, nil
}

// modelOf 取原始请求模型名（响应用）。
func modelOf(body map[string]interface{}) string {
	if m, ok := body["model"].(string); ok && strings.TrimSpace(m) != "" {
		return strings.TrimSpace(m)
	}
	return defaultModel
}

// classifyStatus HTTP 错误分类（wild-work Classify 语义）。
func classifyStatus(status int, body string) error {
	lower := strings.ToLower(body)
	if strings.Contains(body, `"code":1005`) || (status == 403 && strings.Contains(lower, "plan")) {
		return fmt.Errorf("traework: 积分不足或权益不可用（HTTP %d）", status)
	}
	if status == http.StatusUnauthorized {
		return fmt.Errorf("traework: 令牌失效（HTTP 401）：%s", truncate(body, 160))
	}
	if status == http.StatusTooManyRequests {
		return fmt.Errorf("traework: 上游限流（HTTP 429）")
	}
	return fmt.Errorf("traework: 上游 HTTP %d: %s", status, truncate(body, 200))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
