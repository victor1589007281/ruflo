package agent

// orchestrated_runner —— orchestrated 模式的节点执行内核 (design/01 M4: 退役 pkg/orchestrator)。
//
// ---------------------------------------------------------------------------
// 它替换了什么
// ---------------------------------------------------------------------------
//
// 旧 orchestrated 路径 (pkg/orchestrator.Engine) 的执行内核由三块组成:
//
//	orchestrator.LLMRunner          裸 LLM completion (无工具, 无角色模板合并, 无黑板交接)
//	orchestrator.AdversarialRunner  对抗阶段的内层多轮循环 + QualityTermination(7.0, 0.5)
//	orchestrator.RetryPolicy        三级错误分治 (fatal/transient/permanent) 的重试额度
//
// 本文件把这三块**逐条搬到 pkg/agent**, 挂在 pkg/graph 的 NodeRunner 接口上, 于是
// orchestrated 与 pipeline/fanout/adversarial/trading_debate 共用同一个调度器、同一份
// Journal、同一条 hook 桥、同一条拦截器链, 而 pkg/orchestrator 整包可以删除。
//
// **为什么不直接用 stageNodeRunner** (图路径既有的 runner): 它跑
// ExecuteSingleStage —— 角色模板合并 (RoleRegistry.MergedPrompt)、黑板 HandoffContext
// 注入、进化经验注入、**工具权限**、内层重试。orchestrated 的 5 个生产工作流
// (code-review / testing / hiring / parenting / manager-lab-simulation-v2) 现状跑的是
// 裸 completion: 没有工具、没有角色模板。换成 stageNodeRunner 等于悄悄给这些阶段
// 发了 Bash/Write 权限并改了提示词 —— 那是行为变更不是等价迁移, 也是 graph_templates.go
// 里 modeGraphNotTemplated["orchestrated"] 记的那条能力缺口。本文件就是补那条缺口。
//
// ---------------------------------------------------------------------------
// 刻意保留的三处"看起来像 bug"的行为 (等价性优先)
// ---------------------------------------------------------------------------
//
//  1. **系统提示词里的占位符不做替换**: 旧 LLMRunner 只对 user prompt 调
//     resolveTemplate, system prompt (= StageDef.Prompt) 原样下发。于是 code-review 的
//     "审查任务: {objective}" 这类字面量真的进了模型上下文。objective/上游产出是经
//     user prompt (固定模板 orchUserPromptTemplate) 到达的, 所以能力上不缺, 只是
//     system prompt 里多了几个没替换的花括号。修它会改变每个阶段的提示词 ⇒ 改变模型
//     产出 ⇒ 等价性测试必红。故保留, 记账在报告里另行处理。
//  2. **对抗反馈写进 task.Config 但没人读**: 旧 AdversarialRunner 每轮把
//     adversarial_feedback / adversarial_round / review_target 塞进 task.Config, 而
//     LLMRunner 只认 system_prompt/user_prompt/objective ⇒ 反馈从未进入提示词。所以
//     "对抗循环"的真实语义是**同一个提示词最多重跑 3 轮, 取最后一轮产出**。这里照抄:
//     该有的 LLM 调用次数一次不少 (token 账一致), 但不凭空发明反馈回灌。
//  3. **QualityTermination 跨对抗节点共享**: 旧实现一个工作流只造一个
//     AdversarialRunner 实例, 它持有的 QualityTermination 会把**所有**对抗任务的评分
//     累积进同一个 scores 切片 (收敛/退化判定因此串味)。生产上只有 code-review 有
//     且仅有 1 个对抗阶段, 所以不可观测。照抄 + 记账, 而不是"顺手修好"。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/logging"
)

// orchUserPromptTemplate 用户提示词模板。
// 旧路径把它硬编码在 buildGraph 的 task.Config["user_prompt"] 里 (每个阶段都一样),
// 这里提成常量 —— 它是 orchestrated 与 pipeline 之间最本质的差异 (pipeline 走
// buildStagePromptWithRoles 的角色模板), 藏在 map 字面量里没人能一眼看到。
const orchUserPromptTemplate = "{prev_result}\n\n任务目标: {objective}"

