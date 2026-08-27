// evo_gepa_cmd.go —— evo replay / evo gepa-step 子命令 (手册 13.3.3 L5, 方案三第二批)。
//
// evo replay: 拿一份 prompt 正文 + 一个模型档位, 在任务集上跑确定性回放打分,
//   分数向量写入 GEPA Pareto 池 (<state>/evolution/gepa_pool.jsonl)。
// evo gepa-step: 一步完整 GEPA —— T3 差距参照反思 (26B 等独立档位, H3 守恒)
//   → 产出 proposed 草案 → 立即回放打分 → 入池 → 打印当前前沿。
// 晋升仍走既有门禁 (evo promote / 自动 gate), 本命令只负责"产候选 + 打分",
// 不越权直接上线 —— 13.3.6 纪律: 反思产物不过 replay 门禁不冻结。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/skills"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/evolution/learners"
	"github.com/anthropic/claude-go/pkg/evolution/replay"
	"github.com/spf13/cobra"
	"os/exec"
)

// buildClientFromAlias 从模型别名构造 api.Client (与 buildAdvisorClient 同型,
// 供 evo 子命令的轻量路径使用)。
func buildClientFromAlias(alias string) (*api.Client, error) {
	jsonCfg, _, err := loadRuntimeJSONConfig()
	if err != nil {
		return nil, err
	}
	if jsonCfg == nil {
		return nil, fmt.Errorf("未加载到配置 (用 --config 指定含 providers 的 config.json)")
	}
	resolved, ok, err := resolveRuntimeModelConfig(jsonCfg, alias)
	if err != nil || !ok || resolved.BaseURL == "" {
		return nil, fmt.Errorf("别名 %q 解析失败 (err=%v) —— 检查 providers 里是否注册了该别名", alias, err)
	}
	var client *api.Client
	if api.IsLocalEndpoint(resolved.BaseURL) {
		client = api.NewOllamaClient(resolved.BaseURL, resolved.ProviderName)
		client.APIKey = resolved.APIKey
	} else {
		client = api.NewClient(resolved.BaseURL, resolved.APIKey, resolved.ProviderName)
	}
	client.Protocol = resolved.Protocol
	client.SetProxy(resolved.Proxy)
	if resolved.CallTimeoutSec > 0 {
		client.CallTimeout = time.Duration(resolved.CallTimeoutSec) * time.Second
	}
	if resolved.FirstTokenTimeoutSec > 0 {
		client.FirstTokenTimeout = time.Duration(resolved.FirstTokenTimeoutSec) * time.Second
	}
	return client, nil
}

// shellGateRunner replay 的确定性门禁执行器: workspace 下 sh -c, 2 分钟超时。
func shellGateRunner(ctx context.Context, workspace, cmd string) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("Gate 任务缺 workspace")
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	c.Dir = workspace
	out, err := c.CombinedOutput()
	if err != nil {
		s := strings.TrimSpace(string(out))
		if len(s) > 400 {
			s = s[:400] + "…"
		}
		return fmt.Errorf("%v: %s", err, s)
	}
	return nil
}

