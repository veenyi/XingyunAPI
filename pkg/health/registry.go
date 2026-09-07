// Package health implements the per-model circuit breaker: failures put a
// "provider|model" key into cooldown; expiry restores it automatically.
// Cooldown only affects dispatch order — it never permanently disables a model.
package health

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Error classes surfaced by /api/model-status.
const (
	ClassRateLimited = "rate_limited"
	ClassNoCredit    = "no_credit"
	ClassAuthFailed  = "auth_failed"
	ClassNotFound    = "not_found"
	ClassServerError = "server_error"
	ClassNetwork     = "network"
)

// ClassLabels maps classes to the UI labels shown in the 模型与渠道 page.
var ClassLabels = map[string]string{
	ClassRateLimited: "被限流",
	ClassNoCredit:    "额度不足",
	ClassAuthFailed:  "登录失效",
	ClassNotFound:    "模型已下架",
	ClassServerError: "上游故障",
	ClassNetwork:     "网络不通",
}

// defaultCooldown per class.
var defaultCooldown = map[string]time.Duration{
	ClassRateLimited: 5 * time.Minute,
	ClassNoCredit:    30 * time.Minute,
	ClassAuthFailed:  time.Hour,
	ClassNotFound:    24 * time.Hour,
	ClassServerError: 5 * time.Minute,
	ClassNetwork:     2 * time.Minute,
}

// entry is the cooling state of one provider|model key.
type entry struct {
	Class     string     `json:"class"`
	Reason    string     `json:"reason,omitempty"`
	Until     time.Time  `json:"-"`
	Failures  int        `json:"-"`
	UpdatedAt time.Time  `json:"-"`
	timer     *time.Timer `json:"-"`
}

