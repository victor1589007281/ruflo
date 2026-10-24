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

	// cfgMu 保护**组**那三件 (groups/groupOrder) 与 configs 的读取。
	//
	// configs 本身构造后**永不变** (这正是它此前不必加锁的原因), 它一并放进锁里
	// 是为了让 findHooks/Count/Describe 读到的是一个**一致的两层视图** ——
	// 只锁组、不锁基线的话, "基线 + 组" 这个整体在读的那一瞬间仍可能半新半旧。
	//
	// 为什么非有锁不可: 工具调用是**并发批次** (pkg/tool/orchestration.go 的
	// runConcurrentBatch), 每个分支都调 findHooks; 而 pool_load 会在另一个
	// goroutine 上改装/卸组。map 的并发读写会直接 fatal, 不是"偶发错误"。
	cfgMu sync.RWMutex
	// groups 运行期装入的 hook 组 (21.8.12 P3「hook 进池」), 键 = 资产名。
	//
	// 与 configs 分层而不是合并进同一个切片: 基线来自操作员配置, 组来自模型经
	// pool_load 的请求 —— 来源与权限都不同。混一个切片之后, "把模型装的东西全卸掉"
	// 没有下手处 (而那是会话结束/治理回退唯一要走的一步)。
	groups map[string][]types.HookConfig
	// groupOrder 组名装入序。执行顺序是**语义** (同一事件上多条 hook 谁先 deny
	// 会改变结果), 所以它必须是稳定序而不是 map 遍历序。
	groupOrder []string

	// obsCtx 观测 ctx: 仅用于取出 Observer 把事件投到 Hook 总线 (见 bus.go)。
	// nil = 不观测, 与改造前逐字节等价。**不用于取消/超时** —— 各 hook 执行自带
	// 独立的 timeout ctx, 把这个 ctx 接进去会让"总线观测"意外变成一条取消路径。
	obsCtx context.Context

	postMu sync.RWMutex
	// PostSamplingCallbacks 在 ExecutePostSamplingHooks 中依次调用；请用 RegisterPostSamplingHook 注册以保证并发安全。
	PostSamplingCallbacks []func(messages []types.Message)
}

// NewRunner 创建 hook 执行器 (不观测: 事件不进 Hook 总线)。
func NewRunner(configs []types.HookConfig, sessionID string) *Runner {
	return &Runner{
		configs:   configs,
		sessionID: sessionID,
		timeout:   10 * time.Second,
	}
}

// NewRunnerWithContext 创建带**观测 ctx** 的 hook 执行器
// (design/01 §4.5 `ExternalHook` 适配器, 见 bus.go)。
//
// ctx 里若挂了 Observer (WithObserver), 本 Runner 触发的每个**真的匹配到配置**的
// hook 事件会投一条到 Hook 总线。没挂 Observer / 没配 hook ⇒ 行为与 NewRunner 一致。
//
// 只在**per-run 构造**的 Runner 上用它 (生产是 sessionAgentRunner.Execute 里那个)。
// 长期复用的 Runner 传 ctx 会串台, 理由见 bus.go 边界 2。
func NewRunnerWithContext(ctx context.Context, configs []types.HookConfig, sessionID string) *Runner {
	r := NewRunner(configs, sessionID)
	r.obsCtx = ctx
	return r
}

