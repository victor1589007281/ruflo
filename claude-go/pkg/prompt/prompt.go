// Package prompt 实现系统提示词组装。
// 对应 TS 源码: review/claude/src/utils/systemPrompt.ts
//
//	review/claude/src/constants/prompts.ts
//
// Claude Code 的系统提示词由多个部分组成，按优先级链组装:
//
//	优先级 (从高到低):
//	0. Override system prompt (覆盖一切)
//	1. Coordinator prompt (协调器模式, Manager.CoordinatorPrompt)
//	2. Agent prompt (代理定义的提示词)
//	3. Custom system prompt (--system-prompt 参数)
//	4. Default system prompt (标准 Claude Code 提示词)
//	+ appendSystemPrompt (override 以外各分支可追加)
//
//	Default prompt 的组成部分:
//	- 工具描述 (自动生成)
//	- 环境信息 (OS, shell, cwd)
//	- CLAUDE.md 记忆内容
//	- 行为指南 (编码规范, 文件操作规则)
//	- memdir/MEMORY.md 内容
package prompt

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// Manager 提示词管理器
type Manager struct {
	Cwd               string
	CustomPrompt      string // --system-prompt
	AppendPrompt      string // --append-system-prompt
	OverridePrompt    string // 覆盖提示词
	CoordinatorPrompt string // 协调器模式系统提示 (优先级低于 Override、高于 Agent)
	MemoryLoader      *memory.Loader
	AgentPrompt       string // 代理定义的提示词
	// HookConfigs 非空时在默认提示词中追加已配置 hooks 列表
	HookConfigs []types.HookConfig
	// DreamMemoryDir dream 记忆目录 (.claude/memory/)
	// 非空时，加载 consolidated.md 和 index.md 到系统提示词
	DreamMemoryDir string
	// Model 当前主循环模型名 (可选，供环境块展示)
	Model string
	// SkillListing 可选的技能清单，通常来自 skills.Registry.FormatListing()。
	SkillListing string
	// ProductName 产品显示名 (空则默认 "Claude Code (Go)")
	ProductName string
	// FastMode 是否启用快速/精简模式 (环境块展示)
	FastMode bool
}

// NewManager 创建提示词管理器
func NewManager(cwd string) *Manager {
	return &Manager{
		Cwd:          cwd,
		MemoryLoader: memory.NewLoader(cwd),
	}
}

// BuildEffectiveSystemPrompt 构建有效的系统提示词。
// 对应 TS: utils/systemPrompt.ts 中的 buildEffectiveSystemPrompt()
//
// 优先级链算法:
//
//	override > coordinator > agent > custom > default；append 在除 override 外各分支末尾追加。
func (m *Manager) BuildEffectiveSystemPrompt(tools *tool.Registry) []string {
	if m.OverridePrompt != "" {
		return []string{m.OverridePrompt}
	}

	if m.CoordinatorPrompt != "" {
		result := []string{m.CoordinatorPrompt}
		if m.AppendPrompt != "" {
			result = append(result, m.AppendPrompt)
		}
		return result
	}

	if m.AgentPrompt != "" {
		result := []string{m.AgentPrompt}
		if m.AppendPrompt != "" {
			result = append(result, m.AppendPrompt)
		}
		return result
	}

	if m.CustomPrompt != "" {
		result := []string{m.CustomPrompt}
		mcpGuidance := m.buildMCPGuidance(tools)
		if mcpGuidance != "" {
			result = append(result, mcpGuidance)
		}
		if m.AppendPrompt != "" {
			result = append(result, m.AppendPrompt)
		}
		return result
	}

	defaultPrompt := m.buildDefaultSystemPrompt(tools)
	result := []string{defaultPrompt}
	if m.AppendPrompt != "" {
		result = append(result, m.AppendPrompt)
	}
	return result
}

