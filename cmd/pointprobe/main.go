// pointprobe: 逐账号实测积分接口 getNewIdePoint（只读账号库，不写生产）。
// 输出上游 data.usageItems 原始结构，用于核对 SPA 期望的 seatPackage 字段。
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/veenyi/XingyunAPI/pkg/joycode"
	"github.com/veenyi/XingyunAPI/pkg/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: pointprobe <proxy.db>")
		os.Exit(2)
	}
	dbPath := os.Args[1]

	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Println("store open:", err)
		os.Exit(1)
	}
	defer st.Close()

	ro, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		fmt.Println("ro open:", err)
		os.Exit(1)
	}
	defer ro.Close()

	type acct struct{ user, tenant, loginType, colorURL, masterURL, org string }
	rows, err := ro.Query("SELECT user_id, COALESCE(tenant,''), COALESCE(login_type,''), COALESCE(color_base_url,''), COALESCE(master_base_url,''), COALESCE(org_full_name,'') FROM accounts ORDER BY display_order")
	if err != nil {
		fmt.Println("query:", err)
		os.Exit(1)
	}
	var list []acct
	for rows.Next() {
		var a acct
		if err := rows.Scan(&a.user, &a.tenant, &a.loginType, &a.colorURL, &a.masterURL, &a.org); err != nil {
			fmt.Println("scan:", err)
			os.Exit(1)
		}
		list = append(list, a)
	}
	rows.Close()

	accs, err := st.ListAllAccountsWithCredentials()
	if err != nil {
		fmt.Println("list creds:", err)
		os.Exit(1)
	}
	keys := map[string]string{}
	for _, a := range accs {
		keys[a.UserID] = a.PtKey
	}

	for _, a := range list {
		fmt.Printf("=== %s ===\n", a.user)
		ptKey := keys[a.user]
		if ptKey == "" {
			fmt.Println("  pt_key: MISSING")
			continue
		}
		c := joycode.NewClient(ptKey, a.user)
		c.SetTimeout(30 * time.Second)
		c.SetColorContext(a.colorURL, a.masterURL, a.tenant, a.loginType, a.org)

		start := time.Now()
		resp, err := c.GetPoint()
		if err != nil {
			fmt.Printf("  point: ERR after %s: %s\n", time.Since(start).Round(time.Millisecond), trunc(err.Error(), 300))
			continue
		}
		b, _ := json.Marshal(resp["data"])
		fmt.Printf("  point: OK (%s) data=%s\n", time.Since(start).Round(time.Millisecond), trunc(string(b), 600))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
