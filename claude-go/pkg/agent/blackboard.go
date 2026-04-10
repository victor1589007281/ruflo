// Blackboard — 共享黑板架构 (bMAS: Blackboard Multi-Agent System)。
//
// 参考论文: "bMAS: Blackboard LLM Multi-Agent System" (2025)
// 核心思想: 所有 Agent 通过共享黑板进行间接通信,
// 消除私有状态重复, 实现动态 Agent 选择和知识汇聚。
// 比静态多 Agent 系统在常识推理和数学任务上提升 5.02%, 同时消耗更少 token。
//
// 黑板分类:
//   - context:  任务上下文和约束条件
//   - decision: 关键决策和理由
//   - artifact: 代码/文档等产出物
//   - result:   阶段执行结果
//   - progress: 进度更新
//
// 每个 ProductionTeam 持有一个 Blackboard 实例, 文件持久化。
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Blackboard 共享黑板 — Agent 间接通信的中枢。
type Blackboard struct {
	teamName  string
	entries   []*BoardEntry
	mu        sync.RWMutex
	dataDir   string
	dirty     bool          // 延迟写标记
	flushOnce sync.Once
	flushCh   chan struct{} // 触发异步持久化
}

// BoardEntry 黑板上的一条记录。
type BoardEntry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Author    string    `json:"author"`
	Category  string    `json:"category"`
	Timestamp time.Time `json:"timestamp"`
}

// NewBlackboard 创建黑板实例, 从磁盘恢复已有数据。
func NewBlackboard(teamName, dataDir string) *Blackboard {
	bb := &Blackboard{
		teamName: teamName,
		entries:  make([]*BoardEntry, 0),
		dataDir:  dataDir,
		flushCh:  make(chan struct{}, 1),
	}
	bb.load()
	bb.startFlusher()
	return bb
}

// startFlusher 启动异步持久化协程 — debounce 合并高频写入。
func (bb *Blackboard) startFlusher() {
	bb.flushOnce.Do(func() {
		go func() {
			for range bb.flushCh {
				time.Sleep(500 * time.Millisecond)
				bb.mu.RLock()
				if !bb.dirty {
					bb.mu.RUnlock()
					continue
				}
				data, err := json.MarshalIndent(bb.entries, "", "  ")
				bb.mu.RUnlock()
				if err != nil {
					continue
				}
				bb.mu.Lock()
				bb.dirty = false
				bb.mu.Unlock()
				if bb.dataDir != "" {
					os.MkdirAll(bb.dataDir, 0755)
					_ = os.WriteFile(filepath.Join(bb.dataDir, "blackboard.json"), data, 0644)
				}
			}
		}()
	})
}

// Write 向黑板写入/更新条目。相同 key 会覆盖。
func (bb *Blackboard) Write(key, value, author, category string) {
	bb.mu.Lock()
	defer bb.mu.Unlock()

	for _, e := range bb.entries {
		if e.Key == key {
			e.Value = value
			e.Author = author
			e.Category = category
			e.Timestamp = time.Now()
			bb.markDirty()
			return
		}
	}

	bb.entries = append(bb.entries, &BoardEntry{
		Key: key, Value: value, Author: author,
		Category: category, Timestamp: time.Now(),
	})
	bb.markDirty()
}

// Read 读取指定 key 的值。
func (bb *Blackboard) Read(key string) (string, bool) {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	for _, e := range bb.entries {
		if e.Key == key {
			return e.Value, true
		}
	}
	return "", false
}

// ReadByCategory 按分类读取所有条目。
func (bb *Blackboard) ReadByCategory(category string) []*BoardEntry {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	var result []*BoardEntry
	for _, e := range bb.entries {
		if e.Category == category {
			result = append(result, e)
		}
	}
	return result
}

// Snapshot 生成黑板的文本快照, 用于注入 Agent 的 system prompt。
// 这是 bMAS 的核心机制: Agent 在执行前阅读完整黑板。
func (bb *Blackboard) Snapshot() string {
	bb.mu.RLock()
	defer bb.mu.RUnlock()

	if len(bb.entries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Shared Blackboard (Team Knowledge Base)\n\n")

	categories := []string{"context", "decision", "artifact", "result", "progress"}
	for _, cat := range categories {
		var catEntries []*BoardEntry
		for _, e := range bb.entries {
			if e.Category == cat {
				catEntries = append(catEntries, e)
			}
		}
		if len(catEntries) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("### %s\n", capitalizeFirst(cat)))
		for _, e := range catEntries {
			val := e.Value
			if len(val) > 2000 {
				val = val[:2000] + "...(truncated)"
			}
			sb.WriteString(fmt.Sprintf("**[%s]** (by %s):\n%s\n\n", e.Key, e.Author, val))
		}
	}

	return sb.String()
}

