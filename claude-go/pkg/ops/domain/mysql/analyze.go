package mysql

import (
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

// analyze 在证据包上做确定性推断：候选假设、矛盾、缺口。
//
// 这里刻意不做“打分排序 + 挑一个当答案”。规则只有三条：
//  1. 触发条件来自证据读数，不是模型措辞；
//  2. 假设进入 supported 必须有 E3+ 支持证据；
//  3. 根因标记只是“申请”，最终由 Bundle.RootCauseEligible 复核。
//
// 因此健康实例会得到“没有根因”的空结论，而不是硬凑一个。
func analyze(b *evidence.Bundle, s *snapshot) {
	conn := b.EvidenceByID("ev_connections")
	sleep := b.EvidenceByID("ev_processlist_sleep")
	traffic := b.EvidenceByID("ev_traffic")
	errLog := b.EvidenceByID("ev_error_log")
	locks := b.EvidenceByID("ev_lock_waits")
	trx := b.EvidenceByID("ev_innodb_trx")
	repl := b.EvidenceByID("ev_replication")
	digest := b.EvidenceByID("ev_slow_digest")
	pool := b.EvidenceByID("ev_buffer_pool")
	deadlock := b.EvidenceByID("ev_deadlock")
	capacity := b.EvidenceByID("ev_capacity")
	mdl := b.EvidenceByID("ev_metadata_locks")

	ratio := signalFloat(conn, "Threads_connected/max_connections")
	errs1040 := s.countErrorCode("1040")
	sleepShare := signalFloat(sleep, "sleep_session_share")
	active := 0.0
	qps := 0.0
	if traffic != nil {
		qps = signalFloat(traffic, "qps")
	}
	if len(s.samples) > 0 {
		active = s.samples[len(s.samples)-1].values["threads_running"]
	}
	lockWaits := signalFloat(locks, "lock_waits")
	trxAge := signalFloat(trx, "longest_trx_age")
	lag := signalFloat(repl, "seconds_behind_source")
	mdlPending := signalFloat(mdl, "pending_metadata_locks")

	// ---- H1 连接泄漏/空闲连接堆积 -------------------------------------------
	if conn != nil && (ratio >= connWarnRatio || errs1040 > 0) {
		support := []string{"ev_connections"}
		if sleep != nil && sleepShare >= 0.5 {
			support = append(support, "ev_processlist_sleep")
		}
		if errs1040 > 0 {
			support = append(support, "ev_error_log")
		}
		h := evidence.Hypothesis{
			ID:        "H1",
			Statement: "连接池被空闲会话占用（连接泄漏/连接池未回收/监控或批处理连接堆积），而非真实并发增长",
			Mechanism: "空闲连接长期占用 max_connections，使连接余量下降；若继续增长才会导致新连接被拒（1040）",
			Predicts: []string{
				"processlist 中 Sleep 会话占比高且集中在同一 user/host",
				"Threads_running 远低于 Threads_connected",
				"若已耗尽，错误日志出现 Error 1040 / Connection_errors_max_connections 增长",
			},
			Disconfirms: []string{
				"Sleep 会话占比低于 20% 且 Threads_running 与连接数同步上升",
				"QPS 相对窗口起点显著上升，且连接数随请求量同步变化",
				"空闲会话全部来自采集/监控账号自身（观测侧信号，不是业务泄漏）",
			},
			Support:    support,
			Status:     hypothesisStatus(b, support),
			Confidence: hypothesisConfidence(b, support),
			TestPlan: []string{
				"按 user/host 分组查看 Sleep 会话与最长空闲时间",
				"对比 QPS 与活跃线程，排除流量突增",
				"确认 max_connections 与实际连接峰值，排除配置过低",
			},
			RootCause: false,
		}
		if h.Status == evidence.HypothesisSupported {
			h.RootCause = true
		}
		b.Hypotheses = append(b.Hypotheses, h)

		// 备选解释：真实流量突增。只在“连接高但活跃低”时生成并证伪，
		// 避免健康实例里堆一排无关假设。
		if active <= activeThreadsLow && qps >= 0 {
			trafficStatus := evidence.HypothesisRefuted
			contradict := []string{}
			if traffic != nil {
				contradict = append(contradict, "ev_traffic")
			}
			b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
				ID:        "H2",
				Statement: "连接增长由业务流量突增导致",
				Mechanism: "请求量上升使并发连接同步增长，属于容量问题而非泄漏",
				Predicts:  []string{"Threads_running 与 QPS 同步上升", "连接数与请求量变化方向一致"},
				Disconfirms: []string{
					fmt.Sprintf("Threads_running 低（当前 %.0f）且 QPS 未显著上升（当前 %.1f）", active, qps),
					"Sleep 会话占比高，连接并未参与执行",
				},
				Contradict: contradict,
				Status:     trafficStatus,
				Confidence: 0.15,
				TestPlan:   []string{"对比告警窗口与前一等长窗口的 QPS/活跃线程"},
			})
		}
	}

	// ---- H3 锁等待/长事务阻塞 ------------------------------------------------
	if locks != nil && (lockWaits > 0 || trxAge >= lockAgeWarnSeconds) {
		support := []string{"ev_lock_waits"}
		if trx != nil {
			support = append(support, "ev_innodb_trx")
		}
		h := evidence.Hypothesis{
			ID:        "H3",
			Statement: "请求变慢由行锁等待/长事务阻塞导致",
			Mechanism: "长事务持有行锁，后续会话进入 lock wait，连接与线程被占用",
			Predicts: []string{
				"performance_schema.data_lock_waits 出现等待事务与阻塞事务配对",
				"information_schema.innodb_trx 中存在长于 30s 的事务",
			},
			Disconfirms: []string{
				"锁等待为 0 且最长事务短于 5s",
				"等待集中在 metadata lock 而非行锁",
			},
			Support:    support,
			Status:     hypothesisStatus(b, support),
			Confidence: hypothesisConfidence(b, support),
			TestPlan: []string{
				"查看等待事务的 SQL 与阻塞事务的 trx_mysql_thread_id",
				"核对阻塞事务是否来自批处理/未提交事务",
			},
		}
		if h.Status == evidence.HypothesisSupported {
			h.RootCause = true
		}
		b.Hypotheses = append(b.Hypotheses, h)
	}

	// ---- H4 复制延迟 ---------------------------------------------------------
	if repl != nil && (lag >= replLagWarnSeconds || replHasFailure(s)) {
		support := []string{"ev_replication"}
		b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
			ID:        "H4",
			Statement: "副本延迟/复制中断导致读到旧数据或切换后数据不一致",
			Mechanism: "SQL 线程回放落后或 IO 线程断开，Seconds_Behind_Source 持续增长",
			Predicts:  []string{"Seconds_Behind_Source 超过 30s 且持续增长", "Replica_IO_Running/Replica_SQL_Running 非 Yes"},
			Disconfirms: []string{
				"复制线程均为 Yes 且延迟 <5s",
				"实例本身是主库（SHOW REPLICA STATUS 为空）",
			},
			Support:    support,
			Status:     hypothesisStatus(b, support),
			Confidence: hypothesisConfidence(b, support),
			TestPlan: []string{
				"连续 3 个采样点观察延迟趋势，而非单点",
				"读取 Last_Error 与 relay log 位置",
			},
		})
	}

	// ---- H5 慢查询/缺索引 ----------------------------------------------------
	if digest != nil {
		lat := signalFloat(digest, "top_digest_avg_latency")
		if lat >= 0.1 {
			b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
				ID:        "H5",
				Statement: "最耗时语句缺少可用索引，导致扫描行数放大",
				Mechanism: "查询未使用索引，扫描行数远大于返回行数，CPU/IO 与锁持有时间上升",
				Predicts:  []string{"digest 的 SUM_NO_INDEX_USED 大于 0", "扫描行数 / 返回行数 比值很高"},
				Disconfirms: []string{
					"执行计划显示使用索引且扫描行数接近返回行数",
					"慢查询集中在写入而非查询",
				},
				Support:    []string{"ev_slow_digest"},
				Status:     hypothesisStatus(b, []string{"ev_slow_digest"}),
				Confidence: hypothesisConfidence(b, []string{"ev_slow_digest"}),
				TestPlan:   []string{"对 digest 对应的规范化语句执行 EXPLAIN", "核对表统计信息是否过期"},
			})
		}
	}

	// ---- H6 buffer pool 不足 ------------------------------------------------
	if pool != nil {
		miss := signalFloat(pool, "buffer_pool_miss_ratio")
		if miss >= 0.02 {
			b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
				ID:        "H6",
				Statement: "buffer pool 不足导致物理读放大、查询抖动",
				Mechanism: "热数据无法常驻，读请求频繁落到磁盘",
				Predicts:  []string{"Innodb_buffer_pool_reads / read_requests 比例上升", "物理读与 IO 等待同步上升"},
				Disconfirms: []string{
					"缺失率低于 1% 且物理读平稳",
					"实例内存充足但瓶颈在锁或网络",
				},
				Support:    []string{"ev_buffer_pool"},
				Status:     hypothesisStatus(b, []string{"ev_buffer_pool"}),
				Confidence: hypothesisConfidence(b, []string{"ev_buffer_pool"}),
				TestPlan:   []string{"采样多个窗口的缺失率", "对比 innodb_buffer_pool_size 与工作集大小"},
			})
		}
	}

	// ---- H7 死锁 ------------------------------------------------------------
	if deadlock != nil && deadlock.Grade == evidence.GradeE4 {
		b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
			ID:        "H7",
			Statement: "事务死锁被 InnoDB 回滚，业务侧表现为偶发失败",
			Mechanism: "两个事务以相反顺序持有/申请行锁，InnoDB 选择牺牲其中一个",
			Predicts:  []string{"SHOW ENGINE INNODB STATUS 出现窗口内的 LATEST DETECTED DEADLOCK", "错误日志或应用日志出现 1213"},
			Disconfirms: []string{
				"死锁时间早于告警窗口",
				"只有锁等待没有死锁回滚",
			},
			Support:    []string{"ev_deadlock"},
			Status:     evidence.HypothesisSupported,
			Confidence: 0.9,
			TestPlan:   []string{"读取死锁段中的两条事务 SQL 与索引", "确认加锁顺序是否一致"},
			RootCause:  true,
		})
	}

	// ---- H8 表空间碎片 ------------------------------------------------------
	if capacity != nil {
		frac := signalFloat(capacity, "data_free/data_length")
		if frac >= 0.5 {
			b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
				ID:          "H8",
				Statement:   "表空间碎片导致磁盘占用虚高（不一定影响查询）",
				Mechanism:   "大量 DELETE/重建留下 data_free，文件不自动收缩",
				Predicts:    []string{"information_schema.tables 的 DATA_FREE 占比高", "磁盘占用高于数据实际大小"},
				Disconfirms: []string{"DATA_FREE 占比低于 10%", "磁盘告警与表空间增长无关（binlog/redo/临时文件）"},
				Support:     []string{"ev_capacity"},
				Status:      hypothesisStatus(b, []string{"ev_capacity"}),
				Confidence:  hypothesisConfidence(b, []string{"ev_capacity"}),
				TestPlan:    []string{"按表排序 DATA_FREE", "确认磁盘告警来源是 ibd 还是 binlog/redo"},
			})
		}
	}

	// ---- H9 metadata lock ---------------------------------------------------
	if mdl != nil && mdlPending > 0 {
		b.Hypotheses = append(b.Hypotheses, evidence.Hypothesis{
			ID:          "H9",
			Statement:   "DDL 被未提交事务的 metadata lock 阻塞",
			Mechanism:   "长事务持有 MDL，后续 DDL 进入 PENDING，进而阻塞其他查询",
			Predicts:    []string{"performance_schema.metadata_locks 出现 PENDING", "存在长事务且其 SQL 访问同一对象"},
			Disconfirms: []string{"metadata_locks 无 PENDING", "等待来自行锁而非 MDL"},
			Support:     []string{"ev_metadata_locks"},
			Status:      hypothesisStatus(b, []string{"ev_metadata_locks"}),
			Confidence:  hypothesisConfidence(b, []string{"ev_metadata_locks"}),
			TestPlan:    []string{"关联 metadata_locks 与 innodb_trx 的线程/对象", "确认阻塞者是否可安全终止"},
		})
	}

	buildContradictions(b, s, conn, traffic, errLog, locks, trx, deadlock, sleep, ratio, errs1040, lockWaits, trxAge, sleepShare)
	buildMissing(b, s)
	b.Summary = summarize(b)
}

