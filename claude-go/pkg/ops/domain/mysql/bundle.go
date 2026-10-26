package mysql

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// 阈值集中在这里，便于评测时替换而不用改分析逻辑。
const (
	connWarnRatio      = 0.85 // 连接占比告警线
	activeThreadsLow   = 5    // 活跃线程低于此值视为“不是流量突增”
	lockAgeWarnSeconds = 30
	replLagWarnSeconds = 30
	deadlockRecent     = 30 * time.Minute
)

// buildBundle 把原始读数归一成证据。每条证据的 ID 在一次采集内稳定，
// 便于重复跑三次对比证据集合（design PR5 验收）。
func (c *Collector) buildBundle(s *snapshot) *evidence.Bundle {
	b := &evidence.Bundle{
		IncidentID:  fmt.Sprintf("inc_%s_%d", c.cfg.Instance, c.now().Unix()),
		Instance:    c.cfg.Instance,
		Domain:      "mysql",
		GeneratedAt: c.now(),
		Window:      s.window,
		Collection: evidence.CollectionInfo{Capabilities: []evidence.Capability{
			{Name: "fixed_mysql_snapshot", Availability: "tool", Detail: "固定状态、processlist、InnoDB、performance_schema 快照；window_minutes 只控制错误日志等回看。"},
			{Name: "read_only_sql", Availability: "tool", Detail: "可选单条 SELECT/SHOW/EXPLAIN/DESC/WITH，结果仅为 E0，不能单独定因。"},
			{Name: "window_time_series", Availability: "manual", Detail: "完整窗口趋势需外部监控；状态 delta/QPS 只覆盖首末样本跨度，digest 是累计值而非窗口增量。"},
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
			Note:          "状态样本按顺序采集；span 是首末样本时间差。window_minutes 不会延长状态采样，QPS/delta 也不代表整个窗口。",
		}
	}
	subj := func(kind, name string) evidence.Subject {
		return evidence.Subject{Kind: kind, Name: name, Instance: c.cfg.Instance}
	}
	now := c.now()

	// ---- 时间线 -------------------------------------------------------------
	for _, sm := range s.samples {
		b.Timeline = append(b.Timeline, evidence.TimelinePoint{
			At:      sm.at,
			Metrics: sm.values,
			Labels:  map[string]string{"source": "SHOW GLOBAL STATUS"},
		})
	}

	// ---- 配置上下文 (E1) -----------------------------------------------------
	if s.variables != nil {
		ver := s.variables["version"]
		summary := fmt.Sprintf("version=%s max_connections=%s buffer_pool=%s slow_query_log=%s long_query_time=%s read_only=%s",
			ver, s.variables["max_connections"], humanBytes(parseI64(s.variables["innodb_buffer_pool_size"])),
			s.variables["slow_query_log"], s.variables["long_query_time"], s.variables["read_only"])
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_config_variables", IncidentID: b.IncidentID, Type: evidence.TypeConfig,
			Source: "SHOW GLOBAL VARIABLES", CollectedAt: now, Subject: subj("instance", c.cfg.Instance),
			Signal: evidence.Signal{Name: "max_connections", Value: parseI64(s.variables["max_connections"])},
			Grade:  evidence.GradeE1, Reliability: 0.98, Summary: summary, ProbeValid: true,
			RawPreview: truncate(fmt.Sprintf("version=%s log_output=%s log_error=%s performance_schema=%s",
				ver, s.variables["log_output"], s.variables["log_error"], s.variables["performance_schema"]), 300),
		})
	}

	// ---- 连接压力 -----------------------------------------------------------
	if len(s.samples) > 0 {
		first, last := s.samples[0], s.samples[len(s.samples)-1]
		maxConn := parseI64(s.variables["max_connections"])
		connNow := last.values["threads_connected"]
		connBase := first.values["threads_connected"]
		ratioNow, ratioBase := 0.0, 0.0
		if maxConn > 0 {
			ratioNow = connNow / float64(maxConn)
			ratioBase = connBase / float64(maxConn)
		}
		running := last.values["threads_running"]
		// 误差 1040 是“连接被拒”的直接证据（独立于状态变量的第二个来源）。
		errs1040 := s.countErrorCode("1040")
		abortedDelta := last.values["aborted_connects"] - first.values["aborted_connects"]

		grade := evidence.GradeE2
		summary := fmt.Sprintf("连接占比 %.1f%%（当前 %.0f/%d，基线 %.0f/%d），运行中线程 %.0f",
			ratioNow*100, connNow, maxConn, connBase, maxConn, running)
		// 观测侧自造的高位信号不能升 E3：采集/监控账号自己的空闲连接占满池子，
		// 与“业务连接泄漏”在状态读数上完全同形，只有归属账号能区分。
		selfIdle := s.selfInducedIdle()
		if ratioNow >= connWarnRatio && (errs1040 > 0 || (s.idleSleepShare() >= 0.5 && !selfIdle)) {
			grade = evidence.GradeE3
		}
		sampleSpan := s.sampleSpanSeconds()
		windowNote := ""
		if c.cfg.Window > 0 && sampleSpan >= 0 && sampleSpan < c.cfg.Window.Seconds()/10 {
			windowNote = fmt.Sprintf("状态采样仅覆盖 %.0fs，基线是同一窗口内的首样本，不是历史基线", sampleSpan)
		}
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_connections", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
			Source: fmt.Sprintf("SHOW GLOBAL STATUS (%d samples)", len(s.samples)), CollectedAt: last.at,
			Window:  &evidence.TimeWindow{Start: first.at, End: last.at},
			Subject: subj("instance", c.cfg.Instance),
			Signal: evidence.Signal{
				Name: "Threads_connected/max_connections", Value: round3(ratioNow), Unit: "ratio",
				Baseline: round3(ratioBase), Delta: round3(ratioNow - ratioBase),
				Threshold: fmt.Sprintf(">=%.2f", connWarnRatio),
			},
			Grade: grade, Reliability: 0.95, ProbeValid: true, ProbeNote: windowNote,
			Summary: summary,
			RawPreview: truncate(fmt.Sprintf(
				"max_used_connections=%.0f aborted_connects_delta=%.0f connection_errors_max_connections=%s",
				last.values["max_used_connections"], abortedDelta,
				s.variables["connection_errors_max_connections"]), 300),
		})
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_traffic", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
			Source: "SHOW GLOBAL STATUS delta", CollectedAt: last.at,
			Window:  &evidence.TimeWindow{Start: first.at, End: last.at},
			Subject: subj("instance", c.cfg.Instance),
			Signal: evidence.Signal{
				Name: "qps", Value: round3(rate(first.values["queries"], last.values["queries"], first.at, last.at)),
				Unit: "queries/s",
			},
			Grade: evidence.GradeE2, Reliability: 0.9, ProbeValid: true,
			Summary: fmt.Sprintf("首末样本跨度 %.1fs 内平均 QPS %.1f（不是整个 window_minutes 的平均），末次活跃线程 %.0f",
				s.sampleSpanSeconds(), rate(first.values["queries"], last.values["queries"], first.at, last.at), running),
		})
	}

	// ---- processlist 聚合 ----------------------------------------------------
	if s.procValid {
		sleep, total := s.sleepStats()
		systemSessions := s.systemProcessCount()
		top := s.topProcessGroup()
		share := 0.0
		if total > 0 {
			share = float64(sleep) / float64(total)
		}
		grade := evidence.GradeE2
		summary := fmt.Sprintf("非系统会话 %d 个，其中 Sleep %d 个（%.0f%%）", total, sleep, share*100)
		if systemSessions > 0 {
			summary += fmt.Sprintf("；另有系统线程 %d 个，不计入该比例", systemSessions)
		}
		if top != "" {
			summary += "；最大非系统分组 " + top
		}
		note := ""
		if s.selfInducedIdle() {
			u, n, _ := s.dominantSleepUser()
			summary += fmt.Sprintf("；其中采集账号 %s 自身占 %d 个，属观测侧信号", u, n)
			note = "高位连接由采集账号自身造成，不能归因为业务连接泄漏"
		}
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_processlist_sleep", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
			Source: "information_schema.processlist grouped", CollectedAt: now,
			Subject: subj("instance", c.cfg.Instance),
			Signal:  evidence.Signal{Name: "sleep_session_share", Value: round3(share), Unit: "ratio"},
			Grade:   grade, Reliability: 0.9, Summary: summary, ProbeValid: true, ProbeNote: note,
			RawPreview: truncate(s.processPreview(), 600),
		})
	}

	// ---- 锁 -----------------------------------------------------------------
	if s.lockValid {
		grade := evidence.GradeE2
		if s.lockWaits > 0 && s.longestTrxAge() >= lockAgeWarnSeconds {
			grade = evidence.GradeE3
		}
		summary := fmt.Sprintf("当前锁等待 %d 个", s.lockWaits)
		if len(s.lockDetails) > 0 {
			summary += "；首个等待: " + s.lockDetails[0].Schema + "." + s.lockDetails[0].Table +
				" (" + s.lockDetails[0].LockMode + ")"
		}
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_lock_waits", IncidentID: b.IncidentID, Type: evidence.TypeSignal,
			Source: "performance_schema.data_lock_waits", CollectedAt: now,
			Subject: subj("instance", c.cfg.Instance),
			Signal:  evidence.Signal{Name: "lock_waits", Value: s.lockWaits, Threshold: ">0"},
			Grade:   grade, Reliability: 0.95, Summary: summary, ProbeValid: true,
			RawPreview: truncate(s.lockPreview(), 600),
		})
	}
	if s.mdlValid {
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_metadata_locks", IncidentID: b.IncidentID, Type: evidence.TypeSignal,
			Source: "performance_schema.metadata_locks", CollectedAt: now,
			Subject: subj("instance", c.cfg.Instance),
			Signal:  evidence.Signal{Name: "pending_metadata_locks", Value: s.mdlPending},
			Grade:   evidence.GradeE2, Reliability: 0.9, ProbeValid: true,
			Summary: fmt.Sprintf("等待中的 metadata lock %d 个", s.mdlPending),
		})
	}

	// ---- 事务 ---------------------------------------------------------------
	if s.trxValid {
		age := s.longestTrxAge()
		var rowsLocked int64
		for _, t := range s.trx {
			if t.RowsLocked > rowsLocked {
				rowsLocked = t.RowsLocked
			}
		}
		grade := evidence.GradeE2
		if age >= lockAgeWarnSeconds {
			grade = evidence.GradeE2 // 单来源不够 E3；与 lock_waits 合并时由后者升 E3
		}
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_innodb_trx", IncidentID: b.IncidentID, Type: evidence.TypeSignal,
			Source: "information_schema.innodb_trx", CollectedAt: now,
			Subject: subj("instance", c.cfg.Instance),
			Signal: evidence.Signal{Name: "longest_trx_age", Value: age, Unit: "s",
				Threshold: fmt.Sprintf(">=%d", lockAgeWarnSeconds)},
			Grade: grade, Reliability: 0.95, ProbeValid: true,
			Summary:    fmt.Sprintf("活跃事务 %d 个，最长 %.0fs，最多锁定行 %d", len(s.trx), float64(age), rowsLocked),
			RawPreview: truncate(s.trxPreview(), 600),
		})
	}

	// ---- 复制 ---------------------------------------------------------------
	if s.replValid {
		switch {
		case s.replica == nil:
			summary := s.replNote
			if strings.TrimSpace(summary) == "" {
				summary = "实例是主库（SHOW REPLICA STATUS 返回 0 行）"
			}
			b.Evidences = append(b.Evidences, evidence.Evidence{
				ID: "ev_replication", IncidentID: b.IncidentID, Type: evidence.TypeTopology,
				Source: "SHOW REPLICA STATUS", CollectedAt: now, Subject: subj("instance", c.cfg.Instance),
				Signal: evidence.Signal{Name: "role", Value: "primary"},
				Grade:  evidence.GradeE1, Reliability: 0.95, ProbeValid: true,
				Summary: summary, ProbeNote: s.replNote,
			})
		default:
			r := s.replica
			grade := evidence.GradeE2
			summary := fmt.Sprintf("复制 IO=%s SQL=%s 延迟=%ds", r.IO, r.SQL, r.LagSeconds)
			if r.LastError != "" {
				summary += "；最后错误: " + truncate(r.LastError, 160)
			}
			b.Evidences = append(b.Evidences, evidence.Evidence{
				ID: "ev_replication", IncidentID: b.IncidentID, Type: evidence.TypeSignal,
				Source: "SHOW REPLICA STATUS", CollectedAt: now, Subject: subj("instance", c.cfg.Instance),
				Signal: evidence.Signal{Name: "seconds_behind_source", Value: r.LagSeconds, Unit: "s",
					Threshold: fmt.Sprintf(">=%d", replLagWarnSeconds)},
				Grade: grade, Reliability: 0.95, ProbeValid: true, Summary: summary,
				RawPreview: truncate(fmt.Sprintf("source_host=%s sql_delay=%ds retrieved=%s executed=%s",
					normalizeAddr(r.SourceHost), r.SQLDelaySeconds, r.RetrievedGTID, r.ExecutedGTID), 300),
			})
		}
	}

	// ---- 慢查询 digest -------------------------------------------------------
	if s.digValid && len(s.digest) > 0 {
		d := s.digest[0]
		noIdxShare := 0.0
		if d.Count > 0 {
			noIdxShare = float64(d.NoIndexUse) / float64(d.Count)
		}
		grade := evidence.GradeE2
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_slow_digest", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
			Source: "performance_schema.events_statements_summary_by_digest", CollectedAt: now,
			Subject: subj("database", d.Schema),
			Signal: evidence.Signal{Name: "top_digest_avg_latency", Value: round3(d.AvgLatency().Seconds()),
				Unit: "s", Threshold: ">0.1", Baseline: d.Count},
			Grade: grade, Reliability: 0.9, ProbeValid: true,
			ProbeNote: "digest 是实例累计统计（重启或统计重置后清零），不是 window_minutes 增量；no-index 比例只属于该 digest，不能单独证明当前故障。",
			Summary: fmt.Sprintf("累计总耗时最高的 digest（schema=%s）平均 %.3fms；未走索引 %d/%d 次（%.1f%%，累计口径）",
				d.Schema, float64(d.AvgLatency())/float64(time.Millisecond), d.NoIndexUse, d.Count, noIdxShare*100),
			RawPreview: truncate(d.Digest+" | rows_examined="+fmt.Sprint(d.RowsExam)+
				" rows_sent="+fmt.Sprint(d.RowsSent)+" no_index_used="+fmt.Sprint(d.NoIndexUse), 600),
		})
	}

	// ---- 容量 ---------------------------------------------------------------
	if s.capValid {
		frac := 0.0
		if s.capTotals.Data > 0 {
			frac = float64(s.capTotals.Free) / float64(s.capTotals.Data)
		}
		grade := evidence.GradeE1
		if frac >= 0.5 && s.capTotals.Free > 1<<30 {
			grade = evidence.GradeE2
		}
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_capacity", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
			Source: "information_schema.tables", CollectedAt: now, Subject: subj("instance", c.cfg.Instance),
			Signal: evidence.Signal{Name: "data_free/data_length", Value: round3(frac), Unit: "ratio",
				Threshold: ">=0.5 且 >1GiB"},
			Grade: grade, Reliability: 0.85, ProbeValid: true,
			ProbeNote: "容量只汇总非系统库（排除 mysql/information_schema/performance_schema/sys）；0 表/0B 表示该作用域为空，不代表实例异常或磁盘为空。",
			Summary: fmt.Sprintf("非系统库合计：%d 张表，数据 %s，索引 %s，可回收碎片 %s（%.0f%%）",
				s.capTotals.Tables, humanBytes(s.capTotals.Data), humanBytes(s.capTotals.Index),
				humanBytes(s.capTotals.Free), frac*100),
			RawPreview: truncate(s.capacityPreview(), 600),
		})
	}

	// ---- 错误日志 -----------------------------------------------------------
	if s.errLogOK {
		errs := 0
		var codes []string
		for _, e := range s.errorLog {
			if strings.EqualFold(e.Prio, "ERROR") || strings.EqualFold(e.Prio, "SYSTEM") {
				errs++
				codes = append(codes, e.Code)
			}
		}
		grade := evidence.GradeE2
		summary := fmt.Sprintf("窗口内错误日志 %d 条", errs)
		if len(codes) > 0 {
			summary += "，错误码: " + strings.Join(uniqueTop(codes, 5), ",")
		}
		b.Evidences = append(b.Evidences, evidence.Evidence{
			ID: "ev_error_log", IncidentID: b.IncidentID, Type: evidence.TypeLog,
			Source:      fmt.Sprintf("performance_schema.error_log (last %dm)", int(c.cfg.Window.Minutes())),
			CollectedAt: now, Subject: subj("instance", c.cfg.Instance),
			Signal: evidence.Signal{Name: "error_count", Value: errs},
			Grade:  grade, Reliability: 0.9, ProbeValid: true, Summary: summary,
			RawPreview: truncate(s.errorPreview(), 800),
		})
	}

	// ---- 死锁 ---------------------------------------------------------------
	if s.deadlockValid {
		if s.deadlock == nil {
			b.Evidences = append(b.Evidences, evidence.Evidence{
				ID: "ev_deadlock", IncidentID: b.IncidentID, Type: evidence.TypeLog,
				Source: "SHOW ENGINE INNODB STATUS", CollectedAt: now, Subject: subj("instance", c.cfg.Instance),
				Grade: evidence.GradeE1, Reliability: 0.7, ProbeValid: true,
				Summary: "SHOW ENGINE INNODB STATUS 中未发现死锁段",
			})
		} else {
			grade := evidence.GradeE1
			summary := "历史死锁（早于本次时间窗）"
			if s.deadlock.Recent {
				// 死锁报告点名了等待/阻塞事务，属于“证据本身给出因果”。
				grade = evidence.GradeE4
				summary = fmt.Sprintf("最近一次死锁 %s（等待事务与阻塞事务在报告中点名）",
					s.deadlock.At.Format("2006-01-02 15:04:05"))
			}
			b.Evidences = append(b.Evidences, evidence.Evidence{
				ID: "ev_deadlock", IncidentID: b.IncidentID, Type: evidence.TypeLog,
				Source: "SHOW ENGINE INNODB STATUS", CollectedAt: now, Subject: subj("instance", c.cfg.Instance),
				Grade: grade, Reliability: 0.95, ProbeValid: true, Summary: summary,
				ProbeNote: s.deadlockNote, RawPreview: truncate(s.deadlock.Excerpt, 1500),
			})
		}
	}

	// ---- buffer pool 命中率 --------------------------------------------------
	if len(s.samples) > 1 {
		first, last := s.samples[0], s.samples[len(s.samples)-1]
		req := last.values["innodb_buffer_pool_read_requests"] - first.values["innodb_buffer_pool_read_requests"]
		reads := last.values["innodb_buffer_pool_reads"] - first.values["innodb_buffer_pool_reads"]
		if req > 0 {
			miss := reads / req
			grade := evidence.GradeE2
			if miss >= 0.05 {
				grade = evidence.GradeE2
			}
			b.Evidences = append(b.Evidences, evidence.Evidence{
				ID: "ev_buffer_pool", IncidentID: b.IncidentID, Type: evidence.TypeMetric,
				Source: "SHOW GLOBAL STATUS delta", CollectedAt: last.at,
				Window:  &evidence.TimeWindow{Start: first.at, End: last.at},
				Subject: subj("instance", c.cfg.Instance),
				Signal: evidence.Signal{Name: "buffer_pool_miss_ratio", Value: round3(miss), Unit: "ratio",
					Threshold: ">=0.02"},
				Grade: grade, Reliability: 0.85, ProbeValid: true,
				Summary: fmt.Sprintf("InnoDB buffer pool 缺失率 %.2f%%（读请求 %.0f）", miss*100, req),
			})
		}
	}

	sort.SliceStable(b.Evidences, func(i, j int) bool { return b.Evidences[i].ID < b.Evidences[j].ID })
	return b
}

