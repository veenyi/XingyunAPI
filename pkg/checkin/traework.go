// TraeWork(Trae SOLO) 签到客户端：token 刷新 / 每日签到 / 积分查询。
// 协议移植自 wild-work 实测实现。注意：UG 签到/积分接口校验设备指纹头，
// x-device-id 必须是账号真实注册的设备 ID，随机值会以 9074 拒绝。
package checkin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	twUgHost     = "https://api.trae.cn"
	twOAuthHost  = "https://api.trae.com.cn"
	twClientID   = "en1oxy7wnw8j9n"
	twAppID      = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	twIdeVersion = "0.1.52"
	twIdeCode    = "20260811"
	twBrand      = "20Y5A002XX"
	twOS         = "Windows 10 Pro"
	twUA         = "Trae/" + twIdeVersion

	twEpExchange  = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	twEpStatus    = "/trae/api/v2/ug/checkin_credits/status"
	twEpClaim     = "/trae/api/v2/ug/checkin_credits/claim"
	twEpEntUsage  = "/trae/api/v2/pay/web_user_ent_usage"
)

func twUgHeaders(req *http.Request, a *Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", twUA)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	req.Header.Set("X-User-Region", "CN")
	req.Header.Set("x-device-brand", twBrand)
	req.Header.Set("x-device-type", "windows")
	req.Header.Set("x-os-version", twOS)
	req.Header.Set("x-app-version", twIdeVersion)
	if a.DeviceID != "" {
		req.Header.Set("x-device-id", a.DeviceID)
	}
}

// twRefresh 刷新 TraeWork token（ExchangeToken 换新 access/refresh）。
func twRefresh(a *Account) error {
	if a.RefreshToken == "" {
		return fmt.Errorf("缺少 refreshToken，请重新登录")
	}
	host := a.ApiHost
	if host == "" {
		host = twOAuthHost
	}
	body, _ := json.Marshal(map[string]interface{}{
		"ClientID": twClientID, "RefreshToken": a.RefreshToken, "ClientSecret": "-", "UserID": "",
	})
	req, err := http.NewRequest(http.MethodPost, host+twEpExchange, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", twUA)
	data, err := doJSON(req)
	if err != nil {
		return fmt.Errorf("刷新失败: %w", err)
	}
	var resp struct {
		Result struct {
			Token        string `json:"Token"`
			RefreshToken string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("刷新响应解析失败: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("刷新失败：响应中没有 Token，请重新登录")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	return nil
}

// twCheckin 查询签到状态并领取（Trae 是两步：status → claim）。
// 已签到返回可读错误"今日已签到"。
func twCheckin(a *Account) error {
	req, err := http.NewRequest(http.MethodPost, twUgHost+twEpStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	twUgHeaders(req, a)
	data, err := doJSON(req)
	if err != nil {
		return fmt.Errorf("查询签到状态失败: %w", err)
	}
	var status struct {
		CheckedIn bool   `json:"checked_in"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Msg       string `json:"msg"`
		Success   *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return fmt.Errorf("签到状态解析失败: %w", err)
	}
	if status.Code != 0 || (status.Success != nil && !*status.Success) {
		return fmt.Errorf("查询签到状态失败: %s", twMsg(status.Message, status.Msg))
	}
	if status.CheckedIn {
		return fmt.Errorf("今日已签到")
	}

	// claim：9074（限流）重试一次，9095 等业务码交给最终状态验证
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodPost, twUgHost+twEpClaim, bytes.NewReader([]byte("{}")))
		if err != nil {
			return err
		}
		twUgHeaders(req, a)
		data, err := doJSON(req)
		if err != nil {
			return fmt.Errorf("签到请求失败: %w", err)
		}
		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Msg     string `json:"msg"`
			Success *bool  `json:"success"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("签到响应解析失败: %w", err)
		}
		if resp.Code == 9074 && attempt == 0 {
			time.Sleep(3 * time.Second)
			continue
		}
		if resp.Code != 0 && resp.Code != 9095 {
			return fmt.Errorf("签到失败: %s", twMsg(resp.Message, resp.Msg))
		}
		if resp.Success != nil && !*resp.Success && resp.Code != 9095 {
			return fmt.Errorf("签到失败: %s", twMsg(resp.Message, resp.Msg))
		}
		return nil
	}
	return fmt.Errorf("签到被限流（9074），请稍后重试")
}

// twCredits 查询 TraeWork 剩余积分（int64，用于存 state）。
func twCredits(a *Account) (remain, total int64, err error) {
	r, t, err := twCreditsF64(a)
	if err != nil {
		return 0, 0, err
	}
	return int64(r), int64(t), nil
}

// twCreditsF64 查询 TraeWork 剩余积分，保留小数精度。
func twCreditsF64(a *Account) (remain, total float64, err error) {
	req, err := http.NewRequest(http.MethodPost, twUgHost+twEpEntUsage, bytes.NewReader([]byte(`{"require_usage":true}`)))
	if err != nil {
		return 0, 0, err
	}
	twUgHeaders(req, a)
	data, err := doJSON(req)
	if err != nil {
		return 0, 0, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit float64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, fmt.Errorf("积分响应解析失败: %w", err)
	}
	for _, p := range resp.UserEntitlementPackList {
		remain += p.EntitlementBaseInfo.Quota.CreditsLimit - p.Usage.CreditsAmount
		total += p.EntitlementBaseInfo.Quota.CreditsLimit
	}
	if remain < 0 {
		remain = 0
	}
	return remain, total, nil
}

func twMsg(message, msg string) string {
	if strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return strings.TrimSpace(msg)
}