// buildContradictions 把“两个口径不一致”显式写出来。
//
// 矛盾不是错误，而是下一步取证的入口；只有 high 且未解释的矛盾才会阻断根因门控。
func buildContradictions(b *evidence.Bundle, s *snapshot, conn, traffic, errLog, locks, trx, deadlock, sleep *evidence.Evidence,
	ratio float64, errs1040 int, lockWaits, trxAge, sleepShare float64) {
	if conn != nil && traffic != nil && ratio >= connWarnRatio {
		active := 0.0
		if len(s.samples) > 0 {
			active = s.samples[len(s.samples)-1].values["threads_running"]
		}
		desc := fmt.Sprintf("连接占比 %.0f%% 但活跃线程仅 %.0f，连接高不等于并发高", ratio*100, active)
		res := ""
		if sleepShare >= 0.5 {
			res = fmt.Sprintf("已由 processlist 解释：Sleep 会话占比 %.0f%%，属空闲连接堆积", sleepShare*100)
		}
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "C1", Description: desc, EvidenceA: "ev_connections", EvidenceB: "ev_traffic",
			Severity: "medium", Resolution: res,
		})
	}
	if conn != nil && errLog != nil && errs1040 > 0 && ratio < connWarnRatio {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "C2",
			Description: fmt.Sprintf("错误日志出现 1040 连接拒绝 %d 次，但末次采样连接占比仅 %.0f%%；采样窗口可能错过峰值",
				errs1040, ratio*100),
			EvidenceA: "ev_error_log", EvidenceB: "ev_connections",
			Severity: "high",
			// 不写 Resolution：这是未解释的矛盾，必须阻断“连接打满”作为根因。
		})
	}
	if locks != nil && trx != nil && lockWaits > 0 && trxAge < 5 {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "C3",
			Description: fmt.Sprintf("存在 %d 个锁等待，但最长事务仅 %.0fs，更像高频短冲突而非长事务阻塞",
				int(lockWaits), trxAge),
			EvidenceA: "ev_lock_waits", EvidenceB: "ev_innodb_trx",
			Severity: "medium",
		})
	}
	if conn != nil && sleep != nil && s.selfInducedIdle() {
		// 即使同时存在 1040，也不能据此完成归因：拒绝可能是采集账号自己造成的。
		// 保留为 high 未解释矛盾，阻断“业务连接泄漏”这一根因。
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID: "C5",
			Description: "高连接占比主要来自采集账号自身的空闲会话；" +
				"该信号由观测侧制造，不能归因为业务连接泄漏（1040 亦可能由它触发）",
			EvidenceA: "ev_connections", EvidenceB: "ev_processlist_sleep",
			Severity: "high",
		})
	}
	if deadlock != nil && deadlock.Grade == evidence.GradeE4 && locks != nil && lockWaits == 0 {
		b.Contradictions = append(b.Contradictions, evidence.Contradiction{
			ID:          "C4",
			Description: "当前存在新鲜死锁报告，但没有锁等待：死锁已被 InnoDB 回滚，症状可能已自愈",
			EvidenceA:   "ev_deadlock", EvidenceB: "ev_lock_waits",
			Severity:   "medium",
			Resolution: "死锁是过去事件，只能解释当时的失败，不能解释当前持续症状",
		})
	}
}

