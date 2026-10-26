package k8s

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// buildBundle 把一次只读快照归一成证据。ID 在重复采集间稳定，便于对比。
func (c *Collector) buildBundle(s *snapshot) *evidence.Bundle {
	now := c.now()
	name := s.context
	if s.namespace != "" {
		name += "/" + s.namespace
	}
	b := &evidence.Bundle{
		IncidentID:  fmt.Sprintf("inc_k8s_%s_%d", sanitizeID(s.context), now.Unix()),
		Instance:    name,
		Domain:      "k8s",
		GeneratedAt: now,
		Window:      evidence.TimeWindow{Start: now.Add(-c.cfg.Window), End: now},
		Collection: evidence.CollectionInfo{Capabilities: []evidence.Capability{
			{Name: "fixed_kubectl_get", Availability: "tool", Detail: "固定只读 get nodes/pods/events/PVC/workloads；window_minutes 用于事件与故障时间对齐，restartCount 仍是累计值。"},
			{Name: "namespace_rescope", Availability: "tool", Detail: "可用同一工具调整 namespace 后重采；空 namespace 表示全集群。"},
			{Name: "pod_logs", Availability: "manual", Detail: "当前不执行 kubectl logs/describe；当前与 --previous 日志需操作员在工具外采集。"},
			{Name: "metrics_and_traffic", Availability: "manual", Detail: "资源曲线、Service/Endpoints、rollout/变更历史不在当前采集面，需监控或额外只读数据。"},
		}},
	}
	subj := evidence.Subject{Kind: "cluster", Name: s.context, Namespace: s.namespace, Instance: s.context}

	issues := s.podIssues()
	active := activeIssues(issues)
	historical := historicalIssues(issues)
	recentEvents := s.recentEvents(c.cfg.Window)
	pendingPods := s.pendingPods()
	pendingPVCs := s.pendingPVCs()
	unavailable := s.unavailableWorkloads()

	b.Timeline = append(b.Timeline, evidence.TimelinePoint{
		At: now,
		Metrics: map[string]float64{
			"nodes_ready":           float64(s.readyNodes()),
			"nodes_total":           float64(len(s.nodes)),
			"pods_running":          float64(s.countPods("Running")),
			"pods_pending":          float64(len(pendingPods)),
			"pod_failures_active":   float64(len(active)),
			"pod_restarts_observed": float64(s.totalRestarts()),
			"warnings_recent":       float64(len(recentEvents)),
			"pvc_pending":           float64(len(pendingPVCs)),
			"workloads_unavailable": float64(len(unavailable)),
		},
		Labels: map[string]string{"context": s.context, "namespace": emptyAll(s.namespace)},
	})

	// ---- 上下文 (E1) --------------------------------------------------------
	versions := s.nodeVersions()
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_context", IncidentID: b.IncidentID, Type: evidence.TypeConfig,
		Source: "kubectl config get-contexts + get nodes -o json", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "node_count", Value: len(s.nodes)},
		Grade:  evidence.GradeE1, Reliability: 0.98, ProbeValid: true,
		Summary: fmt.Sprintf("context=%s namespace=%s nodes=%d kubelet=%s",
			s.context, emptyAll(s.namespace), len(s.nodes), strings.Join(versions, ",")),
		RawPreview: "context=" + s.context,
	})

	// ---- 节点 ---------------------------------------------------------------
	nodesGrade := evidence.GradeE2
	nodesSummary := "没有可解析的节点"
	nodesNote := s.nodesNote
	if s.nodesValid {
		ready := s.readyNodes()
		var pressure []string
		fused := false
		for _, n := range s.nodes {
			if len(n.Pressure) > 0 {
				pressure = append(pressure, n.Name+"("+strings.Join(n.Pressure, ",")+")")
			}
			if (!n.Ready || len(n.Pressure) > 0) && (s.nodeHasDegradedPod(n.Name) || s.nodeHasWarning(n.Name)) {
				fused = true
			}
		}
		if fused {
			nodesGrade = evidence.GradeE3
		}
		nodesSummary = fmt.Sprintf("%d/%d 节点 Ready", ready, len(s.nodes))
		if len(pressure) > 0 {
			nodesSummary += "；压力条件: " + strings.Join(pressure, ";")
		}
		if ready < len(s.nodes) {
			nodesSummary += "；NotReady: " + strings.Join(s.notReadyNodes(), ",")
		}
		if len(s.nodes) == 0 {
			nodesSummary = "集群没有节点"
		}
	} else {
		nodesSummary = "节点状态不可用（不是“全部正常”）"
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_nodes", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "kubectl get nodes -o json", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "nodes_ready", Value: s.readyNodes(), Threshold: "所有节点 Ready 且无 pressure"},
		Grade:  nodesGrade, Reliability: 0.95, ProbeValid: s.nodesValid, ProbeNote: nodesNote,
		Summary: nodesSummary, RawPreview: nodePreview(s.nodes),
	})

	// ---- Pod 概况 -----------------------------------------------------------
	podsGrade := evidence.GradeE2
	podsSummary := "Pod 状态不可用"
	if s.podsValid {
		podsSummary = fmt.Sprintf("Pod %d 个：Running=%d Pending=%d Failed=%d Succeeded=%d",
			len(s.pods), s.countPods("Running"), s.countPods("Pending"), s.countPods("Failed"), s.countPods("Succeeded"))
		if len(active) > 0 {
			podsGrade = evidence.GradeE3
		}
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_pods", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "kubectl get pods -A -o json", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "pods_total", Value: len(s.pods)},
		Grade:  podsGrade, Reliability: 0.95, ProbeValid: s.podsValid, ProbeNote: s.podsNote,
		Summary: podsSummary, RawPreview: podPreview(s.pods),
	})

	// ---- 当前 Pod 失败 ------------------------------------------------------
	failGrade := evidence.GradeE2
	failSummary := "没有当前 Pod 失败"
	failNote := ""
	failPreview := ""
	if len(active) > 0 {
		failSummary = fmt.Sprintf("当前失败容器 %d 个：%s", len(active), issueSummary(active, 5))
		failPreview = issuePreview(active)
		for _, is := range active {
			switch {
			case s.oomCurrent(is):
				failGrade = evidence.GradeE4
				failSummary += "；直接 OOMKilled: " + is.Namespace + "/" + is.Pod
			case is.Restarts > 0 || s.issueHasEvent(is):
				if failGrade.Rank() < evidence.GradeE3.Rank() {
					failGrade = evidence.GradeE3
				}
			}
			if failGrade == evidence.GradeE4 {
				break
			}
		}
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_pod_failures", IncidentID: b.IncidentID, Type: evidence.TypeEvent,
		Source: "kubectl get pods -A -o json（state.waiting/terminated）", CollectedAt: now, Subject: subj,
		Window: &evidence.TimeWindow{Start: now.Add(-c.cfg.Window), End: now},
		Signal: evidence.Signal{Name: "active_container_failures", Value: len(active)},
		Grade:  failGrade, Reliability: 0.92, ProbeValid: s.podsValid, ProbeNote: failNote,
		Summary: failSummary, RawPreview: failPreview,
	})

	// ---- 调度失败 -----------------------------------------------------------
	schedGrade := evidence.GradeE2
	schedSummary := "没有 Pending/FailedScheduling Pod"
	schedPreview := ""
	if len(pendingPods) > 0 {
		var withEvent []podInfo
		for _, p := range pendingPods {
			if s.eventFor("Pod", p.Name, p.Namespace, "FailedScheduling") != nil {
				withEvent = append(withEvent, p)
			}
		}
		if len(withEvent) > 0 {
			schedGrade = evidence.GradeE3
		}
		schedSummary = fmt.Sprintf("Pending Pod %d 个", len(pendingPods))
		if len(withEvent) > 0 {
			schedSummary += fmt.Sprintf("，其中 %d 个有 FailedScheduling 事件", len(withEvent))
		}
		var lines []string
		for i, p := range pendingPods {
			if i >= 8 {
				break
			}
			lines = append(lines, fmt.Sprintf("%s/%s reason=%s node=%s", p.Namespace, p.Name, emptyDash(p.Reason), emptyDash(p.Node)))
		}
		schedPreview = strings.Join(lines, "\n")
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_scheduling", IncidentID: b.IncidentID, Type: evidence.TypeEvent,
		Source: "kubectl get pods + get events -A --field-selector=type=Warning", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "pending_pods", Value: len(pendingPods)},
		Grade:  schedGrade, Reliability: 0.9, ProbeValid: s.podsValid, ProbeNote: s.podsNote,
		Summary: schedSummary, RawPreview: schedPreview,
	})

	// ---- Warning 事件 -------------------------------------------------------
	eventsGrade := evidence.GradeE2
	eventsSummary := "窗口内没有 Warning 事件"
	if len(recentEvents) > 0 {
		eventsSummary = fmt.Sprintf("窗口内 Warning 事件 %d 条（累计 %d 次）：%s",
			len(recentEvents), eventCount(recentEvents), topEventReasons(recentEvents, 5))
	}
	if !s.eventsValid {
		eventsSummary = "Warning 事件不可用（不是“没有告警”）"
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_events", IncidentID: b.IncidentID, Type: evidence.TypeEvent,
		Source: "kubectl get events -A --field-selector=type=Warning -o json", CollectedAt: now, Subject: subj,
		Window: &evidence.TimeWindow{Start: now.Add(-c.cfg.Window), End: now},
		Signal: evidence.Signal{Name: "warning_events", Value: len(recentEvents)},
		Grade:  eventsGrade, Reliability: 0.85, ProbeValid: s.eventsValid, ProbeNote: s.eventsNote,
		Summary: eventsSummary, RawPreview: eventPreview(recentEvents),
	})

	// ---- PVC ---------------------------------------------------------------
	pvcGrade := evidence.GradeE2
	pvcSummary := "没有 Pending PVC"
	pvcPreview := ""
	if len(pendingPVCs) > 0 {
		fused := false
		for _, p := range pendingPVCs {
			if s.eventFor("PersistentVolumeClaim", p.Name, p.Namespace, "ProvisioningFailed", "FailedBinding") != nil {
				fused = true
			}
		}
		if fused {
			pvcGrade = evidence.GradeE3
		}
		pvcSummary = fmt.Sprintf("Pending PVC %d 个：%s", len(pendingPVCs), pvcNames(pendingPVCs, 5))
		var lines []string
		for i, p := range pendingPVCs {
			if i >= 8 {
				break
			}
			lines = append(lines, fmt.Sprintf("%s/%s phase=%s storageClass=%s", p.Namespace, p.Name, p.Phase, emptyDash(p.StorageClass)))
		}
		pvcPreview = strings.Join(lines, "\n")
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_pvc", IncidentID: b.IncidentID, Type: evidence.TypeSignal,
		Source: "kubectl get pvc -A -o json", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "pending_pvcs", Value: len(pendingPVCs)},
		Grade:  pvcGrade, Reliability: 0.92, ProbeValid: s.pvcsValid, ProbeNote: s.pvcsNote,
		Summary: pvcSummary, RawPreview: pvcPreview,
	})

	// ---- 工作负载 -----------------------------------------------------------
	wlGrade := evidence.GradeE2
	wlSummary := "所有工作负载副本就绪"
	if !s.workloadsValid {
		wlSummary = "工作负载状态不可用（不是“全部就绪”）"
	} else if len(unavailable) > 0 {
		wlSummary = fmt.Sprintf("副本不足的工作负载 %d 个：%s", len(unavailable), workloadNames(unavailable, 5))
		for _, w := range unavailable {
			if s.workloadHasActivePodIssue(w) {
				wlGrade = evidence.GradeE3
				break
			}
		}
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_workloads", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "kubectl get deployments,statefulsets,daemonsets -A -o json", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "unavailable_workloads", Value: len(unavailable)},
		Grade:  wlGrade, Reliability: 0.92, ProbeValid: s.workloadsValid, ProbeNote: s.workloadsNote,
		Summary: wlSummary, RawPreview: workloadPreview(unavailable),
	})

	// ---- 重启计数（含窗口外历史） ------------------------------------------
	restartsGrade := evidence.GradeE2
	restartSummary := "没有观察到容器重启"
	if s.totalRestarts() > 0 {
		restartSummary = fmt.Sprintf("累计重启 %d 次；最高的 Pod：%s（last_finished 仅是最近已知终止时间，不是逐次重启时间线）", s.totalRestarts(), restartTop(s.pods, 5))
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_k8s_restarts", IncidentID: b.IncidentID, Type: evidence.TypeHistory,
		Source: "kubectl get pods -A -o json（status.restartCount/lastState）", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "observed_restarts", Value: s.totalRestarts()},
		Grade:  restartsGrade, Reliability: 0.9, ProbeValid: s.podsValid, ProbeNote: s.podsNote,
		Summary: restartSummary, RawPreview: issuePreview(historical),
	})

	sort.SliceStable(b.Evidences, func(i, j int) bool { return b.Evidences[i].ID < b.Evidences[j].ID })
	return b
}

