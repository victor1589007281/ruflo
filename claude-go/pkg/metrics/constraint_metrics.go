package metrics

import (
	"sync"
)

// ConstraintMetrics tracks the effectiveness of constraint engineering.
type ConstraintMetrics struct {
	mu sync.RWMutex

	GatePassRate               map[string]float64 `json:"gate_pass_rate"`
	GateFailureReason          map[string]map[string]int `json:"gate_failure_reason"`
	SkillEffectiveness         map[string]float64 `json:"skill_effectiveness"`
	ConstitutionViolation      map[string]int `json:"constitution_violation"`
	ConflictRate               float64 `json:"conflict_rate"`
	MergeSuccessRate           float64 `json:"merge_success_rate"`
	AvgRepairRounds            float64 `json:"avg_repair_rounds"`
	RaceDetectionRate          float64 `json:"race_detection_rate"`
	SecurityFindingRate        float64 `json:"security_finding_rate"`
	PerformanceRegressionRate  float64 `json:"performance_regression_rate"`
	DeliveryStatusDistribution map[string]int `json:"delivery_status_distribution"`

	// internal accumulators for rate calculations
	totalTasks        int
	conflictedTasks   int
	totalMerges       int
	succeededMerges   int
	totalRepairRounds int
	repairCount       int
	totalRaces        int
	detectedRaces     int
	totalSecScans     int
	secFindings       int
	totalPerfChecks   int
	perfRegressions   int
}

// NewConstraintMetrics 构造约束指标集, 预建各张 map (往 nil map 里写会 panic, 所以必须在这里建好)。
func NewConstraintMetrics() *ConstraintMetrics {
	return &ConstraintMetrics{
		GatePassRate:               make(map[string]float64),
		GateFailureReason:          make(map[string]map[string]int),
		SkillEffectiveness:         make(map[string]float64),
		ConstitutionViolation:      make(map[string]int),
		DeliveryStatusDistribution: make(map[string]int),
	}
}

// RecordGateResult 记录一次门禁结果: 通过率走指数滑动平均, 失败按原因分类计数。
//
// 算法解读：
//
//	原理：通过率不是"通过数/总数", 而是对 0/1 序列做指数滑动平均 (α=0.1): 通过 →
//	      rate*0.9 + 0.1, 失败 → rate*0.9。它回答的是"最近这段窗口的通过率", 而不是
//	      全历史平均 —— 约束工程改得好不好, 要看近期曲线的走向。
//	关键不变量：失败也必须推进 rate (乘 0.9)。只更新通过分支的话, 失败对指标完全不可见,
//	      于是"改坏了"在曲线上表现为一条水平线。
//	代价与边界：rate 从 0 起步, 冷启动的头几次结果会被压得很低; 且它不带样本量, 无法从
//	      rate 反推跑过几次 (那要看 GateFailureReason 里的计数)。
//	设计权衡：没有做真正的滑动窗口 (环形缓冲 / 分位数), 因为这里只要一个趋势量; 代价是
//	      看不出抖动幅度, 也分不清"偶发一次失败"与"持续失败"。
func (cm *ConstraintMetrics) RecordGateResult(stage string, passed bool, reason string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// 无论成败都先把内层 map 建好: 失败分支要按原因往里写, 往 nil map 写会 panic。
	if _, ok := cm.GateFailureReason[stage]; !ok {
		cm.GateFailureReason[stage] = make(map[string]int)
	}

	if passed {
		// Exponential moving average for pass rate
		// α=0.1: 单次结果只占一成权重, 曲线上一个点的抖动不会改写趋势。
		cm.GatePassRate[stage] = cm.GatePassRate[stage]*0.9 + 1.0*0.1
	} else {
		cm.GatePassRate[stage] = cm.GatePassRate[stage] * 0.9
		cm.GateFailureReason[stage][reason]++
	}
}

// RecordSkillEffect 记录一次技能效果: 以修复轮数的相对下降衡量改善, 再对结果做滑动平均。
//
// 算法解读：
//
//	原理：单次改善量取 (before-after)/before, 再按 0.7/0.3 与历史值做指数滑动平均 ——
//	      单次实验噪声大, 平滑之后才看得出一个技能是不是真的有效。
//	关键不变量：roundsBefore <= 0 时绝不做除法 (会得到 ±Inf 或 NaN, 污染整条曲线),
//	      直接置 0 并返回。
//	代价与边界：置 0 是**覆盖**而不是跳过 —— 拿不到基准轮数的那一次会把该技能已积累的
//	      历史值抹掉 (注意: 这里语义上更像"无法评估", 而写入的却是"效果为零")。
//	设计权衡：权重 0.3 比门禁的 0.1 更激进, 因为技能实验的样本本就少, 需要更快跟上变化;
//	      代价是更容易被单次异常值带偏。
func (cm *ConstraintMetrics) RecordSkillEffect(skillName string, roundsBefore, roundsAfter float64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// 除零守卫: 没有基准轮数就算不出相对改善。
	if roundsBefore <= 0 {
		cm.SkillEffectiveness[skillName] = 0
		return
	}
	improvement := (roundsBefore - roundsAfter) / roundsBefore
	cm.SkillEffectiveness[skillName] = cm.SkillEffectiveness[skillName]*0.7 + improvement*0.3
}

