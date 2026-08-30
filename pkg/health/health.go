// Package health 记录"渠道 + 模型"的可用性状态，让路由能提前绕开被限流、
// 掉登录或已下架的上游，而不是等用户撞上 429 才发现。状态只表示冷却，
// 不做永久禁用：到期自动回到候选名单。
package health

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Class string

const (
	ClassOK       Class = "ok"
	ClassRate     Class = "rate_limited"
	ClassNoCredit Class = "no_credit"
	ClassAuth     Class = "auth_failed"
	ClassNotFound Class = "not_found"
	ClassServer   Class = "server_error"
	ClassNetwork  Class = "network"
)

const (
	StatusOK      = "ok"
	StatusCooling = "cooling"
)

// Policy 是各类错误的冷却档位。
type Policy struct {
	Rate        time.Duration
	NoCredit    time.Duration
	Auth        time.Duration
	NotFound    time.Duration
	Server      time.Duration
	Network     time.Duration
	ServerFails int
}

func DefaultPolicy() Policy {
	return Policy{
		Rate:        60 * time.Second,
		NoCredit:    6 * time.Hour,
		Auth:        1 * time.Hour,
		NotFound:    30 * time.Minute,
		Server:      10 * time.Minute,
		Network:     30 * time.Second,
		ServerFails: 3,
	}
}

// State 是对外可见的单个模型健康度。
type State struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Status    string    `json:"status"`
	Class     Class     `json:"class,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Until     time.Time `json:"until,omitempty"`
	Failures  int       `json:"failures,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

type entry struct {
	status    string
	class     Class
	reason    string
	until     time.Time
	failures  int
	checkedAt time.Time
}

// Registry 并发安全；persist 回调尽力而为，写失败只影响重启后的预热。
type Registry struct {
	mu      sync.Mutex
	entries map[string]*entry
	policy  Policy
	now     func() time.Time
	persist func(string)
}

func NewRegistry(policy Policy, persist func(string)) *Registry {
	if policy.ServerFails <= 0 {
		policy.ServerFails = 3
	}
	return &Registry{
		entries: map[string]*entry{},
		policy:  policy,
		now:     time.Now,
		persist: persist,
	}
}

func Key(provider, model string) string {
	return provider + "\x1f" + strings.ToLower(strings.TrimSpace(model))
}

func splitKey(k string) (string, string) {
	if i := strings.IndexByte(k, '\x1f'); i >= 0 {
		return k[:i], k[i+1:]
	}
	return "", k
}

// MarkOK 清除冷却，失败计数归零。
func (r *Registry) MarkOK(provider, model string) {
	k := Key(provider, model)
	r.mu.Lock()
	if e, ok := r.entries[k]; ok && e.status == StatusOK && e.failures == 0 {
		r.mu.Unlock()
		return
	}
	r.entries[k] = &entry{status: StatusOK, failures: 0, checkedAt: r.now()}
	snap := r.snapshotLocked()
	r.mu.Unlock()
	r.emit(snap)
}

// MarkFailure 按错误类别冷却，冷却时长随连续失败次数递增；返回本次冷却时长便于调用方记日志。
func (r *Registry) MarkFailure(provider, model string, class Class, reason string) time.Duration {
	return r.MarkFailureAfter(provider, model, class, reason, 0)
}

// MarkFailureAfter 在 MarkFailure 之上允许调用方带上上游给出的重试建议时长：
// 建议大于 0 时以它为准（钳到 [1s, 额度用尽档]），因为"上游说了 300 秒后配额恢复"
// 比我们猜的 60s 递增档准得多。
func (r *Registry) MarkFailureAfter(provider, model string, class Class, reason string, after time.Duration) time.Duration {
	if provider == "" && model == "" {
		return 0
	}
	base := r.cooldown(class)
	hint := clampHint(after, r.policy.NoCredit)
	k := Key(provider, model)
	r.mu.Lock()
	e := r.entries[k]
	if e == nil {
		e = &entry{status: StatusOK}
		r.entries[k] = e
	}
	e.failures++
	e.class = class
	e.reason = trim(reason)
	e.checkedAt = r.now()
	cooldown := time.Duration(0)
	switch {
	case hint > 0:
		cooldown = hint
	case base > 0:
		// 反复失败的模型只会被越关越久，避免"60s 一到就放出去继续撞 429"。
		cooldown = base * time.Duration(minInt(e.failures, 6))
	}
	if cooldown > 0 {
		e.status = StatusCooling
		e.until = r.now().Add(cooldown)
	}
	failures := e.failures
	snap := r.snapshotLocked()
	r.mu.Unlock()
	r.emit(snap)
	slog.Debug("health: failure recorded", "provider", provider, "model", model,
		"class", string(class), "failures", failures, "cooldown", cooldown.String(),
		"retry_after", after.String())
	return cooldown
}

