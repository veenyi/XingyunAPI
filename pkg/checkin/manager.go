package checkin

// Manager：多平台自动签到管理器。
//   - 生命周期 Start/Stop：后台调度循环，每个时刻每天各跑一次（runGuard 守卫，
//     重启后按账号 last_checkin_at 去重补跑）。
//   - SetTimes/Times：checkin_times 时刻表（默认 ["09:00"]，HH:mm）。
//   - RunAll/RunOne：手动/定时执行入口（qoder 平台 = 只刷积分）。
//   - List/ListPoints/Save/Remove：账号视图与全量替换 CRUD。
//   - 内部：loadAccounts/loadStored/saveStored（blob 读写）、loadState/saveState/
//     updateState/stateCredits（checkin_state）、refreshStale/persistCredentials
//     （token 保活）、credits/creditsFloat64（额度读取与宽容换算）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/qoder"
	"github.com/veenyi/XingyunAPI/pkg/store"
)

// Manager 是签到中心的后台管理器（dashboard 持有一份）。
type Manager struct {
	mu    sync.Mutex
	store *store.Store // nil 容忍：仅内存运行
	hc    *http.Client

	accounts []*Account               // blob 的内存形态（含解密令牌）
	state    map[string]*checkinState // checkin_state
	times    []string                 // checkin_times
	runGuard map[string]string        // 时刻 -> 最近一次触发的日期（每天一次）

	stateEntries map[string]*stateEntry // checkin_state 原始条目（v0.7.2 兼容形状，保存时拼回）

	qoderLogin *qoder.LoginManager
	wbSessions map[string]*wbLoginSession
	twSessions map[string]*twLoginSession

	started bool
	stopCh  chan struct{}
	done    chan struct{}
}

// NewManager 构造签到管理器（st 可为 nil，则仅内存运行；hc 为 nil 用默认客户端）。
func NewManager(st *store.Store, hc *http.Client) *Manager {
	if hc == nil {
		hc = checkinHTTP
	}
	return &Manager{
		store:      st,
		hc:         hc,
		state:      map[string]*checkinState{},
		runGuard:   map[string]string{},
		wbSessions: map[string]*wbLoginSession{},
		twSessions: map[string]*twLoginSession{},
		times:      append([]string(nil), defaultTimes...),
	}
}

// Start 启动后台调度（幂等）：加载账号/状态/时刻表并进入 tick 循环。
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.stopCh = make(chan struct{})
	m.done = make(chan struct{})
	if m.hc == nil {
		m.hc = checkinHTTP
	}
	if m.wbSessions == nil {
		m.wbSessions = map[string]*wbLoginSession{}
	}
	if m.twSessions == nil {
		m.twSessions = map[string]*twLoginSession{}
	}
	if m.runGuard == nil {
		m.runGuard = map[string]string{}
	}
	m.mu.Unlock()

	m.loadAccounts()
	m.loadState()
	m.loadTimes()

	go m.loop()
	slog.Info("checkin: 后台调度已启动", "times", strings.Join(m.Times(), ","))
}

// Stop 停止后台调度（等待循环退出；已开始的执行会自然结束）。
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false
	stopCh, done := m.stopCh, m.done
	m.mu.Unlock()
	if stopCh != nil {
		close(stopCh)
	}
	if done != nil {
		<-done
	}
}

// loop 是调度主循环：每 30s tick 一次。
func (m *Manager) loop() {
	defer close(m.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case now := <-ticker.C:
			m.tick(now)
		}
	}
}

// tick 检查是否到达签到时刻：每个时刻每天最多触发一次；
// 启动晚于时刻时按当天补跑（账号级去重由 run 完成）。
func (m *Manager) tick(now time.Time) {
	for _, t := range m.Times() {
		sched, err := time.ParseInLocation("15:04", t, now.Location())
		if err != nil {
			continue
		}
		at := time.Date(now.Year(), now.Month(), now.Day(), sched.Hour(), sched.Minute(), 0, 0, now.Location())
		if now.Before(at) {
			continue
		}
		key, day := t, todayStr(now)
		m.mu.Lock()
		if m.runGuard[key] == day {
			m.mu.Unlock()
			continue
		}
		m.runGuard[key] = day
		m.mu.Unlock()
		slog.Info(textScheduleFired, "time", t)
		// 批量执行不套 60s 超时：单请求超时由 checkinHTTP 兜底
		for _, r := range m.RunAll(context.Background()) {
			if !r.OK && !containsFold(r.Message, []string{textCheckedToday, textCheckedIn, textDisabled}) {
				slog.Error("checkin: 自动签到失败", "id", r.ID, "name", r.Name, "message", r.Message)
			}
		}
	}
}