// ---- snapshot 派生读数 ------------------------------------------------------

func (s *snapshot) sleepStats() (sleep, total int) {
	for _, p := range s.process {
		if isSystemProcess(p) {
			continue
		}
		total += int(p.Count)
		if strings.EqualFold(p.Command, "Sleep") {
			sleep += int(p.Count)
		}
	}
	return sleep, total
}

func isSystemProcess(p processRow) bool {
	u := strings.ToLower(strings.TrimSpace(p.User))
	return u == "event_scheduler" || u == "system user" || strings.HasPrefix(u, "mysql.")
}

func (s *snapshot) systemProcessCount() int {
	total := 0
	for _, p := range s.process {
		if isSystemProcess(p) {
			total += int(p.Count)
		}
	}
	return total
}

// dominantSleepUser 返回 Sleep 会话最多的账号及其数量/总 Sleep 数。
func (s *snapshot) dominantSleepUser() (user string, count, total int) {
	byUser := map[string]int{}
	for _, p := range s.process {
		if isSystemProcess(p) || !strings.EqualFold(p.Command, "Sleep") {
			continue
		}
		byUser[p.User] += int(p.Count)
		total += int(p.Count)
	}
	for u, n := range byUser {
		if n > count {
			user, count = u, n
		}
	}
	return user, count, total
}

