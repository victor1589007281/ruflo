// hook_compact.go — 上下文压缩相关内置 Hook。
//
// 包含:
//   - MicroCompactHook    截断过大 tool_result（轻量级）
//   - AutoCompactHook     上下文自动压缩 + 关键事实蒸馏
//   - BudgetDegradeHook   Token 预算分级降级（紧急摘要）
package internal_hook

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// CompactionSink — 折叠读数出口 (H2-1)
// ============================================================================

// CompactionSink 接收一次折叠的读数。
//
// **没有返回值, 且实现方不得阻塞** —— 与 Observer 同一条纪律 (见 bus.go 文件头):
// 采集是观测不是治理, 在这里返回 error 等于给最热路径造出一条谁都没设计过的失败
// 路径。实现方 (引擎侧) 自己做 fail-open。
//
// 与 Observer 分开的两个理由:
//   - Observer 的事件载荷是 {Scope, Phase, Hook, Err, TurnCount} (全字符串), 硬塞
//     七个数值字段要么把 Event 变成万能袋, 要么在旁边开第二个结构体; 而它已经有
//     明确的语义 (哪个 hook 被触发了)。
//   - 观测者由 **ctx** 携带 (per-request), 折叠读数则要在**没有 ctx 可供注入**的
//     地方也能报 (Compactor 是 per-engine 实例, 装配时机早于任何一次请求)。
type CompactionSink interface {
	// RecordCompaction 报一次折叠。fold 语义见 CompactionStage* 常量。
	RecordCompaction(ctx context.Context, ev CompactionEvent)
}

// 折叠阶段。两个值对应两条**各自独立**的折叠路径, 读数不能相加成"总折叠次数" ——
// 一次 PhasePreCompact 里两个都可能发生, 量纲不同 (一个是"把历史换成摘要", 一个
// 是"把大 tool_result 换成占位")。
const (
	// CompactionStageFold 整段历史折叠为摘要 (compact.Compactor.AutoCompact)。
	CompactionStageFold = "fold"
	// CompactionStageMicro 单个过大 tool_result 折叠为占位 (compact.MicroCompact*)
	// —— 唯一会真正产出可回读文件引用的那条路径。
	CompactionStageMicro = "micro"
)

// CompactionEvent 一次折叠的读数 + 发生位置。
type CompactionEvent struct {
	// Stage CompactionStageFold / CompactionStageMicro。
	Stage string
	// TurnCount 第几轮触发 (0 = 不在 turn 内, 例如 PTL 应急压缩)。
	TurnCount int
	// Model 当时的模型标识 (折叠摘要那次调用的模型)。
	Model string
	// Stats 折叠读数本体 (条数 / token / 引用保全), 口径见 pkg/compact。
	Stats compact.CompactionStats
}

// ============================================================================
// MicroCompactHook — 轻量级消息截断
// ============================================================================

// MicroCompactHook 对 tool_result 内容做字符截断，属于 PhasePreCompact 的第一步。
//
// 折叠前会把完整原文落盘 (refStore), 占位里给出可 Read 回读的路径 —— 否则
// 超出保留头部的部分直接销毁, 模型需要时只能重跑一次工具 (再花一轮, 再产生
// 一次同样的输出, 又被折掉)。
type MicroCompactHook struct {
	refStore *compact.ToolResultRefStore
	sink     CompactionSink
}

// SetCompactionSink 装配读数出口 (构造后调用; nil = 不报, 与改造前一致)。
func (h *MicroCompactHook) SetCompactionSink(s CompactionSink) { h.sink = s }

// HasCompactionSink 报告出口是否已装配 —— 供"接线有没有接上"这一类断言使用。
func (h *MicroCompactHook) HasCompactionSink() bool { return h != nil && h.sink != nil }

// NewMicroCompactHook 创建 MicroCompactHook。
//
// cwd 为空 (纯测试 / 未配工作区) 时不落盘, 行为与改造前逐字节相同。
func NewMicroCompactHook(cwd string) *MicroCompactHook {
	return &MicroCompactHook{refStore: compact.NewToolResultRefStoreForCwd(cwd)}
}

// Name 返回 hook 名 "micro_compact"。
func (h *MicroCompactHook) Name() string  { return "micro_compact" }
// Priority 返回执行优先级 20: 同一阶段内数值越小越先执行。
func (h *MicroCompactHook) Priority() int { return 20 }
// Phases 声明本 hook 只挂在 PhasePreCompact 阶段。
func (h *MicroCompactHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreCompact}
}

