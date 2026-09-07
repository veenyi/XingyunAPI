package workbuddy

// 账号池凭据持久化：settings blob（键 workbuddy_accounts）。
// 敏感字段（access/refresh token）经 AES-GCM 加密为 enc_access/enc_refresh 后存储，
// 密钥复用 store 的密钥文件（~/.joycode-proxy/.enc_key，hex 32 字节），
// 密文格式与 store 内部加密一致（nonce 前缀 + hex）。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/store"
)

// storedAccount 是 blob 里的一条账号（明文凭据不落盘）。
type storedAccount struct {
	UserID        string  `json:"user_id"`
	Nickname      string  `json:"nickname,omitempty"`
	Remark        string  `json:"remark,omitempty"`
	EnterpriseID  string  `json:"enterprise_id,omitempty"`
	Domain        string  `json:"domain,omitempty"`
	APIHost       string  `json:"api_host,omitempty"`
	EncAccess     string  `json:"enc_access,omitempty"`
	EncRefresh    string  `json:"enc_refresh,omitempty"`
	Credits       float64 `json:"credits,omitempty"`
	CreditsTotal  float64 `json:"credits_total,omitempty"`
	Disabled      bool    `json:"disabled,omitempty"`
	Note          string  `json:"note,omitempty"`
	LastRefreshAt string  `json:"last_refresh_at,omitempty"`
	LastRefreshOK bool    `json:"last_refresh_ok,omitempty"`
}

// settingsStore 是账号池对设置存储的最小依赖（*store.Store 直接满足）。
type settingsStore interface {
	GetSetting(key string) string
	SetSetting(key, value string) error
}

// encKeyPath 返回与 pkg/store 共享的密钥文件路径。
func encKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, store.DefaultDBDir, ".enc_key"), nil
}

var (
	encKeyOnce sync.Once
	encKeyVal  []byte
	encKeyErr  error
)

// encKey 读取（或创建）32 字节 AES 密钥，进程内缓存。
func encKey() ([]byte, error) {
	encKeyOnce.Do(func() {
		path, err := encKeyPath()
		if err != nil {
			encKeyErr = err
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			encKeyErr = err
			return
		}
		if data, rerr := os.ReadFile(path); rerr == nil {
			if key, derr := hex.DecodeString(strings.TrimSpace(string(data))); derr == nil && len(key) == 32 {
				encKeyVal = key
				return
			}
		}
		key := make([]byte, 32)
		if _, rerr := io.ReadFull(rand.Reader, key); rerr != nil {
			encKeyErr = rerr
			return
		}
		if werr := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); werr != nil {
			encKeyErr = werr
			return
		}
		encKeyVal = key
	})
	return encKeyVal, encKeyErr
}

