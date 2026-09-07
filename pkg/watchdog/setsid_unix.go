//go:build !windows

package watchdog

import (
	"os/exec"
	"syscall"
)

// setSetsid 让子进程脱离会话组（NAS 上 keeper.sh 不随行云进程退出而终止）。
func setSetsid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
