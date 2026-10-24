//go:build !windows

package sandbox

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup 让子进程自成进程组 (Setpgid), 便于按组回收整棵进程树。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree 先对负 pid 发 SIGKILL 杀掉整个进程组, 再兜底杀主进程本身。
func killProcessTree(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	_ = p.Kill()
}
