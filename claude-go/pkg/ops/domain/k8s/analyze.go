package k8s

import (
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// analyze 在集群证据上做确定性推断。规则与其它域一致：触发来自客观状态，
// supported 必须有 E3+ 证据，根因最终由 Bundle.RootCauseEligible 复核。
func analyze(b *evidence.Bundle, s *snapshot) {
	events := b.EvidenceByID("ev_k8s_events")
	podFails := b.EvidenceByID("ev_k8s_pod_failures")
	sched := b.EvidenceByID("ev_k8s_scheduling")
	restarts := b.EvidenceByID("ev_k8s_restarts")

	issues := s.podIssues()
	active := activeIssues(issues)
	historical := historicalIssues(issues)
	window := s.window
	if window <= 0 {
		window = 30 * time.Minute
	}
	recentEvents := s.recentEvents(window)
	pendingPods := s.pendingPods()
	pendingPVCs := s.pendingPVCs()
	unavailable := s.unavailableWorkloads()

	crash := crashIssues(active)
	pull := pullIssues(active)

	// ---- K1 容器崩溃/重启 ---------------------------------------------------
	if len(crash) > 0 {
		support := []string{"ev_k8s_pod_failures"}
		if restarts != nil && s.totalRestarts() > 0 {
			support = append(support, "ev_k8s_restarts")
		}
		h := evidence.Hypothesis{
			ID:        "K1",
			Statement: "容器反复退出/正在崩溃：" + issueSummary(crash, 5),
			Mechanism: "容器进程异常退出后被 kubelet 重启，waiting.reason 进入 CrashLoopBackOff/Error；若上次退出是 OOMKilled，还叠加内存限制",
			Predicts: []string{
				"Pod containerStatuses 显示 restarts>0，state.waiting 为 CrashLoopBackOff/Error",
				"lastState.terminated 有非零 exitCode 或 OOMKilled",
			},
			Disconfirms: []string{
				"容器全部 ready 且 restarts 为 0",
				"失败来自调度/镜像拉取而不是进程退出",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan: []string{
				"kubectl -n <ns> logs <pod> --previous 查看上次退出栈",
				"kubectl -n <ns> describe pod <pod> 查看 Last State/Exit Code",
			},
		}
		h.RootCause = h.Status == evidence.HypothesisSupported
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- K2 镜像拉取 -------------------------------------------------------
	if len(pull) > 0 {
		support := []string{"ev_k8s_pod_failures"}
		if events != nil && len(recentEvents) > 0 {
			support = append(support, "ev_k8s_events")
		}
		h := evidence.Hypothesis{
			ID:        "K2",
			Statement: "镜像不可用导致容器无法启动：" + issueSummary(pull, 5),
			Mechanism: "kubelet 拉取镜像失败或镜像名错误，容器停在 ImagePullBackOff/ErrImagePull",
			Predicts: []string{
				"waiting.reason 为 ImagePullBackOff/ErrImagePull/InvalidImageName",
				"Warning 事件包含 Failed to pull image / not found / unauthorized",
			},
			Disconfirms: []string{
				"镜像已成功拉取且容器进入 Running",
				"事件显示失败发生在启动参数/挂载而不是拉取",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan: []string{
				"kubectl -n <ns> describe pod <pod> 查看 Failed 事件原文",
				"在目标节点上用相同 imagePullSecret 手动拉取镜像",
			},
		}
		h.RootCause = h.Status == evidence.HypothesisSupported
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- K3 调度失败 -------------------------------------------------------
	if len(pendingPods) > 0 {
		withEvent := 0
		for _, p := range pendingPods {
			if s.eventFor("Pod", p.Name, p.Namespace, "FailedScheduling") != nil {
				withEvent++
			}
		}
		if withEvent > 0 || s.countReason(pendingPods, "Unschedulable") > 0 {
			support := []string{"ev_k8s_scheduling"}
			h := evidence.Hypothesis{
				ID:        "K3",
				Statement: fmt.Sprintf("Pod 无法调度到节点（Pending %d 个，其中 FailedScheduling %d 个）", len(pendingPods), withEvent),
				Mechanism: "资源请求超过节点可分配量、亲和性/污点不满足或 PVC 未绑定，调度器持续拒绝绑定",
				Predicts:  []string{"Pod phase=Pending 且 reason=Unschedulable", "Warning 事件 reason=FailedScheduling 并给出 Insufficient/taint 原因"},
				Disconfirms: []string{
					"所有 Pod 已绑定节点并 Running",
					"Pending 原因是镜像拉取而不是调度",
				},
				Support:    support,
				Status:     hypStatus(b, support),
				Confidence: hypConfidence(b, support),
				TestPlan:   []string{"kubectl -n <ns> describe pod <pod> 查看 Events 的 FailedScheduling 原文", "检查节点 allocatable 与 requests、taints/tolerations"},
			}
			h.RootCause = h.Status == evidence.HypothesisSupported
			b.Hypotheses = append(b.Hypotheses, h)
		}
	}

	// ---- K4 节点压力/NotReady ----------------------------------------------
	badNodes := s.badNodes()
	if len(badNodes) > 0 {
		support := []string{"ev_k8s_nodes"}
		if events != nil && len(recentEvents) > 0 {
			support = append(support, "ev_k8s_events")
		}
		h := evidence.Hypothesis{
			ID:        "K4",
			Statement: "节点异常：" + strings.Join(badNodes, ","),
			Mechanism: "节点 NotReady 或处于 Memory/Disk/PID pressure，kubelet 可能驱逐 Pod 或停止接受新 Pod",
			Predicts:  []string{"Node condition Ready=False 或 pressure=True", "该节点上的 Pod 出现 Pending/Failed/NotReady，或出现 Node 相关 Warning 事件"},
			Disconfirms: []string{
				"所有节点 Ready 且 pressure=False",
				"节点异常但没有任何 Pod 受影响，可能是刚恢复的短时抖动",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan:   []string{"kubectl describe node <node> 查看 Conditions/Allocated resources", "在节点上读取 kubelet 日志与 cgroup 内存/磁盘水位"},
		}
		h.RootCause = h.Status == evidence.HypothesisSupported
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- K5 PVC 未绑定 -----------------------------------------------------
	if len(pendingPVCs) > 0 {
		support := []string{"ev_k8s_pvc"}
		statement := "PVC 未绑定：" + pvcNames(pendingPVCs, 5)
		if events != nil && len(recentEvents) > 0 {
			support = append(support, "ev_k8s_events")
		}
		h := evidence.Hypothesis{
			ID:        "K5",
			Statement: statement,
			Mechanism: "StorageClass 动态供应失败或 PV 不匹配，PVC 停在 Pending，使用它的 Pod 无法启动",
			Predicts:  []string{"PVC status.phase=Pending", "Warning 事件 ProvisioningFailed/FailedBinding，或同 namespace 的 Pod Pending"},
			Disconfirms: []string{
				"PVC 全部 Bound",
				"存储已绑定，Pod 失败来自应用启动",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan:   []string{"kubectl -n <ns> describe pvc <name> 查看 ProvisioningFailed 原因", "确认 StorageClass/PV 的 capacity 与 accessModes"},
		}
		h.RootCause = h.Status == evidence.HypothesisSupported
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- K6 工作负载副本不足 ------------------------------------------------
	if len(unavailable) > 0 {
		support := []string{"ev_k8s_workloads"}
		if podFails != nil && len(active) > 0 {
			support = append(support, "ev_k8s_pod_failures")
		}
		if sched != nil && len(pendingPods) > 0 {
			support = append(support, "ev_k8s_scheduling")
		}
		h := evidence.Hypothesis{
			ID:        "K6",
			Statement: "工作负载副本不足：" + workloadNames(unavailable, 5),
			Mechanism: "Deployment/StatefulSet/DaemonSet 的期望副本没有全部 Ready，服务容量下降",
			Predicts:  []string{"readyReplicas < spec.replicas 或 numberReady < desiredNumberScheduled", "存在对应的 Pod 失败/调度/PVC 证据"},
			Disconfirms: []string{
				"所有工作负载 readyReplicas == spec.replicas",
				"副本不足是主动缩容（desired=0）",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan:   []string{"kubectl -n <ns> get pods -l <selector> 找出未 Ready 的 Pod", "按 Pod 失败原因继续排查"},
		}
		h.RootCause = h.Status == evidence.HypothesisSupported
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- K7 窗口外历史重启 --------------------------------------------------
	if len(historical) > 0 && len(active) == 0 && s.totalRestarts() > 0 {
		b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
			ID:        "K7",
			Statement: "观察到历史累计容器重启，当前没有活跃失败（重启时间未与窗口对齐）",
			Mechanism: "restartCount/lastState 只说明过去发生过重启，不能证明与本次症状同一时间",
			Predicts:  []string{"restartCount>0 但当前容器 Running/Ready", "对应时间早于本次窗口"},
			Disconfirms: []string{
				"当前仍有容器处于 waiting/terminated 失败态",
				"重启时间落在本次窗口内",
			},
			Support:    []string{"ev_k8s_restarts"},
			Status:     evidence.HypothesisInconcl,
			Confidence: 0.3,
			TestPlan:   []string{"用 kubectl logs --previous 查看历史退出原因", "核对事件与重启时间是否落在本次故障窗口"},
		})
	}

	buildContradictions(b, s, active, historical, pendingPods, pendingPVCs, unavailable, recentEvents)
	buildMissing(b, s, active, historical, pendingPods)
	b.Summary = summarize(b)
}

func buildContradictions(b *evidence.Bundle, s *snapshot, active, historical []podIssue,
	pendingPods []podInfo, pendingPVCs []pvcInfo, unavailable []workloadInfo, recentEvents []eventInfo) {
	if s.totalRestarts() > 0 && len(active) == 0 {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "KC1", Description: fmt.Sprintf("累计重启 %d 次，但当前没有活跃容器失败", s.totalRestarts()),
			EvidenceA: "ev_k8s_restarts", EvidenceB: "ev_k8s_pod_failures", Severity: "medium",
			Resolution: "restartCount 只有累计值、没有逐次时间戳；当前无活跃失败，历史重启不能解释当前症状，但也不能仅凭计数断言已经恢复",
		})
	}
	if len(recentEvents) > 0 && len(active) == 0 && len(unavailable) == 0 {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "KC2", Description: fmt.Sprintf("有 %d 条 Warning 事件，但 Pod/工作负载当前均正常", len(recentEvents)),
			EvidenceA: "ev_k8s_events", EvidenceB: "ev_k8s_workloads", Severity: "medium",
			Resolution: "告警对应的事件已过去；事件 TTL 内仍可见，但不代表当前故障",
		})
	}
	if len(s.badNodes()) > 0 && !s.anyNodeFused() {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "KC3", Description: "节点存在 NotReady/pressure，但没有受影响 Pod 或 Node 相关 Warning 事件",
			EvidenceA: "ev_k8s_nodes", EvidenceB: "ev_k8s_pods", Severity: "medium",
		})
	}
	if len(active) > 0 && s.readyNodes() == len(s.nodes) && len(s.nodes) > 0 && !s.anyNodeFused() {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "KC4", Description: "Pod 正在失败，但所有节点 Ready 且无节点压力",
			EvidenceA: "ev_k8s_pod_failures", EvidenceB: "ev_k8s_nodes", Severity: "medium",
			Resolution: "更像应用自身/镜像/配置问题，不是节点失联",
		})
	}
	if len(pendingPVCs) > 0 && len(pendingPods) == 0 && len(unavailable) == 0 {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "KC5", Description: "PVC 处于 Pending，但没有 Pending Pod 或副本不足的工作负载在使用它",
			EvidenceA: "ev_k8s_pvc", EvidenceB: "ev_k8s_pods", Severity: "medium",
			Resolution: "该 PVC 当前没有被工作负载阻塞；可能是预留/已废弃存储",
		})
	}
	if len(unavailable) > 0 && len(active) == 0 && len(pendingPods) == 0 {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "KC6", Description: "工作负载副本不足，但没有失败容器或 Pending Pod 解释原因",
			EvidenceA: "ev_k8s_workloads", EvidenceB: "ev_k8s_pod_failures", Severity: "medium",
		})
	}
}

