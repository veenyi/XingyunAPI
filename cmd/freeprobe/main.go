// freeprobe 一次性诊断工具：stdin 读 dbdump 的账号行 JSON，
// 用 joycode 包（color 网关签名，与生产同链路）实测 modelList 元数据与
// 各模型 tiny chat 前后积分差。只打印模型名/状态/积分数值，凭据不落输出。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/joycode"
)

type row struct {
	UserID   string `json:"user_id"`
	PtKey    string `json:"pt_key"`
	Tenant   string `json:"tenant"`
	Login    string `json:"login_type"`
	Color    string `json:"color_base_url"`
	Master   string `json:"master_base_url"`
	Org      string `json:"org_full_name"`
	Valid    int    `json:"credential_valid"`
	Deflt    int    `json:"is_default"`
	Order    int    `json:"display_order"`
	Nickname string `json:"nickname"`
}

func main() {
	var rows []row
	if err := json.NewDecoder(os.Stdin).Decode(&rows); err != nil {
		fmt.Println("STDIN_ERR:", err)
		return
	}
	if len(rows) == 0 {
		fmt.Println("NO_ROWS")
		return
	}
	var picked *joycode.Client
	for _, r := range rows {
		fmt.Printf("ACCOUNT %s*** valid=%d color=%q master=%q login=%q\n",
			short(r.UserID), r.Valid, r.Color, r.Master, r.Login)
		if picked != nil {
			continue
		}
		c := joycode.NewClient(r.PtKey, r.UserID)
		c.Tenant = or(r.Tenant, "JOYCODE")
		c.LoginType = r.Login
		c.ColorBaseURL = or(r.Color, joycode.DefaultColorBaseURL)
		c.MasterBaseURL = r.Master
		c.OrgFullName = r.Org
		info, err := c.UserInfo()
		if err != nil {
			fmt.Println("  PROBE_FAIL:", trunc(err.Error(), 160))
			continue
		}
		// 业务码校验：HTTP 200 不代表登录态有效（envelope code=401 → 账号未登录）；
		// data 可能是全空对象而非 null，所以 bizCode 之外再按 code/msg 双判。
		if bizCode(info) != "" || !envOK(info) {
			fmt.Printf("  PROBE_BIZ_FAIL (code=%v msg=%s)\n", info["code"], info["msg"])
			continue
		}
		fmt.Printf("  PROBE_OK (mode=%s)\n", modeName(c))
		picked = c
	}
	if picked == nil {
		fmt.Println("NO_WORKING_ACCOUNT")
		return
	}
	c := picked
	ctx := context.Background()

	redJSON := func(v interface{}) string {
		b, err := json.Marshal(v)
		if err != nil {
			return "marshal_err"
		}
		var m interface{}
		if json.Unmarshal(redBytes(b), &m) == nil {
			b, _ = json.Marshal(m)
		}
		return string(b)
	}

	rawML, err := c.Post("/api/saas/models/v1/modelList", map[string]interface{}{})
	if err != nil {
		fmt.Println("MODEL_RAW_ERR:", trunc(err.Error(), 200))
	} else {
		fmt.Println("MODEL_RAW:", trunc(redJSON(rawML), 1500))
	}
	rawPT, err := c.GetPoint()
	if err != nil {
		fmt.Println("POINT_RAW_ERR:", trunc(err.Error(), 200))
	} else {
		fmt.Println("POINT_RAW:", trunc(redJSON(rawPT), 900))
	}

	infos, _ := c.ListModels()
	if len(infos) == 0 {
		fmt.Println("MODEL_LIST_UNPARSED (see MODEL_RAW)")
	} else {
		b, _ := json.Marshal(infos[0])
		var keys []string
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		for k := range m {
			keys = append(keys, k)
		}
		fmt.Println("MODEL_FIELDS:", strings.Join(keys, ","))
		for _, mi := range infos {
			api := mi.ChatAPIModel
			if api == "" {
				api = mi.Label
			}
			fmt.Printf("MODEL: %s -> %s (max=%d stream=%v)\n", mi.Label, api, mi.MaxTotalTokens, mi.SupportStream)
		}
	}

	chatModels := []string{}
	for _, mi := range infos {
		if mi.ChatAPIModel != "" {
			chatModels = append(chatModels, mi.ChatAPIModel)
		} else if mi.Label != "" {
			chatModels = append(chatModels, mi.Label)
		}
	}
	if len(chatModels) == 0 {
		chatModels = []string{"JoyAI-Code-1.5", "GLM-5", "GLM-5.1", "MiniMax-M2.7", "Kimi-K2.6", "Doubao-Seed-2.0-pro"}
	}

	for _, model := range chatModels {
		before, perr := c.GetPoint()
		note := ""
		body := map[string]interface{}{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
			"stream":   false,
		}
		start := time.Now()
		resp, err := c.Chat(ctx, body)
		if err != nil {
			note = "CHAT_ERR " + trunc(err.Error(), 200)
		} else {
			if model == chatModels[0] {
				fmt.Println("CHAT_RAW:", trunc(redJSON(resp), 900))
			}
			usage, _ := resp["usage"].(map[string]interface{})
			note = fmt.Sprintf("usage(in=%v,out=%v)", usage["prompt_tokens"], usage["completion_tokens"])
		}
		time.Sleep(1200 * time.Millisecond)
		after, perr2 := c.GetPoint()
		if perr != nil || perr2 != nil {
			note += " | POINT_ERR"
			if perr != nil {
				note += " before=" + trunc(perr.Error(), 100)
			}
			if perr2 != nil {
				note += " after=" + trunc(perr2.Error(), 100)
			}
		} else {
			note += " | " + fmt.Sprintf("point %v -> %v", pointNum(before), pointNum(after))
		}
		fmt.Printf("CHAT %s %s | %s (%.1fs)\n", model, " ", note, time.Since(start).Seconds())
		time.Sleep(600 * time.Millisecond)
	}
}

