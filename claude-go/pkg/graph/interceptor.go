package graph

// interceptor.go —— 节点执行切面链 (design/01 §4.10 全局注入)。
//
// 设计文档把这一项称作 G5 的核心、也是"便于全局注入预算管理"这一原始诉求的落点。
// 挂载点只有一个: engine.callRunner —— 它自称"真正调 runner 的唯一出口", 实测确实
// 是 (agent/gate 的重试与 loop 环、map 分片、loop-group 组内节点全部经它)。切面挂
// 在这一处就自动覆盖全部 Kind 与全部重试/循环轮次, 不用在四条执行路径上各挂一遍。
//
// ## 为什么切面在这一层, 而拦截器实现不在
//
// pkg/graph 是纯调度内核, 不认识 agent 语义 (不 import pkg/agent, 防循环依赖)。
// 所以这里只定义**挂载点与链语义**; BudgetManager 之外的标准拦截器 (EvolutionRecorder /
// GateEnforcer / Notifier / MetricsEmitter) 需要 team/黑板/飞书语义, 实现属 pkg/agent。
// BudgetManager 例外地放在本包: 它只需要 token/时长/次数这些内核自己就有的量, 且
// 超预算要能**中止调度**——那是内核职责。
//
// ## 链语义 (刻意定成这样)
//
//   - **顺序确定**: 按注册顺序 [0] 最外层。设计文档要求"顺序确定、可开关", 因为
//     BudgetManager 必须在 EvolutionRecorder 外层——否则超预算被拒的节点也会被记一
//     条轨迹, 污染 design/03 的学习数据。
//   - **next 必须恰好调一次**: 不调 = 节点被拦截器吞掉 (引擎会当它没跑完); 调多次 =
//     重试语义被绕过 (引擎的 retry 计数看不到)。链本身**检测并拒绝**这两种误用, 见
//     errNextNotCalled/errNextTwice —— 拦截器是第三方注入点 (设计文档明说 aiops 平台
//     的权限桥要从此注入), 静默容忍误用会让排查无从下手。
//   - **panic 不穿透**: 一个第三方拦截器 panic 不该带走整个图运行。捕获后转为节点
//     failed, 因为"拦截器崩了"属于必须暴露的失败, 不是可以 fail-open 放行的情况。
//   - **拦截器返回错误 = 节点 failed**, 且错误原文进 NodeResult.Err。
//
// ## fail-open 还是 fail-closed
//
// 本仓原则是"fail-closed 的治理, fail-open 的交付"。拦截器链承载的是治理
// (预算/门禁/权限), 所以这里一律 fail-closed: 拦截器出错就拒绝执行, 绝不放行。
// 与之对比 pkg/agent/blackboard.go 的 Watch 是交付路径, 无人消费时丢弃而不阻塞。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// NodeExec 链中的"下一跳"。拦截器必须恰好调用一次。
type NodeExec func(context.Context, NodeSpec, NodeInput) NodeResult

// NodeInterceptor 节点执行切面 (design/01 §4.10)。
//
// Around 拿到 next 后**必须恰好调用一次**并返回其结果 (可修饰)。要拒绝执行就
// 不调 next 而是直接返回一个 Status=failed/skipped 的 NodeResult —— 那是明示
// 拒绝, 与"忘了调 next"可区分。
type NodeInterceptor interface {
	// Name 用于 journal 归因与开关配置, 必须稳定且唯一。
	Name() string
	Around(ctx context.Context, node NodeSpec, in NodeInput, next NodeExec) NodeResult
}

// 链误用的两种情形。拦截器是第三方注入点, 这两种错必须响而不是静默。
var (
	errNextNotCalled = errors.New("graph: 拦截器未调用 next (节点被吞掉)")
	errNextTwice     = errors.New("graph: 拦截器重复调用 next (绕过引擎重试计数)")
)