// selfInducedIdle 判断占满连接的空闲会话是否主要来自采集账号自身。
//
// 阈值 10 是为了不把采集器自己的 1–2 条连接当成压力；真正的“监控连接泄漏”
// 会达到几十上百条，与业务泄漏在读数上无法区分，只能靠账号归属。
func (s *snapshot) selfInducedIdle() bool {
	if !s.probeUserValid {
		return false
	}
	u, n, total := s.dominantSleepUser()
	if total == 0 || n < 10 {
		return false
	}
	return strings.EqualFold(u, s.probeUser) && n*100/total >= 80
}

func (s *snapshot) idleSleepShare() float64 {
	sleep, total := s.sleepStats()
	if total == 0 {
		return 0
	}
	return float64(sleep) / float64(total)
}

// sampleSpanSeconds 返回状态采样覆盖的秒数；不足两个样本返回 -1。
func (s *snapshot) sampleSpanSeconds() float64 {
	if len(s.samples) < 2 {
		return -1
	}
	return s.samples[len(s.samples)-1].at.Sub(s.samples[0].at).Seconds()
}

func (s *snapshot) longestTrxAge() int64 {
	var age int64
	for _, t := range s.trx {
		if t.AgeSeconds > age {
			age = t.AgeSeconds
		}
	}
	return age
}

