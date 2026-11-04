// runoverrides.go — 单次 run 的模型与上限覆盖（Moqi R-H17 / R-H10 的引擎侧支撑）。
//
// 为什么用 ctx 而不是 Config 字段：引擎是**会话级复用**的，同一个 *QueryEngine 会被
// 团队路径的 8 个并发 runner 共用（见 inject.go 文件头）。把"本轮用什么模型、上限
// 多少"写进 Config，等于让并发 run 互相覆盖 —— 而覆盖的表现是"某几轮悄悄换了模型"，
// 恰好是 R-H17 明令禁止的静默换模型。ctx 天然按 run 隔离、无需加锁。
//
// 与 Config 的关系是**只覆盖、不改变默认**：没有 ctx 覆盖时所有读取点的值必须与
// 改造前逐字节相同（R-H16 的"新能力关闭时请求字节不变"）。
package engine

import (
	"context"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// RunOverrides 是单次 run 的覆盖项。零值 = 全部沿用 Config。
type RunOverrides struct {
	// Model 覆盖对话模型。空 = 用 Config.Model。
	//
	// 只影响"对话"这一个角色：ExecutionModel / FallbackModel / PlanClient 的既有
	// 路由仍然在它之上生效（升级与相位路由是引擎的既有语义，不应被角色策略抹掉）。
	Model string
	// MaxTurns 覆盖 queryLoop 的迭代上限。<=0 = 用 Config.MaxTurns。
	MaxTurns int
	// MaxTokens 覆盖单次请求的输出上限。<=0 = 用 Config.MaxTokens。
	MaxTokens int
	// MaxToolCalls 是本 run 允许的工具调用次数上限（0 = 不设限）。
	//
	// 必须是引擎侧判据而不是只在外层数事件：外层的取消只在下一个轮次边界生效，
	// 而一轮里可能并发调十个工具 —— 那时预算已经烧掉了，取消只是事后追认。
	// 引擎在这里判，超限的那一批工具**一个都不执行**。
	MaxToolCalls int
	// AllowedTools 是本 run 的工具白名单（Moqi R-H9 / TurnPolicy.AllowedTools）。
	//
	// 三态与 Config.AllowedTools 完全一致，且**只能比 Config 更严**：
	//
	//	nil                   = 本 run 不额外收窄（沿用 Config 的名单/黑名单）
	//	非 nil，含工具名       = 仅这些工具可见可执行
	//	非 nil，空集           = 全部拒绝（fail-closed）
	//
	// 为什么"只能更严"：Config 的名单来自部署策略（agent 约束、租户配置），
	// 而本字段来自**单轮请求**。一个请求体能把 Bash 加回来，部署策略就是装饰品。
	// 实现上取交集，而不是覆盖。
	AllowedTools *[]string
	// Confirmation 是本 run 的宿主确认来源（R-H9）。nil = 不给确认：
	// 声明了 NeedsConfirmation 的工具调用会被拒（fail-closed，见 pkg/tool/orchestration.go）。
	//
	// 为什么放在 ctx 覆盖里而不是 Config：确认是**一次调用一个答复**，同一个引擎被
	// 多个并发 run 复用时，Config 上的字段会被别的 run 覆盖 —— 表现为"A 的确认
	// 放行了 B 的删库"。ctx 按 run 隔离，才配得上这道闸的语义。
	Confirmation tool.ConfirmationChecker
	// Idempotency 是本 run 的工具结果去重闸（R-H9）。nil = 不去重。
	//
	// 同样按 run 隔离：去重缓存的生命周期必须与"一轮"对齐，跨 run 复用会把上一轮
	// 的结果返回给参数相同但世界已经变了的下一轮。
	Idempotency *tool.IdempotencyGate
}

// IsZero 报告没有覆盖项。
//
// AllowedTools 的判据是**存在性**而不是 len：非 nil 的空集是"拒绝一切"这个明确的
// 指令，把它当成"没有覆盖"会让 fail-closed 的意图反转成完全放开 —— 这是本仓
// 已经踩过一次的坑（见 Config.AllowedTools 与 queryLoop 里那段注释）。
//
// Confirmation / Idempotency 的判据同样是**存在性**：它们没有"零值"以外的语义，
// 非 nil 就是一条明确的接线指令。漏掉它们会让 WithRunOverrides 把整个覆盖当成
// 零值**静默丢弃** —— 于是闸门"建成但没通电"，而且没有任何报错。这是本仓
// "建成未通电"反复出现的那一类缺陷，所以判据必须与字段同步增长。
func (o RunOverrides) IsZero() bool {
	return o.Model == "" && o.MaxTurns <= 0 && o.MaxTokens <= 0 && o.MaxToolCalls <= 0 &&
		o.AllowedTools == nil && o.Confirmation == nil && o.Idempotency == nil
}

// runOverridesKey 是未导出的 ctx 键类型：只有本包能写，别的包无法伪造。
type runOverridesKey struct{}

// WithRunOverrides 把本轮覆盖写进 ctx。零值等价于不设置。
func WithRunOverrides(ctx context.Context, o RunOverrides) context.Context {
	if ctx == nil || o.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, runOverridesKey{}, o)
}

