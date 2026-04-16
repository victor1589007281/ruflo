package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/swarm_intel"
)

// RegisterTeamCommands 注册 /team, /go, /wiki 命令。
func RegisterTeamCommands(r *Registry) {
	r.Register(&Command{
		Name:        "team",
		ArgHint:     "[create|run|status|stop|list|delete|workflows]",
		Description: "团队管理 (create/run/status/stop/list/delete/workflows)",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.TeamMgr == nil {
				fmt.Println("[团队管理器未初始化]")
				return nil
			}
			parts := strings.Fields(args)
			if len(parts) == 0 {
				fmt.Println("用法: /team [create|run|status|stop|list|delete|workflows]")
				return nil
			}
			sub := strings.ToLower(parts[0])
			switch sub {
			case "create":
				if len(parts) < 3 {
					fmt.Println("用法: /team create <名称> <工作流>")
					fmt.Println("工作流: development, research, debate, creative, finance, techblog, swarm")
					return nil
				}
				name, workflow := parts[1], parts[2]
				desc := ""
				if len(parts) > 3 {
					desc = strings.Join(parts[3:], " ")
				}
				team, err := ctx.TeamMgr.CreateTeam(name, workflow, desc, "cli")
				if err != nil {
					fmt.Printf("[创建失败: %v]\n", err)
					return nil
				}
				fmt.Printf("✅ 团队 **%s** 已创建 (工作流: %s, Agent: %d)\n", team.Name, team.Workflow, len(team.Agents))
				fmt.Printf("发送 /team run %s <目标> 启动执行\n", team.Name)

			case "run":
				if len(parts) < 3 {
					fmt.Println("用法: /team run <名称> <目标描述>")
					return nil
				}
				name := parts[1]
				objective := strings.Join(parts[2:], " ")
				if err := ctx.TeamMgr.RunTeam(name, objective); err != nil {
					fmt.Printf("[启动失败: %v]\n", err)
					return nil
				}
				fmt.Printf("🚀 团队 **%s** 已启动, 后台执行中...\n", name)

			case "status":
				if len(parts) >= 2 {
					team := ctx.TeamMgr.GetTeam(parts[1])
					if team == nil {
						fmt.Printf("[团队 %q 不存在]\n", parts[1])
						return nil
					}
					fmt.Println(team.FormatStatus())
				} else {
					teams := ctx.TeamMgr.ListAllTeams()
					if len(teams) == 0 {
						fmt.Println("[无活跃团队]")
						return nil
					}
					fmt.Println("所有团队:")
					for _, t := range teams {
						fmt.Printf("  - %s [%s] 工作流=%s\n", t.Name, t.Status, t.Workflow)
					}
				}

			case "stop":
				if len(parts) < 2 {
					fmt.Println("用法: /team stop <名称>")
					return nil
				}
				if err := ctx.TeamMgr.StopTeam(parts[1]); err != nil {
					fmt.Printf("[停止失败: %v]\n", err)
				} else {
					fmt.Printf("⏹️ 团队 %s 已停止\n", parts[1])
				}

			case "list":
				teams := ctx.TeamMgr.ListAllTeams()
				if len(teams) == 0 {
					fmt.Println("[无团队]")
					return nil
				}
				fmt.Println("团队列表:")
				for _, t := range teams {
					fmt.Printf("  - %s [%s] %s\n", t.Name, t.Status, t.Workflow)
				}

			case "delete":
				if len(parts) < 2 {
					fmt.Println("用法: /team delete <名称>")
					return nil
				}
				if err := ctx.TeamMgr.DeleteTeam(parts[1]); err != nil {
					fmt.Printf("[删除失败: %v]\n", err)
				} else {
					fmt.Printf("🗑️ 团队 %s 已删除\n", parts[1])
				}

			case "workflows":
				fmt.Println("可用工作流:")
				fmt.Println("  development  — 研发团队 (architect → coder → tester → reviewer)")
				fmt.Println("  research     — 研究团队 (researcher → analyst → writer)")
				fmt.Println("  debate       — 辩论团队 (正方 → 反方 → 裁判)")
				fmt.Println("  creative     — 创意团队 (designer → illustrator → reviewer)")
				fmt.Println("  finance      — 金融分析 (analyst → strategist → reviewer)")
				fmt.Println("  techblog     — 技术博客 (researcher → writer → editor)")
				fmt.Println("  swarm        — 蜂群模式 (LLM 自动分解并行)")

			default:
				fmt.Printf("未知子命令: %s\n用法: /team [create|run|status|stop|list|delete|workflows]\n", sub)
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "go",
		ArgHint:     "<工作流> <目标>",
		Description: "一键创建并启动团队 (快捷命令)",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.TeamMgr == nil {
				fmt.Println("[团队管理器未初始化]")
				return nil
			}
			parts := strings.Fields(args)
			if len(parts) < 2 {
				fmt.Println("用法: /go <工作流> <目标>")
				fmt.Println("示例: /go research 调研 kubernetes 最佳实践")
				fmt.Println("      /go development 开发用户登录模块")
				fmt.Println("      /go creative 设计一个着陆页")
				return nil
			}
			workflow := strings.ToLower(parts[0])
			objective := strings.Join(parts[1:], " ")
			teamName := fmt.Sprintf("go-%s-%d", workflow, time.Now().Unix()%10000)

			team, err := ctx.TeamMgr.CreateTeam(teamName, workflow, objective, "cli")
			if err != nil {
				fmt.Printf("[创建失败: %v]\n", err)
				return nil
			}
			fmt.Printf("🚀 快速启动: 团队 %s (工作流: %s, Agent: %d)\n", team.Name, workflow, len(team.Agents))
			fmt.Printf("   目标: %s\n", objective)

			if err := ctx.TeamMgr.RunTeam(teamName, objective); err != nil {
				fmt.Printf("[启动失败: %v]\n", err)
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "wiki",
		ArgHint:     "[status|query|organize|lint|health]",
		Description: "知识库管理 (status/query/organize/lint/health)",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.WikiEngine == nil {
				fmt.Println("[Wiki 引擎未初始化]")
				return nil
			}
			parts := strings.Fields(args)
			sub := "status"
			if len(parts) >= 1 {
				sub = strings.ToLower(parts[0])
			}

			switch sub {
			case "status":
				st := ctx.WikiEngine.Status()
				fmt.Println("📚 Wiki 状态:")
				fmt.Printf("  仓库:     %v\n", st["repoDir"])
				fmt.Printf("  Raw 文件: %v\n", st["rawCount"])
				fmt.Printf("  Wiki 页面: %v\n", st["wikiCount"])
				fmt.Printf("  LLM 就绪: %v\n", st["hasLLM"])

			case "query":
				if len(parts) < 2 {
					fmt.Println("用法: /wiki query <问题>")
					return nil
				}
				question := strings.Join(parts[1:], " ")
				fmt.Println("🔍 正在查询知识库...")
				qCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				answer, archived, err := ctx.WikiEngine.QueryAndArchive(qCtx, question)
				if err != nil {
					fmt.Printf("[查询失败: %v]\n", err)
					return nil
				}
				fmt.Println(answer)
				if archived {
					fmt.Println("\n📝 此回答已自动归档到 wiki")
				}

			case "organize":
				mode := "full"
				if len(parts) >= 2 && (parts[1] == "inc" || parts[1] == "incremental") {
					mode = "incremental"
				}
				fmt.Printf("📝 开始 %s 整理...\n", mode)
				oCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				var err error
				if mode == "incremental" {
					result, e := ctx.WikiEngine.IncrementalOrganize(oCtx)
					err = e
					if err == nil {
						fmt.Printf("✅ 整理完成: 更新页面 %d\n", result.UpdatedPages)
					}
				} else {
					result, e := ctx.WikiEngine.Organize(oCtx)
					err = e
					if err == nil {
						fmt.Printf("✅ 整理完成: 更新页面 %d\n", result.UpdatedPages)
					}
				}
				if err != nil {
					fmt.Printf("[整理失败: %v]\n", err)
				}

			case "lint":
				fmt.Println("🔗 正在检查链接...")
				lCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				report, err := ctx.WikiEngine.Lint(lCtx)
				if err != nil {
					fmt.Printf("[Lint 失败: %v]\n", err)
					return nil
				}
				fmt.Printf("Wiki 页面: %d, Raw 文件: %d\n", report.TotalPages, report.TotalRaw)
				if len(report.BrokenLinks) > 0 {
					fmt.Printf("⚠️ 坏链: %d\n", len(report.BrokenLinks))
				}
				if len(report.OrphanedPages) > 0 {
					fmt.Printf("⚠️ 孤页: %d\n", len(report.OrphanedPages))
				}

			case "health":
				fmt.Println("🏥 检查 LLM 连接...")
				hr, err := ctx.WikiEngine.HealthCheck(context.Background())
				if err != nil {
					fmt.Printf("❌ LLM 连接异常: %v\n", err)
				} else {
					fmt.Printf("✅ Wiki LLM 健康检查完成:\n%s\n", hr.Summary)
				}

			default:
				fmt.Println("用法: /wiki [status|query|organize|lint|health]")
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "predict",
		ArgHint:     "<目标问题>",
		Description: "群体智能预测 (Boids+辩论+贝叶斯融合)",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.SwarmIntel == nil {
				fmt.Println("[群体智能引擎未初始化]")
				return nil
			}
			if strings.TrimSpace(args) == "" {
				fmt.Println("用法: /predict <目标问题>")
				fmt.Println("示例: /predict 2026年AI Agent市场规模将达到多少?")
				fmt.Println("      /predict 下一代iPhone会采用哪些关键技术?")
				return nil
			}

			fmt.Println("🧠 启动群体智能预测引擎...")
			result, err := ctx.SwarmIntel.Predict(
				context.Background(), "cli", strings.TrimSpace(args))
			if err != nil {
				fmt.Printf("[预测失败: %v]\n", err)
				return nil
			}

			fmt.Println()
			for _, o := range result.Outcomes {
				barLen := int(o.Probability * 30)
				bar := strings.Repeat("█", barLen) + strings.Repeat("░", 30-barLen)
				fmt.Printf("  %-16s %s %.1f%%\n", o.Outcome, bar, o.Probability*100)
				fmt.Printf("  %-16s 95%% CI: [%.1f%% ~ %.1f%%]\n", "", o.Lower95*100, o.Upper95*100)
			}
			fmt.Printf("\n  共识度: %.0f%% | 辩论: %d轮 | 分析师: %d\n",
				result.Consensus*100, result.Rounds, len(result.Agents))
			if result.Summary != "" {
				fmt.Printf("\n📝 %s\n", result.Summary)
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "simulate",
		ArgHint:     "<场景目标> [--mode social|game|montecarlo]",
		Description: "场景模拟 (社会/博弈/蒙特卡洛)",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.SwarmIntel == nil {
				fmt.Println("[群体智能引擎未初始化]")
				return nil
			}
			if strings.TrimSpace(args) == "" {
				fmt.Println("用法: /simulate <场景目标> [--mode MODE]")
				fmt.Println("模式: social|game|montecarlo|crisis|org|creative|market|policy|tech")
				fmt.Println("示例: /simulate 如果OpenAI开源所有模型会怎样?")
				fmt.Println("      /simulate 中美AI竞赛的未来走向 --mode game")
				fmt.Println("      /simulate 全球芯片供应中断 --mode crisis")
				fmt.Println("      /simulate AI+教育的未来 --mode creative")
				return nil
			}

			mode := "montecarlo"
			objective := args
			if strings.Contains(args, "--mode ") {
				parts := strings.SplitN(args, "--mode ", 2)
				objective = strings.TrimSpace(parts[0])
				modeParts := strings.Fields(parts[1])
				if len(modeParts) > 0 {
					mode = modeParts[0]
				}
			}

			cfg := swarm_intel.SimulationConfig{
				Mode:   mode,
				Agents: 5,
				Rounds: 3,
			}

			fmt.Printf("🎲 启动 %s 模拟...\n", mode)
			result, err := ctx.SwarmIntel.Simulate(
				context.Background(), "cli", strings.TrimSpace(objective), cfg)
			if err != nil {
				fmt.Printf("[模拟失败: %v]\n", err)
				return nil
			}

			fmt.Println()
			if len(result.Scenarios) > 0 {
				fmt.Println("场景分析:")
				for _, s := range result.Scenarios {
					fmt.Printf("  📌 %s (概率: %.0f%%)\n", s.Name, s.Probability*100)
					fmt.Printf("     %s\n", s.Description)
				}
			}
			if len(result.Emergent) > 0 {
				fmt.Println("\n涌现行为:")
				for _, e := range result.Emergent {
					fmt.Printf("  🌊 %s\n", e)
				}
			}
			if result.Summary != "" {
				fmt.Printf("\n📝 %s\n", result.Summary)
			}
			return nil
		},
	})

	r.Register(&Command{
		Name:        "metrics",
		Description: "群体智能引擎可观测性指标",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.SwarmIntel == nil {
				fmt.Println("[群体智能引擎未初始化]")
				return nil
			}
			summary := ctx.SwarmIntel.GetMetrics()
			fmt.Println("\n📊 群体智能引擎 — 可观测性指标")
			fmt.Println(strings.Repeat("─", 50))
			fmt.Printf("总运行次数:     %d\n", summary.TotalRuns)
			fmt.Printf("平均延迟:       %dms\n", summary.AvgLatencyMs)
			fmt.Printf("平均共识度:     %.2f\n", summary.AvgConsensus)
			fmt.Printf("平均Brier分数:  %.4f\n", summary.AvgBrierScore)
			fmt.Printf("平均多样性:     %.2f\n", summary.AvgDiversityScore)
			fmt.Printf("平均LLM调用数:  %.1f\n", summary.AvgLLMCalls)
			fmt.Printf("平均辩论轮数:   %.1f\n", summary.AvgDebateRounds)
			fmt.Printf("辩论跳过率:     %.0f%%\n", summary.DebateSkipRate*100)
			fmt.Printf("DTI触发率:      %.0f%%\n", summary.DTITriggerRate*100)
			return nil
		},
	})

	r.Register(&Command{
		Name:        "compare",
		Description: "与 MiroFish 对比评测报告",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.SwarmIntel == nil {
				fmt.Println("[群体智能引擎未初始化]")
				return nil
			}
			report := ctx.SwarmIntel.GetComparisonReport()
			fmt.Println(swarm_intel.FormatComparisonReport(report))
			return nil
		},
	})

	r.Register(&Command{
		Name:        "history",
		ArgHint:     "[N]",
		Description: "查看最近 N 条预测历史 (默认 10)",
		Type:        CommandTypeLocal,
		Execute: func(args string, ctx *CommandContext) error {
			if ctx.SwarmIntel == nil {
				fmt.Println("[群体智能引擎未初始化]")
				return nil
			}
			n := 10
			if strings.TrimSpace(args) != "" {
				fmt.Sscanf(args, "%d", &n)
			}
			records, err := ctx.SwarmIntel.GetHistory(n)
			if err != nil {
				fmt.Printf("[获取历史失败: %v]\n", err)
				return nil
			}
			if len(records) == 0 {
				fmt.Println("暂无预测历史")
				return nil
			}
			fmt.Printf("\n📜 最近 %d 条预测历史:\n", len(records))
			for _, r := range records {
				fmt.Printf("  [%s] %s — 共识度: %.2f, Brier: %.4f\n",
					r.CreatedAt.Format("01-02 15:04"), r.Question, r.Consensus, r.BrierScore)
				for _, o := range r.Outcomes {
					fmt.Printf("    %s: %.1f%%\n", o.Outcome, o.Probability*100)
				}
			}
			return nil
		},
	})
}