// evoReplayCmd 回放打分: prompt 正文 × 模型档位 × 任务集 → per-task 分数 → 入池。
func evoReplayCmd(resolveSD func() string) *cobra.Command {
	var (
		tasksPath  string
		target     string
		bodyFile   string
		modelAlias string
		outPath    string
		proposalID string
	)
	c := &cobra.Command{
		Use:   "replay",
		Short: "prompt 候选在任务集上的确定性回放打分 (judge=nil, 只认 Expect/Gate)",
		RunE: func(cmd *cobra.Command, args []string) error {
			tasks, err := replay.LoadTasks(tasksPath)
			if err != nil {
				return err
			}
			body, err := os.ReadFile(bodyFile)
			if err != nil {
				return err
			}
			client, err := buildClientFromAlias(modelAlias)
			if err != nil {
				return err
			}
			cand := &agent.PromptCandidate{
				Label:  fmt.Sprintf("%s@%s", target, modelAlias),
				Body:   string(body),
				Client: client,
			}
			h, err := replay.New(replay.Config{
				OutPath:     outPath,
				Concurrency: 1, // 本地推理串行: 并发只会互相拖慢 (25 tok/s 级)
				TaskTimeout: 10 * time.Minute,
				Gate:        shellGateRunner,
			}, nil) // judge=nil: 能确定性判的就不花 LLM 钱 (手册 13.3.1)
			if err != nil {
				return err
			}
			defer h.Close()
			rep, err := h.Run(context.Background(), cand, tasks)
			if err != nil {
				return err
			}
			fmt.Printf("回放完成: %s @ %s\n  任务 %d / 跑 %d / 过 %d / 均值 %.3f\n",
				target, modelAlias, rep.Total, rep.Ran, rep.Passed, rep.MeanScore)
			vec := learners.ScoreVector{
				ProposalID: proposalID,
				Target:     target,
				TaskScores: map[string]float64{},
				Mean:       rep.MeanScore,
				Ran:        rep.Ran,
			}
			for _, r := range rep.Results {
				vec.TaskScores[r.TaskID] = r.Score
				mark := "✅"
				if !r.Passed {
					mark = "❌"
				}
				fmt.Printf("  %s %-24s score=%.2f %s\n", mark, r.TaskID, r.Score, r.Reason)
			}
			if vec.ProposalID == "" {
				vec.ProposalID = "adhoc-" + cand.Label
			}
			if err := learners.RecordScore(resolveSD(), vec); err != nil {
				return fmt.Errorf("入池失败: %w", err)
			}
			fmt.Printf("  分数向量已入池 (%s)\n", vec.ProposalID)
			return nil
		},
	}
	c.Flags().StringVar(&tasksPath, "tasks", "", "任务集 JSONL 路径 (必填)")
	c.Flags().StringVar(&target, "target", "", "进化对象名 (必填, 如 dev/implement)")
	c.Flags().StringVar(&bodyFile, "body-file", "", "prompt 正文文件 (必填)")
	c.Flags().StringVar(&modelAlias, "model", "", "被打分的模型别名 (必填, 如 ollama:lfm2.5:2.6b-q4_k_m)")
	c.Flags().StringVar(&outPath, "out", "", "逐条结果落盘路径 (可选, 供续跑判重)")
	c.Flags().StringVar(&proposalID, "proposal-id", "", "关联的草案 ID (可选, 缺省 adhoc-<target@model>)")
	_ = c.MarkFlagRequired("tasks")
	_ = c.MarkFlagRequired("target")
	_ = c.MarkFlagRequired("body-file")
	_ = c.MarkFlagRequired("model")
	return c
}

