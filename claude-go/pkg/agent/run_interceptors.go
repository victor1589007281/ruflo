package agent

// run_interceptors.go —— 运行级切面链 (design/01 §4.10 的运行级那一半)。
//
// ---------------------------------------------------------------------------
// 为什么运行级要单独一条链, 而不是全塞进 pkg/graph 的 NodeInterceptor
// ---------------------------------------------------------------------------
//
// §4.10 的表里六项拦截器, 收编对象分属两个层次:
//
//	节点级 (一次节点执行)  : BudgetManager (已在 pkg/graph)、RateLimiter、经验注入/轨迹记录
//	运行级 (一次团队运行)  : 产码门禁、团队指标、团队学习触发、飞书收尾通知、媒体推送
//
// 后者收编的代码全都在 executeWorkflow 的**主体执行之后**, 一次运行只发生一次, 而且
// 拿到的是整份 []StageResult。把它们做成 NodeInterceptor 有两个硬问题:
//
//  1. **看不到全局**: 产码门禁要在所有阶段跑完后判"这次交付能不能过", 节点切面手上
//     只有一个节点; 团队指标同理。
//  2. **挂不到默认路径**: NodeInterceptor 的挂载点是 engine.callRunner, 而
//     CLAUDE_GO_GRAPH_ENGINE 默认关 ⇒ pipeline/fanout 类工作流根本不进图引擎
//     (只有 orchestrated / mode=graph 会)。把门禁/通知搬进节点链, 等于让 8+ 个下游
//     平台的主路径**丢掉门禁与通知** —— 那不是重构, 那是生产事故。
//
// 所以这一层自己有一条链。两条链的语义刻意保持一致 (顺序确定 / 可开关 / 名字唯一 /
// 明示拒绝是正当用法 / panic 不穿透且不当放行), 见下文。
//
// ---------------------------------------------------------------------------
// 为什么是"顺序链"而不是"洋葱"
// ---------------------------------------------------------------------------
//
// 节点级切面是洋葱 (Around(next)), 因为它要能**包住**执行、在前后都插手。运行级这五
// 件事全是"主体跑完之后按固定次序做的收尾", 且现状次序有真实依赖:
//
//	门禁裁决 → 落终态 → 出指标 → 记奖励/触发学习 → 落报告+记忆 → 发通知(文案里带报告路径)
//
// 洋葱语义下 [0] 最外层意味着它的**后置**动作最后执行, 于是"先记奖励再落报告"这种
// 顺序要靠"把 evolution 装得比 memory 更内层"来表达 —— 而 memory 又必须比 notify 内层
// (通知要引用报告路径)。层数一多, 顺序表就藏在装配顺序的反向推理里, 而这正是本文件
// 想消除的东西。顺序链把它写成一张能直接读的表 (runBuiltinPhases)。
//
// 洋葱唯一真正需要的能力 —— **中止后续** (fail-closed 门禁) —— 顺序链用返回值表达:
// After 返回 false = 明示中止, 与"忘了做什么"可区分 (见 §4.10 对 next 的同类要求)。
//
// ---------------------------------------------------------------------------
// fail-open 还是 fail-closed
// ---------------------------------------------------------------------------
//
// 本仓原则"fail-closed 的治理, fail-open 的交付"在这条链上的落法:
//   - 门禁 (治理) 判失败 ⇒ 中止链并把团队判失败, 绝不放行;
//   - 拦截器自己 panic ⇒ 也判失败 (不当成放行)。注意这一处**改善了**现状: 拆分前
//     这些代码裸跑在 executeWorkflow 里, 一个 panic 会带走整个 :18080 进程 (团队
//     goroutine 无 recover)。现在只判这一个团队失败。除此之外行为不变。
//   - delivered_with_remediation (交付) 是 fail-open, 它的判定在链之前
//     (computeRunDelivery), 不在门禁拦截器里 —— 见那里的注释。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/trace"
)

