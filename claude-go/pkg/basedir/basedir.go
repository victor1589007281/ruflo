// Package basedir 统一管理 claude-go 所有模块的数据目录。
//
// 每个模块的数据目录都从一个可配置的根目录 (StateDir) 派生，
// 默认值为工作目录下的 ".claude-go"。程序启动时通过 EnsureAll
// 幂等创建所有子目录。
package basedir

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout 定义所有模块子目录的绝对路径。
type Layout struct {
	Root      string // 根目录 (如 /path/.claude-go)
	Logs      string // 日志目录
	Memory    string // 记忆存储
	Teams     string // 团队数据
	Cron      string // 定时任务
	Evolution string // 进化引擎
	Tasks     string // V2 Task 存储
	Skills    string // 技能目录
	Wiki      string // Wiki 知识库
	Hooks     string // Hook 脚本
	Vision    string // 视觉能力缓存
	Agents    string // Agent 角色定义
}

// NewLayout 根据根目录创建完整的目录布局。
// root 不能为空。
func NewLayout(root string) (*Layout, error) {
	if root == "" {
		return nil, fmt.Errorf("basedir: root 不能为空，请在配置中指定 stateDir")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("basedir: 解析路径失败: %w", err)
	}
	return &Layout{
		Root:      abs,
		Logs:      filepath.Join(abs, "logs"),
		Memory:    filepath.Join(abs, "memory"),
		Teams:     filepath.Join(abs, "teams"),
		Cron:      filepath.Join(abs, "cron"),
		Evolution: filepath.Join(abs, "evolution"),
		Tasks:     filepath.Join(abs, "tasks"),
		Skills:    filepath.Join(abs, "skills"),
		Wiki:      filepath.Join(abs, "wiki"),
		Hooks:     filepath.Join(abs, "hooks"),
		Vision:    filepath.Join(abs, "vision"),
		Agents:    filepath.Join(abs, "agents"),
	}, nil
}

// EnsureAll 幂等创建所有子目录 (0755 权限)。
func (l *Layout) EnsureAll() error {
	dirs := []string{
		l.Root, l.Logs, l.Memory, l.Teams, l.Cron,
		l.Evolution, l.Tasks, l.Skills, l.Wiki,
		l.Hooks, l.Vision, l.Agents,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("basedir: 创建目录 %s 失败: %w", d, err)
		}
	}
	return nil
}

// TasksFilePath 返回 tasks.json 的完整路径。
func (l *Layout) TasksFilePath() string {
	return filepath.Join(l.Tasks, "tasks.json")
}

// ResolveDefault 从 Cwd 推导默认的 StateDir (Cwd/.claude-go)。
// 如果 stateDir 非空则直接使用，否则使用 cwd/.claude-go。
func ResolveDefault(stateDir, cwd string) string {
	if stateDir != "" {
		return stateDir
	}
	if cwd != "" {
		return filepath.Join(cwd, ".claude-go")
	}
	return ".claude-go"
}
