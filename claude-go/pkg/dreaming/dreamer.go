// Package dreaming 实现 Auto-Dream 内存整理机制。
// 对应 TS: services/autoDream/autoDream.ts + consolidationPrompt.ts + consolidationLock.ts
//
// Dreaming 是一个后台运行的"记忆整理"过程:
//   - 在每次 query 完成后，检查是否满足触发条件
//   - 满足条件时，启动一个后台 goroutine 执行记忆整理
//   - 整理过程: 分析近期对话 → 提取关键模式 → 更新记忆文件
//   - 通过文件锁防止并发整理
//
// 触发条件 (对应 TS: autoDream.ts 中的 gating):
//   - 距上次整理超过 minHours (默认 24h)
//   - 自上次整理后至少 minSessions 个会话完成 (默认 5)
//   - 每 10 分钟最多扫描一次
//
// 整理流程 (对应 TS: consolidationPrompt.ts 的 4 阶段):
//   1. Orient: 读取现有记忆文件，了解当前状态
//   2. Gather: 收集近期对话的关键信息
//   3. Consolidate: 合并、更新、去重记忆条目
//   4. Prune: 清理过时条目，维护索引
//
// 与 TS 的差异:
//   - TS 使用 forked subagent (runForkedAgent) 执行整理
//   - Go 使用独立 goroutine + QueryEngine 实现相同效果
//   - TS 通过 GrowthBook 远程配置参数; Go 通过 DreamConfig 本地配置
package dreaming

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LLMClient LLM API 客户端接口 (解耦 api.Client 依赖)
type LLMClient interface {
	// SimpleComplete 简单文本补全: 发送 prompt, 返回回复文本
	SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// DreamConfig Dreaming 配置
type DreamConfig struct {
	// Enabled 是否启用 Dreaming
	Enabled bool `json:"enabled"`

	// MinHours 距上次整理的最小间隔 (小时)
	// 对应 TS: minHours (默认 24)
	MinHours int `json:"minHours,omitempty"`

	// MinSessions 触发整理所需的最小会话数
	// 对应 TS: minSessions (默认 5)
	MinSessions int `json:"minSessions,omitempty"`

	// MemoryDir 记忆文件存储目录
	// 默认: <cwd>/.claude/memory/
	MemoryDir string `json:"memoryDir,omitempty"`

	// MaxMemoryFiles 最大记忆文件数 (防止无限增长)
	MaxMemoryFiles int `json:"maxMemoryFiles,omitempty"`

	// ConsolidationModel 整理使用的模型 (可选, 空则使用主模型)
	ConsolidationModel string `json:"consolidationModel,omitempty"`
}

// DefaultDreamConfig 返回默认配置
func DefaultDreamConfig() *DreamConfig {
	return &DreamConfig{
		Enabled:        true,
		MinHours:       24,
		MinSessions:    5,
		MaxMemoryFiles: 50,
	}
}

// SessionRecord 已完成会话的摘要记录
type SessionRecord struct {
	ChatID    string    `json:"chatId"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
	Turns     int       `json:"turns"`
	Summary   string    `json:"summary"`
	Topics    []string  `json:"topics"`
}

// Dreamer 自动记忆整理引擎。
// 对应 TS: autoDream.ts 中的 executeAutoDream 函数。
//
// 核心状态:
//   - lastDreamTime: 上次整理完成时间
//   - sessionsSinceDream: 自上次整理以来完成的会话数
//   - dreaming: 是否正在整理 (防止并发)
//   - lockFile: 文件锁路径 (跨进程互斥)
type Dreamer struct {
	config            *DreamConfig
	cwd               string
	lastDreamTime     time.Time
	lastScanTime      time.Time
	sessionsSinceDream atomic.Int64
	dreaming          atomic.Bool
	mu                sync.Mutex
	recentSessions    []SessionRecord
	lockFile          string

	// ConsolidateFn 整理函数 (可注入, 用于测试或自定义整理逻辑)
	// 如果为 nil, 根据 config.ConsolidateMode 选择内置方法
	ConsolidateFn func(ctx context.Context, sessions []SessionRecord, memoryDir string) error

	// APIClient LLM API 客户端 (用于 LLM 模式整理)
	// 通过 SetAPIClient 注入，避免循环依赖
	APIClient LLMClient
}

// NewDreamer 创建 Dreamer 实例
func NewDreamer(config *DreamConfig, cwd string) *Dreamer {
	if config == nil {
		config = DefaultDreamConfig()
	}
	if config.MemoryDir == "" {
		config.MemoryDir = filepath.Join(cwd, ".claude", "memory")
	}
	if config.MinHours <= 0 {
		config.MinHours = 24
	}
	if config.MinSessions <= 0 {
		config.MinSessions = 5
	}
	if config.MaxMemoryFiles <= 0 {
		config.MaxMemoryFiles = 50
	}

	return &Dreamer{
		config:   config,
		cwd:      cwd,
		lockFile: filepath.Join(config.MemoryDir, ".dream-lock"),
	}
}

// SetConsolidateFn 设置自定义整理函数 (配置入口)。
// 如果设置，将替代内置的 local/LLM 整理逻辑。
func (d *Dreamer) SetConsolidateFn(fn func(ctx context.Context, sessions []SessionRecord, memoryDir string) error) {
	d.ConsolidateFn = fn
}

// SetAPIClient 注入 LLM API 客户端 (用于 LLM 模式整理)
func (d *Dreamer) SetAPIClient(client LLMClient) {
	d.APIClient = client
}

// RecordSession 记录一个已完成的会话。
// 在每次 ProcessMessage 完成后调用。
func (d *Dreamer) RecordSession(record SessionRecord) {
	d.mu.Lock()
	d.recentSessions = append(d.recentSessions, record)
	if len(d.recentSessions) > 100 {
		d.recentSessions = d.recentSessions[len(d.recentSessions)-100:]
	}
	d.mu.Unlock()
	d.sessionsSinceDream.Add(1)
}

// AfterQuery 在每次 query 完成后调用，检查是否应触发 Dreaming。
// 对应 TS: stopHooks.ts 中 "if (!toolUseContext.agentId) { void executeAutoDream(...) }"
//
// 检查门控 (从廉价到昂贵):
//  1. 是否启用
//  2. 是否已在整理中
//  3. 距上次整理是否超过 MinHours
//  4. 会话数是否达到 MinSessions
//  5. 距上次扫描是否超过 10 分钟
//  6. 文件锁是否可获取
func (d *Dreamer) AfterQuery(ctx context.Context) {
	if !d.config.Enabled {
		return
	}
	// CAS 防止并发触发: 仅当 dreaming 从 false → true 时通过
	if d.dreaming.Load() {
		return
	}

	now := time.Now()

	// 时间门控
	if !d.lastDreamTime.IsZero() && now.Sub(d.lastDreamTime) < time.Duration(d.config.MinHours)*time.Hour {
		return
	}

	// 会话数门控
	if d.sessionsSinceDream.Load() < int64(d.config.MinSessions) {
		return
	}

	// 扫描节流 (10 分钟一次)
	d.mu.Lock()
	if !d.lastScanTime.IsZero() && now.Sub(d.lastScanTime) < 10*time.Minute {
		d.mu.Unlock()
		return
	}
	d.lastScanTime = now
	d.mu.Unlock()

	// CAS 原子抢占: 多个并发 goroutine 只有一个能成功
	if !d.dreaming.CompareAndSwap(false, true) {
		return
	}

	// 尝试获取文件锁
	if !d.acquireLock() {
		d.dreaming.Store(false)
		return
	}

	go d.executeDream(ctx)
}

// executeDream 执行记忆整理 (在独立 goroutine 中运行)。
// 对应 TS: autoDream.ts 中的 executeAutoDream → runForkedAgent(consolidationPrompt)
func (d *Dreamer) executeDream(ctx context.Context) {
	// dreaming 已由调用方 CAS 设置为 true，这里仅负责清理
	defer d.dreaming.Store(false)
	defer d.releaseLock()

	log.Printf("[Dreaming] 开始记忆整理...")
	start := time.Now()

	d.mu.Lock()
	sessions := make([]SessionRecord, len(d.recentSessions))
	copy(sessions, d.recentSessions)
	d.mu.Unlock()

	if err := os.MkdirAll(d.config.MemoryDir, 0755); err != nil {
		log.Printf("[Dreaming] 创建记忆目录失败: %v", err)
		return
	}

	var err error
	if d.ConsolidateFn != nil {
		err = d.ConsolidateFn(ctx, sessions, d.config.MemoryDir)
	} else if d.APIClient != nil {
		err = d.llmConsolidate(ctx, sessions)
	} else {
		err = d.localConsolidate(ctx, sessions)
	}

	if err != nil {
		log.Printf("[Dreaming] 整理失败: %v", err)
		return
	}

	d.lastDreamTime = time.Now()
	d.sessionsSinceDream.Store(0)
	d.mu.Lock()
	d.recentSessions = nil
	d.mu.Unlock()

	elapsed := time.Since(start)
	log.Printf("[Dreaming] 整理完成 (耗时 %v)", elapsed)
}

// localConsolidate 本地记忆整理 (不依赖 LLM)。
// 实现 consolidationPrompt.ts 的 4 阶段:
//  1. Orient: 读取现有记忆文件
//  2. Gather: 从 session 记录中提取关键信息
//  3. Consolidate: 合并、更新记忆
//  4. Prune: 限制文件数量
func (d *Dreamer) localConsolidate(_ context.Context, sessions []SessionRecord) error {
	memDir := d.config.MemoryDir

	// Phase 1: Orient — 读取现有记忆
	existingMemories, err := d.readExistingMemories(memDir)
	if err != nil {
		log.Printf("[Dreaming] 读取现有记忆失败 (非致命): %v", err)
	}

	// Phase 2: Gather — 提取会话关键信息
	var newEntries []memoryEntry
	for _, sess := range sessions {
		if sess.Summary != "" {
			entry := memoryEntry{
				Timestamp: sess.EndTime,
				ChatID:    sess.ChatID,
				Content:   sess.Summary,
				Topics:    sess.Topics,
			}
			newEntries = append(newEntries, entry)
		}
	}

	if len(newEntries) == 0 && len(existingMemories) == 0 {
		log.Printf("[Dreaming] 无可整理内容，跳过")
		return nil
	}

	// Phase 3: Consolidate — 合并记忆
	consolidated := d.mergeMemories(existingMemories, newEntries)

	// Phase 4: Prune — 限制大小
	if len(consolidated) > d.config.MaxMemoryFiles {
		consolidated = consolidated[len(consolidated)-d.config.MaxMemoryFiles:]
	}

	// 写入整理结果
	return d.writeConsolidated(memDir, consolidated)
}

// memoryEntry 记忆条目
type memoryEntry struct {
	Timestamp time.Time
	ChatID    string
	Content   string
	Topics    []string
}

// readExistingMemories 读取现有记忆文件
func (d *Dreamer) readExistingMemories(dir string) ([]memoryEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var memories []memoryEntry
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") || entry.Name() == "index.md" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		info, _ := entry.Info()
		memories = append(memories, memoryEntry{
			Timestamp: info.ModTime(),
			Content:   string(data),
		})
	}
	return memories, nil
}

// mergeMemories 合并已有记忆和新条目
func (d *Dreamer) mergeMemories(existing, newEntries []memoryEntry) []memoryEntry {
	dedup := make(map[string]bool)
	var result []memoryEntry

	for _, e := range existing {
		key := strings.TrimSpace(e.Content)
		if key != "" && !dedup[key] {
			dedup[key] = true
			result = append(result, e)
		}
	}

	for _, e := range newEntries {
		key := strings.TrimSpace(e.Content)
		if key != "" && !dedup[key] {
			dedup[key] = true
			result = append(result, e)
		}
	}

	return result
}

// writeConsolidated 写入整理后的记忆
func (d *Dreamer) writeConsolidated(dir string, entries []memoryEntry) error {
	// 写入索引文件
	var indexSb strings.Builder
	indexSb.WriteString("# Memory Index\n\n")
	indexSb.WriteString(fmt.Sprintf("Last consolidated: %s\n", time.Now().Format(time.RFC3339)))
	indexSb.WriteString(fmt.Sprintf("Total entries: %d\n\n", len(entries)))

	for i, e := range entries {
		filename := fmt.Sprintf("memory-%03d.md", i+1)
		preview := e.Content
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		indexSb.WriteString(fmt.Sprintf("- [%s](%s)", strings.ReplaceAll(preview, "\n", " "), filename))
		if len(e.Topics) > 0 {
			indexSb.WriteString(fmt.Sprintf(" — %s", strings.Join(e.Topics, ", ")))
		}
		indexSb.WriteString("\n")

		// 写入单条记忆
		content := fmt.Sprintf("<!-- chat: %s, time: %s -->\n%s\n",
			e.ChatID, e.Timestamp.Format(time.RFC3339), e.Content)
		path := filepath.Join(dir, filename)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", filename, err)
		}
	}

	indexPath := filepath.Join(dir, "index.md")
	return os.WriteFile(indexPath, []byte(indexSb.String()), 0644)
}

// acquireLock 获取文件锁 (防止并发整理)。
// 对应 TS: consolidationLock.ts
func (d *Dreamer) acquireLock() bool {
	dir := filepath.Dir(d.lockFile)
	os.MkdirAll(dir, 0755)

	// 检查锁文件是否存在且未过期 (1小时超时)
	if info, err := os.Stat(d.lockFile); err == nil {
		if time.Since(info.ModTime()) < time.Hour {
			return false
		}
		os.Remove(d.lockFile)
	}

	return os.WriteFile(d.lockFile, []byte(fmt.Sprintf("pid:%d,time:%s", os.Getpid(), time.Now().Format(time.RFC3339))), 0644) == nil
}

// releaseLock 释放文件锁
func (d *Dreamer) releaseLock() {
	os.Remove(d.lockFile)
}

// IsDreaming 检查是否正在整理
func (d *Dreamer) IsDreaming() bool {
	return d.dreaming.Load()
}

// Stats 返回 Dreaming 统计信息
func (d *Dreamer) Stats() DreamStats {
	d.mu.Lock()
	recentCount := len(d.recentSessions)
	d.mu.Unlock()

	return DreamStats{
		Enabled:            d.config.Enabled,
		LastDreamTime:      d.lastDreamTime,
		SessionsSinceDream: d.sessionsSinceDream.Load(),
		IsDreaming:         d.dreaming.Load(),
		RecentSessions:     recentCount,
		MemoryDir:          d.config.MemoryDir,
		MinHours:           d.config.MinHours,
		MinSessions:        d.config.MinSessions,
	}
}

// DreamStats Dreaming 统计
type DreamStats struct {
	Enabled            bool      `json:"enabled"`
	LastDreamTime      time.Time `json:"lastDreamTime"`
	SessionsSinceDream int64     `json:"sessionsSinceDream"`
	IsDreaming         bool      `json:"isDreaming"`
	RecentSessions     int       `json:"recentSessions"`
	MemoryDir          string    `json:"memoryDir"`
	MinHours           int       `json:"minHours"`
	MinSessions        int       `json:"minSessions"`
}

// llmConsolidate LLM 驱动的记忆整理。
// 对应 TS: consolidationPrompt.ts — buildConsolidationPrompt 的 4 阶段
//
// 向 LLM 发送整理指令，包含:
//   - 现有记忆文件列表
//   - 近期会话摘要
//   - 4 阶段整理指令 (Orient → Gather → Consolidate → Prune)
//
// LLM 返回整理后的记忆文本，写入 memory/consolidated.md
func (d *Dreamer) llmConsolidate(ctx context.Context, sessions []SessionRecord) error {
	memDir := d.config.MemoryDir

	// 读取现有记忆
	existingContent := ""
	entries, _ := os.ReadDir(memDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(memDir, e.Name()))
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > 2000 {
			content = content[:2000] + "..."
		}
		existingContent += fmt.Sprintf("### %s\n%s\n\n", e.Name(), content)
	}

	// 构建会话摘要
	var sessionsSummary strings.Builder
	for _, s := range sessions {
		sessionsSummary.WriteString(fmt.Sprintf("- [%s] %s", s.ChatID, s.Summary))
		if len(s.Topics) > 0 {
			sessionsSummary.WriteString(fmt.Sprintf(" (topics: %s)", strings.Join(s.Topics, ", ")))
		}
		sessionsSummary.WriteString("\n")
	}

	// 构建整理提示词 (对应 TS: buildConsolidationPrompt 的 4 阶段)
	systemPrompt := `You are a memory consolidation agent. Your job is to organize and consolidate memory files.
Output ONLY the consolidated memory content in markdown format. No explanations or meta-commentary.`

	userPrompt := fmt.Sprintf(`# Dream: Memory Consolidation

## Existing Memories
%s

## Recent Sessions
%s

## Instructions

Execute these 4 phases:

### Phase 1 — Orient
Review the existing memories above. Understand what's already stored.

### Phase 2 — Gather  
From the recent sessions, identify key facts worth remembering:
- Important decisions made
- Files modified and why
- User preferences learned
- Technical patterns discovered
- Errors encountered and solutions found

### Phase 3 — Consolidate
Merge new information into existing memories:
- Update outdated entries
- Remove contradictions (newer info wins)
- Group related items by topic
- Use absolute dates, not relative ("2026-04-08", not "today")

### Phase 4 — Prune
- Remove duplicates
- Remove trivially obvious information
- Keep entries concise (1-2 sentences each)
- Maximum 50 entries total

Output the consolidated memory as a clean markdown document with topic headers.`,
		existingContent, sessionsSummary.String())

	result, err := d.APIClient.SimpleComplete(ctx, systemPrompt, userPrompt)
	if err != nil {
		log.Printf("[Dreaming/LLM] API 调用失败, 回退到本地整理: %v", err)
		return d.localConsolidate(ctx, sessions)
	}

	// 写入整理结果
	consolidated := filepath.Join(memDir, "consolidated.md")
	header := fmt.Sprintf("<!-- Auto-consolidated: %s -->\n", time.Now().Format(time.RFC3339))
	if err := os.WriteFile(consolidated, []byte(header+result), 0644); err != nil {
		return fmt.Errorf("写入整理结果失败: %w", err)
	}

	// 更新索引
	indexContent := fmt.Sprintf("# Memory Index\n\nLast consolidated: %s (LLM mode)\nSessions processed: %d\n\nSee [consolidated.md](consolidated.md) for full content.\n",
		time.Now().Format(time.RFC3339), len(sessions))
	return os.WriteFile(filepath.Join(memDir, "index.md"), []byte(indexContent), 0644)
}

// ForceDream 强制触发一次记忆整理 (忽略门控条件)
func (d *Dreamer) ForceDream(ctx context.Context) error {
	if !d.dreaming.CompareAndSwap(false, true) {
		return fmt.Errorf("已在整理中")
	}
	if !d.acquireLock() {
		d.dreaming.Store(false)
		return fmt.Errorf("无法获取锁（可能其他进程正在整理）")
	}
	go d.executeDream(ctx)
	return nil
}