// orchDefaultSystemPrompt 阶段未声明 Prompt 时的兜底 (照抄 LLMRunner)。
const orchDefaultSystemPrompt = "你是一个专业的AI助手。"

// 对抗循环参数 (照抄 workflow_orchestrated.go 旧 buildAdversarialRunner 的实参)。
const (
	orchAdversarialRounds    = 3   // NewAdversarialRunner(..., maxRounds=3)
	orchAdvPassThreshold     = 7.0 // NewQualityTermination(passThreshold=7.0, ...)
	orchAdvConvergeDelta     = 0.5 // NewQualityTermination(..., convergeDelta=0.5, ...)
	orchAdversarialRunnerCap = 1   // RunnerPool.SetLimit("llm-adversarial", 1)
)

// orchNodeRunner orchestrated 模式的 graph.NodeRunner 实现。
type orchNodeRunner struct {
	we        *WorkflowExecutor
	team      *ProductionTeam
	objective string

	// deps 节点 → 上游节点 ID (按**边声明序**, 即 StageDef.DependsOn 的声明序)。
	// 旧 LLMRunner 遍历 task.DependsOn 拼 {prev_result}, 顺序必须逐字相同 ——
	// 换成字典序会改变提示词 (进而改变产出与 prompt 缓存命中)。
	deps map[string][]string

	// adversarial 节点 → 是否对抗阶段 (isAdversarialStage 判据, 与旧 buildGraph 同源)。
	adversarial map[string]bool
	// reviewers 对抗内层的评审者数量 = 工作流里对抗阶段的个数。
	// 旧 buildAdversarialRunner 就是 `for i, adv := range advStages` 造一个 reviewer,
	// 与"哪个任务在跑"无关 —— 是个全局数量。照抄。
	reviewers int

	// policy 对抗终止策略, **跨对抗节点共享** (见文件头第 3 条)。
	policy *orchQualityTermination
	// advSem 复刻 RunnerPool.SetLimit("llm-adversarial", 1): 对抗节点串行。
	// 生产上只有 1 个对抗阶段所以不可观测, 但留着才对得上"并发闸"这一项等价。
	advSem chan struct{}

	// retries 本次运行的累计重试次数, 只供收尾通知的"N 次重试"文案 (旧路径取
	// ExecutionMetrics.TotalRetries)。必须记在这里: 图层重试恒 0, 引擎 hook 载荷里的
	// attempts 永远是 1, 真正的重试发生在 RunNode 内部。
	retries atomic.Int64
}

// newOrchNodeRunner 按最终 spec 装配 orchestrated 内核。
func newOrchNodeRunner(we *WorkflowExecutor, team *ProductionTeam, objective string, spec graph.GraphSpec) *orchNodeRunner {
	r := &orchNodeRunner{
		we: we, team: team, objective: objective,
		deps:        graphNodeDeps(spec),
		adversarial: map[string]bool{},
		advSem:      make(chan struct{}, orchAdversarialRunnerCap),
	}
	for _, n := range spec.Nodes {
		if isAdversarialStage(StageDef{Name: n.ID, Role: n.Agent.Role}) {
			r.adversarial[n.ID] = true
			r.reviewers++
		}
	}
	if r.reviewers > 0 {
		r.policy = newOrchQualityTermination(orchAdvPassThreshold, orchAdvConvergeDelta)
	}
	return r
}

var _ graph.NodeRunner = (*orchNodeRunner)(nil)

