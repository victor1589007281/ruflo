// budget.go — 单轮预算闸（Moqi R-H10）。
//
// # 为什么 harness 里还要再有一道闸
//
// engine 已经有 MaxTurns 与 RunBudgetGate（token 计），它们管住了"轮数"和"token"。
// 但 R-H10 要的五类里还有三个引擎层面根本拿不到的维度：
//
//   - **wall time**：引擎只在"一次调用返回后"检查，一个卡在工具里的 turn 永远
//     不会走到检查点。这里用独立定时器取消本轮 ctx，与引擎的事件驱动闸互补。
//   - **工具调用数**：引擎没有这个维度（MaxTurns 是模型回合数，一轮里可以并发调
//     十个工具）。这里在 ToolStart 事件上计数。
//   - **成本（钱）**：只有宿主知道价目表。harness 把 (模型, usage) 交给宿主提供的
//     Price 函数，攒够上限就取消。
//
// # 与取消的区分
//
// 三类耗尽都表现为"取消本轮 ctx"，而引擎只能看到 ctx.Err()。所以**谁取消的**必须
// 由闸门自己记住（exhaustedReason），否则终态会把"钱烧完了"报成 "cancelled" ——
// 业务层据此判断"用户主动停止"，于是不告警、不计费、不降级，问题被静默吞掉。
package harness

import (
	"context"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// budgetGate 是一次 turn 的预算闸。
//
// 零值不可用，请用 newBudgetGate。方法可被多个 goroutine 调用（墙钟定时器在
// 独立 goroutine 上、事件消费在主 goroutine 上）。
type budgetGate struct {
	mu sync.Mutex
	// reason 是第一个触发的维度（"wall" / "tool_calls" / "cost" / "run_tokens"）。
	//
	// 只记第一个：多个维度同时超限时，"哪一个先触发"才是归因依据；覆盖会让
	// 终态理由随调度顺序漂移，同一个 bug 每次复现出不同理由。
	reason string

	cancel context.CancelFunc
	price  func(model string, u types.Usage) float64
	model  string

	maxToolCalls int
	toolCalls    int

	maxCost float64
	cost    float64

	// maxRunTokens 是按 token 计的止损线（输入+输出+缓存读写）。
	//
	// 与引擎的 RunBudget 不是重复：引擎那个是**进程/run 级**的共享预算且只有
	// 输入侧（它拿不到输出 token 的准确值），这里按**本轮**累加完整 usage。
	// 两者并存时取更早触发的那个，不互相覆盖。
	maxRunTokens int
	runTokens    int

	wallTimer *time.Timer
}

// newBudgetGate 构造闸门。所有上限为 0 表示不限。
func newBudgetGate(r *Runner, req TurnRequest, cancel context.CancelFunc, model string) *budgetGate {
	if model == "" {
		// 没做角色覆盖时用 engine 默认模型名计价：价目表按模型名查，
		// 空模型名会让宿主的价格函数无从下手（只能返回 0，于是成本上限静默失效）。
		model = r.opts.Engine.Config.Model
	}
	maxTokens := req.Policy.MaxRunTokens
	maxCost := req.Policy.MaxCost
	// 角色级上限（R-H17）：与轮级上限同时存在时取更严格的那个。
	// 只做收紧、不放大 —— RoleLimit 是"这个角色额外不能超过多少"，
	// 让它覆盖掉调用方设的轮级上限会把一道更宽的闸换进更窄的位置。
	if rl, ok := req.Policy.RoleLimits[normalizedRole(req.Role)]; ok {
		if rl.MaxTokens > 0 && (maxTokens <= 0 || rl.MaxTokens < maxTokens) {
			maxTokens = rl.MaxTokens
		}
		if rl.MaxCost > 0 && (maxCost <= 0 || rl.MaxCost < maxCost) {
			maxCost = rl.MaxCost
		}
	}
	return &budgetGate{
		cancel:       cancel,
		price:        r.opts.Price,
		model:        model,
		maxToolCalls: req.Policy.MaxToolCalls,
		maxCost:      maxCost,
		maxRunTokens: maxTokens,
	}
}

// startWall 起墙钟定时器。MaxWall<=0 时什么都不做（零开销）。
//
// 为什么用 timer+取消而不是在事件循环里比对时间：事件只在模型/工具产出时才来，
// 一个卡住不动的调用不会产生任何事件 —— 而"卡住"正是墙钟要抓的那一类。
func (g *budgetGate) startWall(ctx context.Context, maxWall time.Duration) {
	if maxWall <= 0 {
		return
	}
	g.wallTimer = time.AfterFunc(maxWall, func() {
		g.trip("wall")
	})
	// ctx 先结束时把定时器停掉，避免泄漏一个挂着 Future 的 Timer。
	// 必须用 AfterFunc + 自己的 goroutine 而不是 time.After: 后者仍需有人 select，
	// 而这里没有 select 循环可挂。
	go func() {
		<-ctx.Done()
		g.stopWall()
	}()
}

func (g *budgetGate) stopWall() {
	if g.wallTimer != nil {
		g.wallTimer.Stop()
	}
}

// trip 记录首个耗尽维度并取消本轮。
func (g *budgetGate) trip(reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	first := g.reason == ""
	if first {
		g.reason = reason
	}
	cancel := g.cancel
	g.mu.Unlock()
	if first && cancel != nil {
		cancel()
	}
}

// exhaustedReason 返回触发耗尽的维度；未耗尽返回空串。
func (g *budgetGate) exhaustedReason() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason
}