// SetTimes 更新签到时刻表（HH:mm，非法项丢弃，去重排序）并持久化。
func (m *Manager) SetTimes(times []string) error {
	normalized := normalizeTimes(times)
	if len(normalized) == 0 {
		normalized = append(normalized, defaultTimes...)
	}
	m.mu.Lock()
	m.times = normalized
	err := m.setSetting(timesKey, mustJSON(normalized))
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("保存签到时刻失败: %w", err)
	}
	return nil
}

// Times 返回当前时刻表副本（默认 ["09:00"]）。
func (m *Manager) Times() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.times) == 0 {
		return append([]string(nil), defaultTimes...)
	}
	return append([]string(nil), m.times...)
}

// loadTimes 从 settings 读取时刻表（缺省/损坏回退默认）。
func (m *Manager) loadTimes() {
	raw := m.getSetting(timesKey)
	if strings.TrimSpace(raw) == "" {
		return
	}
	var times []string
	if err := json.Unmarshal([]byte(raw), &times); err != nil {
		slog.Warn("checkin: 解析签到时刻失败", "error", err)
		return
	}
	normalized := normalizeTimes(times)
	if len(normalized) > 0 {
		m.mu.Lock()
		m.times = normalized
		m.mu.Unlock()
	}
}

// RunAll 执行全部启用账号的签到（qoder 平台 = 刷新积分）。
// 定时调度与手动「全部签到」共用。
func (m *Manager) RunAll(ctx context.Context) []Result {
	m.refreshStale(ctx)
	m.mu.Lock()
	snapshot := make([]*Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		if a != nil {
			snapshot = append(snapshot, a.clone())
		}
	}
	m.mu.Unlock()
	out := make([]Result, 0, len(snapshot))
	for _, a := range snapshot {
		out = append(out, m.run(ctx, a))
	}
	return out
}

// RunOne 执行单个账号的签到/积分刷新；账号不存在时报错。
func (m *Manager) RunOne(ctx context.Context, id string) (*Result, error) {
	m.mu.Lock()
	var target *Account
	for _, a := range m.accounts {
		if a != nil && a.ID == id {
			target = a.clone()
			break
		}
	}
	m.mu.Unlock()
	if target == nil {
		return nil, errors.New("账号不存在")
	}
	m.refreshStale(ctx)
	res := m.run(ctx, target)
	return &res, nil
}