// evoGepaStepCmd 一步完整 GEPA: 反思(V2, 带 T3 参照) → 草案 → 回放打分 → 入池 → 前沿。
func evoGepaStepCmd(resolveSD func() string) *cobra.Command {
	var (
		target         string
		currentFile    string
		tasksPath      string
		modelAlias     string
		reflectorAlias string
		refFiles       []string
		failureFiles   []string
		mean           float64
		samples        int
	)
	c := &cobra.Command{
		Use:   "gepa-step",
		Short: "一步完整 GEPA prompt 进化 (反思→草案→回放→入池→前沿)",
		RunE: func(cmd *cobra.Command, args []string) error {
			sd := resolveSD()
			current, err := os.ReadFile(currentFile)
			if err != nil {
				return err
			}
			readAll := func(paths []string) []string {
				var out []string
				for _, p := range paths {
					if b, err := os.ReadFile(p); err == nil {
						out = append(out, string(b))
					}
				}
				return out
			}
			failures := readAll(failureFiles)
			refs := readAll(refFiles)

			// 反思器: 独立档位 (H3), 显式 --reflector 优先。
			if strings.TrimSpace(reflectorAlias) == "" {
				return fmt.Errorf("必须显式指定 --reflector (H3: 反思器与被评分模型不同源, 缺省易踩同源)")
			}
			if reflectorAlias == modelAlias {
				return fmt.Errorf("H3 违规: reflector 与被评分模型同别名 (%s), 反思器必须不同源", reflectorAlias)
			}
			reflClient, err := buildClientFromAlias(reflectorAlias)
			if err != nil {
				return err
			}

			prop, err := learners.EvolvePromptV2(context.Background(), sd, reflClient, learners.PromptEvolveInputV2{
				PromptEvolveInput: learners.PromptEvolveInput{
					Target:    target,
					Current:   string(current),
					Failures:  failures,
					MeanScore: mean,
					Samples:   samples,
				},
				SuccessRefs: refs,
			})
			if err != nil {
				return fmt.Errorf("反思未产草案: %w", err)
			}
			fmt.Printf("草案已产出: %s (%d 字符, parent=%s)\n", prop.ID, len(prop.Body), prop.Parent)

			// 立即回放打分 (同一任务集, 同一被评分模型)。
			if tasksPath != "" {
				tasks, err := replay.LoadTasks(tasksPath)
				if err != nil {
					return err
				}
				scoredClient, err := buildClientFromAlias(modelAlias)
				if err != nil {
					return err
				}
				h, err := replay.New(replay.Config{Concurrency: 1, TaskTimeout: 10 * time.Minute, Gate: shellGateRunner}, nil)
				if err != nil {
					return err
				}
				defer h.Close()
				rep, err := h.Run(context.Background(), &agent.PromptCandidate{
					Label: prop.ID + "@" + modelAlias, Body: prop.Body, Client: scoredClient,
				}, tasks)
				if err != nil {
					return err
				}
				vec := learners.ScoreVector{
					ProposalID: prop.ID, Target: target,
					TaskScores: map[string]float64{}, Mean: rep.MeanScore, Ran: rep.Ran,
				}
				for _, r := range rep.Results {
					vec.TaskScores[r.TaskID] = r.Score
				}
				if err := learners.RecordScore(sd, vec); err != nil {
					return err
				}
				fmt.Printf("回放打分: %d/%d 过, 均值 %.3f, 已入池\n", rep.Passed, rep.Ran, rep.MeanScore)
			}

			// 打印该 target 的当前 Pareto 前沿。
			pool, _ := learners.LoadPool(sd)
			var mine []learners.ScoreVector
			for _, v := range pool {
				if v.Target == target {
					mine = append(mine, v)
				}
			}
			front := learners.Frontier(mine)
			fmt.Printf("当前前沿 (%d 候选):\n", len(front))
			for _, v := range front {
				keys := make([]string, 0, len(v.TaskScores))
				for k := range v.TaskScores {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				fmt.Printf("  %-40s mean=%.3f tasks=%d\n", v.ProposalID, v.Mean, len(keys))
			}
			return nil
		},
	}
	c.Flags().StringVar(&target, "target", "", "进化对象名 (必填)")
	c.Flags().StringVar(&currentFile, "current-file", "", "当前 prompt 正文文件 (必填)")
	c.Flags().StringVar(&tasksPath, "tasks", "", "任务集 JSONL (给--则立即回放打分并入池)")
	c.Flags().StringVar(&modelAlias, "model", "", "被评分的模型别名")
	c.Flags().StringVar(&reflectorAlias, "reflector", "", "反思器模型别名 (必填, H3 必须与被评分模型不同源)")
	c.Flags().StringArrayVar(&refFiles, "ref", nil, "T3 成功参照文件 (可重复, 各 ≤1200 字符入提示词)")
	c.Flags().StringArrayVar(&failureFiles, "failure", nil, "失败证据文件 (可重复, 各 ≤1200 字符)")
	c.Flags().Float64Var(&mean, "mean", -1, "当前加权奖励均值 (负值才会反思)")
	c.Flags().IntVar(&samples, "samples", 5, "负奖励证据条数 (>=3 才反思)")
	_ = c.MarkFlagRequired("target")
	_ = c.MarkFlagRequired("current-file")
	return c
}

// evoGepaLoopCmd 多轮 GEPA 迭代 (L5 完整化: 前沿轮值选父 + 失败证据自动抽取)。
//
// 每轮: 从前沿轮值选父代 (首轮用 --current-file 基线) → 用上一轮回放报告的
// 失败项自动构造证据 (FailuresFromReplayResults) → EvolvePromptV2 反思
// (T3 参照贯穿) → 回放打分 → 入池。收敛即停 (父代回放满分时没有可反思的失败)。
func evoGepaLoopCmd(resolveSD func() string) *cobra.Command {
	var (
		target         string
		currentFile    string
		tasksPath      string
		modelAlias     string
		reflectorAlias string
		refFiles       []string
		rounds         int
	)
	c := &cobra.Command{
		Use:   "gepa-loop",
		Short: "多轮 GEPA 迭代 (前沿选父 + 失败证据自动抽取 + T3 参照贯穿)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if reflectorAlias == "" || reflectorAlias == modelAlias {
				return fmt.Errorf("H3: --reflector 必须显式指定且与被评分模型不同源")
			}
			reflClient, err := buildClientFromAlias(reflectorAlias)
			if err != nil {
				return err
			}
			scoredClient, err := buildClientFromAlias(modelAlias)
			if err != nil {
				return err
			}
			tasks, err := replay.LoadTasks(tasksPath)
			if err != nil {
				return err
			}
			current, err := os.ReadFile(currentFile)
			if err != nil {
				return err
			}
			var refs []string
			for _, rf := range refFiles {
				if b, err := os.ReadFile(rf); err == nil {
					refs = append(refs, string(b))
				}
			}
			sd := resolveSD()
			h, err := replay.New(replay.Config{Concurrency: 1, TaskTimeout: 10 * time.Minute, Gate: shellGateRunner}, nil)
			if err != nil {
				return err
			}
			defer h.Close()
			scoreIt := func(id, body string) (replay.Report, error) {
				rep, err := h.Run(context.Background(), &agent.PromptCandidate{
					Label: id + "@" + modelAlias, Body: body, Client: scoredClient,
				}, tasks)
				if err != nil {
					return rep, err
				}
				vec := learners.ScoreVector{ProposalID: id, Target: target,
					TaskScores: map[string]float64{}, Mean: rep.MeanScore, Ran: rep.Ran}
				for _, r := range rep.Results {
					vec.TaskScores[r.TaskID] = r.Score
				}
				return rep, learners.RecordScore(sd, vec)
			}

			// 基线打分 (round 0 的父代 = 当前 prompt)
			baseID := "base-" + target
			rep, err := scoreIt(baseID, string(current))
			if err != nil {
				return err
			}
			fmt.Printf("[gepa-loop] 基线 %s: %d/%d 过, 均值 %.3f\n", baseID, rep.Passed, rep.Ran, rep.MeanScore)
			lastRep := rep

			for round := 0; round < rounds; round++ {
				failures := learners.FailuresFromReplayResults(lastRep.Results, 8)
				if len(failures) == 0 {
					fmt.Printf("[gepa-loop] 第 %d 轮: 父代回放满分, 无失败证据可反思, 收敛停\n", round)
					break
				}
				// 选父代: 首轮用基线, 之后前沿轮值
				parentBody := string(current)
				if round > 0 {
					pool, _ := learners.LoadPool(sd)
					if pid := learners.SelectForMutation(pool, target, round-1); pid != "" {
						if body, ok := learners.LoadProposalBody(sd, pid); ok {
							parentBody = body
							fmt.Printf("[gepa-loop] 第 %d 轮父代: %s (前沿轮值)\n", round, pid)
						}
					}
				}
				// 门禁区间: 有失败即负证据; 均值映射到 <0, samples 对齐 lite 门槛
				mean := lastRep.MeanScore - 1.0
				samples := len(failures)
				if samples < 3 {
					samples = 3 // 回放失败即有效负证据, 计数口径与奖励流不同
				}
				prop, err := learners.EvolvePromptV2(context.Background(), sd, reflClient, learners.PromptEvolveInputV2{
					PromptEvolveInput: learners.PromptEvolveInput{
						Target: target, Current: parentBody,
						Failures: failures, MeanScore: mean, Samples: samples,
					},
					SuccessRefs: refs,
				})
				if err != nil {
					fmt.Printf("[gepa-loop] 第 %d 轮反思未产草案: %v\n", round, err)
					continue
				}
				rep, err := scoreIt(prop.ID, prop.Body)
				if err != nil {
					return err
				}
				lastRep = rep
				fmt.Printf("[gepa-loop] 第 %d 轮候选 %s: %d/%d 过, 均值 %.3f\n", round, prop.ID, rep.Passed, rep.Ran, rep.MeanScore)
			}

			pool, _ := learners.LoadPool(sd)
			var mine []learners.ScoreVector
			for _, v := range pool {
				if v.Target == target {
					mine = append(mine, v)
				}
			}
			fmt.Printf("[gepa-loop] 收敛。%s 前沿 (%d 候选):\n", target, len(learners.Frontier(mine)))
			for _, v := range learners.Frontier(mine) {
				fmt.Printf("  %-44s mean=%.3f\n", v.ProposalID, v.Mean)
			}
			return nil
		},
	}
	c.Flags().StringVar(&target, "target", "", "进化对象名 (必填)")
	c.Flags().StringVar(&currentFile, "current-file", "", "基线 prompt 正文文件 (必填)")
	c.Flags().StringVar(&tasksPath, "tasks", "", "任务集 JSONL (必填)")
	c.Flags().StringVar(&modelAlias, "model", "", "被评分模型别名 (必填)")
	c.Flags().StringVar(&reflectorAlias, "reflector", "", "反思器模型别名 (必填, H3 不同源)")
	c.Flags().StringArrayVar(&refFiles, "ref", nil, "T3 成功参照文件 (可重复)")
	c.Flags().IntVar(&rounds, "rounds", 3, "最大迭代轮数 (收敛提前停)")
	_ = c.MarkFlagRequired("target")
	_ = c.MarkFlagRequired("current-file")
	_ = c.MarkFlagRequired("tasks")
	_ = c.MarkFlagRequired("model")
	return c
}

// buildImproveCreator 为 evo audit 的 ImproveSkill 接线构造 AutoCreator
// (LLM + 真实 Registry——ImproveSkill 对两者缺一静默 nil, 这里必须都给)。
func buildImproveCreator(stateDir, modelAlias string) *skills.AutoCreator {
	client, err := buildClientFromAlias(modelAlias)
	if err != nil || client == nil {
		return nil
	}
	reg := skills.NewRegistry()
	cwd, _ := os.Getwd()
	reg.LoadDefaults(cwd)
	return skills.NewAutoCreator(filepath.Join(stateDir, "skills"), client, client.Model, reg)
}
