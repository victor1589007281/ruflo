package linux

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// buildBundle 把主机读数归一成证据。ID 在重复采集间稳定，便于对比。
func (c *Collector) buildBundle(s *snapshot) *evidence.Bundle {
	now := c.now()
	b := &evidence.Bundle{
		IncidentID:  fmt.Sprintf("inc_linux_%s_%d", sanitizeID(s.hostname), now.Unix()),
		Instance:    s.hostname,
		Domain:      "linux",
		GeneratedAt: now,
		Window: evidence.TimeWindow{
			Start: now.Add(-c.cfg.Window),
			End:   now,
		},
		Collection: evidence.CollectionInfo{Capabilities: []evidence.Capability{
			{Name: "fixed_host_snapshot", Availability: "tool", Detail: "固定 /proc、ps、df、systemctl --failed、journalctl -k 与 ss -s；不接受任意 argv。"},
			{Name: "unit_logs", Availability: "manual", Detail: "不支持 journalctl -u <unit>；需操作员在工具外按服务采集应用/unit 日志。"},
			{Name: "window_time_series", Availability: "manual", Detail: "完整窗口曲线需 sar/监控等外部数据；samples 只增加顺序采样点，不会覆盖整个 window_minutes。"},
		}},
	}
	if len(s.samples) > 0 {
		interval := c.cfg.SampleInterval
		if interval <= 0 {
			interval = defaultSampleInterval
		}
		span := s.sampleSpanSeconds()
		if len(s.samples) == 1 {
			span = 0
		}
		b.Collection.Sampling = &evidence.Sampling{
			Samples: len(s.samples), IntervalSeconds: interval.Seconds(), SpanSeconds: span,
			WindowSeconds: c.cfg.Window.Seconds(),
			Note:          "样本按顺序采集；span 是首末样本时间差。window_minutes 只控制内核日志回看，不会延长状态采样。",
		}
	}
	subj := evidence.Subject{Kind: "host", Name: s.hostname, Instance: s.hostname}

	for _, sm := range s.samples {
		metrics := map[string]float64{
			"d_state":             float64(sm.dState),
			"zombie":              float64(sm.zombie),
			"mem_available_ratio": ratioU(sm.memAvailable, sm.memTotal),
			"swap_used_ratio":     ratioUsed(sm.swapTotal, sm.swapFree),
			"load1_per_cpu":       perCPU(sm.load1, s.cpuCount),
			"psi_cpu_some_avg300": sm.psiCPU,
			"psi_io_some_avg300":  sm.psiIO,
			"psi_mem_some_avg300": sm.psiMem,
		}
		b.Timeline = append(b.Timeline, evidence.TimelinePoint{
			At: sm.at, Metrics: metrics,
			Labels: map[string]string{"source": "/proc"},
		})
	}

	last := s.latest()
	loadPerCPU := perCPU(last.load1, s.cpuCount)
	memAvail := ratioU(last.memAvailable, last.memTotal)
	swapUsed := ratioUsed(last.swapTotal, last.swapFree)
	psiFused := countAtLeast(s, psiCPUWarn, psiIOWarn, psiMemWarn) >= 2
	oomEvents, faultEvents := classifyKernel(s.kernelEvents)

	// ---- 身份/背景 (E1) ------------------------------------------------------
	uptime := time.Duration(s.uptimeSeconds * float64(time.Second))
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_identity", IncidentID: b.IncidentID, Type: evidence.TypeConfig,
		Source: "/proc + os-release", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "cpu_count", Value: s.cpuCount},
		Grade:  evidence.GradeE1, Reliability: 0.98, ProbeValid: true,
		Summary: fmt.Sprintf("host=%s kernel=%s os=%s cpu=%d uptime=%s",
			s.hostname, s.kernel, s.osRelease, s.cpuCount, uptime.Truncate(time.Minute)),
		RawPreview: fmt.Sprintf("kernel=%s os=%s cpu=%d", s.kernel, s.osRelease, s.cpuCount),
	})

	// ---- 负载 ---------------------------------------------------------------
	loadGrade := evidence.GradeE2
	if loadPerCPU >= loadPerCPUWarn && last.psiCPU >= psiCPUWarn {
		loadGrade = evidence.GradeE3
	}
	loadNote := ""
	if s.sampleSpanSeconds() >= 0 && s.sampleSpanSeconds() < 60 {
		loadNote = fmt.Sprintf("负载采样仅覆盖 %.0fs，基线是窗口内首个样本，不是历史基线", s.sampleSpanSeconds())
	}
	if !last.loadValid {
		loadNote = "/proc/loadavg 读取失败；0 不是“没有负载”"
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_load", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "proc/loadavg + ps", CollectedAt: last.at, Subject: subj,
		Window: &evidence.TimeWindow{Start: s.samples[0].at, End: last.at},
		Signal: evidence.Signal{
			Name: "load1_per_cpu", Value: round3(loadPerCPU), Unit: "load/cpu",
			Baseline:  round3(perCPU(s.samples[0].load1, s.cpuCount)),
			Threshold: fmt.Sprintf(">=%.1f 或 D-state>0", loadPerCPUWarn),
		},
		Grade: loadGrade, Reliability: 0.9, ProbeValid: last.loadValid, ProbeNote: loadNote,
		Summary: fmt.Sprintf("load1=%.2f（每 CPU %.2f），D-state 进程 %d，zombie %d",
			last.load1, loadPerCPU, last.dState, last.zombie),
		RawPreview: "load=" + fmt.Sprintf("%.2f/%.2f/%.2f", last.load1, last.load5, last.load15) +
			" d_top=" + strings.Join(last.dTop, ","),
	})

	// ---- 内存/swap ----------------------------------------------------------
	memGrade := evidence.GradeE2
	if (memAvail <= memAvailWarn && swapUsed >= swapUsedWarn) ||
		(memAvail <= memAvailWarn && last.psiMem >= psiMemWarn) {
		memGrade = evidence.GradeE3
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_memory", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "/proc/meminfo", CollectedAt: last.at, Subject: subj,
		Signal: evidence.Signal{
			Name: "mem_available_ratio", Value: round3(memAvail), Unit: "ratio",
			Baseline:  round3(ratioU(s.samples[0].memAvailable, s.samples[0].memTotal)),
			Threshold: fmt.Sprintf("<=%.2f 或 swap>=%.0f%%", memAvailWarn, swapUsedWarn*100),
		},
		Grade: memGrade, Reliability: 0.92, ProbeValid: last.memValid,
		ProbeNote: func() string {
			if !last.memValid {
				return "/proc/meminfo 读取失败；0 不是“内存充足”"
			}
			return ""
		}(),
		Summary: fmt.Sprintf("内存可用 %s / %s（%.1f%%），swap 已用 %s / %s（%.1f%%）",
			humanBytes(last.memAvailable), humanBytes(last.memTotal), memAvail*100,
			humanBytes(last.swapTotal-last.swapFree), humanBytes(last.swapTotal), swapUsed*100),
	})

	// ---- PSI ----------------------------------------------------------------
	psiGrade := evidence.GradeE2
	if psiFused {
		psiGrade = evidence.GradeE3
	}
	psiNote := ""
	if !last.psiValid {
		psiNote = "/proc/pressure 不可读，CPU/IO/内存争抢只能靠负载与 swap 间接判断"
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_pressure", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "/proc/pressure/{cpu,io,memory}", CollectedAt: last.at, Subject: subj,
		Signal: evidence.Signal{
			Name: "psi_cpu_some_avg300", Value: round3(last.psiCPU), Unit: "%",
			Threshold: fmt.Sprintf("cpu>=%.0f 或 io>=%.0f 或 mem>=%.0f", psiCPUWarn, psiIOWarn, psiMemWarn),
		},
		Grade: psiGrade, Reliability: 0.9, ProbeValid: last.psiValid, ProbeNote: psiNote,
		Summary: fmt.Sprintf("PSI some avg300：cpu %.2f%%，io %.2f%%，memory %.2f%%",
			last.psiCPU, last.psiIO, last.psiMem),
	})

	// ---- 磁盘/inode ---------------------------------------------------------
	worst, inodeWorst := worstFS(s.fs)
	diskGrade := evidence.GradeE2
	if worst != nil && worst.UsedRatio >= diskUsedWarn {
		diskGrade = evidence.GradeE3
	}
	diskNote := s.fsNote
	if !s.fsValid {
		diskNote = "df 未返回可解析的文件系统；磁盘占用未知，不能当作 0"
	}
	diskSummary := "没有可解析的文件系统"
	if worst != nil {
		diskSummary = fmt.Sprintf("最满文件系统 %s（%s）已用 %.1f%%（%s / %s），共 %d 个文件系统",
			worst.Mount, worst.Device, worst.UsedRatio*100, humanBytes(worst.Used), humanBytes(worst.Size), len(s.fs))
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_disk", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "df -P -B1", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{
			Name: "max_disk_used_ratio", Value: round3(ratioOf(worst)), Unit: "ratio",
			Threshold: fmt.Sprintf(">=%.0f%%", diskUsedWarn*100),
		},
		Grade: diskGrade, Reliability: 0.95, ProbeValid: s.fsValid, ProbeNote: diskNote,
		Summary: diskSummary, RawPreview: fsPreview(s.fs),
	})
	if inodeWorst != nil {
		inodeGrade := evidence.GradeE2
		if inodeWorst.InodeRatio >= inodeUsedWarn {
			inodeGrade = evidence.GradeE3
		}
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_host_inodes", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
			Source: "df -P -i", CollectedAt: now, Subject: subj,
			Signal: evidence.Signal{
				Name: "max_inode_used_ratio", Value: round3(inodeWorst.InodeRatio), Unit: "ratio",
				Threshold: fmt.Sprintf(">=%.0f%%", inodeUsedWarn*100),
			},
			Grade: inodeGrade, Reliability: 0.92, ProbeValid: true,
			Summary: fmt.Sprintf("最满 inode：%s 已用 %.1f%%（%d / %d）",
				inodeWorst.Mount, inodeWorst.InodeRatio*100, inodeWorst.InodesUsed, inodeWorst.Inodes),
		})
	}

	// ---- fd -----------------------------------------------------------------
	fdGrade := evidence.GradeE1
	fdRatio := 0.0
	if s.fileMax > 0 && s.fileMax < 1<<60 {
		fdRatio = float64(s.fileNrAllocated) / float64(s.fileMax)
		if fdRatio >= 0.8 {
			fdGrade = evidence.GradeE2
		}
	}
	fdNote := s.fdNote
	if s.fileMax >= 1<<60 {
		fdNote = "file-max 为内核上限（极大值），用分配数/上限算比例没有意义；要看 ulimit 与进程级 fd"
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_fds", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "/proc/sys/fs/file-nr + file-max", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "file_nr_allocated", Value: s.fileNrAllocated, Unit: "fds"},
		Grade:  fdGrade, Reliability: 0.8, ProbeValid: s.fdValid, ProbeNote: fdNote,
		Summary: fmt.Sprintf("系统已分配 fd %d，file-max %d", s.fileNrAllocated, s.fileMax),
	})

	// ---- 进程状态 -----------------------------------------------------------
	procGrade := evidence.GradeE2
	if last.dState > 0 && last.psiIO >= psiIOWarn {
		procGrade = evidence.GradeE3
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_process_states", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
		Source: "ps -eo stat=,comm=", CollectedAt: last.at, Subject: subj,
		Signal: evidence.Signal{Name: "d_state_processes", Value: last.dState},
		Grade:  procGrade, Reliability: 0.85, ProbeValid: last.procValid,
		ProbeNote: func() string {
			if !last.procValid {
				return "ps 不可用；D-state=0 不是“没有阻塞进程”"
			}
			return ""
		}(),
		Summary:    fmt.Sprintf("D-state %d 个，zombie %d 个", last.dState, last.zombie),
		RawPreview: strings.Join(last.dTop, " "),
	})

	// ---- failed units -------------------------------------------------------
	failedGrade := evidence.GradeE2
	failedSummary := fmt.Sprintf("systemd failed units %d 个", len(s.failedUnits))
	if len(s.failedUnits) > 0 {
		names := make([]string, 0, len(s.failedUnits))
		for _, u := range s.failedUnits {
			names = append(names, u.Unit)
		}
		failedSummary += "：" + strings.Join(names, ",")
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_failed_units", IncidentID: b.IncidentID, Type: evidence.TypeSignal,
		Source: "systemctl --failed", CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "failed_units", Value: len(s.failedUnits)},
		Grade:  failedGrade, Reliability: 0.9, ProbeValid: s.failedValid, ProbeNote: s.failedNote,
		Summary: failedSummary, RawPreview: failedPreview(s.failedUnits),
	})

	// ---- 内核日志 -----------------------------------------------------------
	kernelGrade := evidence.GradeE1
	kernelSummary := "窗口内没有可分类的内核告警"
	if len(s.kernelEvents) > 0 {
		kernelGrade = evidence.GradeE2
		kernelSummary = fmt.Sprintf("窗口内核告警 %d 条（oom=%d segfault=%d io=%d hung=%d fault=%d other=%d）",
			len(s.kernelEvents), len(oomEvents), countKind(s.kernelEvents, "segfault"),
			countKind(s.kernelEvents, "io"), countKind(s.kernelEvents, "hung"),
			countKind(s.kernelEvents, "fault"), countKind(s.kernelEvents, "other"))
	}
	if len(oomEvents) > 0 {
		kernelGrade = evidence.GradeE4
		kernelSummary += "；OOM killer 点名进程: " + eventNames(oomEvents)
	}
	if len(faultEvents) > 0 && kernelGrade.Rank() < evidence.GradeE3.Rank() {
		kernelGrade = evidence.GradeE3
		kernelSummary += "；崩溃/内核故障: " + eventNames(faultEvents)
	}
	if !s.kernelValid {
		kernelSummary = "内核日志探针不可用（不是没有告警）"
	}
	kernelNote := s.kernelNote
	if !s.kernelValid {
		kernelNote = "journalctl 无权限/不可用；不能用空结果证明内核无异常"
	}
	b.Evidences = append(b.Evidences, evidence.Evidence{
		ID: "ev_host_kernel_log", IncidentID: b.IncidentID, Type: evidence.TypeLog,
		Source:      fmt.Sprintf("journalctl -k -p warning --since -%dm", int(c.cfg.Window.Minutes())),
		CollectedAt: now, Subject: subj,
		Signal: evidence.Signal{Name: "kernel_warning_events", Value: len(s.kernelEvents)},
		Grade:  kernelGrade, Reliability: 0.9, ProbeValid: s.kernelValid, ProbeNote: kernelNote,
		Summary: kernelSummary, RawPreview: kernelPreview(s.kernelEvents),
	})

	// ---- socket 概览 (E1) ---------------------------------------------------
	if s.socketValid {
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_host_sockets", IncidentID: b.IncidentID, Type: evidence.TypeTopology,
			Source: "ss -s", CollectedAt: now, Subject: subj,
			Grade: evidence.GradeE1, Reliability: 0.8, ProbeValid: true,
			Summary: "socket 概览: " + oneLine(s.sockets), RawPreview: s.sockets,
		})
	}

	sort.SliceStable(b.Evidences, func(i, j int) bool { return b.Evidences[i].ID < b.Evidences[j].ID })
	return b
}

