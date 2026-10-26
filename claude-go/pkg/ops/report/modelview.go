package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// RenderModelView 是给 LLM 的紧凑推理视图，不是给人看的完整报告。
// 保留所有证据 ID/等级/口径、假设、矛盾和缺口；原始预览与逐点时间线只留在
// incident artifact，避免模型把同一份报告再复述一遍。
func RenderModelView(b *evidence.Bundle) string {
	if b == nil {
		return "evidence_bundle: null\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "summary: %s\n", oneLine(b.Summary))
	roots := rootCauses(b)
	if len(roots) == 0 {
		sb.WriteString("root_cause_gate: not_passed (supported + valid E3+ + no unresolved high contradiction required)\n")
	} else {
		ids := make([]string, 0, len(roots))
		for _, h := range roots {
			ids = append(ids, h.ID)
		}
		fmt.Fprintf(&sb, "root_cause_gate: passed (%s)\n", strings.Join(ids, ","))
	}
	renderCollection(&sb, b, true)

	sb.WriteString("evidence:\n")
	if len(b.Evidences) == 0 {
		sb.WriteString("- none\n")
	}
	evs := append([]evidence.Evidence{}, b.Evidences...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].ID < evs[j].ID })
	for _, e := range evs {
		fmt.Fprintf(&sb, "- id=%s grade=%s probe_valid=%t subject=%s summary=%s",
			e.ID, e.Grade, e.ProbeValid, oneLine(subjectName(e.Subject)), oneLine(e.Summary))
		if e.Window != nil {
			fmt.Fprintf(&sb, " evidence_span_seconds=%.1f", e.Window.End.Sub(e.Window.Start).Seconds())
		}
		if e.ProbeNote != "" {
			fmt.Fprintf(&sb, " caliber=%s", oneLine(e.ProbeNote))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("hypotheses:\n")
	if len(b.Hypotheses) == 0 {
		sb.WriteString("- none\n")
	}
	for _, h := range b.Hypotheses {
		eligible, _ := b.RootCauseEligible(h)
		fmt.Fprintf(&sb, "- id=%s status=%s root_cause_eligible=%t statement=%s support=%s contradict=%s predicts=%s disconfirms=%s test=%s\n",
			h.ID, h.Status, h.RootCause && eligible, oneLine(h.Statement), idList(h.Support), idList(h.Contradict),
			oneLine(strings.Join(h.Predicts, "；")), oneLine(strings.Join(h.Disconfirms, "；")),
			oneLine(strings.Join(h.TestPlan, "；")))
	}

	sb.WriteString("contradictions:\n")
	if len(b.Contradictions) == 0 {
		sb.WriteString("- none\n")
	}
	for _, c := range b.Contradictions {
		resolution := c.Resolution
		if resolution == "" {
			resolution = "unresolved"
		}
		fmt.Fprintf(&sb, "- id=%s severity=%s description=%s evidence=%s resolution=%s\n",
			c.ID, c.Severity, oneLine(c.Description), idList([]string{c.EvidenceA, c.EvidenceB}), oneLine(resolution))
	}

	sb.WriteString("missing:\n")
	if len(b.Missing) == 0 {
		sb.WriteString("- none\n")
	}
	for _, m := range b.Missing {
		fmt.Fprintf(&sb, "- what=%s why=%s next=%s\n", oneLine(m.What), oneLine(m.Why), oneLine(m.NextAction))
	}
	return sb.String()
}

func renderCollection(sb *strings.Builder, b *evidence.Bundle, compact bool) {
	if b == nil || (b.Collection.Sampling == nil && len(b.Collection.Capabilities) == 0) {
		return
	}
	if compact {
		sb.WriteString("collection:\n")
	} else {
		sb.WriteString("## 采集口径与能力\n\n")
	}
	if s := b.Collection.Sampling; s != nil {
		if compact {
			fmt.Fprintf(sb, "- sampling: samples=%d interval=%.1fs span=%.1fs lookback=%.1fs note=%s\n",
				s.Samples, s.IntervalSeconds, s.SpanSeconds, s.WindowSeconds, oneLine(s.Note))
		} else {
			fmt.Fprintf(sb, "- 状态采样：%d 个顺序样本，配置间隔 %.1fs，首末跨度 %.1fs；日志/事件回看窗 %.1fs。%s\n",
				s.Samples, s.IntervalSeconds, s.SpanSeconds, s.WindowSeconds, s.Note)
		}
	}
	for _, c := range b.Collection.Capabilities {
		if compact {
			fmt.Fprintf(sb, "- capability=%s availability=%s detail=%s\n", c.Name, c.Availability, oneLine(c.Detail))
		} else {
			fmt.Fprintf(sb, "- **[%s] %s**：%s\n", capabilityLabel(c.Availability), c.Name, c.Detail)
		}
	}
	if !compact {
		sb.WriteString("\n")
	}
}

func capabilityLabel(availability string) string {
	switch availability {
	case "tool":
		return "工具支持"
	case "manual":
		return "需人工"
	default:
		return "采集面不支持"
	}
}

func subjectName(s evidence.Subject) string {
	name := s.Kind + "/" + s.Name
	if s.Namespace != "" {
		name = s.Kind + "/" + s.Namespace + "/" + s.Name
	}
	return name
}

func idList(ids []string) string {
	var kept []string
	for _, id := range ids {
		if id != "" {
			kept = append(kept, id)
		}
	}
	if len(kept) == 0 {
		return "-"
	}
	return strings.Join(kept, ",")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
