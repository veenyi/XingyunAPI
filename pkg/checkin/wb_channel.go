// wb_channel.go WorkBuddy 平台账号桥接：checkin.Account → workbuddy.WBAccount。
package checkin

import (
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/workbuddy"
)

// WBAccounts 返回所有已启用的 WorkBuddy 平台账号（解密态），供 workbuddy.Pool 同步。
func (m *Manager) WBAccounts() []*workbuddy.WBAccount {
	accounts := m.loadAccounts()
	var out []*workbuddy.WBAccount
	for _, a := range accounts {
		if a.Platform != PlatformWorkBuddy || !a.Enabled {
			continue
		}
		out = append(out, wbAccountToRuntime(&a))
	}
	return out
}

func wbAccountToRuntime(a *Account) *workbuddy.WBAccount {
	return &workbuddy.WBAccount{
		UID:          a.UID,
		Nickname:     a.Nickname,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		EnterpriseID: a.EnterpriseID,
		Domain:       a.Domain,
		Region:       "cn",
	}
}
