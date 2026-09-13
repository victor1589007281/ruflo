// orchestration.go 实现工具编排逻辑。
// 对应 TS 源码: review/claude/src/services/tools/toolOrchestration.ts
//
// 核心算法: partitionToolCalls
// 将 LLM 返回的多个 tool_use 块分成批次:
//   - 连续的只读/并发安全工具 → 一个并发批次 (parallel)
//   - 非只读工具 → 单独一个串行批次 (serial)
//
// 例如 [Glob, Grep, FileWrite, FileRead, FileRead] 会被分区为:
//
//	Batch 1: [Glob, Grep] → concurrent
//	Batch 2: [FileWrite] → serial
//	Batch 3: [FileRead, FileRead] → concurrent
//
// 这保证了写操作不会与其他操作并发执行，
// 同时最大化只读操作的并行度。
package tool

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// MaxToolUseConcurrency 单批次内最大并发数
// 对应 TS: getMaxToolUseConcurrency() (默认 10)
const MaxToolUseConcurrency = 10

// Batch 表示一个工具执行批次
type Batch struct {
	IsConcurrencySafe bool
	Blocks            []types.ContentBlock
}

// ToolCallUpdate 单次工具调用的更新事件
// 对应 TS: toolOrchestration.ts 中的 MessageUpdate
type ToolCallUpdate struct {
	Message    *types.Message
	NewContext *ToolContext
}

// PartitionToolCalls 将工具调用分区为并发/串行批次。
// 对应 TS: toolOrchestration.ts 中的 partitionToolCalls()
//
// 算法:
//  1. 遍历所有 tool_use 块
//  2. 对每个块，通过 registry 查找对应工具
//  3. 调用 tool.IsConcurrencySafe(input) 判断是否并发安全
//  4. 相邻的相同类型块合并为一个批次
//  5. 非并发安全的块独立成批
func PartitionToolCalls(blocks []types.ContentBlock, reg *Registry) []Batch {
	if len(blocks) == 0 {
		return nil
	}

	var batches []Batch
	var currentBatch *Batch

	for _, block := range blocks {
		if block.Type != types.ContentBlockToolUse {
			continue
		}

		tool, ok := reg.Get(block.Name)
		isSafe := false
		if ok {
			isSafe = tool.IsConcurrencySafe(block.Input)
		}

		if currentBatch == nil {
			currentBatch = &Batch{
				IsConcurrencySafe: isSafe,
				Blocks:            []types.ContentBlock{block},
			}
		} else if isSafe && currentBatch.IsConcurrencySafe {
			currentBatch.Blocks = append(currentBatch.Blocks, block)
		} else {
			batches = append(batches, *currentBatch)
			currentBatch = &Batch{
				IsConcurrencySafe: isSafe,
				Blocks:            []types.ContentBlock{block},
			}
		}
	}

	if currentBatch != nil {
		batches = append(batches, *currentBatch)
	}

	return batches
}

// RunTools 编排并执行工具调用。
// 对应 TS: toolOrchestration.ts 中的 runTools()
//
// 对于并发安全的批次，使用 goroutine 并发执行；
// 对于串行批次，逐个执行。
// 每次执行通过 RunToolUse 完成。
func RunTools(
	ctx context.Context,
	blocks []types.ContentBlock,
	reg *Registry,
	tctx *ToolContext,
	hookRunner HookRunner,
) []types.Message {
	batches := PartitionToolCalls(blocks, reg)
	var allResults []types.Message

	for _, batch := range batches {
		if batch.IsConcurrencySafe {
			results := runConcurrentBatch(ctx, batch.Blocks, reg, tctx, hookRunner)
			allResults = append(allResults, results...)
		} else {
			results := runSerialBatch(ctx, batch.Blocks, reg, tctx, hookRunner)
			allResults = append(allResults, results...)
		}
	}
	return allResults
}

// HookRunner 工具 hook 执行器接口
// 对应 TS: services/tools/toolHooks.ts
type HookRunner interface {
	RunPreToolUseHooks(toolName string, input json.RawMessage) (*types.HookOutput, error)
	RunPostToolUseHooks(toolName string, input json.RawMessage, result string, isError bool) error
	RunPostToolUseFailureHooks(toolName string, input json.RawMessage, errMsg string) error
}

