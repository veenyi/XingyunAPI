// WorkBuddy(CodeBuddy) 签到客户端：token 刷新 / 每日签到 / 积分查询。
// 协议与请求头移植自 wild-work 实测实现（docs/api-reference.md §0/§4/§6）。
package checkin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	wbChatBaseCN    = "https://copilot.tencent.com"
	wbBillingBaseCN = "https://www.codebuddy.cn"
	wbUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
)

func wbOrigin() string { return "https://www.codebuddy.cn" }

func wbCommonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", wbOrigin())
	req.Header.Set("Referer", wbOrigin()+"/")
	req.Header.Set("User-Agent", wbUA)
}

func wbBillingHeaders(req *http.Request, a *Account) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", wbUA)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

var checkinHTTP = &http.Client{Timeout: 30 * time.Second}

func doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := checkinHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	return raw, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// wbRefresh 刷新 WorkBuddy access token（X-Refresh-Token 只出现在这里）。
func wbRefresh(a *Account) error {
	if a.RefreshToken == "" {
		return fmt.Errorf("缺少 refreshToken，请重新登录")
	}
	req, err := http.NewRequest(http.MethodPost, wbChatBaseCN+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return err
	}
	wbCommonHeaders(req)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	data, err := doJSON(req)
	if err != nil {
		return fmt.Errorf("刷新失败: %w", err)
	}
	var tok struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		} `json:"data"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return fmt.Errorf("刷新响应解析失败: %w", err)
	}
	access := tok.Data.AccessToken
	if access == "" {
		access = tok.AccessToken
	}
	if access == "" {
		return fmt.Errorf("刷新失败：响应中没有 accessToken")
	}
	a.AccessToken = access
	if tok.Data.RefreshToken != "" {
		a.RefreshToken = tok.Data.RefreshToken
	} else if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Data.Domain != "" {
		a.Domain = tok.Data.Domain
	}
	return nil
}

// wbCheckin 执行 WorkBuddy 每日签到。已签到的业务错误原样返回给调用方展示。
func wbCheckin(a *Account) error {
	req, err := http.NewRequest(http.MethodPost, wbBillingBaseCN+"/v2/billing/meter/daily-checkin", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	wbBillingHeaders(req, a)
	data, err := doJSON(req)
	if err != nil {
		return err
	}
	// 业务 code 非 0 也算失败（如"今日已签到"），把消息透出。
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
	}
	if json.Unmarshal(data, &resp) == nil {
		msg := resp.Message
		if msg == "" {
			msg = resp.Msg
		}
		if resp.Code != 0 {
			return fmt.Errorf("code=%d %s", resp.Code, msg)
		}
		if resp.Success != nil && !*resp.Success {
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}

// wbCredits 查询 WorkBuddy 剩余积分。
func wbCredits(a *Account) (remain, total int64, err error) {
	now := time.Now()
	body := map[string]interface{}{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, wbBillingBaseCN+"/v2/billing/meter/get-user-resource", bytes.NewReader(raw))
	if err != nil {
		return 0, 0, err
	}
	wbBillingHeaders(req, a)
	data, err := doJSON(req)
	if err != nil {
		return 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					CapacitySize        int64 `json:"CapacitySize"`
					CapacityRemain      int64 `json:"CapacityRemain"`
					CapacityUsed        int64 `json:"CapacityUsed"`
					CycleCapacitySize   int64 `json:"CycleCapacitySize"`
					CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, fmt.Errorf("积分响应解析失败: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r, t int64
		switch {
		case acct.CycleCapacitySize > 0:
			r, t = acct.CycleCapacityRemain, acct.CycleCapacitySize
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r, t = acct.CycleCapacityRemain, acct.CycleCapacityRemain+acct.CycleCapacityUsed
		default:
			r, t = acct.CapacityRemain, acct.CapacitySize
		}
		if r < 0 {
			r = 0
		}
		remain += r
		total += t
	}
	return remain, total, nil
}

var _ = slog.Debug // 保持 slog 引用（日志在 Manager 层打）
