// stop_signal.go — CaRT 式停止信号检测 (G7)。
//
// 对标:
//   - CaRT: Teaching LLM Agents to Know When They Know Enough (arxiv:2510.08517)
//   - 反事实对: (τ, stop) vs (τ', continue) 对比判定边际信息增益
//
// 当前 QueryEngine 完全依赖模型自主决定何时停止。实际观察到:
//   - 模型会反复 Read 同一文件 (即便 LoopDetector 未触发 — 因 input 略有差异)
//   - Grep 返回结果高度重合仍继续搜
//   - 已足够回答的问题还在 "再确认一下"
//
// 本组件维护轻量统计, 当边际收益递减时输出 soft stop hint,
// 上层可选择是否注入给模型: "you may have enough evidence, consider finalizing"。
//
// 设计原则: 保守 — 宁错放过也少误触, 避免模型过早终止。
package internal_hook

import (
	"fmt"
	"strings"
	"sync"
)

// StopSignalDetector 停止信号检测器。
type StopSignalDetector struct {
	mu sync.Mutex

	// 配置
	MaxFileReadsSamePath int // 同文件被读 N 次即视为冗余 (默认 3)
	MinToolCallsBeforeHint int // 至少经过 N 个工具后才允许 hint (默认 8)
	JaccardOverlapThreshold float64 // grep 结果集合重合阈值 (默认 0.8)
	DiminishingWindow    int // 连续低收益窗口 (默认 3)

	// 状态
	fileReads       map[string]int
	searchResults   []map[string]struct{} // 最近 K 次 grep 结果集合
	totalToolCalls  int
	diminishingCnt  int
	lastSuggestion  string
}

// NewStopSignalDetector 构造默认检测器。
func NewStopSignalDetector() *StopSignalDetector {
	return &StopSignalDetector{
		MaxFileReadsSamePath:    3,
		MinToolCallsBeforeHint:  8,
		JaccardOverlapThreshold: 0.8,
		DiminishingWindow:       3,
		fileReads:               make(map[string]int),
	}
}

// Observe 记录一次工具调用, 返回是否建议终止 + 理由。
//
// suggest=true 时, 上层应在发起下一次 API 调用前注入一条 system hint:
// "<soft_stop_hint>You've gathered substantial evidence about X. Consider finalizing unless critical info is missing.</soft_stop_hint>"
func (d *StopSignalDetector) Observe(toolName, inputSummary, resultSummary string) (suggest bool, reason string) {
	if d == nil {
		return false, ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	d.totalToolCalls++
	if d.totalToolCalls < d.MinToolCallsBeforeHint {
		return false, ""
	}

	tool := strings.ToLower(toolName)

	// 规则 1: 同文件 Read 次数过多
	switch tool {
	case "read", "readfile", "file_read":
		path := extractPath(inputSummary)
		if path != "" {
			d.fileReads[path]++
			if d.fileReads[path] >= d.MaxFileReadsSamePath {
				d.diminishingCnt++
				reason = fmt.Sprintf("already read '%s' %d times", path, d.fileReads[path])
				return d.maybeEmit(reason), reason
			}
		}
	case "grep", "search", "ripgrep":
		// 规则 2: 连续 grep 结果集合高度重合
		hits := extractHitSet(resultSummary)
		if len(hits) == 0 {
			return false, ""
		}
		d.searchResults = append(d.searchResults, hits)
		if len(d.searchResults) > d.DiminishingWindow {
			d.searchResults = d.searchResults[len(d.searchResults)-d.DiminishingWindow:]
		}
		if len(d.searchResults) >= 2 {
			j := jaccard(d.searchResults[len(d.searchResults)-1], d.searchResults[len(d.searchResults)-2])
			if j >= d.JaccardOverlapThreshold {
				d.diminishingCnt++
				reason = fmt.Sprintf("recent search results overlap %.0f%%", j*100)
				return d.maybeEmit(reason), reason
			}
		}
	}

	// 规则 3: 总调用数过多仍未写入任何文件 (编辑/写入类工具缺席)
	if d.totalToolCalls >= 20 {
		reason = fmt.Sprintf("performed %d read-only tool calls without any edit/write", d.totalToolCalls)
		return d.maybeEmit(reason), reason
	}

	// 无明显冗余信号时, 逐步衰减连续低收益计数 (避免一次偶发永远锁死)
	if d.diminishingCnt > 0 && d.totalToolCalls%5 == 0 {
		d.diminishingCnt--
	}
	return false, ""
}

// maybeEmit 只在连续 diminishingWindow 轮都触发时发出建议, 去重重复文本。
func (d *StopSignalDetector) maybeEmit(reason string) bool {
	if d.diminishingCnt < d.DiminishingWindow {
		return false
	}
	if reason == d.lastSuggestion {
		return false
	}
	d.lastSuggestion = reason
	return true
}

// Reset 清空状态 (新 turn 可选择重置)。
func (d *StopSignalDetector) Reset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.fileReads = make(map[string]int)
	d.searchResults = nil
	d.totalToolCalls = 0
	d.diminishingCnt = 0
	d.lastSuggestion = ""
	d.mu.Unlock()
}

// BuildHintMessage 构造要注入的 soft stop hint 文本。
func BuildHintMessage(reason string) string {
	return fmt.Sprintf(
		"<soft_stop_hint>系统观察到: %s。如果你已经掌握足够信息, 建议直接给出最终答复; 如果还缺关键证据, 请解释还需要确认什么。</soft_stop_hint>",
		reason,
	)
}

// extractPath 从 tool input (通常是 JSON) 中抓 path 字段。启发式, 宽松匹配。
func extractPath(input string) string {
	lower := strings.ToLower(input)
	for _, key := range []string{`"path"`, `"file"`, `"filename"`, `"file_path"`} {
		idx := strings.Index(lower, key)
		if idx < 0 {
			continue
		}
		rest := input[idx+len(key):]
		// 找冒号后的第一个字符串值
		co := strings.IndexByte(rest, ':')
		if co < 0 {
			continue
		}
		rest = rest[co+1:]
		q1 := strings.IndexByte(rest, '"')
		if q1 < 0 {
			continue
		}
		rest = rest[q1+1:]
		q2 := strings.IndexByte(rest, '"')
		if q2 < 0 {
			continue
		}
		return rest[:q2]
	}
	return ""
}

// extractHitSet 粗略提取 grep 结果中的唯一行指纹 (文件:行号 的前缀)。
func extractHitSet(result string) map[string]struct{} {
	lines := strings.Split(result, "\n")
	out := make(map[string]struct{})
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		// 典型 ripgrep 行: path/to/file.go:123:code
		parts := strings.SplitN(ln, ":", 3)
		if len(parts) >= 2 {
			out[parts[0]+":"+parts[1]] = struct{}{}
		} else {
			out[ln] = struct{}{}
		}
	}
	return out
}

// jaccard 两个集合的 Jaccard 相似度。
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
