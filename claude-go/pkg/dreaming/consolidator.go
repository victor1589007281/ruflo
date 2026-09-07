// Package dreaming — consolidator: 增强的记忆整合引擎。
//
// 借鉴:
//   - Google Always-On Memory Agent: 定时整合 + 关联发现
//   - MemoryOS: 分层存储升降级
//   - openclaw-memory-final: 每日蒸馏 + 每周整合
//   - Ebbinghaus: 分类衰减 + 间隔重复
package dreaming

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/memory"
)

// Consolidator 增强的记忆整合器
type Consolidator struct {
	factStore *memory.FactStore
	llm       LLMClient
	config    *ConsolidatorConfig
}

// ConsolidatorConfig 整合器配置
type ConsolidatorConfig struct {
	SemanticDedup    bool          // 语义去重 (依赖 LLM)
	PatternDetection bool          // 模式识别 (依赖 LLM)
	ConflictDetection bool         // 矛盾检测
	MaxFactsPerCycle int           // 每次整合最大处理事实数
	ConsolidateInterval time.Duration // 整合间隔
}

// DefaultConsolidatorConfig 默认配置
func DefaultConsolidatorConfig() *ConsolidatorConfig {
	return &ConsolidatorConfig{
		SemanticDedup:       false, // 不依赖 LLM 时关闭
		PatternDetection:    false,
		ConflictDetection:   true,
		MaxFactsPerCycle:    200,
		ConsolidateInterval: 12 * time.Hour,
	}
}

// NewConsolidator 创建整合器
func NewConsolidator(factStore *memory.FactStore, llm LLMClient, config *ConsolidatorConfig) *Consolidator {
	if config == nil {
		config = DefaultConsolidatorConfig()
	}
	if llm != nil {
		config.SemanticDedup = true
		config.PatternDetection = true
	}
	return &Consolidator{
		factStore: factStore,
		llm:       llm,
		config:    config,
	}
}

// DistillationResult 蒸馏结果
type DistillationResult struct {
	NewFacts       int
	MergedFacts    int
	Contradictions int
	PatternsFound  int
	// 13.8.6 P1 更新门裁决分布: Superseded=裁决(旧条目归档), GateKept=低置信矛盾只追加
	Superseded int
	GateKept   int
	Duration   time.Duration
}

// IncrementalDistill 增量蒸馏: 从 SessionRecord 提取事实并整合进 FactStore
func (c *Consolidator) IncrementalDistill(ctx context.Context, sessions []SessionRecord) (*DistillationResult, error) {
	if c.factStore == nil {
		return nil, fmt.Errorf("factStore is nil")
	}
	start := time.Now()
	result := &DistillationResult{}

	for _, sess := range sessions {
		if sess.Summary == "" {
			continue
		}
		facts := c.extractFactsFromSession(ctx, sess)
		for _, f := range facts {
			// 13.8.6 P1 记忆更新门: 低置信矛盾事实只追加不覆盖, 冲突需反思级
			// 置信 (Importance >= memory.ReflectThreshold) 才裁决旧条目。
			// dream:pattern 事实 (0.8 置信的跨事实模式) 走老 Add —— 它是新归纳
			// 而非对既有事实的更正。
			if strings.HasPrefix(f.Source, "dream:pattern") {
				c.factStore.Add(f)
				result.NewFacts++
				continue
			}
			switch c.factStore.AddWithGate(f) {
			case memory.GateSuperseded:
				result.Superseded++
			case memory.GateKeptBoth:
				result.GateKept++
			}
			result.NewFacts++
		}
	}

	// 矛盾检测
	if c.config.ConflictDetection {
		contradictions := c.factStore.DetectContradictions()
		result.Contradictions = len(contradictions)
		for _, conn := range contradictions {
			c.factStore.AddConnection(conn.FactIDA, conn.FactIDB, conn.Relation, conn.Strength)
		}
	}

	// 模式识别 (LLM)
	if c.config.PatternDetection && c.llm != nil {
		patterns := c.detectPatterns(ctx)
		result.PatternsFound = len(patterns)
		for _, p := range patterns {
			c.factStore.Add(p)
		}
	}

	result.Duration = time.Since(start)

	c.factStore.LogConsolidation(memory.ConsolidationEntry{
		ID:             fmt.Sprintf("distill-%s", time.Now().Format("20060102-150405")),
		CycleDate:      time.Now(),
		FactsInput:     len(sessions),
		FactsMerged:    result.MergedFacts,
		PatternsFound:  result.PatternsFound,
		Contradictions: result.Contradictions,
		DurationMs:     result.Duration.Milliseconds(),
	})

	c.factStore.PersistToDisk()

	log.Printf("[Consolidator] 蒸馏完成: +%d 事实, %d 矛盾, %d 模式 (%v)",
		result.NewFacts, result.Contradictions, result.PatternsFound, result.Duration.Round(time.Millisecond))

	return result, nil
}