// teamRunContext 一次团队运行的收尾上下文 (运行级拦截器共享的可变状态)。
//
// 刻意用一个可变结构体而不是"每个拦截器返回新值": 链上各环改的是不同字段
// (门禁改 status、finalize 填 report、memory 填 reportPath), 值语义会让每加一个字段
// 就要改所有环的签名。字段谁读谁写在各自注释里写清。
type teamRunContext struct {
	ptm      *ProductionTeamManager
	team     *ProductionTeam
	wf       *WorkflowDef
	executor *WorkflowExecutor

	// results 阶段产出。门禁环可能替换它 (内容质量门禁会追加修订阶段)。
	results []StageResult
	// status 交付终态。进链时已由 computeRunDelivery 定为 completed 或
	// delivered_with_remediation; 门禁环只能把它改成失败 (且那时直接中止链)。
	status TeamStatus
	// startedAt/finishedAt/report 由 finalize 环填, 之后各环只读。
	// 单独存一份是为了不在锁外读 team 的字段 (拆分前那几处读是裸读)。
	startedAt  time.Time
	finishedAt time.Time
	report     logging.TeamRunReport
	// reportPath 由 memory 环填, notify 环读 (完成通知里的报告链接)。
	reportPath string
}

// RunInterceptor 运行级切面 (design/01 §4.10)。
//
// 第三方横切逻辑 (设计文档点名的 aiops 权限桥/审计) 实现它并经
// RegisterRunInterceptor 注入, 而不是改 executeWorkflow。
type RunInterceptor interface {
	// Name 用于开关配置与归因, 必须稳定且唯一 (重名会让"是谁中止了这次交付"不可考)。
	Name() string
	// After 在工作流主体执行完成后按注册序调用一次。
	// 返回 false = **明示中止**后续环 (治理拒绝)。此时实现方自己负责把团队判失败,
	// 因为只有它知道失败原因文案。
	After(ctx context.Context, rc *teamRunContext) bool
}

// runPhaseFunc 用函数构造运行级拦截器 (内置各环与测试用)。
type runPhaseFunc struct {
	name string
	// core = 不可经开关关掉。只有 finalize 是 core: 它不是横切关注点, 而是这次运行的
	// 终态写入 —— 关掉它团队会永远停在 running, 那不是"回滚一个切面"而是坏掉。
	core bool
	fn   func(ctx context.Context, rc *teamRunContext) bool
}

// Name 见 RunInterceptor。
func (p runPhaseFunc) Name() string { return p.name }

// After 见 RunInterceptor。
func (p runPhaseFunc) After(ctx context.Context, rc *teamRunContext) bool {
	if p.fn == nil {
		return true
	}
	return p.fn(ctx, rc)
}

// —— 第三方注入 ——

var (
	extraRunMu           sync.RWMutex
	extraRunInterceptors []RunInterceptor
)

// RegisterRunInterceptor 注册第三方运行级拦截器 (追加在内置链**之后**)。
//
// 为什么只能追加在最后: 内置链的次序是有依赖的 (门禁→终态→指标→学习→记忆→通知),
// 允许第三方插到中间等于允许它看到半落定的状态。追加在最后时它拿到的是完整、已落定
// 的一次运行 —— 审计/权限桥要的正是这个。要在执行**之前**插手请用 NodeInterceptor。
//
// ⚠️ 只能在任何团队启动前调用 (进程装配阶段)。
func RegisterRunInterceptor(ic RunInterceptor) error {
	if ic == nil {
		return errors.New("agent: 运行级拦截器为 nil")
	}
	if strings.TrimSpace(ic.Name()) == "" {
		return errors.New("agent: 运行级拦截器 Name 为空")
	}
	extraRunMu.Lock()
	defer extraRunMu.Unlock()
	for _, e := range extraRunInterceptors {
		if e.Name() == ic.Name() {
			return fmt.Errorf("agent: 运行级拦截器重名 %q", ic.Name())
		}
	}
	extraRunInterceptors = append(extraRunInterceptors, ic)
	return nil
}

// registeredRunInterceptors 取第三方链快照。
func registeredRunInterceptors() []RunInterceptor {
	extraRunMu.RLock()
	defer extraRunMu.RUnlock()
	return append([]RunInterceptor(nil), extraRunInterceptors...)
}

// —— 链装配与执行 ——

