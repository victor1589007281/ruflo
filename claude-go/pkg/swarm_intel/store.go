package swarm_intel

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PheromoneStore 信素持久化层。
// 将信素轨迹持久化到 JSONL 文件，实现跨会话的经验积累。
type PheromoneStore struct {
	dir   string
	mu    sync.Mutex
	index map[string]string // hypothesisID → 所属领域文件
}

// NewPheromoneStore 创建持久化存储。
func NewPheromoneStore(baseDir string) (*PheromoneStore, error) {
	dir := filepath.Join(baseDir, "pheromones")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create pheromone dir: %w", err)
	}
	return &PheromoneStore{dir: dir, index: make(map[string]string)}, nil
}

// Save 将当前信素快照持久化到对应领域文件。
func (ps *PheromoneStore) Save(domain string, trails []*PheromoneTrail) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	fname := ps.domainFile(domain)
	f, err := os.OpenFile(fname, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, t := range trails {
		ps.index[t.HypothesisID] = domain
		if err := enc.Encode(t); err != nil {
			return err
		}
	}
	return nil
}

// Load 从领域文件加载历史信素。
func (ps *PheromoneStore) Load(domain string) ([]*PheromoneTrail, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	fname := ps.domainFile(domain)
	data, err := os.ReadFile(fname)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var trails []*PheromoneTrail
	seen := make(map[string]*PheromoneTrail)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var t PheromoneTrail
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			continue
		}
		seen[t.HypothesisID] = &t
	}
	for _, t := range seen {
		trails = append(trails, t)
	}
	return trails, nil
}

func (ps *PheromoneStore) domainFile(domain string) string {
	safe := strings.ReplaceAll(strings.ToLower(domain), " ", "_")
	if safe == "" {
		safe = "general"
	}
	return filepath.Join(ps.dir, safe+".jsonl")
}

// PredictionHistory 预测历史存储。
type PredictionHistory struct {
	file string
	mu   sync.Mutex
}

// PredictionRecord 一条历史预测记录。
type PredictionRecord struct {
	ID          string            `json:"id"`
	Question    string            `json:"question"`
	Outcomes    []OutcomePrediction `json:"outcomes"`
	Consensus   float64           `json:"consensus"`
	BrierScore  float64           `json:"brier_score"`
	Rounds      int               `json:"rounds"`
	ActualLabel string            `json:"actual_label,omitempty"`
	Resolved    bool              `json:"resolved"`
	CreatedAt   time.Time         `json:"created_at"`
	ResolvedAt  *time.Time        `json:"resolved_at,omitempty"`
}

// NewPredictionHistory 创建预测历史存储。
func NewPredictionHistory(baseDir string) (*PredictionHistory, error) {
	dir := filepath.Join(baseDir, "predictions")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create predictions dir: %w", err)
	}
	return &PredictionHistory{file: filepath.Join(dir, "history.jsonl")}, nil
}

// Append 追加预测记录。
func (ph *PredictionHistory) Append(rec *PredictionRecord) error {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	f, err := os.OpenFile(ph.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rec)
}

// Recent 获取最近 N 条预测。
func (ph *PredictionHistory) Recent(n int) ([]*PredictionRecord, error) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	data, err := os.ReadFile(ph.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var all []*PredictionRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec PredictionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		all = append(all, &rec)
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// ReasoningBank 推理路径库 — 成功的推理可以被复用。
type ReasoningBank struct {
	file string
	mu   sync.Mutex
}

// ReasoningEntry 一条推理路径。
type ReasoningEntry struct {
	Question   string    `json:"question"`
	Reasoning  string    `json:"reasoning"`
	BrierScore float64   `json:"brier_score"`
	Confidence float64   `json:"confidence"`
	CreatedAt  time.Time `json:"created_at"`
}

// NewReasoningBank 创建推理路径库。
func NewReasoningBank(baseDir string) (*ReasoningBank, error) {
	dir := filepath.Join(baseDir, "learning")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create learning dir: %w", err)
	}
	return &ReasoningBank{file: filepath.Join(dir, "reasoning_bank.jsonl")}, nil
}

// Store 存储一条推理路径。
func (rb *ReasoningBank) Store(entry *ReasoningEntry) error {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	f, err := os.OpenFile(rb.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(entry)
}

// FindSimilar 查找与问题最相关的历史推理 (简单关键词匹配)。
// 生产环境建议替换为向量搜索。
func (rb *ReasoningBank) FindSimilar(question string, topK int) ([]*ReasoningEntry, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	data, err := os.ReadFile(rb.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	words := strings.Fields(strings.ToLower(question))
	type scored struct {
		entry *ReasoningEntry
		score float64
	}
	var candidates []scored
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry ReasoningEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		lower := strings.ToLower(entry.Question)
		overlap := 0.0
		for _, w := range words {
			if strings.Contains(lower, w) {
				overlap++
			}
		}
		if len(words) > 0 {
			overlap /= float64(len(words))
		}
		if overlap > 0.3 {
			candidates = append(candidates, scored{entry: &entry, score: overlap})
		}
	}

	// 按相关度排序
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].score > candidates[i].score {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	var result []*ReasoningEntry
	for i, c := range candidates {
		if i >= topK {
			break
		}
		result = append(result, c.entry)
	}
	return result, nil
}

// DTIController Deficit-Triggered Integration 控制器。
// 源自 arXiv:2604.02674 — 当少数 Agent 主导预测时触发强制交叉检查。
type DTIController struct {
	threshold float64 // 触发阈值 (默认 0.7)
}

// NewDTIController 创建 DTI 控制器。
func NewDTIController(threshold float64) *DTIController {
	if threshold <= 0 {
		threshold = 0.7
	}
	return &DTIController{threshold: threshold}
}

// ShouldTrigger 判断是否需要 DTI 强制交叉检查。
// 当预测的 Gini 系数 > 阈值时触发 (意味着少数 Agent 支配了预测)。
func (d *DTIController) ShouldTrigger(predictions []AgentPrediction) bool {
	if len(predictions) < 2 {
		return false
	}

	// 计算置信度的 Gini 系数
	confidences := make([]float64, len(predictions))
	sum := 0.0
	for i, p := range predictions {
		confidences[i] = p.Confidence
		sum += p.Confidence
	}
	if sum == 0 {
		return false
	}

	n := float64(len(confidences))
	var giniSum float64
	for i, ci := range confidences {
		for j, cj := range confidences {
			if i != j {
				giniSum += math.Abs(ci - cj)
			}
		}
	}
	gini := giniSum / (2 * n * sum)
	return gini > d.threshold
}