// RunNode 执行一个 orchestrated 节点。
//
// 三段: ① 级联闸 (复刻 AND-join + cascadeFailure) → ② 单次尝试超时 →
// ③ 三级错误分治重试 (复刻 orchestrator.RetryPolicy)。
func (r *orchNodeRunner) RunNode(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	// —— ① 级联闸: 复刻旧引擎的 AND-join + cascadeFailure ——
	//
	// 旧引擎 unblockDownstream 要求**全部**上游 Completed 才解锁, 且上游永久失败时
	// cascadeFailure 递归把下游全部标 Cancelled (错误文案 "上游任务 X 失败, 级联取消")。
	// pkg/graph 的 join 语义是 OR-join 且失败上游不阻断下游 (与 pipeline 一致):
	// 4 个上游里挂 1 个, 下游照样跑, 只是 PrevOutputs 少一份。
	//
	// 两者不等价, 而 orchestrated 的下游阶段 (汇总/报告) 拿不全上游就是废产出。
	// 与其改 pkg/graph 的 join 语义 (那会影响 pipeline 灰度路径), 在 runner 里判:
	// 声明期上游只要有一个不在 PrevOutputs 里 (= 未 completed), 本节点直接失败,
	// 零 LLM 调用。这样 AND-join、级联、错误文案、token 账四项全部对齐,
	// 且级联是**自然传递**的 (被判失败的节点其下游同样看不到它的产出)。
	//
	// 与旧实现的唯一差别: 多个上游同时失败时, 旧文案取"递归先到达的那个"
	// (取决于 map 遍历序, 不确定), 这里取声明序第一个 —— 确定性更好。
	if missing := r.missingDep(node.ID, in.PrevOutputs); missing != "" {
		// 错误文案连**任务 ID 形态**一起照抄 (<团队名>/<阶段名>): 它会经 StageResult.Error
		// 进 team.json / REPORT.md / 飞书, 改了字面量就是改了对外可见的输出。
		err := fmt.Sprintf("上游任务 %s 失败, 级联取消", r.taskID(missing))
		logging.Event(ctx, "orchestrated.node.cascade", "node", node.ID, "missing", missing)
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: err}
	}

	// 单次尝试的超时: 旧 executeTask 每次派发都新建 context.WithTimeout(ctx, t.Timeout),
	// 所以每次重试都拿到完整的 3/4/5 分钟。这里同构 —— **不能**把它填进
	// NodeSpec.TimeoutSec, 那是包住全部重试的总预算 (见 graph_adapter.go 的说明)。
	attemptTimeout := taskTimeout(StageDef{Name: node.ID, Role: node.Agent.Role})

	var (
		out string
		err error
	)
	for attempt := 0; ; attempt++ {
		out, err = r.attempt(ctx, node, in, attemptTimeout)
		if err == nil {
			return graph.NodeResult{Status: graph.NodeStatusCompleted, Output: out}
		}
		d := orchDefaultRetryPolicy().ShouldRetry(err.Error(), attempt)
		logging.Event(ctx, "orchestrated.node.error", "node", node.ID,
			"attempt", fmt.Sprintf("%d", attempt+1), "kind", d.Kind.String(),
			"retry", fmt.Sprintf("%v", d.ShouldRetry), "err", err.Error())
		if !d.ShouldRetry {
			break
		}
		r.retries.Add(1)
		if !orchSleepFn(ctx, d.Delay) {
			break // ctx 取消: 停止重试, 保留最后一次失败
		}
	}
	// 旧路径瞬态额度耗尽时置 TaskSuspended (不级联, 等 stallRecovery 唤醒);
	// 那条路在 3 次停滞恢复用尽后会让引擎主循环**永久空转**到 ctx 取消
	// (isComplete 永假 + 无 Ready 任务 + stallTicker 空跑)。图层没有 suspended 态,
	// 这里一律判 failed —— 少了"限流后再等等"的耐心, 换掉了一个挂死路径。
	return graph.NodeResult{Status: graph.NodeStatusFailed, Output: out, Err: err.Error()}
}

// taskID 复刻旧 orchestrator.Task 的 ID 形态 (<团队名>/<阶段名>)。
// 只用于提示词文本与级联错误文案 —— 那两处旧路径都印的是任务 ID。
func (r *orchNodeRunner) taskID(stage string) string {
	if r.team == nil || r.team.Name == "" {
		return stage
	}
	return r.team.Name + "/" + stage
}

