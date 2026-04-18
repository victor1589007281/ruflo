// trajectory.go — 轨迹记忆 (G6)。
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
package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/types"
)

// ToolSig 单次工具调用的简化记录。
type ToolSig struct {
	Name      string
	InputHash string        // 由 LoopDetector 同款 hash
	OK        bool          // 是否成功 (非 IsError)
	LatencyMs int64         // 执行耗时
	At        time.Time
	Summary   string        // 结果摘要 (可选)
}

// Trajectory 单个 turn 的完整轨迹。
type Trajectory struct {
	TurnID     string
	SessionID  string
	UserIntent string        // 用户原始请求 (截断到 400 字)
	Plan       string        // 从 thinking / 首条 assistant text 提取 (可选)
	ToolCalls  []ToolSig
	StopReason string        // max_tokens / tool_use / end_turn / error
	Verdict    TrajVerdict
	Summary    string        // 结构化结论 (可选, LLM 后处理填充)
	RootCause  string        // 失败时的推断根因 (可选)
	At         time.Time
	LatencyMs  int64
}

// TrajVerdict 轨迹裁决。
type TrajVerdict string

const (
	VerdictSuccess TrajVerdict = "success"
	VerdictPartial TrajVerdict = "partial"
	VerdictFail    TrajVerdict = "fail"
	VerdictAborted TrajVerdict = "aborted"
	VerdictUnknown TrajVerdict = "unknown"
)

// TrajectoryStore 写入接口 (可替换为外部向量库实现)。
type TrajectoryStore interface {
	Append(t *Trajectory)
}

// MemoryBackedTrajectoryStore 把 Trajectory 转成 MemoryEntry 写入 TieredStore。
type MemoryBackedTrajectoryStore struct {
	Mem *memory.TieredStore
}

// NewMemoryBackedTrajectoryStore 构造。
func NewMemoryBackedTrajectoryStore(mem *memory.TieredStore) *MemoryBackedTrajectoryStore {
	return &MemoryBackedTrajectoryStore{Mem: mem}
}

// Append 按 verdict 分级写入。
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

// Format 把 Trajectory 格式化为人类可读的 markdown 摘要 (也便于被 BM25 检索)。
func (t *Trajectory) Format() string {
	if t == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s@%s] %s\n", t.Verdict, t.At.Format(time.RFC3339), truncateStr(t.UserIntent, 160))
	if t.Plan != "" {
		fmt.Fprintf(&sb, "plan: %s\n", truncateStr(t.Plan, 240))
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
		fmt.Fprintf(&sb, "cause: %s\n", truncateStr(t.RootCause, 240))
	}
	if t.Summary != "" {
		fmt.Fprintf(&sb, "summary: %s\n", truncateStr(t.Summary, 240))
	}
	return strings.TrimSpace(sb.String())
}

// InferVerdict 从 stop_reason + tool error 比例推断 verdict。
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

// ExtractPlan 从一组 assistant 消息中提取首个 thinking 块或首段文本作为 "plan"。
func ExtractPlan(msgs []types.Message) string {
	for _, m := range msgs {
		if m.Type != types.MessageTypeAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockThinking && b.Thinking != "" {
				return truncateStr(strings.TrimSpace(b.Thinking), 400)
			}
		}
		for _, b := range m.Content {
			if b.Type == types.ContentBlockText && b.Text != "" {
				return truncateStr(strings.TrimSpace(b.Text), 400)
			}
		}
	}
	return ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

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
