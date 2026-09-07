package qwenwork

// grpc：最小 gRPC 客户端（net/http 自动 HTTP/2 + 手写消息帧）。
// 协议：POST {path}，content-type: application/grpc，te: trailers；
// 请求体 = 1 字节压缩标志(0) + 4 字节大端长度 + proto payload；
// 响应同帧格式流式读取；结束状态在 trailers（Grpc-Status/Grpc-Message，
// trailers-only 错误响应时在响应头）。

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// grpcFrame 把单条消息封帧。
func grpcFrame(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// grpcNextFrame 从流中读一帧 payload。
func grpcNextFrame(r io.Reader) ([]byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	if head[0] != 0 {
		return nil, errors.New("grpc: 压缩帧不支持")
	}
	n := binary.BigEndian.Uint32(head[1:5])
	if n > 64<<20 {
		return nil, fmt.Errorf("grpc: 帧过大 %d", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// grpcCall 发起 gRPC 调用，返回已校验 HTTP 层的响应（body 为帧流）。
// headers 里给业务 metadata（authorization 等，键大小写不敏感）。
func grpcCall(ctx context.Context, hc *http.Client, endpoint, path string, headers http.Header, payload []byte, timeout time.Duration) (*http.Response, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	if timeout > 0 && hc.Timeout == 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(grpcFrame(payload))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/grpc")
	req.Header.Set("te", "trailers")
	if timeout > 0 {
		req.Header.Set("grpc-timeout", grpcTimeoutVal(timeout))
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	// trailers-only 错误：grpc-status 直接在响应头
	if st := resp.Header.Get("Grpc-Status"); st != "" {
		code, msg := parseGrpcStatus(st, resp.Header.Get("Grpc-Message"))
		if code != 0 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, grpcError(code, msg)
		}
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("grpc: HTTP %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return resp, nil
}

// grpcFinishErr 在 body 读完后检查 trailers 的结束状态（0=nil）。
func grpcFinishErr(resp *http.Response) error {
	st := resp.Trailer.Get("Grpc-Status")
	if st == "" {
		return nil
	}
	code, msg := parseGrpcStatus(st, resp.Trailer.Get("Grpc-Message"))
	if code == 0 {
		return nil
	}
	return grpcError(code, msg)
}

// parseGrpcStatus 解析状态码与 percent-encoded 消息。
func parseGrpcStatus(codeStr, msg string) (int, string) {
	code, _ := strconv.Atoi(codeStr)
	if dec, err := url.PathUnescape(msg); err == nil {
		msg = dec
	}
	return code, msg
}

// grpcTimeoutVal 生成 gRPC timeout 头（纳秒精度上限 8 位数字）。
func grpcTimeoutVal(d time.Duration) string {
	ns := d.Nanoseconds()
	if ns <= 0 {
		return "600S"
	}
	v := strconv.FormatInt(ns, 10)
	if len(v) > 8 {
		// 降级到微秒/毫秒/秒，保持 <=8 位
		for _, unit := range []struct {
			div int64
			sym string
		}{{1000, "u"}, {1000000, "m"}, {1000000000, "S"}} {
			q := ns / unit.div
			s := strconv.FormatInt(q, 10)
			if len(s) <= 8 {
				return s + unit.sym
			}
		}
		return "99999999u"
	}
	return v + "n"
}

// grpcError 把 gRPC 状态转为错误。
func grpcError(code int, msg string) error {
	if msg == "" {
		msg = "unknown"
	}
	return fmt.Errorf("grpc: status %d: %s", code, msg)
}

// truncateStr 按字符截断。
func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