// run 执行单账号：平台分派 + 状态更新 + 结果组装。
// 当日已签到（状态去重）时不再请求上游，返回「今日已签到」。
func (m *Manager) run(ctx context.Context, a *Account) Result {
	res := Result{ID: a.IDKey(), Name: a.displayName()}
	if !a.Enabled {
		res.OK = false
		res.Message = textDisabled
		return res
	}
	if a.Platform != platformQoder && m.checkedToday(a.IDKey()) {
		res.Message = textCheckedToday
		res.Credits = m.credits(a)
		return res
	}

	var message string
	var err error
	var credits, total float64
	switch a.Platform {
	case platformWorkBuddy:
		message, err = m.wbCheckin(ctx, a)
		if err == nil {
			// 积分查询尽力而为：查询失败不推翻签到成功
			credits, total, err = m.wbCreditsF64(ctx, a)
			if err != nil {
				slog.Error("checkin: 查询积分失败", "id", a.IDKey(), "error", err)
				credits, total, err = 0, 0, nil
			}
		}
	case platformTraeWork:
		message, err = m.twCheckin(ctx, a)
		if err == nil {
			credits, total, err = m.twCreditsF64(ctx, a)
			if err != nil {
				slog.Error("checkin: 查询积分失败", "id", a.IDKey(), "error", err)
				credits, total, err = 0, 0, nil
			}
		}
	case platformQoder:
		credits, total, err = m.qoderCreditsF64(ctx, a)
		if err == nil {
			message = "积分已刷新"
		} else {
			slog.Error("checkin: 查询积分失败", "id", a.IDKey(), "error", err)
		}
	case platformQwenWork:
		credits, total, err = m.qwCreditsF64(ctx, a)
		if err == nil {
			message = "积分已刷新"
		} else {
			slog.Error("checkin: 查询积分失败", "id", a.IDKey(), "error", err)
		}
	default:
		err = fmt.Errorf("不支持的平台: %s", a.Platform)
	}

	switch {
	case errors.Is(err, errAlreadyCheckedIn):
		// 已签到不算失败：状态保持 ok，结果走前端 info 分支
		m.updateState(a.IDKey(), true, textCheckedToday, 0)
		res.Message = textCheckedToday
		res.Credits = m.credits(a)
		return res
	case err != nil:
		slog.Error("checkin: 自动签到失败", "id", a.IDKey(), "error", err)
		m.updateState(a.IDKey(), false, err.Error(), 0)
		res.Message = err.Error()
		return res
	}

	m.updateState(a.IDKey(), true, message, credits)
	m.storeCredits(a.IDKey(), credits, total)
	res.OK = true
	res.Message = message
	res.Credits = credits
	if res.Credits == 0 {
		res.Credits = m.credits(a)
	}
	return res
}

// checkedToday 报告账号今天是否已成功签到（本地状态去重）。
func (m *Manager) checkedToday(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[id]
	if st == nil || !st.LastCheckinOK || st.LastCheckinAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, st.LastCheckinAt)
	if err != nil {
		return false
	}
	return todayStr(t) == todayStr(time.Now())
}

// credits 返回账号当前额度：优先 checkin_state，其次账号快照。
func (m *Manager) credits(a *Account) float64 {
	if a == nil {
		return 0
	}
	if v := m.stateCredits(a.IDKey()); v > 0 {
		return v
	}
	return a.Credits
}

// stateCredits 读取 checkin_state 中的额度。
func (m *Manager) stateCredits(id string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.state[id]; st != nil {
		return st.Credits
	}
	return 0
}

// updateState 写入签到状态并持久化（保存失败只记录，不影响执行结果）。
func (m *Manager) updateState(id string, ok bool, message string, credits float64) {
	if id == "" {
		return
	}
	m.mu.Lock()
	st := m.state[id]
	if st == nil {
		st = &checkinState{}
		m.state[id] = st
	}
	st.LastCheckinAt = nowStr()
	st.LastCheckinOK = ok
	if message != "" {
		st.LastResult = message
	}
	if credits > 0 {
		st.Credits = credits
	}
	err := m.saveStateLocked()
	m.mu.Unlock()
	if err != nil {
		slog.Error("checkin: 保存状态失败", "id", id, "error", err)
	}
}

// storeCredits 把最新额度同步进状态与账号快照（blob 随下次保存落盘）。
func (m *Manager) storeCredits(id string, credits, total float64) {
	if id == "" || (credits <= 0 && total <= 0) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if credits > 0 {
		if st := m.state[id]; st != nil {
			st.Credits = credits
		}
	}
	for _, a := range m.accounts {
		if a != nil && a.ID == id {
			if credits > 0 {
				a.Credits = credits
			}
			if total > 0 {
				a.CreditsTotal = total
			}
			break
		}
	}
}

// loadState 从 checkin_state 读取运行状态（损坏容忍为空）。
func (m *Manager) loadState() {
	raw := m.getSetting(stateKey)
	if strings.TrimSpace(raw) == "" {
		return
	}
	var parsed map[string]*stateEntry
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		slog.Warn("checkin: 解析签到状态失败", "error", err)
		return
	}
	st := make(map[string]*checkinState, len(parsed))
	for id, e := range parsed {
		if e == nil {
			continue
		}
		st[id] = &checkinState{
			LastCheckinAt: e.LastCheckinAt,
			LastCheckinOK: e.LastCheckinOK,
			Credits:       e.Credits,
			LastResult:    e.LastResult,
		}
	}
	m.mu.Lock()
	m.state = st
	m.stateEntries = parsed
	m.mu.Unlock()
}

