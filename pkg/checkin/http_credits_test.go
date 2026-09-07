package checkin

import (
	"encoding/json"
	"testing"
)

// 用真实上游响应结构回归 creditsFrom：WorkBuddy /v2/billing/meter/get-user-resource
// 的 data 是腾讯云包裹层 data.Response.Data.Accounts[]（CapacityRemain/CapacitySize）。
func TestCreditsFromWorkBuddyMeter(t *testing.T) {
	const raw = `{"code":0,"msg":"成功","data":{"Response":{"Data":{
		"TotalCount":16,"TotalDosage":1803,
		"Accounts":[
			{"CapacityRemain":500,"CapacitySize":500,"CapacityUsed":0},
			{"CapacityRemain":403,"CapacitySize":410,"CapacityUsed":7},
			{"CapacityRemain":900,"CapacitySize":900,"CapacityUsed":0}
		]}}}}`
	var env struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m := &Manager{}
	credits, total := m.creditsFrom(env.Data)
	if credits != 1803 {
		t.Errorf("credits = %v, want 1803", credits)
	}
	if total != 1810 {
		t.Errorf("total = %v, want 1810", total)
	}
}

// 其他平台结构不受影响：普通顶层/嵌套字段解析照旧。
func TestCreditsFromFlatAndNested(t *testing.T) {
	m := &Manager{}

	flat := map[string]interface{}{"credits": 42.0, "credits_total": 100.0}
	c, tt := m.creditsFrom(flat)
	if c != 42 || tt != 100 {
		t.Errorf("flat = (%v,%v), want (42,100)", c, tt)
	}

	nested := map[string]interface{}{
		"data": map[string]interface{}{"remaining": 7.0, "limit": 10.0},
	}
	c, tt = m.creditsFrom(nested)
	if c != 7 || tt != 10 {
		t.Errorf("nested = (%v,%v), want (7,10)", c, tt)
	}

	// 无 accounts 且无任何已知字段 → 双零（调用方回退 state，不误写 0）
	empty := map[string]interface{}{"foo": "bar"}
	c, tt = m.creditsFrom(empty)
	if c != 0 || tt != 0 {
		t.Errorf("empty = (%v,%v), want (0,0)", c, tt)
	}
}