// SnapshotForRole 为特定角色生成精简快照。
// 对 result 类条目做智能截断: 保留结构化摘要 + 关键代码块。
func (bb *Blackboard) SnapshotForRole(role string, maxSize int) string {
	bb.mu.RLock()
	defer bb.mu.RUnlock()

	if len(bb.entries) == 0 {
		return ""
	}
	if maxSize <= 0 {
		maxSize = 4000
	}

	var sb strings.Builder
	sb.WriteString("## Shared Blackboard\n\n")

	budget := maxSize
	// context 和 decision 优先给全量
	for _, cat := range []string{"context", "decision"} {
		for _, e := range bb.entries {
			if e.Category != cat {
				continue
			}
			chunk := fmt.Sprintf("**[%s]** (%s): %s\n", e.Key, e.Author, smartTruncate(e.Value, 500))
			if budget-len(chunk) < 0 {
				break
			}
			sb.WriteString(chunk)
			budget -= len(chunk)
		}
	}

	// result/artifact: 提取关键部分 (代码块 + 首尾段落)
	for _, e := range bb.entries {
		if e.Category != "result" && e.Category != "artifact" {
			continue
		}
		if budget < 200 {
			sb.WriteString("\n...(更多结果已省略, 请使用黑板 key 读取)\n")
			break
		}
		perEntry := budget / 2
		if perEntry > 1500 {
			perEntry = 1500
		}
		chunk := fmt.Sprintf("\n**[%s]** (%s):\n%s\n", e.Key, e.Author, extractKeyContent(e.Value, perEntry))
		sb.WriteString(chunk)
		budget -= len(chunk)
	}

	return sb.String()
}

// extractKeyContent 从长文本中提取关键内容: 保留代码块 + 首尾段落。
func extractKeyContent(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}

	var parts []string
	remaining := maxLen

	// 提取代码块
	codeBlocks := extractCodeBlocks(text)
	for _, cb := range codeBlocks {
		if remaining < 100 {
			break
		}
		truncated := smartTruncate(cb, remaining/2)
		parts = append(parts, truncated)
		remaining -= len(truncated)
	}

	// 提取首段 (通常含摘要)
	firstPara := extractFirstParagraph(text)
	if firstPara != "" && remaining > 100 {
		truncated := smartTruncate(firstPara, remaining/2)
		parts = append([]string{truncated}, parts...)
		remaining -= len(truncated)
	}

	if len(parts) == 0 {
		return text[:maxLen] + "..."
	}
	return strings.Join(parts, "\n\n---\n\n")
}

func extractCodeBlocks(text string) []string {
	var blocks []string
	rest := text
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			break
		}
		end := strings.Index(rest[start+3:], "```")
		if end < 0 {
			break
		}
		block := rest[start : start+3+end+3]
		if len(block) > 50 {
			blocks = append(blocks, block)
		}
		rest = rest[start+3+end+3:]
	}
	return blocks
}

func extractFirstParagraph(text string) string {
	idx := strings.Index(text, "\n\n")
	if idx > 0 && idx < 500 {
		return text[:idx]
	}
	if len(text) > 300 {
		return text[:300]
	}
	return text
}

func smartTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - 20
	if cut < 0 {
		cut = max
	}
	// 尝试在换行处截断
	if idx := strings.LastIndex(s[:cut], "\n"); idx > cut/2 {
		return s[:idx] + "\n...(truncated)"
	}
	return s[:cut] + "...(truncated)"
}

// HandoffContext 构建结构化的阶段交接上下文。
// 参考 Anthropic "Harness Design for Long-Running Apps" (2026):
// 在上下文重置之间传递状态, 解决 "context anxiety"。
func (bb *Blackboard) HandoffContext(completedStages []string, nextRole string) string {
	bb.mu.RLock()
	defer bb.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("## Handoff Context\n\n")

	// 已完成的阶段结果
	if len(completedStages) > 0 {
		sb.WriteString("### Completed Work\n")
		for _, stage := range completedStages {
			for _, e := range bb.entries {
				if e.Key == stage+"-result" {
					sb.WriteString(fmt.Sprintf("**%s** (%s):\n%s\n\n", stage, e.Author, truncateResult(e.Value, 1500)))
				}
			}
		}
	}

	// 关键决策
	decisions := bb.filterLocked("decision")
	if len(decisions) > 0 {
		sb.WriteString("### Key Decisions\n")
		for _, d := range decisions {
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", d.Key, d.Value))
		}
		sb.WriteString("\n")
	}

	// 对下一个角色的指引
	sb.WriteString(fmt.Sprintf("### Your Role: %s\n", nextRole))
	sb.WriteString("Read the above context carefully and build upon previous work.\n")

	return sb.String()
}

func (bb *Blackboard) filterLocked(category string) []*BoardEntry {
	var result []*BoardEntry
	for _, e := range bb.entries {
		if e.Category == category {
			result = append(result, e)
		}
	}
	return result
}

// markDirty 标记需要持久化，通知 flusher 协程（非阻塞）。
func (bb *Blackboard) markDirty() {
	bb.dirty = true
	select {
	case bb.flushCh <- struct{}{}:
	default:
	}
}

// Flush 强制同步持久化（关闭前调用）。
func (bb *Blackboard) Flush() {
	bb.mu.RLock()
	data, err := json.MarshalIndent(bb.entries, "", "  ")
	bb.mu.RUnlock()
	if err != nil || bb.dataDir == "" {
		return
	}
	os.MkdirAll(bb.dataDir, 0755)
	_ = os.WriteFile(filepath.Join(bb.dataDir, "blackboard.json"), data, 0644)
	bb.mu.Lock()
	bb.dirty = false
	bb.mu.Unlock()
}

func (bb *Blackboard) load() {
	if bb.dataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(bb.dataDir, "blackboard.json"))
	if err != nil {
		return
	}
	var entries []*BoardEntry
	if json.Unmarshal(data, &entries) == nil {
		bb.entries = entries
	}
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