func (s *snapshot) topProcessGroup() string {
	var best *processRow
	for i := range s.process {
		p := &s.process[i]
		if isSystemProcess(*p) {
			continue
		}
		if best == nil || p.Count > best.Count || (p.Count == best.Count && p.MaxTime > best.MaxTime) {
			best = p
		}
	}
	if best == nil {
		return ""
	}
	return fmt.Sprintf("%s@%s cmd=%s state=%s count=%d max_time=%ds",
		best.User, normalizeAddr(best.Host), best.Command, best.State, best.Count, best.MaxTime)
}

func (s *snapshot) processPreview() string {
	var lines []string
	for i, p := range s.process {
		if i >= 8 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s@%s db=%s cmd=%s state=%q n=%d max=%ds",
			p.User, normalizeAddr(p.Host), p.DB, p.Command, p.State, p.Count, p.MaxTime))
	}
	return strings.Join(lines, "\n")
}

func (s *snapshot) lockPreview() string {
	var lines []string
	for i, l := range s.lockDetails {
		if i >= 5 {
			break
		}
		lines = append(lines, fmt.Sprintf("waiting=%s blocking=%s %s.%s %s %s",
			l.WaitingTrx, l.BlockingTrx, l.Schema, l.Table, l.LockType, l.LockMode))
	}
	return strings.Join(lines, "\n")
}

