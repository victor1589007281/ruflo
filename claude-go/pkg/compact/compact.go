// Package compact 实现上下文压缩 (context compaction) 系统。
// 对应 TS 源码: review/claude/src/services/compact/compact.ts
//
//	review/claude/src/services/compact/autoCompact.ts
//	review/claude/src/services/compact/microCompact.ts
//
// 上下文压缩是 Claude Code 处理长对话的关键机制。
// 当对话 token 数接近模型上下文窗口限制时，
// 系统会调用模型将历史对话压缩成简洁的摘要。
//
// 压缩层次:
//  1. MicroCompact - 细粒度: 截断过大的工具输出 (如大文件内容)
//  2. AutoCompact - 粗粒度: 将整段对话历史压缩为摘要
//  3. ReactiveCompact - 应急: 当 API 返回 prompt_too_long 时触发
//
// 压缩后的消息结构:
//
//	[CompactBoundaryMessage, SummaryMessage, ...recent_messages]
//	CompactBoundary 标记压缩点，之前的消息被摘要替代。
package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/textutil"
	"github.com/anthropic/claude-go/pkg/types"
)

// TokenThreshold 触发自动压缩的 token 阈值比例 (当前 context 占比)
const TokenThreshold = 0.8

// CompactMaxOutputTokens 压缩请求的最大输出 token
const CompactMaxOutputTokens = 8192

// Compactor 上下文压缩器
//
// # 13.6-P1 F6: compaction 事件锁 (docforge planning-dsh-adopt)
//
// 规划语义: 「压缩期间事件写入阻塞排队，杜绝折叠竞态」。此前 pkg/compact 与引擎
// 主循环只有时序约定 (同 goroutine 内顺序执行), 无显式锁语义 —— 跨 goroutine 的
// 事件落盘 (llm_call/node Span 来自团队并发路径) 与折叠窗口交错时, 轨迹上会出现
// 折叠后事件链与摘要边界错序的窗口。现在把该约定物化为显式锁:
//
//   - 折叠窗口: runCompaction 全程 (含 PreCompact 蒸馏与摘要 LLM 调用) 持
//     eventMu 写锁; 并发折叠在同锁上自然串行。
//   - 事件写入门: GateEventWrite 让"折叠后才有意义的事件写入"在窗口内阻塞排队,
//     窗口关闭后按 Lock 顺序放行 —— 事件链要么完整在折叠前, 要么完整在折叠后,
//     不会横跨折叠边界。
//
// Compactor 是 per-engine 实例 (feishu 主会话/嵌套代理/团队 runner 各一份), 锁
// 作用域即单引擎, 与 feishu per-chat busy 串行化正交。存储层 (tracestore/
// statestore) 本身线程安全, 此锁补的是顺序性语义而非数据损坏防护。
type Compactor struct {
	apiClient        *api.Client
	maxContextTokens int // 模型最大上下文窗口

	// refStore 折叠原文的落盘存储 (可为 nil = 不落盘, 退化为纯截断)。
	//
	// 与 Compact 路径共用: 折叠边界那次 StubToolResults 也要给引用, 否则
	// 被摘要掉的那段历史里的大 tool_result 就永久丢了 —— 而那恰恰是最需要
	// 回读的一段 (模型刚据它做过推理)。
	refStore *ToolResultRefStore

	// eventMu 折叠窗口锁 (F6): runCompaction 持写锁; 事件写入侧经
	// GateEventWrite 持读锁阻塞排队。RWMutex 而非 Mutex: 无折叠时事件写入
	// 零阻塞 (读锁共享), 只有窗口真正打开时才出现排队。
	eventMu      sync.RWMutex
	compacting   atomic.Bool  // 窗口状态位: 写锁内置位, Compacting() 快照精确
	compactions  atomic.Int64 // 折叠窗口累计打开次数 (观测用)
	eventsQueued atomic.Int64 // 事件写入过门总次数 (观测用; 排队与否见窗口期时延)

	// PreCompact 蒸馏: 压缩前将关键信息提取到持久记忆
	PreCompactFn func(facts []string, source string)

	// onCompact 每次**真的发生**折叠后回调一次 (H2-1 压缩可观测)。
	//
	// 由 Compactor 持有而不是让调用方从返回值里取: 折叠的调用点有三个
	// (PhasePreCompact 的 AutoCompactHook / PhaseOnRecovery 的 PTL 应急压缩 /
	// queryLoop 直调), 让每个调用点各自去落轨迹, 漏一处就是一处静默黑洞。
	// 一个回调把"折叠发生了"这个事实收口到唯一出口。
	//
	// **回调在折叠窗口关闭之后触发** (见 AutoCompact): 窗口内持写锁, 而事件落盘
	// 走 GateEventWrite 取读锁, 窗口内回调必死锁 —— 这正是 F6 注释里那条"窗口内
	// 绝不调 GateEventWrite"纪律的回调版本。
	onCompact func(ctx context.Context, st CompactionStats)
}

