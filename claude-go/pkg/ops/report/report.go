// Package report 把证据包渲染成领域无关的 Markdown 报告。
//
// 三个领域（MySQL/K8s/Linux）共用同一套门禁与引用规则：结论段必须能被
// evidence_id 追溯，未过门控时明确写出来，而不是换一种更顺耳的措辞。
package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// RenderMarkdown 渲染报告。title 例如 "MySQL RCA"；timelineCols 为空时
// 时间线按 key=value 列表输出，避免把某个领域的列名硬编码进通用层。
func RenderMarkdown(title string, b *evidence.Bundle, timelineCols []string) string {
	if b == nil {
		return "# " + title + "\n\n证据包为空。\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — %s\n\n", title, b.Instance)
	fmt.Fprintf(&sb, "- incident: `%s`\n", b.IncidentID)
	fmt.Fprintf(&sb, "- 生成时间: %s\n", b.GeneratedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&sb, "- 窗口: %s → %s\n\n",
		b.Window.Start.Format("2006-01-02 15:04:05"), b.Window.End.Format("2006-01-02 15:04:05"))

	fmt.Fprintf(&sb, "## 结论\n\n%s\n\n", b.Summary)

	roots := rootCauses(b)
	if len(roots) == 0 {
		sb.WriteString("> 没有假设通过根因门控（supported + E3 支持 + 无未解释 high 矛盾）。\n")
		sb.WriteString("> 下面是已取证事实与未决项；不要在此状态下给出确定性根因。\n\n")
	} else {
		sb.WriteString("### 根因（已过门控）\n\n")
		for _, h := range roots {
			fmt.Fprintf(&sb, "- **%s** %s（support: %s）\n", h.ID, h.Statement, strings.Join(h.Support, ", "))
		}
		sb.WriteString("\n")
	}

	renderCollection(&sb, b, false)

	sb.WriteString("## 证据\n\n")
	sb.WriteString("| ID | 等级 | 类型 | 来源 | 可复核读数 |\n")
	sb.WriteString("|---|---|---|---|---|\n")
	evs := append([]evidence.Evidence{}, b.Evidences...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].ID < evs[j].ID })
	for _, e := range evs {
		valid := "是"
		if !e.ProbeValid {
			valid = "**否**"
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %s | %s（探针有效: %s） |\n",
			e.ID, e.Grade, e.Type, e.Source, mdEscape(e.Summary), valid)
	}
	sb.WriteString("\n")

	if len(b.Timeline) > 0 {
		sb.WriteString("## 时间线\n\n")
		if len(timelineCols) > 0 {
			sb.WriteString("| 时间 | " + strings.Join(timelineCols, " | ") + " |\n")
			sb.WriteString("|---|" + strings.Repeat("---|", len(timelineCols)) + "\n")
			for _, p := range b.Timeline {
				sb.WriteString("| " + p.At.Format("15:04:05") + " | ")
				for _, c := range timelineCols {
					fmt.Fprintf(&sb, "%.0f | ", p.Metrics[c])
				}
				sb.WriteString("\n")
			}
		} else {
			for _, p := range b.Timeline {
				fmt.Fprintf(&sb, "- %s: %s\n", p.At.Format("15:04:05"), formatMetrics(p.Metrics))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## 假设账本\n\n")
	if len(b.Hypotheses) == 0 {
		sb.WriteString("无候选假设（未触发任何异常模式）。\n\n")
	}
	for _, h := range b.Hypotheses {
		fmt.Fprintf(&sb, "### %s · %s\n\n", h.ID, h.Statement)
		fmt.Fprintf(&sb, "- 状态: **%s**，confidence %.2f", h.Status, h.Confidence)
		if h.RootCause {
			if ok, why := b.RootCauseEligible(h); ok {
				fmt.Fprintf(&sb, "，根因门控: **通过**")
			} else {
				fmt.Fprintf(&sb, "，根因门控: **未通过**（%s）", strings.Join(why, "; "))
			}
		}
		sb.WriteString("\n")
		if h.Mechanism != "" {
			fmt.Fprintf(&sb, "- 机制: %s\n", h.Mechanism)
		}
		fmt.Fprintf(&sb, "- 支持证据: %s\n", mdIDs(h.Support))
		fmt.Fprintf(&sb, "- 反证证据: %s\n", mdIDs(h.Contradict))
		fmt.Fprintf(&sb, "- 若成立应观察到: %s\n", strings.Join(h.Predicts, "；"))
		fmt.Fprintf(&sb, "- 可推翻条件: %s\n", strings.Join(h.Disconfirms, "；"))
		if len(h.TestPlan) > 0 {
			fmt.Fprintf(&sb, "- 下一步取证: %s\n", strings.Join(h.TestPlan, "；"))
		}
		sb.WriteString("\n")
	}

	if len(b.Contradictions) > 0 {
		sb.WriteString("## 矛盾\n\n")
		for _, c := range b.Contradictions {
			fmt.Fprintf(&sb, "- **%s**（%s）%s — `%s` vs `%s`", c.ID, c.Severity, c.Description, c.EvidenceA, c.EvidenceB)
			if c.Resolution != "" {
				fmt.Fprintf(&sb, "；处理: %s", c.Resolution)
			} else {
				sb.WriteString("；**未解释**")
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	if len(b.Missing) > 0 {
		sb.WriteString("## 证据缺口\n\n")
		for _, m := range b.Missing {
			fmt.Fprintf(&sb, "- **%s**：%s → %s\n", m.What, m.Why, m.NextAction)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## 复现\n\n")
	sb.WriteString("证据由确定性采集器产出，可按下列来源重跑（只读）：\n\n")
	sources := map[string]bool{}
	for _, e := range b.Evidences {
		sources[e.Source] = true
	}
	var srcs []string
	for s := range sources {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, s := range srcs {
		fmt.Fprintf(&sb, "- %s\n", s)
	}
	return sb.String()
}

func formatMetrics(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if v == float64(int64(v)) {
			parts = append(parts, fmt.Sprintf("%s=%.0f", k, v))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%.2f", k, v))
		}
	}
	return strings.Join(parts, " ")
}

func rootCauses(b *evidence.Bundle) []evidence.Hypothesis {
	var out []evidence.Hypothesis
	for _, h := range b.Hypotheses {
		if !h.RootCause {
			continue
		}
		if ok, _ := b.RootCauseEligible(h); ok {
			out = append(out, h)
		}
	}
	return out
}

func mdIDs(ids []string) string {
	if len(ids) == 0 {
		return "—"
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "`" + id + "`"
	}
	return strings.Join(quoted, ", ")
}

func mdEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}
