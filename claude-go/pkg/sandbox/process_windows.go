//go:build windows

package sandbox

import (
	"os"
	"os/exec"
)

// setProcessGroup Windows 下是空实现: 没有 POSIX 进程组可设, 隔离/回收语义由别处承担。
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree Windows 下只杀进程本身 (无进程树遍历), 孙进程不会被一并回收。
func killProcessTree(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}
