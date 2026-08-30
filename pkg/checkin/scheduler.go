// 签到调度与执行：每日定时签到 + token 过期自动刷新 + 手动触发。
package checkin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// RunOne 手动触发单个账号：刷新 token（如需）→ 签到 → 查积分。
// 注意：m.mu 只保护账号/状态的读写，绝不能在持锁状态下执行网络请求——
// run() 内部的 updateState/persistCredentials 也要拿 m.mu，持锁跑签到会自锁。
func (m *Manager) RunOne(id string) Result {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	var target *Account
	m.mu.Lock()
	for _, a := range m.loadAccounts() {
		if a.ID == id {
			cp := a
			target = &cp
			break
		}
	}
	m.mu.Unlock()
	if target == nil {
		return Result{ID: id, OK: false, Message: "账号不存在"}
	}
	return m.run(target)
}

// RunAll 手动触发全部已启用账号。
func (m *Manager) RunAll() []Result {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	m.mu.Lock()
	var targets []Account
	for _, a := range m.loadAccounts() {
		if a.Enabled {
			targets = append(targets, a)
		}
	}
	m.mu.Unlock()
	var results []Result
	for i := range targets {
		results = append(results, m.run(&targets[i]))
	}
	return results
}

// run 执行一个账号的完整签到流程，并把状态写回。
func (m *Manager) run(a *Account) Result {
	res := Result{ID: a.ID, Name: a.Name, Platform: a.Platform}

	// 1. token 刷新：无 access token 或上次刷新超过 12h 时先刷一次。
	needRefresh := a.AccessToken == "" || m.refreshStale(a.ID)
	if needRefresh {
		var refreshErr error
		switch a.Platform {
		case PlatformWorkBuddy:
			refreshErr = wbRefresh(a)
		case PlatformTraeWork:
			refreshErr = twRefresh(a)
		}
		if refreshErr != nil {
			res.OK = false
			res.Message = refreshErr.Error()
			m.updateState(a.ID, func(st *Account) {
				st.LastRefreshAt = nowStr()
				st.LastRefreshOK = false
			})
			// 凭据可能已轮换，即使刷新失败也要把现有值存回去（refresh token 可能变化）
			m.persistCredentials(a)
			m.updateState(a.ID, func(st *Account) {
				st.LastCheckinAt = nowStr()
				st.LastCheckinOK = false
				st.LastResult = res.Message
			})
			slog.Warn("checkin: token 刷新失败", "platform", a.Platform, "name", a.Name, "error", refreshErr)
			return res
		}
		m.persistCredentials(a)
		m.updateState(a.ID, func(st *Account) {
			st.LastRefreshAt = nowStr()
			st.LastRefreshOK = true
		})
	}

	// 2. 签到
	var checkinErr error
	switch a.Platform {
	case PlatformWorkBuddy:
		checkinErr = wbCheckin(a)
	case PlatformTraeWork:
		checkinErr = twCheckin(a)
	}

	// 3. 积分查询（签到成功或已签到都查）
	if checkinErr == nil || strings.Contains(checkinErr.Error(), "已签到") {
		if remain, total, err := m.credits(a); err == nil {
			res.Credits = remain
			m.updateState(a.ID, func(st *Account) {
				st.Credits = remain
				st.CreditsTotal = total
			})
		}
	}

	if checkinErr != nil {
		res.OK = false
		res.Message = checkinErr.Error()
	} else {
		res.OK = true
		res.Message = "签到成功"
		res.Credits = m.stateCredits(a.ID)
	}
	m.updateState(a.ID, func(st *Account) {
		st.LastCheckinAt = nowStr()
		st.LastCheckinOK = res.OK
		st.LastResult = res.Message
	})
	if res.OK {
		slog.Info("checkin: 签到成功", "platform", a.Platform, "name", a.Name, "credits", res.Credits)
	} else {
		slog.Info("checkin: 签到未完成", "platform", a.Platform, "name", a.Name, "reason", res.Message)
	}
	return res
}

func (m *Manager) credits(a *Account) (remain, total int64, err error) {
	switch a.Platform {
	case PlatformWorkBuddy:
		return wbCredits(a)
	case PlatformTraeWork:
		return twCredits(a)
	}
	return 0, 0, fmt.Errorf("未知平台 %s", a.Platform)
}

func (m *Manager) stateCredits(id string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.loadState()[id]; ok {
		return st.Credits
	}
	return 0
}

// persistCredentials 把（可能轮换过的）凭据写回存储。
// 持 m.mu 防止与 Save 全量覆写竞争导致丢账号。
func (m *Manager) persistCredentials(a *Account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	accounts := m.loadAccounts()
	for i := range accounts {
		if accounts[i].ID == a.ID {
			accounts[i].AccessToken = a.AccessToken
			accounts[i].RefreshToken = a.RefreshToken
			accounts[i].Domain = a.Domain
			_ = m.saveAccounts(accounts)
			return
		}
	}
}

// refreshStale 判断距上次成功刷新是否超过 12h。
func (m *Manager) refreshStale(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.loadState()[id]
	if !ok {
		return true
	}
	if !st.LastRefreshOK || st.LastRefreshAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, st.LastRefreshAt)
	if err != nil {
		return true
	}
	return time.Since(t) > defaultRefreshAfter
}

// IDKey 便于状态映射（state map 的 key 就是账号 ID，此方法仅为模板兼容）。
func (a *Account) IDKey() string { return a.ID }

func nowStr() string { return time.Now().Format(time.RFC3339) }

// --- 后台调度 ---

// Start 拉起后台调度：每分钟醒来检查是否到达签到时刻；
// 每个时刻每天只签一次（按本地日期去重）。
func (m *Manager) Start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.stop = make(chan struct{})
	stop := m.stop
	m.mu.Unlock()

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.tick()
			}
		}
	}()
	slog.Info("checkin: 后台调度已启动", "times", m.Times())
}

// Stop 停止后台调度。
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running && m.stop != nil {
		close(m.stop)
	}
	m.running = false
}

// tick 检查当前 HH:MM 是否在签到时刻表里；是且今天没签过就跑一轮。
func (m *Manager) tick() {
	m.mu.Lock()
	now := time.Now()
	hm := now.Format("15:04")
	today := now.Format("2006-01-02")
	due := false
	for _, t := range m.Times() {
		if t != hm {
			continue
		}
		if m.today[t] == today {
			continue // 这个时刻今天已跑过
		}
		m.today[t] = today
		due = true
	}
	m.mu.Unlock()
	if !due {
		return
	}
	slog.Info("checkin: 到达定时时刻，开始自动签到", "time", hm)
	for _, res := range m.RunAll() {
		if !res.OK && !strings.Contains(res.Message, "已签到") {
			slog.Warn("checkin: 自动签到失败", "name", res.Name, "reason", res.Message)
		}
	}
}

var _ = json.Marshal
