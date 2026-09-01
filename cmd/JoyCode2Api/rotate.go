package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
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

// 积分缓存 TTL。聚合请求在缓存过期后的首个请求要重新查询所有账号积分，
// 120s 的窗口在「积分足够实时」与「降低周期卡顿」之间取平衡。
const remainCacheTTL = 120 * time.Second

// pointQueryTimeout 单个账号积分查询的超时。上游 getNewIdePoint 偶发慢响应，
// 若无超时（NewClient 默认 30min）会拖住整个聚合请求数秒~数十秒，表现为「卡顿」。
const pointQueryTimeout = 3 * time.Second

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
	if account.Tenant != "" || account.LoginType != "" || account.ColorBaseURL != "" || account.MasterBaseURL != "" || account.OrgFullName != "" {
		client.SetColorContext(account.ColorBaseURL, account.MasterBaseURL, account.Tenant, account.LoginType, account.OrgFullName)
	}
	client.SetTimeout(pointQueryTimeout) // 短超时：慢响应直接视为查询失败，不等 30min 默认超时
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
// 积分查询并行执行（缓存过期后的首个聚合请求不再串行等待所有账号，账号多时从 N×延迟降到单次最慢）。
func resolveAggregateClient(s *store.Store, timeout time.Duration, sharedTransport *http.Transport) *joycode.Client {
	threshold := s.GetIntSetting("points_rotate_threshold", 10)

	accounts, err := s.ListAccounts()
	if err != nil || len(accounts) == 0 {
		return nil
	}

	remainMap := parallelGetRemain(s, accounts)

	type cand struct {
		acc    store.AccountInfo
		remain int
	}
	var cands []cand
	for i := range accounts {
		remain := remainMap[accounts[i].UserID]
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
			r := remainMap[accounts[i].UserID]
			if r > bestRemain {
				bestRemain = r
				best = accounts[i]
			}
		}
		return newAggregateClient(s, best, timeout, sharedTransport)
	}

	// 可用账号间轮询
	idx := atomic.AddUint64(&rotateCounter, 1) % uint64(len(cands))
	return newAggregateClient(s, cands[idx].acc, timeout, sharedTransport)
}

// parallelGetRemain 并行查询所有账号的剩余积分（各自走缓存，仅缓存过期者真实请求上游）。
func parallelGetRemain(s *store.Store, accounts []store.AccountInfo) map[string]int {
	results := make(map[string]int, len(accounts))
	if len(accounts) == 0 {
		return results
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func(acc store.AccountInfo) {
			defer wg.Done()
			r := getAccountRemain(s, acc.UserID)
			mu.Lock()
			results[acc.UserID] = r
			mu.Unlock()
		}(accounts[i])
	}
	wg.Wait()
	return results
}

func newAggregateClient(s *store.Store, acc store.AccountInfo, timeout time.Duration, sharedTransport *http.Transport) *joycode.Client {
	account, err := s.GetAccount(acc.UserID)
	if err != nil || account == nil {
		return nil
	}
	// 复用缓存 client（同一账号共享 SessionID + 连接池），避免每次请求新建会话
	return cachedClient(account, timeout, sharedTransport)
}
