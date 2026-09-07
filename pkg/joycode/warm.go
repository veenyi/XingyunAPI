package joycode

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	grayDeniedCode = "AI_GRAY_ACCESS_DENIED"

	// IDE 客户端指纹（逆向自本机 JoyCode IDE 实际上报，实测可过灰度活跃门）。
	warmOSName         = "win32 x64 10.0.26200"
	warmComputerName   = "veenyi"
	warmComputerDomain = "VEENYI"
	warmMac            = "00:50:56:c0:00:08"
	warmIDEVersion     = "3.0.10"
	warmPluginVersion  = "joycodeIDE-3.8.67"
	warmProjectName    = "2026-08-27-11-44-52"
)

// GrayDenied probes the real chat path and reports whether the account is
// blocked by the gray-release activity gate: HTTP 200 wrapping
// {"error":{"code":"AI_GRAY_ACCESS_DENIED"}} instead of an SSE stream.
func (c *Client) GrayDenied() (bool, error) {
	body := map[string]interface{}{
		"model":    APIModelName(DefaultModel),
		"messages": []map[string]string{{"role": "user", "content": "reply with exactly: ok"}},
		"stream":   true,
	}
	resp, err := c.PostStream(chatCompletionsEndpoint, body)
	if err != nil {
		if strings.Contains(err.Error(), grayDeniedCode) {
			return true, nil
		}
		return false, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(resp.Body, buf)
	return n > 0 && bytes.Contains(buf[:n], []byte(grayDeniedCode)), nil
}

// WarmTelemetry replays the IDE client activity signals the gray gate checks:
// telemetry (task_init_start / chat_request_start / first_chunk_received),
// code_generation_metrics, log_history and get_newpoint_ide. The gate
// typically clears about 8 seconds after these land.
func (c *Client) WarmTelemetry() error {
	user := c.UserID
	tenant := c.Tenant
	if tenant == "" {
		tenant = "JOYCODE"
	}
	id := warmHexID()
	taskID := "task-" + id
	sess := id[:8]
	trace := "trace_" + id + "_" + user + "_" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "_89nsp3g"
	conv := taskID + "_session-" + sess

	for _, ev := range []string{"task_init_start", "chat_request_start", "first_chunk_received"} {
		if err := c.warmCall("telemetry", telemetryBody(user, tenant, ev, trace)); err != nil {
			return fmt.Errorf("telemetry %s: %w", ev, err)
		}
	}
	if err := c.warmCall("code_generation_metrics", metricsBody(user, tenant, taskID, conv)); err != nil {
		return fmt.Errorf("code_generation_metrics: %w", err)
	}
	if err := c.warmCall("log_history", logHistoryBody(user, taskID, sess, conv)); err != nil {
		return fmt.Errorf("log_history: %w", err)
	}
	if err := c.warmCall("get_newpoint_ide", map[string]interface{}{}); err != nil {
		return fmt.Errorf("get_newpoint_ide: %w", err)
	}
	return nil
}

// warmCall posts a telemetry-family request through the color gateway.
// These functionIds are not in colorEndpoints: they only exist in gateway
// mode, so the URL is built directly.
func (c *Client) warmCall(functionID string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	base := c.ColorBaseURL
	if base == "" {
		base = DefaultColorBaseURL
	}
	query, sign := colorSign(functionID)
	req, err := http.NewRequest("POST", base+colorGatewayPath+"?"+query+"&sign="+sign, bytes.NewReader(data))
	if err != nil {
		return err
	}
	loginType := c.LoginType
	if loginType == "" {
		loginType = "PIN_JD_CLOUD"
	}
	tenant := c.Tenant
	if tenant == "" {
		tenant = "JOYCODE"
	}
	req.Header = http.Header{
		"Content-Type": {"application/json; charset=UTF-8"},
		"ptKey":        {c.PtKey},
		"loginType":    {loginType},
		"tenant":       {url.QueryEscape(tenant)},
		"User-Agent":   {UserAgent},
		"Accept":       {"*/*"},
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	respData, err := decodeBody(resp)
	if err != nil {
		resp.Body.Close()
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("warm %s: API error %d: %s", functionID, resp.StatusCode, truncate(string(respData), 300))
	}
	return nil
}

func warmHexID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func telemetryBody(user, tenant, eventName, traceID string) map[string]interface{} {
	return map[string]interface{}{
		"telemetryList": []interface{}{map[string]interface{}{
			"osName": warmOSName, "computerName": warmComputerName, "computerDomain": warmComputerDomain,
			"mac": warmMac, "ideVersion": warmIDEVersion, "pluginVersion": warmPluginVersion,
			"projectName": warmProjectName,
			"userId":      user, "tenantId": tenant,
			"curFilePath": "", "codeLanguage": "", "conversationId": "",
			"source": "client", "eventName": eventName, "stage": eventName,
			"eventTimestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			"traceId":        traceID, "spanId": "", "parentSpanId": "",
			"reportStrategy": "REAL_TIME", "actionType": "code_generation",
			"durationMs": 0, "apiResponseTimeMs": 0, "firstChunkMs": 0,
			"firstRenderMs": 0, "streamDurationMs": 0,
			"modelName": DefaultModel, "maxTokens": 0, "temperature": 0,
			"requestId": "", "offset": 0,
		}},
	}
}

func metricsBody(user, tenant, taskID, convID string) map[string]interface{} {
	return map[string]interface{}{
		"osName": warmOSName, "computerName": warmComputerName, "computerDomain": warmComputerDomain,
		"mac": warmMac, "ideVersion": "joycodeIDE-3.0.10",
		"userId": user, "userName": user,
		"departmentC0": "", "departmentC1": "", "departmentC2": "", "departmentC3": "",
		"departmentC4": "", "departmentC5": "", "departmentC6": "",
		"erp": "", "tenant": tenant, "client": "JoyCode IDE",
		"model": DefaultModel, "language": "", "agentType": "code",
		"isCustomModel": false, "mode": "code_generation",
		"taskId": taskID, "conversationSource": "default", "conversationId": convID,
		"isUseFileInInput": false,
	}
}

func logHistoryBody(user, taskID, sessID, convID string) map[string]interface{} {
	now := time.Now().UnixMilli()
	return map[string]interface{}{
		"ideaVersion": warmIDEVersion, "pluginVersion": warmPluginVersion,
		"user": user, "userToken": "", "osName": warmOSName,
		"computerName": warmComputerName, "computerDomain": warmComputerDomain,
		"loginErp": "", "projectName": warmProjectName,
		"curFileName": "", "curFilePath": "", "actionType": "agent_generator",
		"question": "", "result": "",
		"startTime": now - 120, "endTime": now, "consumeTime": 120,
		"conversationId": convID, "model": DefaultModel,
		"extendMsg": map[string]interface{}{
			"gitUrl": "", "gitRepo": "", "gitBranch": "", "gitCommit": "",
			"modeType": "code", "taskId": taskID,
			"sessionId": "session-" + sessID, "requestId": convID,
		},
	}
}
