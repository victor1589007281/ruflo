// Evolution — 自动进化引擎, 从执行经验中学习并持续改进。
//
// 设计参考 (业界顶级方案融合):
//   - EvolveR (2025): 离线自蒸馏 + 在线交互闭环, ExpBase 战略原则库
//   - ERL (2026): 轨迹反思 → 可迁移启发式, 选择性检索注入
//   - Live-Evo (2026): 经验权重动态衰减, 有用经验增强/误导经验淘汰
//   - ruflo v3 ReasoningBank: 短/长期模式 + 质量评分 + 晋升/剪枝/去重
//   - ruflo v3 SONA: 轨迹记录 → 后台挖掘高置信步骤为新模式
//
// 四步进化流水线 (RECORD → DISTILL → RETRIEVE → EVOLVE):
//
//   1. RECORD: 每次 Agent 执行后记录完整轨迹 (输入/输出/错误/耗时/角色)
//   2. DISTILL: LLM 从成功/失败轨迹中提炼战略原则和错误模式
//   3. RETRIEVE: 下次执行前按角色+目标检索相关经验, 注入 Agent prompt
//   4. EVOLVE: 根据使用反馈更新质量分, 晋升/淘汰/去重
//
// 三类经验:
//   - RoleExperience: 角色特定知识 ("architect 设计API时应...")
//   - ErrorPattern: 报错→解决方案 ("遇到X错误时, 用Y方法")
//   - GeneralPrinciple: 跨角色通用原则 ("并行任务注意资源竞争")
//
//	┌──────────────────────────────────────────────────────────┐
//	│ EvolutionEngine                                          │
//	│  RecordTrajectory()  → 记录执行轨迹                      │
//	│  LearnFromTeam()     → LLM 批量提炼经验 (团队完成后)     │
//	│  RetrieveFor()       → BM25 检索相关经验 (执行前)        │
//	│  FormatForPrompt()   → 格式化为 Agent 可用的 prompt 段   │
//	│  RecordFeedback()    → 更新经验质量分 (EMA)              │
//	│  Consolidate()       → 去重/剪枝/晋升 (后台定期)         │
//	└──────────────────────────────────────────────────────────┘
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Experience 经验条目。
type Experience struct {
	ID           string    `json:"id"`
	Category     string    `json:"category"`              // "role", "error", "general"
	Role         string    `json:"role,omitempty"`         // 角色 (category=role 时有值)
	Content      string    `json:"content"`                // 经验/原则文本
	Quality      float64   `json:"quality"`                // 0.0-1.0 (EMA 更新)
	UsageCount   int       `json:"usageCount"`             // 被检索使用的次数
	SuccessCount int       `json:"successCount"`           // 使用后任务成功的次数
	Tags         []string  `json:"tags,omitempty"`         // 标签
	Source       string    `json:"source"`                 // 来源 (team/stage)
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// SuccessRate 成功率。
func (e *Experience) SuccessRate() float64 {
	if e.UsageCount == 0 {
		return 0.5
	}
	return float64(e.SuccessCount) / float64(e.UsageCount)
}

// Trajectory 执行轨迹。
type Trajectory struct {
	ID        string    `json:"id"`
	TeamName  string    `json:"teamName"`
	StageName string    `json:"stageName"`
	Role      string    `json:"role"`
	Objective string    `json:"objective"`
	Input     string    `json:"input"`            // 截断的 prompt
	Output    string    `json:"output"`           // 截断的结果
	Error     string    `json:"error,omitempty"`
	Success   bool      `json:"success"`
	Duration  string    `json:"duration"`
	Timestamp time.Time `json:"timestamp"`
}

// EvolutionEngine 自动进化引擎。
type EvolutionEngine struct {
	experiences  []*Experience
	trajectories []Trajectory
	llm          LLMClient
	dataDir      string
	mu           sync.RWMutex
	nextID       int
}

// NewEvolutionEngine 创建进化引擎。
func NewEvolutionEngine(dataDir string, llm LLMClient) *EvolutionEngine {
	ee := &EvolutionEngine{
		llm:     llm,
		dataDir: dataDir,
	}
	ee.load()
	return ee
}

// --- RECORD: 记录执行轨迹 ---

// RecordTrajectory 记录一条 Agent 执行轨迹。
// 每次 workflow stage / swarm subtask 完成后调用。
func (ee *EvolutionEngine) RecordTrajectory(t Trajectory) {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	if t.ID == "" {
		ee.nextID++
		t.ID = fmt.Sprintf("traj-%d-%d", time.Now().Unix(), ee.nextID)
	}
	if t.Timestamp.IsZero() {
		t.Timestamp = time.Now()
	}
	// 截断防止过大
	if len(t.Input) > 2000 {
		t.Input = t.Input[:2000] + "...(truncated)"
	}
	if len(t.Output) > 3000 {
		t.Output = t.Output[:3000] + "...(truncated)"
	}

	ee.trajectories = append(ee.trajectories, t)

	// 保持轨迹数量在合理范围
	if len(ee.trajectories) > 500 {
		ee.trajectories = ee.trajectories[len(ee.trajectories)-500:]
	}

	ee.persistTrajectories()
}

// --- DISTILL: LLM 提炼经验 ---

// LearnFromTeam 团队执行完成后, 从轨迹中提炼经验。
// 参考: EvolveR 离线自蒸馏 + ruflo v3 SONA runBackgroundLoop
func (ee *EvolutionEngine) LearnFromTeam(ctx context.Context, teamName string) {
	ee.mu.RLock()
	var teamTrajs []Trajectory
	for _, t := range ee.trajectories {
		if t.TeamName == teamName {
			teamTrajs = append(teamTrajs, t)
		}
	}
	ee.mu.RUnlock()

	if len(teamTrajs) == 0 {
		return
	}

	log.Printf("[Evolution] 从团队 %s 的 %d 条轨迹中提炼经验...", teamName, len(teamTrajs))

	// LLM 驱动的经验提炼
	if ee.llm != nil {
		ee.llmDistill(ctx, teamTrajs, teamName)
	} else {
		ee.heuristicDistill(teamTrajs, teamName)
	}

	ee.persistExperiences()
	log.Printf("[Evolution] 经验提炼完成, 当前共 %d 条经验", len(ee.experiences))
}

func (ee *EvolutionEngine) llmDistill(ctx context.Context, trajs []Trajectory, teamName string) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("团队: %s\n\n执行轨迹:\n", teamName))

	for _, t := range trajs {
		status := "✅ 成功"
		if !t.Success {
			status = "❌ 失败"
		}
		sb.WriteString(fmt.Sprintf("\n--- 阶段: %s (角色: %s) [%s] ---\n", t.StageName, t.Role, status))
		sb.WriteString(fmt.Sprintf("目标: %s\n", t.Objective))
		if t.Output != "" {
			output := t.Output
			if len(output) > 1000 {
				output = output[:1000] + "..."
			}
			sb.WriteString(fmt.Sprintf("输出摘要: %s\n", output))
		}
		if t.Error != "" {
			sb.WriteString(fmt.Sprintf("错误: %s\n", t.Error))
		}
		sb.WriteString(fmt.Sprintf("耗时: %s\n", t.Duration))
	}

	sysPrompt := `你是经验提炼专家。分析多Agent团队的执行轨迹，提炼可复用的经验教训。

输出严格JSON数组 (不要解释):
[
  {"category":"role","role":"角色名","content":"该角色的具体经验教训","tags":["关键词"]},
  {"category":"error","role":"相关角色","content":"问题描述→解决方案","tags":["错误类型"]},
  {"category":"general","content":"跨角色的通用原则","tags":["关键词"]}
]

提炼规则:
1. 从成功轨迹提炼"什么做得好、为什么有效" (category=role 或 general)
2. 从失败轨迹提炼"出了什么问题、如何避免/解决" (category=error)
3. 每条经验必须是具体、可操作的 (不要泛泛而谈)
4. 最多提炼10条最有价值的经验
5. content 用中文, 简洁明确 (1-3句话)`

	resp, err := ee.llm.SimpleComplete(ctx, sysPrompt, sb.String())
	if err != nil {
		log.Printf("[Evolution] LLM 提炼失败, 回退到启发式: %v", err)
		ee.heuristicDistill(trajs, teamName)
		return
	}

	resp = strings.TrimSpace(resp)
	if idx := strings.Index(resp, "```"); idx >= 0 {
		resp = resp[idx+3:]
		resp = strings.TrimPrefix(resp, "json")
		if end := strings.Index(resp, "```"); end >= 0 {
			resp = resp[:end]
		}
	}
	resp = strings.TrimSpace(resp)

	var extracted []struct {
		Category string   `json:"category"`
		Role     string   `json:"role"`
		Content  string   `json:"content"`
		Tags     []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(resp), &extracted); err != nil {
		log.Printf("[Evolution] 解析 LLM 结果失败: %v", err)
		ee.heuristicDistill(trajs, teamName)
		return
	}

	ee.mu.Lock()
	defer ee.mu.Unlock()

	for _, ext := range extracted {
		if ext.Content == "" {
			continue
		}

		// 去重: 检查是否已有高度相似的经验
		if ee.isDuplicate(ext.Content) {
			continue
		}

		ee.nextID++
		exp := &Experience{
			ID:        fmt.Sprintf("exp-%d-%d", time.Now().Unix(), ee.nextID),
			Category:  ext.Category,
			Role:      ext.Role,
			Content:   ext.Content,
			Quality:   0.5, // 初始质量
			Tags:      ext.Tags,
			Source:     teamName,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		ee.experiences = append(ee.experiences, exp)
	}
}

