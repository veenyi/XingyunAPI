// Package route dispatches chat requests across providers with failover:
// the JoyCode SaaS client is primary, keyless channels are candidates in
// user-defined ranking order; health cooldowns and the blocklist prune the
// plan, and at most K=3 candidates are tried per request.
package route

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/common"
	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/provider"
)

// maxCandidates is the hard attempt cap per request (surfaced in the UI).
const maxCandidates = 3

// Settings is the router's view of the settings store.
type Settings interface {
	GetSetting(key string) string
}

// chatClient unifies provider.Chat and provider.Keyless.
type chatClient interface {
	Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error)
	ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error)
	Name() string
}

// KeylessSource returns the currently-registered keyless channels (dynamic —
// accounts/providers can come and go at runtime).
type KeylessSource func() []provider.Keyless

// Candidate is one dispatchable provider+model pair.
type Candidate struct {
	Provider string
	Model    string // model name to send upstream
	Ranked   bool   // whether the pair appears in the user's ranking
	Cl       chatClient
}

// Rank is one entry of the persisted user ranking.
type Rank struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Router wires primary + keyless sources with health and settings.
type Router struct {
	Primary provider.Chat
	Keyless KeylessSource
	Health  *health.Registry
	Store   Settings

	mu sync.Mutex
}

// SplitPrefixedModel splits "<provider>/model" when provider names a known
// channel. Returns ok=false for plain model names.
func (r *Router) SplitPrefixedModel(model string) (string, string, bool) {
	i := strings.Index(model, "/")
	if i <= 0 || i == len(model)-1 {
		return "", "", false
	}
	prefix, rest := model[:i], model[i+1:]
	for _, p := range r.keylessList() {
		if p.Enabled() && p.Name() == prefix {
			return prefix, rest, true
		}
	}
	return "", "", false
}

func (r *Router) keylessList() []provider.Keyless {
	if r.Keyless == nil {
		return nil
	}
	return r.Keyless()
}

// KeylessChannels returns the currently registered keyless channels
// (dashboard 模型与渠道页枚举所有候选行时使用).
func (r *Router) KeylessChannels() []provider.Keyless { return r.keylessList() }

// failoverEnabled reports whether cross-candidate retry is on.
func (r *Router) failoverEnabled() bool {
	return common.SettingEnabled(r.Store, "route_failover_enabled")
}

// Ranking returns the persisted ranking order.
func (r *Router) Ranking() []Rank {
	if r.Store == nil {
		return nil
	}
	raw := r.Store.GetSetting("model_ranking")
	if raw == "" {
		return nil
	}
	var out []Rank
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func (r *Router) rankingOr(providerName, model string) int {
	for i, rk := range r.Ranking() {
		if rk.Provider == providerName && rk.Model == model {
			return i
		}
	}
	return -1
}

// RankingOr reports whether the pair appears in the user's persisted ranking.
func (r *Router) RankingOr(providerName, model string) int {
	return r.rankingOr(providerName, model)
}

// Blocked reports whether the pair is on the model blocklist.
func (r *Router) Blocked(providerName, model string) bool {
	return r.blocked(providerName, model)
}

func (r *Router) blocked(providerName, model string) bool {
	if r.Store == nil {
		return false
	}
	raw := r.Store.GetSetting("model_blocklist")
	if raw == "" {
		return false
	}
	var blocked []string
	if err := json.Unmarshal([]byte(raw), &blocked); err != nil {
		return false
	}
	key := providerName + "|" + model
	for _, b := range blocked {
		if b == key {
			return true
		}
	}
	return false
}

// resolve builds the full candidate chain for a model (ranked first, then
// tier order, then unranked remainder). The primary JoyCode channel is the
// head candidate for unprefixed models.
func (r *Router) resolve(model string) []Candidate {
	var out []Candidate
	add := func(c Candidate) {
		if c.Provider == "" || c.Model == "" || r.blocked(c.Provider, c.Model) {
			return
		}
		out = append(out, c)
	}

	prefProvider, prefModel, prefixed := r.SplitPrefixedModel(model)
	if prefixed {
		for _, p := range r.keylessList() {
			if p.Name() == prefProvider && p.Enabled() && p.Supports(prefModel) {
				add(Candidate{Provider: prefProvider, Model: prefModel, Ranked: r.rankingOr(prefProvider, prefModel) >= 0, Cl: p})
				break
			}
		}
		return out
	}

	if r.Primary != nil {
		add(Candidate{Provider: r.Primary.Name(), Model: model, Ranked: r.rankingOr(r.Primary.Name(), model) >= 0, Cl: r.Primary})
	}
	ranked, unranked := []Candidate{}, []Candidate{}
	for _, p := range r.keylessList() {
		if !p.Enabled() || !p.Supports(model) {
			continue
		}
		c := Candidate{Provider: p.Name(), Model: model, Ranked: r.rankingOr(p.Name(), model) >= 0, Cl: p}
		if c.Ranked {
			ranked = append(ranked, c)
		} else {
			unranked = append(unranked, c)
		}
	}
	out = append(out, ranked...)
	out = append(out, unranked...)
	return out
}

// Plan returns at most maxCandidates usable candidates, preferring ranked
// ones and skipping keys currently in cooldown. When the first choice is
// cooling but no same-tier alternative exists, the primary is returned for
// one direct attempt ("只按点名模型走一次").
func (r *Router) Plan(model string) []Candidate {
	all := r.resolve(model)
	if len(all) == 0 {
		return nil
	}
	if r.Health != nil {
		usable := make([]Candidate, 0, len(all))
		for _, c := range all {
			if r.Health.Available(c.Provider, c.Model) {
				usable = append(usable, c)
			}
		}
		if len(usable) > 0 {
			all = usable
		}
		// all cooling → fall through with the original order, one try each
	}
	if len(all) > maxCandidates {
		all = all[:maxCandidates]
	}
	return all
}

// ModelNames lists every visible provider/model pair for /v1/models.
func (r *Router) ModelNames() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(m string) {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if r.Primary != nil {
		for _, m := range r.Primary.ListModels() {
			add(m)
		}
	}
	for _, p := range r.keylessList() {
		if !p.Enabled() {
			continue
		}
		for _, m := range p.ListModels() {
			add(m)
		}
	}
	return out
}

// CleanNames strips a "<provider>/" prefix from every name.
func CleanNames(models []string) []string {
	out := make([]string, len(models))
	for i, m := range models {
		if idx := strings.Index(m, "/"); idx >= 0 {
			out[i] = m[idx+1:]
		} else {
			out[i] = m
		}
	}
	return out
}

// tierOf returns the dispatch tier of a candidate (lower dispatches first
// among unranked peers).
func tierOf(c Candidate) int {
	if t, ok := c.Cl.(interface{ Tier() int }); ok {
		return t.Tier()
	}
	return 100
}

func namesOf(cs []Candidate) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Provider+"/"+c.Model)
	}
	return strings.Join(parts, ", ")
}