// RecordConstitutionViolation 记录一次宪法条款违规, 按规则名累加计数。
func (cm *ConstraintMetrics) RecordConstitutionViolation(rule string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.ConstitutionViolation[rule]++
}

// RecordConflict 把本轮的任务数与冲突数累加进累计量, 并重算冲突率 (冲突数/任务数)。
//
// 入参是**增量**不是累计值: 累加语义意味着重复上报同一批数字会被算两遍。
func (cm *ConstraintMetrics) RecordConflict(totalTasks, conflictedTasks int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalTasks += totalTasks
	cm.conflictedTasks += conflictedTasks
	if cm.totalTasks > 0 {
		cm.ConflictRate = float64(cm.conflictedTasks) / float64(cm.totalTasks)
	}
}

// RecordMerge 累加合并总数与成功数, 并重算合并成功率; 入参同样是增量。
func (cm *ConstraintMetrics) RecordMerge(total, succeeded int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalMerges += total
	cm.succeededMerges += succeeded
	if cm.totalMerges > 0 {
		cm.MergeSuccessRate = float64(cm.succeededMerges) / float64(cm.totalMerges)
	}
}

// RecordRepairRounds 累加修复轮数并重算平均修复轮数 (总轮数/样本数)。
func (cm *ConstraintMetrics) RecordRepairRounds(rounds int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalRepairRounds += rounds
	cm.repairCount++
	if cm.repairCount > 0 {
		cm.AvgRepairRounds = float64(cm.totalRepairRounds) / float64(cm.repairCount)
	}
}

// RecordRaceDetection 记录一次竞态检查及是否命中, 并重算检出率。
func (cm *ConstraintMetrics) RecordRaceDetection(found bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalRaces++
	if found {
		cm.detectedRaces++
	}
	if cm.totalRaces > 0 {
		cm.RaceDetectionRate = float64(cm.detectedRaces) / float64(cm.totalRaces)
	}
}

// RecordSecurityFinding 记录一次安全扫描及是否发现, 并重算发现率。
func (cm *ConstraintMetrics) RecordSecurityFinding(found bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalSecScans++
	if found {
		cm.secFindings++
	}
	if cm.totalSecScans > 0 {
		cm.SecurityFindingRate = float64(cm.secFindings) / float64(cm.totalSecScans)
	}
}

// RecordPerformanceRegression 记录一次性能检查及是否回退, 并重算回退率。
func (cm *ConstraintMetrics) RecordPerformanceRegression(regressed bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalPerfChecks++
	if regressed {
		cm.perfRegressions++
	}
	if cm.totalPerfChecks > 0 {
		cm.PerformanceRegressionRate = float64(cm.perfRegressions) / float64(cm.totalPerfChecks)
	}
}

// RecordDeliveryStatus 交付状态分布计数加一 (status 原样作键, 不做归一化)。
func (cm *ConstraintMetrics) RecordDeliveryStatus(status string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.DeliveryStatusDistribution[status]++
}

// Snapshot returns a copy of current metrics.
// 返回指针以避免 sync.RWMutex 的值拷贝（go vet 检查）。
func (cm *ConstraintMetrics) Snapshot() *ConstraintMetrics {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	snap := &ConstraintMetrics{
		GatePassRate:               make(map[string]float64, len(cm.GatePassRate)),
		GateFailureReason:          make(map[string]map[string]int, len(cm.GateFailureReason)),
		SkillEffectiveness:         make(map[string]float64, len(cm.SkillEffectiveness)),
		ConstitutionViolation:      make(map[string]int, len(cm.ConstitutionViolation)),
		ConflictRate:               cm.ConflictRate,
		MergeSuccessRate:           cm.MergeSuccessRate,
		AvgRepairRounds:            cm.AvgRepairRounds,
		RaceDetectionRate:          cm.RaceDetectionRate,
		SecurityFindingRate:        cm.SecurityFindingRate,
		PerformanceRegressionRate:  cm.PerformanceRegressionRate,
		DeliveryStatusDistribution: make(map[string]int, len(cm.DeliveryStatusDistribution)),
	}

	for k, v := range cm.GatePassRate {
		snap.GatePassRate[k] = v
	}
	for k, v := range cm.GateFailureReason {
		inner := make(map[string]int, len(v))
		for ik, iv := range v {
			inner[ik] = iv
		}
		snap.GateFailureReason[k] = inner
	}
	for k, v := range cm.SkillEffectiveness {
		snap.SkillEffectiveness[k] = v
	}
	for k, v := range cm.ConstitutionViolation {
		snap.ConstitutionViolation[k] = v
	}
	for k, v := range cm.DeliveryStatusDistribution {
		snap.DeliveryStatusDistribution[k] = v
	}

	return snap
}
