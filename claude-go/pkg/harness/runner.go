// runner.go — Harness 的进程内实现: 把 engine 的 SubmitStreamBlocks 收口成
// 结构化 TurnResult + 宿主 TurnSink（Moqi R-H3 / R-H4 / R-H10 / R-H12 / R-H17）。
//
// 本文件**不含任何领域语义**: 它不知道"事实、记忆、边界"是什么, 只负责把 engine
// 的流式事件翻译成结构化结果, 并在正确的时机调用宿主 sink。
//
// 三条时序不变量 (R-H4 的验收全靠它们, 顺序改一个字就破坏契约):
//
//  1. EnsureUser **先于**任何模型调用。写失败 ⇒ 不调模型。改造前的形状是
//     `AppendUserMessage` 无返回值 + 先 append 后调模型, 于是"落盘失败"表现为
//     "这一轮聊完了但历史里没有" —— 下一次对话时模型不记得, 而没有任何报错。
//  2. AppendAssistant **先于**终态事件。终态事件一旦发出, 宿主就可能立刻读库;
//     先发事件后写库会给出一个短暂的"事件说有、库里没有"的窗口。
//  3. MarkTurn 最后写, 且 Persisted 如实反映 AppendAssistant 的结果 ——
//     写失败时**不能**报 Persisted=true, 否则业务层不会重试。
package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// RunnerOptions 是装配 Harness 的依赖。零值可用: 不装 sink 时不持久化,
// 与"没有 harness"的旧行为等价。
type RunnerOptions struct {
	// Engine 是被包装的引擎。必须非 nil。
	Engine *engine.QueryEngine
	// Sink 是宿主持久化。nil = 不持久化 (TurnResult.Persisted 恒 false,
	// 但 Turn 仍然跑完) —— 这是给"只想知道结果、不想落库"的调用方留的口。
	//
	// NewRunner 会自动套一层 IdempotentSink（R-H4），所以同一 TurnID 重放不会
	// 重复落库，重放路径也能命中已完成的助手消息。宿主不需要自己包。
	Sink TurnSink
	// ToolResults 是**完整工具结果**的可选落库口 (H-F1)。nil = 不落库，
	// 与改造前行为一致；ToolResult 的摘要/字节数仍会出现在 TurnResult 里。
	//
	// 与 Sink 分开装配，因为证据账本是增强能力：写入失败只进 Degraded，
	// 不改变 turn 的成败与助手消息的持久化。
	ToolResults ToolResultSink
	// Blobs 是附件 blob 存储（R-H5/R-H11）。
	//
	// nil 且本轮带附件 ⇒ 附件解析失败，本轮以 ErrKindAttachment 结束。
	// **不能**退化成"忽略附件继续问"：那会让模型基于缺了材料的正文给出很确定的
	// 回答，而调用方与用户都以为附件已被看到。
	Blobs BlobStore
	// MaxAttachmentBytes 是单轮附件总字节上限（<=0 用 ResolveAttachments 的默认）。
	MaxAttachmentBytes int64
	// ContextTokenBudget 是本轮临时上下文块的 token 预算（R-H1，<=0 = 不限）。
	//
	// 按轮而不是按块：预算要约束的是"这一轮一共带多少证据"，逐块判会让 20 条
	// 中等长度的证据每一条都合法，合起来把 prompt 撑爆。
	ContextTokenBudget int
	// ContextTimeout 是单次逐轮上下文 provider 调用的硬超时（R-H1）。
	//
	// <=0 = 不限时（改造前行为）。超时按**降级**处理：空 patch + context_degraded，
	// 本轮照常回复。之所以要硬超时而不是只靠 provider 自己看 ctx：检索连不上时，
	// 一个不检查 ctx 的实现会把整轮拖到墙钟预算，而用户等的是回复不是检索。
	// provider 应该配合取消；不配合时这里保证本轮按时返回（goroutine 会随 provider
	// 最终返回而结束）。
	ContextTimeout time.Duration
	// Ledger 是成本账本（R-H17）。nil 时退回 Price 的单轮计价，
	// TurnResult.CostKnown 仍按 usage 与价目函数的可用性如实报告。
	Ledger *CostLedger
	// Period 是时段/日预算账本（R-H17）。nil = 不做跨轮预算闸。
	//
	// 它可以包装 Ledger（构造时把同一个 CostLedger 传进去），也可以独立存在：
	// 本 Runner 每轮结束都会把**同一份** CostRecord 交给 Observe，因此窗口金额
	// 与 TurnResult.Cost 同源，不会出现两套计价口径。判定放在模型调用之前：
	// 已经超额的 owner/角色不应再产生一次真实调用。
	Period *CostPeriodLedger
	// Confirmation 是本轮工具确认来源（R-H9）。nil = 不给确认，声明了
	// NeedsConfirmation 的工具会被拒（fail-closed，引擎侧语义）。
	Confirmation tool.ConfirmationChecker
	// Idempotency 是本轮工具结果去重闸（R-H9）。nil = 不去重。
	//
	// 与 Sink 的幂等是**两层不同的幂等**：这里去重的是"同一次 run 里模型重复
	// 发起同一个工具调用"，Sink 去重的是"同一 TurnID 被业务层重试两次"。
	Idempotency *tool.IdempotencyGate
	// PromptLayout 可选: 装了就上报布局观测 (R-H2)。
	PromptLayout PromptLayoutSink
	// Context 是引擎形状的逐轮上下文提供器 (R-H1)。低层口子, 给已经按引擎
	// 形状写好的装配方用；新代码请用 ContextProvider。
	Context engine.EphemeralProvider
	// ContextProvider 是业务形状的逐轮上下文提供器 (R-H1)。
	//
	// 与 Context 同时设置时**本字段优先**：业务形状带 ID 与 trace，能进 TurnTrace；
	// 引擎形状没有这些，只能算降级路径。同时设两个不是错误，因为宿主可能在
	// 迁移期两个都接。
	ContextProvider TurnContextProvider
	// RoleModels 是角色 → 模型 的默认映射 (R-H17)。TurnPolicy.ModelRoles 优先。
	//
	// 不在这里兜底"找不到就用默认模型"的静默行为: 找不到就走 engine 默认,
	// 并把实际使用的模型写进 TurnResult.Model —— 路由永远可观测。
	RoleModels map[string]string
	// Price 是计价函数 (R-H17 成本归集)。nil = 不计算成本, TurnResult.CostKnown=false。
	//
	// 用函数而不是价目表: 价格随供应商/时段变化, 且分档计价无法用一张静态表表达。
	// 业务层持有价目表, harness 只负责把 (模型, usage) 变成钱。
	Price func(model string, u types.Usage) float64
	// Modality 可选: 模型能力查询 (R-H11)。nil = 用内置保守表 (见 modality.go)。
	Modality func(model, kind string) (supported bool)
	// Trace 可选: 每轮 trace 落库 (R-H14)。nil = 不落 trace。
	//
	// 写失败只进日志、不影响 TurnResult —— 观测面不在事务里。
	Trace TraceSink
	// Layout 可选: prompt 布局构建器 (R-H2)。nil = 不上报布局。
	//
	// 传进来而不是内部 new：布局的 PrefixHash 需要**跨轮**记住上一次的稳定段，
	// 同一个 session 的多轮必须共用同一个 Builder，内部 new 会让"画像没变"
	// 永远无法被观测到。
	Layout *LayoutBuilder
	// Now 可注入便于测试; 零值取 time.Now。
	Now func() time.Time
}

// Runner 是 Harness 的进程内实现。
type Runner struct {
	opts RunnerOptions

	mu      sync.Mutex
	running map[string]*runningTurn // turnID → 在途 turn
	// sessions 保证每个 session 同一时刻最多一个 run (Q-H5)。
	//
	// 引擎本身不支持并发 run: SubmitStreamBlocks 会 append e.Messages 并整体替换,
	// 两个并发 run 会互相吃到对方的消息。在这里挡住比在引擎里加锁便宜: 引擎的并发
	// 使用方还有团队路径 (每个 runner 独立引擎), 不该被会话级约束拖慢。
	sessions map[string]string // sessionID → 占用它的 turnID
}