// snapshot 派生集合 -----------------------------------------------------------

func (s *snapshot) readyNodes() int {
	n := 0
	for _, node := range s.nodes {
		if node.Ready {
			n++
		}
	}
	return n
}

func (s *snapshot) notReadyNodes() []string {
	var out []string
	for _, n := range s.nodes {
		if !n.Ready {
			out = append(out, n.Name)
		}
	}
	return out
}

func (s *snapshot) nodeVersions() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range s.nodes {
		if n.Kubelet != "" && !seen[n.Kubelet] {
			seen[n.Kubelet] = true
			out = append(out, n.Kubelet)
		}
	}
	sort.Strings(out)
	return out
}

func (s *snapshot) countPods(phase string) int {
	n := 0
	for _, p := range s.pods {
		if p.Phase == phase {
			n++
		}
	}
	return n
}

func (s *snapshot) totalRestarts() int {
	n := 0
	for _, p := range s.pods {
		for _, c := range p.Containers {
			n += c.RestartCount
		}
		for _, c := range p.InitContainers {
			n += c.RestartCount
		}
	}
	return n
}

func (s *snapshot) pendingPods() []podInfo {
	var out []podInfo
	for _, p := range s.pods {
		if p.Phase == "Pending" || strings.EqualFold(p.Reason, "Unschedulable") {
			out = append(out, p)
		}
	}
	return out
}