// runConcurrentBatch 并发执行一批工具调用。
// 对应 TS: toolOrchestration.ts 中的 runToolsConcurrently()
// 使用 goroutine + WaitGroup 实现并发，
// 通过 channel 限制并发度不超过 MaxToolUseConcurrency。
func runConcurrentBatch(
	ctx context.Context,
	blocks []types.ContentBlock,
	reg *Registry,
	tctx *ToolContext,
	hookRunner HookRunner,
) []types.Message {
	results := make([]types.Message, len(blocks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, MaxToolUseConcurrency)

	for i, block := range blocks {
		wg.Add(1)
		go func(idx int, b types.ContentBlock) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = RunToolUse(ctx, b, reg, tctx, hookRunner)
		}(i, block)
	}

	wg.Wait()
	return results
}

// runSerialBatch 串行执行一批工具调用
// 对应 TS: toolOrchestration.ts 中的 runToolsSerially()
func runSerialBatch(
	ctx context.Context,
	blocks []types.ContentBlock,
	reg *Registry,
	tctx *ToolContext,
	hookRunner HookRunner,
) []types.Message {
	results := make([]types.Message, 0, len(blocks))
	for _, block := range blocks {
		result := RunToolUse(ctx, block, reg, tctx, hookRunner)
		results = append(results, result)
	}
	return results
}

// RunToolUse 执行单个工具调用，返回 tool_result 消息。
// 对应 TS: services/tools/toolExecution.ts 中的 runToolUse()
//
// 执行流程:
//  1. 查找工具
//  2. 运行 pre-tool-use hooks (可被 hook 阻止)
//  3. 检查权限
//  4. 调用 tool.Call()
//  5. 运行 post-tool-use hooks
//  6. 组装 tool_result 消息返回
func RunToolUse(
	ctx context.Context,
	block types.ContentBlock,
	reg *Registry,
	tctx *ToolContext,
	hookRunner HookRunner,
) types.Message {
	toolUseID := block.ID
	toolName := block.Name

	makeErrorResult := func(errMsg string) types.Message {
		return types.Message{
			Type: types.MessageTypeUser,
			Content: []types.ContentBlock{{
				Type:      types.ContentBlockToolResult,
				ToolUseID: toolUseID,
				Content:   errMsg,
				IsError:   true,
			}},
		}
	}

	t, ok := reg.Get(toolName)
	if !ok {
		return makeErrorResult(fmt.Sprintf("未知工具: %s", toolName))
	}

	// Pre-tool-use hooks
	var preHookContext string
	if hookRunner != nil {
		hookOut, err := hookRunner.RunPreToolUseHooks(toolName, block.Input)
		if err == nil && hookOut != nil {
			// Decision 语义: "deny" / "block" = 阻止; "approve" = 显式放行
			if hookOut.Decision == "deny" || hookOut.Decision == "block" {
				reason := hookOut.Reason
				if reason == "" {
					reason = "被 hook 阻止"
				}
				return makeErrorResult(fmt.Sprintf("Hook 阻止了工具执行: %s", reason))
			}
			preHookContext = strings.TrimSpace(hookOut.AdditionalContext)
		}
	}

	// 权限检查：合并全局 Checker（CheckGlobal）与工具自带 CheckPermissions
	toolPerm := t.CheckPermissions(block.Input, tctx)
	var final types.PermissionResult
	if tctx != nil && tctx.GlobalPerm != nil {
		final = tctx.GlobalPerm.CheckGlobal(toolName, block.Input, t.IsReadOnly(block.Input), toolPerm)
	} else {
		final = types.PermissionResult{Behavior: types.PermissionAllow}
		if toolPerm != nil {
			final = *toolPerm
		}
	}
	switch final.Behavior {
	case types.PermissionDeny:
		reason := final.Reason
		if reason == "" {
			reason = "权限被拒绝"
		}
		return makeErrorResult(reason)
	case types.PermissionAsk:
		reason := final.Reason
		if reason == "" {
			reason = "需要用户确认后方可执行该工具"
		}
		if tctx != nil && !tctx.IsNonInteractive {
			approved, alwaysAllow := promptUserApproval(toolName, block.Input, reason)
			if !approved {
				return makeErrorResult("用户拒绝: " + toolName)
			}
			if alwaysAllow && tctx.GlobalPerm != nil {
				tctx.GlobalPerm.AddSessionAllowRule(toolName)
			}
		} else {
			return makeErrorResult("权限待确认: " + reason)
		}
	default:
		// allow — 继续执行
	}

	// 执行工具
	result, err := t.Call(ctx, block.Input, tctx)
	if err != nil {
		content := fmt.Sprintf("工具执行错误: %v", err)
		if hookRunner != nil {
			_ = hookRunner.RunPostToolUseHooks(toolName, block.Input, content, true)
			_ = hookRunner.RunPostToolUseFailureHooks(toolName, block.Input, content)
		}
		return makeErrorResult(content)
	}

	// Post-tool-use hooks
	if hookRunner != nil {
		_ = hookRunner.RunPostToolUseHooks(toolName, block.Input, result.Content, result.IsError)
	}

	content := result.Content
	if preHookContext != "" {
		content = preHookContext + "\n\n" + content
	}
	content = compactToolResultContent(toolName, block.Input, content, tctx)

	resultBlocks := []types.ContentBlock{{
		Type:      types.ContentBlockToolResult,
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   result.IsError,
	}}
	// 图像附件: 以 image 块追加在同一条 user 消息里 (Anthropic 多模态格式),
	// tool_result 文本负责说明来源, 视觉模型 (kimi/qwen) 可直接查看图像内容。
	for i := range result.Images {
		src := result.Images[i]
		resultBlocks = append(resultBlocks, types.ContentBlock{
			Type:   types.ContentBlockImage,
			Source: &src,
		})
	}

	return types.Message{
		Type:    types.MessageTypeUser,
		Content: resultBlocks,
	}
}

