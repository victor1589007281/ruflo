package agent

// graph_interceptors.go —— 图执行拦截器的生产装配 (design/01 §4.10)。
//
// pkg/graph 定义了切面挂载点与链语义 (interceptor.go), 但内核不认识 agent 语义,
// 所以"装哪些拦截器、参数从哪来"在这一层决定。此前生产恒空链 —— 切面建成未通电,
// 本文件就是那条线。
//
// ## 为什么默认预算是"从图算出来的真上界"而不是"不限制"
//
// 默认不限制等于拦截器挂了但不起作用 (本仓反复吃过的"建成未通电")。默认限死一个
// 拍脑袋的数又会改变现状行为 —— 有 8+ 个下游平台在用 :18080, 那是生产事故。
//
// 折中: 从 GraphSpec **算出**一次合法运行的节点执行次数上界 (节点数 × 重试 ×
// 循环轮次, 动态展开/扇出按 MaxTotalNodes 上界算), 再乘一个宽裕系数。合法运行
// 达不到它, 失控运行会撞上它。所以它不改变任何正常行为, 只是给"无界重试/循环
// 烧钱"这类历史故障加一道兜底闸。真要设紧预算就用下面的环境变量。
//
// ## 环境变量 (都缺省 = 走算出来的上界)
//
//	CLAUDE_GO_GRAPH_BUDGET_NODE_RUNS   整图节点执行次数上限
//	CLAUDE_GO_GRAPH_BUDGET_WALLCLOCK   整图墙钟预算 (Go duration, 如 "45m")
//	CLAUDE_GO_GRAPH_BUDGET_TOKENS      整图 token 预算 (需 runner 回报用量才生效)
//	CLAUDE_GO_GRAPH_BUDGET_ON_EXCEED   超限动作: fail (默认) | skip
//	CLAUDE_GO_GRAPH_INTERCEPTORS       链开关: 逗号分隔的拦截器名; "off"=空链
//	CLAUDE_GO_GRAPH_AIMD_MIN           ratelimit 拦截器的并发下限 (默认 1)
//
// ## 默认开的是哪些, 为什么不是全部
//
// 开关未设时**只开 budget** —— 它有等价性证明 (算出来的上界合法运行撞不到)。
// ratelimit / tokens 必须在开关里**显式点名**才生效, 因为它们会改变可观测行为:
//   - ratelimit 改节点准入时序 (失败后压并发), 那是执行形态的变化;
//   - tokens 让 BudgetManager 的 MaxTokens 闸从"恒不生效"变成"真的会拦",
//     并给 budget.consumed 事件加上 token 字段。
//
// 这与"默认不改现状行为"的硬约束一致: 想要它们就写
// CLAUDE_GO_GRAPH_INTERCEPTORS=budget,ratelimit,tokens。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/trace"
)

// budgetSlack 算出来的上界乘的宽裕系数。
// 2 倍是刻意的: 上界推导已按各闸的最大值取, 再翻一倍确保合法运行绝不会撞闸 ——
// 这道闸的定位是"兜住失控", 不是"精确控成本"。要精确控就用环境变量。
const budgetSlack = 2

