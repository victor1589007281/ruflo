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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	content = compactToolResultContent(toolName, content, tctx)

	return types.Message{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{
			Type:      types.ContentBlockToolResult,
			ToolUseID: toolUseID,
			Content:   content,
			IsError:   result.IsError,
		}},
	}
}

const defaultMaxToolResultChars = 24000

func compactToolResultContent(toolName, content string, tctx *ToolContext) string {
	maxChars := defaultMaxToolResultChars
	if tctx != nil && tctx.MaxToolResultChars > 0 {
		maxChars = tctx.MaxToolResultChars
	}
	if maxChars <= 0 || len(content) <= maxChars {
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

	var b strings.Builder
	b.WriteString("[tool_result compacted]\n")
	b.WriteString(fmt.Sprintf("tool: %s\n", toolName))
	b.WriteString(fmt.Sprintf("original_chars: %d\n", len(content)))
	if artifactPath != "" {
		b.WriteString(fmt.Sprintf("full_artifact: %s\n", artifactPath))
	}
	b.WriteString("summary:\n")
	b.WriteString(summary)
	return b.String()
}

func writeToolResultArtifact(toolName, content string, tctx *ToolContext) string {
	if tctx == nil || strings.TrimSpace(tctx.Cwd) == "" {
		return ""
	}
	dir := filepath.Join(tctx.Cwd, ".claude-go", "artifacts", "tool-results")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	name := sanitizeArtifactName(toolName)
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.txt", time.Now().Format("20060102-150405.000000"), name))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return ""
	}
	return path
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
		if i >= 20 {
			break
		}
		picked = append(picked, line)
	}
	lower := strings.ToLower(raw)
	for _, marker := range []string{"error", "错误", "failed", "panic", "exception", "fatal"} {
		if idx := strings.Index(lower, marker); idx >= 0 {
			end := idx + 800
			if end > len(raw) {
				end = len(raw)
			}
			picked = append(picked, "", "[diagnostic excerpt]", raw[idx:end])
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
