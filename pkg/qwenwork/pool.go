package qwenwork

// Pool 是 QwenWork（千问办公）聊天账号池（provider.Keyless 实现）。
// 凭据权威源是签到中心（checkin blob 的 qwenwork 平台账号），
// 本池经 credsFn 回调实时取凭据（签到侧被动刷新后自动生效），
// 自身无状态持久化。多账号轮换 + 失败即换下一号。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Name 渠道名（前端渠道显示）。
const Name = "qwenwork"

// Credential 是一次聊天可用的账号凭据。
type Credential struct {
	ID           string
	Nickname     string
	AccessToken  string
	RefreshToken string
	DeviceID     string // loginDeviceId（可选，设备绑定校验用）
}

// Pool 是 QwenWork 聊天池。
type Pool struct {
	mu      sync.Mutex
	credsFn func() []Credential
	hc      *http.Client
	rr      int
	sticky  map[string]string // model -> credential ID
}

// NewPool 构造（credsFn 返回 nil/空 = 渠道未启用）。
func NewPool(credsFn func() []Credential, hc *http.Client) *Pool {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Pool{credsFn: credsFn, hc: hc, sticky: map[string]string{}}
}

// Enabled 实现 provider.Keyless。
func (p *Pool) Enabled() bool {
	if p == nil || p.credsFn == nil {
		return false
	}
	return len(p.credsFn()) > 0
}

// Name 实现 provider.Keyless。
func (p *Pool) Name() string { return Name }

// StaticModels 静态模型表（qwork-advanced=旗舰/高级；agnes-2.5-flash=UI 实测默认）。
var StaticModels = []string{"qwork-advanced", "agnes-2.5-flash"}

// staticKeys 小写索引。
var staticKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, m2 := range StaticModels {
		m[strings.ToLower(m2)] = true
	}
	return m
}()

// ListModels 实现 provider.Keyless。
func (p *Pool) ListModels() []string {
	return append([]string(nil), StaticModels...)
}

// Supports 实现 provider.Keyless：未知模型名宽容接受（透传上游裁决）。
func (p *Pool) Supports(model string) bool {
	_ = model
	return true
}

// normalizeModel 去渠道前缀（"qwenwork/xxx"）。
func normalizeModel(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return n
}

// Chat 实现 provider.Keyless（非流式）。
func (p *Pool) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	_, out, err := p.doChat(ctx, body, false)
	return out, err
}

// ChatStream 实现 provider.Keyless（流式，返回 OpenAI SSE）。
func (p *Pool) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	rc, _, err := p.doChat(ctx, body, true)
	return rc, err
}

// doChat 带账号轮换重试。
func (p *Pool) doChat(ctx context.Context, body map[string]interface{}, stream bool) (io.ReadCloser, map[string]interface{}, error) {
	model := normalizeModel(fmt.Sprint(body["model"]))
	creds := p.pickCreds(model)
	if len(creds) == 0 {
		return nil, nil, errors.New("qwenwork: 没有可用账号，请先在签到中心导入 QwenWork 凭据")
	}
	var lastErr error
	for i, cred := range creds {
		if i > 0 {
			slog.Warn("qwenwork: 账号请求失败，轮换下一个", "attempt", i, "error", lastErr)
		}
		rc, out, err := chatOnce(ctx, p.hc, &cred, body, stream)
		if err != nil {
			lastErr = err
			if isAuthErr(err) {
				// 令牌失效：触发一次签到侧同一凭据的刷新语义没有直接通道，
				// 记录后换号（签到轮询会自行刷新令牌）。
				slog.Warn("qwenwork: 令牌失效", "cred_id", cred.ID)
			}
			continue
		}
		p.mu.Lock()
		p.sticky[model] = cred.ID
		p.mu.Unlock()
		return rc, out, nil
	}
	if isAuthErr(lastErr) {
		return nil, nil, fmt.Errorf("qwenwork: 模型服务拒绝凭据（%w）。需在千问办公客户端退出重新登录后，到签到中心重新导入凭据", lastErr)
	}
	return nil, nil, lastErr
}

// pickCreds 取凭据列表：sticky 优先置顶。
func (p *Pool) pickCreds(model string) []Credential {
	all := p.credsFn()
	if len(all) == 0 {
		return nil
	}
	p.mu.Lock()
	stickyID := p.sticky[model]
	p.mu.Unlock()
	if stickyID != "" {
		for i, c := range all {
			if c.ID == stickyID {
				if i > 0 {
					out := append([]Credential{c}, all[:i]...)
					return append(out, all[i+1:]...)
				}
				return all
			}
		}
	}
	p.mu.Lock()
	id := all[p.rr%len(all)].ID
	p.rr++
	p.mu.Unlock()
	for i, c := range all {
		if c.ID == id {
			if i > 0 {
				out := append([]Credential{c}, all[:i]...)
				return append(out, all[i+1:]...)
			}
			return all
		}
	}
	return all
}

// isAuthErr 报告错误是否为认证类（gRPC 16=Unauthenticated / HTTP 401）。
func isAuthErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "status 16") ||
		strings.Contains(s, "Unauthenticated") ||
		strings.Contains(s, "401")
}