// sealField 加密敏感字段（AES-GCM，nonce 前置，hex 编码）。
func sealField(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	key, err := encKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// openField 解密 sealField 的产物；空串/解密失败返回空（旧数据容忍）。
func openField(enc string) string {
	if enc == "" {
		return ""
	}
	key, err := encKey()
	if err != nil {
		return ""
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	data, err := hex.DecodeString(enc)
	if err != nil || len(data) < gcm.NonceSize() {
		return ""
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return ""
	}
	return string(plain)
}

// toStored 把内存账号转为持久化形态（令牌加密）。
func toStored(a *WBAccount) storedAccount {
	st := storedAccount{
		UserID:        a.UserID,
		Nickname:      a.Nickname,
		Remark:        a.Remark,
		EnterpriseID:  a.EnterpriseID,
		Domain:        a.Domain,
		APIHost:       a.APIHost,
		Credits:       a.Credits,
		CreditsTotal:  a.CreditsTotal,
		Disabled:      a.Disabled,
		Note:          a.Note,
		LastRefreshAt: a.LastRefreshAt,
		LastRefreshOK: a.LastRefreshOK,
	}
	if enc, err := sealField(a.AccessToken); err == nil {
		st.EncAccess = enc
	} else {
		slog.Error("store: encrypt secret setting failed", "key", SettingsKey, "error", err)
	}
	if enc, err := sealField(a.RefreshToken); err == nil {
		st.EncRefresh = enc
	} else {
		slog.Error("store: encrypt secret setting failed", "key", SettingsKey, "error", err)
	}
	return st
}

// fromStored 把持久化形态还原为内存账号（令牌解密，失败容忍为空）。
func fromStored(st storedAccount) *WBAccount {
	return &WBAccount{
		UserID:        st.UserID,
		Nickname:      st.Nickname,
		Remark:        st.Remark,
		AccessToken:   openField(st.EncAccess),
		RefreshToken:  openField(st.EncRefresh),
		EnterpriseID:  st.EnterpriseID,
		Domain:        st.Domain,
		APIHost:       st.APIHost,
		Credits:       st.Credits,
		CreditsTotal:  st.CreditsTotal,
		Disabled:      st.Disabled,
		Note:          st.Note,
		LastRefreshAt: st.LastRefreshAt,
		LastRefreshOK: st.LastRefreshOK,
	}
}

// BackfillCheckinAccounts 把签到中心 blob（checkin_accounts）里的 workbuddy
// 账号并入池 blob。v0.7.2 时代签到与聊天共用账号，重建版拆开存储后老库只有
// 签到一半。密文字段原样搬移（同一 .enc_key，不做解密/再加密）；池里已有
// 的 user_id 优先；签到侧禁用或无任何令牌密文的行跳过。返回回填条数。
func BackfillCheckinAccounts(st settingsStore, checkinBlob string) int {
	if st == nil || strings.TrimSpace(checkinBlob) == "" {
		return 0
	}
	var wrapper struct {
		Accounts []struct {
			Platform   string `json:"platform"`
			UID        string `json:"uid"`
			Name       string `json:"name"`
			Enabled    *bool  `json:"enabled"`
			EncAccess  string `json:"enc_access"`
			EncRefresh string `json:"enc_refresh"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(checkinBlob), &wrapper); err != nil {
		slog.Warn("workbuddy: 签到账号 blob 解析失败，跳过回填", "error", err)
		return 0
	}
	raw := strings.TrimSpace(st.GetSetting(SettingsKey))
	var items []storedAccount
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			slog.Warn("workbuddy: 现有账号 blob 解析失败，跳过回填", "error", err)
			return 0
		}
	}
	have := map[string]bool{}
	for _, it := range items {
		have[it.UserID] = true
	}
	added := 0
	for _, ca := range wrapper.Accounts {
		if ca.Platform != Name || ca.UID == "" || have[ca.UID] {
			continue
		}
		if ca.EncAccess == "" && ca.EncRefresh == "" {
			continue
		}
		if ca.Enabled != nil && !*ca.Enabled {
			continue
		}
		items = append(items, storedAccount{
			UserID:     ca.UID,
			Nickname:   ca.Name,
			EncAccess:  ca.EncAccess,
			EncRefresh: ca.EncRefresh,
		})
		have[ca.UID] = true
		added++
	}
	if added == 0 {
		return 0
	}
	blob, err := json.Marshal(items)
	if err != nil {
		slog.Warn("workbuddy: 回填序列化失败", "error", err)
		return 0
	}
	if err := st.SetSetting(SettingsKey, string(blob)); err != nil {
		slog.Warn("workbuddy: 回填写入失败", "error", err)
		return 0
	}
	return added
}

// saveAccounts 把账号列表写入 settings blob。
func saveAccounts(st settingsStore, accounts []*WBAccount) error {
	if st == nil {
		return nil
	}
	items := make([]storedAccount, 0, len(accounts))
	for _, a := range accounts {
		if a == nil || a.UserID == "" {
			continue
		}
		items = append(items, toStored(a))
	}
	blob, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("%s: %w", errSaveAccounts, err)
	}
	if err := st.SetSetting(SettingsKey, string(blob)); err != nil {
		return fmt.Errorf("%s: %w", errSaveAccounts, err)
	}
	return nil
}

// loadAccounts 从 settings blob 读取账号列表。
func loadAccounts(st settingsStore) ([]*WBAccount, error) {
	if st == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(st.GetSetting(SettingsKey))
	if raw == "" {
		return nil, nil
	}
	var items []storedAccount
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("workbuddy: 解析账号 blob 失败: %w", err)
	}
	out := make([]*WBAccount, 0, len(items))
	for _, st1 := range items {
		if st1.UserID == "" {
			continue
		}
		out = append(out, fromStored(st1))
	}
	return out, nil
}

// nowRFC3339 是状态字段的统一时间戳格式。
func nowRFC3339() string { return time.Now().Format(time.RFC3339) }