const defaultMaxToolResultChars = 24000

// compactHeadLines 压缩预览展示的开头行数。摘要实现与 read_hint 的续读起点
// 必须同源 —— 各写一个 20 会让"提示从第 21 行续读"与"实际展示了前 19 行"对不上。
const compactHeadLines = 20

// 弱模型确定性后处理阈值 (方案三 L3, 手册 13.3.3):
// 单行 2000 字符截断 (防单行 minified 文件/二进制 dump 挤爆上下文),
// 总行数 >4000 时保头 2000 行 + 尾 1000 行 (错误信息通常在尾部,
// 命令回显在头部——中间是大段重复输出, lost-in-the-middle 对弱模型最毒)。
const (
	weakMaxLineChars  = 2000
	weakMaxTotalLines = 4000
	weakHeadLines     = 2000
	weakTailLines     = 1000
)

// deterministicToolResultPostprocess 零成本确定性后处理: 单行截断 + 保头尾。
// 不用模型压缩 (LLMLingua 式压缩本身要花调用, 对本地弱模型不划算)。
func deterministicToolResultPostprocess(content string) string {
	lines := strings.Split(content, "\n")
	changed := false
	for i, line := range lines {
		if len(line) > weakMaxLineChars {
			lines[i] = line[:weakMaxLineChars] + "…[line truncated]"
			changed = true
		}
	}
	if len(lines) > weakMaxTotalLines {
		omitted := len(lines) - weakHeadLines - weakTailLines
		out := make([]string, 0, weakHeadLines+weakTailLines+1)
		out = append(out, lines[:weakHeadLines]...)
		out = append(out, fmt.Sprintf("...[omitted %d lines / 省略中段 %d 行, 完整结果见 artifact 或重跑收窄命令]...", omitted, omitted))
		out = append(out, lines[len(lines)-weakTailLines:]...)
		return strings.Join(out, "\n")
	}
	if !changed {
		return content
	}
	return strings.Join(lines, "\n")
}

