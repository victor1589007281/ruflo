// wrapper.go — 外部 CLI 工具路径发现与公共辅助。
package codeintel

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ToolPaths 缓存已发现的外部工具路径。
type ToolPaths struct {
	NodePath     string // Node.js 可执行文件路径
	GitNexusPath string // gitnexus CLI 入口 (index.js)
	GraphifyPath string // graphify 可执行文件路径
}

var globalPaths *ToolPaths

// DiscoverTools 探测外部工具路径（首次调用时缓存）。
func DiscoverTools() (*ToolPaths, error) {
	if globalPaths != nil {
		return globalPaths, nil
	}

	node, err := findNode()
	if err != nil {
		return nil, err
	}

	gitnexus, err := findGitNexus(node)
	if err != nil {
		return nil, err
	}

	graphify, err := findGraphify()
	if err != nil {
		return nil, err
	}

	globalPaths = &ToolPaths{
		NodePath:     node,
		GitNexusPath: gitnexus,
		GraphifyPath: graphify,
	}
	return globalPaths, nil
}

// ResetToolPaths 清除缓存（测试或重新探测时使用）。
func ResetToolPaths() {
	globalPaths = nil
}

// ============================================================================
// 路径发现
// ============================================================================

func findNode() (string, error) {
	if p, err := exec.LookPath("node"); err == nil {
		return p, nil
	}
	// 常见备用路径
	candidates := []string{
		"/usr/local/bin/node",
		"/usr/bin/node",
		"/opt/node/bin/node",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".nvm/versions/node/current/bin/node"),
			filepath.Join(home, "node/bin/node"),
		)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("node.js not found in PATH or common locations")
}

func findGitNexus(node string) (string, error) {
	// 1. 尝试 npm root -g
	npmRoot, _ := exec.Command(node, filepath.Join(filepath.Dir(node), "npm"), "root", "-g").Output()
	if npmRoot != nil {
		root := strings.TrimSpace(string(npmRoot))
		p := filepath.Join(root, "gitnexus", "dist", "cli", "index.js")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	// 2. npx 缓存路径
	if home, err := os.UserHomeDir(); err == nil {
		npxCache := filepath.Join(home, ".npm/_npx")
		if entries, err := os.ReadDir(npxCache); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					p := filepath.Join(npxCache, e.Name(), "node_modules/gitnexus/dist/cli/index.js")
					if _, err := os.Stat(p); err == nil {
						return p, nil
					}
				}
			}
		}
	}

	// 3. 常见全局安装路径
	candidates := []string{
		"/usr/local/lib/node_modules/gitnexus/dist/cli/index.js",
		"/usr/lib/node_modules/gitnexus/dist/cli/index.js",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".nvm/versions/node/current/lib/node_modules/gitnexus/dist/cli/index.js"),
			filepath.Join(home, "base/node/lib/node_modules/gitnexus/dist/cli/index.js"),
			filepath.Join(home, "node/lib/node_modules/gitnexus/dist/cli/index.js"),
		)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("gitnexus CLI not found; run: npm install -g gitnexus")
}

func findGraphify() (string, error) {
	if p, err := exec.LookPath("graphify"); err == nil {
		return p, nil
	}
	candidates := []string{
		"/usr/local/bin/graphify",
		"/usr/bin/graphify",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local/bin/graphify"),
		)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("graphify not found; run: pip3 install graphifyy")
}

// ============================================================================
// 执行辅助
// ============================================================================

// runTool 在指定目录下运行外部命令，返回 stdout/stderr。
func runTool(dir string, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// runWithTimeout 带超时的命令执行。
// 使用进程组确保超时后能清理所有子进程（防止 Node.js worker 孤儿化阻塞管道）。
func runWithTimeout(dir string, timeout time.Duration, name string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := execTimeout(timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// 创建新进程组，便于超时后批量清理子进程
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	// 若因超时退出，强制杀死整个进程组（SIGKILL 给进程组）
	if ctx.Err() != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func execTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
