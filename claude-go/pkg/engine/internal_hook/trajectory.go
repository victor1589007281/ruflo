// internal/trajectory.go — 轨迹记忆 (G6) 类型与工具函数。
//
// 对标:
//   - MiniMax M2.7 自进化循环: 每步结构化 markdown 日志, 失败轨迹入库
//   - ReasoningBank (Letta 衍生): trajectory → verdict → distill 流水线
//   - CaRT 反事实轨迹对: (τ, stop) vs (τ', continue)
//
// 当前 QueryEngine 只在 compact 前提取 "关键事实", 丢弃了 turn 级别的结构:
//   - 用户这一轮想做什么?
//   - 模型的计划是什么?
//   - 用了哪些工具?
//   - 最终是成功还是失败?
//   - 失败的症状和根因?
//
// 本组件定义了 Trajectory 结构并提供写入 TieredStore 的便捷方法,
// 成功/失败轨迹用不同 Importance + Source 分级, 便于下轮定向召回。
package internal_hook

import (
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/textutil"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// ToolSig — 单次工具调用签名
// ============================================================================

// ToolSig 记录单次工具调用的关键信息，用于 Trajectory 和 LoopDetector。
type ToolSig struct {
	Name      string    // 工具名
	InputHash string    // 输入参数的哈希值（由 LoopDetector 同款 hash 生成）
	OK        bool      // 是否成功（非 IsError）
	LatencyMs int64     // 执行耗时（毫秒）
	At        time.Time // 调用时间
	Summary   string    // 结果摘要（可选）
}

// ============================================================================
// Trajectory — 单轮对话轨迹
// ============================================================================

// TrajVerdict 轨迹裁决结果。
type TrajVerdict string

const (
	VerdictSuccess TrajVerdict = "success"  // 完全成功
	VerdictPartial TrajVerdict = "partial"  // 部分成功
	VerdictFail    TrajVerdict = "fail"     // 失败
	VerdictAborted TrajVerdict = "aborted"  // 被中止
	VerdictUnknown TrajVerdict = "unknown"  // 未知
)

// Trajectory 单个 turn 的完整轨迹记录。
type Trajectory struct {
	TurnID     string      // 轮次唯一 ID
	SessionID  string      // 会话 ID
	UserIntent string      // 用户原始请求（截断到 400 字）
	Plan       string      // 从 thinking / 首条 assistant text 提取的计划（可选）
	ToolCalls  []ToolSig   // 本轮工具调用列表
	StopReason string      // 停止原因：max_tokens / tool_use / end_turn / error
	Verdict    TrajVerdict // 裁决结果
	Summary    string      // 结构化结论（可选，LLM 后处理填充）
	RootCause  string      // 失败时的推断根因（可选）
	At         time.Time   // 记录时间
	LatencyMs  int64       // 本轮总耗时（毫秒）
}

// ============================================================================
// TrajectoryStore — 写入接口
// ============================================================================

// TrajectoryStore 定义 Trajectory 的持久化接口（可替换为外部向量库实现）。
type TrajectoryStore interface {
	Append(t *Trajectory)
}

// MemoryBackedTrajectoryStore 把 Trajectory 转成 MemoryEntry 写入 TieredStore。
type MemoryBackedTrajectoryStore struct {
	Mem *memory.TieredStore
}

// NewMemoryBackedTrajectoryStore 构造基于内存的 TrajectoryStore。
func NewMemoryBackedTrajectoryStore(mem *memory.TieredStore) *MemoryBackedTrajectoryStore {
	return &MemoryBackedTrajectoryStore{Mem: mem}
}

// Append 按 verdict 分级写入 TieredStore。
func (s *MemoryBackedTrajectoryStore) Append(t *Trajectory) {
	if s == nil || s.Mem == nil || t == nil {
		return
	}

	content := t.Format()
	importance := 0.7
	src := "trajectory"
	switch t.Verdict {
	case VerdictSuccess:
		importance = 0.85
		src = "traj_success"
	case VerdictFail:
		importance = 0.75
		src = "traj_fail"
	case VerdictPartial:
		importance = 0.65
		src = "traj_partial"
	case VerdictAborted:
		importance = 0.5
		src = "traj_aborted"
	}

	s.Mem.Add(&memory.MemoryEntry{
		Content:    content,
		Source:     src,
		Importance: importance,
		ChatID:     t.SessionID,
	})
}

// taskInstructionPointerText 是 engine.taskInstructionPointer 的**字面副本**。
//
// 为什么是副本而不是引用: 该常量定义在 pkg/engine (engine.go), 而本包被
// pkg/engine 依赖 —— 反向 import 会成环。两个包之间的常量同步靠这条注释与
// TestTaskInstructionPointerInSync 守住 (测试里硬编码同一字符串, 改一处忘另一处
// 会直接失败)。
const taskInstructionPointerText = "（任务指令已置于 system prompt 末尾，请按其中的任务目标与产出要求执行。）"

// IsTaskInstructionPointer 判断一段文本是否为 engine 的 TaskInstruction 占位符。
//
// 占位符的含义是"真正的任务指令在 system prompt 末尾"。它出现在 user 消息里只是
// 为了满足"消息序列以非空 user 轮开始"的协议约束, 本身**不含任何任务信息**。
// 任何拿它做检索 key / 意图摘要 / 记忆内容的地方都会得到零信息量的结果 ——
// 见 MemoryInjectHook.retrievalQuery 与 Trajectory.Format。
func IsTaskInstructionPointer(s string) bool {
	return strings.TrimSpace(s) == taskInstructionPointerText
}

// Format 把 Trajectory 格式化为人类可读的 markdown 摘要（也便于被 BM25 检索）。
//
// 两个"稳定性"约束 —— 这段文本会被写进记忆库, 再被检索出来注入 system prompt:
//
//  1. **不含时间戳**。原先第一行是 "[verdict@2026-09-16T12:34:56Z] ...", 时间戳
//     每次都不同, 于是即便是逻辑上同一条记忆, 跨 run 渲染出的字节也不同 ——
//     记忆块所在位置之后的前缀缓存全部失效。这正是"27 对缓存断裂里 8 对是块
//     内容变了"的主因。时间属于元数据 (MemoryEntry.CreatedAt 已有), 不该进正文。
//
//  2. **占位符不进正文**。UserIntent 是 taskInstructionPointer 时, 把它写进正文
//     会让**每一条记忆都以同一句废话开头**, BM25 索引被这句话污染: 任何查询都
//     容易匹配到它, 于是检索结果退化成"随机捞几条"。占位符直接省略。
func (t *Trajectory) Format() string {
	if t == nil {
		return ""
	}
	var sb strings.Builder
	if intent := t.formatIntent(); intent != "" {
		fmt.Fprintf(&sb, "[%s] %s\n", t.Verdict, intent)
	} else {
		fmt.Fprintf(&sb, "[%s]\n", t.Verdict)
	}
	if t.Plan != "" {
		fmt.Fprintf(&sb, "plan: %s\n", TruncateStr(t.Plan, 240))
	}
	if len(t.ToolCalls) > 0 {
		names := make([]string, 0, len(t.ToolCalls))
		okCount := 0
		for _, tc := range t.ToolCalls {
			names = append(names, tc.Name)
			if tc.OK {
				okCount++
			}
		}
		fmt.Fprintf(&sb, "tools(%d/%d ok): %s\n", okCount, len(t.ToolCalls), strings.Join(uniqStrings(names), ","))
	}
	if t.RootCause != "" {
		fmt.Fprintf(&sb, "cause: %s\n", TruncateStr(t.RootCause, 240))
	}
	if t.Summary != "" {
		fmt.Fprintf(&sb, "summary: %s\n", TruncateStr(t.Summary, 240))
	}
	return strings.TrimSpace(sb.String())
}

// ============================================================================
// InferVerdict — 从 stop_reason 推断裁决
// ============================================================================

// InferVerdict 根据 stop_reason、工具调用签名列表和中止标志推断本轮裁决。
func InferVerdict(stopReason string, toolCalls []ToolSig, aborted bool) TrajVerdict {
	if aborted {
		return VerdictAborted
	}
	if len(toolCalls) == 0 {
		switch stopReason {
		case "end_turn", "":
			return VerdictSuccess
		case "max_tokens":
			return VerdictPartial
		case "error":
			return VerdictFail
		default:
			return VerdictUnknown
		}
	}
	okCount := 0
	for _, t := range toolCalls {
		if t.OK {
			okCount++
		}
	}
	ratio := float64(okCount) / float64(len(toolCalls))
	switch {
	case ratio >= 0.9:
		return VerdictSuccess
	case ratio >= 0.5:
		return VerdictPartial
	default:
		return VerdictFail
	}
}

// ============================================================================
// ExtractPlan — 从消息中提取计划
// ============================================================================

// ExtractPlan 从一组 assistant 消息中提取首个 thinking 块或首段文本作为 "plan"。
func ExtractPlan(msgs []types.Message) string {
	for _, m := range msgs {
		if m.Type != types.MessageTypeAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockThinking && b.Thinking != "" {
				return TruncateStr(strings.TrimSpace(b.Thinking), 400)
			}
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockText && b.Text != "" {
				return TruncateStr(strings.TrimSpace(b.Text), 400)
			}
		}
	}
	return ""
}

// ============================================================================
// 辅助函数
// ============================================================================

// formatIntent 返回可写入正文的意图描述。
//
// 占位符返回空串 —— 它不是意图, 是"意图在别处"的指针。把指针当内容写进记忆,
// 只会让每条记忆都长着同一张脸。
func (t *Trajectory) formatIntent() string {
	if t.UserIntent == "" || IsTaskInstructionPointer(t.UserIntent) {
		return ""
	}
	return TruncateStr(t.UserIntent, 160)
}

// TruncateStr 截断字符串到指定长度 (按**字符**), 超出部分加 "..."。
func TruncateStr(s string, n int) string {
	return textutil.Truncate(s, n, "...")
}

// uniqStrings 去重字符串切片（保持顺序）。
func uniqStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
