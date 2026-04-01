// Package codex 实现 Claude Code 与 OpenAI Codex 的「双模编排」协调层（与 Ruflo CLI 配合）。
//
// 设计思路：
//   - 不直接嵌入各 AI 的 SDK，而是通过 `ruflo` 可执行文件调用 memory / hooks 子命令，初始化共享命名空间、
//     记录任务上下文，实现跨进程、跨工具的松耦合协作。
//   - DualModeOrchestrator 持有 Namespace（默认可与模板中的 collaboration 对齐）与 RufloBin（可显式指定，
//     否则用 os.Executable 或 "ruflo"）。
//   - SpawnWorker 用 hooks pre-task 做轻量协调（非完整子代理）；无头多平台并发见 dual_mode.go 中
//     SpawnHeadlessWorker / RunCollaboration。
//
// 与 templates.go 中预置流水线组合使用：模板产出 WorkerConfig 切片，再交给 RunCollaboration 按依赖拓扑执行。
package codex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Platform 标识协作工作负载所运行的平台（用于语义分层与 CLI 选择）。
type Platform string

const (
	PlatformClaude Platform = "claude" // Claude Code 侧
	PlatformCodex  Platform = "codex"  // Codex 侧
)

// DualModeOrchestrator 协调共享记忆初始化与外部工作进程（通过 ruflo 二进制与可选无头 claude/codex）。
type DualModeOrchestrator struct {
	Namespace string // 记忆与 hooks 使用的命名空间，注入为环境变量 RUFL_NAMESPACE
	RufloBin  string // ruflo 可执行路径；空则自动解析
}

// rufloBin 返回实际调用的 ruflo 路径：优先 RufloBin，其次 os.Executable，最后 "ruflo"。
func (o *DualModeOrchestrator) rufloBin() string {
	if o.RufloBin != "" {
		return o.RufloBin
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "ruflo"
}

// InitializeSharedMemory 执行 `ruflo memory init --force`，确保协作所用命名空间对应存储已初始化。
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

// workspaceDir 返回当前工作目录；失败时为 "."。
func workspaceDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// SpawnWorker 通过 ruflo hooks pre-task 触发轻量协调（不启动完整外部 AI 进程），描述串带 [platform:role] 前缀。
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

// DefaultRegistryPath 返回工作区内约定插件注册表路径（.claude-flow/plugins/registry.json）。
func DefaultRegistryPath() string {
	return filepath.Join(".claude-flow", "plugins", "registry.json")
}
