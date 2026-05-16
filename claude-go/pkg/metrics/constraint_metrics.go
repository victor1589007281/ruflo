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

func NewConstraintMetrics() *ConstraintMetrics {
	return &ConstraintMetrics{
		GatePassRate:               make(map[string]float64),
		GateFailureReason:          make(map[string]map[string]int),
		SkillEffectiveness:         make(map[string]float64),
		ConstitutionViolation:      make(map[string]int),
		DeliveryStatusDistribution: make(map[string]int),
	}
}

func (cm *ConstraintMetrics) RecordGateResult(stage string, passed bool, reason string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if _, ok := cm.GateFailureReason[stage]; !ok {
		cm.GateFailureReason[stage] = make(map[string]int)
	}

	if passed {
		// Exponential moving average for pass rate
		cm.GatePassRate[stage] = cm.GatePassRate[stage]*0.9 + 1.0*0.1
	} else {
		cm.GatePassRate[stage] = cm.GatePassRate[stage] * 0.9
		cm.GateFailureReason[stage][reason]++
	}
}

func (cm *ConstraintMetrics) RecordSkillEffect(skillName string, roundsBefore, roundsAfter float64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if roundsBefore <= 0 {
		cm.SkillEffectiveness[skillName] = 0
		return
	}
	improvement := (roundsBefore - roundsAfter) / roundsBefore
	cm.SkillEffectiveness[skillName] = cm.SkillEffectiveness[skillName]*0.7 + improvement*0.3
}

func (cm *ConstraintMetrics) RecordConstitutionViolation(rule string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.ConstitutionViolation[rule]++
}

func (cm *ConstraintMetrics) RecordConflict(totalTasks, conflictedTasks int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalTasks += totalTasks
	cm.conflictedTasks += conflictedTasks
	if cm.totalTasks > 0 {
		cm.ConflictRate = float64(cm.conflictedTasks) / float64(cm.totalTasks)
	}
}

func (cm *ConstraintMetrics) RecordMerge(total, succeeded int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalMerges += total
	cm.succeededMerges += succeeded
	if cm.totalMerges > 0 {
		cm.MergeSuccessRate = float64(cm.succeededMerges) / float64(cm.totalMerges)
	}
}

func (cm *ConstraintMetrics) RecordRepairRounds(rounds int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.totalRepairRounds += rounds
	cm.repairCount++
	if cm.repairCount > 0 {
		cm.AvgRepairRounds = float64(cm.totalRepairRounds) / float64(cm.repairCount)
	}
}

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
