package qoder

// Pool 是 Qoder CN 账号池（provider.Keyless 实现）：
// 多账号轮换 + 粘性路由 + 错误冷却/禁用 + 额度恢复 + settings blob 持久化。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/common"
	"github.com/veenyi/XingyunAPI/pkg/health"
)

// cooldownFor 与 health 默认冷却对齐。
func cooldownFor(class string) time.Duration {
	switch class {
	case health.ClassRateLimited:
		return 5 * time.Minute
	case health.ClassNoCredit:
		return 30 * time.Minute
	case health.ClassAuthFailed:
		return time.Hour
	case health.ClassNotFound:
		return 24 * time.Hour
	case health.ClassNetwork:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}

// Pool 是 Qoder 账号池。
type Pool struct {
	mu      sync.Mutex
	store   settingsStore
	http    *http.Client
	client  *Client
	entries []*poolEntry
	sticky  map[string]string // stickyKey -> user_id（成功后固化）
	pending map[string]string // stickyKey -> user_id（待成功确认）
	rr      int
}

// New 构造账号池并从 settings 载入账号（hc 为 nil 时用默认客户端）。
func New(st settingsStore, hc *http.Client) *Pool {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	p := &Pool{
		store:   st,
		http:    hc,
		sticky:  map[string]string{},
		pending: map[string]string{},
	}
	p.client = newClient(p, hc)
	p.SyncAccounts()
	return p
}

// Client 返回池内置的上游客户端。
func (p *Pool) Client() *Client { return p.client }

// --- provider.Keyless ---

// Name 实现 provider.Keyless。
func (p *Pool) Name() string { return Name }

// Enabled 实现 provider.Keyless：显式开关优先，缺省 = 有账号即启用。
func (p *Pool) Enabled() bool {
	p.mu.Lock()
	n := len(p.entries)
	p.mu.Unlock()
	return common.SettingEnabledOr(p.store, EnabledKey, n > 0)
}

// ListModels 实现 provider.Keyless：静态模型 + 动态目录。
func (p *Pool) ListModels() []string {
	p.client.ensureModels()
	out := append([]string(nil), StaticModels...)
	seen := map[string]bool{}
	for _, m := range out {
		seen[NormalizeModelName(m)] = true
	}
	for _, m := range dynModels.get() {
		k := NormalizeModelName(m.ID)
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// Supports 实现 provider.Keyless。
func (p *Pool) Supports(model string) bool {
	k := NormalizeModelName(model)
	if staticModelKeys[k] {
		return true
	}
	_, ok := dynamicModel(k)
	return ok
}

// Chat 实现 provider.Keyless（非流式，带账号轮换重试）。
func (p *Pool) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	_, out, err := p.DoChatWithRetry(ctx, body, false)
	return out, err
}

// ChatStream 实现 provider.Keyless（流式，带账号轮换重试），
// 返回规范化后的标准 OpenAI SSE 流。
func (p *Pool) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	rc, _, err := p.DoChatWithRetry(ctx, body, true)
	return rc, err
}

// DoChatWithRetry 带账号轮换的重试执行：stream=true 返回流，否则返回响应体。
func (p *Pool) DoChatWithRetry(ctx context.Context, body map[string]interface{}, stream bool) (io.ReadCloser, map[string]interface{}, error) {
	model := p.client.modelKey(fmt.Sprint(body["model"]))
	exclude := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		acct := p.PickWithSticky(model)
		if acct == nil {
			if lastErr == nil {
				lastErr = errors.New("qoder: 没有可用账号")
			}
			break
		}
		exclude[acct.UserID] = true
		if stream {
			rc, _, err := p.client.doChat(ctx, acct, body, true)
			if err != nil {
				lastErr = err
				slog.Warn("qoder: 账号请求失败，轮换下一个", "attempt", attempt+1, "error", err)
				p.NoteError(acct.UserID, err)
				p.StickyClear(model)
				continue
			}
			p.StickySuccess(model)
			return rc, nil, nil
		}
		out, err := p.client.doChatForAccount(ctx, acct, body)
		if err != nil {
			lastErr = err
			slog.Warn("qoder: 账号请求失败，轮换下一个", "attempt", attempt+1, "error", err)
			p.NoteError(acct.UserID, err)
			p.StickyClear(model)
			continue
		}
		p.StickySuccess(model)
		return nil, out, nil
	}
	return nil, nil, lastErr
}