// runBuiltinPhases 内置运行级拦截器**顺序表**。这张表就是收尾流程的次序真源。
//
// 每一环的名字同时是开关名 (CLAUDE_GO_RUN_INTERCEPTORS)。次序不可随意调:
//   - gate 必须最先: 它可能把交付判失败, 之后各环都在报告"已经定下来的终态"
//   - finalize 必须在 gate 之后、其余之前: 它写 team.Status 并造运行报告, 后面全靠它
//   - memory 必须在 notify 之前: 完成通知的文案里带报告文件路径
func runBuiltinPhases(ptm *ProductionTeamManager) []RunInterceptor {
	return []RunInterceptor{
		runPhaseFunc{name: "gate", fn: ptm.runPhaseGate},
		runPhaseFunc{name: "finalize", core: true, fn: ptm.runPhaseFinalize},
		runPhaseFunc{name: "metrics", fn: ptm.runPhaseMetrics},
		runPhaseFunc{name: "evolution", fn: ptm.runPhaseEvolution},
		runPhaseFunc{name: "memory", fn: ptm.runPhaseMemory},
		runPhaseFunc{name: "notify", fn: ptm.runPhaseNotify},
	}
}

// filterRunInterceptors 按开关过滤并校验重名/空名/nil。
//
// 开关语义与 CLAUDE_GO_GRAPH_INTERCEPTORS 完全一致 (复用 parseInterceptorSwitch):
// 未设=全开; "off"/"none"=只留 core 环; 逗号分隔=白名单 (core 环恒在)。
//
// 校验放在开跑前 (而不是遇到时跳过): Name 是归因与开关的键, 重名会让"谁中止了这次
// 交付"永久不可考 —— 与 pkg/graph 的 validateInterceptors 同一条理由。
func filterRunInterceptors(sw string, builtin, extra []RunInterceptor) ([]RunInterceptor, error) {
	enabled := parseInterceptorSwitch(sw)
	all := append(append([]RunInterceptor(nil), builtin...), extra...)
	seen := map[string]bool{}
	out := make([]RunInterceptor, 0, len(all))
	for _, ic := range all {
		if ic == nil {
			return nil, errors.New("agent: 运行级拦截器为 nil")
		}
		name := strings.TrimSpace(ic.Name())
		if name == "" {
			return nil, errors.New("agent: 运行级拦截器 Name 为空")
		}
		if seen[name] {
			return nil, fmt.Errorf("agent: 运行级拦截器重名 %q", name)
		}
		seen[name] = true
		if core, ok := ic.(runPhaseFunc); ok && core.core {
			out = append(out, ic) // core 环不受开关约束
			continue
		}
		if enabled != nil && !enabled[name] {
			continue
		}
		out = append(out, ic)
	}
	return out, nil
}

// runAfterChain 跑完运行级链。返回 false 表示链被中止 (团队已被判失败)。
func (ptm *ProductionTeamManager) runAfterChain(ctx context.Context, rc *teamRunContext) bool {
	chain, err := filterRunInterceptors(
		os.Getenv("CLAUDE_GO_RUN_INTERCEPTORS"),
		runBuiltinPhases(ptm),
		registeredRunInterceptors(),
	)
	if err != nil {
		// 链装不起来 = 治理设施坏了, fail-closed。
		ptm.failTeam(rc.team, "运行级拦截器链非法: "+err.Error())
		return false
	}
	logging.Event(ctx, "team.run.chain", "team", rc.team.Name,
		"interceptors", strings.Join(runInterceptorNames(chain), ","))
	for _, ic := range chain {
		if !ptm.applyRunInterceptor(ctx, ic, rc) {
			return false
		}
	}
	return true
}

// applyRunInterceptor 跑一环, 并把 panic 关在这一环里。
//
// panic 不穿透但**也不当成放行**: 转为团队失败。理由与 pkg/graph 相同 —— 拦截器崩了
// 属于必须暴露的失败。这里额外有个现实理由: 拆分前这段代码裸跑在团队 goroutine 里,
// 一个 panic 会带走整个 :18080 进程 (8+ 平台一起挂)。
func (ptm *ProductionTeamManager) applyRunInterceptor(ctx context.Context, ic RunInterceptor, rc *teamRunContext) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			logging.Event(ctx, "team.run.interceptor.panic", "team", rc.team.Name,
				"interceptor", ic.Name(), "panic", fmt.Sprint(r))
			ptm.failTeam(rc.team, fmt.Sprintf("运行级拦截器 %s panic: %v", ic.Name(), r))
		}
	}()
	return ic.After(ctx, rc)
}

