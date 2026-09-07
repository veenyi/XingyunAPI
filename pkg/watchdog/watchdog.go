// Package watchdog 在行云进程内看护 keeper.sh（keeper 巡检大脑的看护循环）。
// 场景：NAS 重启后行云应用自启而 keeper.sh（nohup 手动拉起）不在；或 keeper.sh 意外全灭。
// 定时探测 keeper Agent 端口；连续不可达则经 sudo 白名单拉起 keeper.sh（内建自动化，不依赖外部 runner）。
package watchdog

import (
	"log"
	"net/http"
	"os/exec"
	"syscall"
	"time"
)

const keeperSh = "/vol1/@apphome/xingyun-api/keeper/keeper.sh"

func Start(agentURL string, interval time.Duration) {
	go func() {
		time.Sleep(30 * time.Second) // 给 keeper.sh 自身拉起留时间，避开开机高峰
		client := &http.Client{Timeout: 3 * time.Second}
		failStreak := 0
		for {
			resp, err := client.Get(agentURL)
			if err == nil {
				resp.Body.Close()
				failStreak = 0
			} else {
				failStreak++
				log.Printf("[watchdog] keeper agent unreachable (%s): %v streak=%d", agentURL, err, failStreak)
				if failStreak >= 2 {
					// sudo 必须写绝对路径：行云进程（应用中心启动）的 PATH 里没有 sudo（实测踩坑）
					cmd := exec.Command("/usr/bin/sudo", "-n", keeperSh)
					cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
					if e := cmd.Start(); e != nil {
						log.Printf("[watchdog] launch keeper.sh failed: %v", e)
					} else {
						log.Printf("[watchdog] keeper.sh launched pid=%d; keeper.sh 自带防重复锁与回滚", cmd.Process.Pid)
						go func() { _ = cmd.Wait() }()
					}
					failStreak = 0
					time.Sleep(time.Minute)
				}
			}
			time.Sleep(interval)
		}
	}()
}
