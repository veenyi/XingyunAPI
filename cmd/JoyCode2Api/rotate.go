package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// ── 聚合 Key：一个 key 自动轮询多账号（按剩余积分） ────────────────
//
// 使用聚合 Key 调用时（Authorization: Bearer <aggregate_key>），
// 自动在所有账号中按剩余积分选择可用账号（剩余 >= 阈值），
// 并在可用账号之间轮询；积分低于阈值的账号被跳过，全部不可用时回退到积分最高的账号。
//
// 聚合 Key 随机生成并持久化在设置项 aggregate_key 中，可在设置页重新生成（旧 Key 立即失效）。
// 阈值通过设置项 points_rotate_threshold 配置（默认 10）。

const remainCacheTTL = 60 * time.Second

var (
	remainCacheMu sync.Mutex
	remainCache   = map[string]struct {
		remain int
		ts     time.Time
	}{}
	rotateCounter uint64
)

// getAggregateKey 返回当前聚合 Key；未设置时自动生成一个随机 Key 并持久化。
func getAggregateKey(s *store.Store) string {
	if k := s.GetSetting("aggregate_key"); k != "" {
		return k
	}
	k := genAggregateKey()
	_ = s.SetSetting("aggregate_key", k)
	return k
}

// genAggregateKey 生成随机聚合 Key（sk-joy-<32hex>）
func genAggregateKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "sk-joy-" + time.Now().Format("20060102150405")
	}
	return "sk-joy-" + hex.EncodeToString(b)
}

// getAccountRemain 查询账号剩余积分（缓存 TTL 内不重复请求上游）。
// 查询失败返回 -1（视为可用，避免误伤）。
func getAccountRemain(s *store.Store, userID string) int {
	remainCacheMu.Lock()
	if c, ok := remainCache[userID]; ok && time.Since(c.ts) < remainCacheTTL {
		remainCacheMu.Unlock()
		return c.remain
	}
	remainCacheMu.Unlock()

	account, err := s.GetAccount(userID)
	if err != nil || account == nil {
		return -1
	}
	client := joycode.NewClient(account.PtKey, account.UserID)
	resp, err := client.GetPoint()
	if err != nil {
		return -1
	}
	remain := -1
	if data, ok := resp["data"].(map[string]interface{}); ok {
		if items, ok := data["usageItems"].([]interface{}); ok && len(items) > 0 {
			if first, ok := items[0].(map[string]interface{}); ok {
				if r, ok := first["remain"].(float64); ok {
					remain = int(r)
				}
			}
		}
	}

	remainCacheMu.Lock()
	remainCache[userID] = struct {
		remain int
		ts     time.Time
	}{remain: remain, ts: time.Now()}
	remainCacheMu.Unlock()
	return remain
}

// resolveAggregateClient 聚合选号：按剩余积分过滤 + 轮询。
func resolveAggregateClient(s *store.Store, timeout time.Duration) *joycode.Client {
	threshold := s.GetIntSetting("points_rotate_threshold", 10)

	accounts, err := s.ListAccounts()
	if err != nil || len(accounts) == 0 {
		return nil
	}

	type cand struct {
		acc    store.AccountInfo
		remain int
	}
	var cands []cand
	for i := range accounts {
		remain := getAccountRemain(s, accounts[i].UserID)
		// remain < 0 表示查询失败 → 视为可用；remain >= threshold → 可用
		if remain < 0 || remain >= threshold {
			cands = append(cands, cand{acc: accounts[i], remain: remain})
		}
	}

	// 全部低于阈值 → 回退到积分最高的账号
	if len(cands) == 0 {
		best := accounts[0]
		bestRemain := -1
		for i := range accounts {
			r := getAccountRemain(s, accounts[i].UserID)
			if r > bestRemain {
				bestRemain = r
				best = accounts[i]
			}
		}
		return newAggregateClient(s, best, timeout)
	}

	// 可用账号间轮询
	idx := atomic.AddUint64(&rotateCounter, 1) % uint64(len(cands))
	return newAggregateClient(s, cands[idx].acc, timeout)
}

func newAggregateClient(s *store.Store, acc store.AccountInfo, timeout time.Duration) *joycode.Client {
	account, err := s.GetAccount(acc.UserID)
	if err != nil || account == nil {
		return nil
	}
	cl := joycode.NewClient(account.PtKey, account.UserID)
	cl.SetTimeout(timeout)
	return cl
}
