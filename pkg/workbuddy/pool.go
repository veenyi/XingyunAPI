// pool.go 多账号粘性路由 + 冷却状态机，实现 provider.Keyless 接口。
package workbuddy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
)

const (
	defaultMaxReqs   = 50
	defaultMaxRotate = 3
	hardCooldown     = 12 * time.Hour
	softCooldown     = 60 * time.Second
	errCooldown      = 10 * time.Minute
	errThreshold     = 3
	refreshSkew      = 10 * time.Minute
)

type coolKind int

const (
	coolHard coolKind = iota
	coolSoft
	coolErr
	coolLowBalance
)

type poolEntry struct {
	acct     *WBAccount
	disabled bool
	reason   string
	until    time.Time
	errCount int
	credits  int64
}

func (e *poolEntry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if e.until.IsZero() {
		return true
	}
	return now.After(e.until) || now.Equal(e.until)
}

// Pool 管理多账号池、粘性路由与冷却。
type Pool struct {
	mu      sync.Mutex
	entries map[string]*poolEntry
	client  *Client

	stickyUID   string
	stickyCount int
}

// NewPool 创建账号池。
func NewPool(c *Client) *Pool {
	return &Pool{
		entries: make(map[string]*poolEntry),
		client:  c,
	}
}

// SyncAccounts 同步账号列表到池中。
func (p *Pool) SyncAccounts(accs []*WBAccount) {
	p.mu.Lock()
	defer p.mu.Unlock()

	seen := map[string]bool{}
	for _, a := range accs {
		seen[a.UID] = true
		if e, ok := p.entries[a.UID]; ok {
			e.acct = a
		} else {
			p.entries[a.UID] = &poolEntry{acct: a}
		}
	}
	for uid := range p.entries {
		if !seen[uid] {
			delete(p.entries, uid)
			if p.stickyUID == uid {
				p.stickyUID = ""
				p.stickyCount = 0
			}
		}
	}

	p.client.SyncAccounts(accs)
}

// Pick 选一个健康账号（余额最高优先）。
func (p *Pool) Pick() *WBAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickLocked(nil)
}

// PickExcluding 选一个健康账号，排除已尝试的 UID。
func (p *Pool) PickExcluding(tried map[string]bool) *WBAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickLocked(tried)
}

func (p *Pool) pickLocked(tried map[string]bool) *WBAccount {
	now := time.Now()
	var best *poolEntry
	for uid, e := range p.entries {
		if !e.healthy(now) {
			continue
		}
		if tried != nil && tried[uid] {
			continue
		}
		if best == nil || e.credits > best.credits {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.acct
}

// PickWithSticky 粘性选择：同一账号连续使用 maxReqs 次。
func (p *Pool) PickWithSticky() *WBAccount {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	if p.stickyUID != "" {
		if e, ok := p.entries[p.stickyUID]; ok && e.healthy(now) && p.stickyCount < defaultMaxReqs {
			return e.acct
		}
		p.stickyUID = ""
		p.stickyCount = 0
	}

	acct := p.pickLocked(nil)
	if acct != nil {
		p.stickyUID = acct.UID
		p.stickyCount = 0
	}
	return acct
}

// StickySuccess 粘性计数 +1。
func (p *Pool) StickySuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stickyUID == uid {
		p.stickyCount++
	}
	if e, ok := p.entries[uid]; ok {
		e.errCount = 0
	}
}

// StickyClear 清除粘性。
func (p *Pool) StickyClear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stickyUID = ""
	p.stickyCount = 0
}

// Cooldown 对账号施加冷却。
func (p *Pool) Cooldown(uid string, kind coolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	e.until = time.Now().Add(d)
	e.reason = reason
	e.errCount = 0
	slog.Info("workbuddy: account cooldown", "uid", uid, "kind", kind, "duration", d, "reason", reason)
}

// Disable 永久禁用账号。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	e.disabled = true
	e.reason = reason
	if p.stickyUID == uid {
		p.stickyUID = ""
		p.stickyCount = 0
	}
	slog.Warn("workbuddy: account disabled", "uid", uid, "reason", reason)
}

// NoteError 记录一次错误，达到阈值时触发冷却。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	e.errCount++
	if e.errCount >= errThreshold {
		e.until = time.Now().Add(errCooldown)
		e.reason = fmt.Sprintf("consecutive errors (%d)", e.errCount)
		e.errCount = 0
	}
}

