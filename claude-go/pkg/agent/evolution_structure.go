// evolution_structure.go —— 把学习器 d/e 挂到统一循环的空闲相位
// (design/03 §4.3「五个学习器、一个循环」的最后两个)。
//
// # 为什么挂在空闲相位而不是 run 完成时
//
// d (工作流/Prompt 进化) 与 e (权重导出) 的节奏是设计里最慢的两档:
//
//   - 归纳要**跨多个 run** 才有统计意义 (锁定判据要求同一 (签名,序列) 至少 3 次运行),
//     每个 run 完成时跑一次纯属白烧 CPU 并反复写同一份草案。
//   - prompt 反思要花 LLM 调用, 挂在交付路径上等于让每次团队完成都多付一笔钱。
//   - 导出是批处理, 天然属于"没人用系统的时候做"。
//
// 空闲相位由 EvolutionLoop 的 IdleAfter 计时器驱动 (默认 15 分钟静默), 且受
// MaxRoundsPerHour 预算闸约束 —— 三个学习器共享同一个预算, 不会各自失控。
//
// # 三条安全约束
//
//  1. **只产草案, 不改运行期**: 归纳/反思的产物落 <state>/evolution/proposals/,
//     没有任何运行期代码会读它。晋升要走 §4.6 的闸 (含不越权检查)。
//  2. **反思器必须与主模型不同源** (§4.2 H3): 未注入独立 reflector 时 prompt 进化
//     **跳过并记日志**, 绝不退而用主模型自评 —— 那正是 hermes 里被点名的 reward
//     hacking 入口。
//  3. **导出默认关** (设计明写): 需显式 ExportEnabled 或 CLAUDE_GO_EVO_EXPORT=1。
package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/evolution/learners"
)

// FallbackReflector 用 fallback 档位模型构造 prompt 反思器 (§4.2 H3: 反思器不得与
// 被评估的主模型同源)。无 fallback 配置时返回 nil。
//
// 放在这里而不是各装配处: CLI 与飞书两条路都要用, 各写一份必然漂移 (而其中一份写错
// 成"没 fallback 就用主模型"就直接把 H3 破了)。
//
// **绝不退而用主模型**: 让主模型改写自己的 prompt, 它会朝"自己更容易得高分"改而不是
// 朝"任务完成得更好"改 —— hermes 里被点名的 reward hacking 入口。少一条学习支线比
// 引入一个刷分入口便宜得多。
func FallbackReflector(c *api.Client) learners.Reflector {
	if c == nil {
		return nil
	}
	for _, m := range c.FallbackModels {
		if m != "" && m != c.Model {
			// WithModel 共享 HTTP 客户端与 RateLimitGuard, 只覆盖 Model ——
			// 反思调用同样受全局配额约束, 不会绕过限流偷跑。
			return c.WithModel(m)
		}
	}
	return nil
}

// StructureConfig 结构进化 (d/e) 的装配参数。
type StructureConfig struct {
	// StateDir 状态目录 (<state>), 用于定位 evolution/ 与 proposals/。
	// 空则由 EvolutionEngine 的 dataDir 推导 (dataDir = <state>/evolution)。
	StateDir string
	// Reflector 独立档位的反思器 (§4.2 H3 强制)。nil = prompt 进化跳过。
	Reflector learners.Reflector
	// ExportEnabled 是否在空闲期导出 SFT/DPO 语料 (设计要求默认关)。
	ExportEnabled bool
	// ExportDPO 导出时是否同时产 DPO 偏好对。
	ExportDPO bool
	// MaxPromptEvolvePerRound 每轮空闲最多改写几个 prompt; <=0 取 1。
	// 一轮只改一个是刻意的: 同时改多个会让后续 uplift 无法归因到具体改动。
	MaxPromptEvolvePerRound int
}

// structureLearners 循环持有的 d/e 运行器。
type structureLearners struct {
	cfg StructureConfig
}