// StubCounts 一次 tool_result 折叠的计数。
//
// 分开数"折了几条"和"其中几条给出了可回读引用": 两者的差就是**永久丢失**的
// 结果条数。只报前者的化, "折叠成功"与"折叠且信息无损"读数相同。
type StubCounts struct {
	// Folded 被折成占位的 tool_result 条数。
	Folded int
	// Refs 其中成功落盘、占位里给出了可 Read 回读路径的条数。
	Refs int
	// SkippedRefReads 因自指保护整条跳过 (既不折叠也不落盘) 的条数 ——
	// 这些是"模型正在回读折叠引用"的结果, 跳过是刻意的, 不算丢失。
	SkippedRefReads int
}

// CompactionStats 一次上下文折叠的可观测读数 (H2-1)。
//
// 存在的理由: 在此之前, 折叠路径上**没有任何一处**记录了"压了多少、省了多少"。
// 长程任务里模型"忘了前面做过什么"时, 无法区分是摘要写得差还是折叠根本没发生;
// 上下文预算打满时, 也说不清 token 是被折叠省下来的还是被工具结果吃掉的。
// 这几个数就是那两个问题的最小充分集。
type CompactionStats struct {
	// MessagesBefore / MessagesAfter 折叠前后的消息条数。
	MessagesBefore int
	MessagesAfter  int
	// MessagesCompacted 被摘要替代的条数 (= MessagesBefore - 尾部保护条数)。
	// 与 MessagesBefore-MessagesAfter 不等: 折叠**净增** 2 条 (boundary + summary)。
	MessagesCompacted int

	// TokensBefore / TokensAfter 折叠前后的估算 token 数 (口径同 estimateTokens)。
	// TokensAfter 在 tool_result 折叠**之后**测 —— 否则报的是"摘要换历史"那一步的
	// 效果, 漏掉同一次折叠里 tool_result 那一大块。
	TokensBefore int
	TokensAfter  int
	// SummaryTokens 摘要正文自身的 token 数 (chars/4, 同口径)。
	SummaryTokens int

	// TailCount 被尾部保护、未参与折叠的条数。
	TailCount int

	// ===== H2-2 硬约束自检 =====
	//
	// Constraints 从**被折叠的原文**里抽出的硬约束条数; ConstraintsVerifiable
	// 其中可机械核验的条数 (抽不出文件路径/反引号标识符的不可核验)。
	// ConstraintsMissing 重压之后仍然没保住的条数。
	//
	// **Verifiable=0 且 Constraints>0** 是一个明确的读数: 这次折叠确实带约束,
	// 但判据抓不住它们 —— 那种情况下"核验通过"是假的。三个数一起看才不含糊。
	Constraints           int
	ConstraintsVerifiable int
	ConstraintsMissing    int
	// Recompacted 是否因约束丢失触发过重压 (至多一次)。
	Recompacted bool

	// Stub 折叠边界上 tool_result 的折叠计数。
	//
	// **当前恒为 {0 0 0}**: 折叠边界的产物是 [boundary, summary, ...tail], 其中
	// [0, len(result)-len(tail)) 区间**只有 boundary 与 summary 两条**, 不含任何
	// tool_result —— 于是 runCompaction 里那次 StubToolResultsWithRef 实际是空转。
	// 被摘要掉的历史里的 tool_result 不是"折叠后留下引用", 而是**随历史一起消失**。
	// 保留这个字段是为了让这件事在轨迹上可见 (而不是靠读代码才发现), 详见
	// runCompaction 末尾的说明。
	Stub StubCounts
}

// Compacting 报告折叠窗口当前是否打开 (测试/观测用; 快照语义, 返回后可能变化)。
// 状态位在写锁内置位/清除, 与窗口开关精确同步。
func (c *Compactor) Compacting() bool {
	if c == nil {
		return false
	}
	return c.compacting.Load()
}

// Stats 返回 F6 锁的观测计数: compactions=折叠窗口打开总次数,
// events_queued=事件写入过门总次数。
func (c *Compactor) Stats() (compactions, eventsQueued int64) {
	if c == nil {
		return 0, 0
	}
	return c.compactions.Load(), c.eventsQueued.Load()
}

// GateEventWrite 把事件写入 fn 包进折叠窗口门: 窗口打开时阻塞排队, 关闭后立即
// 通过; 与进行中的折叠互斥 (读锁 vs 写锁)。
//
// fn 纪律: 只做事件落盘, 不得再经 GateEventWrite 嵌套取门, 也不得触发折叠 ——
// 否则同一 goroutine 读锁内再取写锁将死锁。fail-open: c 为 nil (未装配压缩器,
// 例如 RunIsolated 的独立小循环) 时直接执行, 与无锁语义等价。
func (c *Compactor) GateEventWrite(fn func()) {
	if c == nil || fn == nil {
		if fn != nil {
			fn()
		}
		return
	}
	c.eventMu.RLock()
	defer c.eventMu.RUnlock()
	c.eventsQueued.Add(1) // 无竞态误差: 未阻塞时也计一次"过门", 排队与否见窗口期时延
	fn()
}

