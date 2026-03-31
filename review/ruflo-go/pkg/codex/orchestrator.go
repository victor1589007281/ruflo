package codex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Platform identifies a collaboration worker runtime.
type Platform string

const (
	PlatformClaude Platform = "claude"
	PlatformCodex  Platform = "codex"
)

// DualModeOrchestrator coordinates shared memory init and external worker processes.
type DualModeOrchestrator struct {
	Namespace string
	RufloBin  string
}

func (o *DualModeOrchestrator) rufloBin() string {
	if o.RufloBin != "" {
		return o.RufloBin
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "ruflo"
}

// InitializeSharedMemory runs `ruflo memory init` so namespaces exist for collaboration.
func (o *DualModeOrchestrator) InitializeSharedMemory() error {
	cmd := exec.Command(o.rufloBin(), "memory", "init", "--force")
	cmd.Dir = workspaceDir()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex: memory init: %w", err)
	}
	return nil
}

func workspaceDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// SpawnWorker launches a lightweight coordination hook via the ruflo binary (no external AI).
func (o *DualModeOrchestrator) SpawnWorker(platform Platform, role, prompt string) error {
	label := fmt.Sprintf("%s:%s", platform, role)
	cmd := exec.Command(o.rufloBin(), "hooks", "pre-task",
		"--description", fmt.Sprintf("[%s] %s", label, prompt),
	)
	cmd.Dir = workspaceDir()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if o.Namespace != "" {
		cmd.Env = append(cmd.Env, "RUFL_NAMESPACE="+o.Namespace)
	}
	return cmd.Run()
}

// DefaultRegistryPath returns the conventional plugin registry location under the working tree.
func DefaultRegistryPath() string {
	return filepath.Join(".claude-flow", "plugins", "registry.json")
}