func (r *Router) noteFailure(c Candidate, err error) {
	if r.Health == nil {
		return
	}
	if cf, ok := c.Cl.(interface {
		ClassifyError(error) string
	}); ok {
		class := cf.ClassifyError(err)
		var d time.Duration
		switch class {
		case health.ClassRateLimited:
			d = 5 * time.Minute
		case health.ClassNoCredit:
			d = 30 * time.Minute
		case health.ClassAuthFailed:
			d = time.Hour
		case health.ClassNotFound:
			d = 24 * time.Hour
		case health.ClassNetwork:
			d = 2 * time.Minute
		default:
			d = 5 * time.Minute
		}
		r.Health.MarkFailureAfter(c.Provider, c.Model, class, d)
		return
	}
	r.Health.MarkFailure(c.Provider, c.Model, err)
}

// failoverNotice appends a system message explaining the substitution so the
// user knows a different channel answered ("提示：%s 暂不可用，本次由 %s 回答").
func failoverNotice(body map[string]interface{}, from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	msgs, _ := body["messages"].([]interface{})
	note := fmt.Sprintf("提示：%s 暂不可用，本次由 %s 回答", from, to)
	msgs = append(msgs, map[string]interface{}{"role": "system", "content": note})
	body["messages"] = msgs
}

// pickModel rewrites the request's model for the chosen candidate.
func pickModel(body map[string]interface{}, c Candidate) (requested string) {
	requested, _ = body["model"].(string)
	body["model"] = c.Model
	return requested
}

// Chat dispatches a non-streaming request across the plan.
func (r *Router) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	model, _ := body["model"].(string)
	plan := r.Plan(model)
	if len(plan) == 0 {
		return nil, fmt.Errorf("没有可用上游渠道")
	}
	var lastErr error
	for i, c := range plan {
		if i > 0 && !r.failoverEnabled() {
			return nil, lastErr
		}
		out, err := c.Cl.Chat(ctx, body)
		if err == nil {
			if lookErr := lookErr(out); lookErr != nil {
				lastErr = lookErr
				slog.Warn("route: 候选进入冷却", "provider", c.Provider, "model", c.Model, "error", lookErr)
				r.noteFailure(c, lookErr)
				continue
			}
			if r.Health != nil {
				r.Health.MarkOK(c.Provider, c.Model)
			}
			if i > 0 {
				failoverNotice(body, model, c.Provider+"/"+c.Model)
			}
			return out, nil
		}
		lastErr = err
		slog.Warn("route: 自动切换到下一个候选", "provider", c.Provider, "model", c.Model,
			"attempt", i+1, "of", len(plan), "error", err)
		r.noteFailure(c, err)
	}
	return nil, lastErr
}

// lookErr surfaces 200-wrapped upstream errors.
func lookErr(out map[string]interface{}) error {
	if out == nil {
		return nil
	}
	if e, ok := out["error"]; ok {
		switch v := e.(type) {
		case map[string]interface{}:
			msg, _ := v["message"].(string)
			if msg == "" {
				raw, _ := json.Marshal(v)
				msg = string(raw)
			}
			return fmt.Errorf("upstream error: %s", msg)
		case string:
			return fmt.Errorf("upstream error: %s", v)
		}
	}
	return nil
}

// ChatStream dispatches a streaming request across the plan; the returned
// reader is the raw upstream SSE stream of whichever candidate answered.
func (r *Router) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, string, error) {
	model, _ := body["model"].(string)
	plan := r.Plan(model)
	if len(plan) == 0 {
		return nil, "", fmt.Errorf("没有可用上游渠道")
	}
	var lastErr error
	for i, c := range plan {
		if i > 0 && !r.failoverEnabled() {
			return nil, "", lastErr
		}
		rc, err := c.Cl.ChatStream(ctx, body)
		if err == nil {
			if r.Health != nil {
				r.Health.MarkOK(c.Provider, c.Model)
			}
			if i > 0 {
				failoverNotice(body, model, c.Provider+"/"+c.Model)
			}
			return rc, c.Provider, nil
		}
		lastErr = err
		slog.Warn("route: 自动切换到下一个候选", "provider", c.Provider, "model", c.Model,
			"attempt", i+1, "of", len(plan), "error", err)
		r.noteFailure(c, err)
	}
	return nil, "", lastErr
}
