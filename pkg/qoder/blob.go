package qoder

// 账号池凭据持久化：settings blob（键 qoder_accounts）。
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
	MachineID     string  `json:"machine_id,omitempty"`
	MachineType   string  `json:"machine_type,omitempty"`
	EnterpriseID  string  `json:"enterprise_id,omitempty"`
	Region        string  `json:"region,omitempty"`
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
func toStored(a *QoderAccount) storedAccount {
	st := storedAccount{
		UserID:        a.UserID,
		Nickname:      a.Nickname,
		Remark:        a.Remark,
		MachineID:     a.MachineID,
		MachineType:   a.MachineType,
		EnterpriseID:  a.EnterpriseID,
		Region:        a.Region,
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
func fromStored(st storedAccount) *QoderAccount {
	return &QoderAccount{
		UserID:        st.UserID,
		Nickname:      st.Nickname,
		Remark:        st.Remark,
		AccessToken:   openField(st.EncAccess),
		RefreshToken:  openField(st.EncRefresh),
		MachineID:     st.MachineID,
		MachineType:   st.MachineType,
		EnterpriseID:  st.EnterpriseID,
		Region:        st.Region,
		Credits:       st.Credits,
		CreditsTotal:  st.CreditsTotal,
		Disabled:      st.Disabled,
		Note:          st.Note,
		LastRefreshAt: st.LastRefreshAt,
		LastRefreshOK: st.LastRefreshOK,
	}
}

// saveAccounts 把账号列表写入 settings blob。
func saveAccounts(st settingsStore, accounts []*QoderAccount) error {
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
func loadAccounts(st settingsStore) ([]*QoderAccount, error) {
	if st == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(st.GetSetting(SettingsKey))
	if raw == "" {
		return nil, nil
	}
	var items []storedAccount
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("qoder: 解析账号 blob 失败: %w", err)
	}
	out := make([]*QoderAccount, 0, len(items))
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
