package traework

// Pool 是 TraeWork（TRAE SOLO CN）聊天账号池（provider.Keyless 实现）。
// 凭据权威源 = 签到中心 blob 的 traework 平台账号（credsFn 回调实时取，
// 签到侧刷新令牌后自动生效），自身无持久化。多账号轮换 + sticky 路由。

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
const Name = "traework"

// Credential 是一次聊天可用的账号凭据（来自签到中心）。
type Credential struct {
	ID           string
	Nickname     string
	AccessToken  string
	RefreshToken string
	UID          string
	DeviceID     string
	MachineID    string
}

// StaticModels 静态模型表（SOLO 免费 config_name；未知名宽容透传上游裁决）。
var StaticModels = []string{
	"glm-5.2",
	"glm-5.3",
	"doubao-seed-1.8",
	"deepseek-v3.2",
	"kimi-k3",
}

// Pool 是 TraeWork 聊天池。
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

// ListModels 实现 provider.Keyless。
func (p *Pool) ListModels() []string {
	return append([]string(nil), StaticModels...)
}

// Supports 实现 provider.Keyless：未知模型名宽容接受（透传上游裁决）。
func (p *Pool) Supports(model string) bool {
	_ = model
	return true
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
	model := modelOf(body)
	creds := p.pickCreds(model)
	if len(creds) == 0 {
		return nil, nil, errors.New("traework: 没有可用账号，请先在签到中心导入 TraeWork（TRAE SOLO）账号")
	}
	var lastErr error
	for i, cred := range creds {
		if i > 0 {
			slog.Warn("traework: 账号请求失败，轮换下一个", "attempt", i, "error", lastErr)
		}
		rc, out, err := chatOnce(ctx, p.hc, &cred, body, stream)
		if err != nil {
			lastErr = err
			if isAuthErr(err) {
				slog.Warn("traework: 令牌失效", "cred_id", cred.ID)
			}
			continue
		}
		p.mu.Lock()
		p.sticky[model] = cred.ID
		p.mu.Unlock()
		return rc, out, nil
	}
	if isAuthErr(lastErr) {
		return nil, nil, fmt.Errorf("traework: 模型服务拒绝凭据（%w）。请到签到中心对 TraeWork 账号重新登录", lastErr)
	}
	return nil, nil, lastErr
}

// pickCreds 取凭据列表：sticky 优先置顶，否则轮换。
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

// isAuthErr 报告错误是否为认证类。
func isAuthErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 401") || strings.Contains(s, "令牌失效") ||
		strings.Contains(s, "unauthorized") || strings.Contains(s, "session")
}