func (s *snapshot) pendingPVCs() []pvcInfo {
	var out []pvcInfo
	for _, p := range s.pvcs {
		if p.Phase == "Pending" || p.Phase == "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *snapshot) unavailableWorkloads() []workloadInfo {
	var out []workloadInfo
	for _, w := range s.workloads {
		if w.Desired > 0 && (w.Ready < w.Desired || w.Unavailable > 0) {
			out = append(out, w)
		}
	}
	return out
}

func (s *snapshot) nodeHasDegradedPod(node string) bool {
	for _, p := range s.pods {
		if p.Node != node {
			continue
		}
		if p.Phase == "Pending" || p.Phase == "Failed" || p.Phase == "Unknown" {
			return true
		}
		for _, c := range p.Containers {
			if !c.Ready {
				return true
			}
		}
	}
	return false
}

func (s *snapshot) nodeHasWarning(node string) bool {
	for _, ev := range s.events {
		if strings.EqualFold(ev.Kind, "Node") && ev.Name == node {
			return true
		}
	}
	return false
}

func (s *snapshot) workloadHasActivePodIssue(w workloadInfo) bool {
	for _, is := range activeIssues(s.podIssues()) {
		if is.Namespace != w.Namespace {
			continue
		}
		if strings.HasPrefix(is.Pod, w.Name+"-") {
			return true
		}
	}
	// PVC/调度造成的不可用也要和工作负载的 Pod 对应上。
	for _, p := range s.pendingPods() {
		if p.Namespace == w.Namespace && strings.HasPrefix(p.Name, w.Name+"-") {
			return true
		}
	}
	return false
}

func (s *snapshot) podByName(ns, name string) *podInfo {
	for i := range s.pods {
		if s.pods[i].Namespace == ns && s.pods[i].Name == name {
			return &s.pods[i]
		}
	}
	return nil
}

func activeIssues(in []podIssue) []podIssue {
	var out []podIssue
	for _, is := range in {
		if is.Active {
			out = append(out, is)
		}
	}
	return out
}

func historicalIssues(in []podIssue) []podIssue {
	var out []podIssue
	for _, is := range in {
		if !is.Active {
			out = append(out, is)
		}
	}
	return out
}

// oomCurrent 判断 OOM 是否是当前因果。state.terminated.reason=OOMKilled 是
// kubelet 的直接判定；lastState 里的 OOM 必须落在窗口内，否则只是历史重启。
func (s *snapshot) oomCurrent(is podIssue) bool {
	if strings.EqualFold(is.Reason, "OOMKilled") {
		return true
	}
	if !is.Active || !strings.EqualFold(is.LastTermination, "OOMKilled") {
		return false
	}
	if is.FinishedAt.IsZero() {
		return false
	}
	return s.now.Sub(is.FinishedAt) <= s.window
}

// issueHasEvent 判断失败容器是否有对应的 Warning 事件。容器状态是一条来源，
// 事件是第二条独立来源；两者同时出现才升到 E3。
func (s *snapshot) issueHasEvent(is podIssue) bool {
	return s.eventFor("Pod", is.Pod, is.Namespace,
		"Failed", "FailedToPullImage", "ErrImagePull", "ImagePullBackOff",
		"BackOff", "FailedCreate", "FailedMount", "FailedAttachVolume",
		"CreateContainerError", "CreateContainerConfigError") != nil
}

func issueSummary(issues []podIssue, n int) string {
	var parts []string
	for i, is := range issues {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s/%s[%s]=%s", is.Namespace, is.Pod, is.Container, emptyDash(is.Reason)))
	}
	return strings.Join(parts, ", ")
}

func issuePreview(issues []podIssue) string {
	var lines []string
	for i, is := range issues {
		if i >= 8 {
			break
		}
		line := fmt.Sprintf("%s/%s container=%s reason=%s restarts=%d active=%v",
			is.Namespace, is.Pod, is.Container, emptyDash(is.Reason), is.Restarts, is.Active)
		if !is.FinishedAt.IsZero() {
			line += " finished=" + is.FinishedAt.Format(time.RFC3339)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func nodePreview(nodes []nodeInfo) string {
	var lines []string
	for i, n := range nodes {
		if i >= 10 {
			break
		}
		conds := make([]string, 0, len(n.Pressure))
		conds = append(conds, n.Pressure...)
		lines = append(lines, fmt.Sprintf("%s ready=%v pressure=%s kubelet=%s",
			n.Name, n.Ready, emptyDash(strings.Join(conds, ",")), n.Kubelet))
	}
	return strings.Join(lines, "\n")
}

func podPreview(pods []podInfo) string {
	var lines []string
	shown := 0
	for _, p := range pods {
		if shown >= 12 {
			break
		}
		if p.Phase == "Running" && p.Reason == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s/%s phase=%s reason=%s node=%s",
			p.Namespace, p.Name, p.Phase, emptyDash(p.Reason), emptyDash(p.Node)))
		shown++
	}
	return strings.Join(lines, "\n")
}

func eventPreview(events []eventInfo) string {
	var lines []string
	for i, ev := range events {
		if i >= 8 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s/%s %s: %s",
			ev.Last.Format(time.RFC3339), ev.Namespace, ev.Name, ev.Reason, truncate(ev.Message, 160)))
	}
	return strings.Join(lines, "\n")
}

