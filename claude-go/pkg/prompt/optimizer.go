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

// NewOptimizer creates an optimizer backed by a JSONL history file.
func NewOptimizer(dataDir string) *Optimizer {
	o := &Optimizer{
		variants: make(map[string][]string),
		dataDir:  dataDir,
	}
	_ = o.load()
	return o
}

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

// hashPrompt returns a stable hash of a prompt for indexing.
func hashPrompt(prompt string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prompt))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

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

// UCBScore computes an Upper Confidence Bound score for exploration/exploitation.
// Higher c encourages exploration of less-tested prompts.
func UCBScore(mean float64, count int, totalTrials int, c float64) float64 {
	if count == 0 {
		return math.Inf(1)
	}
	return mean + c*math.Sqrt(math.Log(float64(totalTrials))/float64(count))
}

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

func (o *Optimizer) countLocked(hash, taskType string) int {
	var count int
	for _, rec := range o.history {
		if rec.PromptHash == hash && (taskType == "" || rec.TaskType == taskType) {
			count++
		}
	}
	return count
}