// saveState 持久化 checkin_state（自带加锁，可在任意处调用）。
func (m *Manager) saveState() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveStateLocked()
}

// saveStateLocked 持久化 checkin_state（须持 m.mu）。
func (m *Manager) saveStateLocked() error {
	if m.store == nil {
		return nil
	}
	// 写 v0.7.2 兼容形状：条目 = 账号快照（无令牌）+ 状态字段
	out := make(map[string]*stateEntry, len(m.stateEntries)+len(m.accounts))
	for id, e := range m.stateEntries {
		if e != nil {
			out[id] = e
		}
	}
	for _, a := range m.accounts {
		if a == nil || a.ID == "" {
			continue
		}
		e := out[a.ID]
		if e == nil {
			e = &stateEntry{}
			out[a.ID] = e
		}
		e.ID = a.ID
		e.Platform = a.Platform
		e.Name = a.Name
		e.UID = a.UID
		e.Enabled = a.Enabled
		e.LastRefreshAt = a.LastRefreshAt
		e.LastRefreshOK = a.LastRefreshOK
		if a.Credits > 0 {
			e.Credits = a.Credits
		}
		if a.CreditsTotal > 0 {
			e.CreditsTotal = a.CreditsTotal
		}
		if st := m.state[a.ID]; st != nil {
			e.LastCheckinAt = st.LastCheckinAt
			e.LastCheckinOK = st.LastCheckinOK
			e.LastResult = st.LastResult
			if st.Credits > 0 {
				e.Credits = st.Credits
			}
		}
	}
	blob, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return m.setSetting(stateKey, string(blob))
}

// List 返回全部签到账号（打码令牌 + 合并运行状态）。
func (m *Manager) List() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		if a == nil {
			continue
		}
		v := a.Masked()
		if st := m.state[a.ID]; st != nil {
			if st.Credits > 0 {
				v.Credits = st.Credits
			}
			v.LastCheckinAt = st.LastCheckinAt
			v.LastCheckinOK = st.LastCheckinOK
			v.LastResult = st.LastResult
		}
		out = append(out, v)
	}
	return out
}

// ListPoints 查询全部启用账号的最新积分（每 5 分钟被前端轮询；
// 单账号失败回退缓存值并记日志）。
func (m *Manager) ListPoints(ctx context.Context) []CheckinPoint {
	m.mu.Lock()
	snapshot := make([]*Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		if a != nil && a.Enabled {
			snapshot = append(snapshot, a.clone())
		}
	}
	m.mu.Unlock()
	out := make([]CheckinPoint, 0, len(snapshot))
	for _, a := range snapshot {
		var credits, total float64
		var err error
		switch a.Platform {
		case platformWorkBuddy:
			credits, total, err = m.wbCreditsF64(ctx, a)
		case platformTraeWork:
			credits, total, err = m.twCreditsF64(ctx, a)
		case platformQoder:
			credits, total, err = m.qoderCreditsF64(ctx, a)
		case platformQwenWork:
			credits, total, err = m.qwCreditsF64(ctx, a)
		}
		if err != nil {
			slog.Error("checkin: 积分查询失败", "id", a.IDKey(), "error", err)
			credits, total = m.credits(a), a.CreditsTotal
		} else {
			m.storeCredits(a.IDKey(), credits, total)
		}
		out = append(out, CheckinPoint{
			ID:       a.ID,
			Name:     a.displayName(),
			Platform: a.Platform,
			Credits:  credits,
			Total:    total,
		})
	}
	return out
}

