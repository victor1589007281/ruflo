package agent

// policy_decision.go —— 策略应用层的决策留痕 (design/03 §4.4 第 2 条)。
//
// ---------------------------------------------------------------------------
// §4.4 要的两件事, 这里补的是第二件
// ---------------------------------------------------------------------------
//
// §4.4 只有两条:
//
//	① 注入点全部走 design/01 的拦截器/hook, headless 与飞书同构;
//	② 每次注入记 policy_decision Span (注入了哪些经验/记忆/技能/模板版本)。
//
// ① 本轮已经成立: 经验注入在 executeStage, 而 executeStage 是 pipeline 与图两条
// 路径共用的 (stageNodeRunner → ExecuteSingleStage → executeStage); 记忆注入的
// MemoryInjectHook 也已在 CLI 装配 (RefreshHooks)。② 此前**零命中** —— 全仓没有
// 任何地方写过这种 Span, 于是 bandit 更新与 uplift 归因只能靠 InjectionRecord 里的
// 经验 ID, 记忆/技能/上下文块注入了什么完全无迹可寻。本文件补的就是 ②。
//
// ---------------------------------------------------------------------------
// 为什么不做成 NodeInterceptor
// ---------------------------------------------------------------------------
//
// 直觉上"策略应用"该是个切面。但节点级切面的挂载点是 engine.callRunner,
// 而 CLAUDE_GO_GRAPH_ENGINE 默认关 ⇒ pipeline/fanout 类工作流根本不进图引擎
// (graph_interceptors.go 文件头把这件事讲得很清楚)。把决策留痕挂到节点链上, 结果是
// 生产上绝大多数 run 一条 policy_decision 都不产 —— 又一个"建成未通电"。
//
// 更根本的是**切面拿不到这份数据**: 注入是在 executeStage 内部拼提示词时发生的,
// 切面手上只有 NodeSpec 与 NodeInput, 看不见"最终提示词里塞了哪三条经验、哪两个技能"。
// 要让切面看见就得让 executeStage 把这些回填出去 —— 那是改共用热路径。
//
// 所以留痕就写在**注入发生的那一行旁边**。这不是绕开切面, 而是承认这件事的天然位置
// 就在那里: §4.4 的原文说的是"每次注入记一条 Span", 没说必须由切面来记。
//
// ---------------------------------------------------------------------------
// 技能名为什么从提示词里反解, 而不是让 MergedPrompt 返回
// ---------------------------------------------------------------------------
//
// 技能注入埋在 RoleRegistry.MergedPrompt 内部 (含选择器的预算裁剪), 改它的签名要动
// 全部调用方。反解有一个额外的好处: 它读的是**真的会发出去的那份提示词**, 所以不可能
// 出现"记的和发的不一致"。选择器把某个技能挤掉时, 这里自然就记不到它 —— 那正是想要的。

import (
	"context"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// policyDecision 一次节点执行前的策略选择。
type policyDecision struct {
	Stage string
	Role  string
	Team  string
	// ExperienceIDs 本次注入的进化经验 ID (RETRIEVE 的结果)。
	ExperienceIDs []string
	// Blackboard/Reference/UserFeedback 三个上下文块是否被前置。
	// 记布尔而不是正文: 正文已经在 Prompt 里 (OutputRef 指向它), 这里只要"注了没"。
	Blackboard   bool
	Reference    bool
	UserFeedback bool
	// Prompt 最终提示词 (供反解技能名, 并作 Span 的 OutputRef 正文)。
	Prompt string
	// Objective 本次目标 (Span 的 InputRef)。
	Objective string
}

// writePolicyDecisionSpan 写一条 policy_decision Span。
//
// fail-open: 底座未装配就零成本 no-op。与 gate Span 同一条理由 —— 采集是观测不是治理,
// 记不下来不该影响这个阶段跑不跑。
func (we *WorkflowExecutor) writePolicyDecisionSpan(ctx context.Context, d policyDecision, canary *promptCanaryRecord) {
	if we == nil || we.traceStore == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := trace.From(ctx)
	skills := injectedSkillNames(d.Prompt)
	attrs := map[string]any{
		"stage":             d.Stage,
		"role":              d.Role,
		"team":              d.Team,
		"experience_count":  len(d.ExperienceIDs),
		"skill_count":       len(skills),
		"prompt_chars":      len(d.Prompt),
		"ctx_blackboard":    d.Blackboard,
		"ctx_reference":     d.Reference,
		"ctx_user_feedback": d.UserFeedback,
	}
	if len(d.ExperienceIDs) > 0 {
		attrs["experiences"] = d.ExperienceIDs
	}
	if len(skills) > 0 {
		attrs["skills"] = skills
	}
	// shadow 比例灰度臂标记 (design/03 §4.6/§4.7): nil = 本阶段无灰度。
	if canary != nil {
		canary.canaryAttrs(attrs)
	}
	we.traceStore.Write(tracestore.Span{
		TraceID:  ids.RunID,
		SpanID:   newSpanID(),
		ParentID: ids.TurnID,
		Kind:     tracestore.KindPolicyDecision,
		Name:     d.Stage,
		NodeID:   firstNonEmpty(d.Stage, ids.NodeID),
		TurnID:   ids.TurnID,
		InputRef: we.traceStore.MakeRef(d.Objective),
		// 正文放最终提示词: uplift 归因时要能回答"注入后那份 prompt 到底长什么样",
		// 而 llm_call Span 的 InputRef 是**渲染过的**请求视图, 两者不是一回事。
		OutputRef: we.traceStore.MakeRef(d.Prompt),
		Attrs:     attrs,
		TS:        time.Now().UnixMilli(),
	})
}

// injectedSkillNames 从提示词的 <role_skills> 段反解被注入的技能名。
//
// 只认这一段: 提示词别处也可能出现 "### Skill:" 字样 (比如经验正文引用了它),
// 把那些也算进来会让"注入了哪些技能"多出根本没注入的名字。
func injectedSkillNames(prompt string) []string {
	const openTag, closeTag = "<role_skills>", "</role_skills>"
	i := strings.Index(prompt, openTag)
	if i < 0 {
		return nil
	}
	j := strings.Index(prompt[i:], closeTag)
	if j < 0 {
		return nil // 段没闭合 (被截断): 宁可不记, 也不记半截名单
	}
	seg := prompt[i+len(openTag) : i+j]
	var out []string
	seen := map[string]bool{}
	for _, ln := range strings.Split(seg, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "### Skill:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(ln, "### Skill:"))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