// interceptorChain 把 [i0, i1, ..., in] 与终点 exec 组装成 i0(i1(...(exec)))。
//
// 每层都包一个"恰好一次"计数器。计数器是 per-调用 的局部变量而不是结构体字段:
// 同一条链会被多个节点 goroutine 并发使用, 字段会互相踩。
func chainNode(ics []NodeInterceptor, exec NodeExec) NodeExec {
	for i := len(ics) - 1; i >= 0; i-- {
		ic, next := ics[i], exec
		exec = func(ctx context.Context, node NodeSpec, in NodeInput) (res NodeResult) {
			calls := 0
			guarded := func(c context.Context, n NodeSpec, i2 NodeInput) NodeResult {
				calls++
				if calls > 1 {
					// 不能直接 panic 出去: 上层 recover 会把它算成拦截器崩溃,
					// 归因错人。记在结果里, 由外层判定。
					return NodeResult{Status: NodeStatusFailed, Err: fmt.Sprintf("%v: %s", errNextTwice, ic.Name())}
				}
				return next(c, n, i2)
			}
			defer func() {
				if r := recover(); r != nil {
					res = NodeResult{
						Status: NodeStatusFailed,
						Err:    fmt.Sprintf("graph: 拦截器 %s panic: %v", ic.Name(), r),
					}
				}
			}()
			res = ic.Around(ctx, node, in, guarded)
			if calls == 0 && res.Status == "" {
				// 既没调 next 又没给出终态 —— 典型的"忘了调 next"。
				// 若拦截器明示了终态 (failed/skipped), 那是正当拒绝, 不报错。
				res = NodeResult{Status: NodeStatusFailed, Err: fmt.Sprintf("%v: %s", errNextNotCalled, ic.Name())}
			}
			return res
		}
	}
	return exec
}

// journalAware 想往 journal 记账的拦截器实现它, Engine.Run 会在开跑前注入记账函数。
//
// 为什么要这个接口: evAppender 是未导出类型, 外部包 (pkg/agent 装配链的地方) **没法
// 自己造一个传进来** —— 若靠构造参数传, 生产装出来的拦截器永远拿不到记账函数, 于是
// budget.consumed/budget.exceeded 一条都不会落盘。让引擎注入才能真记上。
type journalAware interface {
	attachJournal(appendEv evAppender)
}

// —— BudgetManager (design/01 §4.10 表中第一项, 用户点名的全局注入示例) ——

// Budget 图级/节点级预算。零值 = 不限制 (缺省不改变现状行为)。
//
// 单位刻意选"节点执行次数 / 墙钟时长 / 估算 token", 因为这三样内核自己就能算,
// 不必依赖 runner 回报。真实 token 记账在 pkg/api 的 LLMCallRecord 里, 精确值经
// CallInterceptor 汇入 (见 §4.10 CallInterceptor 部分, 属 pkg/agent 侧接线)。
type Budget struct {
	// MaxNodeRuns 整图允许的节点执行次数上限 (含重试与 loop 每一轮)。
	// 这是最有用的一道闸: 无界重试/循环烧钱的历史故障全都表现为它爆掉。
	MaxNodeRuns int `json:"max_node_runs,omitempty"`
	// MaxWallClock 整图墙钟预算。与节点级 TimeoutSec 是两回事:
	// 后者管单节点卡死, 前者管"每个节点都不超时但总共跑了六小时"。
	MaxWallClock time.Duration `json:"max_wall_clock,omitempty"`
	// MaxTokens 整图估算 token 预算 (由 runner 经 NodeResult 回报的用量累加;
	// runner 不报则此项不生效 —— 不猜, 见 Observe)。
	MaxTokens int64 `json:"max_tokens,omitempty"`
	// PerNodeMaxRuns 单节点执行次数上限 (含重试与 loop)。0 = 不限。
	PerNodeMaxRuns int `json:"per_node_max_runs,omitempty"`

	// OnExceed 超预算动作: "fail" (默认, 拒绝后续节点) | "skip" (置 skipped 继续跑完余图)。
	// 刻意不提供 "ignore": 那等于没有预算。降级模型属 pkg/agent 侧策略, 内核不做。
	OnExceed string `json:"on_exceed,omitempty"`
}

