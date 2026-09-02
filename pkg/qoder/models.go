// models.go 动态模型拉取：COSY 签名 GET，body 必须为空串。
package qoder

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9.]+`)

// NormalizeModelName 将上游 display_name 转为客户端模型名。
// "Qwen3.8-Max" → "qwen3.8-max"
func NormalizeModelName(s string) string {
	s = strings.ToLower(s)
	s = nonAlphaNum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

type dynamicModel struct {
	Key           string `json:"key"`
	DisplayName   string `json:"display_name"`
	Enable        bool   `json:"enable"`
	IsReasoning   bool   `json:"is_reasoning"`
	IsVL          bool   `json:"is_vl"`
	MaxInputTokens int   `json:"max_input_tokens"`
	PriceFactor   float64 `json:"price_factor"`
}

// FetchModels 通过 COSY 签名 GET 拉取动态模型列表。
// 注意：签名 body 必须为空串 ""，不是 "{}"。
func (c *Client) FetchModels(a *QoderAccount) ([]DynamicModelEntry, error) {
	url := c.gatewayBase() + EpModels
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("cosy-user", a.UID)
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, a.AccessToken, a.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, "", url, false, ""); err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}

	var env struct {
		Data struct {
			Chat []dynamicModel `json:"chat"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.modelMap == nil {
		c.modelMap = map[string]string{}
	}
	c.mu.Unlock()

	var out []DynamicModelEntry
	for _, dm := range env.Data.Chat {
		if !dm.Enable || dm.Key == "" {
			continue
		}
		name := NormalizeModelName(dm.DisplayName)
		if name == "" {
			name = dm.Key
		}
		ctx := 180000
		if dm.MaxInputTokens > 0 {
			ctx = dm.MaxInputTokens
		}
		out = append(out, DynamicModelEntry{
			Name:          name,
			Key:           dm.Key,
			ContextWindow: ctx,
			IsReasoning:   dm.IsReasoning,
			PriceFactor:   dm.PriceFactor,
		})

		c.mu.Lock()
		c.modelMap[name] = dm.Key
		c.mu.Unlock()
	}

	slog.Debug("qoder: fetched dynamic models", "count", len(out))
	return out, nil
}

// DynamicModelEntry 是动态模型的规范化表示。
type DynamicModelEntry struct {
	Name          string
	Key           string
	ContextWindow int
	IsReasoning   bool
	PriceFactor   float64
}

// modelKey 解析客户端模型名到上游 key：动态缓存优先，静态表兜底。
func (c *Client) modelKey(clientName string) string {
	c.mu.RLock()
	if k, ok := c.modelMap[clientName]; ok {
		c.mu.RUnlock()
		return k
	}
	c.mu.RUnlock()
	return ModelKey(clientName)
}

// dynamicModelsCache 缓存动态模型列表，避免每次请求都拉取。
type dynamicModelsCache struct {
	mu       sync.RWMutex
	entries  []DynamicModelEntry
	fetchedAt time.Time
	err      error
	errAt    time.Time
}

const (
	dynamicModelsTTL       = 1 * time.Hour
	dynamicModelsFailTTL   = 5 * time.Minute
)

func (c *dynamicModelsCache) get() ([]DynamicModelEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	if c.entries != nil && now.Sub(c.fetchedAt) < dynamicModelsTTL {
		return c.entries, true
	}
	if c.err != nil && now.Sub(c.errAt) < dynamicModelsFailTTL {
		return nil, false
	}
	return nil, false
}

func (c *dynamicModelsCache) set(entries []DynamicModelEntry, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.err = err
		c.errAt = time.Now()
		return
	}
	c.entries = entries
	c.fetchedAt = time.Now()
	c.err = nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