// missingDep 返回第一个"声明了但没有产出"的上游 (全部就绪时返回空串)。
func (r *orchNodeRunner) missingDep(nodeID string, prev map[string]string) string {
	for _, dep := range r.deps[nodeID] {
		if _, ok := prev[dep]; !ok {
			return dep
		}
	}
	return ""
}

// attempt 一次尝试 (裸 completion 或整轮对抗循环), 带单次超时。
func (r *orchNodeRunner) attempt(ctx context.Context, node graph.NodeSpec, in graph.NodeInput, timeout time.Duration) (string, error) {
	actx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	sys, user := r.resolvePrompts(node, in)
	if r.adversarial[node.ID] {
		return r.runAdversarial(actx, node, sys, user)
	}
	return r.complete(actx, sys, user)
}

// complete 一次裸 LLM completion (照抄 LLMRunner.Execute 的调用形态: 无工具、无角色合并)。
func (r *orchNodeRunner) complete(ctx context.Context, sys, user string) (string, error) {
	if r.we == nil || r.we.llm == nil {
		return "", fmt.Errorf("orchestrated: LLMClient 未注入")
	}
	return r.we.llm.SimpleComplete(ctx, sys, user)
}

// resolvePrompts 组装系统/用户提示词 (照抄 LLMRunner.Execute + resolveTemplate)。
//
// 注意替换顺序: 先 {objective} 再 {prev_result} 再 {dep:X} —— 与旧实现逐字相同。
// 顺序有观测意义: objective 文本里若含 "{prev_result}" 会被后一步继续替换。
func (r *orchNodeRunner) resolvePrompts(node graph.NodeSpec, in graph.NodeInput) (string, string) {
	sys := node.Agent.Prompt
	// system prompt 刻意不做占位替换 (见文件头第 1 条)。
	user := strings.ReplaceAll(orchUserPromptTemplate, "{objective}", r.objective)

	deps := r.deps[node.ID]
	if strings.Contains(user, "{prev_result}") {
		var parts []string
		for _, dep := range deps {
			if v, ok := in.PrevOutputs[dep]; ok {
				// 标签必须是 <团队名>/<阶段名>: 旧路径的任务 ID 就长这样
				// (buildGraph: taskID = team.Name + "/" + stage.Name), 而 LLMRunner 拼
				// 依赖块用的是任务 ID。图层的节点 ID 是**纯阶段名** (journal 归因、
				// StageResult.Name、覆盖表都按阶段名走, 加前缀会连带改掉那些),
				// 所以前缀只在这一处提示词文本里补回来。
				// 少了它就是每个下游阶段的提示词都变了一个字符串 —— 模型看到的上下文
				// 不同, 等价性不成立 (这条是真跑旧路径比出来的, 不是推理出来的)。
				parts = append(parts, fmt.Sprintf("[%s]\n%v", r.taskID(dep), v))
			}
		}
		user = strings.ReplaceAll(user, "{prev_result}", strings.Join(parts, "\n\n"))
	}
	for _, dep := range deps {
		// 同理, {dep:X} 里的 X 也是旧任务 ID。实践上无人可写 (团队名是运行期生成的),
		// 但照抄比"顺手改成阶段名"安全: 改了就等于悄悄新增一种占位符写法。
		ph := fmt.Sprintf("{dep:%s}", r.taskID(dep))
		if !strings.Contains(user, ph) {
			continue
		}
		if v, ok := in.PrevOutputs[dep]; ok {
			user = strings.ReplaceAll(user, ph, v)
		}
	}

	if sys == "" {
		sys = orchDefaultSystemPrompt
	}
	if user == "" {
		user = r.objective
	}
	return sys, user
}

