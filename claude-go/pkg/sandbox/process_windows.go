//go:build windows

package sandbox

import (
	"os"
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {}

func killProcessTree(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}