func buildMissing(b *evidence.Bundle, s *snapshot, active, historical []podIssue, pendingPods []podInfo) {
	b.Missing = append(b.Missing, evidence.Missing{
		What:       "节点/Pod 的 CPU、内存、磁盘使用率曲线",
		Why:        "集群没有 metrics-server/Prometheus 时，只能看到状态与事件，不能量化资源饱和",
		NextAction: "需人工：部署 metrics-server，或用 kubelet /stats/summary 采集窗口曲线；本工具不读取资源使用率",
	})
	b.Missing = append(b.Missing, evidence.Missing{
		What:       "Pod 日志（当前与 --previous）",
		Why:        "容器退出原因、OOM 前的分配失败、应用层报错只在日志里",
		NextAction: "需人工：对失败 Pod 执行 kubectl logs <pod> [-c <container>] [--previous]；本工具不执行 logs",
	})
	if len(pendingPods) > 0 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "调度器视角的资源与约束详情",
			Why:        "本工具只读 Pod/Event，未展开 Node allocatable、taints、affinity 与 PriorityClass",
			NextAction: "kubectl describe pod/node，或读取 scheduler 日志中的 FailedScheduling 过滤原因",
		})
	}
	if s.namespace != "" {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "其它 namespace 的状态",
			Why:        "本次只采集了 namespace=" + s.namespace + "，跨 namespace 的依赖故障不可见",
			NextAction: "工具支持：去掉 namespace 参数做集群级采集，或分别采集相关 namespace",
		})
	}
	if !s.nodesValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "节点状态",
			Why:        "kubectl get nodes 失败时不能把 0 个节点当成集群健康",
			NextAction: "确认 kubeconfig context 有效、RBAC 允许 list nodes",
		})
	}
	if !s.podsValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "Pod 状态",
			Why:        "采集失败时 CrashLoop/镜像拉取等问题完全不可见",
			NextAction: "确认 RBAC 允许 list pods，或缩小 namespace",
		})
	}
	if !s.eventsValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "Warning 事件",
			Why:        "失败原因经常只在事件里（FailedScheduling/ProvisioningFailed/FailedMount）",
			NextAction: "确认 RBAC 允许 list events；注意事件 TTL 通常只有 1 小时",
		})
	}
	if !s.pvcsValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "PVC 状态",
			Why:        "存储未绑定会让 Pod 永久 Pending，没有 PVC 证据无法排除",
			NextAction: "确认 RBAC 允许 list persistentvolumeclaims",
		})
	}
	if !s.workloadsValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "工作负载副本状态",
			Why:        "没有 Deployment/StatefulSet/DaemonSet 状态就无法判断容量是否下降",
			NextAction: "确认 RBAC 允许 list deployments/statefulsets/daemonsets",
		})
	}
	if len(active) > 0 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "失败容器的退出码与上次日志",
			Why:        "当前证据只有 waiting.reason；是应用 panic、OOM 还是启动配置错误需要日志确认",
			NextAction: "需人工：kubectl logs --previous 与 kubectl describe pod 的 Last State；当前工具只执行固定 get",
		})
	}
	if len(historical) > 0 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "历史重启的完整时间线",
			Why:        "restartCount 没有逐次时间戳，无法把重启与故障窗口对齐",
			NextAction: "从监控/事件历史或容器运行时导出每次退出时间",
		})
	}
	b.Missing = append(b.Missing, evidence.Missing{
		What:       "Service/Endpoints 与变更历史",
		Why:        "本工具不采集 Service/Endpoints，也没有 rollout/config 变更记录，无法判断流量层与变更相关性",
		NextAction: "需人工：补充 kubectl get endpoints、rollout history 与 CI/CD 变更记录；当前工具不采集这些对象",
	})
}

