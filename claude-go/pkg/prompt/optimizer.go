// Package prompt implements prompt auto-optimization via historical feedback.
// Phase 3.5: collect (prompt, result, score) and apply simple Bayesian-style optimization.
package prompt

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Optimizer collects historical prompt-result-score triples and suggests
// prompt variants that maximize expected scores.
type Optimizer struct {
	mu       sync.RWMutex
	history  []PromptRecord
	variants map[string][]string // prompt hash -> suggested variants
	dataDir  string
}

// PromptRecord is a single observation: prompt template + final score.
type PromptRecord struct {
	Timestamp   time.Time       `json:"timestamp"`
	PromptHash  string          `json:"prompt_hash"`
	Prompt      string          `json:"prompt"`
	ResultScore float64         `json:"result_score"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	TaskType    string          `json:"task_type"`
}

// NewOptimizer 构造优化器并从 dataDir 载入历史 (JSONL)。
// NewOptimizer creates an optimizer backed by a JSONL history file.
func NewOptimizer(dataDir string) *Optimizer {
	o := &Optimizer{
		variants: make(map[string][]string),
		dataDir:  dataDir,
	}
	_ = o.load()
	return o
}

// Record 追加一条观测 (提示词, 分数, 任务类型) 并立即落盘。
// Record appends a new observation to history.
func (o *Optimizer) Record(prompt string, score float64, taskType string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	rec := PromptRecord{
		Timestamp:   time.Now(),
		PromptHash:  hashPrompt(prompt),
		Prompt:      prompt,
		ResultScore: score,
		TaskType:    taskType,
	}
	o.history = append(o.history, rec)
	_ = o.persist()
}

// Suggest 返回历史上在该任务类型上表现最好的提示词变体; 数据不足时原样返回入参。
//
// 算法解读：
//
//	原理：把"提示词选择"当成基于历史分数的择优问题 —— 同任务类型里取分数最高的那条直接复用;
//	      若它不够好或就是当前这条, 则退而求其次, 用高分变体的**增量行**去增强当前提示词
//	      (mergePrompts)。
//	关键不变量：样本不足时必须原路返回 (历史 <10 条, 或该类型候选 <5 条)。冷启动时任何"择优"
//	      都只是在噪声上做选择, 会让同一提示词在不同运行间随机跳变。
//	代价与边界：每次调用都全量线性扫一遍 history 再排序 (O(n log n)); 历史无上限增长时这里会
//	      退化。它不做 A/B 校验, 只在最高分 ≥0.85 且与当前不同时才整条替换。
//	设计权衡：选"复用整条高分提示词"而不是"做梯度式改写", 因为前者可解释、可回滚; 代价是
//	      探索能力弱 —— 真正的探索/利用权衡在 SelectBestPrompt 的 UCB 版本里。
// Suggest returns a prompt variant that has historically performed well for
// tasks of the given type. If no history exists, returns the original prompt.
func (o *Optimizer) Suggest(prompt string, taskType string) string {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if len(o.history) < 10 {
		return prompt // Not enough data
	}

	// Find top-performing prompts for this task type
	var candidates []PromptRecord
	for _, rec := range o.history {
		if rec.TaskType == taskType || taskType == "" {
			candidates = append(candidates, rec)
		}
	}
	if len(candidates) < 5 {
		return prompt
	}

	// Sort by score descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ResultScore > candidates[j].ResultScore
	})

	// Return the best-performing prompt if it's different and better
	best := candidates[0]
	if best.ResultScore >= 0.85 && best.Prompt != prompt {
		return best.Prompt
	}

	// Otherwise try a simple variant: append a proven suffix from top 10%
	topN := max(1, len(candidates)/10)
	for i := 0; i < topN && i < len(candidates); i++ {
		variant := mergePrompts(prompt, candidates[i].Prompt)
		if variant != prompt {
			return variant
		}
	}

	return prompt
}

// ExpectedScore 返回某提示词哈希在历史中的平均分; 无样本时返回先验 0.5。
// ExpectedScore returns the mean score for a prompt hash across history.
func (o *Optimizer) ExpectedScore(promptHash string) float64 {
	o.mu.RLock()
	defer o.mu.RUnlock()

	var sum, count float64
	for _, rec := range o.history {
		if rec.PromptHash == promptHash {
			sum += rec.ResultScore
			count++
		}
	}
	if count == 0 {
		return 0.5 // Prior
	}
	return sum / count
}

// TopPromptEntry represents a prompt hash with its aggregate score.
type TopPromptEntry struct {
	Hash  string  `json:"hash"`
	Score float64 `json:"score"`
	Count int     `json:"count"`
}

// TopPrompts 按平均分降序返回前 n 个提示词哈希及其样本数。
// TopPrompts returns the top-N performing prompt hashes with their scores.
func (o *Optimizer) TopPrompts(n int) []TopPromptEntry {
	o.mu.RLock()
	defer o.mu.RUnlock()

	scores := make(map[string]struct {
		Sum   float64
		Count int
	})
	for _, rec := range o.history {
		s := scores[rec.PromptHash]
		s.Sum += rec.ResultScore
		s.Count++
		scores[rec.PromptHash] = s
	}

	type entry struct {
		Hash  string
		Score float64
		Count int
	}
	var entries []entry
	for h, s := range scores {
		entries = append(entries, entry{Hash: h, Score: s.Sum / float64(s.Count), Count: s.Count})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Score > entries[j].Score
	})
	if n > len(entries) {
		n = len(entries)
	}
	var out []TopPromptEntry
	for _, e := range entries[:n] {
		out = append(out, TopPromptEntry{Hash: e.Hash, Score: e.Score, Count: e.Count})
	}
	return out
}

// persist 用 O_TRUNC 重写整个 JSONL 历史文件 (全量覆盖, 不是追加)。
// persist writes history to a JSONL file.
func (o *Optimizer) persist() error {
	if o.dataDir == "" {
		return nil
	}
	_ = os.MkdirAll(o.dataDir, 0755)
	path := filepath.Join(o.dataDir, "prompt_history.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, rec := range o.history {
		b, _ := json.Marshal(rec)
		_, _ = f.Write(b)
		_, _ = f.WriteString("\n")
	}
	return nil
}

// load 从 JSONL 逐行读回历史; 文件不存在与坏行都静默跳过。
// load reads history from a JSONL file.
func (o *Optimizer) load() error {
	if o.dataDir == "" {
		return nil
	}
	path := filepath.Join(o.dataDir, "prompt_history.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec PromptRecord
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			o.history = append(o.history, rec)
		}
	}
	return nil
}

// hashPrompt 返回提示词的稳定短哈希 (sha256 前 16 个十六进制字符), 用作聚合键。
// hashPrompt returns a stable hash of a prompt for indexing.
func hashPrompt(prompt string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prompt))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// mergePrompts 把高分提示词里当前没有的**非空行**追加到 base 末尾, 作为"已验证的补充"。
// mergePrompts attempts to merge two prompts by taking the first and appending
// any additional constraint lines from the second.
func mergePrompts(base, highPerf string) string {
	if base == highPerf {
		return base
	}
	baseLines := strings.Split(base, "\n")
	highLines := strings.Split(highPerf, "\n")
	baseSet := make(map[string]bool)
	for _, l := range baseLines {
		baseSet[strings.TrimSpace(l)] = true
	}
	var additions []string
	for _, l := range highLines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" && !baseSet[trimmed] {
			additions = append(additions, l)
		}
	}
	if len(additions) == 0 {
		return base
	}
	return base + "\n\n# Proven additions from high-performing variant:\n" + strings.Join(additions, "\n")
}

// UCBScore 计算 UCB (置信上界) 分数, 用于探索/利用权衡; c 越大越鼓励探索冷门提示词。
//
// 算法解读：
//
//	原理：UCB1 形式 —— 均值代表"利用", 加号右边 c*sqrt(ln(总试验数)/该臂试验数) 代表"探索":
//	      试验次数越少的臂, 上界抬得越高, 于是迟早会被选到。
//	关键不变量：count==0 必须返回 +Inf。这是冷启动的全部机制 —— 未试过的臂若不返回无穷大,
//	      就永远拿不到第一次试验, 探索项形同虚设。
//	代价与边界：totalTrials==0 时 log(0) 为 -Inf, 但 count==0 分支已先返回, 走不到这里;
//	      因此调用方必须先保证 totalTrials>0。
//	设计权衡：用 UCB 而不是 epsilon-greedy —— 前者无需调探索率, 且探索量随试验数自动衰减。
// UCBScore computes an Upper Confidence Bound score for exploration/exploitation.
// Higher c encourages exploration of less-tested prompts.
func UCBScore(mean float64, count int, totalTrials int, c float64) float64 {
	if count == 0 {
		return math.Inf(1)
	}
	return mean + c*math.Sqrt(math.Log(float64(totalTrials))/float64(count))
}

// SelectBestPrompt 在当前提示词与候选变体之间按 UCB 选出得分最高的一条。
// SelectBestPrompt uses UCB to select between the current prompt and a set of
// variants, returning the one with the highest expected UCB score.
func (o *Optimizer) SelectBestPrompt(currentPrompt string, variants []string, taskType string, c float64) string {
	o.mu.RLock()
	defer o.mu.RUnlock()

	total := len(o.history)
	if total == 0 {
		return currentPrompt
	}

	candidates := append([]string{currentPrompt}, variants...)
	bestScore := -math.MaxFloat64
	bestPrompt := currentPrompt

	for _, p := range candidates {
		hash := hashPrompt(p)
		mean := o.expectedScoreLocked(hash, taskType)
		count := o.countLocked(hash, taskType)
		score := UCBScore(mean, count, total, c)
		if score > bestScore {
			bestScore = score
			bestPrompt = p
		}
	}
	return bestPrompt
}

// expectedScoreLocked 在**已持读锁**的前提下按哈希与任务类型算平均分; 无样本返回 0.5。
func (o *Optimizer) expectedScoreLocked(hash, taskType string) float64 {
	var sum, count float64
	for _, rec := range o.history {
		if rec.PromptHash == hash && (taskType == "" || rec.TaskType == taskType) {
			sum += rec.ResultScore
			count++
		}
	}
	if count == 0 {
		return 0.5
	}
	return sum / count
}

// countLocked 在**已持读锁**的前提下统计某提示词哈希的样本数。
func (o *Optimizer) countLocked(hash, taskType string) int {
	var count int
	for _, rec := range o.history {
		if rec.PromptHash == hash && (taskType == "" || rec.TaskType == taskType) {
			count++
		}
	}
	return count
}
