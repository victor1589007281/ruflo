// hook_memory.go — 记忆系统相关内置 Hook。
//
// 包含:
//   - MemoryInjectHook   L1/L2 记忆注入 system prompt
//   - PromptCacheHook    Prompt cache 构建与命中率追踪
package internal_hook

import (
	"strings"
	"sync/atomic"

	"github.com/anthropic/claude-go/pkg/memory"
)

// ============================================================================
// MemoryInjectHook — L1/L2 记忆注入
// ============================================================================

// MemoryInjectHook 在 PhasePreRequest 阶段从记忆系统中检索与当前用户请求相关的记忆，
// 并追加到 system prompt 中。
//
// 缓存约束 (两条都是硬性的):
//
//  1. 只在 t0 检索一次。检索结果原样**复用**到该 run 的后续每一轮, 而不是每轮重算。
//     原因: 块内容一旦变化, 它所在 system 块之后的前缀全部失效; 而且这块在 t0 是
//     第 4 个块、t1 直接消失, 等于把 tools 段之后整个作废 (实测第 1 轮断裂就断在
//     这里 —— system[3] 1577B 在 t1 不见了)。
//
//  2. 复用而不是"t0 注入、之后不注入"。后者看着更省 token, 但它改变了 system 数组
//     的长度和内容, 数组容量一变, 后面所有块的偏移都跟着变, 缓存同样全废。
//     保持数组容量不变 = 保持字节不变。
type MemoryInjectHook struct {
	memoryStore *memory.TieredStore
	factStore   *memory.FactStore

	// metrics 检索遥测落点 (可选)。每次**真的发起检索**时记一笔, 见 recordRetrieval。
	metrics MemoryMetricsRecorder

	// indexMode 为真时注入**索引**而不是正文 (见 memory.FormatIndexForPrompt):
	// 提示里只放 id + 标题 + 预览, 模型按需调 memory_read 取全文。
	// 默认关 —— 它在改变每次请求的内容, 归宿主开关管 (配置项 engine.memoryIndexMode,
	// 环境变量 CLAUDE_GO_MEMORY_INDEX_MODE 作兜底)。
	//
	// 用 atomic.Bool 而不是 plain bool: 这个开关要能**热改**。它由配置热加载回调
	// 在任意时刻 Set, 而读取发生在 queryLoop 的 PhasePreRequest —— 两个 goroutine,
	// 且热加载不以任何方式与 run 互斥 (改配置不该打断队列里在跑的任务)。
	// plain bool 在这里是数据竞争, `go test -race` 会直接报。
	//
	// 生效边界: 改的是**下一次渲染**的行为。热改不会立刻改写已经发出去的请求,
	// 而是在本 run 的**下一个 turn** 用新形态重渲染 (见 Execute 的 indexGen 分支)。
	// 代价是那一个 turn 的前缀缓存从 memory 块起失效一次 —— 块排在 system 末尾,
	// 这已经是最便宜的失效点了。
	indexMode atomic.Bool

	// indexGen 是形态的**代数**: SetIndexMode 真改变了取值才 +1。
	//
	// 为什么要有它: cachedIn 是在 run goroutine 上算出来并缓存的, 而 SetIndexMode
	// 跑在配置热加载 goroutine 上。让后者直接清缓存就是数据竞争 —— 于是改成
	// 「后者只原子地 +1, 前者发现代数变了就重算」。写 cachedIn 的永远只有 run
	// goroutine 一个, 竞争面就此消失。
	indexGen atomic.Uint64

	// 会话内缓存: 首次检索的结果, 后续轮次原样复用。
	// key 用 runKey (会话/运行标识) 而非 hook 实例——hook 是按引擎构造的,
	// 同一引擎上不同的 run 不应互相串味。
	runKey string
	// 缓存的是**检索结果**而不是渲染好的文本: 索引/正文两种形态共用同一批条目,
	// 只差怎么呈现 (retrieve 里那句注释是这条契约的出处)。若缓存文本, 热切换就
	// 只能重新检索 —— 而记忆库在 run 结尾会被写一次, 重检索捞回的是**另一批**
	// 记忆, 于是「换个呈现方式」悄悄变成了「换一批内容」。
	entries []*memory.MemoryEntry
	facts   []*memory.MemoryFact

	cachedIn string // 已渲染的记忆块文本 (空 = 尚未检索或检索无结果)
	cached   bool   // 是否已检索过 (区分"没检索"与"检索了但没结果")
	// rendered 标记 cachedIn 是否已按某个形态算出来过 (空结果也算渲染过 ——
	// "检索了但没捞到"与"还没渲染"必须分得开)。
	rendered bool
	// seenGen 是 cachedIn 对应的代数, 只在 run goroutine 上读写 (与 cachedIn 同域)。
	seenGen uint64
}