// runInterceptorNames 按注册序 (即执行序) 列名。**不排序** —— 顺序即语义。
func runInterceptorNames(ics []RunInterceptor) []string {
	out := make([]string, 0, len(ics))
	for _, ic := range ics {
		out = append(out, ic.Name())
	}
	return out
}

// ---------------------------------------------------------------------------
// 内置环 1: GateEnforcer (§4.10 表第 3 行)
// ---------------------------------------------------------------------------

// runPhaseGate 产码门禁 (编译/测试/一致性) + 内容质量门禁。
//
// **判据读 GraphSpec.Meta 声明, 不按工作流名白名单** —— WorkflowGateMetaByName 是那份
// 声明的唯一入口, 图引擎与这里共用同一份, 于是动态注册的工作流不会再静默丢门禁。
//
// fail-closed: 任一门禁未过即判团队失败并中止链。这一条不许改成 fail-open ——
// "编译不过也交付"是本仓明确禁止的形态。
func (ptm *ProductionTeamManager) runPhaseGate(ctx context.Context, rc *teamRunContext) bool {
	team := rc.team
	// Global Gates: compile, test, and consistency checks after workflow stages complete.
	//
	// 注: 当 compile / test gate 失败时, 跑 1-2 轮"修复尝试", 由 coder 角色
	// 拿到具体错误输出去修源码 (修 bug / 删重复声明 / 修 vet 警告 / 删死循环).
	// 修完后再跑 gate. 这样团队就有自我恢复能力, 不会因为单个 vet 警告或
	// 一处明显 bug 就把 30 分钟的工作直接判废.
	// 编译/测试/一致性门禁仅对"产出可编译代码"的工作流生效。
	// techblog/creative/novel/research 等写作类工作流不产出代码, 跑 go build 会因
	// "no main module" 误判失败并触发无意义的修复轮次 (浪费 token)。
	if team.Cwd != "" && WorkflowGateMetaByName(team.Workflow).ProducesCode {
		if gateErr := ptm.tryGateWithRemediation(ctx, team, rc.executor, "compile",
			ptm.runGlobalCompileGate, 2); gateErr != "" {
			ptm.failTeam(team, fmt.Sprintf("全局编译门禁失败: %s", gateErr))
			return false
		}
		if gateErr := ptm.tryGateWithRemediation(ctx, team, rc.executor, "test",
			ptm.runGlobalTestGate, 2); gateErr != "" {
			ptm.failTeam(team, fmt.Sprintf("全局测试门禁失败: %s", gateErr))
			return false
		}
		if gateErr := ptm.runGlobalConsistencyCheck(team); gateErr != "" {
			ptm.failTeam(team, fmt.Sprintf("全局一致性检查失败: %s", gateErr))
			return false
		}
	}

	// 内容质量门禁: 写作类(pipeline)工作流的"评审→未达标→自动修订"环 (见 content_gate.go)。
	// 仅对声明了 content 质量门禁的工作流生效; 跳过用户驱动的精修运行 (PendingFeedback
	// 非空), 避免双重注入。
	if WorkflowGateMetaByName(team.Workflow).QualityGate == "content" && strings.TrimSpace(team.PendingFeedback) == "" {
		rc.results = ptm.tryContentQualityGate(ctx, team, rc.executor, rc.wf, rc.results)
	}
	return true
}

// ---------------------------------------------------------------------------
// 内置环 2: finalize (core, 不属 §4.10 六项之一)
// ---------------------------------------------------------------------------