func summarize(b *evidence.Bundle) string {
	var roots []string
	for _, h := range b.Hypotheses {
		if h.RootCause {
			if ok, _ := b.RootCauseEligible(h); ok {
				roots = append(roots, h.ID+": "+h.Statement)
			}
		}
	}
	counts := map[evidence.Grade]int{}
	for _, e := range b.Evidences {
		counts[e.Grade]++
	}
	head := fmt.Sprintf("证据 %d 条（E1=%d E2=%d E3=%d E4=%d），假设 %d 条，矛盾 %d 条，缺口 %d 条",
		len(b.Evidences), counts[evidence.GradeE1], counts[evidence.GradeE2],
		counts[evidence.GradeE3], counts[evidence.GradeE4],
		len(b.Hypotheses), len(b.Contradictions), len(b.Missing))
	if len(roots) == 0 {
		return head + "。未发现满足 E3 门控的根因；不要基于单条 E2 证据下结论。"
	}
	return head + "。满足门控的根因候选：" + strings.Join(roots, "；")
}

func hypStatus(b *evidence.Bundle, support []string) evidence.HypothesisStatus {
	for _, id := range support {
		if e := b.EvidenceByID(id); e != nil && e.ProbeValid && e.Grade.AtLeast(evidence.GradeE3) {
			return evidence.HypothesisSupported
		}
	}
	if len(support) == 0 {
		return evidence.HypothesisProposed
	}
	return evidence.HypothesisInconcl
}