type runningTurn struct {
	cancel  context.CancelFunc
	session string
	// budgetReason 非空表示这次取消是预算闸门触发的 (R-H10)。
	//
	// 必须与用户主动取消分开: 两者都表现为 ctx 被取消, 但终态一个该报
	// budget_exhausted、一个该报 cancelled。靠"取消后看 ctx.Err()"分不开 ——
	// 它只说 canceled, 不说谁取消的。
	budgetReason string
}

// 编译期断言: Runner 满足 Harness。
var _ Harness = (*Runner)(nil)

// NewRunner 构造 Harness。Engine 为 nil 时返回的 Runner 在 RunTurn 里报错,
// 而不是在构造期 panic —— 装配期的 nil 应该是可读的错误, 不是崩溃。
func NewRunner(opts RunnerOptions) *Runner {
	// R-H4 的幂等落在**装配处**而不是要求每个宿主各自实现：宿主的 sink 实现
	// 分散且互不共享状态，只要有一家漏了，重复消息就会出现在用户面前。这里包一层
	// 对"所有已发布 TurnSink 实现"同时生效，且 ComposeAssistant 的重放查询也
	// 随之可用（包装层同时实现 TurnReplay）。
	//
	// sink 为 nil 时不要赋值：NewIdempotentSink(nil) 返回的是**类型化 nil 指针**，
	// 装进 TurnSink 接口后 `r.opts.Sink != nil` 为真 —— 于是 NilSink 路径会走进
	// 持久化分支并把 Persisted 报成 true（nil receiver 的方法返回 nil）。必须先
	// 判空再包。
	if opts.Sink != nil {
		opts.Sink = NewIdempotentSink(opts.Sink)
	}
	return &Runner{
		opts:     opts,
		running:  make(map[string]*runningTurn),
		sessions: make(map[string]string),
	}
}

func (r *Runner) now() time.Time {
	if r.opts.Now != nil {
		return r.opts.Now()
	}
	return time.Now()
}

// ErrNoEngine 表示 Runner 没有装配 engine。
var ErrNoEngine = errors.New("harness: engine 未装配")

// ErrSessionBusy 表示同一 session 上已有在途 run（Q-H5）。
var ErrSessionBusy = errors.New("harness: 该 session 已有在途 run")

// Result 构造一个只填了身份字段的空结果, 供各条早退路径使用。
func emptyResult(req TurnRequest) *TurnResult {
	return &TurnResult{
		TurnID:     req.TurnID,
		SessionID:  req.SessionID,
		OwnerID:    NormalizeOwner(req.OwnerID),
		EpisodeID:  req.EpisodeID,
		UsageKnown: false,
		CostKnown:  false,
		Terminal:   types.Terminal{Reason: "completed"},
	}
}

// RunTurn 跑一轮并返回结构化结果。
//
// 返回值契约: TurnResult **恒非 nil**, 即使 error 非 nil —— 业务层需要 Terminal
// 与已经产生的部分文本才能正确渲染失败。返回 nil result 会让每条错误路径都多一个
// nil 检查, 而漏掉一处就是整轮崩溃。
func (r *Runner) RunTurn(ctx context.Context, req TurnRequest, emit func(TurnEvent)) (*TurnResult, error) {
	started := r.now()
	var eventCount int
	wrapped := emit
	if emit != nil {
		wrapped = func(ev TurnEvent) {
			eventCount++
			emit(ev)
		}
	}
	run := &runState{}
	res, err := r.runTurn(ctx, req, wrapped, run)
	// trace 在**所有**出口都落（包括失败轮）── "100% turn 有 trace" 是 R-H14 的原话,
	// 只在成功路径记会让最需要排查的那些轮恰好缺记录。
	r.recordTrace(ctx, req, res, started, eventCount, run)
	return res, err
}

// runState 是一次 run 的临时状态。
//
// 用局部对象而不是 Runner 字段：Runner 被所有 session 共用，字段会让并发 run
// 互相看到对方的证据块 ID —— 而 trace 里"本轮带了哪几条证据"错到别的 turn 上,
// 比没有这条数据更坏。
//
// 加锁是因为写入方不止一个：context 选择与回退观测由**引擎侧 goroutine** 在
// 请求过程中回调，而读取发生在 drain 返回之后。虽然 stream channel 关闭提供了
// happens-before，但把正确性寄托在"调用方一定会读干 channel"上太脆 —— 早退
// 路径（预算取消、调用方直接放弃）不保证这一点。
type runState struct {
	mu sync.Mutex

	contextBlockIDs    []string
	contextDroppedIDs  []string
	contextTokens      int
	contextTokensKnown bool
	contextTrace       string

	// layout 是**本轮真正用于出站的那一份**，不是事后重算的。recordTrace 直接
	// 用它，否则两次 Build 会让 LayoutBuilder 的状态机（prev/prevHash）多走一步，
	// 第二份观测的 InvalidationReason 会变成"无变化"，与真实请求对不上。
	layout *PromptLayout

	fallbacks []api.FallbackInfo
	// fallbackChain 是 fallbacks 经账本口径转换后的链，runTurn 收尾时算一次。
	// recordTrace 直接用它，避免"trace 里一套、账本里另一套"。
	fallbackChain []FallbackEvent

	degraded []string
	// citations 是本轮真正进入请求的上下文块所携带的引用（R-H3）。
	// 与 contextBlockIDs 同源：被预算裁掉的块对应的引用不在这里。
	citations []Citation
	// hooks 是逐 hook 执行记录（R-H7），由引擎侧 observer 在请求过程中追加。
	hooks []HookRecord
}

// setContext 记录进入请求的上下文选择（裁剪后）。
func (s *runState) setContext(sel ContextSelection, trace string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contextBlockIDs = append([]string(nil), sel.IDs...)
	s.contextDroppedIDs = append([]string(nil), sel.DroppedIDs...)
	s.contextTokens = sel.Tokens
	s.contextTokensKnown = sel.TokensKnown
	s.contextTrace = trace
	s.citations = append([]Citation(nil), sel.Citations...)
	if sel.Degraded != "" {
		s.degraded = appendUnique(s.degraded, sel.Degraded)
	}
}

// setLayout 记录本轮出站布局。
func (s *runState) setLayout(l PromptLayout) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.layout = &l
	s.mu.Unlock()
}

// addFallback 记录一次模型回退（由 api 观测回调在引擎 goroutine 里调用）。
func (s *runState) addFallback(info api.FallbackInfo) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.fallbacks = append(s.fallbacks, info)
	s.mu.Unlock()
}

// setFallbackChain 记录本轮的回退链（含写账本后的结果）。
func (s *runState) setFallbackChain(chain []FallbackEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.fallbackChain = append([]FallbackEvent(nil), chain...)
	s.mu.Unlock()
}

// addDegraded 记录一条降级原因，去重（同一原因每轮只报一次）。
func (s *runState) addDegraded(reason string) {
	if s == nil || reason == "" {
		return
	}
	s.mu.Lock()
	s.degraded = appendUnique(s.degraded, reason)
	s.mu.Unlock()
}

// addHookTrace 记录一次内置 hook 执行（R-H7）。
//
// 观测者可能在引擎侧 goroutine 里被调用（并行工具路径），因此与其它 runState
// 写入一样走锁。panic/timeout 同时写进 Degraded：trace 记录回答"哪个 hook 出了
// 什么事"，Degraded 回答"这一轮是否因此降级"，两者不能只留一个。
func (s *runState) addHookTrace(tr engine.HookTrace) {
	if s == nil {
		return
	}
	rec := HookRecord{
		Name:        tr.Name,
		Phase:       tr.Phase.String(),
		Criticality: tr.Criticality.String(),
		DurationMS:  float64(tr.Duration.Microseconds()) / 1000,
		Result:      tr.Result,
		Error:       tr.Err,
		Panic:       tr.Panic,
		TimedOut:    tr.TimedOut,
		Skipped:     tr.Skipped,
	}
	s.mu.Lock()
	s.hooks = append(s.hooks, rec)
	if tr.Panic != "" {
		s.degraded = appendUnique(s.degraded, "hook_panic")
	}
	if tr.TimedOut {
		s.degraded = appendUnique(s.degraded, "hook_timeout")
	}
	s.mu.Unlock()
}

