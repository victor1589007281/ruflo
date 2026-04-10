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
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
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
			if output.Decision == "block" {
				return output, nil
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
		if output != nil && output.ContinueDecision == "block" {
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
	return blockingMessages
}

// ExecuteSessionHooks 执行会话生命周期 hooks
func (r *Runner) ExecuteSessionHooks(event types.HookEvent) {
	hooks := r.findHooks(event, "")
	for _, h := range hooks {
		_, _ = r.executeHook(h, types.HookInput{
			Event:     event,
			SessionID: r.sessionID,
		})
	}
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

// executeHook 执行单个 hook（command / prompt / http）。
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

	cmd := exec.CommandContext(ctx, "sh", "-c", config.Command)

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("序列化 hook 输入失败: %w", err)
	}
	cmd.Stdin = bytes.NewReader(inputJSON)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	output := bytes.TrimSpace(stdout.Bytes())
	if len(output) > 0 && output[0] == '{' {
		var hookOutput types.HookOutput
		if err := json.Unmarshal(output, &hookOutput); err == nil {
			return &hookOutput, nil
		}
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 2 {
				reason := strings.TrimSpace(stderr.String())
				if reason == "" {
					reason = "Hook exited with code 2 (blocking)"
				}
				return &types.HookOutput{
					Decision: "block",
					Reason:   reason,
				}, nil
			}
		}
		return nil, fmt.Errorf("hook 执行失败: %w", runErr)
	}

	return nil, nil
}

// 确保 Runner 实现 tool.HookRunner 接口
var _ tool.HookRunner = (*Runner)(nil)
