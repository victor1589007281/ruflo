// automode.go — AutoModeHook: 引擎级"复杂度自判断 → 自动 Plan Mode + Advisor"能力。
//
// 目标: 对齐 Claude 客户端的行为 —— 会话思考中引擎自行判断任务复杂度, 复杂任务
// 自动进入只读规划阶段, 并可选自动咨询 advisor; 计划就绪后由模型调用 ExitPlanMode
// 自动转入实施, 全程无需人为参与。这是推理引擎的内建能力, 不依赖任何入口外挂。
//
// 触发时机: PhasePreRequest + TurnCount==0。每个新的 SubmitMessage 是一次任务边界,
// hook 在此重置并重新判定 —— 这同时规避了"hook 状态跨 queryLoop 泄漏"的问题。
//
// 注入通道 (致命约束): PhasePreRequest 里 HookResult.AppendMsgs 只推流 (ch <- m),
// 不会进入 messages (engine.go PhasePreRequest 合并规则) —— 因此规划指令必须走
// HookResult.Messages **全量替换**。
//
// Plan Mode 单一状态源: hook 通过装配方注入的 SetPlanMode 回调写"会话级 plan flag"
// (通常是 builtin.SetPlanModeForSession), 与模型调用的 EnterPlanMode / ExitPlanMode
// 工具读写**同一个** flag。于是:
//   - hook 判定复杂 → SetPlanMode(true) → DynamicPlanCheck 读到 → 只读强制生效;
//   - 模型规划完调 ExitPlanMode → 清同一 flag → 自动放行实施, 无需人为确认。
//
// 防死锁: 模型即使不调 ExitPlanMode, 下一条用户消息 TurnCount==0 时 hook 也会重置,
// 不会永久锁只读。
package internal_hook

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/complexity"
	"github.com/anthropic/claude-go/pkg/types"
)

// AutoModeHintFormat 注入给模型的规划引导。%s1=判定原因, %s2=计划文件提示(可为空), %s3=advisor 建议小节(可为空)。
// 注意措辞: 规划期**不是**绝对只读 —— 计划文件目录 (PlanFileDir) 内允许 Write/Edit,
// 模型可在此起草/更新计划草稿 (对齐 Claude 客户端 plans 目录语义)。
const AutoModeHintFormat = `<system-reminder>检测到复杂任务（原因：%s），已进入规划模式（只读，唯一例外：计划文件目录内可写）。
请先使用只读工具调研并制定一份可执行方案；可在计划文件目录起草/更新计划草稿，方案就绪后调用 ExitPlanMode 提交计划，然后进入实施环节并按需验证。
%s
需要方向判断或对方案拿不准时，可调用 advisor 获取更强调模型的建议。%s</system-reminder>`

// AutoModeHook 在 PhasePreRequest 判定任务复杂度, 复杂则开启只读规划并注入引导。
type AutoModeHook struct {
	// Enabled 总开关 (对应 Config.AutoPlanMode)。
	Enabled bool
	// Mode 复杂度判定模式: heuristic|llm|hybrid (空=heuristic)。
	Mode string
	// LLMClassify 可选 LLM 分类器: (是否复杂, 原因, 错误)。对应 Config.LLMComplexityFn。
	LLMClassify func(ctx context.Context, text string) (bool, string, error)
	// AutoAdvisor 复杂任务是否自动咨询 advisor 并注入建议 (对应 Config.AutoAdvisor)。
	AutoAdvisor bool
	// AdvisorConsult 咨询 advisor 的函数 (通常是 builtin.AdvisorTool.Consult)。
	// 与 AdvisorCheckpointHook 共享同一 MaxCallsPerSession 预算。
	AdvisorConsult AdvisorConsultFn
	// SetPlanMode 写会话级 plan flag 的回调 (装配方注入, 绑定到 builtin planSessions)。
	SetPlanMode func(on bool)
	// PlanFileDir 计划文件目录。非空时提醒模型 ExitPlanMode 会把计划落盘, 实施阶段先读取。
	PlanFileDir string
	// Metrics 引擎级指标 (可 nil)。
	Metrics *EngineMetrics

	// autoPlanActive 本 hook 为当前任务开启的规划状态。仅用于 TurnCount==0 重置判断。
	autoPlanActive bool
}

