package checkin

// 数据类型：Account（内存/前端视图）、storedAccount（blob 持久化形态，
// 令牌加密为 enc_access/enc_refresh/enc_machine_token）、AccountInput
// （PUT 全量替换入参）、CheckinPoint、Result、QoderLoginResult、
// WBLoginResult、checkinState（checkin_state 状态）。
// JSON 标签全部取自 strings-notes §3 的二进制标签全集。

import (
	"log/slog"
)

// Account 是一个签到账号（内存形态，含解密后的令牌；对外 List 一律 Masked）。
type Account struct {
	ID            string  `json:"id"`                 // 前端生成 ci_${Date.now()}，后端兼容任意字符串
	Platform      string  `json:"platform"`           // workbuddy | traework | qoder
	Name          string  `json:"name,omitempty"`     // 备注名
	Nickname      string  `json:"nickname,omitempty"` // 登录昵称
	UID           string  `json:"uid,omitempty"`
	AccessToken   string  `json:"access_token,omitempty"`  // 加密存储，List 时打码
	RefreshToken  string  `json:"refresh_token,omitempty"` // 加密存储，List 时打码
	MachineToken  string  `json:"machine_token,omitempty"` // 加密存储，List 时打码
	DeviceKey     string  `json:"device_key,omitempty"`    // traework 设备 RSA 私钥 PEM（加密存储，List 时打码）
	EnterpriseID  string  `json:"enterprise_id,omitempty"` // workbuddy 可选
	Domain        string  `json:"domain,omitempty"`        // workbuddy 可选
	DeviceID      string  `json:"device_id,omitempty"`     // traework 必填（x-device-id）
	MachineID     string  `json:"machine_id,omitempty"`    // traework 可选（x-machine-id）
	MachineType   string  `json:"machine_type,omitempty"`
	APIHost       string  `json:"api_host,omitempty"`
	Enabled       bool    `json:"enabled"`
	Credits       float64 `json:"credits,omitempty"`
	CreditsTotal  float64 `json:"credits_total,omitempty"`
	LastCheckinAt string  `json:"last_checkin_at,omitempty"`
	LastCheckinOK bool    `json:"last_checkin_ok,omitempty"`
	LastResult    string  `json:"last_result,omitempty"`

	LastRefreshAt string `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool   `json:"last_refresh_ok,omitempty"`
}

// IDKey 返回账号主键（状态/去重/守卫的统一键）。
func (a *Account) IDKey() string {
	if a == nil {
		return ""
	}
	return a.ID
}

// Masked 返回打码副本（令牌清空，凭据不出服务端）。
func (a *Account) Masked() Account {
	if a == nil {
		return Account{}
	}
	v := *a
	v.AccessToken = ""
	v.RefreshToken = ""
	v.MachineToken = ""
	v.DeviceKey = ""
	return v
}

// clone 返回完整副本（运行期读写隔离）。
func (a *Account) clone() *Account {
	if a == nil {
		return nil
	}
	v := *a
	return &v
}

// storedAccount 是 blob 里的一条账号（明文凭据不落盘）。
// 字段标签对齐 workbuddy/qoder blob.go。
type storedAccount struct {
	ID           string `json:"id"`
	Platform     string `json:"platform"`
	Name         string `json:"name,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	UID          string `json:"uid,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Domain       string `json:"domain,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	MachineType  string `json:"machine_type,omitempty"`
	APIHost      string `json:"api_host,omitempty"`

	EncAccess       string `json:"enc_access,omitempty"`
	EncRefresh      string `json:"enc_refresh,omitempty"`
	EncMachineToken string `json:"enc_machine_token,omitempty"`
	EncDeviceKey    string `json:"enc_device_key,omitempty"`

	Enabled       bool    `json:"enabled"`
	Credits       float64 `json:"credits,omitempty"`
	CreditsTotal  float64 `json:"credits_total,omitempty"`
	LastCheckinAt string  `json:"last_checkin_at,omitempty"`
	LastCheckinOK bool    `json:"last_checkin_ok,omitempty"`
	LastResult    string  `json:"last_result,omitempty"`

	LastRefreshAt string `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool   `json:"last_refresh_ok,omitempty"`
	Note          string `json:"note,omitempty"`
}