// --- 账号池管理 ---

// Pick 挑一个可用账号（轮换）。
func (p *Pool) Pick() *QoderAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.pickLocked(nil)
	if e == nil {
		return nil
	}
	return e.acct
}

// PickExcluding 按排除表挑一个可用账号。
func (p *Pool) PickExcluding(exclude map[string]bool) *QoderAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.pickLocked(exclude)
	if e == nil {
		return nil
	}
	return e.acct
}

// PickWithSticky 按 sticky 键优先挑选：粘性账号健康时优先复用，
// 否则轮换新账号并记为待确认（StickySuccess 后固化）。
func (p *Pool) PickWithSticky(key string) *QoderAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	if key != "" {
		if id, ok := p.sticky[key]; ok {
			for _, e := range p.entries {
				if e.acct != nil && e.acct.UserID == id && p.usableLocked(e) {
					return e.acct
				}
			}
			delete(p.sticky, key)
		}
	}
	e := p.pickLocked(nil)
	if e == nil {
		return nil
	}
	if key != "" {
		p.pending[key] = e.acct.UserID
	}
	return e.acct
}

// StickySuccess 确认 sticky 键本次使用成功（固化绑定）。
func (p *Pool) StickySuccess(key string) {
	if key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.pending[key]; ok {
		p.sticky[key] = id
		delete(p.pending, key)
	}
}

// StickyClear 清除 sticky 键的绑定。
func (p *Pool) StickyClear(key string) {
	if key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sticky, key)
	delete(p.pending, key)
}

// pickLocked 在持锁状态下轮换挑选（跳过禁用/冷却/不健康/被排除的账号）。
func (p *Pool) pickLocked(exclude map[string]bool) *poolEntry {
	n := len(p.entries)
	if n == 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		e := p.entries[(p.rr+i)%n]
		if exclude != nil && exclude[e.acct.UserID] {
			continue
		}
		if !p.usableLocked(e) {
			continue
		}
		p.rr = (p.rr + i + 1) % n
		return e
	}
	return nil
}

func (p *Pool) usableLocked(e *poolEntry) bool {
	if e == nil || e.acct == nil {
		return false
	}
	if e.disabled || e.acct.Disabled || !e.healthy {
		return false
	}
	if time.Now().Before(e.cooldown) {
		return false
	}
	return e.acct.AccessToken != "" || e.acct.RefreshToken != ""
}

func (p *Pool) byIDLocked(id string) *poolEntry {
	for _, e := range p.entries {
		if e.acct != nil && e.acct.UserID == id {
			return e
		}
	}
	return nil
}

// NoteError 记录账号错误：按 class 冷却，硬标记/额度类禁用（可被恢复）。
func (p *Pool) NoteError(id string, err error) {
	if err == nil {
		return
	}
	class := Classify(err)
	hard := hitsAny(err.Error(), hardMarkers)
	p.mu.Lock()
	e := p.byIDLocked(id)
	if e != nil {
		e.healthy = false
		e.lastErr = truncateRunes(err.Error(), 300)
		e.failClass = class
		switch {
		case hard:
			e.acct.Disabled = true
		case class == health.ClassAuthFailed:
			e.acct.Disabled = true
		case class == health.ClassNoCredit:
			e.acct.Disabled = true
			e.cooldown = time.Now().Add(cooldownFor(class))
		default:
			e.cooldown = time.Now().Add(cooldownFor(class))
		}
	}
	p.mu.Unlock()
	if e != nil {
		slog.Warn("qoder: 账号进入冷却/禁用", "user_id", id, "class", class, "error", e.lastErr)
	}
}

// Cooldown 手动给账号加冷却。
func (p *Pool) Cooldown(id string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.byIDLocked(id); e != nil {
		e.cooldown = time.Now().Add(d)
		e.healthy = false
	}
}