// Execute 折叠本轮新增消息里过大的 tool_result。
//
// 只动 [SentUpTo, len) 区间。此前它是全量遍历 + 两级递减阈值 (16000, 总量仍超
// 30000 再降到 8000), 结果是同一条 tool_result 在 run 中途被改写一次 —— 从改写
// 位置起的缓存全废 (实测 if-question-expand 第 3 轮 12151B → 8075B)。
//
// 折叠产物是 call_id 的纯函数 (compact.ToolResultStub), 所以对同一段新消息重复
// 调用结果一致; 反过来, 已发出的历史不再有任何路径能改到它。
func (h *MicroCompactHook) Execute(ctx *HookContext) (*HookResult, error) {
	// 自指保护 (本轮有人在读折叠引用就不落盘) 由 MicroCompactWithRef 统一处理,
	// 见 compact.RefDirReadCallIDs —— 缺了它读回的原文会被再折一次, 引用指向引用。
	before := compact.EstimateTokens(ctx.Messages)
	messages, counts := compact.StubToolResultsCounted(ctx.Messages, ctx.SentUpTo, -1, h.refStore)
	if counts.Folded > 0 && h.sink != nil {
		st := compact.CompactionStats{
			MessagesBefore: len(ctx.Messages),
			MessagesAfter:  len(messages), // 折叠是原地换内容, 条数不变
			TokensBefore:   before,
			TokensAfter:    compact.EstimateTokens(messages),
			Stub:           counts,
		}
		// 折叠后总 token 数**变大**是可能的 (占位文本 + 回读路径提示比短结果长),
		// 但 count>0 说明确实折过, 读数照样要报 —— 报的是事实不是"省了多少"。
		h.sink.RecordCompaction(ctx.Ctx, CompactionEvent{
			Stage:     CompactionStageMicro,
			TurnCount: ctx.TurnCount,
			Model:     ctx.Model,
			Stats:     st,
		})
	}
	if len(messages) == len(ctx.Messages) {
		// 长度不变不代表内容没变 (tool_result 是原地折叠), 逐个比对一次块内容
		// 才能判断是否真的改了什么; 没改就返回 nil, 让 HookChain 跳过覆盖写。
		changed := false
		for i := range messages {
			if len(messages[i].Content) != len(ctx.Messages[i].Content) {
				changed = true
				break
			}
			for j := range messages[i].Content {
				if messages[i].Content[j].Content != ctx.Messages[i].Content[j].Content {
					changed = true
					break
				}
			}
			if changed {
				break
			}
		}
		if !changed {
			return nil, nil
		}
	}
	logging.For("engine").Debug("MicroCompact applied",
		"sentUpTo", ctx.SentUpTo, "messages", len(messages))
	return &HookResult{Messages: messages}, nil
}

// ============================================================================
// AutoCompactHook — 自动压缩与关键事实蒸馏
// ============================================================================

// AutoCompactHook 在 PhasePreCompact 阶段调用 Compactor 进行上下文压缩，
// 并将压缩前的关键事实提取到 L1（TieredStore）和 L2（FactStore）记忆系统。
type AutoCompactHook struct {
	compactor   *compact.Compactor
	memoryStore *memory.TieredStore
	ingestor    *memory.Ingestor
	hookRunner  *hooks.Runner // 外部 HookRunner，用于调用外部 PreCompact hooks
	roster      TaskRosterSource
	sink        CompactionSink
}

// SetCompactionSink 装配折叠读数出口 (H2-1; nil = 不报, 与改造前一致)。
func (h *AutoCompactHook) SetCompactionSink(s CompactionSink) { h.sink = s }

// HasCompactionSink 报告出口是否已装配 —— 见 engine.HasCompactionSink 的说明。
func (h *AutoCompactHook) HasCompactionSink() bool { return h != nil && h.sink != nil }

// TaskRosterSource 提供"当前还剩什么没做完"的快照。
//
// 用函数注入而不是直接依赖 *builtin.TodoWriteTool: 那个类型在 tool/builtin,
// 而 builtin 经 evotools → evolution/govern 与引擎侧成环。引擎只认这个签名,
// 由装配处 (cmd / feishu) 把真实 todo 工具的方法值传进来。
//
// 返回空串/nil 表示没有清单 —— 此时行为与改造前逐字节相同。
type TaskRosterSource func() []TaskRosterItem

// TaskRosterItem 是清单里的一条。
type TaskRosterItem struct {
	ID      string
	Content string
	Status  string
}

// SetTaskRosterSource 装配清单来源。构造后调用。
func (h *AutoCompactHook) SetTaskRosterSource(src TaskRosterSource) {
	h.roster = src
}