// IDKey 返回账号主键。
func (s *storedAccount) IDKey() string {
	if s == nil {
		return ""
	}
	return s.ID
}

// Masked 返回打码副本（加密字段清空，用于日志/调试）。
func (s *storedAccount) Masked() storedAccount {
	if s == nil {
		return storedAccount{}
	}
	v := *s
	v.EncAccess = ""
	v.EncRefresh = ""
	v.EncMachineToken = ""
	v.EncDeviceKey = ""
	return v
}

// toStored 把内存账号转为持久化形态（令牌经 store 加密）。
func (m *Manager) toStored(a *Account) storedAccount {
	st := storedAccount{
		ID:            a.ID,
		Platform:      a.Platform,
		Name:          a.Name,
		Nickname:      a.Nickname,
		UID:           a.UID,
		EnterpriseID:  a.EnterpriseID,
		Domain:        a.Domain,
		DeviceID:      a.DeviceID,
		MachineID:     a.MachineID,
		MachineType:   a.MachineType,
		APIHost:       a.APIHost,
		Enabled:       a.Enabled,
		Credits:       a.Credits,
		CreditsTotal:  a.CreditsTotal,
		LastCheckinAt: a.LastCheckinAt,
		LastCheckinOK: a.LastCheckinOK,
		LastResult:    a.LastResult,
		LastRefreshAt: a.LastRefreshAt,
		LastRefreshOK: a.LastRefreshOK,
	}
	if enc, err := m.encryptField(a.AccessToken); err == nil {
		st.EncAccess = enc
	} else {
		logEncryptError(err)
	}
	if enc, err := m.encryptField(a.RefreshToken); err == nil {
		st.EncRefresh = enc
	} else {
		logEncryptError(err)
	}
	if enc, err := m.encryptField(a.MachineToken); err == nil {
		st.EncMachineToken = enc
	} else {
		logEncryptError(err)
	}
	if enc, err := m.encryptField(a.DeviceKey); err == nil {
		st.EncDeviceKey = enc
	} else {
		logEncryptError(err)
	}
	return st
}

// fromStored 把持久化形态还原为内存账号（令牌解密，失败容忍为空）。
func (m *Manager) fromStored(st storedAccount) *Account {
	return &Account{
		ID:            st.ID,
		Platform:      st.Platform,
		Name:          st.Name,
		Nickname:      st.Nickname,
		UID:           st.UID,
		AccessToken:   m.decryptField(st.EncAccess),
		RefreshToken:  m.decryptField(st.EncRefresh),
		MachineToken:  m.decryptField(st.EncMachineToken),
		DeviceKey:     m.decryptField(st.EncDeviceKey),
		EnterpriseID:  st.EnterpriseID,
		Domain:        st.Domain,
		DeviceID:      st.DeviceID,
		MachineID:     st.MachineID,
		MachineType:   st.MachineType,
		APIHost:       st.APIHost,
		Enabled:       st.Enabled,
		Credits:       st.Credits,
		CreditsTotal:  st.CreditsTotal,
		LastCheckinAt: st.LastCheckinAt,
		LastCheckinOK: st.LastCheckinOK,
		LastResult:    st.LastResult,
		LastRefreshAt: st.LastRefreshAt,
		LastRefreshOK: st.LastRefreshOK,
	}
}

// encryptField/decryptField 复用 store 的 AES-GCM（与 qoder/workbuddy blob
// 的密文格式一致：nonce 前置 + hex，密钥同 ~/.joycode-proxy/.enc_key）。
func (m *Manager) encryptField(plain string) (string, error) {
	if plain == "" || m.store == nil {
		return "", nil
	}
	return m.store.Encrypt(plain)
}