// NewCompactor 创建压缩器
func NewCompactor(apiClient *api.Client, maxContextTokens int) *Compactor {
	if maxContextTokens == 0 {
		maxContextTokens = 200000 // 默认 200k
	}
	return &Compactor{
		apiClient:        apiClient,
		maxContextTokens: maxContextTokens,
	}
}

// AutoCompact 自动压缩: 当 token 使用量超过阈值时触发。
// 对应 TS: services/compact/autoCompact.ts 中的自动压缩逻辑
//
// 算法:
//  1. 估算当前 token 数 (简化: 按字符数 / 4)
//  2. 如果低于阈值 → 不压缩
//  3. 如果超过阈值 → 调用 runCompaction()
//  4. 返回 [boundary, summary, ...recent_tail]
func (c *Compactor) AutoCompact(ctx context.Context, messages []types.Message, model string) ([]types.Message, error) {
	estimatedTokens := estimateTokens(messages)
	threshold := int(float64(c.maxContextTokens) * TokenThreshold)

	if estimatedTokens < threshold {
		return nil, nil // 无需压缩
	}

	return c.runCompaction(ctx, messages, model)
}

// SetOnCompact 装配折叠回调 (H2-1 压缩可观测)。
//
// **回调在折叠窗口关闭之后、同一 goroutine 上同步触发**。实现方可以安全地走
// GateEventWrite 落轨迹; 反过来, 实现方**不得**在回调里再触发一次折叠 (同
// goroutine 重入写锁)。回调为 nil 时零开销, 行为与改造前逐字节相同。
func (c *Compactor) SetOnCompact(fn func(ctx context.Context, st CompactionStats)) {
	if c == nil {
		return
	}
	c.onCompact = fn
}

// fireOnCompact 触发回调 (nil 安全)。
func (c *Compactor) fireOnCompact(ctx context.Context, st CompactionStats) {
	if c == nil || c.onCompact == nil {
		return
	}
	c.onCompact(ctx, st)
}

// runCompaction 执行实际的上下文压缩。
// 对应 TS: services/compact/compact.ts 中的核心压缩逻辑
//
// 压缩策略:
//  1. 保留最近 N 条消息 (尾部保护)
//  2. 将其余消息发送给模型, 要求生成摘要
//  3. 构建新的消息序列: [boundary, summary_user_msg, ...tail]
//
// 全程处于 F6 折叠窗口 (eventMu 写锁) 内。
// SetPreCompactFn 设置 PreCompact 蒸馏回调
func (c *Compactor) SetPreCompactFn(fn func(facts []string, source string)) {
	c.PreCompactFn = fn
}

// runCompaction 是折叠的对外形状: 开窗口 → 折叠 → **关窗口之后**才报数。
//
// 拆成两层不是为了复用, 是为了让"H2-1 的回调在窗口外"这件事**结构上成立**
// 而不是靠注释约定: 窗口的关闭是 defer 在 compactLocked 返回时才发生, 任何写在
// compactLocked 里的回调都会持着写锁跑。见 SetOnCompact 的纪律。
func (c *Compactor) runCompaction(ctx context.Context, messages []types.Message, model string) ([]types.Message, error) {
	out, st, err := c.compactLocked(ctx, messages, model)
	if err != nil || out == nil {
		return out, err // 没折成不报数: "报了一条 0 收益的折叠"比不报更坏
	}
	c.fireOnCompact(ctx, st)
	return out, nil
}