// MemoryMetricsRecorder 是记忆检索遥测的最小落点接口。
//
// 刻意只要求 Record(module, name, value): 与 memory.MetricsRecorder 同形, 于是
// metrics.Collector 天然满足它, 本包又不必反向依赖 metrics (也就不必依赖它背后的
// Prometheus 注册表)。
type MemoryMetricsRecorder interface {
	Record(module, name string, value float64)
}

// NewMemoryInjectHook 创建 MemoryInjectHook。
func NewMemoryInjectHook(memoryStore *memory.TieredStore, factStore *memory.FactStore) *MemoryInjectHook {
	return &MemoryInjectHook{memoryStore: memoryStore, factStore: factStore}
}

// SetMetrics 装配检索遥测落点 (nil = 不采集)。
//
// 调用方注意: 传 **带类型的 nil 指针** (如未初始化的 *metrics.Collector) 不等于
// 不采集 —— 它装箱成非 nil 接口, 本包里的 `h.metrics == nil` 拦不住, 会在第一次
// 检索时 panic 到 nil 接收者上。调用方要先判自己的指针再传, 见
// engine.registerInternalHooks 里对 GlobalLLMCollector 的处理。
func (h *MemoryInjectHook) SetMetrics(m MemoryMetricsRecorder) { h.metrics = m }

// SetIndexMode 切换索引注入形态 (默认关, 见 indexMode 字段)。
// 可并发调用: 配置热加载路径在跑, run 路径在读。
//
// 只有**真的变了**才推进代数 —— 热加载每 5 秒比对一次, 传进来的值绝大多数时候
// 与当前相同, 无条件 +1 会让下一个 turn 白白重渲染一次 (缓存白失效)。
func (h *MemoryInjectHook) SetIndexMode(on bool) {
	if h.indexMode.Swap(on) != on {
		h.indexGen.Add(1)
	}
}

// IndexMode 返回当前注入形态 (供遥测/测试断言, 不必读私有字段)。
func (h *MemoryInjectHook) IndexMode() bool { return h.indexMode.Load() }