func (m *Manager) decryptField(enc string) string {
	if enc == "" || m.store == nil {
		return ""
	}
	plain, err := m.store.Decrypt(enc)
	if err != nil {
		return ""
	}
	return plain
}

// AccountInput 是 PUT /api/checkin/accounts 的单条入参（全量替换）。
// access_token/refresh_token 留空 = 保持原凭据不变（编辑语义）。
type AccountInput struct {
	ID           string `json:"id,omitempty"`
	Platform     string `json:"platform"`
	Name         string `json:"name,omitempty"`
	UID          string `json:"uid,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Domain       string `json:"domain,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	MachineType  string `json:"machine_type,omitempty"`
	APIHost      string `json:"api_host,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

// CheckinPoint 是 GET /api/checkin/points 的单条积分视图
// （Dashboard 与签到中心每 5 分钟轮询）。
type CheckinPoint struct {
	ID       string  `json:"id"`
	Name     string  `json:"name,omitempty"`
	Platform string  `json:"platform,omitempty"`
	Credits  float64 `json:"credits"`
	Total    float64 `json:"total"`
}

// Result 是一次签到/积分刷新的结果（POST /api/checkin/run 的 results 元素）。
type Result struct {
	ID      string  `json:"id"`
	Name    string  `json:"name,omitempty"`
	OK      bool    `json:"ok"`
	Message string  `json:"message"`
	Credits float64 `json:"credits,omitempty"`
}

// displayName 返回结果/卡片用的显示名（name || uid || id，对齐前端）。
func (a *Account) displayName() string {
	if a == nil {
		return ""
	}
	if a.Name != "" {
		return a.Name
	}
	if a.UID != "" {
		return a.UID
	}
	return a.ID
}

// QoderLoginResult 是 Qoder 设备流登录的发起/轮询结果：
//   - init: {session_id, auth_url, message}
//   - poll: {status, message, nickname, uid}（status ∈ ok/expired/error/其他=等待；
//     dashboard 据此组 {status, message, account:{nickname, uid}} 契约形状）。
type QoderLoginResult struct {
	SessionID string `json:"session_id,omitempty"`
	AuthURL   string `json:"auth_url,omitempty"`
	Status    string `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	UID       string `json:"uid,omitempty"`
}

// WBLoginResult 是 WorkBuddy 扫码登录的发起/轮询结果（形状同 QoderLoginResult）。
type WBLoginResult struct {
	SessionID string `json:"session_id,omitempty"`
	AuthURL   string `json:"auth_url,omitempty"`
	Status    string `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	UID       string `json:"uid,omitempty"`
}

// checkinState 是 checkin_state 键里单账号的运行状态。
type checkinState struct {
	LastCheckinAt string  `json:"last_checkin_at,omitempty"`
	LastCheckinOK bool    `json:"last_checkin_ok,omitempty"`
	Credits       float64 `json:"credits,omitempty"`
	LastResult    string  `json:"last_result,omitempty"`
}

// stateEntry 是 checkin_state 的单条目，形状与 v0.7.2 一致：
// 账号快照（无令牌）+ 状态字段合并，保证新旧二进制双向可读。
type stateEntry struct {
	ID            string  `json:"id,omitempty"`
	Platform      string  `json:"platform,omitempty"`
	Name          string  `json:"name,omitempty"`
	UID           string  `json:"uid,omitempty"`
	Enabled       bool    `json:"enabled,omitempty"`
	Credits       float64 `json:"credits,omitempty"`
	CreditsTotal  float64 `json:"credits_total,omitempty"`
	LastCheckinAt string  `json:"last_checkin_at,omitempty"`
	LastCheckinOK bool    `json:"last_checkin_ok,omitempty"`
	LastRefreshAt string  `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool    `json:"last_refresh_ok,omitempty"`
	LastResult    string  `json:"last_result,omitempty"`
}

// logEncryptError 统一记录加密失败（对齐 qoder/workbuddy blob.go）。
func logEncryptError(err error) {
	slog.Error("store: encrypt secret setting failed", "key", accountsKey, "error", err)
}