// MergeConfigs 返回一个**新** Runner: 本 Runner 的基线配置 + extra (extra 排在后)。
//
// ---------------------------------------------------------------------------
// 为什么需要它: 追加配置此前只有"调用方自己拼一份"这一条路
// ---------------------------------------------------------------------------
//
// 在它出现之前, "在既有 hook 之上再追加几条"这件事是调用方各自拼的 ——
// pkg/feishu/session.go 与 cmd/claude-go/main.go 的嵌套子代理路径都是
// `merged := append(基线, 新增...)` 再 NewRunnerWithContext。那份基线之所以拼得出来,
// 只因为两条路径**顺手持有**了构造 Runner 用的 configs (engineDeps.hookConfigs /
// sm.hookConfigs)。而 StageDef.Hooks (21.8.12 P4/X1) 的第三个消费点 (CLI 的
// cliAgentRunner) 手上只有 *engine.QueryEngine, 没有那份 configs —— Runner 构造完
// 就把入参丢掉了, 没人答得出"它现在挂着哪些配置"。
//
// 三条出路: ① 让调用方也能拿到基线 configs (等于把"谁持有配置"这件事再复制一遍,
// 且要求每个构造点都记得并排存一份); ② 在 cliAgentRunner 里另起一个 Runner 只装
// 阶段 hook (结果是同一轮的工具调用要问两个 Runner, 而**先问谁**决定了 deny 的
// 赢家 —— 一份 deny 语义被拆到两处); ③ 让 Runner 自己会合并。取 ③。
//
// ---------------------------------------------------------------------------
// 为什么返回新 Runner 而不是就地装入一个 hook 组 (InstallHookGroup)
// ---------------------------------------------------------------------------
//
// 组是**会话级**的生命周期: 装进去之后所有后续轮次都看得见, 直到 Uninstall。
// 阶段声明的 hook 生命周期是**一次 Execute**, 用完就该消失 —— 而 Execute 是可能
// 并发的 (cmd/claude-go 的 agentPool 是 8 并发, 各 worker 共享同一个 eng)。
// 用组来实现, 就必须给每次 Execute 编一个不会撞的名字, 并在所有返回路径上卸干净;
// 漏卸的那条路径留下的 hook 会挂到**别人**的轮次上, 而且没有任何读数看得出来。
//
// 返回新 Runner 则天然是每调用一份, 没有共享就没有泄漏。代价是每次多构造一个
// 几十字节的结构体 —— 换掉一整类"钩子串台"的失效形态, 划算。
//
// ★ 顺序是语义: extra 排在基线**之后**。同一事件上多条 hook 谁先 deny 会改变结果,
// 而基线的口径是"部署方/操作员配置" —— 作者的阶段声明不该抢在它前面 (与 pkg/feishu
// 三段来源的排列一致)。
func (r *Runner) MergeConfigs(extra []types.HookConfig) *Runner {
	if r == nil {
		if len(extra) == 0 {
			return nil
		}
		return NewRunner(extra, "")
	}
	out := &Runner{
		sessionID: r.sessionID,
		timeout:   r.timeout,
		obsCtx:    r.obsCtx,
	}
	if len(extra) == 0 {
		// 空追加重放一行没有任何好处, 但**必须**返回一个独立的 Runner 而不是 r:
		// 调用方拿到的要么是"基线 + 阶段", 要么就是基线本身, 不该有一条路径
		// 悄悄把共享对象漏出去 —— 那会让"这次执行挂了什么"重新取决于别人。
		out.configs = append([]types.HookConfig(nil), r.configs...)
		return out
	}
	out.configs = make([]types.HookConfig, 0, len(r.configs)+len(extra))
	out.configs = append(out.configs, r.configs...)
	out.configs = append(out.configs, extra...)
	return out
}

// HookSummary 是 Runner 上一份 hook 配置的**可回读快照**, 供装配自报使用。
//
// 存在的理由: 到改造为止, "这次运行到底挂了哪些外部 hook"只活在 NewRunner 的入参
// 里 —— 进程起来之后没人能回答, 配置里写了什么与实际装了什么是两件事 (CLI 与飞书
// 两条路径装出来的还不一样)。装配对账要的就是这一份"实际"。
//
// **只报名称与形状, 不报命令原文与 URL**: hook 的 command/env 里常有凭据与内网
// 地址, 而装配报告是要落盘、要展示到面板上的东西。要看原文应当去看配置文件本身。
type HookSummary struct {
	// Event 事件名 (如 PreToolUse / PostTurn)。
	Event string `json:"event"`
	// Type 生效的 hook 类型; 配置里留空时是 command (与 effectiveHookType 同源)。
	Type string `json:"type"`
	// Index 在配置列表里的下标 —— 同一事件上挂多条同名 hook 时唯一的区分手段。
	Index int `json:"index"`
	// HasCondition 是否带 If 条件 (带条件的 hook 只在部分调用上触发, 统计时不可比)。
	HasCondition bool `json:"has_condition,omitempty"`
	// Timeout 秒; 0 = 用默认值 (10s)。
	Timeout int `json:"timeout,omitempty"`
	// Group 这条 hook 来自哪个运行期装入的组 (P3 池化 hook); 空 = 基线配置。
	//
	// 为什么要报出来: 基线是操作员给的, 组是模型经 pool_load 装上来的 —— 两者在
	// 治理上不是一回事 (回退时该卸的只有后者)。读数里分不出来, "这条 hook 谁装的"
	// 就只能靠人回忆。
	Group string `json:"group,omitempty"`
}

