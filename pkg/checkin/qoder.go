// Qoder CN 平台：token 保活（刷新）+ 积分查询 + 设备流登录。
// Qoder 无签到活动，跳过签到步骤。
package checkin

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/qoder"
)

const qoderLoginSessionTTL = 10 * time.Minute

// qoderLoginSession 是一次进行中的 Qoder 设备流登录会话。
type qoderLoginSession struct {
	lm        *qoder.LoginManager
	sessionID string
	machineID string
	createdAt time.Time
}

// QoderLoginResult 是 Qoder 设备流登录成功后的账号信息。
type QoderLoginResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
}

// QoderLoginStart 发起 Qoder 设备流登录，返回会话 ID 与授权 URL。
func (m *Manager) QoderLoginStart() (sessionID, authURL string, err error) {
	lm := qoder.NewLoginManager()
	sid, url, err := lm.Start()
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	m.pruneQoderSessionsLocked()
	m.qoderSessions[sid] = &qoderLoginSession{
		lm:        lm,
		sessionID: sid,
		createdAt: time.Now(),
	}
	m.mu.Unlock()
	slog.Info("checkin: Qoder 设备流登录会话已创建", "session", sid)
	return sid, url, nil
}

// QoderLoginPoll 轮询 Qoder 登录状态。
func (m *Manager) QoderLoginPoll(sessionID string) (status string, acct *QoderLoginResult, msg string) {
	m.mu.Lock()
	sess := m.qoderSessions[sessionID]
	m.mu.Unlock()
	if sess == nil {
		return "expired", nil, "登录会话不存在或已过期"
	}
	if time.Since(sess.createdAt) > qoderLoginSessionTTL {
		m.mu.Lock()
		delete(m.qoderSessions, sessionID)
		m.mu.Unlock()
		return "expired", nil, "登录已过期，请重新获取"
	}

	result, machineID, err := sess.lm.Poll(sess.sessionID)
	if err != nil {
		if err == qoder.ErrPending {
			return "pending", nil, ""
		}
		return "pending", nil, err.Error()
	}
	if result.AccessToken == "" {
		return "pending", nil, ""
	}

	qa := &qoder.QoderAccount{
		UID:          result.UID,
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		MachineID:    machineID,
	}
	qoder.EnsureFingerprint(qa)

	if err := m.addQoderAccount(qa, result); err != nil {
		slog.Error("checkin: 保存 Qoder 登录账号失败", "uid", result.UID, "error", err)
		return "error", nil, "保存账号失败: " + err.Error()
	}
	m.mu.Lock()
	delete(m.qoderSessions, sessionID)
	m.mu.Unlock()
	slog.Info("checkin: Qoder 设备流登录成功", "uid", result.UID)
	return "ok", &QoderLoginResult{UID: result.UID, Nickname: result.Nickname}, "登录成功"
}

// addQoderAccount 把设备流拿到的凭据加密入库；同 UID 的 Qoder 账号则更新凭据。
func (m *Manager) addQoderAccount(qa *qoder.QoderAccount, res qoder.LoginResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.loadStored()

	encA, err := m.store.Encrypt(qa.AccessToken)
	if err != nil {
		return fmt.Errorf("加密 access token: %w", err)
	}
	encR := ""
	if qa.RefreshToken != "" {
		if encR, err = m.store.Encrypt(qa.RefreshToken); err != nil {
			return fmt.Errorf("加密 refresh token: %w", err)
		}
	}
	encM := ""
	if qa.MachineToken != "" {
		if encM, err = m.store.Encrypt(qa.MachineToken); err != nil {
			return fmt.Errorf("加密 machine token: %w", err)
		}
	}

	name := res.Nickname
	if name == "" {
		name = qa.UID
	}

	for i := range stored {
		if stored[i].Account.Platform == PlatformQoder && stored[i].Account.UID == qa.UID {
			stored[i].EncAccess = encA
			if encR != "" {
				stored[i].EncRefresh = encR
			}
			if encM != "" {
				stored[i].EncMachineToken = encM
			}
			stored[i].Account.MachineID = qa.MachineID
			stored[i].Account.MachineType = qa.MachineType
			if name != "" {
				stored[i].Account.Name = name
			}
			if res.Nickname != "" {
				stored[i].Account.Nickname = res.Nickname
			}
			stored[i].Account.AccessToken = ""
			stored[i].Account.RefreshToken = ""
			stored[i].Account.MachineToken = ""
			return m.saveStored(stored)
		}
	}

	acc := Account{
		ID:           fmt.Sprintf("ci_%d", time.Now().UnixNano()),
		Platform:     PlatformQoder,
		Name:         name,
		UID:          qa.UID,
		MachineID:    qa.MachineID,
		MachineToken: "",
		MachineType:  qa.MachineType,
		Nickname:     res.Nickname,
		Enabled:      true,
	}
	sa := storedAccount{Account: acc, EncAccess: encA, EncRefresh: encR, EncMachineToken: encM}
	stored = append(stored, sa)
	return m.saveStored(stored)
}

func (m *Manager) pruneQoderSessionsLocked() {
	for id, s := range m.qoderSessions {
		if time.Since(s.createdAt) > qoderLoginSessionTTL {
			delete(m.qoderSessions, id)
		}
	}
}

// --- 保活与积分 ---

// qoderRefresh 刷新 Qoder 账号 token。
func qoderRefresh(a *Account) error {
	qa := accountToQoder(a)
	if err := qoder.RefreshToken(qa); err != nil {
		return err
	}
	a.AccessToken = qa.AccessToken
	a.RefreshToken = qa.RefreshToken
	return nil
}

// qoderCredits 查询 Qoder 账号余额。
func qoderCredits(a *Account) (remain, total int64, err error) {
	qa := accountToQoder(a)
	remain, err = qoder.UserResource(qa)
	if err != nil {
		return 0, 0, err
	}
	return remain, 0, nil
}

// accountToQoder 把 checkin.Account 转为 qoder.QoderAccount。
func accountToQoder(a *Account) *qoder.QoderAccount {
	return &qoder.QoderAccount{
		UID:          a.UID,
		Nickname:     a.Nickname,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		MachineID:    a.MachineID,
		MachineToken: a.MachineToken,
		MachineType:  a.MachineType,
	}
}

// QoderAccounts 返回所有已启用的 Qoder 平台账号（解密态），供 qoder.Pool 同步。
func (m *Manager) QoderAccounts() []*qoder.QoderAccount {
	accounts := m.loadAccounts()
	var out []*qoder.QoderAccount
	for _, a := range accounts {
		if a.Platform != PlatformQoder || !a.Enabled {
			continue
		}
		out = append(out, accountToQoder(&a))
	}
	return out
}