// runPhaseFinalize 落终态 + 精修留痕 + TaskCompleted hooks + 造运行报告。
//
// 它**不是**横切关注点, 所以不可关闭; 放进这条链只是为了让"收尾次序"在
// runBuiltinPhases 一张表里读得完, 而不是一半在表里一半散在 executeWorkflow。
func (ptm *ProductionTeamManager) runPhaseFinalize(_ context.Context, rc *teamRunContext) bool {
	team := rc.team
	team.mu.Lock()
	team.Status = rc.status
	team.FinishedAt = time.Now()
	team.Stages = rc.results
	// 顺手在同一把锁里取走后续各环要用的量。拆分前这些字段是在锁外裸读的
	// (executeWorkflow 里 report 的 StartTime/EndTime/Status), 值一样但那是数据竞争。
	rc.startedAt, rc.finishedAt = team.StartedAt, team.FinishedAt
	team.mu.Unlock()
	ptm.finishRefine(team, true) // 若本轮是精修: 清空反馈并留痕"已采纳", 喂 Evolution 经验闭环
	team.persist()
	ptm.runTaskCompletedHooks(rc.results)

	// 结构化运行报告 (可观测性: 供后续 AI 分析团队运行效果)
	rc.report = logging.TeamRunReport{
		TeamName: team.Name, Workflow: team.Workflow, Objective: team.Objective,
		StartTime: rc.startedAt, EndTime: rc.finishedAt,
		DurationSec: rc.finishedAt.Sub(rc.startedAt).Seconds(),
		Status:      string(rc.status),
	}
	for _, r := range rc.results {
		durSec := 0.0
		if d, err := time.ParseDuration(r.Duration); err == nil {
			durSec = d.Seconds()
		}
		rc.report.Stages = append(rc.report.Stages, logging.StageReport{
			Name: r.Name, Role: r.Role, DurationSec: durSec,
			Status: string(r.Status), OutputLen: len(r.Output), Error: r.Error,
		})
	}
	return true
}

// ---------------------------------------------------------------------------
// 内置环 3: MetricsEmitter (§4.10 表第 4 行)
// ---------------------------------------------------------------------------