func eventCount(events []eventInfo) int {
	n := 0
	for _, ev := range events {
		n += max(ev.Count, 1)
	}
	return n
}

func pvcNames(pvcs []pvcInfo, n int) string {
	var parts []string
	for i, p := range pvcs {
		if i >= n {
			break
		}
		parts = append(parts, p.Namespace+"/"+p.Name)
	}
	return strings.Join(parts, ",")
}

func workloadNames(ws []workloadInfo, n int) string {
	var parts []string
	for i, w := range ws {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s/%s %d/%d", w.Namespace, w.Name, w.Ready, w.Desired))
	}
	return strings.Join(parts, ",")
}

func workloadPreview(ws []workloadInfo) string {
	var lines []string
	for i, w := range ws {
		if i >= 10 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s/%s ready=%d/%d unavailable=%d",
			w.Kind, w.Namespace, w.Name, w.Ready, w.Desired, w.Unavailable))
	}
	return strings.Join(lines, "\n")
}

func restartTop(pods []podInfo, n int) string {
	type row struct {
		name         string
		restarts     int
		lastFinished time.Time
	}
	var rows []row
	for _, p := range pods {
		total := 0
		var lastFinished time.Time
		for _, c := range p.Containers {
			total += c.RestartCount
			if c.LastState.Terminated != nil && c.LastState.Terminated.FinishedAt.After(lastFinished) {
				lastFinished = c.LastState.Terminated.FinishedAt
			}
		}
		if total > 0 {
			rows = append(rows, row{p.Namespace + "/" + p.Name, total, lastFinished})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].restarts != rows[j].restarts {
			return rows[i].restarts > rows[j].restarts
		}
		return rows[i].name < rows[j].name
	})
	var parts []string
	for i, r := range rows {
		if i >= n {
			break
		}
		part := fmt.Sprintf("%s=%d", r.name, r.restarts)
		if !r.lastFinished.IsZero() {
			part += "(last_finished=" + r.lastFinished.Format(time.RFC3339) + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func emptyAll(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "...(truncated)"
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "cluster"
	}
	return b.String()
}
