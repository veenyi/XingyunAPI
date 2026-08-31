package common

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// GuardPublicHTTPS 只放行公网 https 端点。
// 上游地址来自设置页（管理员可填），写坏一次就会让服务端替用户去敲内网接口，
// 所以重定向目标也必须过同一道门。
func GuardPublicHTTPS(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("空地址")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("只允许 https，当前 %q", u.Scheme)
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return fmt.Errorf("地址缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsPrivate() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
			return fmt.Errorf("拒绝内网/链路本地地址 %s", host)
		}
		return nil
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".internal") || strings.HasSuffix(lower, ".local") ||
		strings.HasSuffix(lower, ".localdomain") {
		return fmt.Errorf("拒绝本机/内网主机名 %s", host)
	}
	return nil
}

// NormalizeBaseURL 去掉尾部斜杠并校验；校验失败时返回错误，由调用方决定回退策略。
func NormalizeBaseURL(raw string) (string, error) {
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	if v == "" {
		return "", fmt.Errorf("地址为空")
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", err
	}
	if err := GuardPublicHTTPS(u); err != nil {
		return "", err
	}
	return v, nil
}

// NormalizeBaseURLLocal 是 freepool 这类"故意连本地聚合代理"的宽松版：
// 允许 http 与 loopback / 内网主机（9Router、FreeLLMAPI 常跑在 localhost），
// 但仍要求是能解析的绝对 http(s) URL。公网 SSRF 闸门在这里不适用，因为
// 地址由管理员在渠道设置里主动填写、目的就是连本地服务。
func NormalizeBaseURLLocal(raw string) (string, error) {
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	if v == "" {
		return "", fmt.Errorf("地址为空")
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("仅支持 http/https，当前 %q", u.Scheme)
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return "", fmt.Errorf("地址缺少主机名")
	}
	return v, nil
}
