package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

var startTime = time.Now()

// RegisterBuiltins 注册所有内置命令。
func RegisterBuiltins(r *Registry) {
	r.Register(&Command{
		Name:        "help",
		Description: "显示所有可用命令",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			r.PrintHelp()
			return nil
		},
	})

	r.Register(&Command{
		Name:        "exit",
		Aliases:     []string{"quit", "q"},
		Description: "退出交互式模式",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.OnExit != nil {
				ctx.OnExit()
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "clear",
		Aliases:     []string{"reset", "new"},
		Description: "清空对话历史, 开始新会话",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine != nil {
				ctx.Engine.ClearMessages()
			}
			if ctx.OnClear != nil {
				ctx.OnClear()
			}
			fmt.Println("[对话已清空]")
			return nil
		},
	})

	r.Register(&Command{
		Name:        "compact",
		Description: "压缩对话上下文",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil || ctx.Engine.Compactor == nil {
				fmt.Println("[compact 不可用]")
				return nil
			}
			fmt.Println("[正在压缩上下文...]")
			return nil
		},
	})

	r.Register(&Command{
		Name:        "model",
		ArgHint:     "[name]",
		Description: "查看或切换当前模型",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil {
				return nil
			}
			if args == "" {
				fmt.Printf("当前模型: %s\n", ctx.Engine.Config.Model)
				return nil
			}
			old := ctx.Engine.Config.Model
			ctx.Engine.Config.Model = args
			fmt.Printf("模型已切换: %s → %s\n", old, args)
			return nil
		},
	})

	r.Register(&Command{
		Name:        "status",
		Description: "显示系统状态 (版本、模型、工具、会话)",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			fmt.Println("╔══════════════════════════════════════╗")
			fmt.Println("║      Claude Code (Go) Status        ║")
			fmt.Println("╚══════════════════════════════════════╝")
			if ctx.Engine != nil {
				fmt.Printf("  模型:     %s\n", ctx.Engine.Config.Model)
				fmt.Printf("  工作目录: %s\n", ctx.Engine.Config.Cwd)
				fmt.Printf("  权限模式: %s\n", ctx.Engine.Config.PermissionMode)
				fmt.Printf("  消息数:   %d\n", len(ctx.Engine.Messages))
				if ctx.Engine.Tools != nil {
					fmt.Printf("  工具数:   %d\n", ctx.Engine.Tools.Count())
				}
			}
			if ctx.SessionStore != nil {
				fmt.Printf("  会话 ID:  %s\n", ctx.SessionStore.SessionID())
			}
			fmt.Printf("  运行时间: %s\n", time.Since(startTime).Round(time.Second))
			fmt.Printf("  Go 版本:  %s\n", runtime.Version())
			return nil
		},
	})

	r.Register(&Command{
		Name:        "cost",
		Description: "显示 token 消耗和会话时长",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil {
				return nil
			}
			usage := ctx.Engine.GetContextUsage()
			fmt.Printf("Token 消耗:\n")
			fmt.Printf("  输入:  %d tokens\n", usage.InputTokens)
			fmt.Printf("  输出:  %d tokens\n", usage.OutputTokens)
			fmt.Printf("  合计:  %d tokens\n", usage.TotalTokens)
			fmt.Printf("  预算:  %.1f%% (%d / %d)\n", usage.Percentage, usage.TotalTokens, usage.Budget)
			fmt.Printf("  时长:  %s\n", time.Since(startTime).Round(time.Second))
			return nil
		},
	})

	r.Register(&Command{
		Name:        "session",
		Description: "显示当前会话信息",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.SessionStore == nil {
				fmt.Println("[会话持久化未启用]")
				return nil
			}
			fmt.Printf("会话 ID:   %s\n", ctx.SessionStore.SessionID())
			if ctx.Engine != nil {
				fmt.Printf("消息数:    %d\n", len(ctx.Engine.Messages))
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "resume",
		ArgHint:     "[sessionID]",
		Description: "恢复指定会话",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.SessionStore == nil {
				fmt.Println("[会话持久化未启用]")
				return nil
			}
			if args == "" {
				metas, err := ctx.SessionStore.ListSessions()
				if err != nil || len(metas) == 0 {
					fmt.Println("[无可用会话]")
					return nil
				}
				fmt.Println("最近会话:")
				for i, m := range metas {
					if i >= 10 {
						break
					}
					prompt := m.FirstPrompt
					if prompt == "" {
						prompt = "(空)"
					}
					fmt.Printf("  %d. %s  [%d条] %s  \"%s\"\n",
						i+1, m.SessionID[:12]+"...", m.MessageCount,
						m.LastActive.Format("01-02 15:04"), prompt)
				}
				return nil
			}
			msgs, err := ctx.SessionStore.ResumeSession(args)
			if err != nil {
				fmt.Printf("[恢复失败: %v]\n", err)
				return nil
			}
			if ctx.Engine != nil {
				ctx.Engine.Messages = msgs
			}
			fmt.Printf("[已恢复会话 %s, %d 条消息]\n", args, len(msgs))
			return nil
		},
	})

	r.Register(&Command{
		Name:        "diff",
		Description: "显示未提交的 Git 变更",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			cmd := exec.Command("git", "diff", "--stat")
			cmd.Dir = ctx.Cwd
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Printf("[git diff 失败: %v]\n", err)
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "doctor",
		Description: "诊断系统环境",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			checks := []struct {
				name string
				cmd  string
				args []string
			}{
				{"git", "git", []string{"--version"}},
				{"rg (ripgrep)", "rg", []string{"--version"}},
				{"node", "node", []string{"--version"}},
			}
			for _, c := range checks {
				out, err := exec.Command(c.cmd, c.args...).Output()
				if err != nil {
					fmt.Printf("  ✗ %s: 未安装\n", c.name)
				} else {
					fmt.Printf("  ✓ %s: %s\n", c.name, strings.TrimSpace(string(out)))
				}
			}
			if ctx.Engine != nil && ctx.Engine.Config.Model != "" {
				fmt.Printf("  ✓ 模型: %s\n", ctx.Engine.Config.Model)
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "permissions",
		Description: "查看当前权限模式",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil {
				return nil
			}
			fmt.Printf("权限模式: %s\n", ctx.Engine.Config.PermissionMode)
			return nil
		},
	})

	r.Register(&Command{
		Name:        "memory",
		Description: "打开 CLAUDE.md 编辑 (或显示内容)",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			path := "CLAUDE.md"
			if _, err := os.Stat(path); os.IsNotExist(err) {
				fmt.Println("[CLAUDE.md 不存在]")
				return nil
			}
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vi"
			}
			cmd := exec.Command(editor, path)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		},
	})

	r.Register(&Command{
		Name:        "config",
		Description: "显示当前配置",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil || ctx.Engine.Config == nil {
				return nil
			}
			cfg := ctx.Engine.Config
			data, _ := json.MarshalIndent(map[string]interface{}{
				"model":          cfg.Model,
				"maxTokens":      cfg.MaxTokens,
				"maxTurns":       cfg.MaxTurns,
				"permissionMode": cfg.PermissionMode,
				"cwd":            cfg.Cwd,
				"debug":          cfg.Debug,
			}, "", "  ")
			fmt.Println(string(data))
			return nil
		},
	})

	r.Register(&Command{
		Name:        "copy",
		ArgHint:     "[n]",
		Description: "复制最后一条 (或第 n 条) 回复到剪贴板",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil || len(ctx.Engine.Messages) == 0 {
				fmt.Println("[无消息可复制]")
				return nil
			}
			var lastText string
			for i := len(ctx.Engine.Messages) - 1; i >= 0; i-- {
				msg := ctx.Engine.Messages[i]
				if msg.Type == types.MessageTypeAssistant {
					for _, b := range msg.Content {
						if b.Type == types.ContentBlockText {
							lastText += b.Text
						}
					}
					break
				}
			}
			if lastText == "" {
				fmt.Println("[无文本可复制]")
				return nil
			}
			cmd := exec.Command("pbcopy")
			cmd.Stdin = strings.NewReader(lastText)
			if err := cmd.Run(); err != nil {
				cmd = exec.Command("xclip", "-selection", "clipboard")
				cmd.Stdin = strings.NewReader(lastText)
				if err := cmd.Run(); err != nil {
					fmt.Printf("[复制失败 (无 pbcopy/xclip): %v]\n", err)
					return nil
				}
			}
			fmt.Printf("[已复制 %d 字符到剪贴板]\n", len(lastText))
			return nil
		},
	})

	r.Register(&Command{
		Name:        "export",
		ArgHint:     "[filename]",
		Description: "导出对话记录到文件",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil || len(ctx.Engine.Messages) == 0 {
				fmt.Println("[无消息可导出]")
				return nil
			}
			filename := args
			if filename == "" {
				filename = fmt.Sprintf("conversation-%s.md", time.Now().Format("20060102-150405"))
			}
			var sb strings.Builder
			sb.WriteString("# Conversation Export\n\n")
			for _, msg := range ctx.Engine.Messages {
				role := "User"
				if msg.Type == types.MessageTypeAssistant {
					role = "Assistant"
				}
				sb.WriteString(fmt.Sprintf("## %s\n\n", role))
				for _, b := range msg.Content {
					if b.Type == types.ContentBlockText {
						sb.WriteString(b.Text)
						sb.WriteString("\n\n")
					}
				}
			}
			if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
				fmt.Printf("[导出失败: %v]\n", err)
				return nil
			}
			fmt.Printf("[已导出到 %s]\n", filename)
			return nil
		},
	})

	r.Register(&Command{
		Name:        "context",
		Description: "显示上下文使用情况",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil {
				return nil
			}
			usage := ctx.Engine.GetContextUsage()
			barLen := 40
			filled := int(usage.Percentage / 100 * float64(barLen))
			if filled > barLen {
				filled = barLen
			}

			barColor := "\033[32m" // green
			if usage.Percentage > 80 {
				barColor = "\033[31m" // red
			} else if usage.Percentage > 60 {
				barColor = "\033[33m" // yellow
			}
			bar := barColor + strings.Repeat("█", filled) + "\033[0m" + strings.Repeat("░", barLen-filled)
			fmt.Printf("上下文使用: [%s] %.1f%%\n", bar, usage.Percentage)
			fmt.Printf("  消息数:    %d\n", usage.MessageCount)
			fmt.Printf("  输入 Token: %d\n", usage.InputTokens)
			fmt.Printf("  输出 Token: %d\n", usage.OutputTokens)
			fmt.Printf("  合计:      %d / %d\n", usage.TotalTokens, usage.Budget)
			if usage.Percentage > 80 {
				fmt.Println("  \033[31m⚠️ 上下文即将用完, 建议执行 /compact\033[0m")
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "files",
		Description: "列出上下文中涉及的文件",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine == nil {
				return nil
			}
			files := make(map[string]bool)
			for _, msg := range ctx.Engine.Messages {
				for _, b := range msg.Content {
					if b.Type == types.ContentBlockToolUse {
						var input map[string]interface{}
						if json.Unmarshal(b.Input, &input) == nil {
							for _, key := range []string{"path", "file_path", "filePath", "target_file"} {
								if v, ok := input[key]; ok {
									if s, ok := v.(string); ok {
										files[s] = true
									}
								}
							}
						}
					}
				}
			}
			if len(files) == 0 {
				fmt.Println("[上下文中无文件引用]")
				return nil
			}
			fmt.Printf("上下文涉及的文件 (%d):\n", len(files))
			for f := range files {
				fmt.Printf("  %s\n", f)
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "debug",
		Description: "切换调试模式",
		Type:        CommandTypeLocal,
		IsHidden:    true,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.Engine != nil {
				ctx.Engine.Config.Debug = !ctx.Engine.Config.Debug
				fmt.Printf("[调试模式: %v]\n", ctx.Engine.Config.Debug)
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "history",
		Description: "显示输入历史",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.History == nil {
				fmt.Println("[历史记录不可用]")
				return nil
			}
			prompts := ctx.History.Prompts()
			start := 0
			if len(prompts) > 20 {
				start = len(prompts) - 20
			}
			for i := start; i < len(prompts); i++ {
				p := prompts[i]
				if len(p) > 80 {
					p = p[:80] + "..."
				}
				fmt.Printf("  %d. %s\n", i+1, p)
			}
			return nil
		},
	})

	RegisterTeamCommands(r)
}