func (c *Compactor) compactLocked(ctx context.Context, messages []types.Message, model string) ([]types.Message, CompactionStats, error) {
	st := CompactionStats{
		MessagesBefore: len(messages),
		TokensBefore:   estimateTokens(messages),
	}
	// F6 折叠窗口: 写锁自蒸馏起、至摘要落定止。窗口内事件写入侧 (GateEventWrite)
	// 阻塞排队, 折叠完成后按序放行 —— 折叠竞态在此显式收口。写锁不可重入, 故
	// 窗口内绝不调 GateEventWrite, PreCompactFn 实现方亦不得 (见其纪律注释)。
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.compacting.Store(true)
	c.compactions.Add(1)
	defer c.compacting.Store(false)

	// PreCompact 蒸馏: 在压缩前提取关键信息到持久记忆
	if c.PreCompactFn != nil {
		facts := c.SmartExtractKeyFacts(ctx, messages)
		if len(facts) > 0 {
			c.PreCompactFn(facts, "pre_compact")
		}
	}

	// 保护尾部 (最近 6 条消息不被压缩, 从 4 提升)
	tailCount := 6
	if tailCount > len(messages) {
		tailCount = len(messages)
	}

	st.TailCount = tailCount

	if len(messages) <= tailCount {
		return nil, st, nil // 消息太少，无需压缩
	}

	st.MessagesCompacted = len(messages) - tailCount
	toCompress := messages[:len(messages)-tailCount]
	tail := messages[len(messages)-tailCount:]

	// H2-2 硬约束自检: 先在**尚未折叠**的原文上抽出约束, 再拿摘要比对。
	// 顺序不能反 —— 折叠一旦发生, 判据 (原句与判别 token) 就没了。
	constraints := ExtractHardConstraints(toCompress)
	st.Constraints = len(constraints)
	st.ConstraintsVerifiable = 0
	for _, c := range constraints {
		if c.Verifiable() {
			st.ConstraintsVerifiable++
		}
	}

	summaryText, err := c.summarize(ctx, toCompress, nil)
	if err != nil {
		return nil, st, fmt.Errorf("压缩请求失败: %w", err)
	}

	// 核验 → 不合格重压**一次**。
	//
	// 只重压一次是刻意的: 第二次仍缺说明"模型认为这条不该进摘要"或判据本身过严,
	// 再试是拿线性成本换一个边际为负的收益。单次重压的代价 = 一次摘要请求, 而它
	// 换的是"这条 run 后面 40 轮还能记得不许改哪个文件" —— 那笔账在长程任务上
	// 是划算的, 第二次就不一定了。
	if len(constraints) > 0 {
		before := CheckConstraints(summaryText, constraints)
		st.ConstraintsMissing = len(before.Missing)
		if len(before.Missing) > 0 {
			st.Recompacted = true
			retryText, rerr := c.summarize(ctx, toCompress, before.Missing)
			// 采纳重压结果**当且仅当它确实更全**。
			//
			// 无条件采纳是错的: 模型在"被要求补上某几条"时可能整篇重新组织,
			// 顺带把别的已经保住的约束挤掉 —— 那样这次重压是净损失, 而只看
			// "Missing 里那几条补上了没有"的判据会放过它。这里比的是**总量**。
			if rerr == nil && retryText != "" {
				after := CheckConstraints(retryText, constraints)
				if len(after.Missing) < len(before.Missing) {
					summaryText = retryText
					st.ConstraintsMissing = len(after.Missing)
				}
			}
		}
	}

	st.SummaryTokens = len(summaryText) / 4 // 与 estimateTokens 同口径

	// 构建压缩后的消息序列
	result := []types.Message{
		{
			Type:              types.MessageTypeSystem,
			UUID:              fmt.Sprintf("compact-%d", len(messages)),
			Content:           []types.ContentBlock{{Type: types.ContentBlockText, Text: "[Context compacted]"}},
			IsCompactBoundary: true,
		},
		{
			Type: types.MessageTypeUser,
			UUID: fmt.Sprintf("summary-%d", len(messages)),
			Content: []types.ContentBlock{{
				Type: types.ContentBlockText,
				Text: fmt.Sprintf("<context_summary>\n%s\n</context_summary>", summaryText),
			}},
		},
	}
	result = append(result, tail...)

	// 折叠尾部之前的全部 tool_result。
	//
	// 这一步只能在这里做, 不能放到每轮的 MicroCompact 里: 折叠边界是**一次性**
	// 事件, 之后这段前缀永不改动; 而每轮都跑就等于每轮都有机会产出不同字节。
	// 折叠产物是 call_id 的纯函数 (见 ToolResultStub), 所以即便将来再次经过
	// 这个边界, 结果也逐字节相同。
	//
	// 范围 [0, len(result)-tailCount): 摘要本身是 user 文本块, 不受影响; 尾部
	// 保护的 6 条保持原样 —— 它们是模型当下正在推理的上下文, 折掉会伤质量。
	// 自指保护 (本轮有人在读引用就不落盘) 在 StubToolResultsWithRef 内统一生效。
	//
	// H2-1 起改用 Counted 版本, 把"折了几条 / 其中几条给出引用"数进统计。**这里
	// 的计数当前恒为 0**: result 的该区间只有 boundary 与 summary 两条, 都不含
	// tool_result。也就是说, 被摘要掉的历史里的 tool_result 不是"折叠后留下可回读
	// 的引用", 而是随历史一起消失 —— 引用机制只在 MicroCompact 那条路上生效。
	// 计数摆在这里正是为了让这件事在轨迹上直接读出来, 而不是靠读代码才发现。
	result, st.Stub = StubToolResultsCounted(result, 0, len(result)-len(tail), c.refStore)

	st.MessagesAfter = len(result)
	st.TokensAfter = estimateTokens(result)
	return result, st, nil
}

