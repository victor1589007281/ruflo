// harness_evo_cmd.go — `claude-go harness-evo` CLI: HarnessPro 论文进化逻辑的
// 端到端入口 (出卷 → 进化主循环 → 三档幸存者), 供脚本与 Web 后端程序化调用。
//
// 模型经全局 --model 选择 (与 swarm 同一条 buildEngine 路径, 实测组合
// --model opencode:deepseek-v4.1-flash)。过程通知走 stderr, --json 时 stdout 仅报告 JSON。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/harnesspro"
	"github.com/spf13/cobra"
)

// harnessEvoCmd harness-evo 子命令树。
func harnessEvoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "harness-evo",
		Short: "HarnessPro 式 harness 自动进化 (固定 DAG 工作流)",
		Long: `复现 HarnessPro 论文的进化逻辑: 确定性编排器 + 固定 DAG
(出卷→训练rollout→诊断→三向出案→动刀git→验证→地板/帕累托/三档选点),
LLM 只在 出卷/诊断/出案/动刀 四个节点内部出现。`,
	}
	cmd.AddCommand(harnessEvoRunCmd())
	cmd.AddCommand(harnessEvoTasksCmd())
	return cmd
}

// harnessEvoTasksCmd 列出内置任务池与选题结果 (离线, 零 LLM)。
func harnessEvoTasksCmd() *cobra.Command {
	var trainN int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "列出内置任务池 / DPP-lite 选题结果",
		RunE: func(cmd *cobra.Command, args []string) error {
			pool := harnesspro.DefaultPool()
			train := harnesspro.SelectTrainTasks(pool, trainN)
			out := map[string]any{
				"pool":  pool,
				"train": train,
				"valid": complementTaskIDs(pool, train),
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				return enc.Encode(out)
			}
			fmt.Printf("任务池 %d 题:\n", len(pool))
			for _, t := range pool {
				fmt.Printf("  %-28s cat=%-10s diff=%.2f\n", t.ID, t.Category, t.Difficulty)
			}
			fmt.Printf("\n选题 (训练 %d):\n", len(train))
			for _, t := range train {
				fmt.Printf("  %s\n", t.ID)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&trainN, "train", 10, "训练集规模 (DPP-lite 选出)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "JSON 输出")
	return cmd
}

// harnessEvoRunCmd 跑一次完整进化。
func harnessEvoRunCmd() *cobra.Command {
	var epochs, k, trainN, timeoutMin, maxPar int
	var workdir, tiersArg string
	var jsonOut, resume bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "跑一轮完整进化 (出卷 → T 轮 → 三档幸存者)",
		RunE: func(cmd *cobra.Command, args []string) error {
			notify := func(msg string) { fmt.Fprintln(os.Stderr, "[harness-evo] "+msg) }
			eng, err := buildEngine()
			if err != nil {
				return err
			}
			cfg := harnesspro.Config{
				Epochs: epochs, K: k, TrainN: trainN,
				Workdir: workdir, Resume: resume, MaxParallel: maxPar,
			}
			if tiersArg != "" {
				tiers, err := parseTiers(tiersArg)
				if err != nil {
					return err
				}
				cfg.Tiers = tiers
			}
			orch, err := harnesspro.NewOrchestrator(cfg, &harnesspro.APILLM{Client: eng.APIClient})
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMin)*time.Minute)
			defer cancel()
			notify(fmt.Sprintf("开始进化: epochs=%d k=%d train=%d model=%s", epochs, k, trainN, eng.APIClient.Model))
			report, err := orch.Run(ctx, notify)
			if report != nil && jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				_ = enc.Encode(report) // 报告尽力输出, 错误随后返回
			}
			if err != nil {
				return err
			}
			if !jsonOut {
				printTextReport(report)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&epochs, "epochs", 2, "进化轮次 T (论文 TB2/SWE-Pro=3, ProgramBench=4)")
	cmd.Flags().IntVar(&k, "k", 3, "训练集 rollout 重跑次数")
	cmd.Flags().IntVar(&trainN, "train", 10, "训练集规模 (DPP-lite 选出)")
	cmd.Flags().IntVar(&timeoutMin, "timeout", 60, "总超时 (分钟)")
	cmd.Flags().IntVar(&maxPar, "max-parallel", 6, "图内并发上限")
	cmd.Flags().StringVar(&workdir, "workdir", "", "运行目录 (git 工作区+journal+报告; 默认 ~/.claude-go/harnesspro/<ts>)")
	cmd.Flags().StringVar(&tiersArg, "tiers", "", "三档权重, 形如 lite:1,3|medium:1,1|heavy:3,1")
	cmd.Flags().BoolVar(&resume, "resume", false, "从 journal 续跑 (已完成节点吃缓存)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "结构化 JSON 输出 (stdout 仅 JSON)")
	return cmd
}

// printTextReport 人类可读报告。
func printTextReport(r *harnesspro.EvolutionReport) {
	fmt.Println()
	fmt.Printf("== harness-evo %s ==\n", r.RunID)
	fmt.Printf("模型: %s | 时长: %.0fs | 总 token: %d\n", r.Model, r.DurationS, r.TotalTokens)
	fmt.Printf("地板: %.2f | 训练 %d 题 | 验证 %d 题\n", r.Floor, len(r.TrainIDs), len(r.ValidIDs))
	for _, ep := range r.Epochs {
		fmt.Printf("\n-- epoch %d (tokens=%d) --\n", ep.Epoch, ep.Tokens)
		fmt.Printf("  前沿: %v\n  地板过滤: %v\n", ep.Frontier, ep.Filtered)
		for _, s := range ep.Survivors {
			fmt.Printf("  ★ %-6s %-28s J=%.3f\n", s.Tier, s.CandidateID, s.J)
		}
		for _, ev := range ep.Evals {
			fmt.Printf("    %-28s pass=%.2f cost=%d\n", ev.CandidateID, ev.PassRate, ev.CostTokens)
		}
	}
	fmt.Println("\n三档幸存者 harness 见 workdir/harnesses/<id>/instructions.md")
}

// parseTiers 解析 --tiers 旗标 (形如 lite:1,3|medium:1,1|heavy:3,1)。
func parseTiers(s string) ([]harnesspro.TierWeight, error) {
	var out []harnesspro.TierWeight
	for _, part := range strings.Split(s, "|") {
		name, rest, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("tiers 格式错误 (应为 lite:1,3|medium:1,1|heavy:3,1): %q", part)
		}
		aStr, bStr, ok := strings.Cut(rest, ",")
		if !ok {
			return nil, fmt.Errorf("tiers 权重缺少 β: %q", part)
		}
		alpha, err := strconv.ParseFloat(strings.TrimSpace(aStr), 64)
		if err != nil {
			return nil, fmt.Errorf("tiers α 解析失败: %q", part)
		}
		beta, err := strconv.ParseFloat(strings.TrimSpace(bStr), 64)
		if err != nil {
			return nil, fmt.Errorf("tiers β 解析失败: %q", part)
		}
		out = append(out, harnesspro.TierWeight{Tier: name, Alpha: alpha, Beta: beta})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tiers 为空")
	}
	return out, nil
}

// complementTaskIDs 池子里不在训练集的题 (验证集)。
func complementTaskIDs(pool []harnesspro.Task, train []harnesspro.Task) []string {
	inTrain := map[string]bool{}
	for _, t := range train {
		inTrain[t.ID] = true
	}
	var out []string
	for _, t := range pool {
		if !inTrain[t.ID] {
			out = append(out, t.ID)
		}
	}
	return out
}