// runAdversarial 复刻 orchestrator.AdversarialRunner.Execute。
//
// 每轮: generator 一次 completion → 每个 reviewer 一次 completion (串行) →
// 用 generator 产出打分判终止。reviewer 失败**不致命** (旧实现把错误文本拼进
// 反馈里继续跑), generator 失败则整轮失败并把错误交给外层重试。
//
// 提示词每轮都一样 —— generator 与 reviewer 共用同一个 task.Config, 而反馈从未
// 进入提示词 (文件头第 2 条)。所以这里不给 reviewer 造不同的提示词: 造了就是
// 行为变更, 不是等价迁移。
func (r *orchNodeRunner) runAdversarial(ctx context.Context, node graph.NodeSpec, sys, user string) (string, error) {
	// 复刻 RunnerPool.SetLimit("llm-adversarial", 1)。
	select {
	case r.advSem <- struct{}{}:
		defer func() { <-r.advSem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	var lastOutput string
	var history []string
	for round := 1; round <= orchAdversarialRounds; round++ {
		if ctx.Err() != nil {
			return lastOutput, ctx.Err()
		}
		genOut, err := r.complete(ctx, sys, user)
		if err != nil {
			return lastOutput, fmt.Errorf("generator 第 %d 轮失败: %w", round, err)
		}
		lastOutput = genOut

		for i := 0; i < r.reviewers; i++ {
			if _, rErr := r.complete(ctx, sys, user); rErr != nil {
				logging.Event(ctx, "orchestrated.adversarial.review_failed",
					"node", node.ID, "round", fmt.Sprintf("%d", round),
					"reviewer", fmt.Sprintf("adv-reviewer-%d", i), "err", rErr.Error())
			}
		}

		history = append(history, lastOutput)
		if r.policy != nil && r.policy.ShouldTerminate(lastOutput) {
			logging.Event(ctx, "orchestrated.adversarial.terminate",
				"node", node.ID, "round", fmt.Sprintf("%d", round), "rounds_run", fmt.Sprintf("%d", len(history)))
			break
		}
	}
	return lastOutput, nil
}

// ---------------------------------------------------------------------------
// 质量评分与自适应终止 (照搬 orchestrator/llm_runner.go 的 JSONQualityScorer +
// QualityTermination, 逐条对齐三个终止条件)
// ---------------------------------------------------------------------------

// orchQualityScore 一次评审的结构化评分 (JSON 字段名与旧实现一致, 否则解析结果不同)。
type orchQualityScore struct {
	Pass         bool    `json:"pass"`
	Overall      float64 `json:"overall"`
	Completeness float64 `json:"completeness"`
	Correctness  float64 `json:"correctness"`
	Feedback     string  `json:"feedback"`
}

// scoreOrchOutput 从产出里提取评分: 先试 JSON, 否则关键词启发式。
//
// 关键词分支的**判定顺序**必须保留: 先扫"失败词"再扫"通过词", 于是同时含两类词时
// 通过词胜出 (Pass=true, Overall=8)。反过来写会让含 "failed" 又含 "通过" 的产出
// 从"过线即终止"变成"跑满 3 轮", 是纯粹的 token 差异。
func scoreOrchOutput(output string) orchQualityScore {
	text := output

	var score orchQualityScore
	if json.Unmarshal([]byte(text), &score) == nil && score.Overall > 0 {
		return score
	}

	score = orchQualityScore{Overall: 6.0, Completeness: 6.0, Correctness: 6.0, Pass: true}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "blocker") || strings.Contains(lower, "critical") ||
		strings.Contains(lower, "不通过") || strings.Contains(lower, "failed") {
		score.Pass = false
		score.Overall = 4.0
	}
	if strings.Contains(lower, "passed") || strings.Contains(lower, "通过") ||
		strings.Contains(lower, "合格") {
		score.Pass = true
		score.Overall = 8.0
	}
	score.Feedback = text
	return score
}

// orchQualityTermination 三条终止条件: 达标 / 收敛 / 退化 (照抄 QualityTermination)。
type orchQualityTermination struct {
	passThreshold float64
	convergeDelta float64

	mu     sync.Mutex
	scores []orchQualityScore
}

func newOrchQualityTermination(passThreshold, convergeDelta float64) *orchQualityTermination {
	return &orchQualityTermination{passThreshold: passThreshold, convergeDelta: convergeDelta}
}

// ShouldTerminate 判断是否应结束对抗循环。
// 旧签名带 (iteration, lastOutput, history) 三个参数但实现里只用 lastOutput ——
// 这里收窄成一个参数, 免得留两个恒被忽略的形参让人以为轮次会影响判定。
func (t *orchQualityTermination) ShouldTerminate(lastOutput string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	score := scoreOrchOutput(lastOutput)
	t.scores = append(t.scores, score)

	// 条件 1: 质量达标
	if score.Pass && score.Overall >= t.passThreshold {
		return true
	}
	n := len(t.scores)
	if n < 3 {
		return false
	}
	// 条件 2: 收敛 (最近 3 轮变化都小于 delta)
	d1 := math.Abs(t.scores[n-1].Overall - t.scores[n-2].Overall)
	d2 := math.Abs(t.scores[n-2].Overall - t.scores[n-3].Overall)
	if d1 < t.convergeDelta && d2 < t.convergeDelta {
		return true
	}
	// 条件 3: 退化 (连续 2 轮下降)
	return t.scores[n-1].Overall < t.scores[n-2].Overall &&
		t.scores[n-2].Overall < t.scores[n-3].Overall
}

// ---------------------------------------------------------------------------
// 三级错误分治重试 (照搬 orchestrator/errors.go)
// ---------------------------------------------------------------------------
//
// 为什么重试放在 runner 里而不是用 pkg/graph 的 NodeSpec.Retry:
// 图层的重试是**扁平**的 (一个 MaxRetries + 固定指数退避), 而旧 orchestrated 的额度
// 按错误类型分档 —— 致命 0 次 / 永久 2 次 / 瞬态 7 次, 退避基数也不同 (2s vs 10s,
// 全抖动)。用扁平额度顶替只能二选一: 取 2 则 429 限流的耐心从 8 次尝试掉到 3 次,
// 取 7 则代码缺陷类失败白烧 8 次 token。所以分档判定必须留在这一层,
// 图层的 NodeSpec.Retry 相应设为 0 (见 workflow_orchestrated.go), 避免两层相乘。

// orchErrorKind 错误三分类。
type orchErrorKind int

const (
	orchErrTransient orchErrorKind = iota // 429/网络/超时 — 值得等待重试
	orchErrPermanent                      // 验证失败/逻辑缺陷 — 重试意义有限
	orchErrFatal                          // API Key 无效/配额耗尽 — 永不重试
)

func (k orchErrorKind) String() string {
	switch k {
	case orchErrTransient:
		return "transient"
	case orchErrPermanent:
		return "permanent"
	case orchErrFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// 关键词表逐条照抄 orchestrator.ClassifyErrorMsg (匹配优先级: 致命 > 瞬态 > 永久)。
var (
	orchFatalPatterns = []string{
		"invalid api key", "invalid_api_key", "authentication",
		"quota exceeded", "billing", "unauthorized", "forbidden",
	}
	orchTransientPatterns = []string{
		"429", "rate", "throttl", "限流", "频率",
		"timeout", "deadline exceeded", "超时",
		"connection refused", "connection reset", "网络错误",
		"503", "529", "overloaded", "过载",
		"temporary", "unavailable", "econnreset",
		"broken pipe", "eof",
	}
)

// classifyOrchError 从错误消息分类 (空消息按永久处理, 与旧实现一致)。
func classifyOrchError(msg string) orchErrorKind {
	if msg == "" {
		return orchErrPermanent
	}
	lower := strings.ToLower(msg)
	for _, p := range orchFatalPatterns {
		if strings.Contains(lower, p) {
			return orchErrFatal
		}
	}
	for _, p := range orchTransientPatterns {
		if strings.Contains(lower, p) {
			return orchErrTransient
		}
	}
	return orchErrPermanent
}

// orchRetryPolicy 重试额度与退避 (字段与 orchestrator.RetryPolicy 一一对应)。
type orchRetryPolicy struct {
	MaxRetries    int
	MaxTransient  int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	TransientBase time.Duration
}

// orchDefaultRetryPolicy 与 orchestrator.DefaultRetryPolicy() 逐值相同。
//
// 注意这是**引擎级**默认值而不是 Task 上的 MaxRetries/MaxTransient: 旧
// handleFailure 用的是 e.config.RetryPolicy, buildGraph 给每个 Task 填的
// MaxRetries:1 / MaxTransient:2 **从未被读取** (死字段)。照抄真实生效的那份。
func orchDefaultRetryPolicy() orchRetryPolicy {
	return orchRetryPolicy{
		MaxRetries:    2,
		MaxTransient:  5,
		BaseDelay:     2 * time.Second,
		MaxDelay:      2 * time.Minute,
		TransientBase: 10 * time.Second,
	}
}

// orchMaxAttempts 一个节点最多的尝试次数 (供节点总预算推导, 见 workflow_orchestrated.go)。
func (p orchRetryPolicy) orchMaxAttempts() int { return 1 + p.MaxRetries + p.MaxTransient }

// orchRetryDecision 重试决策。
type orchRetryDecision struct {
	ShouldRetry bool
	Delay       time.Duration
	Kind        orchErrorKind
}

// ShouldRetry 照抄 orchestrator.RetryPolicy.ShouldRetry 的决策树。
func (p orchRetryPolicy) ShouldRetry(errMsg string, attempt int) orchRetryDecision {
	kind := classifyOrchError(errMsg)
	switch kind {
	case orchErrFatal:
		return orchRetryDecision{Kind: kind}
	case orchErrTransient:
		if attempt < p.MaxRetries+p.MaxTransient {
			return orchRetryDecision{ShouldRetry: true, Kind: kind,
				Delay: orchBackoffWithJitter(p.TransientBase, attempt, p.MaxDelay)}
		}
		return orchRetryDecision{Kind: kind}
	default: // orchErrPermanent
		if attempt < p.MaxRetries {
			return orchRetryDecision{ShouldRetry: true, Kind: kind,
				Delay: orchBackoffWithJitter(p.BaseDelay, attempt, p.MaxDelay)}
		}
		return orchRetryDecision{Kind: kind}
	}
}

// orchBackoffWithJitter Full Jitter 指数退避 (照抄, 含"抖动后可能接近 0"这一特性)。
func orchBackoffWithJitter(base time.Duration, attempt int, maxDelay time.Duration) time.Duration {
	delay := time.Duration(float64(base) * math.Pow(2, float64(attempt)))
	if delay > maxDelay {
		delay = maxDelay
	}
	return time.Duration(rand.Float64() * float64(delay))
}

// orchSleepFn 重试退避的测试注入点 (与 pkg/graph 的 Engine.sleepFn 同一手法)。
// 生产恒为 orchSleep —— 瞬态退避总时长可达数分钟, 测试里真睡会让门禁不可用。
var orchSleepFn = orchSleep

// orchSleep ctx 感知休眠; 返回 false 表示 ctx 已取消 (调用方应停止重试)。
//
// **这条退避在旧路径上从未生效**, 而它是同一条 bug 链的起点:
// ① Engine.handleFailure 拿到 decision.Delay 后把任务立刻置 TaskReady, 再开一个
// goroutine `time.Sleep(delay)` 然后**什么都不做** (函数体只有一个空 if);
// ② 唯一能拦住"冷却期内别调度"的 CooldownFilter 根本没注册进 NewScheduler
// (而且它自己也只看 key 存在不看时间戳)。
// 于是瞬态重试在不到一秒内烧完 8 次尝试、8 次撞同一个 429 → 额度耗尽 → Suspended →
// 停滞恢复 3 次也救不回来 → 引擎主循环永久空转 (isComplete 永假 + 无 Ready 任务)。
// 所以这里真退避不是"顺手改行为", 是把那条链掐断。
//
// 刻意不用 "if !t.Stop() { <-t.C }" 那个 drain 惯用法: Go 1.23 起被 Stop 过的定时器
// channel 永不再送值, 那行会永久阻塞 —— pkg/orchestrator 的预存死锁就是这个形态。
func orchSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