// Save 全量替换账号列表（PUT {accounts: [...]}）：
//   - id 为空时生成 ci_ 前缀随机 id；
//   - access_token/refresh_token 留空 = 保持原凭据（编辑「留空保持不变」）；
//   - enabled 缺省时沿用原值（新账号默认启用）；
//   - 被移除账号的运行状态一并清理。
func (m *Manager) Save(inputs []AccountInput) error {
	m.mu.Lock()
	prev := map[string]*Account{}
	for _, a := range m.accounts {
		if a != nil {
			prev[a.ID] = a
		}
	}
	next := make([]*Account, 0, len(inputs))
	seen := map[string]bool{}
	for _, in := range inputs {
		id := strings.TrimSpace(in.ID)
		if id == "" {
			id = "ci_" + randHex(8)
		}
		if seen[id] {
			m.mu.Unlock()
			return fmt.Errorf("签到账号 id 重复: %s", id)
		}
		seen[id] = true
		if err := validPlatform(in.Platform); err != nil {
			m.mu.Unlock()
			return err
		}
		a := &Account{
			ID:           id,
			Platform:     in.Platform,
			Name:         in.Name,
			UID:          strings.TrimSpace(in.UID),
			AccessToken:  in.AccessToken,
			RefreshToken: in.RefreshToken,
			EnterpriseID: in.EnterpriseID,
			Domain:       in.Domain,
			DeviceID:     strings.TrimSpace(in.DeviceID),
			MachineID:    strings.TrimSpace(in.MachineID),
			MachineType:  in.MachineType,
			APIHost:      in.APIHost,
		}
		if p, ok := prev[id]; ok {
			// 留空保持不变 + 沿用登录元数据
			if a.AccessToken == "" {
				a.AccessToken = p.AccessToken
			}
			if a.RefreshToken == "" {
				a.RefreshToken = p.RefreshToken
			}
			// 设备私钥不经 API 入参（登录流写入），全量替换时恒保留
			a.DeviceKey = p.DeviceKey
			if a.Nickname == "" {
				a.Nickname = p.Nickname
			}
			a.LastRefreshAt = p.LastRefreshAt
			a.LastRefreshOK = p.LastRefreshOK
		}
		switch {
		case in.Enabled != nil:
			a.Enabled = *in.Enabled
		case prev[id] != nil:
			a.Enabled = prev[id].Enabled
		default:
			a.Enabled = true
		}
		if st := m.state[id]; st != nil {
			a.Credits, a.CreditsTotal = st.Credits, prevCreditsTotal(prev[id], a)
		}
		next = append(next, a)
	}
	// 清理被移除账号的状态
	for id := range m.state {
		if !seen[id] {
			delete(m.state, id)
		}
	}
	m.accounts = next
	err := m.saveStored()
	m.mu.Unlock()
	if err != nil {
		slog.Error("checkin: 保存账号失败", "error", err)
		return err
	}
	_ = m.saveState()
	return nil
}

// prevCreditsTotal 保留原账号的总量快照（入参不带 credits_total）。
func prevCreditsTotal(prev, cur *Account) float64 {
	if prev != nil && prev.CreditsTotal > 0 {
		return prev.CreditsTotal
	}
	return cur.CreditsTotal
}

// Remove 删除单个签到账号（含运行状态）。
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	next := make([]*Account, 0, len(m.accounts))
	removed := false
	for _, a := range m.accounts {
		if a != nil && a.ID == id {
			removed = true
			continue
		}
		next = append(next, a)
	}
	if !removed {
		m.mu.Unlock()
		return errors.New("账号不存在")
	}
	m.accounts = next
	delete(m.state, id)
	err := m.saveStored()
	m.mu.Unlock()
	if err != nil {
		slog.Error("checkin: 删除账号失败", "id", id, "error", err)
		return err
	}
	_ = m.saveState()
	return nil
}

// validPlatform 校验平台标识。
func validPlatform(p string) error {
	switch p {
	case platformWorkBuddy, platformTraeWork, platformQoder, platformQwenWork:
		return nil
	case "":
		return errors.New("缺少 platform")
	default:
		return fmt.Errorf("不支持的平台: %s", p)
	}
}

// loadAccounts 从 blob 加载账号（解密令牌）到内存；解析失败保留现有内存。
func (m *Manager) loadAccounts() {
	stored, err := m.loadStored()
	if err != nil {
		slog.Error("checkin: 解析账号失败", "error", err)
		return
	}
	accounts := make([]*Account, 0, len(stored))
	for _, st := range stored {
		if st.ID == "" {
			continue
		}
		accounts = append(accounts, m.fromStored(st))
	}
	m.mu.Lock()
	m.accounts = accounts
	m.mu.Unlock()
}

