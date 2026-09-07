package qoder

// CosySession 封装 Qoder「Cosy」协议的会话态与请求头（strings-notes §7）：
// 认证头 Authorization: Bearer COSY.<token>，以及 cosy-client / login-version /
// x-device-type / x-app-version / X-User-Region / X-Enterprise-Id /
// X-Refresh-Token / X-No-User-Id。
// 仅用于聊天网关；OpenAPI 家族（deviceToken/userinfo/quota/models）请用
// ApplyOpenAPIHeaders（裸 Bearer，无 COSY. 前缀）。

import (
	"net/http"
	"strings"
)

// CosySession 是一个账号的 Cosy 协议会话态。
type CosySession struct {
	AccessToken  string
	RefreshToken string
	DeviceID     string
	MachineID    string
	MachineType  string
	AppVersion   string
	Region       string
	EnterpriseID string
	LoginVersion string
}

// NewCosySession 以访问令牌/刷新令牌/设备 ID 构造会话，其余头取默认值。
func NewCosySession(accessToken, refreshToken, deviceID string) *CosySession {
	return &CosySession{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		DeviceID:     deviceID,
		MachineType:  defaultMachineType,
		AppVersion:   defaultAppVersion,
		Region:       defaultRegion,
		LoginVersion: headerLoginVersion,
	}
}

// AuthHeader 返回 Authorization 头的值；无令牌时返回空串（不设置该头）。
func (s *CosySession) AuthHeader() string {
	if s == nil || s.AccessToken == "" {
		return ""
	}
	if strings.HasPrefix(s.AccessToken, authScheme) {
		return "Bearer " + s.AccessToken
	}
	return "Bearer " + authScheme + s.AccessToken
}

// ApplyHeaders 把 Cosy 协议头写入 h（nil 安全）。
func (s *CosySession) ApplyHeaders(h http.Header) {
	if h == nil {
		return
	}
	if a := s.AuthHeader(); a != "" {
		h.Set("Authorization", a)
	}
	if s.RefreshToken != "" {
		h.Set("X-Refresh-Token", s.RefreshToken)
	}
	h.Set("cosy-client", headerCosyClient)
	if s.LoginVersion != "" {
		h.Set("login-version", s.LoginVersion)
	}
	if s.MachineType != "" {
		h.Set("x-device-type", s.MachineType)
	}
	if s.AppVersion != "" {
		h.Set("x-app-version", s.AppVersion)
	}
	if s.Region != "" {
		h.Set("X-User-Region", s.Region)
	}
	if s.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", s.EnterpriseID)
	}
	if s.DeviceID != "" {
		h.Set("X-No-User-Id", s.DeviceID)
	}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream, application/json")
}