// toolResultArtifactDir 超大 tool_result 的落盘目录。空串 = 不可用 (无 cwd)。
func toolResultArtifactDir(tctx *ToolContext) string {
	if tctx == nil || strings.TrimSpace(tctx.Cwd) == "" {
		return ""
	}
	return filepath.Join(tctx.Cwd, ".claude-go", "artifacts", "tool-results")
}

// toolInputPath 从工具入参里取"这次操作的文件路径"。各工具字段名不统一, 都试一遍。
func toolInputPath(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"path", "file_path", "notebook_path", "file"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// readsArtifact 判断这次调用读的是不是落盘的 tool_result 本身。
//
// 这是溢出逻辑的**自指保护**: artifact 是"把超大结果搬出上下文"的目的地, 如果模型
// 去 Read 它、读回来的内容又超预算、于是再落一份 artifact…… 每一轮都只把上一轮的
// 预览再预览一遍, 正文永远进不了上下文, 而且每轮多一个文件。必须在入口掐掉。
//
// 判定按**目录**而不是按文件名: 落盘文件名带内容哈希, 换个名字照样落在同一目录。
func readsArtifact(input json.RawMessage, tctx *ToolContext) bool {
	dir := toolResultArtifactDir(tctx)
	if dir == "" {
		return false
	}
	p := toolInputPath(input)
	if p == "" {
		return false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(tctx.Cwd, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func compactToolResultContent(toolName string, input json.RawMessage, content string, tctx *ToolContext) string {
	// 方案三 L3: 弱模型后处理先做 (单行+总行整形), 再走既有字符预算逻辑。
	if tctx != nil && tctx.WeakResultPostprocess {
		content = deterministicToolResultPostprocess(content)
	}
	maxChars := defaultMaxToolResultChars
	if tctx != nil && tctx.MaxToolResultChars > 0 {
		maxChars = tctx.MaxToolResultChars
	}
	if maxChars <= 0 || len(content) <= maxChars {
		return content
	}
	// 自指保护先于落盘: 读 artifact 的结果不再落 artifact (见 readsArtifact 注释)。
	if readsArtifact(input, tctx) {
		return content
	}

	artifactPath := writeToolResultArtifact(toolName, content, tctx)
	summaryLimit := maxChars / 3
	if summaryLimit < 2500 {
		summaryLimit = 2500
	}
	if summaryLimit > 6000 {
		summaryLimit = 6000
	}
	summary := summarizeLongToolResult(content, summaryLimit)

	totalLines := strings.Count(content, "\n") + 1
	shownHead := compactHeadLines
	if shownHead > totalLines {
		shownHead = totalLines
	}

	var b strings.Builder
	b.WriteString("[tool_result compacted]\n")
	b.WriteString(fmt.Sprintf("tool: %s\n", toolName))
	b.WriteString(fmt.Sprintf("original_chars: %d\n", len(content)))
	b.WriteString(fmt.Sprintf("original_lines: %d\n", totalLines))
	if artifactPath != "" {
		b.WriteString(fmt.Sprintf("full_artifact: %s\n", artifactPath))
		b.WriteString(fmt.Sprintf("shown: 开头 %d 行 + 结尾片段\n", shownHead))
		// 给可执行的续读坐标, 而不是只丢一个路径让模型自己试:
		// 告诉它 (a) 不必重跑命令, (b) **从预览已经展示过的位置之后**接着读, (c) 一次别要太多行。
		b.WriteString("artifact_note: 完整结果已落盘, **不要重跑命令来再看一遍**。\n")
		// 续读优先指向**原始来源**(Read/Bash 入参里的文件路径), 而不是 artifact:
		// artifact 存的是 Read 的渲染结果(每行带 "N|" 前缀), 拿它当"原文"续读会叠一层行号。
		// 从 shownHead+1 起而不是从 1 起 —— 从 1 起会让模型把刚看过的开头再读一遍。
		if srcPath := toolInputPath(input); srcPath != "" {
			b.WriteString(fmt.Sprintf(
				"read_hint: 续读用 Read(path=%q, offset=%d, limit=200); 单次读太多行会被再次压缩, 按返回里的续读入口往下翻。\n",
				srcPath, shownHead+1))
		} else {
			b.WriteString(fmt.Sprintf(
				"read_hint: 续读用 Read(path=%q, offset=%d, limit=200); 读 artifact 本身不会再被压缩。\n",
				artifactPath, shownHead+1))
		}
	} else {
		// 落盘失败 (无 cwd / 磁盘只读): 明说拿不到全文, 免得模型以为还能读到。
		b.WriteString("full_artifact: (落盘失败, 本次无全文留存)\n")
	}
	b.WriteString("summary:\n")
	b.WriteString(summary)
	return b.String()
}

// writeToolResultArtifact 把超大结果落盘, 返回路径 (失败返回空串)。
//
// 文件名用**内容哈希**而不是时间戳: 同一份输出被反复产生 (重跑同一条命令、重复
// Read 同一文件) 是常态, 时间戳命名每次都新建一份, 目录里全是重复内容; 内容寻址
// 天然去重, 且同一份内容恒得同一路径 —— 模型上下文里引用过的路径不会因为换了一次
// 时间戳而失效。
func writeToolResultArtifact(toolName, content string, tctx *ToolContext) string {
	dir := toolResultArtifactDir(tctx)
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.txt", sanitizeArtifactName(toolName), hex.EncodeToString(sum[:])[:12]))
	// 同一内容已落过盘就复用: 语义相同, 省一次写。
	if _, err := os.Stat(path); err == nil {
		maybeSweepToolArtifacts(dir)
		return path
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return ""
	}
	maybeSweepToolArtifacts(dir)
	return path
}

// ─── artifact 目录清理 ────────────────────────────────────────────────────────
//
// 目录跨 run 共享且只增不减 (内容寻址能去重, 但不同内容仍会累积), 长期跑下去会把
// 工作区撑满。策略两条, 都可配:
//
//	CLAUDE_GO_TOOL_ARTIFACT_MAX_FILES     条数上限, 默认 1000, 0 = 不限
//	CLAUDE_GO_TOOL_ARTIFACT_MAX_AGE_DAYS  天数上限, 默认 30,  0 = 不限
//
// 先按年龄删, 再按条数删最旧的 (两条独立生效, 不是二选一)。
//
// 保守之处: 只删本目录下的普通文件, 不动子目录; 出错一律静默放弃 —— 清理失败不该
// 影响工具结果本身 (原文已经返回给模型了)。磁盘配额只是兜底, 不是正确性依赖。
const (
	envArtifactMaxFiles   = "CLAUDE_GO_TOOL_ARTIFACT_MAX_FILES"
	envArtifactMaxAgeDays = "CLAUDE_GO_TOOL_ARTIFACT_MAX_AGE_DAYS"

	defaultArtifactMaxFiles   = 1000
	defaultArtifactMaxAgeDays = 30

	// 两次清理的最小间隔: 每次落盘都 ReadDir 不划算, 而目录涨到需要清理的量级
	// 是分钟级的, 十秒一次足够及时。
	artifactSweepInterval = 10 * time.Second
)

// lastArtifactSweep 上次清理时间 (UnixNano)。进程内共享, 多 run 并发时也只需一个。
var lastArtifactSweep atomic.Int64

func artifactLimitFromEnv(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		// 配错就退回默认值而不是当成"不限" —— 一个拼错的变量名不该静默关掉清理
		logging.For("toolartifact").Warn("清理配置无效, 用默认值", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

func maybeSweepToolArtifacts(dir string) {
	now := time.Now()
	if prev := lastArtifactSweep.Load(); prev != 0 && now.Sub(time.Unix(0, prev)) < artifactSweepInterval {
		return
	}
	// 竞争失败也不重试: 另一个 goroutine 正在扫, 让它扫完就是。
	if !lastArtifactSweep.CompareAndSwap(lastArtifactSweep.Load(), now.UnixNano()) {
		return
	}
	sweepToolArtifacts(dir, artifactLimitFromEnv(envArtifactMaxFiles, defaultArtifactMaxFiles),
		artifactLimitFromEnv(envArtifactMaxAgeDays, defaultArtifactMaxAgeDays), now)
}

// sweepToolArtifacts 执行一次清理。单独拆出来便于测试直接驱动 (绕过节流)。
func sweepToolArtifacts(dir string, maxFiles, maxAgeDays int, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type artifact struct {
		name string
		mod  time.Time
	}
	items := make([]artifact, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue // 不碰子目录: 目录结构不是本机制建的, 语义不明
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, artifact{name: e.Name(), mod: info.ModTime()})
	}
	removedAge, removedCap := 0, 0
	live := items[:0:0]
	if maxAgeDays > 0 {
		cutoff := now.AddDate(0, 0, -maxAgeDays)
		for _, it := range items {
			if it.mod.Before(cutoff) {
				if os.Remove(filepath.Join(dir, it.name)) == nil {
					removedAge++
				}
				continue
			}
			live = append(live, it)
		}
	} else {
		live = items
	}
	if maxFiles > 0 && len(live) > maxFiles {
		// 按修改时间升序, 从最旧的开始删到刚好剩 maxFiles 条
		sort.Slice(live, func(i, j int) bool { return live[i].mod.Before(live[j].mod) })
		for i := 0; i < len(live)-maxFiles; i++ {
			if os.Remove(filepath.Join(dir, live[i].name)) == nil {
				removedCap++
			}
		}
	}
	if removedAge+removedCap > 0 {
		logging.For("toolartifact").Info("tool_result artifact 清理",
			"dir", dir, "按年龄", removedAge, "按条数", removedCap, "剩余", len(live)-removedCap)
	}
}

func sanitizeArtifactName(s string) string {
	if s == "" {
		return "tool"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "tool"
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func summarizeLongToolResult(raw string, maxChars int) string {
	if len(raw) <= maxChars {
		return raw
	}
	lines := strings.Split(raw, "\n")
	var picked []string
	for i, line := range lines {
		if i >= compactHeadLines {
			break
		}
		picked = append(picked, line)
	}
	lower := strings.ToLower(raw)
	for _, marker := range []string{"error", "错误", "failed", "panic", "exception", "fatal"} {
		if idx := strings.Index(lower, marker); idx >= 0 {
			// idx 是 lower 的字节偏移。strings.ToLower 底层 strings.Map 会把每个非法 UTF-8
			// 字节替换成 U+FFFD(1 字节→3 字节)，故当 raw 含二进制/截断的多字节数据(如读到
			// 二进制文件、被切断的流)时 lower 会比 raw 长，idx 可能越过 len(raw)——用它切 raw
			// 会越界(曾崩: slice bounds out of range [103740:100043], 约 1850 个坏字节即触发)。
			// 改切 lower：同一坐标系，恒合法且始终对齐 marker；诊断摘要小写化无碍。end 按 lower 长度夹取。
			end := idx + 800
			if end > len(lower) {
				end = len(lower)
			}
			picked = append(picked, "", "[diagnostic excerpt]", lower[idx:end])
			break
		}
	}
	tail := raw
	if len(tail) > 1200 {
		tail = tail[len(tail)-1200:]
	}
	picked = append(picked, "", "[tail excerpt]", tail)
	out := strings.Join(picked, "\n")
	if len(out) > maxChars {
		out = out[:maxChars] + "\n...(summary truncated)"
	}
	return out
}

func promptUserApproval(toolName string, input json.RawMessage, reason string) (approved bool, alwaysAllow bool) {
	inputStr := string(input)
	if len(inputStr) > 200 {
		inputStr = inputStr[:200] + "..."
	}
	fmt.Printf("\n\033[33m[权限] %s 请求执行: %s\033[0m\n", toolName, reason)
	fmt.Printf("  输入: %s\n", inputStr)
	fmt.Print("  允许? [y/n/a(始终允许)] ")

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))

	switch line {
	case "y", "yes":
		return true, false
	case "a", "always":
		return true, true
	default:
		return false, false
	}
}