// runPhaseMetrics 团队级指标 + 结构化运行日志。
//
// **阶段级指标不在这里** —— 那一半早已单源: pipeline 路径的
// Coordinator.recordStageMetrics 直接委托图侧的 recordGraphStageMetrics, 两条路径
// 共用同一份实现与同样 4 个 label。再在这条链上记一遍就是双计 (dashboard 的阶段
// 通过率会凭空翻倍), 所以这一环只管"一次运行一条"的团队级量。
func (ptm *ProductionTeamManager) runPhaseMetrics(ctx context.Context, rc *teamRunContext) bool {
	team := rc.team
	logging.LogTeamRun(ctx, rc.report)
	logging.IncrCounter("team.complete." + team.Workflow)

	// 持续观测指标: 团队运行质量
	if ptm.metrics != nil {
		labels := map[string]string{"workflow": team.Workflow, "status": string(rc.status)}
		recordTeamRun(ptm.metrics, "team", metrics.MTeamRunCount, 1, team, labels)
		recordTeamRun(ptm.metrics, "team", metrics.MTeamDurationSec, rc.report.DurationSec, team, labels)
		if isSuccessfulTeamStatus(rc.status) {
			recordTeamRun(ptm.metrics, "team", metrics.MTeamSuccessCount, 1, team, labels)
		} else {
			recordTeamRun(ptm.metrics, "team", metrics.MTeamFailCount, 1, team, labels)
		}
		// 阶段通过率
		total, passed := 0, 0
		totalOutLen := 0
		for _, s := range rc.report.Stages {
			total++
			if s.Status == string(TaskCompleted) {
				passed++
			}
			totalOutLen += s.OutputLen
		}
		if total > 0 {
			recordTeamRun(ptm.metrics, "team", metrics.MTeamStagePassRate, float64(passed)/float64(total), team, labels)
			recordTeamRun(ptm.metrics, "team", metrics.MTeamOutputAvgLen, float64(totalOutLen)/float64(total), team, labels)
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 内置环 4: EvolutionRecorder (§4.10 表第 2 行的运行级那一半)
// ---------------------------------------------------------------------------

// runPhaseEvolution episode 奖励 + 时长 shaping + 团队学习触发 + 技能提炼 + Dreaming。
//
// §4.10 表里 EvolutionRecorder 的三件收编对象分处两层:
//   - 经验注入 (workflow.go RETRIEVE) 与轨迹记录 (RECORD) 是**节点级**, 已在
//     executeStage 里, 而 executeStage 是图路径与 pipeline 路径**共用**的
//     (stageNodeRunner → ExecuteSingleStage → executeStage), 所以那两件在两条路径上
//     本来就同构, 不需要搬。唯一漏网的是 orchNodeRunner (裸 completion 内核, 不经
//     executeStage), 见文件尾"已知缺口"。
//   - 团队学习触发是**运行级**, 就是这一环。
func (ptm *ProductionTeamManager) runPhaseEvolution(ctx context.Context, rc *teamRunContext) bool {
	team := rc.team
	// episode 级奖励 (design/03 §4.2): 团队终态是最稳定的奖励信号, 统一落 rewards.jsonl。
	// delivered_with_remediation 记 0.5 —— fail-open 交付语义: 交付了但过程有瑕疵。
	if ptm.evolution != nil {
		episodeVal := -1.0
		switch rc.status {
		case TeamStatusCompleted:
			episodeVal = 1.0
		case TeamStatusDeliveredWithRemediation:
			episodeVal = 0.5
		}
		ptm.evolution.RecordReward(RewardEvent{
			RunID:  trace.From(ctx).RunID,
			Source: RewardSourceEpisode,
			Value:  episodeVal,
			Raw:    string(rc.status),
			Team:   team.Name,
		})
		// 时长 shaping 负项 (design/03 §4.2 第 8 行, 防"堆 turn 堆 token 刷分")。
		// 只在超预算时写, 预算内不写 —— 见 recordLatencyReward。
		ptm.recordLatencyReward(ctx, team, rc.report.DurationSec)
		// turn 级启发式裁决 (design/03 §4.2 第 7 行) —— 从本 run 的轨迹派生。
		// 必须在这里而不是 turn 发生时: 引擎侧的 turn verdict 没有 run 归因,
		// 而带 run 归因的轨迹只有 run 收尾时才读得全。见 verdict_reward.go。
		ptm.recordVerdictRewards(ctx, team)
	}

	// 触发进化学习 (DISTILL: 从轨迹中提炼经验) + 采集进化指标。
	// 经统一循环提交 (design/03 §4.3), 循环未装配时回落到原来的直调路径。
	ptm.submitLearn(team.Name)

	// 技能自创建 (design/03 §1.2 开环2 接线): 仅全阶段通过的干净成功才尝试提炼,
	// 是否值得由 LLM 自判 (AutoCreator 内含判据); 产物 frontmatter 带 status: shadow,
	// 晋升裁决归进化门禁 (E3), 提炼失败静默不影响交付。
	if ptm.skillCreator != nil && len(rc.results) > 0 && allStagesCompleted(rc.results) {
		// design/03 §4.6 必过闸: 全阶段通过只说明"没崩", 不说明"做得好"。这里消费本 run 的
		// 门禁类奖励 (gate.compile/gate.test/gate.content; 排除与交付状态共线的 episode):
		// 有证据且为负时不提炼 —— 否则会把一次"编译勉强过但内容评审很差"的做法固化成技能。
		// 无证据时保持原行为, 否则未接奖励源的工作流永远产不出技能。
		gateScore, gateEvidence := ptm.evolution.GateRewardScore(trace.From(ctx).RunID, team.Name)
		if !skillDistillAllowed(gateScore, gateEvidence) {
			// 只跳过提炼, 不影响后续交付流程 (报告/记忆/Dreaming)。
			logging.Event(ctx, "skill.distill.skipped", "team", team.Name,
				"reason", "gate_reward_negative", "gate_score", gateScore)
		} else {
			objective := team.Objective
			chatID := team.ChatID
			approach := summarizeStageApproach(rc.results)
			outcome := lastNonEmptyOutput(rc.results, 2000)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				if name, err := ptm.skillCreator.MaybeCreate(ctx, objective, approach, outcome); err == nil && name != "" {
					ptm.notify(chatID, fmt.Sprintf("🧬 已自动提炼 shadow 技能: **%s** (待进化门禁验证晋升)", name))
				}
			}()
		}
	}

	// 触发 Dreaming 记录 (覆盖 team agent 会话盲区)
	if ptm.dreamer != nil {
		for _, r := range rc.results {
			ptm.dreamer.RecordSession(DreamSessionRecord{
				ChatID:  team.ChatID,
				EndTime: time.Now(),
				Summary: fmt.Sprintf("[Team:%s] [%s/%s] %s", team.Name, r.Name, r.Role, truncateResult(r.Output, 300)),
			})
		}
		// 团队完成后触发 Dreaming 检查 (解决仅有团队工作流时不触发的问题)
		ptm.dreamer.AfterQuery(context.Background())
	}
	return true
}

// ---------------------------------------------------------------------------
// 内置环 5: memory (§4.10 表里没有单独一行, 见注释)
// ---------------------------------------------------------------------------

// runPhaseMemory 报告落盘 + 高权重记忆写入。
//
// 为什么单独一环而不是折进 notify 或 evolution: 现状次序是
// "落报告 → 写记忆 → 发通知", 而通知文案要引用报告路径。折进 evolution 会把两次写
// 提到奖励记账之前, 折进 notify 会让一个叫"通知"的环里藏着两次持久化。既然拆分的
// 硬约束是**一次写入都不许换位置**, 就照现状的边界给它一个自己的名字。
// (用户口径里的"门禁/进化/记忆/通知"四件事, 这是"记忆"那件。)
func (ptm *ProductionTeamManager) runPhaseMemory(_ context.Context, rc *teamRunContext) bool {
	team := rc.team
	// 持久化完整报告文件 (解决产出散落、无法检索的问题)
	rc.reportPath = ptm.saveTeamReport(team, rc.results)

	// 写入高权重记忆 (解决"失忆"问题: 团队名+目标+结果摘要可被 BM25 检索)
	if ptm.memWriter != nil {
		var stageSummary string
		for _, r := range rc.results {
			if r.Output != "" {
				stageSummary += fmt.Sprintf("[%s/%s] %s\n", r.Name, r.Role, truncateResult(r.Output, 200))
			}
		}
		ptm.memWriter.AddTeamMemory(team.Name, team.Workflow, team.Objective, stageSummary)
	}
	return true
}

// ---------------------------------------------------------------------------
// 内置环 6: Notifier (§4.10 表第 5 行)
// ---------------------------------------------------------------------------

// runPhaseNotify 收尾通知 + 媒体推送。
func (ptm *ProductionTeamManager) runPhaseNotify(_ context.Context, rc *teamRunContext) bool {
	team := rc.team
	var summary string
	for _, r := range rc.results {
		if r.Output != "" {
			summary += fmt.Sprintf("\n\n**[%s]**\n%s", r.Role, truncateResult(r.Output, 500))
		}
	}
	reportNote := ""
	if rc.reportPath != "" {
		reportNote = fmt.Sprintf("\n\n📄 **完整报告**: `%s`", rc.reportPath)
	}
	ptm.notify(team.ChatID, fmt.Sprintf("✅ 团队 **%s** 执行完成 (耗时 %v)\n\n**成果汇总:**%s%s",
		team.Name, time.Since(rc.startedAt).Round(time.Second), summary, reportNote))

	// Creative 工作流: 提取 SVG/HTML 多媒体资产，通过媒体通道发送
	if ptm.mediaNotify != nil && (team.Workflow == "creative") {
		ptm.sendMediaAssets(team, rc.results)
	}
	return true
}

// ---------------------------------------------------------------------------
// 已知缺口 (记账在此, 不假装做完了)
// ---------------------------------------------------------------------------
//
//  1. **orchNodeRunner 不产轨迹**: orchestrated 模式的节点内核是裸 completion, 不经
//     executeStage, 于是它既不注入经验也不记轨迹。要补齐必须让节点级切面接过这两件事,
//     而节点级切面拿不到"最终提示词" (提示词在 runner 内部拼), 所以需要 executeStage
//     配合让位 (ctx 标记 + 提示词回填)。那是改共享热路径的动作, 与本轮"默认行为一字
//     不变"的硬约束冲突, 故未做。
//  2. **headless (claude-go run) 路径不实例化 EvolutionEngine**, 所以这条链在 headless
//     下同样是空转 —— design/03 诊断的那个开环在团队路径这一侧已闭合 (submitLearn 在
//     这一环), 在单次查询路径那一侧仍开着, 因为那条路径根本没有"一次运行"的概念。
