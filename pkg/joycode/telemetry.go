package joycode

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 上游用 functionId=telemetry 的设备指纹上报判定"真实客户端活跃"。
// 只发 chat_completions 的账号约 24h 后会被 AI_GRAY_ACCESS_DENIED 冻结；
// 补发这组上报即可解冻（实测 pk0377 / 法罗力中国 均一次生效）。
const (
	telemetryFunctionID = "telemetry"
	metricsFunctionID   = "code_generation_metrics"
	logHistoryFunction  = "log_history"
)

// ideFingerprint 是官方 JoyCode IDE 客户端上报的设备指纹。
// 默认按运行主机生成：写死某台真机的身份会随安装包分发给所有实例，既能反查打包者，
// 也让所有实例上报同一设备、易被上游关联。上游按 ptKey 归属账号，指纹只需形态合法。
// 需要指定时用 JOYCODE_FP_OS/IDE/PLUGIN/HOST/DOMAIN/MAC/PROJECT 覆盖。
type ideFingerprint struct {
	OSName         string
	IDEVersion     string
	PluginVersion  string
	ComputerName   string
	ComputerDomain string
	MAC            string
	ProjectName    string
}

func hostName() string {
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return strings.TrimSpace(h)
	}
	return "joycode-client"
}

// localMAC 取第一块非回环网卡的硬件地址；取不到时用主机名哈希兜底，
// 保证同一主机每次上报一致、不同安装互不相同。
func localMAC() string {
	if ifcs, err := net.Interfaces(); err == nil {
		for _, ifc := range ifcs {
			if ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) == 0 {
				continue
			}
			return ifc.HardwareAddr.String()
		}
	}
	sum := sha256.Sum256([]byte(hostName()))
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", (sum[0]&0xfc)|0x02, sum[1], sum[2], sum[3], sum[4], sum[5])
}

func currentFingerprint() ideFingerprint {
	host := envOr("JOYCODE_FP_HOST", hostName())
	return ideFingerprint{
		OSName:         envOr("JOYCODE_FP_OS", "win32 x64 10.0.19045"),
		IDEVersion:     envOr("JOYCODE_FP_IDE", "3.0.10"),
		PluginVersion:  envOr("JOYCODE_FP_PLUGIN", "joycodeIDE-3.8.67"),
		ComputerName:   host,
		ComputerDomain: envOr("JOYCODE_FP_DOMAIN", strings.ToUpper(host)),
		MAC:            envOr("JOYCODE_FP_MAC", localMAC()),
		ProjectName:    envOr("JOYCODE_FP_PROJECT", "xingyun-keepalive"),
	}
}

// gatewayURL 按 functionId 直接构造 color gateway 地址（签名只覆盖 appid&functionId&t，不含 body）。
func (c *Client) gatewayURL(functionID string) (string, error) {
	base := c.ColorBaseURL
	if base == "" {
		base = DefaultColorBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid color base url %q: %w", base, err)
	}
	query, sig := colorSign(functionID)
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/") + colorGatewayPath + "?" + query + "&sign=" + sig, nil
}

// telemetryHeaders 复刻真机上报头：不带 source-type / x-ms-client-request-id，UA 为 node-fetch。
func (c *Client) telemetryHeaders(functionID string) http.Header {
	loginType := c.LoginType
	if loginType == "" {
		loginType = "N_PIN_PC"
	}
	h := http.Header{
		"Content-Type":    {"application/json; charset=UTF-8"},
		"ptKey":           {c.PtKey},
		"loginType":       {loginType},
		"User-Agent":      {"node-fetch"},
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip"},
	}
	if c.Tenant != "" {
		h.Set("tenant", url.QueryEscape(c.Tenant))
	}
	return h
}