// SetCredits 更新账号余额。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		e.credits = credits
	}
}

// DoChatWithRetry 带重试的聊天请求（粘性路由 + 错误分类 + 冷却）。
func (p *Pool) DoChatWithRetry(rawBody []byte) (io.ReadCloser, int, []byte, *WBAccount, error) {
	tried := map[string]bool{}

	for attempt := 0; attempt < defaultMaxRotate; attempt++ {
		acct := p.PickWithSticky()
		if acct == nil {
			return nil, 0, nil, nil, fmt.Errorf("no healthy workbuddy account available")
		}
		if tried[acct.UID] {
			p.StickyClear()
			acct = p.PickExcluding(tried)
			if acct == nil {
				return nil, 0, nil, nil, fmt.Errorf("all accounts unavailable (tried: %d)", len(tried))
			}
		}
		tried[acct.UID] = true

		if acct.NeedsRefresh(refreshSkew) {
			chatBase := p.client.chatBase(acct)
			if err := RefreshToken(acct, p.client.HTTP, chatBase); err != nil {
				p.StickyClear()
				if strings.Contains(err.Error(), "session dead") || strings.Contains(err.Error(), "401") {
					p.Disable(acct.UID, "refresh: session dead")
				} else {
					p.Cooldown(acct.UID, coolErr, errCooldown, "refresh failed")
				}
				continue
			}
		}

		rc, status, errBody, err := p.client.doChatForAccount(acct, rawBody)
		if err != nil {
			p.StickyClear()
			p.NoteError(acct.UID)
			continue
		}

		if rc != nil {
			p.StickySuccess(acct.UID)
			return rc, status, nil, acct, nil
		}

		p.StickyClear()
		kind := Classify(status, string(errBody))
		switch kind {
		case ErrHardCredit:
			p.Cooldown(acct.UID, coolHard, hardCooldown, "余额/权益不足")
		case ErrSoftRate:
			p.Cooldown(acct.UID, coolSoft, softCooldown, "429 rate limit")
		case ErrSessionDead:
			p.Disable(acct.UID, fmt.Sprintf("http %d session dead", status))
		case ErrNotFound:
			p.Cooldown(acct.UID, coolSoft, softCooldown, "upstream 404")
		default:
			p.NoteError(acct.UID)
		}
		return nil, status, errBody, acct, nil
	}

	return nil, 0, nil, nil, fmt.Errorf("all accounts unavailable after %d attempts", defaultMaxRotate)
}

// ---- provider.Keyless 接口 ----

// Name 返回渠道名。
func (p *Pool) Name() string { return "workbuddy" }

// Enabled 当池中有至少一个可用账号时返回 true。
func (p *Pool) Enabled() bool { return p.client.Enabled() }

// Supports 判定模型是否由 WorkBuddy 渠道提供。
func (p *Pool) Supports(model string) bool { return p.client.Supports(model) }

// ListModels 返回可用模型列表。
func (p *Pool) ListModels() ([]joycode.ModelInfo, error) {
	return p.client.ListModels()
}

// ---- 池化聊天 ----

// ChatStream 流式池化聊天。
func (p *Pool) ChatStream(body map[string]any) (*http.Response, error) {
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	rc, status, errBody, _, chatErr := p.DoChatWithRetry(rawBody)
	if chatErr != nil {
		return nil, chatErr
	}
	if rc != nil {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_ = Stream(pw, rc)
		}()
		return &http.Response{
			StatusCode: 200,
			Body:       pr,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
		}, nil
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(errBody))),
		Header:     http.Header{"Content-Type": {"application/json"}},
	}, nil
}

// Chat 非流式池化聊天：强制上游流式，本地聚合。
func (p *Pool) Chat(body map[string]any) (map[string]any, error) {
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	rc, status, errBody, _, chatErr := p.DoChatWithRetry(rawBody)
	if chatErr != nil {
		return nil, chatErr
	}
	if rc != nil {
		defer rc.Close()
		model, _ := body["model"].(string)
		return Aggregate(rc, model)
	}
	if errBody != nil {
		var result map[string]any
		if json.Unmarshal(errBody, &result) == nil {
			return result, nil
		}
		return nil, fmt.Errorf("upstream http %d: %s", status, truncate(string(errBody), 200))
	}
	return nil, fmt.Errorf("upstream http %d", status)
}