// Disable 禁用账号（管理操作，持久化）。
func (p *Pool) Disable(id string) {
	p.mu.Lock()
	e := p.byIDLocked(id)
	if e != nil {
		e.disabled = true
		e.healthy = false
		e.acct.Disabled = true
	}
	var err error
	if e != nil {
		err = saveAccounts(p.store, p.accountListLocked())
	}
	p.mu.Unlock()
	if err != nil {
		slog.Error(errSaveAccounts, "error", err)
	}
}

// SetCredits 更新账号额度；额度恢复（>0）时解除额度类禁用。
func (p *Pool) SetCredits(id string, credits float64) {
	p.mu.Lock()
	e := p.byIDLocked(id)
	if e != nil {
		e.acct.Credits = credits
		if credits > 0 && e.failClass == health.ClassNoCredit {
			e.acct.Disabled = false
			e.disabled = false
			e.healthy = true
			e.cooldown = time.Time{}
			e.failClass = ""
		}
	}
	var err error
	if e != nil {
		err = saveAccounts(p.store, p.accountListLocked())
	}
	p.mu.Unlock()
	if err != nil {
		slog.Error(errSaveAccounts, "error", err)
	}
}

// ReenableIfCredits 对因额度不足被禁用的账号重新查询用量，有余额则恢复。
func (p *Pool) ReenableIfCredits() {
	p.mu.Lock()
	var ids []string
	for _, e := range p.entries {
		if e.acct != nil && e.acct.Disabled && e.failClass == health.ClassNoCredit {
			ids = append(ids, e.acct.UserID)
		}
	}
	p.mu.Unlock()
	for _, id := range ids {
		p.mu.Lock()
		e := p.byIDLocked(id)
		var acct *QoderAccount
		if e != nil {
			acct = e.acct
		}
		p.mu.Unlock()
		if acct == nil || acct.AccessToken == "" {
			continue
		}
		res, err := p.fetchUsage(acct)
		if err != nil {
			continue
		}
		p.SetCredits(id, res.Remaining)
	}
}

// SyncAccounts 从 settings blob 重载账号（保留运行态：冷却/失败分类）。
func (p *Pool) SyncAccounts() {
	accts, err := loadAccounts(p.store)
	if err != nil {
		slog.Warn("qoder: 加载账号失败", "error", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	old := map[string]*poolEntry{}
	for _, e := range p.entries {
		if e.acct != nil {
			old[e.acct.UserID] = e
		}
	}
	entries := make([]*poolEntry, 0, len(accts))
	for _, a := range accts {
		EnsureFingerprint(a)
		e := &poolEntry{acct: a, healthy: true}
		if prev, ok := old[a.UserID]; ok {
			e.cooldown = prev.cooldown
			e.lastErr = prev.lastErr
			e.failClass = prev.failClass
			e.healthy = prev.healthy || !a.Disabled
		}
		if a.Disabled {
			e.disabled = true
			e.healthy = false
		}
		entries = append(entries, e)
	}
	p.entries = entries
}

// accountListLocked 返回账号切片（须持锁）。
func (p *Pool) accountListLocked() []*QoderAccount {
	out := make([]*QoderAccount, 0, len(p.entries))
	for _, e := range p.entries {
		if e.acct != nil {
			out = append(out, e.acct)
		}
	}
	return out
}

// fetchUsage 查询账号额度（quota/usage）。
func (p *Pool) fetchUsage(acct *QoderAccount) (UserResourceF64, error) {
	req, err := http.NewRequest("GET", OpenAPIBase+quotaUsagePath, nil)
	if err != nil {
		return UserResourceF64{}, err
	}
	ApplyOpenAPIHeaders(req.Header, acct.AccessToken)
	hc := p.http
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return UserResourceF64{}, fmt.Errorf("qoder: 额度查询失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return UserResourceF64{}, fmt.Errorf("qoder: 额度查询失败: HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 200))
	}
	var parsed struct {
		Code int                 `json:"code"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return UserResourceF64{}, fmt.Errorf("qoder: 额度响应解析失败: %w", err)
	}
	return parseUserResource(parsed.Data), nil
}
