// Package hooks 实现 Claude Code 的 Hook 系统。
// 对应 TS 源码: review/claude/src/utils/hooks.ts + review/claude/src/utils/hooks/postSamplingHooks.ts
//
// Hook 系统在工具执行和对话生命周期的关键节点运行用户自定义逻辑。
//
// Hook 类型 (HookType):
//   - command: spawn shell，stdin 传入 JSON (HookInput)，stdout 解析 HookOutput
//   - prompt: 不执行命令；将 Command 字段作为 AdditionalContext 返回
//   - http: POST HookInput JSON 到 URL（URL 为 url 字段或 Command），解析响应 JSON 为 HookOutput
//
// Hook 事件:
//   - PreToolUse: 工具执行前 (可阻止执行)
//   - PostToolUse: 工具执行后
//   - PostToolUseFailure: 工具执行失败后
//   - Stop / StopFailure: 模型回复完成或失败时
//   - PreCompact/PostCompact: 上下文压缩前后
//   - SessionStart/SessionEnd: 会话开始/结束
//   - SubagentStart/SubagentStop: 子代理启停
//   - TeammateIdle / TaskCompleted: Agent Teams
//
// 条件: 若配置 If 非空，仅当 tool_name 匹配该 glob（如 Read*）时运行。
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/sandbox"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// Runner Hook 执行器。
// 管理 hook 配置，在各生命周期点执行相应的 shell 命令。
type Runner struct {
	configs   []types.HookConfig
	sessionID string
	timeout   time.Duration

	postMu sync.RWMutex
	// PostSamplingCallbacks 在 ExecutePostSamplingHooks 中依次调用；请用 RegisterPostSamplingHook 注册以保证并发安全。
	PostSamplingCallbacks []func(messages []types.Message)
}

// NewRunner 创建 hook 执行器
func NewRunner(configs []types.HookConfig, sessionID string) *Runner {
	return &Runner{
		configs:   configs,
		sessionID: sessionID,
		timeout:   10 * time.Second,
	}
}

// RegisterPostSamplingHook 注册后采样回调（在 ExecutePostSamplingHooks 中调用）。
func (r *Runner) RegisterPostSamplingHook(fn func(messages []types.Message)) {
	if fn == nil {
		return
	}
	r.postMu.Lock()
	defer r.postMu.Unlock()
	r.PostSamplingCallbacks = append(r.PostSamplingCallbacks, fn)
}

// RunPreToolUseHooks 执行 PreToolUse hooks。
// 对应 TS: services/tools/toolHooks.ts 中的 runPreToolUseHooks()
//
// 如果任何 hook 返回 decision="block"，工具执行将被阻止。
// prompt/http/command 返回的 AdditionalContext 会合并到返回的 HookOutput（无 block 时）。
func (r *Runner) RunPreToolUseHooks(toolName string, input json.RawMessage) (*types.HookOutput, error) {
	hooks := r.findHooks(types.HookEventPreToolUse, toolName)
	if len(hooks) == 0 {
		return nil, nil
	}

	hookInput := types.HookInput{
		Event:     types.HookEventPreToolUse,
		SessionID: r.sessionID,
		ToolName:  toolName,
		ToolInput: input,
	}

	var contexts []string
	for _, h := range hooks {
		output, err := r.executeHook(h, hookInput)
		if err != nil {
			continue
		}
		if output != nil {
			// Decision 语义: "deny" / "block" = 阻止; "approve" = 显式放行并跳过剩余 hook
			if output.Decision == "deny" || output.Decision == "block" {
				return output, nil
			}
			if output.Decision == "approve" {
				// 显式批准: 跳过剩余 hooks，返回 nil 表示不阻止
				if len(contexts) > 0 {
					return &types.HookOutput{AdditionalContext: strings.Join(contexts, "\n")}, nil
				}
				return nil, nil
			}
			if output.AdditionalContext != "" {
				contexts = append(contexts, output.AdditionalContext)
			}
		}
	}
	if len(contexts) > 0 {
		return &types.HookOutput{AdditionalContext: strings.Join(contexts, "\n")}, nil
	}
	return nil, nil
}