// State is one row of Snapshot (mirrors the model-status API shape).
type State struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Status   string `json:"status"`
	Class    string `json:"class,omitempty"`
	Until    string `json:"until,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type listener func(State)

// Registry is the concurrency-safe circuit-breaker store.
type Registry struct {
	mu        sync.Mutex
	entries   map[string]*entry
	listeners []listener
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{entries: map[string]*entry{}}
}

// OnChange subscribes to state transitions (used by the probe loop).
func (r *Registry) OnChange(fn listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = append(r.listeners, fn)
}

// Key builds the registry key for a provider/model pair.
func Key(provider, model string) string { return provider + "|" + model }

// splitKey splits a registry key back into its parts.
func splitKey(key string) (string, string) {
	i := strings.Index(key, "|")
	if i < 0 {
		return key, ""
	}
	return key[:i], key[i+1:]
}

// MarkFailure puts a key into cooldown, classified from err (or an explicit
// class via MarkFailureAfter). Retry-After wins over the default cooldown.
func (r *Registry) MarkFailure(provider, model string, err error) {
	class := Classify(err)
	until := time.Now().Add(defaultCooldown[class])
	if ra := RetryAfter(err); ra > 0 {
		until = time.Now().Add(ra)
	}
	r.put(provider, model, class, reasonText(err), until)
}

// MarkFailureAfter marks a key with an explicit class and cooldown duration.
func (r *Registry) MarkFailureAfter(provider, model, class string, d time.Duration) {
	r.put(provider, model, class, "", time.Now().Add(d))
}

// MarkOK clears any cooling state for the key and records success.
func (r *Registry) MarkOK(provider, model string) {
	r.mu.Lock()
	e, ok := r.entries[Key(provider, model)]
	if ok {
		if e.timer != nil {
			e.timer.Stop()
		}
		delete(r.entries, Key(provider, model))
	}
	r.mu.Unlock()
	if ok {
		r.emit(State{Provider: provider, Model: model, Status: "ok"})
	}
}

// Restore clears ALL cooling state (admin action from the models page).
func (r *Registry) Restore() {
	r.mu.Lock()
	keys := make([]string, 0, len(r.entries))
	for k, e := range r.entries {
		if e.timer != nil {
			e.timer.Stop()
		}
		keys = append(keys, k)
	}
	r.entries = map[string]*entry{}
	r.mu.Unlock()
	for _, k := range keys {
		p, m := splitKey(k)
		r.emit(State{Provider: p, Model: m, Status: "ok"})
	}
}

// Available reports whether the key is currently usable.
func (r *Registry) Available(provider, model string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[Key(provider, model)]
	if !ok {
		return true
	}
	return time.Now().After(e.Until)
}

// Failures returns the failure count recorded for the key.
func (r *Registry) Failures(provider, model string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[Key(provider, model)]; ok {
		return e.Failures
	}
	return 0
}

// Snapshot returns every cooling entry (the /api/model-status payload);
// expired entries are reported as available.
func (r *Registry) Snapshot() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []State{}
	now := time.Now()
	for k, e := range r.entries {
		p, m := splitKey(k)
		st := State{Provider: p, Model: m, Status: "ok", Reason: e.Reason}
		if now.Before(e.Until) {
			st.Status = "cooling"
			st.Class = e.Class
			st.Until = e.Until.UTC().Format(time.RFC3339)
		}
		out = append(out, st)
	}
	return out
}

// StreamProblem records a mid-stream failure (truncated response) as a
// failure without interrupting the current answer path.
func (r *Registry) StreamProblem(provider, model string, err error) {
	r.MarkFailure(provider, model, err)
}

func (r *Registry) put(provider, model, class, reason string, until time.Time) {
	k := Key(provider, model)
	r.mu.Lock()
	e := r.entries[k]
	if e == nil {
		e = &entry{}
		r.entries[k] = e
	}
	e.Class = class
	e.Reason = reason
	e.Until = until
	e.Failures++
	e.UpdatedAt = time.Now()
	if e.timer != nil {
		e.timer.Stop()
	}
	st := State{Provider: provider, Model: model, Status: "cooling", Class: class, Until: until.UTC().Format(time.RFC3339), Reason: reason}
	e.timer = time.AfterFunc(time.Until(until), func() { r.emit(State{Provider: provider, Model: model, Status: "ok"}) })
	r.mu.Unlock()
	r.emit(st)
}

func (r *Registry) emit(st State) {
	r.mu.Lock()
	ls := append([]listener(nil), r.listeners...)
	r.mu.Unlock()
	for _, fn := range ls {
		fn(st)
	}
}

// Classify maps an error to one of the known classes by inspecting its text.
func Classify(err error) string {
	if err == nil {
		return ""
	}
	return classifyText(err.Error())
}

func reasonText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

var classMarkers = []struct {
	markers []string
	class   string
}{
	{[]string{"429", "rate limit", "ratelimit", "too many requests", "被限流", "限流", "frequent"}, ClassRateLimited},
	{[]string{"insufficient", "no credit", "no_credit", "out of quota", "quota exhaust", "额度不足", "额度用尽", "余额不足", "配额用尽", "积分不足", "积分用完", "每日上限", "402", "payment"}, ClassNoCredit},
	{[]string{"401", "403", "unauthorized", "forbidden", "auth_failed", "登录失效", "invalid api key", "invalid_api_key", "token expired", "登录已过期", "session dead", "login pending"}, ClassAuthFailed},
	{[]string{"404", "not found", "not_found", "not in the plan", "模型不在套餐", "不支持的模型", "模型不存在", "已下架", "no available model", "model_not_found"}, ClassNotFound},
	{[]string{"500", "502", "503", "504", "server error", "bad gateway", "service unavailable", "上游故障", "上游服务"}, ClassServerError},
	{[]string{"timeout", "connection refused", "connection reset", "network", "eof", "tls", "dns", "网络不通", "网络"}, ClassNetwork},
}

func classifyText(text string) string {
	t := strings.ToLower(text)
	for _, cm := range classMarkers {
		if containsAny(t, cm.markers) {
			return cm.class
		}
	}
	return ClassServerError
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ParseRetryAfter extracts a duration from a Retry-After header value
// (seconds or HTTP-date).
func ParseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// RetryAfter extracts a retry delay from an error, looking for a
// "retry-after" marker or embedded seconds.
func RetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{"retry-after:", "retry after ", "try again in ", "请 ", "秒后重试"} {
		if i := strings.Index(s, marker); i >= 0 {
			rest := strings.TrimPrefix(s[i:], marker)
			var n int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(rest), "%d", &n); scanErr == nil && n > 0 && n < 86400 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return 0
}