// RunOverridesFromContext 取本轮覆盖；没有时返回零值与 false。
func RunOverridesFromContext(ctx context.Context) (RunOverrides, bool) {
	if ctx == nil {
		return RunOverrides{}, false
	}
	o, ok := ctx.Value(runOverridesKey{}).(RunOverrides)
	if !ok || o.IsZero() {
		return RunOverrides{}, false
	}
	return o, true
}

// ToolAllowlist 计算本 run 的**有效**工具白名单（nil = 不受白名单限制）。
//
// 语义是"只收窄、永不放宽"，与 Config.ApplyConstraints 同一套原则：
//
//	Config.AllowedTools | RunOverrides.AllowedTools | 结果
//	--------------------+---------------------------+------------------------
//	nil                 | nil                       | nil（不设白名单）
//	nil                 | 非 nil                    | 本 run 的名单
//	非 nil              | nil                       | Config 的名单
//	非 nil              | 非 nil                    | **交集**
//
// 为什么要交集而不是"本 run 优先"：Config 的名单来自部署策略（agent 约束、
// 租户配置），本 run 的名单来自单轮请求。让请求体覆盖部署策略，等于把一个
// 越权面直接开在 API 上 —— 而两者都"允许"时才真的允许，是最小权限的自然表达。
//
// 空集不是"没有限制"：任何一个非 nil 输入配上空的交集，结果就是空 map，
// 而空 map 的语义是**拒绝一切**（与 Config.AllowedTools 的 fail-closed 一致）。
// 返回 nil 还是空 map 必须严格区分，所以这里不把 len==0 归一成 nil。
func (c *Config) ToolAllowlist(ov RunOverrides) map[string]bool {
	var base map[string]bool
	if c != nil {
		base = c.AllowedTools
	}
	extra := ov.AllowedTools
	if extra == nil {
		return base
	}
	if base == nil {
		next := make(map[string]bool, len(*extra))
		for _, name := range *extra {
			next[name] = true
		}
		return next
	}
	next := make(map[string]bool, len(base))
	for _, name := range *extra {
		if base[name] {
			next[name] = true
		}
	}
	return next
}

// LastTerminal 返回最近一次 run 的终态。
//
// 读到的必然是**已经结束**的 run 的终态：SubmitMessage / SubmitStreamBlocks 的收尾
// goroutine 先写 lastTerminal、再 close(streamCh)，所以一个已经把流读干的调用方经
// channel close 拿到 happens-before 保证。
//
// 这个访问器补的是一个真实缺口：在此之前的调用方只能靠日志判断"这一轮为什么结束"，
// 而 H0-2/H0-5 的验收要求终态可断言且带身份。
//
// 注意它读的是"引擎最近一次"而不是"你这个 turn 的"：同一个引擎上并发跑两个 run 时
// 两者会混淆。引擎本身也不支持并发 run（SubmitStreamBlocks 会 append e.Messages），
// 调用方应在 run 级别串行（harness.Runner 已内建这道闸）。
func (e *QueryEngine) LastTerminal() types.Terminal {
	if e == nil {
		return types.Terminal{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastTerminal
}