// RunPostToolUseHooks 执行 PostToolUse hooks。
// 对应 TS: services/tools/toolHooks.ts 中的 runPostToolUseHooks()
func (r *Runner) RunPostToolUseHooks(toolName string, input json.RawMessage, result string, isError bool) error {
	hooks := r.findHooks(types.HookEventPostToolUse, toolName)
	if len(hooks) == 0 {
		return nil
	}

	hookInput := types.HookInput{
		Event:      types.HookEventPostToolUse,
		SessionID:  r.sessionID,
		ToolName:   toolName,
		ToolInput:  input,
		ToolResult: result,
		IsError:    isError,
	}

	for _, h := range hooks {
		_, _ = r.executeHook(h, hookInput)
	}
	return nil
}

// ExecutePostSamplingHooks 执行后采样 hooks。
// 对应 TS: utils/hooks/postSamplingHooks.ts 中的 executePostSamplingHooks()
//
// 调用所有通过 RegisterPostSamplingHook 注册的回调；单个回调 panic/错误仅记录日志。
func (r *Runner) ExecutePostSamplingHooks(messages []types.Message) {
	r.postMu.RLock()
	n := len(r.PostSamplingCallbacks)
	callbacks := make([]func([]types.Message), n)
	copy(callbacks, r.PostSamplingCallbacks)
	r.postMu.RUnlock()

	for i, fn := range callbacks {
		func(idx int, f func([]types.Message)) {
			defer func() {
				if rec := recover(); rec != nil {
					logging.For("hooks").Error("post-sampling callback panic", "index", idx, "recover", rec)
				}
			}()
			if f == nil {
				return
			}
			f(messages)
		}(i, fn)
	}
}

// ExecuteStopHooks 执行 Stop hooks。
// 对应 TS: query/stopHooks.ts 中的 handleStopHooks()
//
// 当模型回复完成且没有 tool_use 时调用。
// 返回值: 需要注入到对话中的阻止消息 (让模型继续处理)。
// 空返回表示正常结束。
func (r *Runner) ExecuteStopHooks(messages []types.Message) []types.Message {
	return r.executeStopLikeHooks(types.HookEventStop, messages)
}

// ExecuteStopFailureHooks 执行 StopFailure 事件的 hooks。
func (r *Runner) ExecuteStopFailureHooks(messages []types.Message) []types.Message {
	return r.executeStopLikeHooks(types.HookEventStopFailure, messages)
}

func (r *Runner) executeStopLikeHooks(event types.HookEvent, messages []types.Message) []types.Message {
	hooks := r.findHooks(event, "")
	if len(hooks) == 0 {
		return nil
	}

	hookInput := types.HookInput{
		Event:     event,
		SessionID: r.sessionID,
		Messages:  messages,
	}

	var blockingMessages []types.Message
	for _, h := range hooks {
		output, err := r.executeHook(h, hookInput)
		if err != nil {
			continue
		}
		if output != nil {
			// ContinueDecision 语义: "deny" / "block" = 阻止并注入恢复消息; "approve" = 不阻止
			if output.ContinueDecision == "approve" {
				return nil
			}
			if output.ContinueDecision == "deny" || output.ContinueDecision == "block" {
				reason := output.Reason
				if reason == "" {
					reason = "Stop hook 要求继续"
				}
				blockingMessages = append(blockingMessages, types.Message{
					Type: types.MessageTypeUser,
					Content: []types.ContentBlock{{
						Type: types.ContentBlockText,
						Text: reason,
					}},
					IsMeta: true,
				})
			}
		}
	}
	return blockingMessages
}

// ExecuteSessionHooks 执行会话生命周期 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteSessionHooks(event types.HookEvent) *types.HookOutput {
	return r.executeMessageHooksWithDecision(event, nil, types.HookInput{})
}

// ExecutePreCompactHooks 执行上下文压缩前 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecutePreCompactHooks(messages []types.Message) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventPreCompact, messages, types.HookInput{})
}

// ExecutePostCompactHooks 执行上下文压缩后 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecutePostCompactHooks(messages []types.Message) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventPostCompact, messages, types.HookInput{})
}

// ExecutePreTurnHooks 执行单轮开始前 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecutePreTurnHooks(messages []types.Message, turnCount int) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventPreTurn, messages, types.HookInput{TurnCount: turnCount})
}

// ExecutePostTurnHooks 执行单轮结束后 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecutePostTurnHooks(messages []types.Message, turnCount int) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventPostTurn, messages, types.HookInput{TurnCount: turnCount})
}