// loadStored 读取并解析账号 blob（密文原样返回）。
// 兼容 v0.7.2 的 {"accounts":[...]} 包裹格式与裸数组格式。
func (m *Manager) loadStored() ([]storedAccount, error) {
	raw := strings.TrimSpace(m.getSetting(accountsKey))
	if raw == "" {
		return nil, nil
	}
	var stored []storedAccount
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		var wrapped struct {
			Accounts []storedAccount `json:"accounts"`
		}
		if err2 := json.Unmarshal([]byte(raw), &wrapped); err2 != nil {
			return nil, err
		}
		stored = wrapped.Accounts
	}
	return stored, nil
}

// saveStored 把内存账号加密写回 blob（须持 m.mu）。
// 写 v0.7.2 的 {"accounts":[...]} 包裹格式；条目字段为旧版超集，
// 旧版解析时忽略未知字段，保证双向回滚兼容。
func (m *Manager) saveStored() error {
	if m.store == nil {
		return nil
	}
	items := make([]storedAccount, 0, len(m.accounts))
	for _, a := range m.accounts {
		if a == nil || a.ID == "" {
			continue
		}
		items = append(items, m.toStored(a))
	}
	return m.setSetting(accountsKey, mustJSON(map[string]interface{}{"accounts": items}))
}

// persistCredentials 把刷新后的令牌写回内存与 blob。
func (m *Manager) persistCredentials(a *Account) {
	if a == nil || a.ID == "" || a.Platform == platformQoder {
		return // qoder 凭据由 qoderRefresh 写 qoder_accounts blob
	}
	m.mu.Lock()
	for _, cur := range m.accounts {
		if cur != nil && cur.ID == a.ID {
			cur.AccessToken = a.AccessToken
			cur.RefreshToken = a.RefreshToken
			cur.MachineToken = a.MachineToken
			cur.LastRefreshAt = a.LastRefreshAt
			cur.LastRefreshOK = a.LastRefreshOK
			break
		}
	}
	err := m.saveStored()
	m.mu.Unlock()
	if err != nil {
		slog.Error("checkin: 保存账号失败", "id", a.ID, "error", err)
	}
}

// refreshStale 刷新令牌过期的账号（仅 workbuddy/traework；
// qoder 由 qoderCredits 内部按 NeedsRefresh 处理）。
// 单账号失败仅告警并沿用现有 access token 继续签到。
func (m *Manager) refreshStale(ctx context.Context) {
	m.mu.Lock()
	var stale []*Account
	for _, a := range m.accounts {
		if a == nil || !a.Enabled || a.Platform == platformQoder {
			continue
		}
		if a.RefreshToken == "" {
			continue
		}
		if tokenStale(a) {
			stale = append(stale, a.clone())
		}
	}
	m.mu.Unlock()
	for _, a := range stale {
		var err error
		switch a.Platform {
		case platformWorkBuddy:
			err = m.wbRefresh(ctx, a)
		case platformTraeWork:
			err = m.twRefresh(ctx, a)
		}
		if err != nil {
			slog.Warn("checkin: token 刷新失败，改用现有 access token 继续签到", "id", a.ID, "error", err)
			continue
		}
		slog.Info("checkin: token 刷新成功", "id", a.ID, "platform", a.Platform)
		m.persistCredentials(a)
	}
}

// tokenStale 报告令牌是否需要刷新（时间驱动，失败不立即重试避免打爆上游）。
func tokenStale(a *Account) bool {
	if a.RefreshToken == "" {
		return false
	}
	if a.AccessToken == "" {
		return true
	}
	if a.LastRefreshAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, a.LastRefreshAt)
	if err != nil {
		return true
	}
	return time.Since(t) > tokenRefreshInterval
}

// ---- settings 访问（nil store 容忍）----

// hcOr 返回可用的 HTTP 客户端（m.hc 优先，nil 回退 fallback）。
func (m *Manager) hcOr(fallback *http.Client) *http.Client {
	if m.hc != nil {
		return m.hc
	}
	return fallback
}

func (m *Manager) getSetting(key string) string {
	if m.store == nil {
		return ""
	}
	return m.store.GetSetting(key)
}

func (m *Manager) setSetting(key, value string) error {
	if m.store == nil {
		return nil
	}
	return m.store.SetSetting(key, value)
}

// mustJSON 序列化（失败返回 "null"，调用方均为可序列化结构）。
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