// observeToolCall 记一次工具调用；超限返回 true（已触发取消）。
func (g *budgetGate) observeToolCall() bool {
	if g == nil || g.maxToolCalls <= 0 {
		return false
	}
	g.mu.Lock()
	g.toolCalls++
	over := g.toolCalls > g.maxToolCalls
	g.mu.Unlock()
	if over {
		g.trip("tool_calls")
	}
	return over
}

// observeUsage 把一次调用的 usage 折成钱；超限返回 true（已触发取消）。
//
// 同时累加 run 级 token（Moqi R-H10 的 MaxRunTokens）。两者共用这一个入口是因为
// 它们的输入是同一份 usage —— 分成两个函数就会出现"某条路径只记了钱没记 token"
// 的漏记，而漏记的表现是预算永远不会触发。
//
// 没有价目表（price==nil）时成本维度直接跳过：算不出钱就不该假装有上限。
// 这是刻意的 —— 一个"配了 MaxCost 但没装价目表"的部署，静默不限额比误杀安全，
// 而假装限额（比如按 token 数硬套一个默认价）会在换供应商时变成系统性误判。
//
// 但 **token 维度不受 price 影响**：它只用 usage 里的数字，跟有没有价目表无关。
// 把 token 检查写在 price==nil 的早退之后，会让"没装价目表"顺带关掉 token 上限
// —— 两个不相干的配置项互相否决。
func (g *budgetGate) observeUsage(u types.Usage) bool {
	if g == nil {
		return false
	}
	tokenOver := g.addTokens(u)

	if g.maxCost <= 0 || g.price == nil {
		return tokenOver
	}
	c := g.price(g.model, u)
	if c <= 0 {
		return tokenOver // 未知模型/零价：不计费，也不误判为超限
	}
	g.mu.Lock()
	g.cost += c
	over := g.cost > g.maxCost
	g.mu.Unlock()
	if over {
		g.trip("cost")
	}
	return over || tokenOver
}

// addTokens 累加本轮 token 并判定 MaxRunTokens；超限返回 true。
//
// 口径是**全部四类之和**：缓存读写在计费上是真实消耗，也不算"免费重放"——
// 把它们排除会让一个靠缓存堆上下文的 run 永远打不到上限。
//
// maxRunTokens<=0 时不做任何事（连累加都省）：零值语义是"不设上限"，
// 而累加一个没人读的数字只是多一点开销。
func (g *budgetGate) addTokens(u types.Usage) bool {
	if g == nil || g.maxRunTokens <= 0 {
		return false
	}
	n := u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	g.mu.Lock()
	g.runTokens += n
	over := g.runTokens > g.maxRunTokens
	g.mu.Unlock()
	if over {
		g.trip("run_tokens")
	}
	return over
}

// spentTokens 返回本轮已累计 token（测试与观测用）。
func (g *budgetGate) spentTokens() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runTokens
}

// spent 返回已累计成本（测试与观测用）。
func (g *budgetGate) spent() float64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cost
}
