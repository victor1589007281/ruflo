package agent

// prompt_canary.go —— `shadow_ratio` 比例灰度的**运行期落点** (design/03 §4.6/§4.7)。
//
// ---------------------------------------------------------------------------
// 这里补的是哪一格
// ---------------------------------------------------------------------------
//
// `evo_run_experiment` 一直在往 `<state>/evolution/experiments/*.json` 写
// `shadow_ratio`, 但**运行期没有任何地方读它** —— design/01 §六 把这条记作
// 「🟡 建成未通电」。分流算法与实验记录在 `pkg/evolution/learners/canary.go`
// (含"为什么不用 rand"与"上限为什么要在决策点再夹一次"两段), 本文件只做三件事:
//
//	① 在 stage 提示词组装**之前**问一次: 本 (实验, run) 落哪个臂;
//	② shadow 臂把 StageDef.Prompt 换成候选正文;
//	③ 两个臂都打点 (policy_decision Span + 一行日志)。
//
// ---------------------------------------------------------------------------
// 为什么落在 executeStage, 而不是别处
// ---------------------------------------------------------------------------
//
// 灰度决策要同时拿到三样东西, 全仓只有这里三样齐备:
//
//	RunID     —— 抽样键。ctx 到 executeStage 时已带 trace 四元组 (workflow.go 里
//	             `trace.With(ctx, trace.IDs{NodeID: stage.Name})` 那一行的上游)。
//	灰度对象  —— prompt 草案的 Target 就是 `<workflow>/<stage>` (见
//	             `evolution_structure.go` 的 resolveStagePrompt: 草案本来就按这个键产出),
//	             而 team.Workflow + stage.Name 正好拼出它。
//	打点位置  —— `writePolicyDecisionSpan` 已经在这条路径上, 臂标记跟着注入记录一起走,
//	             不必新开一条观测通道。
//
// 另一个理由是覆盖面: `executeStage` 是 pipeline 与图两条路径**共用**的
// (stageNodeRunner → ExecuteSingleStage → executeStage), 挂在这里两种形态同构 ——
// 挂到图节点拦截器上就只有 `CLAUDE_GO_GRAPH_ENGINE` 开着时才生效, 又是一个
// "建成未通电"(policy_decision.go 文件头详述过同一个坑)。
//
// ---------------------------------------------------------------------------
// 灰度的是 prompt 正文, 不是权限面 —— 「不越权」律为什么仍然成立
// ---------------------------------------------------------------------------
//
// 被替换的只有 `StageDef.Prompt`。这次执行能用哪些工具由角色/ToolProfile/ConstraintSet
// 决定, 与 stage 提示词正文无关 ⇒ 候选无论写什么, 都拿不到基线拿不到的工具。
// 所以 §4.6 四律里的"不越权"在这条路上是**结构性成立**的, 不依赖对候选正文做审查
// (审查 LLM 写的自然语言本来也拦不住有意绕过的人)。
//
// 边界仍然有三道, 缺一条这件事就不该做:
//
//	比例上限 50%  候选再坏也有一半流量是基线 (learners.MaxShadowRatio, 读侧再夹一次)
//	占位符守恒    候选丢了 {objective} 之类锚点直接拒绝 —— 那是"上线即静默失效"
//	              (learners.ResolvePromptCanary 里查; ProposePrompt 手工提交那条路
//	              没有原文可比, 查不了, 所以必须在注入点补)
//	歧义即放弃    两个实验抢同一个 stage 时全部不注入
//
// ---------------------------------------------------------------------------
// 为什么基线臂也要打点
// ---------------------------------------------------------------------------
//
// 只记 shadow 臂的话, 事后能看到"这些 run 注入了候选", 但看不到"哪些 run 是对照组"
// —— 于是配对分析退化成 `learners.SplitByTime` 那种**前后对照**, 期间任何别的改动
// (换模型/改工作流) 都会混进 uplift。两个臂都记, 才有真正的随机对照。
//
// 拒绝也要记 (`canary_refused`)。"开了实验, 但一条都没注入"若不给理由, 事后完全
// 无从查证 —— 与 pkg/graph 的 `graph.expand_rejected` 同一条理由。