// ExecutePreRequestHooks 执行 API 请求前 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecutePreRequestHooks(messages []types.Message, model string) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventPreRequest, messages, types.HookInput{Model: model})
}

// ExecutePostRequestHooks 执行 API 请求后 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecutePostRequestHooks(messages []types.Message, model string) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventPostRequest, messages, types.HookInput{Model: model})
}

// ExecuteOnContextOverflowHooks 执行上下文溢出预警 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnContextOverflowHooks(messages []types.Message, level int) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventOnContextOverflow, messages, types.HookInput{BudgetLevel: level})
}

// ExecuteOnMaxTurnsReachedHooks 执行到达最大轮次 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnMaxTurnsReachedHooks(messages []types.Message, turnCount int) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventOnMaxTurnsReached, messages, types.HookInput{TurnCount: turnCount})
}

// ExecuteOnErrorHooks 执行错误捕获 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnErrorHooks(messages []types.Message, reason string, err error) *types.HookOutput {
	in := types.HookInput{Reason: reason}
	if err != nil {
		in.ErrorMessage = err.Error()
	}
	return r.executeMessageHooksWithDecision(types.HookEventOnError, messages, in)
}

// ExecuteOnRecoveryHooks 执行恢复/降级 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnRecoveryHooks(messages []types.Message, reason string) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventOnRecovery, messages, types.HookInput{Reason: reason})
}

// ExecuteOnRateLimitHooks 执行限流触发 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnRateLimitHooks(err error, backoff time.Duration) *types.HookOutput {
	in := types.HookInput{BackoffMs: int(backoff.Milliseconds())}
	if err != nil {
		in.ErrorMessage = err.Error()
	}
	return r.executeMessageHooksWithDecision(types.HookEventOnRateLimit, nil, in)
}

// ExecuteOnRetryHooks 执行重试 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnRetryHooks(messages []types.Message, reason string, attempt int) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventOnRetry, messages, types.HookInput{Reason: reason, TurnCount: attempt})
}

// ExecuteOnMessageFilterHooks 执行消息过滤 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnMessageFilterHooks(messages []types.Message) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventOnMessageFilter, messages, types.HookInput{})
}

// ExecuteNotificationHooks 执行通知类 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteNotificationHooks(msg string) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventNotification, nil, types.HookInput{ErrorMessage: msg})
}

// ExecuteSubagentStartHooks 执行子代理启动 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteSubagentStartHooks(role, prompt string) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventSubagentStart, nil, types.HookInput{Role: role, Reason: prompt})
}

// ExecuteSubagentStopHooks 执行子代理停止 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteSubagentStopHooks(role, result string) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventSubagentStop, nil, types.HookInput{Role: role, ToolResult: result})
}

// ExecuteTeammateIdleHooks 执行队友空闲 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteTeammateIdleHooks(teammate string) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventTeammateIdle, nil, types.HookInput{Role: teammate})
}

// ExecuteTaskCompletedHooks 执行任务完成 hooks。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteTaskCompletedHooks(taskID string, success bool) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventTaskCompleted, nil, types.HookInput{TaskID: taskID, Success: success})
}

// ExecuteOnChunkHooks 执行流式 chunk 输出 hooks。
// 在每个 content_block_delta（text 或 thinking）时触发。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnChunkHooks(chunkText string, blockIndex int, isThinking bool) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventOnChunk, nil, types.HookInput{
		ChunkText:  chunkText,
		BlockIndex: blockIndex,
		IsThinking: isThinking,
	})
}

// ExecuteOnTokenStreamHooks 执行流式 token 输出 hooks。
// 在每个 text_delta（非 thinking）时触发。
// 返回值：若 Hook 返回 Decision/ContinueDecision，则返回该 HookOutput；否则 nil。
func (r *Runner) ExecuteOnTokenStreamHooks(tokenText string, blockIndex int) *types.HookOutput {
	return r.executeMessageHooksWithDecision(types.HookEventOnTokenStream, nil, types.HookInput{
		ChunkText:  tokenText,
		BlockIndex: blockIndex,
		IsThinking: false,
	})
}

