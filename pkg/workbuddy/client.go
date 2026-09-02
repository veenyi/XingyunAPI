// client.go WorkBuddy 渠道核心客户端：HTTP 连接、ChatStream、token 刷新、配额查询、动态模型。
package workbuddy

import (
	"bytes"
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

// WBAccount 是运行时账号表示（从 checkin.Account 同步而来）。
type WBAccount struct {
	UID          string
	Nickname     string
	AccessToken  string
	RefreshToken string
	EnterpriseID string
	Domain       string
	Region       string // "cn" (default) or "global"
	ExpiresAt    int64  // unix 秒
}

// NeedsRefresh 判断 token 是否需要刷新。
func (a *WBAccount) NeedsRefresh(skew time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(skew).Unix() >= a.ExpiresAt
}

// ErrKind 错误分类。
type ErrKind int

const (
	ErrNone        ErrKind = iota
	ErrHardCredit          // 余额不足 → 长冷却
	ErrSoftRate            // 429 → 短冷却
	ErrSessionDead         // session 失效 → 禁用
	ErrNotFound            // 404 → 短冷却
	ErrServer              // 5xx
	ErrClient              // 其他 4xx
)

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, m) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client 是 WorkBuddy 渠道上游 HTTP 客户端。
type Client struct {
	HTTP *http.Client

	ChatBaseCN      string
	BillingBaseCN   string
	ChatBaseGlobal  string
	BillingBaseGlob string

	mu       sync.RWMutex
	modelMap map[string]bool // 动态模型名集合

	dynCache   *dynModelsCache
	accountsMu sync.RWMutex
	accounts   []*WBAccount
}

// New 创建 WorkBuddy 客户端。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		HTTP:            &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatBaseCN:      "https://copilot.tencent.com",
		BillingBaseCN:   "https://www.codebuddy.cn",
		ChatBaseGlobal:  "https://www.workbuddy.ai",
		BillingBaseGlob: "https://www.workbuddy.ai",
		dynCache:        &dynModelsCache{},
	}
}

func (c *Client) chatBase(a *WBAccount) string {
	if a != nil && a.Region == "global" {
		return c.ChatBaseGlobal
	}
	return c.ChatBaseCN
}

func (c *Client) billingBase(a *WBAccount) string {
	if a != nil && a.Region == "global" {
		return c.BillingBaseGlob
	}
	return c.BillingBaseCN
}

// Name 返回渠道名。
func (c *Client) Name() string { return "workbuddy" }

// Enabled 当有至少一个活跃账号时返回 true。
func (c *Client) Enabled() bool {
	c.accountsMu.RLock()
	defer c.accountsMu.RUnlock()
	for _, a := range c.accounts {
		if a.AccessToken != "" {
			return true
		}
	}
	return false
}

// SyncAccounts 同步账号列表。
func (c *Client) SyncAccounts(accs []*WBAccount) {
	c.accountsMu.Lock()
	defer c.accountsMu.Unlock()
	c.accounts = accs
}

// Supports 判定模型是否由 WorkBuddy 渠道提供。
func (c *Client) Supports(model string) bool {
	c.mu.RLock()
	if c.modelMap != nil && c.modelMap[model] {
		c.mu.RUnlock()
		return true
	}
	c.mu.RUnlock()
	entries, ok := c.dynCache.get()
	if !ok {
		return false
	}
	for _, e := range entries {
		if e.Name == model {
			return true
		}
	}
	return false
}

// ListModels 返回可用模型列表：动态优先，静态兜底。
func (c *Client) ListModels() ([]joycode.ModelInfo, error) {
	entries, ok := c.dynCache.get()
	if ok {
		return wbEntriesToModelInfo(entries), nil
	}

	c.accountsMu.RLock()
	var acct *WBAccount
	for _, a := range c.accounts {
		if a.AccessToken != "" {
			acct = a
			break
		}
	}
	c.accountsMu.RUnlock()

	if acct == nil {
		return StaticModels(), nil
	}

	entries, err := c.FetchModels(acct)
	c.dynCache.set(entries, err)
	if err != nil {
		slog.Warn("workbuddy: dynamic model fetch failed, using static", "error", err)
		return StaticModels(), nil
	}
	return wbEntriesToModelInfo(entries), nil
}

