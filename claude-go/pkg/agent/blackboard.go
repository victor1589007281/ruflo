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
	"sync/atomic"
	"time"
)

// Board 通信机制抽象 (design/01 §4.11) — 黑板的单一接口。
//
// **双黑板已归一 (M4)**: 曾经并存的 pkg/orchestrator.Blackboard 随该包一同删除,
// orchestrated 模式改由图引擎的 NodeInput.PrevOutputs 传递上游产出, 跨黑板手工同步
// (旧 workflow_orchestrated.go:133-136 的 NewBlackboard + Write("objective")) 消失。
// 设计稿要"新增"的 Watch 曾只存在于被删的那份实现里, 现已在本文件落地。
//
// 为什么用 Board 而不叫 Blackboard: 同包里 Blackboard 已是结构体名 (本文件默认
// 实现), Go 不允许同名。接口方法名也就地对齐既有实现 (SnapshotForRole /
// HandoffContext 语义), 而不是照抄设计稿里的 Snapshot(role,budget)/Handoff(from,to)
// —— 后者会与既有 Snapshot() 冲突, 逼所有调用方改签名, 违背"不破坏现有调用方"。
//
// 两个实现方向:
//   - *Blackboard (本文件): 文件后端 + debounce 落盘, 生产默认;
//   - 分布式后端 (design/02): 换实现不动调用方。
type Board interface {
	// Put 写入/更新一条黑板条目 (同 key 覆盖)。
	Put(e BoardEntry) error
	// Read 读取指定 key 的值。
	Read(key string) (string, bool)
	// SnapshotForRole 为特定角色生成受预算约束的文本快照 (注入 system prompt)。
	SnapshotForRole(role string, budget int) string
	// Handoff 构建阶段交接上下文 (已完成阶段 → 下一个角色)。
	Handoff(completedStages []string, nextRole string) string
	// Watch 订阅 key 前缀匹配的变更事件 (MetaGPT 消息池风格的发布订阅)。
	// 无消费者/消费过慢时事件被丢弃, 绝不阻塞写入方 (fail-open)。
	Watch(prefix string) <-chan BoardEntry
}

// BoardFuncs 用函数字段把任意黑板实现适配成 Board。
//
// 原始存在理由已消失: 它当初是为了把 pkg/orchestrator.Blackboard 适配进 Board 而**不**
// 把 agent→orchestrator 的依赖钉死 (钉死了反而给 M4 退役添阻)。M4 完成后那份实现已删,
// 于是它眼下只有测试调用方。**保留而不删**的理由有二: ① 它是 design/02 分布式后端
// (以及任何第三方黑板) 唯一的零依赖接入点 —— 换实现不必改调用方, 正是 §4.11 抽象的
// 目的; ② 它是纯适配器 (无状态、6 个分支), 留着的成本近似为零, 而删掉一个导出符号
// 要付兼容代价。若日后确认永久无人接线, 再删。
// 任一字段为 nil 时对应方法退化为零值 (fail-open, 通信降级不该打断交付)。
type BoardFuncs struct {
	PutFn      func(e BoardEntry) error
	ReadFn     func(key string) (string, bool)
	SnapshotFn func(role string, budget int) string
	HandoffFn  func(completedStages []string, nextRole string) string
	WatchFn    func(prefix string) <-chan BoardEntry
}

var _ Board = BoardFuncs{}

func (f BoardFuncs) Put(e BoardEntry) error {
	if f.PutFn == nil {
		return nil
	}
	return f.PutFn(e)
}

func (f BoardFuncs) Read(key string) (string, bool) {
	if f.ReadFn == nil {
		return "", false
	}
	return f.ReadFn(key)
}

func (f BoardFuncs) SnapshotForRole(role string, budget int) string {
	if f.SnapshotFn == nil {
		return ""
	}
	return f.SnapshotFn(role, budget)
}

func (f BoardFuncs) Handoff(completedStages []string, nextRole string) string {
	if f.HandoffFn == nil {
		return ""
	}
	return f.HandoffFn(completedStages, nextRole)
}

func (f BoardFuncs) Watch(prefix string) <-chan BoardEntry {
	if f.WatchFn == nil {
		ch := make(chan BoardEntry) // 永不产出的空通道: 订阅方 range 会一直阻塞在读上, 但不会 panic
		return ch
	}
	return f.WatchFn(prefix)
}

// Blackboard 共享黑板 — Agent 间接通信的中枢。
type Blackboard struct {
	teamName  string
	entries   []*BoardEntry
	mu        sync.RWMutex
	dataDir   string
	dirty     bool // 延迟写标记
	flushOnce sync.Once
	flushCh   chan struct{} // 触发异步持久化

	// watchers 前缀 → 订阅通道 (design/01 §4.11 新增的发布订阅)。
	// 受 mu 保护: 派发在写路径的临界区内做, 但全部是非阻塞 send,
	// 因此不会把慢消费者的延迟传染给写入方。
	watchers map[string][]chan BoardEntry
	// watchDrops 因通道满而丢弃的事件数。参照 tracestore.writeErr 的风格:
	// 降级必须可观测, 但绝不反压写入方 (黑板写入在交付主路径上)。
	watchDrops atomic.Int64
}