// RunPostToolUseFailureHooks 执行工具执行失败（Go-error 级）hooks。
// 与 PostToolUse 区分：PostToolUse 观察工具结果（含业务错误），
// PostToolUseFailure 观察工具抛出异常/ panic 等执行失败。
func (r *Runner) RunPostToolUseFailureHooks(toolName string, input json.RawMessage, errMsg string) error {
	hooks := r.findHooks(types.HookEventPostToolUseFailure, toolName)
	if len(hooks) == 0 {
		return nil
	}
	hookInput := types.HookInput{
		Event:       types.HookEventPostToolUseFailure,
		SessionID:   r.sessionID,
		ToolName:    toolName,
		ToolInput:   input,
		ErrorMessage: errMsg,
		IsError:     true,
	}
	for _, h := range hooks {
		_, _ = r.executeHook(h, hookInput)
	}
	return nil
}

// executeMessageHooksWithDecision 决策感知执行框架。
// 按配置顺序逐个执行 hooks，遇到第一个返回非空 Decision 或 ContinueDecision 的 hook 即停止并返回。
// 适用于需要同步决策干预的场景（PreCompact、PreRequest、OnContextOverflow、OnMaxTurnsReached 等）。
func (r *Runner) executeMessageHooksWithDecision(event types.HookEvent, messages []types.Message, base types.HookInput) *types.HookOutput {
	hooks := r.findHooks(event, "")
	if len(hooks) == 0 {
		return nil
	}
	in := base
	in.Event = event
	in.SessionID = r.sessionID
	if messages != nil {
		in.Messages = messages
	}
	for _, h := range hooks {
		output, err := r.executeHook(h, in)
		if err != nil || output == nil {
			continue
		}
		// 只要有明确的决策字段就返回（block/deny/approve）
		if output.Decision != "" || output.ContinueDecision != "" {
			return output
		}
	}
	return nil
}

// findHooks 查找匹配事件与 If 条件的 hooks
func (r *Runner) findHooks(event types.HookEvent, toolName string) []types.HookConfig {
	var result []types.HookConfig
	for _, h := range r.configs {
		if h.Event != event {
			continue
		}
		if !matchHookIf(toolName, h.If) {
			continue
		}
		result = append(result, h)
	}
	return result
}

func matchHookIf(toolName, ifPattern string) bool {
	if ifPattern == "" {
		return true
	}
	if toolName == "" {
		return false
	}
	ok, err := path.Match(ifPattern, toolName)
	return err == nil && ok
}

func effectiveHookType(h types.HookConfig) types.HookType {
	if h.HookType == "" {
		return types.HookTypeCommand
	}
	return h.HookType
}

// executeHook 执行单个 hook（command / prompt / http / mcp / plugin / opa / function / grpc）。
func (r *Runner) executeHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	switch effectiveHookType(config) {
	case types.HookTypePrompt:
		text := strings.TrimSpace(config.Command)
		if text == "" {
			return nil, nil
		}
		return &types.HookOutput{AdditionalContext: text}, nil
	case types.HookTypeHTTP:
		return r.executeHTTPHook(config, input)
	case types.HookTypeMCP:
		return r.executeMCPHook(config, input)
	case types.HookTypePlugin:
		return r.executePluginHook(config, input)
	case types.HookTypeOPA:
		return r.executeOPAHook(config, input)
	case types.HookTypeFunction:
		return r.executeFunctionHook(config, input)
	case types.HookTypeGRPC:
		return r.executeGRPCHook(config, input)
	default:
		return r.executeCommandHook(config, input)
	}
}