// budgetExceeded 预算超限的判定结果。
type budgetExceeded struct {
	kind  string // node_runs|wall_clock|tokens|per_node_runs
	limit string
	got   string
}

func (b budgetExceeded) String() string {
	return fmt.Sprintf("超出图预算 %s: 上限 %s, 实际 %s", b.kind, b.limit, b.got)
}

// BudgetManager 预算台账拦截器。并发安全 (引擎从多个节点 goroutine 调用)。
//
// 记账与判定分离: 判定发生在**执行前**(拒绝还没花的钱), 记账发生在执行后。
// 这个顺序是必须的 —— 执行后才判定意味着预算总会被超出至少一个节点的开销。
type BudgetManager struct {
	budget  Budget
	started time.Time

	mu       sync.Mutex
	// appendEv 记 budget.consumed/exceeded 事件; nil = 不记账 (判定照常执行)。
	// 由 Engine.Run 经 attachJournal 注入 (见 journalAware), 受 mu 保护:
	// Engine 可被复用跑多张图, 每次 Run 都会重新注入。
	appendEv evAppender
	nodeRuns int
	perNode  map[string]int
	tokens   int64
	exceeded *budgetExceeded // 一旦超限就钉住第一次的原因 (后续节点同因拒绝)
}

// NewBudgetManager 构造预算拦截器。started 为图运行起点 (墙钟预算的基准);
// 零值 = 以 Run 开跑时刻为基准。journal 记账函数由 Engine.Run 自动注入。
func NewBudgetManager(b Budget, started time.Time) *BudgetManager {
	return &BudgetManager{budget: b, started: started, perNode: map[string]int{}}
}

// attachJournal 见 journalAware。
func (m *BudgetManager) attachJournal(appendEv evAppender) {
	m.mu.Lock()
	m.appendEv = appendEv
	if m.started.IsZero() {
		m.started = time.Now() // 墙钟基准: 未显式给就以本次 Run 开跑为起点
	}
	m.mu.Unlock()
}

// journal 取记账函数 (读需持锁: Engine 复用时会重新注入)。
func (m *BudgetManager) journal() evAppender {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendEv
}

// Name 见 NodeInterceptor。
func (m *BudgetManager) Name() string { return "budget" }

// Around 见 NodeInterceptor。
func (m *BudgetManager) Around(ctx context.Context, node NodeSpec, in NodeInput, next NodeExec) NodeResult {
	if ex := m.check(node.ID); ex != nil {
		status := NodeStatusFailed
		if strings.EqualFold(m.budget.OnExceed, "skip") {
			status = NodeStatusSkipped
		}
		if ap := m.journal(); ap != nil {
			ap(EvBudgetExceeded, node.ID, map[string]any{
				"kind": ex.kind, "limit": ex.limit, "got": ex.got, "action": status,
			})
		}
		// 明示终态 = 正当拒绝, 链不会把它当"忘了调 next"。
		return NodeResult{Status: status, Err: ex.String()}
	}

	res := next(ctx, node, in)
	m.observe(node.ID, res)
	return res
}

