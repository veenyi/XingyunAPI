// Package checkin 实现多平台每日自动签到（积分领取）。
// 首批支持 WorkBuddy(CodeBuddy) 与 TraeWork(Trae SOLO)，协议移植自 wild-work
// 实测实现：token 自动刷新 + 每日定时签到 + 积分查询。
// 凭据以 AES-GCM 加密存于 settings.checkin_accounts，绝不落日志。
package checkin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

const (
	SettingAccounts = "checkin_accounts" // 加密的账号凭据 JSON
	SettingState    = "checkin_state"    // 明文的运行状态 JSON
	SettingTimes    = "checkin_times"    // 每日签到时刻 ["09:00", ...]

	PlatformWorkBuddy = "workbuddy"
	PlatformTraeWork  = "traework"

	defaultRefreshAfter = 12 * time.Hour
)

// Account 是一条签到账号。凭据字段在持久化时整体走 store.Encrypt，
// 对前端只回显掩码。
type Account struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Name     string `json:"name"`
	UID      string `json:"uid"`

	// 凭据（加密存储，列表接口输出掩码）
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"` // workbuddy 租户
	Domain       string `json:"domain,omitempty"`        // workbuddy 部门
	DeviceID     string `json:"device_id,omitempty"`     // traework 设备指纹（签到强校验）
	MachineID    string `json:"machine_id,omitempty"`    // traework 机器号
	ApiHost      string `json:"api_host,omitempty"`      // traework OAuth host，留空用官方

	Enabled bool `json:"enabled"`

	// 运行状态（存 settings.checkin_state，明文无敏感信息）
	LastCheckinAt string `json:"last_checkin_at,omitempty"`
	LastCheckinOK bool   `json:"last_checkin_ok"`
	LastResult    string `json:"last_result,omitempty"`
	Credits       int64  `json:"credits,omitempty"`
	CreditsTotal  int64  `json:"credits_total,omitempty"`
	LastRefreshAt string `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool   `json:"last_refresh_ok"`
}

// Masked 返回给前端的副本：凭据替换为掩码。
func (a Account) Masked() Account {
	out := a
	if out.AccessToken != "" {
		out.AccessToken = store.SecretMask
	}
	if out.RefreshToken != "" {
		out.RefreshToken = store.SecretMask
	}
	return out
}

// Result 是一次签到的结果。
type Result struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Platform string `json:"platform"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Credits int64  `json:"credits,omitempty"`
}

// Manager 管理签到账号与后台调度。
type Manager struct {
	mu      sync.Mutex
	store   *store.Store
	version string
	stop    chan struct{}
	timer   *time.Timer
	running bool
	// runMu 串行化签到执行（含网络请求），与 m.mu 分离：
	// run 流程内要写状态（拿 m.mu），持 m.mu 跑网络请求会自锁。
	runMu  sync.Mutex
	// today 记录调度器已签到的日期（本地时区），防止同一天重复。
	today map[string]string
	// wbSessions 暂存进行中的 WorkBuddy 扫码登录会话（sessionID → state），带 TTL。
	wbSessions map[string]*wbLoginSession
}

func New(s *store.Store, version string) *Manager {
	return &Manager{store: s, version: version, today: map[string]string{}, wbSessions: map[string]*wbLoginSession{}}
}

// --- 账号 CRUD ---

// List 返回全部账号（凭据掩码），并合并运行状态。
func (m *Manager) List() []Account {
	accounts := m.loadAccounts()
	state := m.loadState()
	out := make([]Account, 0, len(accounts))
	for _, a := range accounts {
		if st, ok := state[a.ID]; ok {
			a.LastCheckinAt = st.LastCheckinAt
			a.LastCheckinOK = st.LastCheckinOK
			a.LastResult = st.LastResult
			a.Credits = st.Credits
			a.CreditsTotal = st.CreditsTotal
			a.LastRefreshAt = st.LastRefreshAt
			a.LastRefreshOK = st.LastRefreshOK
		}
		out = append(out, a.Masked())
	}
	return out
}

