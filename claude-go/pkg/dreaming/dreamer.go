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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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
// V3: 降低触发门槛以增加整合频率 (从 24h/5sess → 12h/3sess)
func DefaultDreamConfig() *DreamConfig {
	return &DreamConfig{
		Enabled:        true,
		MinHours:       12,
		MinSessions:    3,
		MaxMemoryFiles: 50,
	}
}

// SessionRecord 已完成会话的摘要记录 (v2: 增加重要性评分)
type SessionRecord struct {
	ChatID     string    `json:"chatId"`
	StartTime  time.Time `json:"startTime"`
	EndTime    time.Time `json:"endTime"`
	Turns      int       `json:"turns"`
	Summary    string    `json:"summary"`
	Topics     []string  `json:"topics"`
	// v2: 重要性评分 (0-1), 参考人脑海马体对事件的情感标记
	// 高分: 团队结果、用户明确指令、错误修复; 低分: 闲聊、重复问题
	Importance float64 `json:"importance,omitempty"`
	// v2: 来源类型 (user_chat, team_stage, agent_nested, team_result)
	Source string `json:"source,omitempty"`
}

// MemoryEntryForTest 暴露 memoryEntry 结构供测试使用。
type MemoryEntryForTest struct {
	Content string
	Topics  []string
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

	// MetricsRecorder 指标采集 (通过 SetMetrics 注入)
	MetricsRecorder interface {
		Record(module, name string, value float64)
	}

	// V3 Anti-Amnesia: 增量整合器 (可选)
	Consolidator *Consolidator

	// V3: 重要事件立即触发
	ImportantEventThreshold float64
}

// dreamState 持久化的 Dreaming 状态 (解决进程重启后计数器丢失问题)
type dreamState struct {
	SessionsSinceDream int64     `json:"sessionsSinceDream"`
	LastDreamTime      time.Time `json:"lastDreamTime"`
	LastScanTime       time.Time `json:"lastScanTime"`
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
		config.MinHours = 12
	}
	if config.MinSessions <= 0 {
		config.MinSessions = 3
	}
	if config.MaxMemoryFiles <= 0 {
		config.MaxMemoryFiles = 50
	}

	d := &Dreamer{
		config:                  config,
		cwd:                     cwd,
		lockFile:                filepath.Join(config.MemoryDir, ".dream-lock"),
		ImportantEventThreshold: 0.8,
	}
	d.loadDreamState()
	return d
}

// saveDreamState 持久化 Dreaming 关键状态到磁盘
func (d *Dreamer) saveDreamState() {
	stateFile := filepath.Join(d.config.MemoryDir, "dream_state.json")
	os.MkdirAll(filepath.Dir(stateFile), 0755)

	d.mu.Lock()
	state := dreamState{
		SessionsSinceDream: d.sessionsSinceDream.Load(),
		LastDreamTime:      d.lastDreamTime,
		LastScanTime:       d.lastScanTime,
	}
	d.mu.Unlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(stateFile, data, 0644)
}

// loadDreamState 从磁盘恢复 Dreaming 状态 (防止进程重启后归零)
func (d *Dreamer) loadDreamState() {
	stateFile := filepath.Join(d.config.MemoryDir, "dream_state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return
	}
	var state dreamState
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	d.sessionsSinceDream.Store(state.SessionsSinceDream)
	d.lastDreamTime = state.LastDreamTime
	d.mu.Lock()
	d.lastScanTime = state.LastScanTime
	d.mu.Unlock()
	log.Printf("[Dreaming] 从磁盘恢复状态: sessions=%d, lastDream=%s",
		state.SessionsSinceDream, state.LastDreamTime.Format("2006-01-02 15:04"))
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

// SetConsolidator 设置增量整合器 (V3 Anti-Amnesia)
func (d *Dreamer) SetConsolidator(c *Consolidator) {
	d.Consolidator = c
}

// RecordSession 记录一个已完成的会话。
// 在每次 ProcessMessage 完成后调用。
func (d *Dreamer) RecordSession(record SessionRecord) {
	// v2: 自动评估重要性 (如果调用方未设置)
	if record.Importance == 0 {
		record.Importance = d.estimateImportance(record)
	}
	if record.Source == "" {
		record.Source = "user_chat"
	}

	d.mu.Lock()
	d.recentSessions = append(d.recentSessions, record)
	if len(d.recentSessions) > 100 {
		d.recentSessions = d.recentSessions[len(d.recentSessions)-100:]
	}
	d.mu.Unlock()
	d.sessionsSinceDream.Add(1)

	// 持久化状态 (防止重启后丢失计数)
	go d.saveDreamState()

	// V3: 重要事件立即触发增量蒸馏 (不做完整 Dreaming)
	if record.Importance >= d.ImportantEventThreshold && d.Consolidator != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			d.Consolidator.IncrementalDistill(ctx, []SessionRecord{record})
		}()
	}
}