// hookTraceObserver 把引擎侧 HookTrace 收进本轮 runState。
//
// 无状态（只持有 runState 指针），因此天然并发安全；真正的锁在 addHookTrace。
type hookTraceObserver struct {
	run *runState
}

func (o hookTraceObserver) ObserveHookTrace(_ context.Context, tr engine.HookTrace) {
	if o.run == nil {
		return
	}
	o.run.addHookTrace(tr)
}

// snapshot 取本轮观测状态的一致副本。
func (s *runState) snapshot() runSnapshot {
	if s == nil {
		return runSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return runSnapshot{
		contextBlockIDs:    append([]string(nil), s.contextBlockIDs...),
		contextDroppedIDs:  append([]string(nil), s.contextDroppedIDs...),
		contextTokens:      s.contextTokens,
		contextTokensKnown: s.contextTokensKnown,
		contextTrace:       s.contextTrace,
		layout:             s.layout,
		fallbacks:          append([]api.FallbackInfo(nil), s.fallbacks...),
		fallbackChain:      append([]FallbackEvent(nil), s.fallbackChain...),
		degraded:           append([]string(nil), s.degraded...),
		citations:          append([]Citation(nil), s.citations...),
		hooks:              append([]HookRecord(nil), s.hooks...),
	}
}

type runSnapshot struct {
	contextBlockIDs    []string
	contextDroppedIDs  []string
	contextTokens      int
	contextTokensKnown bool
	contextTrace       string
	layout             *PromptLayout
	fallbacks          []api.FallbackInfo
	fallbackChain      []FallbackEvent
	degraded           []string
	citations          []Citation
	hooks              []HookRecord
}

// toolPreviewBytes 是 ToolEvent 输入/结果预览的上限（H-F5）。
// 与历史 ResultPreview 的 500 字节保持一致；用字节而不是 rune，是为了让
// 事件的体积上限可预测（事件会进 SSE 与 trace）。
const toolPreviewBytes = 500

// previewBytes 截取前 n 字节；n <= 0 或内容更短时原样返回。
//
// 与旧实现相同的字节切片口径（允许切断 UTF-8 序列）：预览不是正文，消费方
// 不得把它当作可解析内容；改成 rune 截断会改变既有预览的字节长度，反而让
// "500B 预览"这条契约变得不可断言。
func previewBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

// appendUnique 追加非空且不重复的原因，保持顺序。
func appendUnique(dst []string, v string) []string {
	if v == "" {
		return dst
	}
	for _, x := range dst {
		if x == v {
			return dst
		}
	}
	return append(dst, v)
}

// recordTrace 落本轮 trace。写失败只记日志：观测面不在事务里。
func (r *Runner) recordTrace(ctx context.Context, req TurnRequest, res *TurnResult, started time.Time, events int, run *runState) {
	if r == nil || r.opts.Trace == nil || res == nil {
		return
	}
	tr := TraceFromResult(res, req.Role, started, events)
	snap := run.snapshot()
	tr.ContextBlockIDs = snap.contextBlockIDs
	tr.ContextDroppedIDs = snap.contextDroppedIDs
	tr.ContextTokens = snap.contextTokens
	tr.ContextTokensKnown = snap.contextTokensKnown
	tr.ContextTrace = snap.contextTrace
	tr.PromptLayout = snap.layout
	tr.FallbackChain = snap.fallbackChain
	tr.Hooks = snap.hooks
	if err := r.opts.Trace.RecordTurnTrace(ctx, tr); err != nil {
		logging.For("harness").Warn("trace 落库失败", "turn", res.TurnID, "err", err)
	}
}

func (r *Runner) runTurn(ctx context.Context, req TurnRequest, emit func(TurnEvent), run *runState) (*TurnResult, error) {
	res := emptyResult(req)

	// 事件发射器在**所有**可能发事件的出口之前定义：重放短路、预算取消、
	// 持久化失败都会发终态事件，晚定义就意味着那些路径静默不发。
	//
	// 加锁是因为 drain 里的事件由消费循环发，而预算闸/取消路径可能从另一个
	// goroutine 触发（Cancel）。Seq 必须在一轮内单调且唯一。
	var emitMu sync.Mutex
	var seq int64
	emitEvent := func(ev TurnEvent) {
		if emit == nil {
			return
		}
		emitMu.Lock()
		seq++
		ev.Seq = seq
		ev.TurnID = req.TurnID
		emitMu.Unlock()
		emit(ev)
	}

	// 终态**恰好一条**（R-H12），且每条出口都要有。
	//
	// 改造前只有"重放 / 持久化失败 / 正常完成"三条路径显式发终态，其余早退
	// （参数错误、模态不支持、附件解析失败、EnsureUser 失败、session busy、
	// 布局错误、时段预算）都是静默返回 —— 消费方按"见到终态才收尾"实现时会
	// 永远等下去。这里用一个 defer 兜底：显式路径仍然发第一条，其余出口由它补。
	//
	// 用 defer 而不是在每个 return 前补一行：早退点有十来个，逐个补漏一个就是
	// 一次挂死；而且后续新增早退点也会自动被兜住。
	var terminalSent bool
	emitTerminal := func() {
		if terminalSent {
			return
		}
		terminalSent = true
		emitEvent(TurnEvent{Kind: EventTerminal, Terminal: &res.Terminal})
	}
	defer emitTerminal()

	// 引擎未装配也要走终态事件：消费方只认终态，缺一条就是挂死。
	if r == nil || r.opts.Engine == nil {
		res.Terminal = types.Terminal{Reason: "error", Error: ErrNoEngine}
		return res, NewError(ErrKindInternal, req.TurnID, ErrNoEngine)
	}

	// ===== 工具档位 (R-H9 TurnPolicy.ToolProfile) 归一 =====
	//
	// 在写 ctx 之前做: profile 会**收窄** AllowedTools, 而收窄后的结果才是本轮
	// 的真实档位。放到引擎侧收窄就晚了 —— 那里只看得到名单, 不知道"调用方要求的
	// 是陪伴档"。
	//
	// 失败即拒: "companion 档位与调用方名单无交集" 说明配置对不上, 继续跑会得到
	// 一个什么工具都不能用的哑巴会话, 而用户看到的是"模型不会用工具"。宁可当场
	// 报错并让装配问题暴露。
	policy := req.Policy
	if err := applyToolProfile(&policy); err != nil {
		res.Terminal = types.Terminal{Reason: "error", Error: err}
		return res, NewError(ErrKindSafety, req.TurnID, err)
	}
	req.Policy = policy

	// ===== owner 进 ctx (R-H6) =====
	//
	// 这一行是 R-H6 的**唯一接缝**：engine 的 ownerFrom、ToolContext.OwnerID、
	// trace/metrics 的 owner 全都读 ctx，不读 TurnRequest。所以要死守两条：
	//
	//  1. 必须在**任何**下游调用之前写入。写晚了，engine 会退回到
	//     Config.OwnerID 或 DefaultOwnerID —— 多租户部署下那就是全部请求归到
	//     同一个 owner，且不会报错。
	//  2. 必须写进 run 自己的 ctx 派生链，而不是 Runner 字段。Runner 被并发
	//     run 共用（见 runState 注释），字段会把 A 的 owner 串给 B。
	//
	// 这里的 ctx 后面会派生出 runCtx（enter）再交给 engine，派生链天然带值。
	ctx = WithOwner(ctx, req.OwnerID)
	// session 同一条接缝：引擎侧 plan mode / 计划文件目录都按它命名空间化，
	// 读 Config.SessionID 会让所有会话共用一个 —— 见 pkg/types/session.go。
	ctx = types.WithSession(ctx, req.SessionID)

	if err := ctx.Err(); err != nil {
		res.Terminal = types.Terminal{Reason: "cancelled", Error: err}
		return res, NewError(ErrKindCanceled, req.TurnID, err)
	}
	if req.TurnID == "" {
		// 空 TurnID 会让 sink 的幂等键退化成"每次都不同", 重试就变成重复消息。
		// 这里给一个明确的失败而不是静默生成 —— 静默生成会让调用方以为幂等生效了。
		err := errors.New("harness: TurnRequest.TurnID 不能为空（它是 sink 的幂等键）")
		res.Terminal = types.Terminal{Reason: "error", Error: err}
		return res, NewError(ErrKindInternal, "", err)
	}

	// ===== 0. 业务安全判定 (R-H3) =====
	//
	// 位置必须早于**任何**模型调用与落库：safety=block 的语义是"这段内容不能
	// 发出去"，先调模型再拦等于已经发出去了。改造前 TurnRequest 里根本没有这个
	// 字段，TurnResult.Safety 因此永远是零值 —— 业务层的危机拦截只能发生在
	// harness 之外，而"harness 保证不发送"这条验收无从谈起。
	res.Safety = req.Safety
	if err := applySafety(req.Safety); err != nil {
		res.Terminal = types.Terminal{Reason: "safety_blocked", Error: err}
		return res, NewError(ErrKindSafety, req.TurnID, err)
	}
	if strings.EqualFold(req.Safety.Decision, "degrade") {
		res.Degraded = appendUnique(res.Degraded, "safety_degraded")
	}

	// ===== 0.1 角色路由 (R-H17) 与模态协商 (R-H11) =====
	model, err := r.resolveModel(req)
	if err != nil {
		res.Terminal = types.Terminal{Reason: "error", Error: err}
		return res, NewError(ErrKindModel, req.TurnID, err)
	}
	res.Model = model
	// 用**实际会用的模型**做能力判定，而不是只看角色覆盖值：没有覆盖时
	// model=="" 会让整段协商被跳过（见 modality.go 的 checkModalities 注释）。
	if err := checkModalities(r.effectiveModel(model), req.Input, req.Attachments, r.opts.Modality); err != nil {
		res.Terminal = types.Terminal{Reason: "error", Error: err}
		return res, NewError(ErrKindModality, req.TurnID, err)
	}

	// ===== 0.5 附件解析成协议内容块 (R-H5/R-H11) =====
	//
	// 这一步必须在**任何**模型调用之前完成，而且失败必须整轮失败（见
	// resolve_input.go 文件头）：只发正文、把附件悄悄丢掉，会让模型基于缺了
	// 材料的正文给出很确定的回答，而调用方与用户都以为附件已被看到。
	//
	// 返回值同时用于两处：req.Input 保持原样落库（sink 记的是引用，不是展开后的
	// 协议块），modelInput 才是发给模型的完整输入。
	modelInput, err := ResolveTurnInput(r.opts.Blobs, req.Attachments, req.Input, r.opts.MaxAttachmentBytes)
	if err != nil {
		res.Terminal = types.Terminal{Reason: "attachment_failed", Error: err}
		return res, NewError(ErrKindAttachment, req.TurnID, err)
	}

	// ===== 1. 先确认用户消息已落盘 (R-H4 不变量 1) =====
	userRec := UserRecord{
		OwnerID:     res.OwnerID,
		SessionID:   req.SessionID,
		TurnID:      req.TurnID,
		EpisodeID:   req.EpisodeID,
		Blocks:      req.Input,
		Attachments: req.Attachments,
		Speaker:     req.Speaker,
		Sensitivity: req.Sensitivity,
		CreatedAt:   r.now(),
	}
	if r.opts.Sink != nil {
		if _, err := r.opts.Sink.EnsureUser(ctx, userRec); err != nil {
			// 写失败 ⇒ 不进模型调用。这是与"先调模型再落盘"的关键差别:
			// 后者会在库里丢消息而对话照常进行, 且没有任何报错。
			res.Terminal = types.Terminal{Reason: "sink_failed", Error: err}
			res.Persisted = false
			return res, NewError(ErrKindSink, req.TurnID, err)
		}
	}

	// ===== 1.5 已完成 turn 的重放短路 (R-H4 验收第 3 条) =====
	//
	// 同一 TurnID 的第二次请求必须返回**同一份回答**，而不是再问一次模型。
	// 位置在 EnsureUser 之后：重放的前提是用户消息确实存在；放在它之前，一次
	// 从未落库的 turn 会因助手缓存命中而"看起来已经完成"，用户消息却永远丢了。
	//
	// 查询失败**不**短路：宁可再跑一次（AppendAssistant 侧仍然幂等），也不要
	// 把一次本可正常回答的 turn 变成失败。降级标记如实写上。
	//
	// 重放不记成本：原轮已经记过账，这里再记一次就是双计；而 AssistantRecord
	// 不带金额，猜一个 0 会把重放轮伪装成免费（见 replayResult）。
	if replay, ok := r.opts.Sink.(TurnReplay); ok {
		rec, done, lookupErr := replay.CompletedAssistant(ctx, res.OwnerID, req.SessionID, req.TurnID)
		if lookupErr != nil {
			res.Degraded = appendUnique(res.Degraded, "replay_lookup_failed")
		} else if done {
			res = replayResult(req, rec)
			emitEvent(TurnEvent{Kind: EventDegraded, Note: "turn_replayed"})
			emitTerminal()
			return res, classifyTerminal(res)
		}
	}

	// ===== 2. 占用 session 与 run 槽位 (Q-H5) =====
	runCtx, cancel, err := r.enter(ctx, req)
	if err != nil {
		res.Terminal = types.Terminal{Reason: "busy", Error: err}
		return res, NewError(ErrKindInternal, req.TurnID, err)
	}
	defer r.leave(req.TurnID, cancel)

	// 在途状态 (Q-H11)：崩溃恢复扫描的是"pending/running"的 turn，而本方法
	// 此前从不写它们 —— RecoverInFlight 因此永远面对一个空集合，标记为
	// interrupted 的能力存在但从不触发。这里在模型调用之前落 running。
	//
	// 写失败只降级、不中断：状态标记是恢复能力的支撑，不是用户这一轮的必要条件；
	// 为了标记失败而让用户收不到回复，是把观测设施的故障变成业务故障。
	if r.opts.Sink != nil {
		if err := r.opts.Sink.MarkTurn(runCtx, TurnState{
			OwnerID: res.OwnerID, SessionID: req.SessionID, TurnID: req.TurnID,
			State: stateRunning, UpdatedAt: r.now(),
		}); err != nil {
			res.Degraded = appendUnique(res.Degraded, "mark_running_failed")
		}
	}

	// 引擎级覆盖: 模型 / 轮数 / 工具调用数上限 / 单轮工具白名单。
	//
	// AllowedTools 必须在这里合并而不是只传给下游: 引擎侧是唯一能拦住工具**执行**
	// 的地方 (RunToolUse 之前), 而 harness 只看得见事件 —— 等到事件里出现一个
	// 不该调的工具时, 副作用已经发生了。
	runCtx = engine.WithRunOverrides(runCtx, engine.RunOverrides{
		Model:        model,
		MaxTurns:     req.Policy.MaxTurns,
		MaxTokens:    req.Policy.MaxTokens,
		MaxToolCalls: req.Policy.MaxToolCalls,
		AllowedTools: req.Policy.AllowedTools,
		// 工具闸 (R-H9) 随本轮覆盖一起下发。引擎把它们写进 ToolContext，
		// orchestration 才会真正执行"需确认而未确认 ⇒ 拒绝"与"幂等去重"。
		// 不接线时这两个字段为 nil，既有工具行为逐字节不变。
		Confirmation: r.opts.Confirmation,
		Idempotency:  r.opts.Idempotency,
	})

	// 逐 hook 观测（R-H7）：observer 挂在**本轮 ctx** 上，引擎在每次 hook
	// 执行/跳过时同步回调。装在 ctx 而不是链路字段上，并发 run 才不会串记录。
	//
	// 只有要落 trace 时才装：观测本身很轻，但没有消费方时记录一个永远不读的
	// 切片只是给每轮多加一次分配。panic/timeout 的 Degraded 标记也因此只在
	// 开了 trace 的部署里出现 —— 它们是观测面的一部分，不是业务语义。
	if r.opts.Trace != nil {
		runCtx = engine.WithHookTraceObserver(runCtx, hookTraceObserver{run: run})
	}

	// 逐轮上下文（R-H1）：业务形状优先，引擎形状兜底。
	//
	// 业务形状走带预算的适配器：块 ID、被裁 ID、估算 token、结构性降级都要
	// 写进 trace。旧 adaptContextProvider 只回报"发出去哪些 ID"，预算裁剪
	// 与丢弃原因在 trace 里看不到 —— 那正是"证据为什么没出现"最难答的一半。
	if r.opts.ContextProvider != nil {
		provider := TurnContextProvider(r.opts.ContextProvider)
		// provider 自报/抛出的降级在适配器里没有出口，这里包一层补上；
		// 否则检索超时这件事在 TurnResult 与 trace 里完全不可见。
		provider = &degradingProvider{
			inner:     provider,
			timeout:   r.opts.ContextTimeout,
			onDegrade: run.addDegraded,
		}
		runCtx = engine.WithEphemeralProvider(runCtx,
			adaptContextProviderWithBudget(provider, req, r.opts.ContextTokenBudget,
				func(sel ContextSelection, trace string) {
					run.setContext(sel, trace)
				}))
	} else if r.opts.Context != nil {
		runCtx = engine.WithEphemeralProvider(runCtx, r.opts.Context)
	}

	// 布局与缓存计划计算**一次**（R-H2）：同一份结果既用于出站请求，也写进 trace。
	//
	// 放在模型调用之前、所有输入定型之后。失败按配置错误处理：把错误吞成
	// "没有计划"会让超限断点照样发出去（上游整请求失败），或者让配置错误
	// 悄悄退化成内置启发式而无人知晓。
	if r.opts.Layout != nil {
		layout, plan, _, layoutErr := r.turnLayout(req)
		if layoutErr != nil {
			res.Terminal = types.Terminal{Reason: "error", Error: layoutErr}
			return res, NewError(ErrKindInternal, req.TurnID, layoutErr)
		}
		run.setLayout(layout)
		if plan != nil {
			// 装饰在引擎侧"发送前最后一刻"应用（见 engine/clientdeco_ctx.go）：
			// 这里的 client 只是模板，真正的副本由 engine 在 WithModel/PlanClient
			// 之后生成。回退观测与缓存计划共用同一次装饰。
			runCtx = engine.WithClientDecorator(runCtx, func(c *api.Client) *api.Client {
				return c.WithCachePlan(plan).WithFallbackObserver(run.addFallback)
			})
		} else if r.opts.Trace != nil || r.opts.Ledger != nil {
			runCtx = engine.WithClientDecorator(runCtx, func(c *api.Client) *api.Client {
				return c.WithFallbackObserver(run.addFallback)
			})
		}
	} else if r.opts.Trace != nil || r.opts.Ledger != nil {
		// 没有布局也要观测回退：回退链是 trace 的一等字段（R-H17），
		// 不能因为没装配缓存布局就整段缺失。
		runCtx = engine.WithClientDecorator(runCtx, func(c *api.Client) *api.Client {
			return c.WithFallbackObserver(run.addFallback)
		})
	}

	// ===== 2.5 时段/日预算闸 (R-H17) =====
	//
	// 位置必须在**模型调用之前**：跨越窗口的账本闸约束的是"这个 owner 的这个
	// 角色今天还能不能发起新调用"，等到本轮烧完再查就已经晚了。与 budget.go 的
	// 单轮闸是两层：单轮闸保证一轮内不失控，这里保证一天内不失控。
	//
	// 只按 Over 拒绝。Known=false（窗口里存在未计价条目）**不能**当超额处理：
	// 未知成本不是 0，但也不是"已确定超限"；把未知当超限会让一个价目表缺项
	// 直接变成全员不可用。此时放行，额度判断留给能补齐价格的调用方。
	if r.opts.Period != nil {
		label, unknown := exceededPeriod(r.opts.Period.QueryAt(r.now(), res.OwnerID, req.Role))
		if label != "" {
			err := fmt.Errorf("turn %s 超出时段预算 %s (owner=%s role=%s)",
				req.TurnID, label, res.OwnerID, req.Role)
			res.Terminal = types.Terminal{Reason: "budget_exhausted", Error: err}
			res.Degraded = appendUnique(res.Degraded, "budget_exhausted:period:"+label)
			return res, NewError(ErrKindBudget, req.TurnID, err)
		}
		// 未计价条目既不是 0，也不能当超限。这里把它写进 Degraded，避免
		// "窗口看起来没超"被误读成"窗口确实没超"。
		for _, l := range unknown {
			res.Degraded = appendUnique(res.Degraded, "period_cost_unknown:"+l)
		}
	}

	// ===== 3. 预算闸 (R-H10): wall / cost 由这里驱动 =====
	gate := newBudgetGate(r, req, cancel, model)
	gate.startWall(runCtx, req.Policy.MaxWall)

	// modelInput 而不是 req.Input：附件块已经解析并追加在正文之后（R-H5/R-H11）。
	// 非文本块（图片/PDF）由 splitInput 归到 extra，引擎再放回本条消息里。
	text, extra := splitInput(modelInput)

	// 完整工具结果经由 drain 交给可选的 ToolResultSink。放在这里（助手消息
	// 持久化之前）是 H-F1 的顺序要求：证据先落，回复后落；即使随后助手消息
	// 持久化失败，证据也不会缺。
	drain(runCtx, r.opts.Engine.SubmitStreamBlocks(runCtx, text, extra), emitEvent, res, gate, r.now,
		r.opts.ToolResults, res.OwnerID, req.SessionID, req.TurnID)

	// ===== 4. 终态与预算归因 =====
	terminal := r.opts.Engine.LastTerminal()
	res.Terminal = terminal
	if reason := gate.exhaustedReason(); reason != "" {
		// 预算闸门取消了本轮: 引擎只能看到"ctx 被取消", 说不出为什么。
		// 终态理由必须由闸门提供, 否则业务层会把"钱烧完了"当作用户取消。
		res.Terminal = types.Terminal{
			Reason: "budget_exhausted",
			Error:  fmt.Errorf("turn %s 超出%s预算", req.TurnID, reason),
		}
		res.Degraded = append(res.Degraded, "budget_exhausted:"+reason)
	} else if ctx.Err() != nil && terminal.Reason != "cancelled" {
		// 调用方取消但引擎报了别的终态 —— 以取消为准, 否则业务层会把它当正常完成。
		res.Terminal = types.Terminal{Reason: "cancelled", Error: ctx.Err()}
	}
	// partial（R-H10）：非正常完成且有内容 ⇒ 这段文字是半截的。
	// 没有任何内容时不标 partial —— 那只是失败，不是"部分回复"。
	res.Partial = isPartialTerminal(res.Terminal) && (res.Text != "" || len(res.Blocks) > 0)

	// 本轮观测状态合流：上下文裁剪/检索降级、回退链、引用。
	//
	// 放在终态归因之后、写助手消息之前：这些字段会进 trace，而 trace 的
	// Degraded 来自 res.Degraded —— 晚合并就会漏报。
	snap := run.snapshot()
	for _, d := range snap.degraded {
		res.Degraded = appendUnique(res.Degraded, d)
	}
	// 引用来自**真正进入请求**的上下文块（R-H3）。与 contextBlockIDs 同一来源，
	// 因此"trace 说带了这条证据"与"业务层能渲染这个角标"永远对得上。
	//
	// 每个引用发一条 EventCitation：前端不必解析 prose 就能画角标（R-H12）。
	for i := range snap.citations {
		c := snap.citations[i]
		res.Citations = append(res.Citations, c)
		// 不填 BlockIndex：引用是按证据块顺序编号的，与内容块下标不是同一个
		// 坐标系。填一个看起来像块下标的值会让前端把角标挂到错误的块上。
		emitEvent(TurnEvent{Kind: EventCitation, Citation: &c})
	}

	// 布局观测（R-H2）：把**本轮出站的那一份**布局与真实 usage 交给 sink。
	// 只装配 Layout 而不上报是改造前的状态 —— LayoutBuilder 的 PrevHash 状态机
	// 在跑，观测却没有任何出口，业务层看不到前缀是否稳定、缓存是否被复用。
	if snap.layout != nil && (r.opts.PromptLayout != nil || r.opts.Layout != nil) {
		sink := r.opts.PromptLayout
		if sink == nil {
			sink = r.opts.Layout
		}
		// 用不随取消失效的 ctx：用户取消时这一轮仍应被观测到，观测面不该
		// 因为取消而整段缺失。值（owner/session）保留，取消信号摘除。
		if err := sink.RecordPromptLayout(context.WithoutCancel(ctx), *snap.layout, res.Usage); err != nil {
			logging.For("harness").Warn("布局观测上报失败", "turn", req.TurnID, "err", err)
		}
	}
	costKey := CostKey{
		Role:      req.Role,
		TurnID:    req.TurnID,
		SessionID: req.SessionID,
		OwnerID:   res.OwnerID,
	}
	run.setFallbackChain(fallbackChain(r.opts.Ledger, costKey, snap.fallbacks))

	// 成本归集 (R-H17): 与 usage 同源, usage 未知时成本也必须未知。
	//
	// 装了账本就走账本（四维归集 + 未知模型不当免费）；否则保持单轮计价。
	// 两条路径**不叠加**：同时算会把同一笔消耗记成两个口径，而业务层无从分辨
	// TurnResult.Cost 是哪一个。
	// 计价按**实际作答**的模型：发生了回退时，usage 属于最后接住请求的那个备用
	// 模型，按主模型单价计费会把回退成本算错（而回退恰恰常发生在主模型最贵/
	// 最不稳的时候）。没有回退时用路由结果或引擎默认。
	ledgerModel := r.effectiveModel(model)
	if n := len(snap.fallbacks); n > 0 {
		if to := snap.fallbacks[n-1].ToModel; to != "" {
			ledgerModel = to
		}
	}
	costEntry := TurnCostEntry(req, req.Role, ledgerModel, res.Usage, res.UsageKnown)
	if r.opts.Ledger != nil {
		rec := r.opts.Ledger.Record(costEntry)
		res.Cost = rec.Cost
		res.CostKnown = rec.CostKnown()
		if r.opts.Period != nil {
			// Observe 而不是 Record：金额已经由 CostLedger 记过一次，
			// 再 Record 会让底层账本翻倍（D 在 cost_period.go 文件头写了这条）。
			r.opts.Period.ObserveAt(rec, r.now())
		}
	} else if r.opts.Price != nil && res.UsageKnown {
		res.Cost = r.opts.Price(ledgerModel, res.Usage)
		res.CostKnown = true
		if r.opts.Period != nil {
			// 没有 CostLedger 时也要让窗口看见这一笔，否则时段预算在
			// "只用 Price 的单轮计价"装配下永远停在 0，闸门形同虚设。
			r.opts.Period.ObserveAt(CostRecord{
				CostKey:    costEntry.CostKey,
				Model:      ledgerModel,
				Usage:      res.Usage,
				UsageKnown: res.UsageKnown,
				Cost:       res.Cost,
				Pricing:    CostPricingKnown,
			}, r.now())
		}
	} else if r.opts.Period != nil {
		// usage 未知或没有价目函数：窗口必须看见"有过一次调用但金额未知"，
		// 否则 UnknownEntries 永远是 0，Known 会把不完整的窗口伪装成全量。
		pricing := CostPricingUnknownUsage
		if r.opts.Price == nil {
			pricing = CostPricingNoPriceFunc
		}
		r.opts.Period.ObserveAt(CostRecord{
			CostKey:    costEntry.CostKey,
			Model:      ledgerModel,
			Usage:      res.Usage,
			UsageKnown: res.UsageKnown,
			Pricing:    pricing,
		}, r.now())
	}

	// ===== 5. 写完助手消息, 再发终态 (R-H4 不变量 2) =====
	assistantRec := AssistantRecord{
		OwnerID:     res.OwnerID,
		SessionID:   req.SessionID,
		TurnID:      req.TurnID,
		EpisodeID:   req.EpisodeID,
		Blocks:      res.Blocks,
		Attachments: req.Attachments,
		Usage:       res.Usage,
		UsageKnown:  res.UsageKnown,
		Terminal:    res.Terminal,
		Citations:   res.Citations,
		Safety:      res.Safety,
		Speaker:     req.Speaker,
		Sensitivity: req.Sensitivity,
		Model:       model,
		CreatedAt:   r.now(),
	}
	if r.opts.Sink != nil {
		if err := r.opts.Sink.AppendAssistant(ctx, assistantRec); err != nil {
			// 已流出的文本不丢: res 里已经带着它。业务层可据此重试持久化。
			res.Persisted = false
			res.Degraded = append(res.Degraded, "assistant_persist_failed")
			_ = r.opts.Sink.MarkTurn(ctx, TurnState{
				OwnerID: res.OwnerID, SessionID: req.SessionID, TurnID: req.TurnID,
				State: terminalState(res.Terminal), Persisted: false,
				Reason: err.Error(), UpdatedAt: r.now(),
			})
			emitTerminal()
			return res, NewError(ErrKindSink, req.TurnID, err)
		}
		res.Persisted = true
		// MarkTurn 失败**不**改变 TurnResult.Persisted: 消息本身已经落盘,
		// 失败的是状态标记。把它混为一谈会让业务层误以为要重发消息。
		if err := r.opts.Sink.MarkTurn(ctx, TurnState{
			OwnerID: res.OwnerID, SessionID: req.SessionID, TurnID: req.TurnID,
			State: terminalState(res.Terminal), Persisted: true, UpdatedAt: r.now(),
		}); err != nil {
			res.Degraded = append(res.Degraded, "mark_turn_failed")
		}
	}

	if len(res.Degraded) > 0 {
		emitEvent(TurnEvent{Kind: EventDegraded, Note: res.Degraded[len(res.Degraded)-1]})
	}
	emitTerminal()

	// ===== 6. 错误分类 (Q-H7) =====
	return res, classifyTerminal(res)
}

// resolveModel 按角色解析本轮模型 (R-H17)。
//
// 优先级: TurnPolicy.ModelRoles[role] > RunnerOptions.RoleModels[role] > engine 默认。
// 任何一层命中都记录到日志 —— "不允许静默换模型"意味着路由变化必须可查。
func (r *Runner) resolveModel(req TurnRequest) (string, error) {
	role := req.Role
	if role == "" {
		role = RoleDialogue
	}
	model := req.Policy.ModelRoles[role]
	if model == "" {
		model = r.opts.RoleModels[role]
	}
	if model == "" {
		// 走 engine 默认: 返回空串, 让 queryLoop 用 Config.Model。
		return "", nil
	}
	if !knownRole(role) {
		// 未知角色不静默当成 dialogue: 那会让一个拼错的角色名悄悄走强模型,
		// 成本归集也会把它记到对话上。
		return "", fmt.Errorf("harness: 未知角色 %q（合法值: %s/%s/%s/%s）",
			role, RoleDialogue, RoleExtract, RoleReflect, RoleClassify)
	}
	logging.For("harness").Info("角色路由生效", "role", role, "model", model, "turn", req.TurnID)
	return model, nil
}

// exceededPeriod 从窗口视图里挑出拒绝理由。
//
// 返回值一非空表示"已知下界已超限"——即使 Known=false 也要拒绝：Spent 只累加
// 已计价部分，它都超了，真实花费只会更多。返回值二是"金额未知但未证明超限"的
// 窗口标签：调用方放行，但必须把这件事写进 Degraded。
func exceededPeriod(statuses []CostPeriodStatus) (string, []string) {
	var unknown []string
	for _, s := range statuses {
		if s.Over {
			label := s.Label
			if label == "" {
				label = s.Window.String()
			}
			return label, unknown
		}
		if !s.Known {
			label := s.Label
			if label == "" {
				label = s.Window.String()
			}
			unknown = append(unknown, label)
		}
	}
	return "", unknown
}

func knownRole(role string) bool {
	switch role {
	case RoleDialogue, RoleExtract, RoleReflect, RoleClassify:
		return true
	}
	return false
}

// normalizedRole 把空角色落到 dialogue，与 resolveModel 的口径一致。
func normalizedRole(role string) string {
	if role == "" {
		return RoleDialogue
	}
	return role
}

// ErrSafetyBlocked 表示业务层给出的安全判定是 block（R-H3）。
//
// 单独一个哨兵：业务层与日志都需要把"被安全策略拦下"与"模型调用失败"分开。
// 前者不该重试，也不该计费。
var ErrSafetyBlocked = errors.New("harness: 业务安全判定拒绝本轮")

// applySafety 执行 TurnRequest.Safety 的拦截语义。
//
// allow / 空 / degrade 放行；block 拒绝；未知 decision **失败关闭** —— 一个
// 拼错的 "deny" 如果被当成放行，安全 gate 就静默失效了，而这正是它存在的理由。
func applySafety(v SafetyVerdict) error {
	switch strings.ToLower(strings.TrimSpace(v.Decision)) {
	case "", "allow", "degrade":
		return nil
	case "block":
		detail := strings.Join(v.Reasons, "; ")
		if detail == "" {
			detail = strings.Join(v.Categories, "; ")
		}
		if detail == "" {
			return ErrSafetyBlocked
		}
		return fmt.Errorf("%w: %s", ErrSafetyBlocked, detail)
	default:
		return fmt.Errorf("%w: 未知 safety decision %q", ErrSafetyBlocked, v.Decision)
	}
}

// isPartialTerminal 判断该终态下已经产出的文本是否只能算"部分内容"。
func isPartialTerminal(t types.Terminal) bool {
	switch t.Reason {
	case "", "completed":
		return false
	}
	return true
}

// enter 占用 session 与 run 槽位。
func (r *Runner) enter(ctx context.Context, req TurnRequest) (context.Context, context.CancelFunc, error) {
	runCtx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	if req.SessionID != "" {
		if holder, busy := r.sessions[req.SessionID]; busy && holder != req.TurnID {
			cancel()
			return nil, nil, fmt.Errorf("%w: session=%q turn=%q", ErrSessionBusy, req.SessionID, holder)
		}
		r.sessions[req.SessionID] = req.TurnID
	}
	r.running[req.TurnID] = &runningTurn{cancel: cancel, session: req.SessionID}
	return runCtx, cancel, nil
}

func (r *Runner) leave(turnID string, cancel context.CancelFunc) {
	cancel()
	r.mu.Lock()
	if rt, ok := r.running[turnID]; ok {
		if rt.session != "" && r.sessions[rt.session] == turnID {
			delete(r.sessions, rt.session)
		}
		delete(r.running, turnID)
	}
	r.mu.Unlock()
}

// markBudget 记录"这次取消是预算触发", 供终态归因。
func (r *Runner) markBudget(turnID, reason string) {
	r.mu.Lock()
	if rt, ok := r.running[turnID]; ok {
		rt.budgetReason = reason
	}
	r.mu.Unlock()
}

// Cancel 取消一个在途 turn。未知 turnID 不报错 (幂等): 调用方可能取消一个
// 刚好已经结束的 turn, 那不该变成失败。
func (r *Runner) Cancel(turnID string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	rt, ok := r.running[turnID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	rt.cancel()
	return nil
}

// drain 消费 streamCh 并填充 res, 同时向宿主发事件, 并把每次 usage 交给预算闸。
//
// 为什么把"填 res"和"发事件"放在同一处: 两者必须看到同一份数据。分成两个
// goroutine 会出现"事件里说有引用、结果里没有"这种不一致, 而它只在慢宿主上现形。
//
// now 是可注入的时钟; nil 时用 time.Now。只用于 provenance 的执行时刻 (R-H9) ——
// 它是"这条结论是哪次读取得来的"里的时间那一半, 用真实墙钟而不是零值, 否则
// 宿主侧按时间排序会把所有工具调用归到同一刻。
func drain(ctx context.Context, streamCh <-chan types.StreamEvent, emit func(TurnEvent), res *TurnResult, gate *budgetGate, now func() time.Time,
	sink ToolResultSink, ownerID, sessionID, turnID string) {
	if now == nil {
		now = time.Now
	}
	var textSB []byte
	// sinkFailed 保证同一个工具的 sink 失败只记一次 Degraded：一次 turn 里
	// 同一个工具可能被调用几十次，逐次记会把降级列表刷成噪音，而"证据落库
	// 坏了"这一事实只需要被宿主知道一次。
	sinkFailed := make(map[string]bool, 4)
	// openTools 是尚未闭合的工具调用。ToolStart 时登记, ToolDone 时按
	// **ToolUseID** 精确配对 (无 ID 时退回按名字配对最早的一条)。
	//
	// 为什么优先用 ID: 同一轮里模型可以并发调两个同名工具 (两个 Read、两个 Bash)。
	// 只按名字配对时,"开始"与"完成"会被配成任意一对 —— 耗时记到另一个调用上、
	// 结果预览张冠李戴, 而两边都不报错。ID 是协议给的唯一身份。
	openTools := make([]*ToolCallRecord, 0, 4)
	// startedAt 按记录指针存, 不放在 ToolCallRecord 上: 它是 harness 的本地时钟,
	// 不是要落库的字段 (DurationMS 才是)。
	startedAt := make(map[*ToolCallRecord]time.Time, 4)
	for ev := range streamCh {
		switch ev.Kind {
		case types.StreamEventDelta:
			if ev.IsThinking {
				continue // 思考链不进最终文本, 与 CLI 的 final 口径一致
			}
			textSB = append(textSB, ev.DeltaText...)
			emit(TurnEvent{Kind: EventDelta, Delta: ev.DeltaText, BlockIndex: ev.BlockIndex})
		case types.StreamEventToolStart:
			rec := &ToolCallRecord{Name: ev.ToolName, ToolUseID: ev.ToolUseID}
			if ev.ToolInput != "" {
				rec.Input = []byte(ev.ToolInput)
				rec.InputHash = hashText(ev.ToolInput)
			}
			openTools = append(openTools, rec)
			startedAt[rec] = now()
			if gate != nil && gate.observeToolCall() {
				// 闸门在工具**开始**处计数并取消: 这一条工具还来得及被拦,
				// 而在 ToolDone 处计数只能事后追认。
				emit(TurnEvent{Kind: EventDegraded, Note: "budget_exhausted:tool_calls"})
			}
			emit(TurnEvent{Kind: EventToolStart, Tool: &ToolEvent{
				ToolUseID:    ev.ToolUseID,
				Name:         ev.ToolName,
				InputPreview: previewBytes(ev.ToolInput, toolPreviewBytes),
			}})
		case types.StreamEventToolDone:
			idx := matchOpenTool(openTools, ev.ToolUseID, ev.ToolName)
			if idx < 0 {
				// 没配到任何未闭合调用: 上游给了一条没有开始的完成事件。
				// 仍然要报事件 (宿主需要知道工具结束了), 但不编一条 ToolCallRecord ——
				// 凭空补一条会让 res.ToolCalls 里出现没有输入的幽灵调用。
				emit(TurnEvent{Kind: EventToolDone, Tool: &ToolEvent{ToolUseID: ev.ToolUseID, Name: ev.ToolName}})
				continue
			}
			rec := openTools[idx]
			rec.IsError = strings.HasPrefix(ev.ToolResult, "[error]")
			// 全文优先；旧生产者只填 200B 摘要时退回摘要，行为与改造前一致。
			//
			// 全文**不**写进 ToolCallRecord（trace 膨胀），只用于预览、字节数
			// 与 ToolResultSink。
			full := ev.ToolResultFull
			if full == "" {
				full = ev.ToolResult
			}
			rec.ResultPreview = previewBytes(full, toolPreviewBytes)
			rec.ResultBytes = len(full)
			rec.ResultTruncated = len(full) > toolPreviewBytes
			rec.Trust = ev.ToolTrust
			rec.SourceURI = ev.ToolSource
			// provenance (R-H9): 工具名 + 参数哈希 + 执行时刻。缺这一段的话,
			// 宿主无法回答"这条结论是哪次读取得来的", 也无法在做证据审计时
			// 把同一工具的不同调用区分开。
			if start, ok := startedAt[rec]; ok {
				rec.DurationMS = now().Sub(start).Milliseconds()
			}
			rec.Provenance = Provenance{
				Tool:       rec.Name,
				SourceID:   rec.ToolUseID,
				ExecutedAt: startedAt[rec],
			}
			res.ToolCalls = append(res.ToolCalls, *rec)
			delete(startedAt, rec)
			openTools = append(openTools[:idx], openTools[idx+1:]...)

			// 先把完成事件发给 UI，再落证据：sink 是宿主存储，可能慢；让事件
			// 等待存储会让"工具已经完成"的可观测性取决于存储可用性。
			emit(TurnEvent{Kind: EventToolDone, Tool: &ToolEvent{
				ToolUseID:     ev.ToolUseID,
				Name:          ev.ToolName,
				ResultPreview: rec.ResultPreview,
				Trust:         ev.ToolTrust,
				SourceURI:     ev.ToolSource,
			}})

			// H-F1: 完整结果交给宿主。失败**不**让 turn 失败 —— 证据账本是
			// 增强能力；只记一次 Degraded，供业务层知道"这轮证据没落全"。
			if sink != nil {
				srec := ToolResultRecord{
					OwnerID:    ownerID,
					SessionID:  sessionID,
					TurnID:     turnID,
					ToolUseID:  rec.ToolUseID,
					Name:       rec.Name,
					Input:      rec.Input,
					InputHash:  rec.InputHash,
					FullResult: full,
					IsError:    rec.IsError,
					DurationMS: rec.DurationMS,
					Trust:      ev.ToolTrust,
					SourceURI:  ev.ToolSource,
					ExecutedAt: rec.Provenance.ExecutedAt,
				}
				if err := sink.AppendToolResult(ctx, srec); err != nil && !sinkFailed[rec.Name] {
					sinkFailed[rec.Name] = true
					res.Degraded = appendUnique(res.Degraded, "tool_result_sink_failed:"+rec.Name)
				}
			}
		case types.StreamEventMessageDone:
			if ev.Message != nil {
				res.Blocks = ev.Message.Content
				if ev.Message.Usage != nil {
					res.Usage = *ev.Message.Usage
					res.UsageKnown = true
					if gate != nil && gate.observeUsage(res.Usage) {
						emit(TurnEvent{Kind: EventDegraded, Note: "budget_exhausted:cost"})
					}
				}
				if res.Text == "" {
					res.Text = textOf(ev.Message.Content)
				}
				// 逐块收尾（R-H12）：消费方按 block_done 定格一个块。
				// 改造前只发 message_done，前端无法在"整条消息完成"之前
				// 结束某一块的流式状态（引用角标也挂不上）。
				for i := range ev.Message.Content {
					emit(TurnEvent{Kind: EventBlockDone, BlockIndex: i})
				}
			}
			emit(TurnEvent{Kind: EventMessageDone})
		case types.StreamEventError:
			if ev.Error != nil {
				res.Terminal = types.Terminal{Reason: "error", Error: ev.Error}
				emit(TurnEvent{Kind: EventError, Err: ev.Error.Error()})
			}
		}
	}
	res.Text = string(textSB)
	// 收尾时仍未闭合的工具调用也要报出来 —— 静默丢弃会让"模型调了工具但失败了"
	// 在结果里变成"模型没调工具"。
	for _, rec := range openTools {
		// 未闭合的记录没有完成时刻, 但它**开始过**: provenance 里给开始时间
		// 而不是零值, 否则审计时"这条调用是什么时候发起的"直接丢失。
		rec.Provenance = Provenance{Tool: rec.Name, SourceID: rec.ToolUseID, ExecutedAt: startedAt[rec]}
		if start, ok := startedAt[rec]; ok {
			rec.DurationMS = now().Sub(start).Milliseconds()
		}
		res.ToolCalls = append(res.ToolCalls, *rec)
	}
	if res.UsageKnown {
		emit(TurnEvent{Kind: EventUsage, Usage: &res.Usage})
	}
}

// matchOpenTool 为一条 ToolDone 事件找它对应的未闭合记录。
//
// 优先按 ToolUseID 精确匹配；ID 为空 (XML 回退路径、老上游) 时退回"第一个同名
// 记录"。返回 -1 表示没配到。
//
// 为什么必须区分这两种匹配: 有 ID 却按名字配会把并发同名调用错配, 而错配是
// 静默的 (记录数、事件数、成功/失败都对得上, 只有耗时和预览张冠李戴)。
func matchOpenTool(open []*ToolCallRecord, toolUseID, name string) int {
	if toolUseID != "" {
		for i, rec := range open {
			if rec.ToolUseID == toolUseID {
				return i
			}
		}
		// 有 ID 但没有任何记录带着它: 这是一条不属于当前未闭合集合的完成事件
		// (可能 ToolStart 在压缩/裁剪中被丢掉)。**不要**退回按名字配对 ——
		// 那会把这条结果嫁接到另一个同名调用上, 制造出一条假记录。
		return -1
	}
	for i, rec := range open {
		if rec.Name == name {
			return i
		}
	}
	return -1
}

// textOf 拼接内容块里的文本, 用于 delta 为空但 MessageDone 带了完整内容的情况。
func textOf(blocks []types.ContentBlock) string {
	var out []byte
	for _, b := range blocks {
		if b.Type == types.ContentBlockText {
			out = append(out, b.Text...)
		}
	}
	return string(out)
}

// splitInput 把内容块拆成 engine 的 (userContent, extraBlocks)。
func splitInput(blocks []types.ContentBlock) (string, []types.ContentBlock) {
	var text []byte
	extra := make([]types.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == types.ContentBlockText {
			if len(text) > 0 {
				text = append(text, '\n')
			}
			text = append(text, b.Text...)
			continue
		}
		extra = append(extra, b)
	}
	return string(text), extra
}

// terminalState 把 terminal reason 映射成 TurnState.State。
//
// interrupted 是专门为崩溃恢复留的: "用户消息已提交、助手消息未提交"的 turn
// 重启后不能假装完成, 也不能自动重放 (会重复或产生不同回复)。
func terminalState(t types.Terminal) string {
	switch t.Reason {
	case "completed", "":
		return "completed"
	case "cancelled", "aborted", "aborted_streaming":
		return "interrupted"
	default:
		return "failed"
	}
}

// classifyTerminal 把终态翻译成分类错误。
func classifyTerminal(res *TurnResult) error {
	if res.Terminal.Error != nil {
		return NewError(terminalErrKind(res.Terminal.Reason), res.TurnID, res.Terminal.Error)
	}
	switch res.Terminal.Reason {
	case "completed", "":
		return nil
	case "cancelled", "aborted", "aborted_streaming":
		return NewError(ErrKindCanceled, res.TurnID,
			fmt.Errorf("turn %s %s", res.TurnID, res.Terminal.Reason))
	case "budget_exhausted":
		return NewError(ErrKindBudget, res.TurnID,
			fmt.Errorf("turn %s 预算耗尽", res.TurnID))
	case "max_turns":
		return NewError(ErrKindBudget, res.TurnID,
			fmt.Errorf("turn %s 达到轮数上限", res.TurnID))
	case "circuit_breaker", "model_error":
		return NewError(ErrKindModel, res.TurnID,
			fmt.Errorf("turn %s 终止原因 %q", res.TurnID, res.Terminal.Reason))
	case "context_critical":
		return NewError(ErrKindContext, res.TurnID, res.Terminal.Error)
	default:
		return NewError(ErrKindInternal, res.TurnID,
			fmt.Errorf("turn %s 终止原因 %q", res.TurnID, res.Terminal.Reason))
	}
}

// terminalErrKind 把 terminal reason 映射成错误分类。
func terminalErrKind(reason string) ErrorKind {
	switch reason {
	case "cancelled", "aborted", "aborted_streaming":
		return ErrKindCanceled
	case "safety_blocked":
		return ErrKindSafety
	case "budget_exhausted", "max_turns":
		return ErrKindBudget
	case "context_critical":
		return ErrKindContext
	case "error", "model_error", "circuit_breaker":
		return ErrKindModel
	default:
		return ErrKindInternal
	}
}

// CountRunning 返回当前在途 turn 数（观测与测试用）。
func (r *Runner) CountRunning() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running)
}