func hypConfidence(b *evidence.Bundle, support []string) float64 {
	best := evidence.Grade("")
	for _, id := range support {
		if e := b.EvidenceByID(id); e != nil && e.Grade.Rank() > best.Rank() {
			best = e.Grade
		}
	}
	switch best {
	case evidence.GradeE4:
		return 0.9
	case evidence.GradeE3:
		return 0.75
	case evidence.GradeE2:
		return 0.4
	default:
		return 0.2
	}
}

// ---- 小工具 ----------------------------------------------------------------

func crashIssues(in []podIssue) []podIssue {
	var out []podIssue
	for _, is := range in {
		if isPullIssue(is) {
			continue
		}
		out = append(out, is)
	}
	return out
}

func pullIssues(in []podIssue) []podIssue {
	var out []podIssue
	for _, is := range in {
		if isPullIssue(is) {
			out = append(out, is)
		}
	}
	return out
}

func isPullIssue(is podIssue) bool {
	switch is.Reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
		return true
	}
	return false
}

func (s *snapshot) badNodes() []string {
	var out []string
	for _, n := range s.nodes {
		if !n.Ready || len(n.Pressure) > 0 {
			out = append(out, n.Name)
		}
	}
	return out
}

func (s *snapshot) anyNodeFused() bool {
	for _, n := range s.nodes {
		if (!n.Ready || len(n.Pressure) > 0) && (s.nodeHasDegradedPod(n.Name) || s.nodeHasWarning(n.Name)) {
			return true
		}
	}
	return false
}

func (s *snapshot) countReason(pods []podInfo, reason string) int {
	n := 0
	for _, p := range pods {
		if strings.EqualFold(p.Reason, reason) {
			n++
		}
	}
	return n
}