// heuristicDistill 启发式经验提炼 (LLM 不可用时的降级方案)。
func (ee *EvolutionEngine) heuristicDistill(trajs []Trajectory, teamName string) {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	for _, t := range trajs {
		if t.Error != "" && !t.Success {
			content := fmt.Sprintf("[%s] 执行目标「%s」时失败: %s",
				t.Role, truncateResult(t.Objective, 100), truncateResult(t.Error, 200))
			if !ee.isDuplicate(content) {
				ee.nextID++
				ee.experiences = append(ee.experiences, &Experience{
					ID:        fmt.Sprintf("exp-%d-%d", time.Now().Unix(), ee.nextID),
					Category:  "error",
					Role:      t.Role,
					Content:   content,
					Quality:   0.4,
					Source:    teamName,
					Tags:      []string{t.Role, "error"},
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				})
			}
		}

		if t.Success && t.Output != "" {
			content := fmt.Sprintf("[%s] 成功完成「%s」, 耗时 %s",
				t.Role, truncateResult(t.Objective, 100), t.Duration)
			if !ee.isDuplicate(content) {
				ee.nextID++
				ee.experiences = append(ee.experiences, &Experience{
					ID:        fmt.Sprintf("exp-%d-%d", time.Now().Unix(), ee.nextID),
					Category:  "role",
					Role:      t.Role,
					Content:   content,
					Quality:   0.5,
					Source:    teamName,
					Tags:      []string{t.Role, "success"},
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				})
			}
		}
	}
}