var _ Board = (*Blackboard)(nil)

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
		watchers: make(map[string][]chan BoardEntry),
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
	bb.writeLocked(BoardEntry{
		Key: key, Value: value, Author: author,
		Category: category, Timestamp: time.Now(),
	})
}

// Put 实现 Board 接口: 写入一条条目。Timestamp 为零值时取当前时间。
// key 为空直接报错 —— 空 key 会在覆盖查找里匹配到任意未命名条目, 是数据污染。
func (bb *Blackboard) Put(e BoardEntry) error {
	if strings.TrimSpace(e.Key) == "" {
		return fmt.Errorf("blackboard: 条目 key 不能为空")
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.writeLocked(e)
	return nil
}

// writeLocked 写入的唯一实现 (调用方须持有 bb.mu 写锁)。
// 覆盖与新增两条路径都在这里派发 watch 事件, 避免将来漏派发。
func (bb *Blackboard) writeLocked(e BoardEntry) {
	for _, old := range bb.entries {
		if old.Key == e.Key {
			old.Value = e.Value
			old.Author = e.Author
			old.Category = e.Category
			old.Timestamp = e.Timestamp
			bb.markDirty()
			bb.notifyLocked(*old)
			return
		}
	}
	entry := e
	bb.entries = append(bb.entries, &entry)
	bb.markDirty()
	bb.notifyLocked(entry)
}

// notifyLocked 向匹配前缀的订阅者派发变更事件 (调用方须持有 bb.mu 写锁)。
//
// 关键纪律: send 一律带 default 分支。黑板写入在交付主路径上, 若某个订阅者
// (dashboard SSE、飞书播报) 卡住, 绝不允许把整个团队执行拖死 —— 宁可丢事件并
// 计数 (WatchDrops), 事后可观测。
func (bb *Blackboard) notifyLocked(e BoardEntry) {
	if len(bb.watchers) == 0 {
		return
	}
	for prefix, chans := range bb.watchers {
		if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		for _, ch := range chans {
			select {
			case ch <- e:
			default:
				bb.watchDrops.Add(1)
			}
		}
	}
}

// watchBuffer 每个订阅通道的缓冲深度。取 64 是照抄已删除的 pkg/orchestrator/blackboard.go
// 那份实现 —— M4 退役它时订阅方的丢弃行为因此无感知变化。
const watchBuffer = 64

// Watch 订阅 key 前缀匹配的黑板变更 (prefix 为空 = 订阅全部)。
// 通道缓冲 watchBuffer 条, 消费过慢时丢弃新事件而不阻塞写入方。
// 订阅方用完应调用 StopWatch 归还通道, 否则该通道会一直参与派发 (仅浪费一次 select)。
func (bb *Blackboard) Watch(prefix string) <-chan BoardEntry {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	if bb.watchers == nil {
		bb.watchers = make(map[string][]chan BoardEntry)
	}
	ch := make(chan BoardEntry, watchBuffer)
	bb.watchers[prefix] = append(bb.watchers[prefix], ch)
	return ch
}

// StopWatch 取消订阅并关闭通道。
// 先从派发表摘除再 close, 且全程持写锁 —— 保证 notifyLocked 绝不会向已关闭通道 send。
func (bb *Blackboard) StopWatch(prefix string, ch <-chan BoardEntry) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	chans := bb.watchers[prefix]
	for i, c := range chans {
		if (<-chan BoardEntry)(c) != ch {
			continue
		}
		bb.watchers[prefix] = append(chans[:i:i], chans[i+1:]...)
		if len(bb.watchers[prefix]) == 0 {
			delete(bb.watchers, prefix)
		}
		close(c)
		return
	}
}

// WatchDrops 返回因订阅通道满而丢弃的事件数 (可观测的降级证据)。
func (bb *Blackboard) WatchDrops() int64 { return bb.watchDrops.Load() }

// Handoff 实现 Board 接口, 等价于 HandoffContext (保留旧名给现有调用方)。
func (bb *Blackboard) Handoff(completedStages []string, nextRole string) string {
	return bb.HandoffContext(completedStages, nextRole)
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
					sb.WriteString(fmt.Sprintf("**%s** (%s):\n%s\n\nFull artifact/ref: blackboard key `%s`.\n\n", stage, e.Author, truncateResult(e.Value, 800), e.Key))
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
	sb.WriteString("Use the summaries above by default. Query/read the referenced blackboard artifact only when precise details are required.\n")

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
