// SONA（Self-Optimizing Neural Architecture，自优化神经架构）协调器实现（package neural）：
// 将经验组织为轨迹（Trajectory）与模式（Pattern），在轨迹结束时执行类 RETRIEVE/JUDGE/DISTILL/CONSOLIDATE 管线。
//
// # 运行模式（与 sona_modes.go、SetMode 配合）
//
// 支持 real-time、balanced、research、edge、batch 等预设：通过 LearningRate、EWCLambda、缓冲上限等调节
// 学习速度、巩固强度与资源占用。轨迹步上的置信度更新可视为对“模式价值”的在线估计；与 EWC++ 风格项配合，
// 减轻新模式对旧模式置信度的冲击（灾难性遗忘的工程近似）。
//
// # 与 EWC++ 的关系
//
// EndTrajectory 在根据 verdict 调整置信度后，用 FisherDiagonal 近似对角 Fisher 信息，对置信度做二次型拉回，
// 与 pkg/neural/ewc.go 中的弹性巩固思想一致（参数 λ 由 EWCLambda 控制）。
package neural

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// SONAStats 汇总协调器可观测指标：模式数、轨迹数、进行中的轨迹数、平均置信度、信号计数等。
type SONAStats struct {
	TotalPatterns      int
	TotalTrajectories  int
	ActiveTrajectories int
	AvgConfidence      float64
	SignalCount        int
}

// SONACoordinator 实现轨迹采集、裁决、模式蒸馏与巩固，并可选用 PatternStore 做 JSON 持久化。
//
// 字段：mu 读写锁；cfg 超参；mode 当前模式名；signals/sigHead 环形缓冲实时信号；
// trajectories 活跃轨迹；patterns 模式库；store 可选磁盘存储；ewc 巩固器（部分逻辑也在 EndTrajectory 内联）。
type SONACoordinator struct {
	mu           sync.RWMutex
	cfg          SONAConfig
	mode         string
	signals      []Signal
	sigHead      int
	trajectories map[string]*Trajectory
	patterns     []*Pattern
	store        *PatternStore
	ewc          *EWCConsolidator
}