// EnableStructureLearning 装配学习器 d/e。循环为 nil 时静默 no-op (与其余方法一致)。
//
// 幂等: 重复调用以最后一次为准。
func (l *EvolutionLoop) EnableStructureLearning(cfg StructureConfig) {
	if l == nil {
		return
	}
	if cfg.MaxPromptEvolvePerRound <= 0 {
		cfg.MaxPromptEvolvePerRound = 1
	}
	if strings.TrimSpace(cfg.StateDir) == "" && l.engine != nil {
		// dataDir 是 <state>/evolution, 上一级即 <state>。
		cfg.StateDir = filepath.Dir(l.engine.DataDir())
	}
	if !cfg.ExportEnabled && os.Getenv("CLAUDE_GO_EVO_EXPORT") == "1" {
		// env 只能**打开**导出, 不能关掉显式配置 —— 单向开关避免"配置说开、环境说关"
		// 这种谁也说不清的状态。
		cfg.ExportEnabled = true
	}
	// DPO 偏好对同理需要一个生产开关: 两处装配 (CLI / 飞书) 都不设 ExportDPO,
	// 于是 ExportSFT 的 IncludeDPO 分支此前**在生产上恒不可达** —— 导出器写了一半
	// 通电。DPO 单独一个开关而不是跟着 ExportEnabled: 它要求同签名任务有奖励差
	// (dpoMinGap), 冷启动阶段开了也只会产 0 对, 白扫一遍轨迹。
	if !cfg.ExportDPO && os.Getenv("CLAUDE_GO_EVO_EXPORT_DPO") == "1" {
		cfg.ExportDPO = true
		cfg.ExportEnabled = true // 只要 DPO 一个开关也能生效: 少一个"两个都要设"的坑
	}
	l.mu.Lock()
	l.structure = &structureLearners{cfg: cfg}
	l.mu.Unlock()
}

// runStructureLearners 空闲相位跑 d/e。任何一步失败只记日志, 不影响其余步骤 ——
// 学习是增强项, 一个学习器坏了不该拖垮别的。
func (l *EvolutionLoop) runStructureLearners(ctx context.Context) {
	if l == nil {
		return
	}
	l.mu.Lock()
	s := l.structure
	l.mu.Unlock()
	if s == nil || strings.TrimSpace(s.cfg.StateDir) == "" {
		return // 未装配: 保持"不配置就什么都不做"的既有语义
	}

	// ---- d1. AWM 工作流归纳 (确定性, 零 LLM) ----
	if res, err := learners.InduceWorkflows(s.cfg.StateDir); err != nil {
		log.Printf("[evolution-loop] 工作流归纳失败: %v", err)
	} else {
		if len(res.Proposals) > 0 {
			names := make([]string, 0, len(res.Proposals))
			for _, p := range res.Proposals {
				names = append(names, p.ID)
			}
			log.Printf("[evolution-loop] 工作流归纳: %d run / %d 组 → 新草案 %s (proposed 态, 未过闸)",
				res.Shapes, res.Groups, strings.Join(names, ","))
		} else if len(res.Skipped) > 0 {
			log.Printf("[evolution-loop] 工作流归纳无产出: %s", strings.Join(res.Skipped, "; "))
		}
		if l.metrics != nil {
			l.metrics.Record("evolution", "workflow_proposals", float64(len(res.Proposals)))
		}
	}

	// ---- d2. GEPA prompt 反思 (要 LLM, 要独立档位) ----
	l.evolvePrompts(ctx, s.cfg)

	// ---- e. 权重导出 (默认关) ----
	if s.cfg.ExportEnabled {
		res, err := learners.ExportSFT(s.cfg.StateDir, learners.ExportConfig{
			MaxTurnsPerSample: 8, // 超过 8 轮走确定性中段截断 (H5, 仅 SFT 侧允许有损)
			IncludeDPO:        s.cfg.ExportDPO,
		})
		if err != nil {
			log.Printf("[evolution-loop] 语料导出失败: %v", err)
		} else {
			log.Printf("[evolution-loop] 语料导出: SFT %d 条, DPO %d 对, 过滤 %v",
				res.Samples, res.Pairs, res.Filtered)
			if l.metrics != nil {
				l.metrics.Record("evolution", "export_sft_samples", float64(res.Samples))
				l.metrics.Record("evolution", "export_dpo_pairs", float64(res.Pairs))
			}
		}
	}
}

