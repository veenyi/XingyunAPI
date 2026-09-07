package qwenwork

// 聊天执行：OpenAI body → ChatCompletionRequest → gRPC ChatCompletionStream
// → 帧流 → OpenAI SSE / 完整响应。

import (
	"bufio"
	"bytes"
	crand "crypto/rand"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"
)

// gRPC 端点（worker byl={prod:"api2-v2.qoder.sh"}，CN 无特判）。
const (
	grpcEndpoint   = "https://api2-v2.qoder.sh"
	chatStreamPath = "/model.chat.ChatService/ChatCompletionStream"
	chatUnaryPath  = "/model.chat.ChatService/ChatCompletion"
)

// cosy metadata 常量（worker Gbl()：cosy-version/cosy-clienttype/cosy-machineos）。
const (
	cosyVersion       = "1.1.32"
	clientTypeDesktop = "6"
)

// cosyMachineOS 生成 cosy-machineos 值（worker: `${arch}_${platform}`，
// platform 取 node process.platform 命名：win32/darwin/linux）。
func cosyMachineOS() string {
	arch := runtime.GOARCH
	if arch == "arm64" {
		arch = "aarch64"
	} else if arch == "amd64" {
		arch = "x86_64"
	}
	plat := runtime.GOOS
	switch plat {
	case "windows":
		plat = "win32"
	case "darwin":
		plat = "darwin"
	}
	return arch + "_" + plat
}

// chatMetadata 组装 gRPC 业务 metadata（worker GrpcTransport 同构：
// authorization + x-request-id + x-session-id + cosy-*）。
func chatMetadata(cred *Credential, requestID, sessionID string) http.Header {
	h := http.Header{}
	if cred != nil && cred.AccessToken != "" {
		h.Set("authorization", "Bearer "+cred.AccessToken)
	}
	if requestID != "" {
		h.Set("x-request-id", requestID)
	}
	if sessionID != "" {
		h.Set("x-session-id", sessionID)
	}
	h.Set("cosy-version", cosyVersion)
	h.Set("cosy-clienttype", clientTypeDesktop)
	h.Set("cosy-machineos", cosyMachineOS())
	h.Set("user-agent", "QoderWork")
	return h
}

// chatOnce 执行单账号聊天（stream=true 返回规范化 OpenAI SSE 流）。
func chatOnce(ctx context.Context, hc *http.Client, cred *Credential, body map[string]interface{}, stream bool) (io.ReadCloser, map[string]interface{}, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, nil, fmt.Errorf("qwenwork: 账号缺少 access_token")
	}
	requestID := "req-" + randHexStr(12)
	sessionID := "sess-" + randHexStr(12)
	payload := encodeChatRequest(body, stream, requestID, sessionID)
	timeout := chatTimeout(body)
	resp, err := grpcCall(ctx, hc, grpcEndpoint, chatStreamPath, chatMetadata(cred, requestID, sessionID), payload, timeout)
	if err != nil {
		return nil, nil, err
	}
	if stream {
		return newGRPCSSE(resp), nil, nil
	}
	defer resp.Body.Close()
	// 非流式：读全部帧，取最后一个含 choices 的 chunk 作为响应
	var out map[string]interface{}
	for {
		frame, ferr := grpcNextFrame(resp.Body)
		if ferr != nil {
			break
		}
		if c := pbChunkCode(frame); c != 0 {
			return nil, nil, fmt.Errorf("qwenwork: 上游错误 code=%d", c)
		}
		out = decodeChunk(frame)
	}
	if err := grpcFinishErr(resp); err != nil {
		return nil, nil, err
	}
	if out == nil {
		return nil, nil, fmt.Errorf("qwenwork: 空响应")
	}
	out["object"] = "chat.completion"
	return nil, out, nil
}

// chatTimeout 估算请求超时（长上下文/长输出放宽）。
func chatTimeout(body map[string]interface{}) time.Duration {
	d := 10 * time.Minute
	if mt, ok := body["max_tokens"].(float64); ok && mt > 8192 {
		d = 20 * time.Minute
	}
	return d
}

// newGRPCSSE 把 gRPC 帧流转成标准 OpenAI SSE（data: {...}\n\n + [DONE]）。
func newGRPCSSE(resp *http.Response) io.ReadCloser {
	return &grpcSSEReader{resp: resp, br: bufio.NewReaderSize(resp.Body, 64<<10)}
}

type grpcSSEReader struct {
	resp   *http.Response
	br     *bufio.Reader
	out    bytes.Buffer
	closed bool
}

func (r *grpcSSEReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 {
		frame, err := grpcNextFrame(r.br)
		if err != nil {
			// 上游流结束：检查 trailers 状态
			if ferr := grpcFinishErr(r.resp); ferr != nil {
				r.emitSSEError(ferr.Error())
				break
			}
			r.out.WriteString("data: [DONE]\n\n")
			break
		}
		if code := pbChunkCode(frame); code != 0 {
			r.emitSSEError(fmt.Sprintf("上游错误 code=%d", code))
			break
		}
		chunk := decodeChunk(frame)
		if len(chunk) > 0 {
			fmt.Fprintf(&r.out, "data: %s\n\n", mustJSONString(chunk))
		}
	}
	if r.out.Len() == 0 {
		return 0, io.EOF
	}
	n, _ := r.out.Read(p)
	return n, nil
}

// emitSSEError 以 OpenAI chunk 形态注入错误（流已开始，无法改 HTTP 状态）。
func (r *grpcSSEReader) emitSSEError(msg string) {
	errChunk := map[string]interface{}{
		"id":      "chatcmpl-error",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "",
		"choices": []interface{}{},
		"error": map[string]interface{}{
			"message": msg,
			"type":    "upstream_error",
		},
	}
	fmt.Fprintf(&r.out, "data: %s\n\ndata: [DONE]\n\n", mustJSONString(errChunk))
}

func (r *grpcSSEReader) Close() error {
	r.closed = true
	return r.resp.Body.Close()
}

// randHexStr 返回 n 字节随机 hex（request id 用）。
func randHexStr(n int) string {
	// 独立于 checkin 包，直接用 crypto/rand
	const hexDigits = "0123456789abcdef"
	b := make([]byte, n)
	buf := make([]byte, n)
	if _, err := crand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	for i, v := range buf {
		b[i] = hexDigits[v&0x0f]
	}
	return string(b)
}
