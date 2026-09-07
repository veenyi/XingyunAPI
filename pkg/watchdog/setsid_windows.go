//go:build windows

package watchdog

import "os/exec"

// setSetsid 在 Windows 上无会话语义，watchdog 仅编译可达（keeper.sh 只跑 NAS）。
func setSetsid(cmd *exec.Cmd) {}