// NewAutoModeHook 构造 AutoModeHook。Enabled 为 false 时返回 nil (不注册)。
func NewAutoModeHook(enabled bool, mode string, llmClassify func(ctx context.Context, text string) (bool, string, error),
	autoAdvisor bool, consult AdvisorConsultFn, setPlanMode func(on bool), planFileDir string, metrics *EngineMetrics) *AutoModeHook {
	if !enabled {
		return nil
	}
	return &AutoModeHook{
		Enabled:        enabled,
		Mode:           mode,
		LLMClassify:    llmClassify,
		AutoAdvisor:    autoAdvisor,
		AdvisorConsult: consult,
		SetPlanMode:    setPlanMode,
		PlanFileDir:    planFileDir,
		Metrics:        metrics,
	}
}

func (h *AutoModeHook) Name() string { return "auto_mode" }

// Priority 45: PhasePreRequest 里位于 MessageFilter(40) 之后、MessageMetrics(48)
// 之前 —— 输入消息已过滤, 判定结果可被后续 hook 观测到。
func (h *AutoModeHook) Priority() int { return 45 }

func (h *AutoModeHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreRequest}
}

// Execute 判定复杂度并按需开启规划 / 注入引导。
func (h *AutoModeHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h == nil || !h.Enabled {
		return nil, nil
	}
	// 仅首轮判定: 每个 SubmitMessage 只判一次, 避免 tool_use 循环中反复注入膨胀上下文。
	if ctx.Phase != PhasePreRequest || ctx.TurnCount != 0 || len(ctx.Messages) == 0 {
		return nil, nil
	}

	lastUserText := lastNonMetaUserText(ctx.Messages)
	if lastUserText == "" || lastUserText == "." { // "." = 空 user 输入占位符
		return nil, nil
	}

	// 新任务边界: 重置上个任务遗留的自动规划状态 (即使模型没调 ExitPlanMode, 也不永久锁只读)。
	if h.autoPlanActive {
		h.autoPlanActive = false
		if h.SetPlanMode != nil {
			h.SetPlanMode(false)
		}
	}

	verdict := complexity.Judge(ctx.Ctx, lastUserText, complexity.Options{
		Mode: h.Mode,
		LLM:  h.LLMClassify,
	})
	if h.Metrics != nil {
		h.Metrics.ComplexityChecks.Add(1)
	}
	if !verdict.Complex {
		return nil, nil
	}

	h.autoPlanActive = true
	if h.SetPlanMode != nil {
		h.SetPlanMode(true)
	}
	if h.Metrics != nil {
		h.Metrics.AutoPlanEntered.Add(1)
	}

	advisorSection := ""
	if h.AutoAdvisor && h.AdvisorConsult != nil {
		if advice, err := h.AdvisorConsult(ctx.Ctx, ctx.Messages); err == nil && strings.TrimSpace(advice) != "" {
			advisorSection = fmt.Sprintf("\n\nadvisor 建议（强模型审阅了你当前任务后的方向性指导，请认真权衡）：\n%s", advice)
			if h.Metrics != nil {
				h.Metrics.AutoAdvisorInjected.Add(1)
			}
		}
	}

	planNote := ""
	if strings.TrimSpace(h.PlanFileDir) != "" {
		planNote = fmt.Sprintf("规划期 Write/Edit 仅放行计划文件目录（%s）内文件，可在此起草/更新计划草稿；调用 ExitPlanMode 时正式计划会保存到该目录，实施阶段请先读取并严格按其执行。", h.PlanFileDir)
	}

	hint := types.Message{
		Type: types.MessageTypeUser,
		UUID: GenerateUUID(),
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: fmt.Sprintf(AutoModeHintFormat, verdict.Reason, planNote, advisorSection),
		}},
		IsMeta:    true,
		CreatedAt: time.Now(),
	}

	// PhasePreRequest 的 AppendMsgs 不达模型上下文 → 必须 Messages 全量替换。
	newMsgs := make([]types.Message, 0, len(ctx.Messages)+1)
	newMsgs = append(newMsgs, ctx.Messages...)
	newMsgs = append(newMsgs, hint)
	return &HookResult{Messages: newMsgs}, nil
}

// lastNonMetaUserText 反向扫最后一条非 meta user 消息的文本内容。
func lastNonMetaUserText(messages []types.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Type != types.MessageTypeUser || m.IsMeta {
			continue
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockText && strings.TrimSpace(b.Text) != "" {
				return b.Text
			}
		}
		return "" // 该 user 消息无文本块 (如 tool_result), 继续向前找更早的
	}
	return ""
}