// DynamicModelEntry 动态模型条目。
type DynamicModelEntry struct {
	Name          string
	ID            string
	ContextWindow int
}

func wbEntriesToModelInfo(entries []DynamicModelEntry) []joycode.ModelInfo {
	out := make([]joycode.ModelInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, joycode.ModelInfo{
			Label:          e.Name,
			ChatAPIModel:   e.Name,
			ModelID:        e.Name,
			MaxTotalTokens: e.ContextWindow,
			SupportStream:  true,
			Provider:       "workbuddy",
		})
	}
	return out
}

// FetchModels 调上游动态模型接口，过滤 cli agent 的模型列表。
func (c *Client) FetchModels(a *WBAccount) ([]DynamicModelEntry, error) {
	url := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererCN
	if a.Region == "global" {
		origin = "https://www.workbuddy.ai"
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", wbUA)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}

	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID             string `json:"id"`
				Name           string `json:"name"`
				MaxInputTokens int64  `json:"maxInputTokens"`
				Disabled       bool   `json:"disabled"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}

	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}

	dynMap := make(map[string]struct {
		ID             string
		Name           string
		MaxInputTokens int64
		Disabled       bool
	}, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = struct {
			ID             string
			Name           string
			MaxInputTokens int64
			Disabled       bool
		}{m.ID, m.Name, m.MaxInputTokens, m.Disabled}
	}

	out := make([]DynamicModelEntry, 0, len(cliIDs))
	c.mu.Lock()
	if c.modelMap == nil {
		c.modelMap = map[string]bool{}
	}
	c.mu.Unlock()

	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		name := normalizeModelName(m.Name)
		if name == "" {
			name = m.ID
		}
		ctx := 180000
		if m.MaxInputTokens > 0 {
			ctx = int(m.MaxInputTokens)
		}
		out = append(out, DynamicModelEntry{Name: name, ID: m.ID, ContextWindow: ctx})

		c.mu.Lock()
		c.modelMap[name] = true
		c.mu.Unlock()
	}

	slog.Debug("workbuddy: fetched dynamic models", "count", len(out))
	return out, nil
}

func normalizeModelName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// doChatForAccount 对指定账号执行一次上游 chat 请求。
// 成功时 rc != nil, err == nil；上游 4xx/5xx 时 rc == nil, err == nil, status+respBody 有值。
func (c *Client) doChatForAccount(acct *WBAccount, rawBody []byte) (io.ReadCloser, int, []byte, error) {
	url := c.chatBase(acct) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(PrepareBody(rawBody)))
	if err != nil {
		return nil, 0, nil, err
	}
	chatHeaders(req, acct)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// RefreshToken 刷新 WorkBuddy access token。
func RefreshToken(a *WBAccount, client *http.Client, chatBase string) error {
	if a.RefreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	url := chatBase + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	refreshHeaders(req, a)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("refresh http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var env struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("refresh parse: %w", err)
	}
	if env.Data.AccessToken == "" {
		return fmt.Errorf("refresh: no accessToken in response")
	}
	a.AccessToken = env.Data.AccessToken
	if env.Data.RefreshToken != "" {
		a.RefreshToken = env.Data.RefreshToken
	}
	if env.Data.Domain != "" {
		a.Domain = env.Data.Domain
	}
	if env.Data.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(env.Data.ExpiresIn) * time.Second).Unix()
	}
	slog.Info("workbuddy: token refreshed", "uid", a.UID)
	return nil
}

// UserResource 查询账号积分余额。
func UserResource(a *WBAccount, client *http.Client, billingBase string) (int64, error) {
	url := billingBase + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	billingHeaders(req, a)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(rawResp), 200))
	}

	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rawResp, &env); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	if env.Code != 0 {
		return 0, fmt.Errorf("code=%d %s", env.Code, truncate(env.Msg, 160))
	}

	var resource struct {
		Response struct {
			Data struct {
				Accounts []struct {
					CapacitySize        int64 `json:"CapacitySize"`
					CapacityRemain      int64 `json:"CapacityRemain"`
					CycleCapacitySize   int64 `json:"CycleCapacitySize"`
					CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(env.Data, &resource); err != nil {
		return 0, fmt.Errorf("resource data parse: %w", err)
	}

	var remain int64
	for _, acct := range resource.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
