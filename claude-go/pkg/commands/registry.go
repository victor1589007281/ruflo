package commands

import (
	"fmt"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/session"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
	"github.com/anthropic/claude-go/pkg/wiki"
)

// CommandType 命令类型: prompt 发给 LLM, local 本地执行。
type CommandType string

const (
	CommandTypePrompt CommandType = "prompt"
	CommandTypeLocal  CommandType = "local"
)

// Command 斜杠命令定义。
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Type        CommandType
	IsHidden    bool
	ArgHint     string // 参数提示 e.g. "[model]"
	Execute     func(args string, ctx *CommandContext) error
}

// CommandContext 命令执行上下文。
type CommandContext struct {
	Engine       *engine.QueryEngine
	SessionStore *session.SessionStore
	History      *session.PromptHistory
	TeamMgr      *agent.ProductionTeamManager  // 团队管理器 (可选)
	WikiEngine   *wiki.Engine                  // Wiki 引擎 (可选)
	SwarmIntel   *swarm_intel.Engine            // 群体智能引擎 (可选)
	Cwd          string
	OnClear      func()
	OnExit       func()
}

// Registry 命令注册表。
type Registry struct {
	commands map[string]*Command
	aliases  map[string]string // alias → canonical name
}

// NewRegistry 创建命令注册表。
func NewRegistry() *Registry {
	return &Registry{
		commands: make(map[string]*Command),
		aliases:  make(map[string]string),
	}
}

// Register 注册一个命令。
func (r *Registry) Register(cmd *Command) {
	r.commands[cmd.Name] = cmd
	for _, alias := range cmd.Aliases {
		r.aliases[alias] = cmd.Name
	}
}

// Find 按名称或别名查找命令。
func (r *Registry) Find(name string) *Command {
	name = strings.TrimPrefix(name, "/")
	if cmd, ok := r.commands[name]; ok {
		return cmd
	}
	if canonical, ok := r.aliases[name]; ok {
		return r.commands[canonical]
	}
	return nil
}

// All 返回所有可见命令 (按名称排序)。
func (r *Registry) All() []*Command {
	var cmds []*Command
	for _, cmd := range r.commands {
		if !cmd.IsHidden {
			cmds = append(cmds, cmd)
		}
	}
	sort.Slice(cmds, func(i, j int) bool {
		return cmds[i].Name < cmds[j].Name
	})
	return cmds
}

// CommandNames 返回所有命令名 + 别名列表 (用于 tab 补全)。
func (r *Registry) CommandNames() []string {
	var names []string
	for name := range r.commands {
		names = append(names, "/"+name)
	}
	for alias := range r.aliases {
		names = append(names, "/"+alias)
	}
	sort.Strings(names)
	return names
}

// ParseSlashCommand 解析 "/cmd args" 格式。
func ParseSlashCommand(input string) (cmd string, args string) {
	input = strings.TrimPrefix(input, "/")
	parts := strings.SplitN(input, " ", 2)
	cmd = strings.ToLower(parts[0])
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return
}

// PrintHelp 打印所有命令帮助。
func (r *Registry) PrintHelp() {
	fmt.Println("可用命令:")
	fmt.Println()
	for _, cmd := range r.All() {
		argStr := ""
		if cmd.ArgHint != "" {
			argStr = " " + cmd.ArgHint
		}
		aliasStr := ""
		if len(cmd.Aliases) > 0 {
			aliasStr = " (别名: /" + strings.Join(cmd.Aliases, ", /") + ")"
		}
		fmt.Printf("  /%-15s %s%s\n", cmd.Name+argStr, cmd.Description, aliasStr)
	}
	fmt.Println()
}
