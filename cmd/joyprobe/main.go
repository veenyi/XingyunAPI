// joyprobe: 逐账号实测 saas/color 双面死活（只读快照库，不写生产）。
// 第二个参数 "warm" 时，被灰度门冻住的账号会补报 IDE 遥测并复测。
package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/veenyi/XingyunAPI/pkg/joycode"
	"github.com/veenyi/XingyunAPI/pkg/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: joyprobe <snapshot-proxy.db> [warm]")
		os.Exit(2)
	}
	dbPath := os.Args[1]
	mode := ""
	if len(os.Args) > 2 {
		mode = os.Args[2]
	}
	warm := mode == "warm" || mode == "warmall"

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

	type acct struct {
		user, tenant, loginType, colorURL, masterURL, org string
	}
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
		fmt.Printf("  pt_key: len=%d\n", len(ptKey))
		c := joycode.NewClient(ptKey, a.user)
		c.SetTimeout(45 * time.Second)
		c.SetColorContext(a.colorURL, a.masterURL, a.tenant, a.loginType, a.org)

		if vErr := c.Validate(); vErr != nil {
			fmt.Println("  saas_validate: FAIL:", trunc(vErr.Error(), 160))
		} else {
			fmt.Println("  saas_validate: OK")
		}

		start := time.Now()
		denied, err := c.GrayDenied()
		if err != nil {
			fmt.Printf("  color_chat: ERR after %s: %s\n", time.Since(start).Round(time.Millisecond), trunc(err.Error(), 220))
			continue
		}
		if !denied {
			fmt.Printf("  color_chat: OPEN (%s) — gray gate not active\n", time.Since(start).Round(time.Millisecond))
			if mode == "warmall" {
				fmt.Println("  warm: replaying telemetry on OPEN account (upstream acceptance check) ...")
				if wErr := c.WarmTelemetry(); wErr != nil {
					fmt.Println("  warm: ERR:", trunc(wErr.Error(), 220))
				} else {
					fmt.Println("  warm: 6 events accepted by upstream")
				}
			}
			continue
		}
		fmt.Printf("  color_chat: AI_GRAY_ACCESS_DENIED (%s)\n", time.Since(start).Round(time.Millisecond))
		if !warm {
			fmt.Println("  gray_gate: DENIED (pass \"warm\" to replay IDE telemetry)")
			continue
		}
		fmt.Println("  gray_gate: DENIED — replaying IDE telemetry ...")
		if wErr := c.WarmTelemetry(); wErr != nil {
			fmt.Println("  warm: ERR:", trunc(wErr.Error(), 220))
			continue
		}
		fmt.Println("  warm: 6 events sent, waiting 8s ...")
		time.Sleep(8 * time.Second)
		denied2, err2 := c.GrayDenied()
		if err2 != nil {
			fmt.Println("  recheck: ERR:", trunc(err2.Error(), 220))
			continue
		}
		if denied2 {
			fmt.Println("  recheck: STILL DENIED")
		} else {
			fmt.Println("  recheck: OPEN — account warmed")
		}
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