// Describe 返回**生效面** hook 配置的快照 (基线在前, 运行期装入的组按装入序在后)。
//
// 走 describeAll: 它是唯一一处把两层拼起来的地方。曾经这里直接遍历 r.configs —
// 那样 pool_load 装上的 hook 在装配报告里**一条都看不到**, 而报告正是"声明有没有
// 落地"的唯一出口: 装上了却不在 Describe 里, 与没装上的报告长得一样。
func (r *Runner) Describe() []HookSummary {
	return r.describeAll()
}

// Count 返回**生效面**的 hook 配置条数 (基线 + 运行期装入的组)。
//
// 与 BaselineCount 的分工见 BaselineCount 的注释: 这一个回答"一共挂了几条",
// 那一个回答"配置/装配给了几条"。
func (r *Runner) Count() int {
	if r == nil {
		return 0
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	n := len(r.configs)
	for _, g := range r.groups {
		n += len(g)
	}
	return n
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

	// 总线观测 (design/01 §4.5): 一次事件只发一条, 相位=事件名原文。用 defer 是因为
	// 下面有三条 return 出口 (block / approve / 收尾), 逐条手写 emit 必漏一条 ——
	// 漏掉的恰好会是 block 那条 (最该被看见的干预)。
	ev := Event{Scope: ScopeTool, Phase: string(types.HookEventPreToolUse),
		Matched: len(hooks), Tool: toolName}
	defer func() { r.observe(ev) }()

	var contexts []string
	for _, h := range hooks {
		output, err := r.executeHook(h, hookInput)
		if err != nil {
			if ev.Err == "" {
				ev.Err = err.Error() // 只记第一条: 改造前这里 continue 静默吞掉全部 hook 报错
			}
			continue
		}
		if output != nil {
			// Decision 语义: "deny" / "block" = 阻止; "approve" = 显式放行并跳过剩余 hook
			if output.Decision == "deny" || output.Decision == "block" {
				ev.Decision = output.Decision
				return output, nil
			}
			if output.Decision == "approve" {
				ev.Decision = output.Decision
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

	ev := Event{Scope: ScopeTool, Phase: string(types.HookEventPostToolUse),
		Matched: len(hooks), Tool: toolName}
	for _, h := range hooks {
		if _, err := r.executeHook(h, hookInput); err != nil && ev.Err == "" {
			ev.Err = err.Error()
		}
	}
	r.observe(ev)
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

// executeStopLikeHooks 是 Stop / StopFailure 两条事件共用的执行体: 依次跑匹配到的 hook, 把
// deny/block 的裁决翻译成"注入一条用户消息"从而让模型继续处理。
//
// 算法解读：
//
//	原理：Stop 类 hook 的产物不是"允许/禁止"而是**续跑信号** —— 命中 deny/block 时把 hook 给的
//	      原因包成一条 IsMeta 的用户消息塞回对话, 模型看到它就会接着干活; 没有任何 hook 阻止时
//	      返回 nil, 对话正常结束。
//	关键不变量：approve 必须**立刻返回 nil** (而不是记下决定继续循环)。approve 的语义是"这次
//	      停止是合法的", 继续跑后面的 hook 会让一个显式放行被另一个 hook 的 block 翻掉。
//	代价与边界：hook 串行执行, 每个都可能是一次子进程/HTTP 往返, 总耗时是各 hook 之和;
//	      单个 hook 报错只记录 (ev.Err) 并跳过, 不中断整条链 —— 观测失败不该阻断对话。
//	设计权衡：只保留最后一条非 approve 的 Decision 供观测, 而阻塞消息则全部累积 —— 观测要的是
//	      "最终裁决是什么", 而恢复消息是给模型的输入, 少一条就等于少一条线索。
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

	ev := Event{Scope: ScopeOf(event), Phase: string(event), Matched: len(hooks)}
	defer func() { r.observe(ev) }()

	var blockingMessages []types.Message
	for _, h := range hooks {
		output, err := r.executeHook(h, hookInput)
		if err != nil {
			if ev.Err == "" {
				ev.Err = err.Error()
			}
			continue
		}
		if output != nil {
			// ContinueDecision 语义: "deny" / "block" = 阻止并注入恢复消息; "approve" = 不阻止
			// approve 立即放行并结束整条链 —— 后面的 hook 无权翻掉一个显式的"允许停止"。
			if output.ContinueDecision == "approve" {
				ev.Decision = output.ContinueDecision
				return nil
			}
			if output.ContinueDecision == "deny" || output.ContinueDecision == "block" {
				ev.Decision = output.ContinueDecision
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
		Event:        types.HookEventPostToolUseFailure,
		SessionID:    r.sessionID,
		ToolName:     toolName,
		ToolInput:    input,
		ErrorMessage: errMsg,
		IsError:      true,
	}
	ev := Event{Scope: ScopeTool, Phase: string(types.HookEventPostToolUseFailure),
		Matched: len(hooks), Tool: toolName}
	for _, h := range hooks {
		if _, err := r.executeHook(h, hookInput); err != nil && ev.Err == "" {
			ev.Err = err.Error()
		}
	}
	r.observe(ev)
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
	// 这是 20+ 个事件 (SubagentStart/SessionStart/PreCompact/OnError/TeammateIdle...)
	// 的共同漏斗, 所以总线接线只需一处 —— 逐个 Execute*Hooks 方法各接一次的话,
	// 将来新增一个事件必然忘记接, 表现为"总线上少一类事件"且无人察觉。
	ev := Event{Scope: ScopeOf(event), Phase: string(event), Matched: len(hooks),
		Role: base.Role}
	defer func() { r.observe(ev) }()
	for _, h := range hooks {
		output, err := r.executeHook(h, in)
		if err != nil {
			if ev.Err == "" {
				ev.Err = err.Error()
			}
			continue
		}
		if output == nil {
			continue
		}
		// 只要有明确的决策字段就返回（block/deny/approve）
		if output.Decision != "" || output.ContinueDecision != "" {
			ev.Decision = decisionOf(output)
			return output
		}
	}
	return nil
}

// findHooks 查找匹配事件与 If 条件的 hooks。基线在前、运行期装入的组按装入序在后。
//
// ★ 这是"hook 进池"**唯一**的行为落点: pool_load 一条 hook 之后到底生不生效,
// 取决于这个函数看不看得见组。看不见的表现是——池里列着它、加载回"已加载"、
// 装配读数 wired、而对这条 hook 的执行**一次都没有**发生 (与"没人配它"同形)。
//
// 锁只包住"取匹配列表"这一段, 返回值是**拷贝**: executeHook (可能 spawn 进程 /
// 发 HTTP) 全部发生在锁外。把执行放进锁里, 一条慢 hook 会阻塞所有并发工具调用,
// 那是比数据竞争更难查的性能故障。
func (r *Runner) findHooks(event types.HookEvent, toolName string) []types.HookConfig {
	if r == nil {
		return nil
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	var result []types.HookConfig
	appendMatches := func(cfgs []types.HookConfig) {
		for _, h := range cfgs {
			if h.Event != event {
				continue
			}
			if !matchHookIf(toolName, h.If) {
				continue
			}
			result = append(result, h)
		}
	}
	appendMatches(r.configs)
	for _, name := range r.groupOrder {
		appendMatches(r.groups[name])
	}
	return result
}

// matchHookIf 用 path.Match 通配符判定 hook 的 if 条件是否匹配当前工具名; 模式为空表示无条件匹配。
//
// 注意: toolName 为空 (非工具事件) 且模式非空时返回 false —— 否则 "Bash*" 这类模式会因为
// path.Match("Bash*", "") 的失败被误当成"该 hook 适用"。
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

// effectiveHookType 返回 hook 的实际类型; 未声明时默认为 command (与 TS 侧的默认一致)。
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

// executeHTTPHook 把 hook 输入 POST 到配置的 URL, 并按响应的形状决定是否产出裁决。
//
// 算法解读：
//
//	原理：hook 输入序列化成 JSON POST 出去; 响应体若以 `{` 开头且能反序列化成 HookOutput,
//	      就直接把它当作裁决返回 (内容协商靠**形状**而不是 Content-Type 或状态码)。
//	关键不变量：JSON 解析必须排在状态码检查**之前**。hook 服务用 4xx/5xx 携带结构化裁决是常见
//	      写法 (例如 403 + {"continue": false}), 先查状态码会把这类合法裁决当成传输错误丢掉,
//	      表现为"hook 明明拒绝了, 工具却照常执行"。
//	代价与边界：响应体全量读进内存, 无大小上限; 超时取 config.Timeout, 缺省用 Runner 级超时。
//	      非 JSON 响应且状态码非 2xx 才报错, 2xx 的非 JSON 响应一律当作"无裁决"返回 nil。
//	设计权衡：不做重试 —— hook 是同步前置闸门, 重试会把延迟直接叠加到每一次工具调用上;
//	      需要容错的 hook 应在自己那一侧做成幂等 + 自带重试。
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