// ---- snapshot 派生读数 ------------------------------------------------------

func (s *snapshot) latest() statusSample {
	if len(s.samples) == 0 {
		return statusSample{}
	}
	return s.samples[len(s.samples)-1]
}

func (s *snapshot) sampleSpanSeconds() float64 {
	if len(s.samples) < 2 {
		return -1
	}
	return s.samples[len(s.samples)-1].at.Sub(s.samples[0].at).Seconds()
}

func countAtLeast(s *snapshot, cpu, io, mem float64) int {
	n := 0
	last := s.latest()
	if last.psiCPU >= cpu {
		n++
	}
	if last.psiIO >= io {
		n++
	}
	if last.psiMem >= mem {
		n++
	}
	return n
}

func classifyKernel(events []kernelEvent) (oom, faults []kernelEvent) {
	for _, e := range events {
		switch e.Kind {
		case "oom":
			oom = append(oom, e)
		case "segfault", "hung", "fault", "io":
			faults = append(faults, e)
		}
	}
	return
}

func countKind(events []kernelEvent, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func eventNames(events []kernelEvent) string {
	seen := map[string]bool{}
	var names []string
	for _, e := range events {
		name := e.Process
		if name == "" {
			name = e.Kind
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
		if len(names) >= 5 {
			break
		}
	}
	return strings.Join(names, ",")
}

func worstFS(rows []fsRow) (worst, inodeWorst *fsRow) {
	for i := range rows {
		r := &rows[i]
		if worst == nil || r.UsedRatio > worst.UsedRatio {
			worst = r
		}
		if r.InodeValid && (inodeWorst == nil || r.InodeRatio > inodeWorst.InodeRatio) {
			inodeWorst = r
		}
	}
	return
}

func ratioOf(r *fsRow) float64 {
	if r == nil {
		return 0
	}
	return r.UsedRatio
}

func ratioU(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func ratioUsed(total, free uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(total-free) / float64(total)
}

func perCPU(load float64, cpus int) float64 {
	if cpus <= 0 {
		return load
	}
	return load / float64(cpus)
}

func fsPreview(rows []fsRow) string {
	sorted := append([]fsRow{}, rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].UsedRatio > sorted[j].UsedRatio })
	var lines []string
	for i, r := range sorted {
		if i >= 5 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s used=%.1f%% %s/%s",
			r.Mount, r.Device, r.UsedRatio*100, humanBytes(r.Used), humanBytes(r.Size)))
	}
	return strings.Join(lines, "\n")
}

func failedPreview(units []failedUnit) string {
	var lines []string
	for i, u := range units {
		if i >= 8 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s/%s %s", u.Unit, u.Load, u.Active, u.Description))
	}
	return strings.Join(lines, "\n")
}

func kernelPreview(events []kernelEvent) string {
	var lines []string
	for i, e := range events {
		if i >= 8 {
			break
		}
		lines = append(lines, truncate(e.Line, 240))
	}
	return strings.Join(lines, "\n")
}

func oneLine(s string) string {
	return truncate(strings.Join(strings.Fields(s), " "), 240)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
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
		return "host"
	}
	return b.String()
}