// clampHint 把上游给的重试建议钳到可信区间：0 或负数表示没有建议，
// 超长（上游乱填 Retry-After: 31536000）也不能把模型关到比"额度用尽"更久。
func clampHint(after, ceiling time.Duration) time.Duration {
	if after <= 0 {
		return 0
	}
	if after < time.Second {
		return time.Second
	}
	if ceiling > 0 && after > ceiling {
		return ceiling
	}
	return after
}

type retryHinter interface{ RetryAfter() time.Duration }

// RetryAfter 取一次失败里上游给出的重试建议时长。渠道把它挂在自家错误类型上，
// 路由层不 import 具体渠道包就能读到；没有建议时返回 0。
func RetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	var h retryHinter
	if errors.As(err, &h) {
		return h.RetryAfter()
	}
	return 0
}

// ParseRetryAfter 解析 Retry-After 头：既可能是等待秒数，也可能是 HTTP-日期。
// 无法理解或已过期返回 0，交给调用方按默认档位处理。
func ParseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (r *Registry) cooldown(class Class) time.Duration {
	switch class {
	case ClassRate:
		return r.policy.Rate
	case ClassNoCredit:
		return r.policy.NoCredit
	case ClassAuth:
		return r.policy.Auth
	case ClassNotFound:
		return r.policy.NotFound
	case ClassServer:
		return r.policy.Server
	case ClassNetwork:
		return r.policy.Network
	}
	return 0
}

// Available 表示此刻允许把请求派给它。没有记录的模型乐观放行。
func (r *Registry) Available(provider, model string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[Key(provider, model)]
	if e == nil {
		return true
	}
	if e.status != StatusCooling {
		return true
	}
	if !r.now().Before(e.until) {
		e.status = StatusOK
		e.failures = 0
		e.until = time.Time{}
		return true
	}
	return false
}

// Failures 返回连续失败次数，供"5xx 累计到阈值才冷却"这类判断使用。
func (r *Registry) Failures(provider, model string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[Key(provider, model)]; e != nil {
		return e.failures
	}
	return 0
}

func (r *Registry) Snapshot() map[string]State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *Registry) snapshotLocked() map[string]State {
	out := make(map[string]State, len(r.entries))
	now := r.now()
	for k, e := range r.entries {
		provider, model := splitKey(k)
		st := State{
			Provider: provider, Model: model, Status: e.status,
			Class: e.class, Reason: e.reason, Failures: e.failures, CheckedAt: e.checkedAt,
		}
		if e.status == StatusCooling && now.Before(e.until) {
			st.Until = e.until
		} else if e.status == StatusCooling {
			st.Status = StatusOK
		}
		out[k] = st
	}
	return out
}

// Restore 把上次进程留下的冷却状态读回来，避免重启后重新踩一遍坏模型。
func (r *Registry) Restore(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	var saved map[string]State
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		slog.Warn("health: 无法解析已存健康状态，忽略", "error", err)
		return
	}
	now := r.now()
	r.mu.Lock()
	loaded := 0
	for k, st := range saved {
		if st.Status != StatusCooling || now.After(st.Until) {
			continue
		}
		r.entries[k] = &entry{
			status: StatusCooling, class: st.Class, reason: st.Reason,
			until: st.Until, failures: st.Failures, checkedAt: st.CheckedAt,
		}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 {
		slog.Info("health: 已恢复冷却中的模型状态", "count", loaded)
	}
}

func (r *Registry) emit(snap map[string]State) {
	if r.persist == nil {
		return
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	r.persist(string(b))
}

func trim(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}
