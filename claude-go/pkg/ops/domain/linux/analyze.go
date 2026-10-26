package linux

import (
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// analyze 在主机证据上做确定性推断。规则与 MySQL 域一致：
// 触发来自读数、supported 必须有 E3+、根因最终由 Bundle.RootCauseEligible 复核。
func analyze(b *evidence.Bundle, s *snapshot) {
	load := b.EvidenceByID("ev_host_load")
	mem := b.EvidenceByID("ev_host_memory")
	psi := b.EvidenceByID("ev_host_pressure")
	disk := b.EvidenceByID("ev_host_disk")
	inodes := b.EvidenceByID("ev_host_inodes")
	procs := b.EvidenceByID("ev_host_process_states")
	failed := b.EvidenceByID("ev_host_failed_units")
	klog := b.EvidenceByID("ev_host_kernel_log")

	last := s.latest()
	loadPerCPU := perCPU(last.load1, s.cpuCount)
	memAvail := ratioU(last.memAvailable, last.memTotal)
	swapUsed := ratioUsed(last.swapTotal, last.swapFree)
	worst, inodeWorst := worstFS(s.fs)
	oom, faults := classifyKernel(s.kernelEvents)

	// ---- L1 CPU 争抢 --------------------------------------------------------
	if load != nil && last.loadValid && (loadPerCPU >= loadPerCPUWarn || last.psiCPU >= psiCPUWarn) {
		support := []string{"ev_host_load"}
		if psi != nil && last.psiCPU >= psiCPUWarn {
			support = append(support, "ev_host_pressure")
		}
		if procs != nil && last.dState > 0 {
			support = append(support, "ev_host_process_states")
		}
		h := evidence.Hypothesis{
			ID:        "L1",
			Statement: "CPU 争抢/运行队列积压导致请求变慢",
			Mechanism: "可运行任务数超过 CPU 配额，调度延迟上升；PSI cpu some 会同步升高",
			Predicts: []string{
				"load1 / CPU 数 >= 1.5，且 PSI cpu some 升高",
				"D-state 或可运行进程增多，但单进程 CPU 占比未定位",
			},
			Disconfirms: []string{
				"load 高但 PSI cpu 接近 0（更像 IO 阻塞或不可中断睡眠）",
				"CPU 配额充足、PSI 低且没有排队",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan: []string{
				"用 pidstat -u 1 3 找到 CPU 占用最高的进程",
				"对比 cgroup CPU throttling（cpu.stat throttled_usec）",
			},
		}
		if h.Status == evidence.HypothesisSupported {
			h.RootCause = true
		}
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- L2 内存/swap 压力 --------------------------------------------------
	if mem != nil && last.memValid && (memAvail <= memAvailWarn || swapUsed >= swapUsedWarn || last.psiMem >= psiMemWarn || len(oom) > 0) {
		support := []string{"ev_host_memory"}
		if psi != nil && last.psiMem >= psiMemWarn {
			support = append(support, "ev_host_pressure")
		}
		if klog != nil && len(oom) > 0 {
			support = append(support, "ev_host_kernel_log")
		}
		h := evidence.Hypothesis{
			ID:        "L2",
			Statement: "内存不足/swap 换页导致进程变慢或被内核回收",
			Mechanism: "MemAvailable 逼近 0 或 swap 大量占用，分配与换页延迟上升；严重时触发 OOM killer",
			Predicts: []string{
				"MemAvailable / MemTotal <= 10% 或 swap 已用 >= 50%",
				"PSI memory some 升高，或内核日志出现 OOM kill",
			},
			Disconfirms: []string{
				"MemAvailable 充足且 swap 使用率低",
				"OOM 记录早于本次窗口，当前内存已恢复",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan: []string{
				"用 ps/pidstat 按 RSS 排序找内存增长进程",
				"读取 cgroup memory.current / memory.events 确认是否限流或 OOM",
			},
		}
		if h.Status == evidence.HypothesisSupported {
			h.RootCause = true
		}
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- L3 磁盘/inode 耗尽 --------------------------------------------------
	if disk != nil && s.fsValid && worst != nil && worst.UsedRatio >= diskUsedWarn {
		h := evidence.Hypothesis{
			ID:        "L3",
			Statement: fmt.Sprintf("文件系统 %s 空间耗尽，写入失败并可能拖慢同盘服务", worst.Mount),
			Mechanism: "可用块低于安全水位，写操作返回 ENOSPC；日志/临时文件/数据库写入同时受影响",
			Predicts:  []string{"df 显示目标挂载点已用 >= 90%", "应用日志出现 no space left on device"},
			Disconfirms: []string{
				"目标挂载点已用低于 80%",
				"告警来自 inode 而不是块空间",
			},
			Support:    []string{"ev_host_disk"},
			Status:     hypStatus(b, []string{"ev_host_disk"}),
			Confidence: hypConfidence(b, []string{"ev_host_disk"}),
			TestPlan: []string{
				"用 du -x -h --max-depth=2 定位增长目录",
				"确认是否有未轮转日志/临时文件/备份",
			},
		}
		if h.Status == evidence.HypothesisSupported {
			h.RootCause = true
		}
		b.Hypotheses = append(b.Hypotheses, h)
	}
	if inodes != nil && inodeWorst != nil && inodeWorst.InodeRatio >= inodeUsedWarn {
		h := evidence.Hypothesis{
			ID:        "L4",
			Statement: fmt.Sprintf("文件系统 %s inode 耗尽，无法创建新文件", inodeWorst.Mount),
			Mechanism: "小文件/临时文件大量堆积，块空间可能仍充足但 inode 用尽",
			Predicts:  []string{"df -i 显示 inode 使用率 >= 90%", "创建文件返回 No space left on device"},
			Disconfirms: []string{
				"inode 使用率低于 80%",
				"失败原因是块空间而不是 inode",
			},
			Support:    []string{"ev_host_inodes"},
			Status:     hypStatus(b, []string{"ev_host_inodes"}),
			Confidence: hypConfidence(b, []string{"ev_host_inodes"}),
			TestPlan:   []string{"用 find 按目录统计文件数", "清理过期临时文件/会话文件并复核 inode"},
		}
		if h.Status == evidence.HypothesisSupported {
			h.RootCause = true
		}
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- L5 IO 阻塞 ---------------------------------------------------------
	if psi != nil && last.psiValid && (last.psiIO >= psiIOWarn || (last.dState > 0 && loadPerCPU >= 1.0)) {
		support := []string{"ev_host_pressure"}
		if procs != nil {
			support = append(support, "ev_host_process_states")
		}
		b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
			ID:        "L5",
			Statement: "磁盘 IO 饱和/阻塞导致进程进入 D-state、请求延迟上升",
			Mechanism: "设备队列拥塞使读写等待变长，PSI io some 与 D-state 进程同时升高",
			Predicts:  []string{"PSI io some 升高", "进程处于 D-state，负载高但 CPU 不一定忙"},
			Disconfirms: []string{
				"PSI io 接近 0 且没有 D-state 进程",
				"慢请求来自网络/锁而不是本地磁盘",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan: []string{
				"用 iostat -x 1 3 看 r_await/w_await/util",
				"用 pidstat -d 1 3 找 IO 最高的进程",
			},
		})
	}

	// ---- L6 OOM kill --------------------------------------------------------
	if klog != nil && len(oom) > 0 {
		support := []string{"ev_host_kernel_log"}
		if mem != nil && (memAvail <= 0.2 || swapUsed >= 0.3) {
			support = append(support, "ev_host_memory")
		}
		h := evidence.Hypothesis{
			ID:        "L6",
			Statement: "进程在窗口内被内核 OOM killer 杀死（" + eventNames(oom) + "）",
			Mechanism: "内存分配失败触发 oom-kill，内核选择并终止评分最高的进程",
			Predicts:  []string{"内核日志出现 Killed process / oom-kill 并点名进程", "同一时间段内存或 cgroup 接近上限"},
			Disconfirms: []string{
				"内核日志没有 OOM 记录",
				"进程退出原因是自身 panic/信号而不是 OOM",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan: []string{
				"读取 cgroup memory.events 的 oom_kill 计数",
				"核对被杀进程的 RSS 与当时的 memory.max",
			},
		}
		if h.Status == evidence.HypothesisSupported {
			h.RootCause = true
		}
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- L7 内核故障/进程崩溃 ------------------------------------------------
	if klog != nil && len(faults) > 0 {
		support := []string{"ev_host_kernel_log"}
		h := evidence.Hypothesis{
			ID:        "L7",
			Statement: "内核记录到进程崩溃、IO 错误或任务卡死（" + eventNames(faults) + "）",
			Mechanism: "段错误/硬件 IO 错误/任务 hung 会直接表现为进程退出或长时间不可中断",
			Predicts:  []string{"journalctl -k 出现 segfault / I/O error / hung_task", "同一时间进程重启或服务不可用"},
			Disconfirms: []string{
				"内核日志没有对应记录",
				"进程退出有明确的应用层错误",
			},
			Support:    support,
			Status:     hypStatus(b, support),
			Confidence: hypConfidence(b, support),
			TestPlan:   []string{"按进程名关联服务重启时间", "读取设备 SMART 与文件系统错误计数"},
		}
		if h.Status == evidence.HypothesisSupported {
			h.RootCause = true
		}
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- L8 failed units ----------------------------------------------------
	if failed != nil && s.failedValid && len(s.failedUnits) > 0 {
		b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
			ID:        "L8",
			Statement: "systemd 单元处于 failed 状态，相关服务未运行",
			Mechanism: "单元启动失败或运行中退出，依赖它的服务可能级联不可用",
			Predicts:  []string{"systemctl --failed 列出该单元", "journalctl -u <unit> 有退出原因"},
			Disconfirms: []string{
				"systemctl --failed 为空",
				"单元是主动停止而不是失败",
			},
			Support:    []string{"ev_host_failed_units"},
			Status:     evidence.HypothesisInconcl,
			Confidence: 0.4,
			TestPlan:   []string{"需人工：对每个 failed unit 读取 journalctl -u <unit> -n 50；本工具不采集 unit 日志"},
		})
	}

	buildContradictions(b, s, load, mem, psi, klog, loadPerCPU, memAvail, swapUsed)
	buildMissing(b, s, mem, klog, loadPerCPU, memAvail, swapUsed)
	b.Summary = summarize(b)
}

func buildContradictions(b *evidence.Bundle, s *snapshot, load, mem, psi, klog *evidence.Evidence,
	loadPerCPU, memAvail, swapUsed float64) {
	last := s.latest()
	if load != nil && psi != nil && loadPerCPU >= loadPerCPUWarn && last.psiValid && last.psiCPU < 5 {
		res := ""
		if last.psiIO >= psiIOWarn || last.dState > 0 {
			res = "已由 D-state/PSI io 解释：高负载更像 IO 阻塞，不是 CPU 排队"
		}
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "LC1", Description: fmt.Sprintf("load/CPU=%.2f 很高，但 PSI cpu some 仅 %.2f%%", loadPerCPU, last.psiCPU),
			EvidenceA: "ev_host_load", EvidenceB: "ev_host_pressure", Severity: "medium", Resolution: res,
		})
	}
	if psi != nil && load != nil && last.psiValid && last.psiCPU >= psiCPUWarn && loadPerCPU < 1.0 {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "LC2", Description: fmt.Sprintf("PSI cpu some %.1f%% 高，但 load/CPU 仅 %.2f，可能是短时突发或 cgroup 限流", last.psiCPU, loadPerCPU),
			EvidenceA: "ev_host_pressure", EvidenceB: "ev_host_load", Severity: "medium",
		})
	}
	if mem != nil && mem.ProbeValid && klog != nil && len(collectOOM(s)) > 0 && memAvail > 0.2 && swapUsed < 0.3 {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "LC3", Description: "内核日志有 OOM kill，但当前 MemAvailable/swap 已恢复",
			EvidenceA: "ev_host_kernel_log", EvidenceB: "ev_host_memory", Severity: "medium",
			Resolution: "OOM 是窗口内的过去事件，只能解释当时的进程退出，不能解释当前内存压力",
		})
	}
}