// renderTaskRoster 把清单渲染成摘要里的一段。
//
// 只在**还有未完成项**时才渲染。全部做完时返回空串: 那时清单没有信息量,
// 而"全部完成"的误读风险 (模型以为可以收尾了) 大于它带来的帮助。
func renderTaskRoster(items []TaskRosterItem) (string, int) {
	if len(items) == 0 {
		return "", 0
	}
	pending := make([]TaskRosterItem, 0, len(items))
	done := 0
	for _, it := range items {
		switch it.Status {
		case "completed":
			done++
		case "cancelled", "deleted":
			// 既不算未完成也不算完成 —— 已作废的条目不该出现在"还剩什么"里。
		default:
			pending = append(pending, it)
		}
	}
	if len(pending) == 0 {
		return "", 0
	}
	var sb strings.Builder
	sb.WriteString("<task_roster>\n")
	sb.WriteString("以下是**压缩发生时**任务清单里尚未完成的条目。这是一份状态快照，不是新指令；\n")
	sb.WriteString("继续按原目标推进，并在完成后用 TodoWrite 更新状态。\n\n")
	for _, it := range pending {
		status := it.Status
		if status == "" {
			status = "unknown"
		}
		fmt.Fprintf(&sb, "- [%s] %s\n", status, it.Content)
	}
	sb.WriteString("</task_roster>")
	return sb.String(), len(pending)
}

// NewAutoCompactHook 创建 AutoCompactHook。
func NewAutoCompactHook(
	compactor *compact.Compactor,
	memoryStore *memory.TieredStore,
	ingestor *memory.Ingestor,
	hookRunner *hooks.Runner,
) *AutoCompactHook {
	return &AutoCompactHook{
		compactor:   compactor,
		memoryStore: memoryStore,
		ingestor:    ingestor,
		hookRunner:  hookRunner,
	}
}

// Name 返回 hook 名 "auto_compact"。
func (h *AutoCompactHook) Name() string  { return "auto_compact" }
// Priority 返回执行优先级 10(高于 micro_compact, 先于它执行)。
func (h *AutoCompactHook) Priority() int { return 10 }
// Phases 声明本 hook 只挂在 PhasePreCompact 阶段。
func (h *AutoCompactHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreCompact}
}

// Execute 先检查外部 PreCompact Hook 是否阻止压缩，若未阻止则执行 AutoCompact
// 并将关键事实蒸馏到记忆系统。
func (h *AutoCompactHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.compactor == nil {
		return nil, nil
	}

	// H2-1 读数出口: 装在 compactor 上而不是在这里报, 因为折叠还有另外两条
	// 调用路径 (PTL 应急压缩 / queryLoop 直调) —— 报在这里就漏掉那两条。
	// 装配是幂等的, 重复调用无害。
	h.compactor.SetOnCompact(func(cctx context.Context, st compact.CompactionStats) {
		if h.sink == nil {
			return
		}
		h.sink.RecordCompaction(cctx, CompactionEvent{
			Stage:     CompactionStageFold,
			TurnCount: ctx.TurnCount,
			Model:     ctx.Model,
			Stats:     st,
		})
	})

	// 外部 PreCompact Hook 干预：若返回 block/deny，跳过本轮压缩
	if h.hookRunner != nil {
		hookOut := h.hookRunner.ExecutePreCompactHooks(ctx.Messages)
		if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
			logging.For("engine").Warn("PreCompact blocked by hook", "reason", hookOut.Reason)
			return &HookResult{SkipRemaining: true}, nil
		}
	}

	compacted, err := h.compactor.AutoCompact(ctx.Ctx, ctx.Messages, ctx.Model)
	if err != nil || compacted == nil {
		return nil, err
	}

	// PreCompact 蒸馏: 提取关键事实到 L1 + L2
	cutoff := len(ctx.Messages) - 6
	if cutoff > 0 {
		facts := h.compactor.SmartExtractKeyFacts(ctx.Ctx, ctx.Messages[:cutoff])
		// L1: TieredStore
		if h.memoryStore != nil {
			for _, fact := range facts {
				h.memoryStore.Add(&memory.MemoryEntry{
					Content:    fact,
					Source:     "pre_compact",
					Importance: 0.7,
				})
			}
		}
		// L2: FactStore（通过 Ingestor 分类）
		if h.ingestor != nil && len(facts) > 0 {
			h.ingestor.IngestFacts(facts, "pre_compact")
		}
	}

	return &HookResult{Messages: reinjectTaskRoster(h.compactor, compacted, h.roster)}, nil
}

// rosterMessageUUID 是清单消息的固定 UUID。
//
// **刻意用常量而不是带计数的唯一值。** 这条消息在压缩边界之后**每一轮**都会被
// 重新插进请求里; 若 UUID 每轮不同, 前缀缓存从插入位置起全废 —— 而那正是压缩
// 之后最贵的一段 (整个摘要)。固定 UUID + 内容由清单状态决定 ⇒ 清单没变时
// 逐字节相同, 缓存续得上。
const rosterMessageUUID = "task-roster-snapshot"