// summarize 发一次摘要请求。
//
// retryMissing 非空时是**重压**: 提示词里逐字列出上一次漏掉的硬约束 (H2-2)。
// 两次调用共用本函数而不是各写一段拼接 —— 提示词的公共部分 (要求摘要保留文件
// 路径与函数名) 一旦分叉, "重压是不是在同一个口径下比出来的"就说不清了。
//
// 历史正文每轮都要重新拼接: 折叠是低频事件 (一次 run 一两次), 为它缓存一份
// 几万字符的中间串不值得, 而缓存住反而会让"折的是哪一份历史"变成隐式状态。
func (c *Compactor) summarize(ctx context.Context, toCompress []types.Message, retryMissing []HardConstraint) (string, error) {
	var sb strings.Builder
	// 重压时把约束块放在**最前面**: 后面的历史正文极长 (可达数万字符), 把要求
	// 压在末尾会让它在长上下文里被稀释 —— 而这正是上一次漏掉它的同一个成因。
	sb.WriteString(constraintPromptBlock(retryMissing))
	sb.WriteString("Please provide a concise summary of the following conversation. ")
	sb.WriteString("Focus on: key decisions made, files modified, current state of the task, and important context. ")
	sb.WriteString("Be specific about file paths, function names, and technical details. ")
	// 常规路径也显式提一句约束保留 —— 让重压成为**例外**而非常规路径。多数约束
	// 靠这一句就够了, 重压只兜住剩下的。
	sb.WriteString("Preserve all hard constraints and prohibitions stated by the user verbatim.\n\n")

	for _, msg := range toCompress {
		role := string(msg.Type)
		text := extractText(msg)
		if text != "" {
			sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", role, truncate(text, 2000)))
		}
	}

	compactMessages := []types.APIMessage{{
		Role:    "user",
		Content: mustMarshal([]types.ContentBlock{{Type: types.ContentBlockText, Text: sb.String()}}),
	}}

	resp, err := c.apiClient.SendMessage(ctx, compactMessages, []string{"You are a conversation summarizer."}, nil, CompactMaxOutputTokens)
	if err != nil {
		return "", err
	}
	var summaryText string
	for _, block := range resp.Content {
		if block.Type == types.ContentBlockText {
			summaryText += block.Text
		}
	}
	return summaryText, nil
}

// SetToolResultRefStore 装配折叠原文的落盘目录 (工作区路径)。
//
// 空目录 = 关闭落盘, 退回纯截断。构造后再调用即可 —— Compactor 是 per-engine
// 实例, 引擎拿到工作区 (Config.Cwd) 的时机晚于 NewCompactor。
func (c *Compactor) SetToolResultRefStore(dir string) {
	if c == nil {
		return
	}
	c.refStore = NewToolResultRefStore(dir)
}

// MicroCompactMessages 对一段**独立**消息历史做 tool_result 折叠 (H3-2)。
//
// 与主循环的 MicroCompactHook 是**同一个产物**: 都调 StubToolResultsCounted,
// 用同一个 refStore、同一组阈值常量。差异只在"折哪一段": 主循环按 SentUpTo 只折
// 本轮新增 (已发出的历史一字节都不能动, 否则前缀缓存从改写点起全废); 本方法折
// 全段 —— 隔离运行的历史是它自己的, 从头到尾没有任何别的消费者, 不存在"已发出"
// 这回事。
//
// 为什么给 RunIsolated 而不是让它自己调 StubToolResultsCounted: refStore 是
// Compactor 的私有字段, 而折叠**必须**与主循环共用同一个 refStore —— 两个 store
// 会在同一个工作区里落下两份互不知晓的引用目录, 而回读路径只认其中一个。
// 收口在这里, 调用方拿不到也无法传错。
//
// c 为 nil (未装配压缩器) 时原样返回, 与旧行为一致。
func (c *Compactor) MicroCompactMessages(messages []types.Message) ([]types.Message, StubCounts) {
	if c == nil {
		return messages, StubCounts{}
	}
	return StubToolResultsCounted(messages, 0, len(messages), c.refStore)
}

// FoldIfNeeded 在消息估算 token 超过阈值时折叠整段历史 (H3-2)。
//
// 与 AutoCompact 是同一段代码 (都走 runCompaction), 薄一层只是为了把"阈值判断
// 用的是本 Compactor 的 maxContextTokens"这件事写在 Compactor 里 —— 隔离运行
// 拿不到引擎的 contextWindow 配置, 让它自己算阈值会把同一个常量抄成两份。
//
// 返回 (nil, nil) 表示无需折叠 (与 AutoCompact 同约定)。
func (c *Compactor) FoldIfNeeded(ctx context.Context, messages []types.Message, model string) ([]types.Message, error) {
	if c == nil || len(messages) == 0 {
		return nil, nil
	}
	return c.AutoCompact(ctx, messages, model)
}

// 折叠阈值与保留量。
//
// 这两个值刻意都是**包级常量**而不是调用方传进来的参数: 折叠结果必须只由
// "这条 tool_result 本身"决定。此前保留量由调用方传 (hook 先按 16000 折一遍,
// 总长仍超 30000 再按 8000 折第二遍), 于是**同一条 tool_result 在不同轮次可能
// 折成不同字节** —— 第一次请求发的是 16000 版, 第二次变成 8000 版, 前缀缓存
// 从这个位置起全废。实测 if-question-expand 第 3 轮的断裂就是这么来的
// (12151B → 8075B)。
const (
	// MicroCompactThreshold 触发折叠的 tool_result 体量上限 (字节)。
	MicroCompactThreshold = 16000
	// MicroCompactKeepBytes 折叠后保留的原文头部长度 (字节)。
	MicroCompactKeepBytes = 2048
)

// ToolResultStub 生成 tool_result 的折叠占位 (纯截断, 不落盘)。
//
// 等价于 ToolResultStubWithRef(block, nil), 保留给"没有工作区可写"的场景
// (测试、独立小循环、只读挂载)。行为与改造前逐字节相同。
func ToolResultStub(block types.ContentBlock) string {
	return ToolResultStubWithRef(block, nil)
}