// check 执行前判定。返回非 nil 表示应拒绝。
func (m *BudgetManager) check(nodeID string) *budgetExceeded {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exceeded != nil {
		return m.exceeded // 已超限: 后续一律同因拒绝, 不再重新判定
	}
	b := m.budget
	if b.MaxNodeRuns > 0 && m.nodeRuns >= b.MaxNodeRuns {
		m.exceeded = &budgetExceeded{"node_runs", fmt.Sprint(b.MaxNodeRuns), fmt.Sprint(m.nodeRuns)}
		return m.exceeded
	}
	if b.PerNodeMaxRuns > 0 && m.perNode[nodeID] >= b.PerNodeMaxRuns {
		// per-node 超限**不钉住全局** exceeded: 别的节点还该继续跑。
		return &budgetExceeded{"per_node_runs", fmt.Sprint(b.PerNodeMaxRuns), fmt.Sprint(m.perNode[nodeID])}
	}
	if b.MaxWallClock > 0 {
		if el := time.Since(m.started); el >= b.MaxWallClock {
			m.exceeded = &budgetExceeded{"wall_clock", b.MaxWallClock.String(), el.Truncate(time.Millisecond).String()}
			return m.exceeded
		}
	}
	if b.MaxTokens > 0 && m.tokens >= b.MaxTokens {
		m.exceeded = &budgetExceeded{"tokens", fmt.Sprint(b.MaxTokens), fmt.Sprint(m.tokens)}
		return m.exceeded
	}
	return nil
}

// observe 执行后记账。
func (m *BudgetManager) observe(nodeID string, res NodeResult) {
	m.mu.Lock()
	m.nodeRuns++
	m.perNode[nodeID]++
	m.tokens += res.Tokens
	runs, tokens, total := m.perNode[nodeID], m.tokens, m.nodeRuns
	m.mu.Unlock()

	if ap := m.journal(); ap != nil {
		d := map[string]any{"node_runs": total, "node_runs_this": runs}
		// tokens 只在 runner 真回报时入账 —— 报 0 会让"未实现用量回报"看起来
		// 像"这次没花 token", 两者必须可区分。
		if res.Tokens > 0 {
			d["tokens"] = res.Tokens
			d["tokens_total"] = tokens
		}
		ap(EvBudgetConsumed, nodeID, d)
	}
}

// Spent 返回当前台账 (供调用方在 Run 返回后读取, 或测试断言)。
func (m *BudgetManager) Spent() (nodeRuns int, tokens int64, elapsed time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nodeRuns, m.tokens, time.Since(m.started)
}

// Exceeded 返回是否已因图级预算超限而停止放行 (per-node 超限不计入)。
func (m *BudgetManager) Exceeded() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exceeded == nil {
		return false, ""
	}
	return true, m.exceeded.String()
}

// —— 观测型拦截器 ——

// FuncInterceptor 用函数构造拦截器 (测试与轻量注入用)。
type FuncInterceptor struct {
	N  string
	Fn func(ctx context.Context, node NodeSpec, in NodeInput, next NodeExec) NodeResult
}

// Name 见 NodeInterceptor。
func (f FuncInterceptor) Name() string {
	if f.N == "" {
		return "func"
	}
	return f.N
}

// Around 见 NodeInterceptor。
func (f FuncInterceptor) Around(ctx context.Context, node NodeSpec, in NodeInput, next NodeExec) NodeResult {
	if f.Fn == nil {
		return next(ctx, node, in)
	}
	return f.Fn(ctx, node, in, next)
}

// interceptorNames 供 journal 记录链构成。**按注册顺序**而非字母序 ——
// 顺序即语义 (见文件头), 排过序的名单会让"当时链是怎么装的"不可考。
func interceptorNames(ics []NodeInterceptor) []string {
	out := make([]string, 0, len(ics))
	for _, ic := range ics {
		out = append(out, ic.Name())
	}
	return out
}

// validateInterceptors 拒绝重名 (Name 用于 journal 归因与开关, 重名会让归因失真)。
func validateInterceptors(ics []NodeInterceptor) error {
	seen := map[string]bool{}
	for _, ic := range ics {
		if ic == nil {
			return errors.New("graph: 拦截器为 nil")
		}
		n := ic.Name()
		if strings.TrimSpace(n) == "" {
			return errors.New("graph: 拦截器 Name 为空")
		}
		if seen[n] {
			return fmt.Errorf("graph: 拦截器重名 %q", n)
		}
		seen[n] = true
	}
	return nil
}