// graphInterceptors 装配一次图运行的拦截器链。
//
// 顺序即语义 (见 pkg/graph/interceptor.go), 这条链的顺序表:
//
//	[0] budget     预算闸。必须最外层 —— 被预算拒掉的节点不该被内层记账/记轨迹/占并发额。
//	[1] ratelimit  AIMD 背压。在 budget 之内: 已经被预算拒掉的节点不该去排并发队列
//	               (排到了也不会执行, 白占位置还拖慢别人)。
//	[2] tokens     真实 token 回报。必须最内层 —— 它量的是"这个节点实际烧了多少",
//	               而 budget 要在**下一个**节点准入时读到这个量, 所以它得在 budget 里面。
func graphInterceptors(we *WorkflowExecutor, team *ProductionTeam, spec graph.GraphSpec) []graph.NodeInterceptor {
	enabled := parseInterceptorSwitch(os.Getenv("CLAUDE_GO_GRAPH_INTERCEPTORS"))
	if enabled != nil && len(enabled) == 0 {
		return nil // 显式 off
	}

	var out []graph.NodeInterceptor
	if enabled == nil || enabled["budget"] {
		b := budgetFromEnv(spec)
		out = append(out, graph.NewBudgetManager(b, time.Now()))
	}
	// ratelimit / tokens 必须显式点名 (见文件头"默认开的是哪些")。
	if enabled != nil && enabled["ratelimit"] {
		out = append(out, newGraphRateLimiter(effectiveGraphParallel(we, spec), aimdMinFromEnv(), team))
	}
	if enabled != nil && enabled["tokens"] {
		// 台账在 pkg/api 侧收数, 这里只把它挂上 (幂等注册: 每次运行都调也不会叠加)。
		_ = api.RegisterCallInterceptor(api.DefaultTokenLedger)
		out = append(out, &graphTokenReporter{ledger: api.DefaultTokenLedger})
	}
	return out
}

// effectiveGraphParallel 本次图运行的并发上限 (与 runGraphSpec 装 Engine 时同一口径)。
// 图声明的 Policies.MaxParallel 优先, 否则 executor 的动态建议值。
func effectiveGraphParallel(we *WorkflowExecutor, spec graph.GraphSpec) int {
	if spec.Policies.MaxParallel > 0 {
		return spec.Policies.MaxParallel
	}
	if we != nil {
		if p := we.effectiveParallel(); p > 0 {
			return p
		}
	}
	return 1
}

// aimdMinFromEnv AIMD 并发下限。下限必须 ≥1 —— 降到 0 就是把整图卡死, 而 AIMD 的
// 目的是"慢下来", 不是"停下来"。
func aimdMinFromEnv() int {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_AIMD_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return 1
}

// ---------------------------------------------------------------------------
// ratelimit: 图层 AIMD 背压 (design/01 §4.10 表最后一行)
// ---------------------------------------------------------------------------

// graphAIMDRecoveryStreak 连续成功多少次才加 1 并发 (加性增)。
//
// 取 5 而不是 api.RateLimitGuard 的 10: 图层一次运行总共可能只有十几次节点执行,
// 用 10 会让并发在一次运行内几乎不可能恢复 —— 那就退化成"只会降不会升"的单向闸。
const graphAIMDRecoveryStreak = 5

// graphRateLimiter 图层 AIMD (加性增 / 乘性减) 并发背压节点拦截器。
//
// ## 它填的是哪个洞
//
// design/01 M4 退役 pkg/orchestrator 时, 那套引擎的 AIMD "失败后并发折半" **没有等价
// 物** (workflow_orchestrated.go 文件头把这件事记了账)。注意不要与 api.RateLimitGuard
// 混淆: 后者管的是"同时在飞几个 HTTP 请求", 一直都在; 这里管的是"同时在跑几个节点"
// —— 一个节点可以发很多次 LLM 调用 (工具循环), 所以压住请求数并不等于压住节点数。
// 节点级并发过高的真实代价是: 每个节点都在重试等 429, 墙钟被烧光而进度为零。
//
// ## 为什么用"票池"而不是 sync.Cond
//
// 票池 (带缓冲 channel) 能让等待方 select ctx.Done() ——AIMD 压到 1 并发时, 被取消的
// 团队必须能立刻退出, 而 sync.Cond.Wait 没法被 ctx 唤醒 (api.RateLimitGuard 的
// acquireSem 就是裸 `<-g.sem`, 那是一条真实的挂死路径, 这里刻意不照抄)。
// 容量变化用"吞票/放票"表达: 降并发就从池里捞走多余的票, 升并发就放一张新票进去。
type graphRateLimiter struct {
	sem  chan struct{} // 票池, 容量 = hardMax; 池中票数 = 当前允许并发
	team *ProductionTeam

	mu      sync.Mutex
	limit   int // 当前允许并发 (= 池中票数 + 在飞数)
	hardMax int
	hardMin int
	streak  int // 连续成功次数

	cuts     atomic.Int64
	waitedNs atomic.Int64
}