func (c *Client) postGateway(functionID string, body map[string]interface{}) error {
	endpoint, err := c.gatewayURL(functionID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header = c.telemetryHeaders(functionID)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		if zr, zerr := gzip.NewReader(bytes.NewReader(raw)); zerr == nil {
			raw, _ = io.ReadAll(zr)
			zr.Close()
		}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway %s http %d: %s", functionID, resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("gateway %s unparseable response: %w", functionID, err)
	}
	if parsed.Code != 0 {
		return fmt.Errorf("gateway %s rejected: code=%d msg=%s", functionID, parsed.Code, parsed.Msg)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// telemetryEvent 构造一条与真机字段完全对齐的遥测事件。
func (c *Client) telemetryEvent(fp ideFingerprint, traceID, eventName string, seq int) map[string]interface{} {
	tenant := c.Tenant
	if tenant == "" {
		tenant = "JOYCODE"
	}
	return map[string]interface{}{
		"osName":         fp.OSName,
		"ideVersion":     fp.IDEVersion,
		"pluginVersion":  fp.PluginVersion,
		"userId":         c.UserID,
		"tenantId":       tenant,
		"computerName":   fp.ComputerName,
		"computerDomain": fp.ComputerDomain,
		"mac":            fp.MAC,
		"projectName":    fp.ProjectName,
		"curFilePath":    "",
		"codeLanguage":   "",
		// 与真机抓包一致留空；实测这份形态的单条上报即可解除 AI_GRAY_ACCESS_DENIED。
		"conversationId":    "",
		"source":            "client",
		"eventName":         eventName,
		"stage":             eventName,
		"eventTimestamp":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"traceId":           traceID,
		"spanId":            "",
		"parentSpanId":      "",
		"reportStrategy":    "REAL_TIME",
		"actionType":        "code_generation",
		"durationMs":        seq * 37,
		"apiResponseTimeMs": seq * 11,
		"firstChunkMs":      0,
		"firstRenderMs":     0,
		"streamDurationMs":  0,
		"modelName":         DefaultModel,
		"maxTokens":         0,
		"temperature":       0,
		"requestId":         "",
		"offset":            0,
	}
}

// ReportClientActivity 上报一轮"客户端真实使用"轨迹，用于解除/维持上游灰度授权。
// 真机在一次对话里会连续发 task_init_start → chat_request_start → first_chunk_received，
// 单条即可解冻，但按整序列上报与真机一致、更稳。
func (c *Client) ReportClientActivity() error {
	if c == nil || c.PtKey == "" {
		return fmt.Errorf("no credentials")
	}
	fp := currentFingerprint()
	id := randomHex(16)
	traceID := "trace_" + id + "_" + c.UserID + "_" + fmt.Sprintf("%d", time.Now().UnixMilli()) + "_" + randomHex(3)
	events := []string{"task_init_start", "chat_request_start", "first_chunk_received"}
	var lastErr error
	for i, ev := range events {
		body := map[string]interface{}{
			"telemetryList": []map[string]interface{}{c.telemetryEvent(fp, traceID, ev, i+1)},
		}
		if err := c.postGateway(telemetryFunctionID, body); err != nil {
			lastErr = err
		}
		time.Sleep(time.Duration(60+i*40) * time.Millisecond)
	}
	if lastErr != nil {
		slog.Warn("joycode: client activity report failed", "user_id", c.UserID, "error", lastErr)
		return lastErr
	}
	slog.Info("joycode: client activity reported", "user_id", c.UserID, "trace_id", traceID,
		"host", fp.ComputerName, "mac", fp.MAC)
	return nil
}

// ReportUsageMetrics 补齐 code_generation_metrics 与 log_history 两条上报，
// 与真机一次生成的收尾行为一致（灰度受限时与 telemetry 一起发效果最好）。
func (c *Client) ReportUsageMetrics() error {
	fp := currentFingerprint()
	tenant := c.Tenant
	if tenant == "" {
		tenant = "JOYCODE"
	}
	taskID := "task-" + randomHex(16)
	sess := randomHex(4)
	conv := taskID + "_session-" + sess
	now := time.Now().UnixMilli()
	metrics := map[string]interface{}{
		"osName": fp.OSName, "computerName": fp.ComputerName, "computerDomain": fp.ComputerDomain,
		"mac": fp.MAC, "ideVersion": "joycodeIDE-" + fp.IDEVersion, "pluginVersion": fp.PluginVersion,
		"userId": c.UserID, "userName": c.UserID, "erp": "", "tenant": tenant,
		"client": "JoyCode IDE", "model": DefaultModel, "language": "", "agentType": "code",
		"isCustomModel": false, "mode": "code_generation", "taskId": taskID,
		"conversationSource": "default", "conversationId": conv, "isUseFileInInput": false,
	}
	for i := 0; i <= 6; i++ {
		metrics[fmt.Sprintf("departmentC%d", i)] = ""
	}
	logHistory := map[string]interface{}{
		"ideaVersion": fp.IDEVersion, "pluginVersion": fp.PluginVersion, "user": c.UserID, "userToken": "",
		"osName": fp.OSName, "computerName": fp.ComputerName, "computerDomain": fp.ComputerDomain,
		"loginErp": "", "projectName": fp.ProjectName, "curFileName": "", "curFilePath": "",
		"actionType": "agent_generator", "question": "", "result": "",
		"startTime": now - 120, "endTime": now, "consumeTime": 120,
		"conversationId": conv, "model": DefaultModel,
		"extendMsg": map[string]interface{}{
			"gitUrl": "", "gitRepo": "", "gitBranch": "", "gitCommit": "",
			"modeType": "code", "taskId": taskID, "sessionId": "session-" + sess, "requestId": conv,
		},
	}
	err1 := c.postGateway(metricsFunctionID, metrics)
	err2 := c.postGateway(logHistoryFunction, logHistory)
	if err1 != nil {
		return err1
	}
	return err2
}