// AccountInput 是前端提交的账号表单。凭据为空或掩码时保留旧值。
type AccountInput struct {
	ID           string `json:"id"`
	Platform     string `json:"platform"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	EnterpriseID string `json:"enterprise_id"`
	Domain       string `json:"domain"`
	DeviceID     string `json:"device_id"`
	MachineID    string `json:"machine_id"`
	ApiHost      string `json:"api_host"`
	Enabled      bool   `json:"enabled"`
}

// Save 全量覆写账号列表（与 custom_providers 同一套交互模式）。
// 密文保持式：未改动的凭据原样保留旧密文（哪怕此刻解不开也不丢账号），
// 新给的凭据加密失败立即报错——绝不把明文 token 落库。
func (m *Manager) Save(inputs []AccountInput) error {
	if m.store == nil {
		return fmt.Errorf("store unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := map[string]storedAccount{}
	for _, sa := range m.loadStored() {
		old[sa.Account.ID] = sa
	}

	out := make([]storedAccount, 0, len(inputs))
	for _, in := range inputs {
		platform := strings.TrimSpace(strings.ToLower(in.Platform))
		if platform != PlatformWorkBuddy && platform != PlatformTraeWork {
			continue
		}
		base := old[in.ID]
		a := base.Account
		a.Platform = platform
		a.Enabled = in.Enabled
		if v := strings.TrimSpace(in.Name); v != "" {
			a.Name = v
		}
		if v := strings.TrimSpace(in.UID); v != "" {
			a.UID = v
		}
		if in.EnterpriseID != "" {
			a.EnterpriseID = strings.TrimSpace(in.EnterpriseID)
		}
		if in.Domain != "" {
			a.Domain = strings.TrimSpace(in.Domain)
		}
		if in.DeviceID != "" {
			a.DeviceID = strings.TrimSpace(in.DeviceID)
		}
		if in.MachineID != "" {
			a.MachineID = strings.TrimSpace(in.MachineID)
		}
		if in.ApiHost != "" {
			a.ApiHost = strings.TrimSpace(in.ApiHost)
		}
		if in.ID == "" {
			a.ID = fmt.Sprintf("ci_%d", time.Now().UnixNano())
		} else {
			a.ID = in.ID
		}
		if a.Name == "" {
			a.Name = a.UID
		}

		sa := storedAccount{Account: a}
		if tok := in.AccessToken; tok != "" && tok != store.SecretMask {
			enc, err := m.store.Encrypt(tok)
			if err != nil {
				return fmt.Errorf("加密 access token 失败: %w", err)
			}
			sa.EncAccess = enc
		} else {
			sa.EncAccess = base.EncAccess
		}
		if tok := in.RefreshToken; tok != "" && tok != store.SecretMask {
			enc, err := m.store.Encrypt(tok)
			if err != nil {
				return fmt.Errorf("加密 refresh token 失败: %w", err)
			}
			sa.EncRefresh = enc
		} else {
			sa.EncRefresh = base.EncRefresh
		}
		sa.Account.AccessToken = ""
		sa.Account.RefreshToken = ""
		if sa.EncAccess == "" && sa.EncRefresh == "" {
			continue // 没有任何凭据的账号没有意义
		}
		out = append(out, sa)
	}
	if err := m.saveStored(out); err != nil {
		return err
	}
	// 裁剪已删除账号的孤儿状态，防止 List 越积越多。
	state := m.loadState()
	alive := map[string]bool{}
	for _, sa := range out {
		alive[sa.Account.ID] = true
	}
	pruned := false
	for id := range state {
		if !alive[id] {
			delete(state, id)
			pruned = true
		}
	}
	if pruned {
		_ = m.saveState(state)
	}
	return nil
}

// Remove 删除账号并清理状态。
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.loadStored()
	out := make([]storedAccount, 0, len(stored))
	for _, sa := range stored {
		if sa.Account.ID != id {
			out = append(out, sa)
		}
	}
	if err := m.saveStored(out); err != nil {
		return err
	}
	state := m.loadState()
	delete(state, id)
	return m.saveState(state)
}

// Times 返回每日签到时刻（HH:MM 列表）。
func (m *Manager) Times() []string {
	raw := strings.TrimSpace(m.store.GetSetting(SettingTimes))
	if raw == "" {
		return []string{"09:00"}
	}
	var times []string
	if json.Unmarshal([]byte(raw), &times) != nil || len(times) == 0 {
		return []string{"09:00"}
	}
	return times
}

// SetTimes 覆写每日签到时刻。
func (m *Manager) SetTimes(times []string) error {
	cleaned := make([]string, 0, len(times))
	for _, t := range times {
		t = strings.TrimSpace(t)
		if len(t) == 5 && t[2] == ':' {
			cleaned = append(cleaned, t)
		}
	}
	if len(cleaned) == 0 {
		cleaned = []string{"09:00"}
	}
	data, _ := json.Marshal(cleaned)
	return m.store.SetSetting(SettingTimes, string(data))
}

// --- 存储 ---

type storedAccounts struct {
	Accounts []storedAccount `json:"accounts"`
}

// storedAccount 是加密存储形态：AccessToken/RefreshToken 各自整体加密。
type storedAccount struct {
	Account
	EncAccess  string `json:"enc_access,omitempty"`
	EncRefresh string `json:"enc_refresh,omitempty"`
}

// loadStored 读取原始存储形态（含密文，不解密）。
func (m *Manager) loadStored() []storedAccount {
	if m.store == nil {
		return nil
	}
	raw := strings.TrimSpace(m.store.GetSetting(SettingAccounts))
	if raw == "" {
		return nil
	}
	var stored storedAccounts
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		slog.Error("checkin: 解析账号失败", "error", err)
		return nil
	}
	return stored.Accounts
}

// loadAccounts 返回解密后的运行态账号。解密失败的账号仍保留（凭据置空），
// 下次 Save 会原样保留其密文——绝不因一次解密失败而丢账号。
func (m *Manager) loadAccounts() []Account {
	stored := m.loadStored()
	out := make([]Account, 0, len(stored))
	for _, sa := range stored {
		a := sa.Account
		if sa.EncAccess != "" {
			if dec, err := m.store.Decrypt(sa.EncAccess); err == nil {
				a.AccessToken = dec
			}
		}
		if sa.EncRefresh != "" {
			if dec, err := m.store.Decrypt(sa.EncRefresh); err == nil {
				a.RefreshToken = dec
			}
		}
		out = append(out, a)
	}
	return out
}

// saveStored 序列化并写入。调用方保证 token 已是密文（Enc*）且明文已清空。
func (m *Manager) saveStored(accounts []storedAccount) error {
	data, err := json.Marshal(storedAccounts{Accounts: accounts})
	if err != nil {
		return err
	}
	return m.store.SetSetting(SettingAccounts, string(data))
}

func (m *Manager) loadState() map[string]Account {
	raw := strings.TrimSpace(m.store.GetSetting(SettingState))
	out := map[string]Account{}
	if raw == "" {
		return out
	}
	var st map[string]Account
	if json.Unmarshal([]byte(raw), &st) == nil {
		return st
	}
	return out
}

func (m *Manager) saveState(state map[string]Account) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return m.store.SetSetting(SettingState, string(data))
}

// updateState 在签到/刷新后写回单个账号状态。
func (m *Manager) updateState(id string, fn func(*Account)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.loadState()
	st := state[id]
	fn(&st)
	state[id] = st
	if err := m.saveState(state); err != nil {
		slog.Warn("checkin: 保存状态失败", "id", id, "error", err)
	}
}