// newGraphRateLimiter 造图层 AIMD 拦截器。start 为初始允许并发 (通常 = 图并发上限)。
func newGraphRateLimiter(start, min int, team *ProductionTeam) *graphRateLimiter {
	if start < 1 {
		start = 1
	}
	if min < 1 {
		min = 1
	}
	if min > start {
		min = start
	}
	r := &graphRateLimiter{sem: make(chan struct{}, start), team: team,
		limit: start, hardMax: start, hardMin: min}
	for i := 0; i < start; i++ {
		r.sem <- struct{}{}
	}
	return r
}

// Name 见 graph.NodeInterceptor。
func (r *graphRateLimiter) Name() string { return "ratelimit" }

// Around 见 graph.NodeInterceptor。
func (r *graphRateLimiter) Around(ctx context.Context, node graph.NodeSpec, in graph.NodeInput, next graph.NodeExec) graph.NodeResult {
	waitStart := time.Now()
	select {
	case <-r.sem:
	case <-ctx.Done():
		// 明示拒绝 (给终态) —— 链因此不会把它当成"忘了调 next"。
		// 判 failed 而不是 skipped: 被取消不是"这一步不用做", 是"这一步没做成"。
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "ratelimit: 等待并发额度时被取消"}
	}
	r.waitedNs.Add(int64(time.Since(waitStart)))

	res := next(ctx, node, in)

	// 判限流用的是**节点错误原文**: 图层看不到 HTTP 状态码, 而 stage 侧把 429 的原文
	// 一路带进 NodeResult.Err (workflow.go 的重试环也是靠同一个判据认限流的)。
	if res.Status == graph.NodeStatusFailed && isRateLimitErrText(res.Err) {
		r.onThrottled(ctx, node.ID)
	} else if res.Status == graph.NodeStatusCompleted {
		r.onSuccess(ctx)
	} else {
		// 非限流失败: 既不降也不升 (它跟额度无关, 拿它当信号会让 AIMD 追着业务错误抖)。
		r.releaseTicket()
	}
	return res
}

// onThrottled 乘性减: 并发折半 (夹到 hardMin), 并**不归还**本次的票 (少还即降容)。
func (r *graphRateLimiter) onThrottled(ctx context.Context, nodeID string) {
	r.mu.Lock()
	r.streak = 0
	cur := r.limit
	next := cur / 2
	if next < r.hardMin {
		next = r.hardMin
	}
	drop := cur - next // 需要少还/捞走的票数
	if drop > 0 {
		r.limit = next
	}
	newLimit := r.limit
	r.mu.Unlock()

	if drop <= 0 {
		r.releaseTicket() // 已到下限: 正常归还
		return
	}
	r.cuts.Add(1)
	// 本次这张票就是第一张被吞掉的; 还差的从池里捞 (捞不到说明别人在飞, 他们还回来时
	// releaseTicket 会因为 limit 变小而自动吞掉 —— 见那里的注释)。
	for i := 1; i < drop; i++ {
		select {
		case <-r.sem:
		default:
		}
	}
	logging.Event(ctx, "graph.ratelimit.cut", "node", nodeID,
		"limit", fmt.Sprintf("%d", newLimit), "cuts", fmt.Sprintf("%d", r.cuts.Load()))
	if r.team != nil {
		if mc := r.team.metrics(); mc != nil {
			recordTeamRun(mc, "llm", metrics.MLLMGuardAIMDCut, 1, r.team,
				map[string]string{"layer": "graph", "workflow": r.team.Workflow})
			recordTeamRun(mc, "llm", metrics.MLLMGuardMaxParallel, float64(newLimit), r.team,
				map[string]string{"layer": "graph", "workflow": r.team.Workflow})
		}
	}
}