// extractFactsFromSession 从会话记录提取记忆事实
func (c *Consolidator) extractFactsFromSession(ctx context.Context, sess SessionRecord) []*memory.MemoryFact {
	var facts []*memory.MemoryFact

	// LLM 提取 (如果可用)
	if c.llm != nil {
		llmFacts := c.llmExtractFacts(ctx, sess)
		facts = append(facts, llmFacts...)
	}

	// 启发式补充
	if len(facts) == 0 {
		facts = c.heuristicExtractFacts(sess)
	}

	return facts
}

// llmExtractFacts 使用 LLM 提取关键事实
func (c *Consolidator) llmExtractFacts(ctx context.Context, sess SessionRecord) []*memory.MemoryFact {
	sysPrompt := `你是记忆提取专家。从对话摘要中提取值得记忆的关键事实。
每行一个事实，格式: [类别] 事实内容
类别: fact(技术事实)/preference(偏好)/goal(目标)/event(事件)/context(上下文)
最多5条，简洁准确。`

	userPrompt := fmt.Sprintf("对话摘要:\n%s\n\n主题: %s\n来源: %s",
		sess.Summary, strings.Join(sess.Topics, ", "), sess.Source)

	resp, err := c.llm.SimpleComplete(ctx, sysPrompt, userPrompt)
	if err != nil {
		return nil
	}

	var facts []*memory.MemoryFact
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cat, content := parseCategoryLine(line)
		if content == "" {
			continue
		}
		fact := memory.NewMemoryFact(
			"",
			content,
			cat,
			sess.Importance,
			"dream:"+sess.Source,
			sess.Topics,
		)
		facts = append(facts, fact)
	}
	return facts
}

// heuristicExtractFacts 启发式提取
func (c *Consolidator) heuristicExtractFacts(sess SessionRecord) []*memory.MemoryFact {
	if len(sess.Summary) < 20 {
		return nil
	}

	// 将整个摘要作为一个事实
	cat := classifyContent(sess.Summary)
	fact := memory.NewMemoryFact(
		"",
		sess.Summary,
		cat,
		sess.Importance,
		"dream:"+sess.Source,
		sess.Topics,
	)
	return []*memory.MemoryFact{fact}
}

// detectPatterns 使用 LLM 发现跨事实的行为模式
func (c *Consolidator) detectPatterns(ctx context.Context) []*memory.MemoryFact {
	allFacts := c.factStore.GetAll()
	if len(allFacts) < 5 {
		return nil
	}

	// 取最近 30 条事实
	recent := allFacts
	if len(recent) > 30 {
		recent = recent[len(recent)-30:]
	}

	var sb strings.Builder
	for _, f := range recent {
		sb.WriteString(fmt.Sprintf("- [%s] %s\n", f.Category, truncStr(f.Content, 150)))
	}

	sysPrompt := `分析以下记忆事实列表，识别重复出现的行为模式或规律。
每行一个模式，格式: [pattern] 模式描述
最多3个模式，要求有足够证据支持(至少2条相关事实)。`

	resp, err := c.llm.SimpleComplete(ctx, sysPrompt, sb.String())
	if err != nil {
		return nil
	}

	var patterns []*memory.MemoryFact
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "pattern") {
			continue
		}
		content := strings.TrimPrefix(line, "[pattern]")
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		fact := memory.NewMemoryFact(
			"",
			"[模式] "+content,
			memory.CategoryFact,
			0.8,
			"dream:pattern",
			[]string{"_pattern"},
		)
		patterns = append(patterns, fact)
	}
	return patterns
}

// --- 辅助函数 ---

func parseCategoryLine(line string) (memory.MemoryCategory, string) {
	catMap := map[string]memory.MemoryCategory{
		"[fact]":       memory.CategoryFact,
		"[preference]": memory.CategoryPreference,
		"[goal]":       memory.CategoryGoal,
		"[event]":      memory.CategoryEvent,
		"[context]":    memory.CategoryContext,
	}
	lower := strings.ToLower(line)
	for prefix, cat := range catMap {
		if strings.HasPrefix(lower, prefix) {
			content := strings.TrimSpace(line[len(prefix):])
			return cat, content
		}
	}
	return memory.CategoryContext, line
}

func classifyContent(content string) memory.MemoryCategory {
	lower := strings.ToLower(content)
	factSignals := []string{"func ", "type ", "api", "架构", "决策", "interface"}
	for _, s := range factSignals {
		if strings.Contains(lower, s) {
			return memory.CategoryFact
		}
	}
	eventSignals := []string{"修复", "部署", "完成", "发布", "fixed", "deploy"}
	for _, s := range eventSignals {
		if strings.Contains(lower, s) {
			return memory.CategoryEvent
		}
	}
	return memory.CategoryContext
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