func (ee *EvolutionEngine) isDuplicate(content string) bool {
	contentLower := strings.ToLower(content)
	for _, existing := range ee.experiences {
		existingLower := strings.ToLower(existing.Content)
		// Jaccard 近似去重 (快速)
		if jaccardSimilarity(contentLower, existingLower) > 0.7 {
			return true
		}
	}
	return false
}

// --- RETRIEVE: 检索相关经验 ---

// RetrieveFor 按角色和目标检索相关经验。
// 使用 BM25 + 角色匹配 + 质量加权。
func (ee *EvolutionEngine) RetrieveFor(role, objective string, topK int) []*Experience {
	if topK <= 0 {
		topK = 5
	}

	ee.mu.RLock()
	defer ee.mu.RUnlock()

	if len(ee.experiences) == 0 {
		return nil
	}

	queryTerms := evolutionTokenize(objective)
	if len(queryTerms) == 0 {
		return nil
	}

	type scored struct {
		exp   *Experience
		score float64
	}
	var candidates []scored

	for _, exp := range ee.experiences {
		if exp.Quality < 0.2 {
			continue
		}

		expTerms := evolutionTokenize(exp.Content + " " + strings.Join(exp.Tags, " "))

		// BM25 简化版
		bm25 := simpleBM25(queryTerms, expTerms)
		if bm25 < 0.01 {
			continue
		}

		// 角色匹配加权
		roleBoost := 1.0
		if role != "" && strings.EqualFold(exp.Role, role) {
			roleBoost = 2.0
		}
		if exp.Category == "general" {
			roleBoost = math.Max(roleBoost, 1.3)
		}

		// 质量加权 (参考 Live-Evo: 有效经验增强)
		qualityWeight := 0.5 + exp.Quality*0.5

		// 新鲜度衰减
		hoursSince := time.Since(exp.UpdatedAt).Hours()
		freshness := 1.0 / (1.0 + hoursSince/720.0) // 30天半衰期

		score := bm25 * roleBoost * qualityWeight * (1.0 + freshness)
		candidates = append(candidates, scored{exp, score})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	if len(candidates) > topK {
		candidates = candidates[:topK]
	}

	result := make([]*Experience, len(candidates))
	for i, c := range candidates {
		result[i] = c.exp
	}
	return result
}

// FormatForPrompt 格式化经验为 Agent 可用的 prompt 段。
func FormatExperiencesForPrompt(experiences []*Experience) string {
	if len(experiences) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n<learned_experiences>\n")
	sb.WriteString("以下是从过往执行中提炼的经验教训, 请参考:\n\n")

	for _, exp := range experiences {
		icon := "💡"
		switch exp.Category {
		case "error":
			icon = "⚠️"
		case "role":
			icon = "🎯"
		}

		sb.WriteString(fmt.Sprintf("%s %s", icon, exp.Content))
		if exp.SuccessRate() > 0.7 {
			sb.WriteString(" (高成功率)")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("</learned_experiences>\n")
	return sb.String()
}

// --- EVOLVE: 反馈 + 质量更新 ---

// RecordFeedback 记录经验使用反馈。
// 使用 EMA (指数移动平均) 更新质量分, 参考 ruflo v3 recordPatternUsage。
func (ee *EvolutionEngine) RecordFeedback(expID string, success bool) {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	for _, exp := range ee.experiences {
		if exp.ID == expID {
			exp.UsageCount++
			if success {
				exp.SuccessCount++
			}
			// EMA 更新 Quality
			alpha := 0.3
			reward := 0.0
			if success {
				reward = 1.0
			}
			exp.Quality = (1-alpha)*exp.Quality + alpha*reward
			exp.UpdatedAt = time.Now()
			break
		}
	}
}

// RecordBatchFeedback 批量更新: 检索到的经验用于了某次执行, 按结果反馈。
func (ee *EvolutionEngine) RecordBatchFeedback(expIDs []string, success bool) {
	for _, id := range expIDs {
		ee.RecordFeedback(id, success)
	}
}

// --- CONSOLIDATE: 去重 + 剪枝 + 晋升 ---

// Consolidate 整理经验库。参考 ruflo v3 ReasoningBank.consolidate()。
// 在后台定期调用 (或团队完成后调用)。
func (ee *EvolutionEngine) Consolidate() {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	if len(ee.experiences) == 0 {
		return
	}

	before := len(ee.experiences)

	// 1. 剪枝: 删除低质量 + 长期未使用的经验
	var kept []*Experience
	for _, exp := range ee.experiences {
		ageDays := time.Since(exp.CreatedAt).Hours() / 24
		if exp.Quality < 0.15 && ageDays > 7 {
			continue // 淘汰
		}
		if exp.UsageCount == 0 && ageDays > 30 {
			continue // 30天未被使用
		}
		kept = append(kept, exp)
	}

	// 2. 去重: 合并高度相似的经验 (保留质量更高的)
	var deduped []*Experience
	merged := make(map[int]bool)

	for i := 0; i < len(kept); i++ {
		if merged[i] {
			continue
		}
		best := kept[i]
		for j := i + 1; j < len(kept); j++ {
			if merged[j] {
				continue
			}
			sim := jaccardSimilarity(
				strings.ToLower(kept[i].Content),
				strings.ToLower(kept[j].Content),
			)
			if sim > 0.6 {
				merged[j] = true
				if kept[j].Quality > best.Quality {
					best = kept[j]
					merged[i] = true
				}
				// 合并使用统计
				best.UsageCount += kept[j].UsageCount
				best.SuccessCount += kept[j].SuccessCount
			}
		}
		deduped = append(deduped, best)
	}

	// 3. 限制总量
	if len(deduped) > 200 {
		sort.Slice(deduped, func(i, j int) bool {
			return deduped[i].Quality > deduped[j].Quality
		})
		deduped = deduped[:200]
	}

	ee.experiences = deduped
	ee.persistExperiences()

	after := len(ee.experiences)
	if before != after {
		log.Printf("[Evolution] 整理: %d → %d 条经验 (剪枝 %d)", before, after, before-after)
	}
}

// --- 持久化 ---

func (ee *EvolutionEngine) persistExperiences() {
	if ee.dataDir == "" {
		return
	}
	os.MkdirAll(ee.dataDir, 0755)
	data, err := json.MarshalIndent(ee.experiences, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(ee.dataDir, "experiences.json"), data, 0644)
}

func (ee *EvolutionEngine) persistTrajectories() {
	if ee.dataDir == "" {
		return
	}
	os.MkdirAll(ee.dataDir, 0755)
	data, err := json.MarshalIndent(ee.trajectories, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(ee.dataDir, "trajectories.json"), data, 0644)
}

func (ee *EvolutionEngine) load() {
	if ee.dataDir == "" {
		return
	}

	if data, err := os.ReadFile(filepath.Join(ee.dataDir, "experiences.json")); err == nil {
		_ = json.Unmarshal(data, &ee.experiences)
	}
	if data, err := os.ReadFile(filepath.Join(ee.dataDir, "trajectories.json")); err == nil {
		_ = json.Unmarshal(data, &ee.trajectories)
	}
}

// Stats 统计信息。
func (ee *EvolutionEngine) Stats() EvolutionStats {
	ee.mu.RLock()
	defer ee.mu.RUnlock()

	stats := EvolutionStats{
		TotalExperiences:  len(ee.experiences),
		TotalTrajectories: len(ee.trajectories),
	}

	for _, exp := range ee.experiences {
		switch exp.Category {
		case "role":
			stats.RoleExperiences++
		case "error":
			stats.ErrorPatterns++
		case "general":
			stats.GeneralPrinciples++
		}
	}

	return stats
}

// EvolutionStats 进化统计。
type EvolutionStats struct {
	TotalExperiences  int `json:"totalExperiences"`
	TotalTrajectories int `json:"totalTrajectories"`
	RoleExperiences   int `json:"roleExperiences"`
	ErrorPatterns     int `json:"errorPatterns"`
	GeneralPrinciples int `json:"generalPrinciples"`
}

// --- 工具函数 ---

func evolutionTokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var current strings.Builder
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 0x4e00 && r <= 0x9fff {
			current.WriteRune(r)
		} else {
			if current.Len() > 1 {
				tokens = append(tokens, current.String())
			}
			current.Reset()
		}
	}
	if current.Len() > 1 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

func simpleBM25(queryTerms, docTerms []string) float64 {
	if len(docTerms) == 0 {
		return 0
	}
	tf := make(map[string]int)
	for _, t := range docTerms {
		tf[t]++
	}

	score := 0.0
	for _, qt := range queryTerms {
		if tf[qt] > 0 {
			score += float64(tf[qt]) / (float64(tf[qt]) + 1.5)
		}
	}
	return score
}

func jaccardSimilarity(a, b string) float64 {
	tokensA := evolutionTokenize(a)
	tokensB := evolutionTokenize(b)
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return 0
	}

	setA := make(map[string]bool)
	for _, t := range tokensA {
		setA[t] = true
	}
	setB := make(map[string]bool)
	for _, t := range tokensB {
		setB[t] = true
	}

	intersection := 0
	for t := range setA {
		if setB[t] {
			intersection++
		}
	}

	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