func (r *Runner) executeHTTPHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	urlStr := strings.TrimSpace(config.URL)
	if urlStr == "" {
		urlStr = strings.TrimSpace(config.Command)
	}
	if urlStr == "" {
		return nil, fmt.Errorf("http hook: empty url")
	}

	timeout := r.timeout
	if config.Timeout > 0 {
		timeout = time.Duration(config.Timeout) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("http hook: marshal input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("http hook: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http hook: do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http hook: read body: %w", err)
	}

	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '{' {
		var hookOutput types.HookOutput
		if err := json.Unmarshal(body, &hookOutput); err == nil {
			return &hookOutput, nil
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http hook: status %s: %s", resp.Status, string(body))
	}

	return nil, nil
}

// executeCommandHook 通过 shell 子进程执行 hook。
// 对应 TS: utils/hooks.ts 中的 execCommandHook()
//
// 退出码处理 (对应 TS: hooks.ts ~2647-2666):
//   - exit 0: 正常完成, 解析 stdout JSON
//   - exit 2: blocking (阻止工具执行), 使用 stderr 作为原因
//   - 其他: 忽略错误, 不阻止工具执行
func (r *Runner) executeCommandHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	timeout := r.timeout
	if config.Timeout > 0 {
		timeout = time.Duration(config.Timeout) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化 hook 输入失败: %w", err)
	}

	result, runErr := sandbox.DefaultManager().Run(ctx, sandbox.CommandSpec{
		Purpose:             "hook",
		Runtime:             "process",
		AllowUnsafeFallback: true,
		Args:                []string{"sh", "-c", config.Command},
		Stdin:               inputJSON,
		Limits: sandbox.ResourceLimits{
			Timeout:         timeout,
			OutputMaxBytes:  512 * 1024,
			PreviewMaxBytes: 128 * 1024,
			LogMaxBytes:     1024 * 1024,
		},
	})
	stdoutPreview := ""
	stderrPreview := ""
	if result != nil {
		stdoutPreview = result.StdoutPreview
		stderrPreview = result.StderrPreview
	}

	output := bytes.TrimSpace([]byte(stdoutPreview))
	if len(output) > 0 && output[0] == '{' {
		var hookOutput types.HookOutput
		if err := json.Unmarshal(output, &hookOutput); err == nil {
			return &hookOutput, nil
		}
	}

	if runErr != nil {
		if result != nil && result.ExitCode == 2 {
			reason := strings.TrimSpace(stderrPreview)
			if reason == "" {
				reason = "Hook exited with code 2 (blocking)"
			}
			return &types.HookOutput{
				Decision: "block",
				Reason:   reason,
			}, nil
		}
		return nil, fmt.Errorf("hook 执行失败: %w", runErr)
	}

	return nil, nil
}

// ============================================================================
// 扩展 Hook 执行器: MCP / OPA / Function / gRPC
// Plugin 见 hooks_plugin.go (build tag: linux || freebsd || darwin)
// ============================================================================

// mcpJSONRPCRequest MCP JSON-RPC 请求体
type mcpJSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
}

// mcpJSONRPCResponse MCP JSON-RPC 响应体
type mcpJSONRPCResponse struct {
	Result *mcpToolCallResult `json:"result,omitempty"`
	Error  *mcpJSONRPCError   `json:"error,omitempty"`
	ID     int                `json:"id"`
}

// mcpToolCallResult MCP tools/call 结果
type mcpToolCallResult struct {
	Content []mcpContentItem `json:"content"`
}

// mcpContentItem MCP 内容项
type mcpContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// mcpJSONRPCError MCP JSON-RPC 错误
type mcpJSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// executeMCPHook 通过 HTTP JSON-RPC 调用 MCP 工具。
// 配置: URL = MCP 服务端点, MCPTool = 工具名（Command 作为 MCPTool 回退）。
func (r *Runner) executeMCPHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	urlStr := strings.TrimSpace(config.URL)
	if urlStr == "" {
		return nil, fmt.Errorf("mcp hook: empty url")
	}

	toolName := strings.TrimSpace(config.MCPTool)
	if toolName == "" {
		toolName = strings.TrimSpace(config.Command)
	}
	if toolName == "" {
		return nil, fmt.Errorf("mcp hook: empty tool name")
	}

	timeout := r.timeout
	if config.Timeout > 0 {
		timeout = time.Duration(config.Timeout) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 将 HookInput 转为 map 以作为 arguments
	inputJSON, _ := json.Marshal(input)
	var args map[string]interface{}
	_ = json.Unmarshal(inputJSON, &args)

	reqBody, err := json.Marshal(mcpJSONRPCRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: map[string]interface{}{
			"name":      toolName,
			"arguments": args,
		},
		ID: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp hook: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("mcp hook: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp hook: do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mcp hook: read body: %w", err)
	}

	// 优先按 JSON-RPC 响应解析
	var rpcResp mcpJSONRPCResponse
	if err := json.Unmarshal(body, &rpcResp); err == nil {
		if rpcResp.Error != nil {
			return &types.HookOutput{
				Decision: "deny",
				Reason:   fmt.Sprintf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message),
			}, nil
		}
		if rpcResp.Result != nil && len(rpcResp.Result.Content) > 0 {
			text := rpcResp.Result.Content[0].Text
			var hookOut types.HookOutput
			if err := json.Unmarshal([]byte(text), &hookOut); err == nil {
				return &hookOut, nil
			}
			return &types.HookOutput{AdditionalContext: text}, nil
		}
	}

	// 回退: 直接解析为 HookOutput
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '{' {
		var hookOut types.HookOutput
		if err := json.Unmarshal(body, &hookOut); err == nil {
			return &hookOut, nil
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp hook: status %s: %s", resp.Status, string(body))
	}

	return nil, nil
}

