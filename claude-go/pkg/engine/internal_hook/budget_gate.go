// budget_gate.go — P0-1 / H0-5: run 级 token 预算闸门。
//
// # 为什么需要它
//
// QueryEngine.CumulativeUsage 累加了输入/输出/缓存读写四类 token (engine.go 的
// 流式累计点), 但全仓检索确认它**没有任何一处被拿来与上限比较** —— 只增不减、
// 只写不读, 唯一读它的 GetContextUsage 拿它算的是"上下文占用百分比", 而那比的是
// 单次请求的窗口, 不是累计成本。
//
// 于是当前真正生效的"预算"只有 maxTurns, 而那是**步数**上限不是成本上限: 一次
// WebFetch 与一次 Read 都算 1 轮, 成本可能差三个数量级 (实测 209 个 run 里
// WebSearch + WebFetch 合计 3206 次, 占比极高)。步数跑满 ≠ 钱没花超。
//
// # 为什么放在 internal_hook 而不是 pkg/agent
//
// 引擎与 agent 是**互不 import** 的两层 (见 Config.Team 的注释: 避免 agent↔engine
// 环), 而这道闸门两个层次都要用 —— engine 在 queryLoop 里按 turn 计费, agent 在
// Coordinator 里按阶段计费。internal_hook 是两者共同的下游依赖 (agent 已经 import
// 它来拿 ErrorClassifier), 放这里是唯一不必在两个包里各写一份的位置。
//
// 与 H0-2 的 ErrorClassifier 同处一个包、同一族语义: 两者都是"该停的时候停得下来"
// 的止损判据, 一个按失败数, 一个按消耗量。
//
// # 为什么是事件驱动而不是墙钟
//
// 闸门只在"一次 LLM 调用返回后"被检查, 由调用方驱动, 不自起定时器。理由是 R2
// (误杀正常长任务) 的同源考虑: 一个正在安静等待上游的长任务, 没有产生任何消耗,
// 却会被墙钟一路烧掉预算 —— 那是把"上游慢"误判成"这个 run 太贵"。
// 按消耗计费、按事件检查, 才是与成本真正对应的判据。
//
// # 与 H0-1 的关系 (P0-1 的硬依赖)
//
// 输入是 token 计数, 而 H0-1 已确认: 失败调用不带 usage 是**正常的** (流里没有
// message_start/message_delta), 不是采集缺口 —— 按 status 分组后 success 侧
// 6524/6529 都带 usage。所以调用方**只应在成功调用上累加**, 否则失败的 run 会被
// 记成零消耗, 闸门等于没设。
package internal_hook

import (
	"errors"
	"fmt"
	"sync"
)

// ErrRunBudgetExhausted 是预算终止的哨兵错误, 供调用方用 errors.Is 分流。
//
// 为什么要有哨兵而不只是文案: 上层对"预算用完了"要做的事与"调用失败了"完全不同 ——
// 前者**不该重试** (重试就是继续烧钱), 后者常常该。靠字符串匹配 "budget" 来分流会
// 在文案改动时静默失效, 而失效的表现是"预算耗尽后还在重试", 正好是这道闸门要防的
// 事情。见 pkg/agent 的阶段重试诊断 (H0-4) 对它的分流。
var ErrRunBudgetExhausted = errors.New("run token budget exhausted")

// RunBudgetError 带上限与实耗的错误, 让读日志/台账的人不必再去翻配置。
type RunBudgetError struct {
	Limit int
	Spent int
}

func (e *RunBudgetError) Error() string {
	return fmt.Sprintf("%s (limit=%d spent=%d)", ErrRunBudgetExhausted.Error(), e.Limit, e.Spent)
}

// Is 让 errors.Is(err, ErrRunBudgetExhausted) 对本类型成立 —— 调用方分流时不该
// 被"错误是哨兵本身还是带数据的包装"这种实现细节绊住。
func (e *RunBudgetError) Is(target error) bool { return target == ErrRunBudgetExhausted }

// RunBudgetGate 是一个 run 的 token 预算闸门。
//
// 零值不可用, 请用 NewRunBudgetGate。方法可在多 goroutine 下安全调用
// (并发子代理各自报账)。
type RunBudgetGate struct {
	mu       sync.Mutex
	limit    int // <=0 表示不设上限 (只观测不拦截)
	spent    int
	exceeded bool
}

// NewRunBudgetGate 构造闸门。limit<=0 时永不触发, 但 Spent() 仍照常累计 ——
// 这正是"没设上限"与"没在记账"必须可区分的地方: 前者是运营选择, 后者是缺陷。
func NewRunBudgetGate(limit int) *RunBudgetGate {
	return &RunBudgetGate{limit: limit}
}

// Spend 记一次消耗并返回 (累计消耗, 是否已超预算)。
//
// 一旦超过上限即进入**单调**的耗尽态: 后续调用无论消耗多少都返回 trip=true。
// 这条单调性是刻意的 —— 预算是 run 级事实, 不该被某次零消耗的调用"撤销"。
//
// 负值被钳制为 0: 上游返回异常用量时, 朴素累加会让 spent 回退, 闸门就永远
// 触发不了 (见 budget_gate_test.go 的 TestH05_NegativeSpendCannotUnwind)。
func (g *RunBudgetGate) Spend(tokens int) (spent int, trip bool) {
	if g == nil {
		return 0, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if tokens > 0 {
		g.spent += tokens
	}
	if g.limit > 0 && g.spent > g.limit {
		g.exceeded = true
	}
	return g.spent, g.exceeded
}

// Exhausted 报告闸门是否已进入耗尽态。
func (g *RunBudgetGate) Exhausted() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exceeded
}

// Spent 返回累计消耗 (不设上限时也照常累计, 供指标与日志读数)。
func (g *RunBudgetGate) Spent() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spent
}

// Limit 返回本闸门的上限 (<=0 = 不设限)。供终止文案与日志报数用 ——
// "被拦下了但说不出上限是多少"会让排障从读一行日志变成翻配置。
func (g *RunBudgetGate) Limit() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// Reset 把闸门复位到"这个 run 还没花过钱"的状态, 上限保持不变。
//
// 存在的理由: 引擎是**会话级复用**的 (CumulativeUsage 的注释就写着"本会话累积"),
// 而闸门要的语义是 **run 级**。若不在 run 边界复位, 一个长会话跑到第 N 个 run 时
// 会被前面所有 run 的累计消耗呛死 —— 那不是"这个 run 太贵", 是判据挂错了对象。
//
// 由调用方在识别到 run 身份变化时调用 (引擎侧按 trace RunID 判), 闸门自己不猜
// run 边界在哪。
func (g *RunBudgetGate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.spent = 0
	g.exceeded = false
}

// RunBudgetExhaustedReason 是所有预算耗尽终止的**统一理由**。
//
// 单独定义成常量而不是各调用点写字面量, 是为了让"这个 run 是被预算拦下的"在终态
// 里可被断言与统计 —— 与 H0-2 的 circuit_breaker / circuit_breaker_no_progress
// 同属一类"终止原因必须有身份"的要求。没有身份的原因在台账里只会显示成"失败了",
// 而"失败"与"止损"是两件事。
const RunBudgetExhaustedReason = "budget_exhausted"