// ToolResultStubWithRef 生成 tool_result 的折叠占位, 并把原文落盘供回读。
//
// **这是纯函数** (给定 store): 输出只由入参 block 的 ToolUseID / IsError /
// Content 决定, 以及 store 的**目录名** —— 不含时间戳、轮次、随机数、序号。
// 因此同一条 tool_result 无论在哪个回合、被哪条代码路径折叠, 产出的字节完全
// 相同 —— 折叠是**幂等**的, 折过的那段前缀之后永远稳定。这是整个折叠机制能跟
// 前缀缓存共存的前提, 加文件引用也不能破坏它 (引用名是内容哈希, 见 refstore.go)。
//
// store 为 nil 或落盘失败时**退化为纯截断**: 宁可不给引用, 也不能让折叠在
// 这里失败 —— 折叠失败意味着上下文压不下去, 那是一条更严重的故障路径。
//
// 保留 call_id 是必需的: 上游要求 tool_use 与 tool_result 按 id 配对, 占位
// 只换掉 payload, 绝不换掉 id。
func ToolResultStubWithRef(block types.ContentBlock, store *ToolResultRefStore) string {
	var sb strings.Builder
	sb.WriteString("[tool_result 已折叠] call_id=")
	sb.WriteString(block.ToolUseID)
	fmt.Fprintf(&sb, " 原文=%d字节", len(block.Content))
	if block.IsError {
		sb.WriteString(" status=error")
	}
	sb.WriteString("\n")

	// 落盘并给出回读路径。失败就退回纯截断 (不写任何路径提示)。
	//
	// 路径独占一行且用 `full_artifact: ` 前缀 —— 与 pkg/tool 的执行期溢出用同一个
	// 键名。两个原因: (a) 键值行是机器可解析的, 路径后面跟说明文字会让"按行取路径"
	// 的模型把说明一起当文件名; (b) 模型在别处已经学过 full_artifact 的含义, 不必
	// 为压缩期折叠再造一套说法。
	if store != nil && len(block.Content) > MicroCompactKeepBytes {
		if name, err := store.Put(block.Content); err == nil {
			sb.WriteString("full_artifact: ")
			sb.WriteString(store.Path(name))
			sb.WriteString("\n")
			sb.WriteString("artifact_note: 完整原文已落盘, 不要重跑命令; 用 Read 工具按上面的路径读回。\n")
		}
	}

	// 保留原文开头一小段作定位线索。定长截断 -> 仍是纯函数。
	sb.WriteString(textutil.TruncateBytes(block.Content, MicroCompactKeepBytes, "…"))
	return sb.String()
}

// StubToolResults 把 [from, to) 区间内超阈值的 tool_result 折叠为占位 (纯截断)。
//
// to < 0 表示"一直到末尾"。区间外的消息原样保留 —— 调用方用它来把"允许改造的
// 范围"限死在指定区间内, 这正是防止改写已发出历史的关键。
func StubToolResults(messages []types.Message, from, to int) []types.Message {
	return StubToolResultsWithRef(messages, from, to, nil)
}

// StubToolResultsWithRef 同 StubToolResults, 但把被折叠的原文落盘并给出引用。
//
// store 为 nil 时与 StubToolResults 完全等价。
func StubToolResultsWithRef(messages []types.Message, from, to int, store *ToolResultRefStore) []types.Message {
	out, _ := StubToolResultsCounted(messages, from, to, store)
	return out
}

// StubToolResultsCounted 与 StubToolResultsWithRef 同行为, 额外返回折叠计数。
//
// 计数是从**同一个循环**里数出来的, 不是事后比对前后文本重算的: 后者要把两份
// 消息数组逐块 diff, 而在"折了 3 条、其中 2 条给出引用"这种问题上, diff 分不清
// "折了但没给引用"与"根本没折"。H2-1 要的正是那个差。
func StubToolResultsCounted(messages []types.Message, from, to int, store *ToolResultRefStore) ([]types.Message, StubCounts) {
	var counts StubCounts
	if from < 0 {
		from = 0
	}
	if to < 0 || to > len(messages) {
		to = len(messages)
	}
	if from >= to {
		return messages, counts
	}
	// 自指保护: 读了折叠引用目录的那几条结果**整条跳过**, 既不折叠也不落盘。
	//
	// 保护写在**这里**而不是各调用点: MicroCompact 与折叠边界两条路都汇到本函数,
	// 写在外面就要记得改两处, 漏一处就重现坏循环。详见 RefDirReadCallIDs 的注释。
	var refReads map[string]bool
	if store != nil {
		refReads = RefDirReadCallIDs(messages, from, to, store.RefDirs()...)
	}
	out := make([]types.Message, len(messages))
	copy(out, messages)
	for i := from; i < to; i++ {
		msg := out[i]
		if msg.Type != types.MessageTypeUser {
			continue
		}
		var changed bool
		newBlocks := make([]types.ContentBlock, len(msg.Content))
		copy(newBlocks, msg.Content)
		for j, block := range newBlocks {
			if block.Type != types.ContentBlockToolResult || len(block.Content) <= MicroCompactThreshold {
				continue
			}
			// 这条结果是"把折叠引用读回来"得到的 -> 放它进上下文。
			// 折它等于让模型永远看不到自己刚要求回读的原文。
			if refReads[block.ToolUseID] {
				counts.SkippedRefReads++
				continue
			}
			block.Content = ToolResultStubWithRef(block, store)
			counts.Folded++
			// 判"有没有给出引用"靠占位文本本身, 不靠重算 store.Put 的成败:
			// Put 是幂等的 (内容寻址 + 字节数相同的短路), 重算一次会多一次盘
			// 交互, 而判据就在刚生成的那段字节里。
			if strings.Contains(block.Content, "full_artifact: ") {
				counts.Refs++
			}
			newBlocks[j] = block
			changed = true
		}
		if changed {
			msg.Content = newBlocks
			out[i] = msg
		}
	}
	return out, counts
}