// onSuccess 加性增: 连续 graphAIMDRecoveryStreak 次成功加 1 (不超 hardMax)。
func (r *graphRateLimiter) onSuccess(ctx context.Context) {
	r.releaseTicket()
	r.mu.Lock()
	r.streak++
	grow := r.streak >= graphAIMDRecoveryStreak && r.limit < r.hardMax
	if grow {
		r.streak = 0
		r.limit++
	}
	newLimit := r.limit
	r.mu.Unlock()
	if !grow {
		return
	}
	select {
	case r.sem <- struct{}{}: // 多放一张票 = 容量 +1
	default: // 池满 (不该发生, 池容量 = hardMax): 宁可不加也不阻塞
	}
	logging.Event(ctx, "graph.ratelimit.grow", "limit", fmt.Sprintf("%d", newLimit))
}

// releaseTicket 归还一张票, 但**若当前 limit 已被压低到"在外的票超额"就吞掉它**。
//
// 为什么要在归还时判: 降容时其它节点的票正在飞, 捞不到。把"吞掉"推迟到它们归还时做,
// 降容才是真的 (否则并发会在几个节点陆续完成后自己弹回去, AIMD 形同虚设)。
func (r *graphRateLimiter) releaseTicket() {
	r.mu.Lock()
	// 池中票数 + 本次要还的这张 是否超过 limit
	over := len(r.sem)+1 > r.limit
	r.mu.Unlock()
	if over {
		return // 吞掉
	}
	select {
	case r.sem <- struct{}{}:
	default:
	}
}

// Limit 当前允许并发 (测试与观测用)。
func (r *graphRateLimiter) Limit() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.limit
}

// Cuts 降并发次数。
func (r *graphRateLimiter) Cuts() int64 { return r.cuts.Load() }

// isRateLimitErrText 判错误原文是否为 LLM 限流。
// 与 workflow.go 的 stage 重试环共用同一个判据 —— 两处各写一份必然漂移, 而漂移的
// 后果是"重试环认为在限流、背压环认为没有"。
func isRateLimitErrText(errStr string) bool {
	if errStr == "" {
		return false
	}
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "429") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "限流") ||
		strings.Contains(lower, "throttl")
}

// ---------------------------------------------------------------------------
// tokens: 真实 token 用量回报 (让 BudgetManager 的 MaxTokens 闸真的生效)
// ---------------------------------------------------------------------------

// graphTokenReporter 把 pkg/api 侧台账里的真实用量填进 NodeResult.Tokens。
//
// pkg/graph 的 BudgetManager 有一道 MaxTokens 闸, 但内核只能读 NodeResult.Tokens,
// 而**没有任何 runner 填过它** —— 那道闸至今恒不生效 (interceptor.go 自己写着
// "runner 不报则此项不生效")。真实用量只有 api.Client 知道 (LLMCallRecord.TotalTokens),
// 经 api.TokenLedger 按 (RunID, NodeID) 记账, 这里取**本次节点执行前后的差值**。
//
// 为什么取差值而不是取总量: 同一个节点会被重试/循环多次执行, 每次都该只报本次的量,
// 否则 BudgetManager 累加的是"总量的前缀和", token 会被重复计到爆。
type graphTokenReporter struct {
	ledger *api.TokenLedger
}

// Name 见 graph.NodeInterceptor。
func (t *graphTokenReporter) Name() string { return "tokens" }

// Around 见 graph.NodeInterceptor。
func (t *graphTokenReporter) Around(ctx context.Context, node graph.NodeSpec, in graph.NodeInput, next graph.NodeExec) graph.NodeResult {
	if t.ledger == nil {
		return next(ctx, node, in)
	}
	// 归因键与 executeStage 盖在 ctx 上的那份一致 (trace.IDs{NodeID: stage.Name}),
	// 而图路径的 stage.Name 就是 node.ID —— 两侧对不上就会恒取到 0。
	ids := trace.From(ctx)
	before := t.ledger.Tokens(ids.RunID, node.ID)
	res := next(ctx, node, in)
	if delta := t.ledger.Tokens(ids.RunID, node.ID) - before; delta > 0 && res.Tokens == 0 {
		// 只在 runner 自己没报时填 —— 将来有 runner 真报了, 它的值更准, 不覆盖。
		res.Tokens = delta
	}
	return res
}