func (s *snapshot) trxPreview() string {
	var lines []string
	for i, t := range s.trx {
		if i >= 5 {
			break
		}
		lines = append(lines, fmt.Sprintf("trx=%s state=%s age=%ds thread=%d rows_locked=%d rows_mod=%d query=%s",
			t.TrxID, t.State, t.AgeSeconds, t.ThreadID, t.RowsLocked, t.RowsMod, truncate(t.Query, 120)))
	}
	return strings.Join(lines, "\n")
}

func (s *snapshot) countErrorCode(code string) int {
	n := 0
	for _, e := range s.errorLog {
		if e.Code == code {
			n++
		}
	}
	return n
}

func (s *snapshot) errorPreview() string {
	var lines []string
	for i, e := range s.errorLog {
		if i >= 10 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s [%s] %s %s: %s",
			e.At.Format("15:04:05"), e.Prio, e.Code, e.Subsys, truncate(e.Message, 180)))
	}
	return strings.Join(lines, "\n")
}

func (s *snapshot) capacityPreview() string {
	var lines []string
	for i, t := range s.capacity {
		if i >= 5 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s.%s engine=%s rows=%d data=%s index=%s free=%s",
			t.Schema, t.Table, t.Engine, t.Rows, humanBytes(t.Data), humanBytes(t.Index), humanBytes(t.Free)))
	}
	return strings.Join(lines, "\n")
}

// ---- 小工具 ----------------------------------------------------------------

func rate(from, to float64, t0, t1 time.Time) float64 {
	sec := t1.Sub(t0).Seconds()
	if sec <= 0 {
		return 0
	}
	if to < from { // 计数器被重置（重启）
		return 0
	}
	return (to - from) / sec
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func uniqueTop(items []string, n int) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
		if len(out) >= n {
			break
		}
	}
	return out
}

func parseI64(s string) int64 {
	var n int64
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}