// NewSONACoordinator 构造协调器；patternsPath 为空则跳过磁盘 I/O。
// 若 cfg 中 MaxSignals/MaxTrajectorySize 均未正，则回退 DefaultSONAConfig()。
func NewSONACoordinator(cfg SONAConfig, patternsPath string) *SONACoordinator {
	if cfg.MaxSignals <= 0 && cfg.MaxTrajectorySize <= 0 {
		cfg = DefaultSONAConfig()
	}
	s := &SONACoordinator{
		cfg:          cfg,
		signals:      make([]Signal, max(1, cfg.MaxSignals)),
		trajectories: make(map[string]*Trajectory),
		ewc:          NewEWCConsolidator(cfg.EWCLambda),
	}
	if patternsPath != "" {
		s.store = NewPatternStore(patternsPath)
		if loaded, err := s.store.Load(); err == nil && len(loaded) > 0 {
			s.patterns = loaded
		}
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// RecordSignal 将信号写入环形缓冲（sigHead 递增取模）。用于实时遥测/事件流，O(1) 时间，O(cap) 空间。
func (s *SONACoordinator) RecordSignal(sig Signal) {
	if s == nil {
		return
	}
	if sig.Timestamp.IsZero() {
		sig.Timestamp = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	capN := s.cfg.MaxSignals
	if capN <= 0 {
		capN = 512
	}
	if len(s.signals) != capN {
		s.signals = make([]Signal, capN)
	}
	s.signals[s.sigHead%capN] = sig
	s.sigHead++
}

// BeginTrajectory starts a new trajectory and returns its id.
func (s *SONACoordinator) BeginTrajectory(taskID string) string {
	if s == nil {
		return ""
	}
	id := taskID
	if id == "" {
		id = randomSONAID("traj")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trajectories[id] = &Trajectory{
		ID:        id,
		Steps:     nil,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	return id
}

// RecordStep appends a step, trimming when over max size.
func (s *SONACoordinator) RecordStep(trajectoryID string, step TrajectoryStep) {
	if s == nil {
		return
	}
	if step.Timestamp.IsZero() {
		step.Timestamp = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tr, ok := s.trajectories[trajectoryID]
	if !ok {
		return
	}
	maxN := s.cfg.MaxTrajectorySize
	if maxN <= 0 {
		maxN = 256
	}
	if len(tr.Steps) >= maxN {
		tr.Steps = tr.Steps[1:]
	}
	tr.Steps = append(tr.Steps, step)
	tr.UpdatedAt = time.Now().UTC()
}

// EndTrajectory applies JUDGE/DISTILL/CONSOLIDATE and persists patterns.
func (s *SONACoordinator) EndTrajectory(trajectoryID, verdict string) error {
	if s == nil {
		return nil
	}
	reward := judgeReward(verdict)
	s.mu.Lock()
	tr, ok := s.trajectories[trajectoryID]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	tr.Outcome = verdict
	tr.Reward = reward
	tr.UpdatedAt = time.Now().UTC()

	lr := s.cfg.LearningRate
	if lr <= 0 {
		lr = 0.05
	}
	oldConf := make(map[string]float64)
	for _, st := range tr.Steps {
		if st.Content == "" && len(st.Embedding) == 0 {
			continue
		}
		p := s.findOrCreatePatternLocked(st)
		oldConf[p.ID] = p.Confidence
		p.Confidence += lr * reward * (1 - p.Confidence)
		if p.Confidence < 0 {
			p.Confidence = 0
		}
		if p.Confidence > 1 {
			p.Confidence = 1
		}
		p.UsageCount++
		p.UpdatedAt = time.Now().UTC()
	}

	// CONSOLIDATE: EWC-style anchoring toward pre-distillation confidence
	lam := s.cfg.EWCLambda
	if lam <= 0 {
		lam = 0.4
	}
	for _, p := range s.patterns {
		prev, ok := oldConf[p.ID]
		if !ok {
			continue
		}
		F := FisherDiagonal(p)
		learned := p.Confidence
		adj := learned + lam*F*(prev-learned)*(prev-learned)
		if adj < 0 {
			adj = 0
		}
		if adj > 1 {
			adj = 1
		}
		p.Confidence = adj
	}

	s.mu.Unlock()
	return s.persist()
}

// judgeReward 将离散裁决映射为强化信号，驱动置信度更新方向与幅度。
func judgeReward(verdict string) float64 {
	switch verdict {
	case "success", "succeeded", "ok":
		return 1.0
	case "partial", "partial_success":
		return 0.5
	case "failure", "failed", "error":
		return -0.5
	default:
		return 0
	}
}

func (s *SONACoordinator) findOrCreatePatternLocked(st TrajectoryStep) *Pattern {
	keyContent := st.Content
	if keyContent == "" {
		keyContent = string(st.Type)
	}
	for _, p := range s.patterns {
		if p.Content == keyContent && sameEmbedding(p.Embedding, st.Embedding) {
			return p
		}
	}
	maxP := s.cfg.MaxPatterns
	if maxP > 0 && len(s.patterns) >= maxP {
		sort.Slice(s.patterns, func(i, j int) bool {
			return s.patterns[i].Confidence < s.patterns[j].Confidence
		})
		s.patterns = s.patterns[1:]
	}
	np := &Pattern{
		ID:         randomSONAID("pat"),
		Type:       string(st.Type),
		Embedding:  append([]float32(nil), st.Embedding...),
		Content:    keyContent,
		Confidence: 0.1,
		UsageCount: 0,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Metadata:   mapsCopyString(st.Metadata),
	}
	s.patterns = append(s.patterns, np)
	return np
}

func mapsCopyString(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sameEmbedding(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *SONACoordinator) persist() error {
	if s.store == nil {
		return nil
	}
	s.mu.RLock()
	cp := make([]*Pattern, len(s.patterns))
	copy(cp, s.patterns)
	s.mu.RUnlock()
	return s.store.Save(cp)
}

// FindSimilarPatterns runs cosine similarity over stored embeddings (HNSW-like linear scan).
func (s *SONACoordinator) FindSimilarPatterns(embedding []float32, topK int) []*Pattern {
	if s == nil || len(embedding) == 0 {
		return nil
	}
	if topK <= 0 {
		topK = 8
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	type scored struct {
		p *Pattern
		v float64
	}
	var buf []scored
	for _, p := range s.patterns {
		if len(p.Embedding) != len(embedding) {
			continue
		}
		buf = append(buf, scored{p: p, v: cosine(embedding, p.Embedding)})
	}
	sort.Slice(buf, func(i, j int) bool { return buf[i].v > buf[j].v })
	out := make([]*Pattern, 0, topK)
	for i := 0; i < len(buf) && i < topK; i++ {
		pc := *buf[i].p
		if pc.Embedding != nil {
			pc.Embedding = append([]float32(nil), pc.Embedding...)
		}
		out = append(out, &pc)
	}
	return out
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Save persists patterns to disk.
func (s *SONACoordinator) Save() error {
	return s.persist()
}

// Load reloads patterns from the backing store.
func (s *SONACoordinator) Load() error {
	if s == nil || s.store == nil {
		return nil
	}
	p, err := s.store.Load()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.patterns = p
	s.mu.Unlock()
	return nil
}

// ConsolidatePatterns merges patterns whose embedding cosine similarity meets threshold (or identical content).
// The higher-confidence pattern is kept; usage counts are summed. Returns number of patterns removed.
func (s *SONACoordinator) ConsolidatePatterns(threshold float64) int {
	if s == nil {
		return 0
	}
	if threshold <= 0 || threshold > 1 {
		threshold = 0.92
	}
	s.mu.Lock()
	n := s.consolidatePatternsLocked(threshold)
	s.mu.Unlock()
	_ = s.persist()
	return n
}

func (s *SONACoordinator) consolidatePatternsLocked(threshold float64) int {
	if len(s.patterns) < 2 {
		return 0
	}
	alive := make([]bool, len(s.patterns))
	for i := range alive {
		alive[i] = true
	}
	merged := 0
	for i := 0; i < len(s.patterns); i++ {
		if !alive[i] {
			continue
		}
		pi := s.patterns[i]
		for j := i + 1; j < len(s.patterns); j++ {
			if !alive[j] {
				continue
			}
			pj := s.patterns[j]
			sim := 0.0
			if len(pi.Embedding) > 0 && len(pi.Embedding) == len(pj.Embedding) {
				sim = cosine(pi.Embedding, pj.Embedding)
			} else if pi.Content != "" && pi.Content == pj.Content {
				sim = 1
			}
			if sim < threshold {
				continue
			}
			pi.UsageCount += pj.UsageCount
			if pj.Confidence > pi.Confidence {
				pi.Confidence = pj.Confidence
			}
			pi.UpdatedAt = time.Now().UTC()
			alive[j] = false
			merged++
		}
	}
	if merged == 0 {
		return 0
	}
	out := make([]*Pattern, 0, len(s.patterns)-merged)
	for i, p := range s.patterns {
		if alive[i] {
			out = append(out, p)
		}
	}
	s.patterns = out
	return merged
}

// IngestPattern appends a pattern (e.g. from memory bridge); persists when a store is configured.
func (s *SONACoordinator) IngestPattern(p Pattern) {
	if s == nil {
		return
	}
	pc := new(Pattern)
	*pc = p
	if pc.Embedding != nil {
		pc.Embedding = append([]float32(nil), pc.Embedding...)
	}
	if pc.Metadata != nil {
		md := make(map[string]string, len(pc.Metadata))
		for k, v := range pc.Metadata {
			md[k] = v
		}
		pc.Metadata = md
	}
	if pc.ID == "" {
		pc.ID = randomSONAID("pat")
	}
	now := time.Now().UTC()
	if pc.CreatedAt.IsZero() {
		pc.CreatedAt = now
	}
	pc.UpdatedAt = now
	s.mu.Lock()
	s.patterns = append(s.patterns, pc)
	s.mu.Unlock()
	_ = s.persist()
}

// Patterns returns a defensive copy of patterns.
func (s *SONACoordinator) Patterns() []*Pattern {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Pattern, len(s.patterns))
	for i, p := range s.patterns {
		if p == nil {
			continue
		}
		cp := *p
		if p.Embedding != nil {
			cp.Embedding = append([]float32(nil), p.Embedding...)
		}
		out[i] = &cp
	}
	return out
}

// SetMode selects a preset tuning profile (real-time, balanced, research, edge, batch).
func (s *SONACoordinator) SetMode(mode string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
	base := DefaultSONAConfig()
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "real-time", "realtime":
		s.cfg = base
		s.cfg.LearningRate = 0.08
		s.cfg.EWCLambda = 0.2
		s.cfg.MaxSignals = 1024
	case "balanced", "":
		s.cfg = base
	case "research":
		s.cfg = base
		s.cfg.LearningRate = 0.03
		s.cfg.EWCLambda = 0.55
		s.cfg.MaxTrajectorySize = 512
	case "edge":
		s.cfg = base
		s.cfg.MaxPatterns = 512
		s.cfg.MaxSignals = 256
		s.cfg.MaxTrajectorySize = 128
	case "batch":
		s.cfg = base
		s.cfg.LearningRate = 0.02
		s.cfg.EWCLambda = 0.6
	default:
		s.cfg = base
	}
}

// GetTrajectory returns a defensive copy of a trajectory by id.
func (s *SONACoordinator) GetTrajectory(id string) (*Trajectory, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	tr, ok := s.trajectories[id]
	if !ok || tr == nil {
		return nil, false
	}
	cp := *tr
	cp.Steps = append([]TrajectoryStep(nil), tr.Steps...)
	return &cp, true
}

// GetStats returns aggregate SONA metrics.
func (s *SONACoordinator) GetStats() SONAStats {
	if s == nil {
		return SONAStats{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := SONAStats{
		TotalPatterns:     len(s.patterns),
		TotalTrajectories: len(s.trajectories),
	}
	active := 0
	var confSum float64
	for _, tr := range s.trajectories {
		if tr != nil && tr.Outcome == "" {
			active++
		}
	}
	st.ActiveTrajectories = active
	for _, p := range s.patterns {
		if p != nil {
			confSum += p.Confidence
		}
	}
	if len(s.patterns) > 0 {
		st.AvgConfidence = confSum / float64(len(s.patterns))
	}
	st.SignalCount = s.sigHead
	return st
}

// Cleanup 删除置信度 < 0.05 的模式与已结束轨迹，并持久化。O(P+T)。
func (s *SONACoordinator) Cleanup() {
	if s == nil {
		return
	}
	s.mu.Lock()
	const minConf = 0.05
	var kept []*Pattern
	for _, p := range s.patterns {
		if p != nil && p.Confidence >= minConf {
			kept = append(kept, p)
		}
	}
	s.patterns = kept
	for id, tr := range s.trajectories {
		if tr != nil && tr.Outcome != "" {
			delete(s.trajectories, id)
		}
	}
	s.mu.Unlock()
	_ = s.persist()
}

// randomSONAID 生成带加密随机后缀的 id。O(1)。
func randomSONAID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