// parseInterceptorSwitch 解析链开关。
// 返回 nil = 未设置 (全开); 返回空 map = 显式关闭全部。
func parseInterceptorSwitch(v string) map[string]bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.EqualFold(v, "off") || strings.EqualFold(v, "none") {
		return map[string]bool{}
	}
	set := map[string]bool{}
	for _, name := range strings.Split(v, ",") {
		if n := strings.TrimSpace(name); n != "" {
			set[n] = true
		}
	}
	return set
}

// budgetFromEnv 环境变量优先, 缺省用从 spec 算出的上界。
func budgetFromEnv(spec graph.GraphSpec) graph.Budget {
	b := graph.Budget{
		MaxNodeRuns: nodeRunCeiling(spec),
		OnExceed:    strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_BUDGET_ON_EXCEED")),
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_BUDGET_NODE_RUNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			b.MaxNodeRuns = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_BUDGET_WALLCLOCK")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			b.MaxWallClock = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_BUDGET_TOKENS")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			b.MaxTokens = n
		}
	}
	return b
}

// nodeRunCeiling 算一次**合法**运行最多能有多少次节点执行。
//
// 推导 (每一项都取该闸允许的最大值, 故结果是真上界):
//   - 节点基数: 声明了 map/展开时按 MaxTotalNodes 上界算 (运行图可以长到那么大),
//     否则就是声明的节点数;
//   - 每节点最多 (maxRetries+1) 次尝试 × 最多 maxLoopIters 轮节点级循环;
//   - loop-group 再乘组级最大轮次;
//   - 最后乘 budgetSlack。
//
// 返回 0 表示算不出上界 (不该发生, 保守起见 = 不限制而不是限死)。
func nodeRunCeiling(spec graph.GraphSpec) int {
	base := len(spec.Nodes)
	if base == 0 {
		return 0
	}
	// 动态展开/扇出会让运行图长大, 上界就是总量闸。
	if specHasDynamicGrowth(spec) {
		limit := spec.Policies.MaxTotalNodes
		if limit <= 0 {
			limit = graph.DefaultMaxTotalNodes
		}
		if limit > base {
			base = limit
		}
	}

	retries := 0
	if spec.Policies.DefaultRetry != nil {
		retries = spec.Policies.DefaultRetry.MaxRetries
	}
	loopIters, groupIters := 1, 1
	for _, n := range spec.Nodes {
		if n.Retry != nil && n.Retry.MaxRetries > retries {
			retries = n.Retry.MaxRetries
		}
		if n.Loop != nil && n.Loop.MaxIterations > loopIters {
			loopIters = n.Loop.MaxIterations
		}
		// GroupPolicy.Loop 是值不是指针 (组循环策略必填), 轮次在它里面。
		if n.Group != nil && n.Group.Loop.MaxIterations > groupIters {
			groupIters = n.Group.Loop.MaxIterations
		}
	}

	ceiling := base * (retries + 1) * loopIters * groupIters * budgetSlack
	if ceiling <= 0 { // 溢出保护: 宁可不限制也不要限出一个负数/0 把整图拒死
		return 0
	}
	return ceiling
}

// specHasDynamicGrowth 图是否可能在运行期长大 (map 扇出 / 动态展开 / 组循环)。
func specHasDynamicGrowth(spec graph.GraphSpec) bool {
	for _, n := range spec.Nodes {
		if n.Map != nil || n.Expand != nil || n.Group != nil {
			return true
		}
	}
	return false
}
