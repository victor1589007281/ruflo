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
	teamName string
	entries  []*BoardEntry
	mu       sync.RWMutex
	dataDir  string
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
	}
	bb.load()
	return bb
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
			bb.persistLocked()
			return
		}
	}

	bb.entries = append(bb.entries, &BoardEntry{
		Key: key, Value: value, Author: author,
		Category: category, Timestamp: time.Now(),
	})
	bb.persistLocked()
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

func (bb *Blackboard) persistLocked() {
	if bb.dataDir == "" {
		return
	}
	data, err := json.MarshalIndent(bb.entries, "", "  ")
	if err != nil {
		return
	}
	os.MkdirAll(bb.dataDir, 0755)
	_ = os.WriteFile(filepath.Join(bb.dataDir, "blackboard.json"), data, 0644)
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
