// Package common holds small shared helpers used across the fork packages.
package common

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SettingSource is the minimal settings reader the packages depend on.
type SettingSource interface {
	GetSetting(key string) string
}

// SettingEnabled reads a boolean setting stored as a string.
// Missing/empty means false.
func SettingEnabled(s SettingSource, key string) bool {
	return SettingEnabledOr(s, key, false)
}

// SettingEnabledOr reads a boolean setting with a default applied when the
// key is missing or empty.
func SettingEnabledOr(s SettingSource, key string, def bool) bool {
	if s == nil {
		return def
	}
	v := strings.TrimSpace(s.GetSetting(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	}
	return def
}

// SettingInt reads an integer setting with a default.
func SettingInt(s SettingSource, key string, def int) int {
	if s == nil {
		return def
	}
	v := strings.TrimSpace(s.GetSetting(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// IsTimeoutError reports whether err is (or wraps) a timeout.
func IsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	type timeouter interface{ Timeout() bool }
	if t, ok := err.(timeouter); ok && t.Timeout() {
		return true
	}
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return IsTimeoutError(u.Unwrap())
	}
	if ne, ok := err.(*net.OpError); ok {
		return ne.Timeout()
	}
	return false
}

// NormalizeBaseURL trims trailing slashes and guarantees a scheme. Public
// hosts are forced to https (see GuardPublicHTTPS).
func NormalizeBaseURL(raw string) (string, error) {
	u, err := NormalizeBaseURLLocal(raw)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || isPrivateHost(host) {
		return u.String(), nil
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("只允许 https，当前 %q", u.Scheme)
	}
	return u.String(), nil
}

// NormalizeBaseURLLocal normalizes without forcing https on public hosts —
// used for user-defined custom providers that may legitimately proxy http.
func NormalizeBaseURLLocal(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("地址缺少主机名")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("仅支持 http/https，当前 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("地址缺少主机名")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

// GuardPublicHTTPS validates a base URL for preset channels: https only,
// no localhost / private / link-local targets (SSRF guard).
func GuardPublicHTTPS(raw string) (*url.URL, error) {
	u, err := NormalizeBaseURLLocal(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return nil, fmt.Errorf("拒绝本机/内网主机名")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return nil, fmt.Errorf("拒绝内网/链路本地地址 %s", host)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("只允许 https，当前 %q", u.Scheme)
	}
	return u, nil
}

func isPrivateHost(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

// NowStr is the shared local-time timestamp format used in state blobs.
func NowStr() string {
	return time.Now().Format("2006-01-02 15:04:05")
}