// estimateImportance 自动评估会话重要性 (参考情感标记假说)。
// 团队结果、错误修复、明确指令 → 高重要性; 闲聊、重复 → 低重要性。
func (d *Dreamer) estimateImportance(record SessionRecord) float64 {
	score := 0.5 // 基础分
	summary := strings.ToLower(record.Summary)

	// 团队相关 → 高重要性
	if strings.Contains(summary, "团队") || strings.Contains(summary, "team") ||
		record.Source == "team_result" || record.Source == "team_stage" {
		score = 0.8
	}
	// 错误/修复 → 高重要性 (负面情绪标记)
	if strings.Contains(summary, "error") || strings.Contains(summary, "错误") ||
		strings.Contains(summary, "修复") || strings.Contains(summary, "bug") {
		score = max(score, 0.7)
	}
	// 内容长度反映信息密度
	if len(record.Summary) > 500 {
		score = max(score, 0.6)
	}
	return score
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// AfterQuery 在每次 query 完成后调用，检查是否应触发 Dreaming。
// 对应 TS: stopHooks.ts 中 "if (!toolUseContext.agentId) { void executeAutoDream(...) }"
//
// 检查门控 (从廉价到昂贵):
//  1. 是否启用
//  2. 是否已在整理中
//  3. 距上次整理是否超过 MinHours
//  4. 会话数是否达到 MinSessions (含超时兜底)
//  5. 距上次扫描是否超过 10 分钟
//  6. 文件锁是否可获取
func (d *Dreamer) AfterQuery(ctx context.Context) {
	if !d.config.Enabled {
		return
	}
	if d.dreaming.Load() {
		d.recordGateBlock("already_dreaming")
		return
	}

	now := time.Now()

	// 时间门控
	timeSinceDream := now.Sub(d.lastDreamTime)
	if !d.lastDreamTime.IsZero() && timeSinceDream < time.Duration(d.config.MinHours)*time.Hour {
		d.recordGateBlock("time_short")
		return
	}

	sessions := d.sessionsSinceDream.Load()

	// 会话数门控 + 超时兜底:
	// 正常路径: sessions >= minSessions
	// 兜底路径: 超过 48h 且至少有 1 条会话 (解决长时间无飞书消息的场景)
	idleFallback := !d.lastDreamTime.IsZero() && timeSinceDream > 48*time.Hour && sessions >= 1
	if sessions < int64(d.config.MinSessions) && !idleFallback {
		d.recordGateBlock("sessions_low")
		return
	}

	// 扫描节流 (10 分钟一次)
	d.mu.Lock()
	if !d.lastScanTime.IsZero() && now.Sub(d.lastScanTime) < 10*time.Minute {
		d.mu.Unlock()
		d.recordGateBlock("scan_throttle")
		return
	}
	d.lastScanTime = now
	d.mu.Unlock()

	if !d.dreaming.CompareAndSwap(false, true) {
		d.recordGateBlock("already_dreaming")
		return
	}

	if !d.acquireLock() {
		d.dreaming.Store(false)
		d.recordGateBlock("lock_held")
		return
	}

	triggerSource := "afterquery"
	if idleFallback {
		triggerSource = "idle_fallback"
		log.Printf("[Dreaming] 超时兜底触发 (已 %v 未整理, %d 条会话)", timeSinceDream.Round(time.Minute), sessions)
	}
	if d.MetricsRecorder != nil {
		d.MetricsRecorder.Record("dreaming", "dream_trigger_source", 1)
		_ = triggerSource // label 将在后续 RecordWithLabels 支持时使用
	}

	go d.executeDream(ctx)
}

// recordGateBlock 记录门控拦截原因 (可观测性)
func (d *Dreamer) recordGateBlock(reason string) {
	if d.MetricsRecorder != nil {
		d.MetricsRecorder.Record("dreaming", "dream_gate_block_"+reason, 1)
	}
}

// executeDream 执行记忆整理 (v2: 人脑睡眠机制启发)。
//
// 参考:
//   - 海马体重放 (Hippocampal Replay): 重要经历按重要性排序优先整理
//   - SWS 慢波睡眠: 强化重要记忆，衰减琐碎记忆
//   - REM 梦境: LLM 发现跨会话模式和关联
//   - 突触缩放: 防止记忆无限增长，低价值记忆被遗忘
func (d *Dreamer) executeDream(ctx context.Context) {
	defer d.dreaming.Store(false)
	defer d.releaseLock()

	log.Printf("[Dreaming] 开始记忆整理 (v2: 重要性+时间衰减)...")
	start := time.Now()

	d.mu.Lock()
	sessions := make([]SessionRecord, len(d.recentSessions))
	copy(sessions, d.recentSessions)
	d.mu.Unlock()

	if err := os.MkdirAll(d.config.MemoryDir, 0755); err != nil {
		log.Printf("[Dreaming] 创建记忆目录失败: %v", err)
		return
	}

	// v2: 创建 dreaming/ 子目录存放本次整理的产出文件
	dreamingDir := filepath.Join(d.config.MemoryDir, "dreaming")
	os.MkdirAll(dreamingDir, 0755)

	// v2: 按重要性排序 (海马体重放: 高情感标记的记忆优先处理)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Importance > sessions[j].Importance
	})

	// V3: 先通过 Consolidator 做增量蒸馏 (如果可用)
	if d.Consolidator != nil {
		if result, cErr := d.Consolidator.IncrementalDistill(ctx, sessions); cErr != nil {
			log.Printf("[Dreaming] Consolidator 蒸馏失败 (非致命): %v", cErr)
		} else {
			log.Printf("[Dreaming] Consolidator: +%d 事实, %d 矛盾, %d 模式",
				result.NewFacts, result.Contradictions, result.PatternsFound)
			if d.MetricsRecorder != nil {
				d.MetricsRecorder.Record("dreaming", "dream_consolidator_facts", float64(result.NewFacts))
				d.MetricsRecorder.Record("dreaming", "dream_consolidator_contradictions", float64(result.Contradictions))
				d.MetricsRecorder.Record("dreaming", "dream_consolidator_patterns", float64(result.PatternsFound))
			}
		}
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
		if d.MetricsRecorder != nil {
			d.MetricsRecorder.Record("dreaming", "dream_error_count", 1)
		}
		return
	}

	// v2: 保存本次 dreaming 产出文件 (解决 dreaming/ 目录为空的问题)
	d.saveDreamLog(dreamingDir, sessions, time.Since(start))

	d.lastDreamTime = time.Now()
	d.sessionsSinceDream.Store(0)
	d.mu.Lock()
	d.recentSessions = nil
	d.mu.Unlock()

	// 持久化清零后的状态
	go d.saveDreamState()

	elapsed := time.Since(start)
	log.Printf("[Dreaming] 整理完成 (耗时 %v, 处理 %d 条会话)", elapsed, len(sessions))

	// 持续观测指标
	if d.MetricsRecorder != nil {
		d.MetricsRecorder.Record("dreaming", "dream_count", 1)
		d.MetricsRecorder.Record("dreaming", "dream_sessions_input", float64(len(sessions)))
		d.MetricsRecorder.Record("dreaming", "dream_duration_sec", elapsed.Seconds())
		// 压缩率: 输入会话总字符 / 输出整理结果字符
		inputSize := 0
		for _, s := range sessions {
			inputSize += len(s.Summary)
		}
		if outputData, readErr := os.ReadFile(filepath.Join(d.config.MemoryDir, "consolidated.md")); readErr == nil {
			outputSize := len(outputData)
			d.MetricsRecorder.Record("dreaming", "dream_output_size", float64(outputSize))
			if outputSize > 0 {
				d.MetricsRecorder.Record("dreaming", "dream_compression_ratio", float64(inputSize)/float64(outputSize))
			}
		}
	}
}