// evolvePrompts 找出奖励为负的节点, 对其 stage prompt 做反思式改写。
func (l *EvolutionLoop) evolvePrompts(ctx context.Context, cfg StructureConfig) {
	weak := learners.FindWeakNodes(cfg.StateDir)
	if len(weak) == 0 {
		return
	}
	if cfg.Reflector == nil {
		// 有活该干但没有合规的工具 —— 必须说出来, 否则这条支线会"静默不存在"。
		log.Printf("[evolution-loop] %d 个节点奖励为负 (最差: %s %.3f), 但未注入独立档位 reflector, "+
			"prompt 进化跳过 (design/03 §4.2 H3 禁止用主模型自评)", len(weak), weak[0].Node, weak[0].MeanScore)
		return
	}
	done := 0
	for _, w := range weak {
		if done >= cfg.MaxPromptEvolvePerRound {
			return
		}
		if ctx.Err() != nil {
			return
		}
		target, current, ok := resolveStagePrompt(w.Node)
		if !ok {
			continue // 认不出这个节点属于哪个工作流的哪个阶段 → 不猜
		}
		in := learners.PromptEvolveInput{
			Target:    target,
			Current:   current,
			Failures:  collectFailureEvidence(cfg.StateDir, w),
			MeanScore: w.MeanScore,
			Samples:   w.NegRuns,
		}
		p, err := learners.EvolvePrompt(ctx, cfg.StateDir, cfg.Reflector, in)
		if err != nil {
			log.Printf("[evolution-loop] prompt 进化跳过 (%s): %v", target, err)
			continue
		}
		log.Printf("[evolution-loop] prompt 草案 %s → %s (proposed 态, 未过闸; 父版本 %q)",
			target, p.ID, p.Parent)
		if l.metrics != nil {
			l.metrics.Record("evolution", "prompt_proposals", 1)
		}
		done++
	}
}

// resolveStagePrompt 把奖励里的节点名解析回"哪个工作流的哪个阶段 + 它的 prompt"。
//
// **歧义即放弃**: 同名阶段出现在多个工作流里时返回 false。猜一个的后果是把 A 工作流的
// 失败证据拿去改 B 工作流的 prompt, 那比不改更坏。
//
// 门禁类节点 (gate.compile / gate.test / latency 这些人造 NodeID) 天然匹配不到阶段,
// 自然被这条规则排除 —— 它们没有 prompt 可改。
func resolveStagePrompt(node string) (target, prompt string, ok bool) {
	node = strings.TrimSpace(node)
	if node == "" || strings.HasPrefix(node, "gate.") {
		return "", "", false
	}
	var hits []string
	var hitPrompt string
	for _, wf := range ListWorkflows() {
		for _, st := range wf.Stages {
			if st.Name != node {
				continue
			}
			if strings.TrimSpace(st.Prompt) == "" {
				continue // 阶段用角色 SystemPrompt 而非 StageDef.Prompt: 不在本支线范围
			}
			hits = append(hits, wf.Name+"/"+st.Name)
			hitPrompt = st.Prompt
		}
	}
	if len(hits) != 1 {
		return "", "", false
	}
	return hits[0], hitPrompt, true
}

// collectFailureEvidence 从 trajectories.json 捞该节点在负奖励 run 里的真实产出/报错。
//
// 只取**负奖励 run** 的证据: 拿成功 run 的产出去让 LLM"反思失败"会直接把它带偏。
func collectFailureEvidence(stateDir string, w learners.NodeWeakness) []string {
	rows := learners.LoadTrajectories(filepath.Join(stateDir, "evolution", "trajectories.json"))
	negRuns := map[string]bool{}
	for _, id := range w.NegRunIDs {
		negRuns[id] = true
	}
	var out []string
	for _, r := range rows {
		if r.StageName != w.Node || !negRuns[r.RunID] {
			continue
		}
		ev := r.Output
		if r.Error != "" {
			ev = "[error] " + r.Error + "\n" + ev
		}
		if strings.TrimSpace(ev) == "" {
			continue
		}
		out = append(out, ev)
		if len(out) >= 5 {
			break
		}
	}
	return out
}