// redBytes 把 JSON 原文里的长字符串值（疑似凭据）替换为 <R:n>。
func redBytes(b []byte) []byte {
	var m interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return b
	}
	var walk func(v interface{}) interface{}
	walk = func(v interface{}) interface{} {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, vv := range t {
				t[k] = walk(vv)
			}
			return t
		case []interface{}:
			for i, vv := range t {
				t[i] = walk(vv)
			}
			return t
		case string:
			if len(t) > 40 {
				return fmt.Sprintf("<R:%d>", len(t))
			}
			return t
		default:
			return v
		}
	}
	out, err := json.Marshal(walk(m))
	if err != nil {
		return b
	}
	return out
}

// envOK 判定业务信封是否成功（HTTP 200 下上游仍可能回 401 账号未登录）。
func envOK(m map[string]interface{}) bool {
	if m == nil {
		return false
	}
	switch v := m["code"].(type) {
	case nil:
	case float64:
		if v != 0 && v != 200 {
			return false
		}
	case string:
		if v != "" && v != "0" && v != "200" {
			return false
		}
	}
	if msg, _ := m["msg"].(string); strings.Contains(msg, "未登录") {
		return false
	}
	return true
}

func pointNum(m map[string]interface{}) interface{} {
	if m == nil {
		return "?"
	}
	if d, ok := m["data"].(map[string]interface{}); ok {
		if items, ok := d["usageItems"].([]interface{}); ok && len(items) > 0 {
			if it, ok := items[0].(map[string]interface{}); ok {
				return fmt.Sprintf("remain=%v total=%v used=%v", it["remain"], it["total"], it["used"])
			}
		}
		m = d
	}
	for _, k := range []string{"point", "points", "totalPoint", "surplusPoint", "balance", "credit", "credits", "remain"} {
		if v, ok := m[k]; ok {
			return v
		}
	}
	if len(m) == 0 {
		return "empty"
	}
	return "shape:" + fmt.Sprint(keysOf(m))
}

func keysOf(m map[string]interface{}) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return strings.Join(ks, "/")
}

func short(s string) string {
	if len(s) > 4 {
		return s[:4]
	}
	return s
}

func or(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func modeName(c *joycode.Client) string {
	// Client 未暴露 base 字段；用端点行为近似：这里只区分是否配置 color
	return "client"
}

// bizCode 提取业务层错误（envelope code 非 0/200/缺省 或 msg=账号未登录）。
func bizCode(resp map[string]interface{}) string {
	if resp == nil {
		return "nil response"
	}
	if d, ok := resp["data"]; ok && d == nil {
		if msg, _ := resp["msg"].(string); msg != "" {
			return msg
		}
		return "data null"
	}
	return ""
}