func buildMissing(b *evidence.Bundle, s *snapshot, mem, klog *evidence.Evidence,
	loadPerCPU, memAvail, swapUsed float64) {
	if span := s.sampleSpanSeconds(); span >= 0 && span < 60 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       fmt.Sprintf("覆盖完整窗口的主机时间序列（当前采样仅 %.0fs）", span),
			Why:        "数秒内的瞬时值不能区分“一直高”与“刚刚升高”，也不能算增长率",
			NextAction: "需人工：用 sar/监控补齐窗口曲线；本工具只有短时顺序采样，window_minutes 不会延长采样",
		})
	}
	last := s.latest()
	if !last.psiValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "PSI（/proc/pressure）读数",
			Why:        "缺 PSI 时无法区分 CPU 排队、IO 阻塞与内存直接回收",
			NextAction: "确认内核 CONFIG_PSI 与 /proc/pressure 可读；容器内需挂载该路径",
		})
	}
	if !s.kernelValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "内核日志（journalctl -k）",
			Why:        "探针无权限时不能把空结果当成“没有 OOM/崩溃”",
			NextAction: "确认用户属于 adm/systemd-journal 组，或由宿主侧导出 kernel ring buffer",
		})
	}
	if !s.failedValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "systemd failed units 清单",
			Why:        "没有这份清单就不能排除服务级故障",
			NextAction: "确认 systemctl 可用（容器内通常没有 systemd，应在宿主采集）",
		})
	}
	if loadPerCPU >= loadPerCPUWarn || memAvail <= memAvailWarn || swapUsed >= swapUsedWarn {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "进程级 CPU/RSS/IO 归因",
			Why:        "当前只有状态计数，能证明有压力但不能定位是哪个进程",
			NextAction: "采集 pidstat -u -r -d 1 3，或读取 cgroup 的 cpu.stat/memory.current",
		})
	}
	if !last.procValid {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "进程状态（ps）",
			Why:        "ps 失败时 D-state=0 不是事实",
			NextAction: "确认 /proc 可读或改用 /proc/*/stat 直接采集",
		})
	}
	if len(collectOOM(s)) > 0 && mem != nil && memAvail > 0.2 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "OOM 事件的 cgroup/容器归属",
			Why:        "内核日志只点名进程，宿主上同名进程可能有多个实例",
			NextAction: "读取 cgroup memory.events 与对应容器 ID，确认被杀实例",
		})
	}
	if klog != nil {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "应用/服务日志（非内核）",
			Why:        "内核告警只能解释崩溃/资源错误，不能解释应用层报错与超时",
			NextAction: "需人工：按服务读取 journalctl -u <unit> 或容器日志；OpsInvestigate 当前不支持 unit 日志",
		})
	}
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

func collectOOM(s *snapshot) []kernelEvent {
	oom, _ := classifyKernel(s.kernelEvents)
	return oom
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