// MicroCompact 折叠过大的工具输出。
// 对应 TS: services/compact/microCompact.ts
//
// protectFrom 是**本轮新增消息的起始下标**: 只有 [protectFrom, len) 会被改造,
// 之前的一切原样透传。这条约束是硬性的 —— 前缀缓存按字节比对, 任何对已发出
// 历史的改写都会让该位置之后的缓存全部失效。传 0 退化为"全量可改", 仅供
// 单元测试与无缓存的单轮场景使用。
//
// 折叠本身用 ToolResultStub (纯函数), 所以对同一段新消息重复调用结果一致。
//
// 无引用存储 -> 纯截断。要落盘回读用 MicroCompactWithRef。
func MicroCompact(messages []types.Message, protectFrom int) []types.Message {
	return StubToolResults(messages, protectFrom, -1)
}

// MicroCompactWithRef 同 MicroCompact, 但把被折叠的原文落盘并给出引用路径。
// store 为 nil 时与 MicroCompact 完全等价。
func MicroCompactWithRef(messages []types.Message, protectFrom int, store *ToolResultRefStore) []types.Message {
	return StubToolResultsWithRef(messages, protectFrom, -1, store)
}

// GetMessagesAfterCompactBoundary 获取 compact boundary 之后的消息。
// 对应 TS: utils/messages.ts 中的 getMessagesAfterCompactBoundary()
func GetMessagesAfterCompactBoundary(messages []types.Message) []types.Message {
	lastBoundary := -1
	for i, msg := range messages {
		if msg.IsCompactBoundary {
			lastBoundary = i
		}
	}
	if lastBoundary >= 0 {
		return messages[lastBoundary:]
	}
	return messages
}

// 辅助函数

// estimateTokens 粗估消息的 token 数 (chars/4)。
//
// 口径: 凡参与出站序列化的文本字段都计入 —— Text (text 块) / Content
// (tool_result 结果体) / Thinking (扩展思考) / Input (tool_use 入参 JSON)。
//
// 为什么不能只算 Text/Content: thinking 块与 tool_use 入参在长会话里是体量
// 大头 (单次 Write/Edit 的入参可达上万字符, 每个 thinking 块可达成千 token),
// 漏算会让 AutoCompact 的触发点被系统性推迟 —— 真实上下文已逼近窗口, 估算
// 值仍显示安全, 直到上游返回 prompt_too_long 才被动补救。
//
// 与 internal_hook.TokenBudgetManager.EstimateTokens 保持同一口径:
// 二者一个卡压缩触发、一个定预算等级, 口径分叉会让"该压的没压"。
// EstimateTokens 是 estimateTokens 的导出形态。
//
// 导出是为了让折叠读数 (H2-1) 的**报送方** (引擎侧 hook) 能算出与折叠本身同口径
// 的 token 数: 两边各写一份 chars/4 会立刻分叉 (estimateTokens 要计入 thinking 与
// tool_use 入参, 漏算的版本读出来的"折叠前后"是同一套谎言)。
func EstimateTokens(messages []types.Message) int { return estimateTokens(messages) }

func estimateTokens(messages []types.Message) int {
	total := 0
	for _, msg := range messages {
		for _, block := range msg.Content {
			total += len(block.Text) / 4
			total += len(block.Content) / 4
			total += len(block.Thinking) / 4
			total += len(block.Input) / 4
		}
	}
	return total
}

