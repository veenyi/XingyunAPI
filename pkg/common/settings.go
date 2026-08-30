package common

import (
	"strconv"
	"strings"
)

// SettingSource 是 settings 表的最小读取面，避免各包为一个大 Store 打架。
type SettingSource interface {
	GetSetting(key string) string
}

// SettingEnabled 统一开关型设置的取值约定："1" 与 "true" 视为开启，缺省视为关闭。
// 新渠道默认都走这条约定，保证没写过这个键的老安装行为不变。
func SettingEnabled(settings SettingSource, key string) bool {
	if settings == nil {
		return false
	}
	v := strings.TrimSpace(settings.GetSetting(key))
	return v == "1" || strings.EqualFold(v, "true")
}

// SettingEnabledOr 给"默认开启"的老开关用：只有显式写成 "0" / "false" 才算关。
// 看板保存开关用的是 "1"/"0"，只判 "false" 会让用户点了关闭却仍然开着。
func SettingEnabledOr(settings SettingSource, key string, def bool) bool {
	if settings == nil {
		return def
	}
	v := strings.TrimSpace(settings.GetSetting(key))
	if v == "" {
		return def
	}
	if v == "0" || strings.EqualFold(v, "false") {
		return false
	}
	return true
}

// SettingInt 读整型设置，缺失或非法时用默认值。
func SettingInt(settings SettingSource, key string, defaultVal int) int {
	if settings == nil {
		return defaultVal
	}
	raw := strings.TrimSpace(settings.GetSetting(key))
	if raw == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultVal
	}
	return n
}