// executeOPAHook 通过 shell 调用 `opa eval` 执行 Rego 策略。
// 配置: OPAPolicy = Rego 文件路径（Command 作为回退）, OPAQuery = 查询表达式。
func (r *Runner) executeOPAHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	policyFile := strings.TrimSpace(config.OPAPolicy)
	if policyFile == "" {
		policyFile = strings.TrimSpace(config.Command)
	}
	if policyFile == "" {
		return nil, fmt.Errorf("opa hook: empty policy")
	}

	query := strings.TrimSpace(config.OPAQuery)
	if query == "" {
		query = "data.hook.allow"
	}

	timeout := r.timeout
	if config.Timeout > 0 {
		timeout = time.Duration(config.Timeout) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("opa hook: marshal input: %w", err)
	}

	// 将输入写入临时文件
	tmpFile, err := os.CreateTemp("", "opa-input-*.json")
	if err != nil {
		return nil, fmt.Errorf("opa hook: create temp: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(inputJSON); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("opa hook: write temp: %w", err)
	}
	tmpFile.Close()

	cmd := exec.CommandContext(ctx, "opa", "eval", "--data", policyFile, "--input", tmpFile.Name(), query)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("opa hook: eval failed: %s", string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("opa hook: eval: %w", err)
	}

	// 解析 OPA 输出
	var opaOut struct {
		Result []struct {
			Expressions []struct {
				Value interface{} `json:"value"`
			} `json:"expressions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &opaOut); err != nil {
		return nil, fmt.Errorf("opa hook: parse output: %w", err)
	}

	if len(opaOut.Result) > 0 && len(opaOut.Result[0].Expressions) > 0 {
		val := opaOut.Result[0].Expressions[0].Value

		// 布尔结果
		if allow, ok := val.(bool); ok {
			if !allow {
				return &types.HookOutput{Decision: "deny", Reason: "OPA policy denied"}, nil
			}
			return &types.HookOutput{Decision: "approve"}, nil
		}

		// 对象结果
		if m, ok := val.(map[string]interface{}); ok {
			hookOut := types.HookOutput{}
			if d, ok := m["decision"].(string); ok {
				hookOut.Decision = d
			}
			if reason, ok := m["reason"].(string); ok {
				hookOut.Reason = reason
			}
			if ctx, ok := m["additional_context"].(string); ok {
				hookOut.AdditionalContext = ctx
			}
			if hookOut.Decision == "" {
				// 默认对象存在即 allow
				hookOut.Decision = "approve"
			}
			return &hookOut, nil
		}
	}

	return nil, nil
}

// executeFunctionHook 通过 HTTP 调用函数端点（Function-as-a-Service 风格）。
// 配置: URL = 端点地址, FunctionName = 函数名（Command 作为回退）。
func (r *Runner) executeFunctionHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	urlStr := strings.TrimSpace(config.URL)
	if urlStr == "" {
		return nil, fmt.Errorf("function hook: empty url")
	}

	funcName := strings.TrimSpace(config.FunctionName)
	if funcName == "" {
		funcName = strings.TrimSpace(config.Command)
	}

	timeout := r.timeout
	if config.Timeout > 0 {
		timeout = time.Duration(config.Timeout) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	payload, err := json.Marshal(map[string]interface{}{
		"function": funcName,
		"input":    input,
	})
	if err != nil {
		return nil, fmt.Errorf("function hook: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("function hook: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("function hook: do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("function hook: read body: %w", err)
	}

	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '{' {
		var hookOutput types.HookOutput
		if err := json.Unmarshal(body, &hookOutput); err == nil {
			return &hookOutput, nil
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("function hook: status %s: %s", resp.Status, string(body))
	}

	return nil, nil
}

// 确保 Runner 实现 tool.HookRunner 接口
var _ tool.HookRunner = (*Runner)(nil)