// reinjectTaskRoster 在压缩产物里补一段"还剩什么没做完"。
//
// 为什么必须在这里做: 压缩会把摘要之前的一切折成一段文本, 而清单原先活在
// 摘要之前的历史里 (模型写过的 TodoWrite 调用)。折叠之后它对模型不可见了 ——
// 长任务跑到第 80 轮时"还剩什么"就是这个后果。行业侧 (Crush) 的做法是把清单
// 嵌进摘要本身, 这里同构。
//
// 位置: 摘要消息之后、tail 之前。放在 tail 之后会改变 tail 的字节, 而 tail 是
// 模型当下正在推理的上下文、也是缓存最热的区段。
func reinjectTaskRoster(c *compact.Compactor, messages []types.Message, src TaskRosterSource) []types.Message {
	if src == nil || c == nil {
		return messages
	}
	roster, pending := renderTaskRoster(src())
	if roster == "" || pending == 0 {
		return messages
	}

	// 幂等: 清单消息已存在就整条换掉, 不再插一条新的 —— 否则反复压缩会把
	// 同一份清单叠加成 N 条。
	out := make([]types.Message, 0, len(messages)+1)
	replaced := false
	for _, m := range messages {
		if m.UUID == rosterMessageUUID {
			if replaced {
				continue // 重复的旧副本直接丢弃
			}
			replaced = true
			m.Content = []types.ContentBlock{{Type: types.ContentBlockText, Text: roster}}
			out = append(out, m)
			continue
		}
		out = append(out, m)
	}
	if !replaced {
		msg := types.Message{
			Type:    types.MessageTypeUser,
			UUID:    rosterMessageUUID,
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: roster}},
		}
		out = insertAfterSummary(out, msg)
	}

	logging.For("engine").Info("任务清单已嵌入压缩摘要",
		"pending", pending, "messages", len(out))
	return out
}

// insertAfterSummary 把 msg 插到摘要消息之后 (找不到摘要就插在开头)。
func insertAfterSummary(messages []types.Message, msg types.Message) []types.Message {
	at := 0
	for i, m := range messages {
		if m.UUID != "" && strings.HasPrefix(m.UUID, "summary-") {
			at = i + 1
			break
		}
	}
	out := make([]types.Message, 0, len(messages)+1)
	out = append(out, messages[:at]...)
	out = append(out, msg)
	out = append(out, messages[at:]...)
	return out
}

// ============================================================================
// BudgetDegradeHook — Token 预算分级降级
// ============================================================================

// BudgetDegradeHook 在 PhasePreCompact 阶段检测 Token 预算状态。
// 当预算达到 Red/Critical 级别时，先触发外部 OnContextOverflow Hook，
// 若未阻止则调用 Budget.Degrade 生成紧急摘要消息。
type BudgetDegradeHook struct {
	budget     *TokenBudgetManager
	hookRunner *hooks.Runner  // 外部 HookRunner
	metrics    *EngineMetrics // 引擎指标
}

// NewBudgetDegradeHook 创建 BudgetDegradeHook。
func NewBudgetDegradeHook(budget *TokenBudgetManager, hookRunner *hooks.Runner, metrics *EngineMetrics) *BudgetDegradeHook {
	return &BudgetDegradeHook{budget: budget, hookRunner: hookRunner, metrics: metrics}
}

// Name 返回 hook 名 "budget_degrade"。
func (h *BudgetDegradeHook) Name() string  { return "budget_degrade" }
// Priority 返回执行优先级 30(排在 micro/auto 之后兜底)。
func (h *BudgetDegradeHook) Priority() int { return 30 }
// Phases 声明本 hook 只挂在 PhasePreCompact 阶段。
func (h *BudgetDegradeHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreCompact}
}

// Execute 检测预算级别，触发外部干预，必要时执行降级并记录指标。
func (h *BudgetDegradeHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h.budget == nil {
		return nil, nil
	}

	level := h.budget.Level(ctx.Messages)
	if level < BudgetRed {
		return nil, nil
	}

	// 外部 OnContextOverflow Hook 干预
	if h.hookRunner != nil {
		hookOut := h.hookRunner.ExecuteOnContextOverflowHooks(ctx.Messages, int(level))
		if hookOut != nil && (hookOut.Decision == "block" || hookOut.Decision == "deny") {
			logging.For("engine").Warn("OnContextOverflow blocked degrade by hook", "reason", hookOut.Reason)
			return &HookResult{SkipRemaining: true}, nil
		}
	}

	degraded := h.budget.Degrade(ctx.Messages, level)
	if h.metrics != nil {
		h.metrics.RecordBudgetDegrade(int(level))
	}

	logging.For("engine").Debug("BudgetDegrade applied", "level", level, "before", len(ctx.Messages), "after", len(degraded))
	return &HookResult{Messages: degraded}, nil
}