// buildDefaultSystemPrompt 构建默认系统提示词。
// 对应 TS: constants/prompts.ts 中的 getSystemPrompt()
//
// 组装顺序:
//  1. 身份声明
//  2. 环境信息 (OS, date, cwd, shell)
//  3. 工具使用指南
//  4. CLAUDE.md 记忆内容
//  5. 行为准则 (代码编写、文件操作、安全)
func (m *Manager) buildDefaultSystemPrompt(tools *tool.Registry) string {
	var sb strings.Builder

	// 身份声明
	sb.WriteString("You are Claude, an AI coding assistant created by Anthropic.\n")
	sb.WriteString("You are operating in a coding environment where you can use tools to help the user.\n\n")

	sb.WriteString(buildEnvironmentSection(m))

	if len(m.HookConfigs) > 0 {
		sb.WriteString(buildHooksSection(m.HookConfigs))
	}

	// 工具列表
	var mcpTools []string
	var builtinTools []string
	sb.WriteString("<available_tools>\n")
	for _, t := range tools.All() {
		name := t.Name()
		desc := firstLine(t.Description())
		sb.WriteString(fmt.Sprintf("- %s: %s\n", name, desc))
		if strings.HasPrefix(name, "mcp_") {
			mcpTools = append(mcpTools, name)
		} else {
			builtinTools = append(builtinTools, name)
		}
	}
	sb.WriteString("</available_tools>\n\n")

	if m.SkillListing != "" {
		sb.WriteString(m.SkillListing)
		sb.WriteString("\n")
	}

	// 工具使用指南
	sb.WriteString("<tool_usage>\n")
	sb.WriteString("- Use Read to read files, not shell cat/head/tail\n")
	sb.WriteString("- Use Write to create files, not shell echo/cat heredoc\n")
	sb.WriteString("- Use StrReplace to edit files, not shell sed/awk\n")
	sb.WriteString("- Use Grep to search code, not shell grep\n")
	sb.WriteString("- Use Glob to find files by pattern, not shell find\n")
	sb.WriteString("- Multiple parallel tool calls are encouraged when independent\n")
	sb.WriteString("</tool_usage>\n\n")

	// MCP 工具使用引导 (当有 MCP 工具时)
	if len(mcpTools) > 0 {
		sb.WriteString("<mcp_tool_guidance>\n")
		sb.WriteString("You have MCP (Model Context Protocol) tools available. These are EXTERNAL tools from connected servers.\n\n")
		sb.WriteString("IMPORTANT rules for using MCP tools:\n")
		sb.WriteString("1. MCP tools are for COORDINATION and EXTERNAL services, not for replacing built-in tools.\n")
		sb.WriteString("2. For complex tasks that involve MCP tools, DECOMPOSE the task into steps:\n")
		sb.WriteString("   - First understand what needs to be done\n")
		sb.WriteString("   - Use MCP tools to coordinate or fetch external data\n")
		sb.WriteString("   - Use built-in tools (Read, Write, Shell, etc.) for actual file/code operations\n")
		sb.WriteString("3. Do NOT just call one MCP tool and stop. Follow through with the complete workflow.\n")
		sb.WriteString("4. When using swarm/agent MCP tools, you still need to do the actual implementation work.\n")
		sb.WriteString("5. Prefer built-in tools for: file I/O, code search, shell commands, editing.\n")
		sb.WriteString("6. Use MCP tools for: external service integration, swarm coordination, memory retrieval.\n")
		sb.WriteString("</mcp_tool_guidance>\n\n")
	}

	sb.WriteString(buildSessionToolHints(tools))

	// 行为准则
	// 对应 TS: prompts.ts 中的 getSimpleToneAndStyleSection() + getActionsSection()
	sb.WriteString("<guidelines>\n")
	sb.WriteString("- Always read files before editing them\n")
	sb.WriteString("- Prefer editing existing files over creating new ones\n")
	sb.WriteString("- Never commit secrets or credentials\n")
	sb.WriteString("- Fix linter errors you introduce\n")
	sb.WriteString("- Only use emojis if explicitly requested\n")
	sb.WriteString("- Be concise; avoid unnecessary commentary\n")
	sb.WriteString("- Execute risky operations (delete, overwrite, git push) only with confirmation\n")
	sb.WriteString("</guidelines>\n\n")

	// Plan Mode 自动切换引导
	// 对应 TS: planModeV2.ts 中的 auto-plan 行为
	sb.WriteString("<plan_mode>\n")
	sb.WriteString("For complex tasks (multi-file changes, architectural decisions, large refactors):\n")
	sb.WriteString("1. Call EnterPlanMode first to enter read-only exploration mode\n")
	sb.WriteString("2. Read relevant files, analyze the codebase, design your approach\n")
	sb.WriteString("3. Call ExitPlanMode with a clear plan summary\n")
	sb.WriteString("4. Then execute the plan step by step\n")
	sb.WriteString("Skip plan mode for simple, single-file changes.\n")
	sb.WriteString("</plan_mode>\n\n")

	// CLAUDE.md 记忆内容
	// 对应 TS: prompts.ts 中整合 claudemd 的部分
	memoryFiles := m.MemoryLoader.LoadAll()
	if len(memoryFiles) > 0 {
		memoryPrompt := memory.BuildMemoryPrompt(memoryFiles)
		sb.WriteString(memoryPrompt)
	}

	// [NEW] Dream 长期记忆
	// 对应 TS: getAutoMemoryContent → MEMORY.md 注入到 user context
	if m.DreamMemoryDir != "" {
		dreamContent := loadDreamMemories(m.DreamMemoryDir)
		if dreamContent != "" {
			sb.WriteString("<long_term_memory>\n")
			sb.WriteString("The following is consolidated knowledge from past sessions:\n\n")
			sb.WriteString(dreamContent)
			sb.WriteString("</long_term_memory>\n\n")
		}
	}

	return sb.String()
}