import (
	"context"
	"log"
	"strings"

	"github.com/anthropic/claude-go/pkg/evolution/learners"
	"github.com/anthropic/claude-go/pkg/trace"
)

// promptCanaryRecord 一次灰度决策的可打点形态 (nil = 本阶段无灰度)。
type promptCanaryRecord struct {
	ExperimentID string
	ProposalID   string
	Target       string
	Ratio        float64
	Arm          string // learners.ArmShadow | learners.ArmBaseline
	// Refused 非空表示"有相关实验但被拒", 此时 Arm 为空。
	Refused string
}

// applyPromptCanary 解析比例灰度, 返回 (可能被替换 Prompt 的 stage, 打点记录)。
//
// **默认零影响**是硬要求: 没装进化引擎 / 没有实验目录 / 阶段没有 StageDef.Prompt /
// 没有 RunID —— 任一条成立就原样返回入参 stage, 提示词与改造前逐字节一致。
// 绝大多数 run 命中的是"实验目录不存在", 代价一次 ReadDir。
func (we *WorkflowExecutor) applyPromptCanary(ctx context.Context, stage StageDef, team *ProductionTeam) (StageDef, *promptCanaryRecord) {
	if we == nil || we.evolution == nil || team == nil {
		return stage, nil
	}
	stateDir := we.evolution.StateDir()
	if stateDir == "" {
		return stage, nil
	}
	// 角色型阶段 (StageDef.Prompt 为空, 提示词全来自角色 SystemPrompt) 不在本支线
	// 范围内: 没有可替换的对象, 而 resolveStagePrompt 产草案时也正是这么排除的 ——
	// 两侧口径必须一致, 否则会出现"产得出草案却永远灰度不了"或反之。
	if strings.TrimSpace(stage.Prompt) == "" || strings.TrimSpace(team.Workflow) == "" {
		return stage, nil
	}
	target := team.Workflow + "/" + stage.Name
	runID := trace.From(ctx).RunID

	c, refused := learners.ResolvePromptCanary(stateDir, target, runID, stage.Prompt)
	if refused != "" {
		log.Printf("[prompt-canary] %s: 拒绝注入 —— %s", target, refused)
		return stage, &promptCanaryRecord{Target: target, Refused: refused}
	}
	if c == nil {
		return stage, nil
	}
	rec := &promptCanaryRecord{
		ExperimentID: c.ExperimentID,
		ProposalID:   c.ProposalID,
		Target:       c.Target,
		Ratio:        c.Ratio,
		Arm:          c.Arm,
	}
	// 两个臂都记一行日志: policy_decision Span 在 traceStore 未装配时是 no-op
	// (fail-open, 见 policy_decision.go), 那种形态下日志是唯一的臂记录。
	log.Printf("[prompt-canary] %s: 实验 %s 草案 %s 比例 %.2f run=%s → %s 臂",
		target, c.ExperimentID, c.ProposalID, c.Ratio, runID, c.Arm)
	if c.Arm != learners.ArmShadow {
		return stage, rec
	}
	// 只改 Prompt 这一个字段: stage 是**值拷贝**, 不会污染工作流模板 (同一
	// WorkflowDef 会被后续 run 复用, 就地改会让灰度一次污染到全部后续 run)。
	shadowStage := stage
	shadowStage.Prompt = c.Body
	return shadowStage, rec
}

// canaryAttrs 把灰度记录展开成 policy_decision Span 的属性。
func (r *promptCanaryRecord) canaryAttrs(attrs map[string]any) {
	if r == nil {
		return
	}
	if r.Refused != "" {
		attrs["canary_refused"] = r.Refused
		attrs["canary_target"] = r.Target
		return
	}
	attrs["canary_arm"] = r.Arm
	attrs["canary_exp"] = r.ExperimentID
	attrs["canary_proposal"] = r.ProposalID
	attrs["canary_ratio"] = r.Ratio
	attrs["canary_target"] = r.Target
}