func (h *MemoryInjectHook) Name() string                { return "memory_inject" }
func (h *MemoryInjectHook) Priority() int               { return 50 }
func (h *MemoryInjectHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// ResetRun 清空会话内缓存。新的 run (新会话/新任务) 开始时必须调用, 否则会复用
// 上一个 run 的记忆。
func (h *MemoryInjectHook) ResetRun(runKey string) {
	if h.runKey == runKey {
		return
	}
	h.runKey = runKey
	h.cachedIn = ""
	h.cached = false
	h.entries = nil
	h.facts = nil
}

// Execute 检索 L1/L2 记忆并追加到 system prompt。
func (h *MemoryInjectHook) Execute(ctx *HookContext) (*HookResult, error) {
	// 首次检索后一律走复用分支 —— 无论 t0 检索到没检索到, 后续轮次的**行为**
	// 都必须与 t0 一致 (没检索到就一直不注入, 而不是 t0 不注入 t1 又注入)。
	if !h.cached {
		h.cached = true
		h.search(ctx) // 只取结果, 不渲染 —— 渲染推迟到下面按**当前**形态做
	}
	// 首轮, 或形态被热改过: 按**当前**形态渲染。
	//
	// 热改时检索**不重跑**: 记忆库在每次 run 结尾被写一次 (memory 拦截器), 重检索
	// 捞回的是另一批记忆 —— 那等于把"换个呈现"变成"换批内容", 两个变量就分不开了。
	// 这里只换外壳, 内容逐条相同。
	if !h.rendered || h.indexGen.Load() != h.seenGen {
		h.rendered = true
		h.seenGen = h.indexGen.Load()
		h.cachedIn = h.render()
	}
	if h.cachedIn == "" {
		return nil, nil
	}

	// 追加到末尾而不是插进中间: 末尾块的变动不影响它前面的前缀。
	systemPrompt := append(append([]string{}, ctx.SystemPrompt...), h.cachedIn)
	return &HookResult{SystemPrompt: systemPrompt}, nil
}

// search 执行一次检索, 把结果**留在 hook 上** (不渲染)。
//
// 检索与渲染分开, 是为了让形态热改只换外壳不换内容 —— 见 Execute 里的说明。
// 落在 hook 上的两个切片都只在 run goroutine 上读写, 与 SetIndexMode 无交集。
func (h *MemoryInjectHook) search(ctx *HookContext) {
	h.entries, h.facts = nil, nil

	query := h.retrievalQuery(ctx)
	if query == "" {
		// 连查询都没有 = **压根没检索**, 不记 —— recordRetrieval 的注释把这层区分
		// 说得很清楚: 记了就等于把"没检索"报成"检索了但没捞到"。
		return
	}

	// L1: TieredStore 情景记忆
	if h.memoryStore != nil && h.memoryStore.Count() > 0 {
		h.entries = h.memoryStore.Retrieve(query, 7)
	}

	// L2: FactStore 结构化记忆
	if h.factStore != nil && h.factStore.Count() > 0 {
		h.facts = h.factStore.Retrieve(query, 5)
	}
	h.recordRetrieval(len(h.entries), len(h.facts))
}

// render 按**当前**形态把上次检索的结果拼成注入文本, 并记一笔遥测。
//
// 两种形态共用同一批检索结果, 只差**怎么呈现** —— 正文 (默认) 或索引 (模型按需
// 取回)。选哪条不影响检索本身, 所以遥测里的命中数两种模式下含义相同、可直接对比。
func (h *MemoryInjectHook) render() string {
	var memPrompt string
	if len(h.entries) > 0 {
		if h.indexMode.Load() {
			memPrompt = memory.FormatIndexForPrompt(h.entries)
		} else {
			memPrompt = memory.FormatForPrompt(h.entries)
		}
	}
	if len(h.facts) > 0 {
		memPrompt += memory.FormatFactsForPrompt(h.facts)
	}

	// 注入字符数记在**渲染之后** —— 形态切换才在账本上可见 (索引比正文短一个量级,
	// 这正是要量的东西)。命中数不在这里记, 见 recordRetrieval 的分工说明。
	h.recordInjection(len(memPrompt))
	return memPrompt
}

// recordRetrieval 记一笔**检索**遥测 (攒够一个 run 的量才有意义的那些)。
//
// 为什么这些数以前量不出来: memory.jsonl 里此前只有 L2 的库存量 (fact_*), 而
// **L1 才是注入的大头** (实测 top-7 合计 ~19K 字符)。于是"记忆到底注入了多少、
// 是不是噪声"这个问题, 在账本上完全没有落点 —— 只有 llm.jsonl 的 memory_chars
// 总量, 分不出 L1/L2, 也分不出检索命中几条。
//
// 三条都不带任何**无界标签**: 记忆 ID 每条都不同, 做成标签就是无界基数 (与
// Collector.RecordRunAtTime 注释里对 run_id 的处理同一条理由)。所以细粒度归因
// 走 memory_type 这个两值标签, 而不是把命中的 ID 塞进去。
//
// 分工: 只记**检索**本身 (每次真的发起 +1)。注入字符数走 recordInjection —— 形态
// 热改会让同一个 run 多渲染一次, 那次是"同一批记忆换个外壳", 没有检索发生, 记进
// 这里就是把 1 次检索数成 2 次。而注入字符数恰恰相反: 正是要在每次**实际发出的
// 形态**上各记一笔, 两笔的差值就是这次切换省下的预算。
func (h *MemoryInjectHook) recordRetrieval(l1, l2 int) {
	if h.metrics == nil {
		return
	}
	// 库存量: 每次检索都报一次当前值 (gauge), 让"记忆库涨到多大"可回看。
	// 这也顺手补上了 mem_entry_count 此前**零发射方**的缺口 (声明在
	// module_metrics.go, 但全仓没有任何一处 Record 过它)。
	if h.memoryStore != nil {
		h.metrics.Record("memory", "mem_entry_count", float64(h.memoryStore.Count()))
	}
	// 检索次数本身 (旧口径, counter): 每次真的发起检索 +1。它与下面按层的**命中条数**
	// 是两回事 —— 打了 10 次检索全落空, 这里记 10, 那边记 0。
	h.metrics.Record("memory", "mem_retrieval_count", 1)
	// 命中数: counter, 按层分。0 命中也记 —— "检索了但没捞到"和"压根没检索"
	// 是两件不同的事, 而后者由本函数没被调到区分, 不需要在这里再编一个值出来。
	h.metrics.Record("memory", "mem_retrieval_count_l1", float64(l1))
	h.metrics.Record("memory", "mem_retrieval_count_l2", float64(l2))
}

// recordInjection 记一笔**注入形态**遥测。每次渲染各记一笔 (含形态热改后的重渲染)。
func (h *MemoryInjectHook) recordInjection(chars int) {
	if h.metrics == nil {
		return
	}
	// 注入字符数: 这是真正决定"这块吃掉多少预算"的数 —— 索引形态比正文短一个量级,
	// 两者在这条曲线上应当**肉眼可分**, 否则开关等于没生效。
	h.metrics.Record("memory", "mem_injection_chars", float64(chars))
}

// retrievalQuery 决定用哪段文本做检索 key。
//
// 历史坑: 原先直接取"最后一条 user 消息的 text"。而当 TaskInstruction 已经进
// system prompt 时, 那条 user 消息是 engine 塞的占位符 (taskInstructionPointer,
// 内容为"（任务指令已置于 system prompt 末尾…）") —— 拿它去检索, 等于用一个
// 对任何记忆都零信息量的句子做查询, BM25 只能按"指令/任务/执行"这类高频词乱匹配,
// 捞回来的全是噪声。
//
// 真实任务文本由 ctx.TaskInstruction 直接给出 (engine 的稳定/可变分层把它排在
// 稳定块之后, 且在 **本 phase 执行时尚未写入 SystemPrompt**)。
func (h *MemoryInjectHook) retrievalQuery(ctx *HookContext) string {
	lastUserText := ""
	for i := len(ctx.Messages) - 1; i >= 0; i-- {
		if ctx.Messages[i].Type != "user" {
			continue
		}
		for _, b := range ctx.Messages[i].Content {
			if b.Text != "" {
				lastUserText = b.Text
				break
			}
		}
		break
	}

	if !IsTaskInstructionPointer(lastUserText) {
		return lastUserText
	}

	// 占位符 -> 取本轮任务指令正文。
	//
	// 为什么优先 ctx.TaskInstruction 而不是去 SystemPrompt 里取: 这两处的取值
	// **会**不一致 —— 本 hook 自己就是往 SystemPrompt 追加块的那个, 一旦注入过
	// 一次, 末段就变成记忆块本身, 拿记忆去检索记忆。ctx.TaskInstruction 是引擎
	// 直接给的正文, 没有这层混淆。
	if s := strings.TrimSpace(ctx.TaskInstruction); s != "" {
		return s
	}

	// 兜底: 无 TaskInstruction (纯角色型会话) 时仍扫 system 末段。
	for i := len(ctx.SystemPrompt) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(ctx.SystemPrompt[i]); s != "" {
			return s
		}
	}
	return ""
}

// ============================================================================
// PromptCacheHook — Prompt Cache 追踪
// ============================================================================

// PromptCacheHook 在 PhasePreRequest 阶段拆分 static/dynamic system prompt，
// 构建 prompt cache 并记录 cache 命中率到 Metrics。
type PromptCacheHook struct {
	cache   *PromptCacheBuilder
	metrics *EngineMetrics // 引擎指标
}

// NewPromptCacheHook 创建 PromptCacheHook。
func NewPromptCacheHook(cache *PromptCacheBuilder, metrics *EngineMetrics) *PromptCacheHook {
	return &PromptCacheHook{cache: cache, metrics: metrics}
}

func (h *PromptCacheHook) Name() string                { return "prompt_cache" }
func (h *PromptCacheHook) Priority() int               { return 55 }
func (h *PromptCacheHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 拆分 system prompt，构建 cache 并记录命中率。
func (h *PromptCacheHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.cache == nil || len(ctx.SystemPrompt) == 0 {
		return nil, nil
	}

	static, dynamic := SplitStaticDynamic(ctx.SystemPrompt)
	_, hit := h.cache.Build(static, dynamic)
	if h.metrics != nil {
		h.metrics.RecordCache(hit)
	}

	return nil, nil
}