// buildEnvironmentSection 组装 <environment> 块：OS、日期、cwd、shell、git、模型、产品名、快速模式。
func buildEnvironmentSection(m *Manager) string {
	var sb strings.Builder
	sb.WriteString("<environment>\n")
	product := m.ProductName
	if product == "" {
		product = "Claude Code (Go)"
	}
	sb.WriteString(fmt.Sprintf("Product: %s\n", product))
	if m.Model != "" {
		sb.WriteString(fmt.Sprintf("Model: %s\n", m.Model))
	}
	sb.WriteString(fmt.Sprintf("OS: %s %s\n", runtime.GOOS, runtime.GOARCH))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02")))
	sb.WriteString(fmt.Sprintf("Working Directory: %s\n", m.Cwd))
	if shell := os.Getenv("SHELL"); shell != "" {
		sb.WriteString(fmt.Sprintf("Shell: %s\n", shell))
	}
	if m.FastMode {
		sb.WriteString("Fast mode: enabled\n")
	} else {
		sb.WriteString("Fast mode: disabled\n")
	}
	if gitBranch := getGitBranch(m.Cwd); gitBranch != "" {
		sb.WriteString(fmt.Sprintf("Git Branch: %s\n", gitBranch))
	}
	sb.WriteString("</environment>\n\n")
	return sb.String()
}

// buildHooksSection 列出已配置的 hook 事件与命令，便于模型知晓生命周期扩展点。
func buildHooksSection(configs []types.HookConfig) string {
	var sb strings.Builder
	sb.WriteString("<configured_hooks>\n")
	for _, h := range configs {
		cmd := h.Command
		if len(cmd) > 120 {
			cmd = cmd[:117] + "..."
		}
		sb.WriteString(fmt.Sprintf("- event=%s timeout_ms=%d command=%q\n", h.Event, h.Timeout, cmd))
	}
	sb.WriteString("</configured_hooks>\n\n")
	return sb.String()
}