// buildMissing 输出“还缺什么证据”，而不是用置信度话术掩盖。
func buildMissing(b *evidence.Bundle, s *snapshot) {
	if span := s.sampleSpanSeconds(); span >= 0 && s.samples != nil && span < 60 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       fmt.Sprintf("覆盖完整窗口的状态时间序列（当前采样仅 %.0fs）", span),
			Why:        "短时顺序采样只能证明首末样本间的瞬态/delta，不能证明整个 window_minutes 的趋势",
			NextAction: "需人工：用外部监控补齐窗口曲线；本工具的 window_minutes 不会延长状态采样",
		})
	}
	if len(s.samples) < 3 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "至少 3 个状态采样点（当前 " + fmt.Sprint(len(s.samples)) + "）",
			Why:        "单点快照无法区分“一直高”与“刚刚升高”，也无法算增量",
			NextAction: "提高采集频率或延长窗口后重跑；不要在单点上判定趋势",
		})
	}
	if s.replValid && s.replica != nil && s.replica.LagSeconds >= replLagWarnSeconds {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "复制延迟的时间序列与 relay log 位置变化",
			Why:        "单点 Seconds_Behind_Source 可能瞬时抖动，无法判断是持续落后还是回放追平中",
			NextAction: "连续采样 SHOW REPLICA STATUS 3–5 次，并读取 Last_Error / 主库 binlog 位置",
		})
	}
	if s.digValid && len(s.digest) > 0 && s.digest[0].AvgLatency() >= 100*1e6 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "最耗时 digest 的执行计划（EXPLAIN）",
			Why:        "digest 只有规范化文本与耗时，没有访问路径，无法证明“缺索引”",
			NextAction: "工具支持：从 digest 还原参数化语句后，对目标表单条执行 EXPLAIN（E0，只读）；不得凭累计 no-index 比例直接定因",
		})
	}
	if s.selfInducedIdle() {
		u, n, _ := s.dominantSleepUser()
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "空闲连接归属确认（采集账号 " + u + " 自身已占 " + fmt.Sprint(n) + " 个）",
			Why:        "观测侧自行制造的连接压力与业务连接泄漏在状态读数上完全同形",
			NextAction: "先停止/收敛采集器的连接池并复采；连接占比随之下降则确认是观测侧信号",
		})
	}
	if s.capValid && s.capTotals.Data > 0 && float64(s.capTotals.Free)/float64(s.capTotals.Data) >= 0.5 {
		b.Missing = append(b.Missing, evidence.Missing{
			What:       "磁盘使用来源拆分（ibd / binlog / redo / 临时文件）",
			Why:        "DATA_FREE 只说明表空间内碎片，不能解释实例磁盘告警",
			NextAction: "按目录统计 binlog、redo、tmp 占用，与 information_schema 汇总对账",
		})
	}
}

// summarize 生成确定性摘要。根因句只在门控通过时出现。
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

// ---- 门控辅助 --------------------------------------------------------------

func hypothesisStatus(b *evidence.Bundle, support []string) evidence.HypothesisStatus {
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

func hypothesisConfidence(b *evidence.Bundle, support []string) float64 {
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

func signalFloat(e *evidence.Evidence, name string) float64 {
	if e == nil || e.Signal.Name != name {
		return 0
	}
	switch v := e.Signal.Value.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}

func replHasFailure(s *snapshot) bool {
	if s.replica == nil {
		return false
	}
	return !strings.EqualFold(s.replica.IO, "Yes") || !strings.EqualFold(s.replica.SQL, "Yes")
}
