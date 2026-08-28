package joycode

import (
	"os"
	"testing"
)

// 门控联网自检：JOYCODE_LIVE_TEST=1 时向 color gateway 真实发一次 telemetry 上报。
// 注意：telemetry 是批量入库接口，实测即使 pt_key 无效也返回 code:0，
// 因此这里只能证明 URL/签名/头/载荷形态与 gzip 解码全链路正确，
// 真正的解冻效果必须在已冻结账号上验证。
func TestReportClientActivity_GatewayShape(t *testing.T) {
	if os.Getenv("JOYCODE_LIVE_TEST") != "1" {
		t.Skip("set JOYCODE_LIVE_TEST=1 to hit the real gateway")
	}
	c := NewClient("0123456789abcdef0123456789abcdef", "selfcheck-invalid-user")
	c.LoginType = "N_PIN_PC"
	c.Tenant = "JOYCODE"

	if err := c.ReportClientActivity(); err != nil {
		t.Fatalf("gateway rejected the report shape: %v", err)
	}
}