// buildSessionToolHints 根据当前注册的工具名生成本会话专用提示 (与静态 tool_usage 互补)。
func buildSessionToolHints(tools *tool.Registry) string {
	if tools == nil || tools.Count() == 0 {
		return ""
	}
	hints := map[string]string{
		"Read":        "Use Read to inspect file contents instead of Shell cat/head/tail.",
		"Write":       "Use Write for new files or full rewrites instead of shell redirection/heredoc.",
		"StrReplace":  "Use StrReplace for surgical edits instead of sed/awk one-liners.",
		"Glob":        "Use Glob for path patterns instead of find(1) when possible.",
		"Grep":        "Use Grep for repository search instead of spawning shell grep/rg.",
		"Shell":       "Reserve Shell for operations that have no first-class tool; prefer file tools when applicable.",
		"TodoWrite":   "Use TodoWrite to track multi-step tasks and mark progress.",
		"TaskCreate":  "Use TaskCreate / Task tools to break work into trackable sub-tasks.",
		"TaskUpdate":  "Use TaskUpdate to advance structured sub-tasks.",
		"TaskList":    "Use TaskList to inspect outstanding structured tasks.",
		"TaskGet":     "Use TaskGet to fetch details for a specific task id.",
		"WebFetch":    "Use WebFetch to retrieve URL bodies when you need page content.",
		"WebSearch":   "Use WebSearch for open-web discovery instead of guessing URLs.",
		"SendMessage": "Use SendMessage when coordinating with teammate agents.",
		"TeamCreate":  "Use TeamCreate / team tools for multi-agent sessions when available.",
		"ToolSearch":  "Use ToolSearch to discover lesser-used tools instead of guessing names.",
		"Skill":       "Use Skill to load project-specific skill instructions when present.",
	}
	names := tools.Names()
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	var sb strings.Builder
	sb.WriteString("<session_tool_hints>\n")
	sb.WriteString("This session registers the following tools; prefer them as indicated:\n")
	wrote := false
	for _, n := range names {
		if hint, ok := hints[n]; ok {
			sb.WriteString(fmt.Sprintf("- %s\n", hint))
			wrote = true
		}
	}
	// 通用子任务提示：若存在 Agent 相关工具名变体
	if _, ok := nameSet["Agent"]; ok {
		sb.WriteString("- Use Agent (or Task) for delegated sub-work rather than overloading a single turn.\n")
		wrote = true
	}
	if !wrote {
		sb.WriteString("- Follow <tool_usage> and <available_tools> for this session.\n")
	}
	sb.WriteString("</session_tool_hints>\n\n")
	return sb.String()
}

// buildMCPGuidance 提取 MCP 工具引导段，供 CustomPrompt 模式追加。
func (m *Manager) buildMCPGuidance(tools *tool.Registry) string {
	if tools == nil {
		return ""
	}
	var mcpTools []string
	for _, t := range tools.All() {
		if strings.HasPrefix(t.Name(), "mcp_") {
			mcpTools = append(mcpTools, t.Name())
		}
	}
	if len(mcpTools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<mcp_tool_guidance>\n")
	sb.WriteString("You have MCP (Model Context Protocol) tools available. These are EXTERNAL tools from connected servers.\n\n")
	sb.WriteString("IMPORTANT rules for using MCP tools:\n")
	sb.WriteString("1. MCP tools are for COORDINATION and EXTERNAL services, not for replacing built-in tools.\n")
	sb.WriteString("2. For complex tasks that involve MCP tools, DECOMPOSE the task into steps.\n")
	sb.WriteString("3. Do NOT just call one MCP tool and stop. Follow through with the complete workflow.\n")
	sb.WriteString("4. When using swarm/agent MCP tools, you still need to do the actual implementation work.\n")
	sb.WriteString("5. Prefer built-in tools for: file I/O, code search, shell commands, editing.\n")
	sb.WriteString("6. Use MCP tools for: external service integration, swarm coordination, memory retrieval.\n")
	sb.WriteString("</mcp_tool_guidance>\n")
	return sb.String()
}

// firstLine 返回字符串的第一行
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// loadDreamMemories 加载 dream 记忆文件
// 优先读 consolidated.md (LLM 整理结果)，否则读所有 memory-*.md
func loadDreamMemories(dir string) string {
	// 优先读取 LLM 整理的结果
	consolidated := filepath.Join(dir, "consolidated.md")
	if data, err := os.ReadFile(consolidated); err == nil {
		content := string(data)
		if len(content) > 25000 {
			content = content[:25000] + "\n...(truncated)"
		}
		return content
	}

	// 回退到读取 memory-*.md 文件
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var sb strings.Builder
	totalBytes := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") || entry.Name() == "index.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > 2000 {
			content = content[:2000] + "..."
		}
		if totalBytes+len(content) > 25000 {
			break
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
		totalBytes += len(content)
	}
	return sb.String()
}

// getGitBranch 获取当前 git 分支名
// 对应 TS: computeSimpleEnvInfo() 中的 git 检测逻辑
func getGitBranch(cwd string) string {
	// 先检查是否是 git 仓库
	if _, err := os.Stat(filepath.Join(cwd, ".git")); os.IsNotExist(err) {
		return ""
	}
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