func extractText(msg types.Message) string {
	var parts []string
	for _, block := range msg.Content {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
		if block.Content != "" {
			parts = append(parts, block.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// truncate 按**字符数**截断 (不是字节) —— 中文一个字 3 字节, 按字节切会把字
// 劈成半个非法 UTF-8 序列。详见 pkg/textutil 的包注释。
func truncate(s string, max int) string {
	// 判断条件与截断口径必须一致: 按 rune 数比较、按 rune 数截。
	return textutil.Truncate(s, max, "...")
}

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// SmartExtractKeyFacts 使用 LLM 智能提取关键事实 (Anchored Iterative Summarization)。
// 业界最佳实践: 仅对新增消息段做增量摘要, 合并到持久化状态。
// 参考: Mem0 fact-extraction + Letta sliding-window compaction
//
// 如果 LLM 调用失败, 自动回退到 ExtractKeyFacts 启发式提取。
func (c *Compactor) SmartExtractKeyFacts(ctx context.Context, messages []types.Message) []string {
	if c.apiClient == nil {
		return ExtractKeyFacts(messages)
	}

	var sb strings.Builder
	count := 0
	for _, msg := range messages {
		text := extractText(msg)
		if text == "" || msg.IsApiErrorMessage || msg.IsMeta {
			continue
		}
		role := string(msg.Type)
		sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", role, truncate(text, 800)))
		count++
	}
	if count < 2 || sb.Len() < 100 {
		return ExtractKeyFacts(messages)
	}

	systemPrompt := `You are a memory extraction agent. Extract key facts from the conversation that are worth remembering for future sessions.

Output a JSON array of strings. Each string is one concise fact (1-2 sentences).

Focus on:
- Important decisions made and their rationale
- Files created/modified/deleted and why
- Technical patterns, architectures, or tools used
- User preferences and working style
- Errors encountered and how they were resolved
- Key conclusions or outcomes

Exclude: trivial acknowledgments, repeated info, generic advice.
Maximum 10 facts. Output ONLY valid JSON array, nothing else.`

	resp, err := c.apiClient.SimpleComplete(ctx, systemPrompt, "Extract key facts:\n\n"+sb.String())
	if err != nil {
		return ExtractKeyFacts(messages)
	}

	var facts []string
	resp = strings.TrimSpace(resp)
	// 尝试直接解析 JSON
	if err := json.Unmarshal([]byte(resp), &facts); err != nil {
		// 尝试从 markdown code block 中提取
		if idx := strings.Index(resp, "["); idx >= 0 {
			if end := strings.LastIndex(resp, "]"); end > idx {
				_ = json.Unmarshal([]byte(resp[idx:end+1]), &facts)
			}
		}
	}
	if len(facts) == 0 {
		return ExtractKeyFacts(messages)
	}
	return facts
}

// ExtractKeyFacts 启发式提取关键事实 (LLM 不可用时的回退方案)。
// 比原版更智能: 识别文件路径、决策语句、错误-解决对。
func ExtractKeyFacts(messages []types.Message) []string {
	var facts []string
	for _, msg := range messages {
		text := extractText(msg)
		if text == "" || msg.IsApiErrorMessage || msg.IsMeta {
			continue
		}

		switch msg.Type {
		case types.MessageTypeAssistant:
			facts = append(facts, extractStructuredFacts(text)...)
		case types.MessageTypeUser:
			if len(text) > 30 {
				facts = append(facts, "用户: "+truncate(text, 200))
			}
		}
	}

	seen := make(map[string]bool)
	var unique []string
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f != "" && !seen[f] && len(f) > 10 {
			seen[f] = true
			unique = append(unique, f)
		}
	}
	if len(unique) > 15 {
		unique = unique[:15]
	}
	return unique
}

// extractStructuredFacts 从 assistant 回复中提取结构化事实
func extractStructuredFacts(text string) []string {
	var facts []string

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 文件操作: 包含路径模式的行
		if containsFilePath(line) && len(line) < 300 {
			facts = append(facts, truncate(line, 200))
			continue
		}
		// 决策语句
		if isDecisionStatement(line) && len(line) < 300 {
			facts = append(facts, truncate(line, 200))
			continue
		}
		// 错误和解决方案
		if isErrorSolution(line) && len(line) < 300 {
			facts = append(facts, truncate(line, 200))
		}
	}

	// 补充: 如果结构化提取太少, 加入整体摘要
	if len(facts) < 2 && len(text) > 200 {
		facts = append(facts, truncate(text, 300))
	}
	return facts
}

// containsFilePath 检查是否包含文件路径
func containsFilePath(s string) bool {
	pathPatterns := []string{"/", ".go", ".ts", ".js", ".py", ".rs", ".java", ".md", ".json", ".yaml", ".toml"}
	for _, p := range pathPatterns {
		if strings.Contains(s, p) && (strings.Contains(s, "pkg/") || strings.Contains(s, "src/") ||
			strings.Contains(s, "创建") || strings.Contains(s, "修改") || strings.Contains(s, "删除") ||
			strings.Contains(s, "create") || strings.Contains(s, "modify") || strings.Contains(s, "wrote") ||
			strings.Contains(s, "updated")) {
			return true
		}
	}
	return false
}

// isDecisionStatement 检查是否是决策/结论语句
func isDecisionStatement(s string) bool {
	lower := strings.ToLower(s)
	markers := []string{
		"决定", "选择", "采用", "使用", "方案", "结论", "建议",
		"decided", "chose", "using", "approach", "conclusion", "recommend",
		"should", "will use", "best practice", "pattern",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// isErrorSolution 检查是否是错误-解决对
func isErrorSolution(s string) bool {
	lower := strings.ToLower(s)
	markers := []string{
		"error", "错误", "bug", "fix", "修复", "解决", "solution",
		"问题", "原因", "cause", "resolved", "workaround",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// ExtractText 导出 extractText 供外部使用
func ExtractText(msg types.Message) string {
	return extractText(msg)
}
