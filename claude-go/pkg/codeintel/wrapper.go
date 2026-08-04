// wrapper.go — 外部 CLI 工具路径发现与公共辅助。
package codeintel

import (
	"bytes"
	"context"
	"fmt"
	"log"
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
//
// 优先读取环境变量覆盖（CLAUDE_GO_NODE_PATH / CLAUDE_GO_GITNEXUS_PATH /
// CLAUDE_GO_GRAPHIFY_PATH）—— 多实例共享 PV 部署时，node/gitnexus/graphify
// 注入到共享卷，容器内不必重新安装。env 覆盖路径下 graphify 缺失仅告警不致命
// （gitnexus 仍可用，graphify 相关查询返回明确的未安装错误）。
func DiscoverTools() (*ToolPaths, error) {
	if globalPaths != nil {
		return globalPaths, nil
	}

	if os.Getenv("CLAUDE_GO_NODE_PATH") != "" ||
		os.Getenv("CLAUDE_GO_GITNEXUS_PATH") != "" ||
		os.Getenv("CLAUDE_GO_GRAPHIFY_PATH") != "" {
		paths := &ToolPaths{}
		if p := os.Getenv("CLAUDE_GO_NODE_PATH"); p != "" {
			paths.NodePath = p
		} else if n, err := findNode(); err == nil {
			paths.NodePath = n
		}
		if g := os.Getenv("CLAUDE_GO_GITNEXUS_PATH"); g != "" {
			paths.GitNexusPath = g
		} else if paths.NodePath != "" {
			if gn, err := findGitNexus(paths.NodePath); err == nil {
				paths.GitNexusPath = gn
			}
		}
		if gf := os.Getenv("CLAUDE_GO_GRAPHIFY_PATH"); gf != "" {
			paths.GraphifyPath = gf
		} else if g, err := findGraphify(); err == nil {
			paths.GraphifyPath = g
		}
		if err := validatePaths(paths); err != nil {
			return nil, err
		}
		globalPaths = paths
		return paths, nil
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

// validatePaths 校验工具路径可用性。node + gitnexus 是硬依赖；graphify 缺失
// 仅告警（降级模式），避免单工具缺失拖垮整个 codeintel。
func validatePaths(p *ToolPaths) error {
	if p.NodePath == "" {
		return fmt.Errorf("node.js not found; set CLAUDE_GO_NODE_PATH or put node in PATH")
	}
	if _, err := os.Stat(p.NodePath); err != nil {
		return fmt.Errorf("node path %s: %w", p.NodePath, err)
	}
	if p.GitNexusPath == "" {
		return fmt.Errorf("gitnexus CLI not found; set CLAUDE_GO_GITNEXUS_PATH or run: npm install -g gitnexus")
	}
	if _, err := os.Stat(p.GitNexusPath); err != nil {
		return fmt.Errorf("gitnexus path %s: %w", p.GitNexusPath, err)
	}
	if p.GraphifyPath != "" {
		if _, err := os.Stat(p.GraphifyPath); err != nil {
			return fmt.Errorf("graphify path %s: %w", p.GraphifyPath, err)
		}
	} else {
		log.Printf("[codeintel] graphify not found — 降级为 gitnexus-only（graphify 查询将返回错误）")
	}
	return nil
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
	return runWithTimeoutEnv(dir, timeout, nil, name, args...)
}

// runWithTimeoutEnv 带超时和额外环境变量的命令执行。
// env 中的条目会追加到当前进程环境变量之后（同名变量后出现的优先）。
func runWithTimeoutEnv(dir string, timeout time.Duration, env []string, name string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := execTimeout(timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
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