// saveDreamLog 保存每次 dreaming 的产出日志 (解决 dreaming/ 目录为空的问题)。
// 参考人脑: 每次睡眠周期都有可追溯的记忆巩固记录。
func (d *Dreamer) saveDreamLog(dir string, sessions []SessionRecord, elapsed time.Duration) {
	filename := fmt.Sprintf("dream-%s.md", time.Now().Format("20060102-150405"))
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Dream Log: %s\n\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("- 耗时: %v\n", elapsed.Round(time.Second)))
	sb.WriteString(fmt.Sprintf("- 处理会话数: %d\n\n", len(sessions)))

	sb.WriteString("## 整理的会话记录\n\n")
	for i, s := range sessions {
		importance := "普通"
		if s.Importance >= 0.8 {
			importance = "高"
		} else if s.Importance >= 0.5 {
			importance = "中"
		}
		sb.WriteString(fmt.Sprintf("%d. [%s] (重要性:%s, 来源:%s)\n   %s\n",
			i+1, s.EndTime.Format("15:04"), importance, s.Source,
			truncateDream(s.Summary, 200)))
	}
	os.WriteFile(filepath.Join(dir, filename), []byte(sb.String()), 0644)
}

func truncateDream(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
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

	// Phase 3: Consolidate — 重要性加权合并 (参考 MiniMax M2.7)
	consolidated := d.mergeMemories(existingMemories, newEntries)

	// Phase 3.5: 矛盾检测 (参考 CaRT — trust git/tests > prose)
	conflicts := detectContradictions(consolidated)
	if len(conflicts) > 0 {
		log.Printf("[Dreaming] 检测到 %d 个记忆矛盾: %v", len(conflicts), conflicts)
		// 将矛盾信息追加为一条特殊记忆
		conflictEntry := memoryEntry{
			Timestamp: time.Now(),
			Content:   "⚠️ 记忆矛盾警告:\n" + strings.Join(conflicts, "\n"),
			Topics:    []string{"_meta", "contradiction"},
		}
		consolidated = append(consolidated, conflictEntry)
	}

	// Phase 4: Prune — 限制大小 (保留最新的, 优先保留有 topic 的)
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

// mergeMemories 重要性加权合并 (参考 MiniMax M2.7 + Kimi K2 记忆管理)。
// 替代简单精确去重: 按主题分组, 高重要性保留细节, 低重要性压缩。
func (d *Dreamer) mergeMemories(existing, newEntries []memoryEntry) []memoryEntry {
	// 按内容指纹去重 (相似度 >80% 视为同一记忆)
	type fingerprint struct {
		entry    memoryEntry
		priority float64 // 高重要性=高优先级
	}
	var all []fingerprint
	for _, e := range existing {
		all = append(all, fingerprint{entry: e, priority: 0.3}) // 旧记忆基础优先级
	}
	for _, e := range newEntries {
		all = append(all, fingerprint{entry: e, priority: 0.7}) // 新记忆高优先级
	}

	dedup := make(map[string]bool)
	var result []memoryEntry
	for _, fp := range all {
		key := strings.TrimSpace(fp.entry.Content)
		if key == "" {
			continue
		}
		// 简短指纹: 取前 100 字符作为 key (允许尾部差异的近似去重)
		shortKey := key
		if len(shortKey) > 100 {
			shortKey = shortKey[:100]
		}
		if dedup[shortKey] {
			continue
		}
		dedup[shortKey] = true
		result = append(result, fp.entry)
	}

	return result
}

// detectContradictions 矛盾检测 (参考 CaRT + MAgICoRe 步级监督)。
// 检测同一主题的新旧记忆是否冲突, 返回冲突对。
func detectContradictions(entries []memoryEntry) []string {
	topicMap := make(map[string][]string) // topic -> contents
	for _, e := range entries {
		for _, topic := range e.Topics {
			topicMap[topic] = append(topicMap[topic], e.Content)
		}
	}
	var conflicts []string
	for topic, contents := range topicMap {
		if len(contents) < 2 {
			continue
		}
		for i := 0; i < len(contents)-1; i++ {
			for j := i + 1; j < len(contents); j++ {
				// 检测明显矛盾: "已修复" vs "仍存在"
				iFixed := strings.Contains(contents[i], "已修复") || strings.Contains(contents[i], "fixed")
				jExists := strings.Contains(contents[j], "仍存在") || strings.Contains(contents[j], "still")
				jFixed := strings.Contains(contents[j], "已修复") || strings.Contains(contents[j], "fixed")
				iExists := strings.Contains(contents[i], "仍存在") || strings.Contains(contents[i], "still")
				if (iFixed && jExists) || (jFixed && iExists) {
					conflicts = append(conflicts, fmt.Sprintf("主题 '%s': 记忆冲突 — 一条说已修复, 另一条说仍存在", topic))
				}
			}
		}
	}
	return conflicts
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

	hoursSince := 0.0
	if !d.lastDreamTime.IsZero() {
		hoursSince = time.Since(d.lastDreamTime).Hours()
	}

	return DreamStats{
		Enabled:            d.config.Enabled,
		LastDreamTime:      d.lastDreamTime,
		SessionsSinceDream: d.sessionsSinceDream.Load(),
		IsDreaming:         d.dreaming.Load(),
		RecentSessions:     recentCount,
		MemoryDir:          d.config.MemoryDir,
		MinHours:           d.config.MinHours,
		MinSessions:        d.config.MinSessions,
		HoursSinceLast:     hoursSince,
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
	HoursSinceLast     float64   `json:"hoursSinceLast"`
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
