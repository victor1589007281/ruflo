// runner.go - 独立的一次性 (one-shot) Agent 运行器.
//
// 背景:
//
//	cliAgentRunner.Execute (cmd/claude-go/main.go) 早期实现直接调 client.SendMessage,
//	绕过了 QueryEngine 的工具循环和 XML 回退解析. 当 LLM (MiniMax M2.x 等) 在文本里
//	塞 <minimax:tool_call> 或 [TOOL_CALL] 风格的工具调用时, Agent 实际什么也没做.
//
//	本文件提供一个轻量循环: 共享 QueryEngine 的工具注册表, 但消息历史完全独立 (不污染
//	QueryEngine.Messages), 既支持 Anthropic 原生 tool_use 协议也支持 XML/Bracket 回退,
//	适合 Team workflow 的每一个 stage 独立运行.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/trace"
	"github.com/anthropic/claude-go/pkg/types"
)

// IsolatedRunOptions 控制独立运行器行为.
type IsolatedRunOptions struct {
	// SystemPrompt 该 Agent 专属的 system prompt (会与 PromptMgr 默认 system 一起注入).
	SystemPrompt string
	// MaxTurns 最多循环多少轮, <=0 时使用 30.
	MaxTurns int
	// MaxTokens 单次请求最大输出 tokens, <=0 时使用 8192.
	MaxTokens int
	// Client 可选, 覆盖 engine 默认的 api.Client (用于按 role/plan 切换 provider).
	Client *api.Client
	// DisableTools 为 true 时:
	//   - 不向模型暴露任何工具 (apiTools=nil)
	//   - 即使模型在文本里塞了 <minimax:tool_call> 或 [TOOL_CALL] 也只清洗文本,
	//     不会真的执行工具 (extracted tool_uses 直接丢弃)
	//   - 整个循环最多跑 1 轮就返回
	// 适合 architect / researcher / planner 这类"思考型"角色, 它们的 system prompt
	// 明确禁止工具调用, 只需要产出最终设计文档/调研报告/JSON.
	DisableTools bool

	// Stage 本次运行的稳定阶段标识, 用于中间态持久化的身份 (H3-2)。
	//
	// 由编排层填 (团队执行时是 stage 名), **不要填运行时生成的值** ——
	// 那会让恢复所需的键在重启后失配, 而失配是静默的 (见 runner_state.go 的
	// RunKey)。留空合法 (CLI 单跑没有 stage), 此时身份退化为 Role+Cwd。
	Stage string

	// Hooks 本次运行**额外**要挂的外部 hook (21.8.12 P4/X1 的消费方)。
	//
	// 由编排层把 StageDef.Hooks 解析好之后填进来 (解析走 hooks.ParseHookDecls,
	// 与资产 hooks: 段同一份实现)。空 = 逐字节走改造前的行为。
	//
	// ★ 为什么是"追加"而不是"替换"引擎既有的 HookRunner: 本路径上真正该执行的
	// 是"部署方配的 hook"与"作者在 stage 上声明的 hook"的**并集** —— 替换会让
	// 平台侧的审计/门禁 hook 在某条执行路径上静默消失, 而那正是最不该消失的一类。
	// 合并规则与优先级见 hooks.Runner.MergeConfigs。
	//
	// ★ 为什么走 opts 而不是在 Runner 上装一个 hook 组: 组是会话级生命周期, 而
	// 这里的声明是**一次运行**的。Execute 是可能并发的 (agentPool 8 并发共享同一个
	// eng), 装组就必须给每次运行编一个不撞的名字并在每条返回路径上卸干净, 漏卸的
	// 那一处会把作者的 hook 挂到别人的轮次上且无读数可见。
	Hooks []types.HookConfig

	// Inject 本次运行的**入站队列** (21.8.12 P5 / D6-3 缺口 2 级 2)。nil = 不接注入,
	// 逐字节走改造前的行为。
	//
	// ★ 为什么由调用方传入而不是从引擎上取: 这条路径是**并发**的 (CLI 团队路径下
	// agentPool 8 并发共用同一个 *QueryEngine)。队列挂在引擎上时, "这条消息是给哪一次
	// run 的"没有答案 —— 表现是 A 阶段的更正被 B 阶段收走, 而两边都看不出异常 (收到的
	// 那个会当真, 没收到的那个以为没人给它发信)。队列的身份因此是 **run**。
	//
	// 与 TaskInstruction 那类"由编排层填"的字段同一处置: 谁知道自己是谁, 谁负责填。
	Inject *InjectQueue
}

// RunIsolated 跑一次独立的 Agent 循环, 共享 QueryEngine 的工具集合,
// 但完全独立的消息历史, 适合 team workflow 的每个 stage.
//
// 行为:
//  1. 用 SystemPrompt + userPrompt 构造起始消息.
//  2. 进入循环: 调 client.SendMessage → 解析响应 → 处理 tool_use → 把 result 追加, 再调用.
//  3. 终止条件: 模型不再返回 tool_use, 或达到 MaxTurns, 或 ctx 取消.
//  4. 返回最终 assistant 的文本内容 (合并所有 text content block).
//
// 与 QueryEngine.queryLoop 的区别:
//   - 不持久化 SessionStore / Trajectory
//   - 中间态落盘走自己的机制 (见 runner_state.go), 不是 SessionStore
//   - 但仍跑 internal_hook.MergeXMLToolCalls 回退 + RunTools, 这是核心功能.
//
// H3-2 起**不再豁免压缩与持久化**: 这条路径上常态就是上百轮 (cliAgentRunner 给
// 做事型角色 150 turns), 原来那两条豁免 (见下) 对应的假设 "stage 输出量适中"
// 在团队执行路径上不成立。两条豁免的实际后果是"跑得越久的 run 越会撞死在 PTL
// 上, 且进程一死全部归零" —— 正是长程任务最不该有的两个性质。
//
//	e.Compactor 非 nil 时, 每轮进入请求前折叠一次 (micro + fold, 阈值同主循环);
//	e.SetRunStateDir 装配过时, 每轮落一次中间态, 启动时按稳定身份恢复上次中断处。
func (e *QueryEngine) RunIsolated(ctx context.Context, userPrompt string, opts IsolatedRunOptions) (finalText string, finalErr error) {
	if e == nil {
		return "", fmt.Errorf("nil engine")
	}
	// 轨迹采集 (design/03 §4.1 E1): RunIsolated 不经 HookChain, TraceCaptureHook
	// 不会触发。此处补一个 episode 级 turn span (输入 prompt → 最终产出), 使 CLI
	// 团队路径也进 TraceStore。tool_call 级细粒度采集属飞书完整循环路径 (已覆盖)。
	if e.TraceStore != nil {
		start := time.Now()
		ids := trace.From(ctx)
		defer func() {
			status := "completed"
			if finalErr != nil {
				status = "failed"
			}
			// F6 compaction 事件锁 (planning-dsh-adopt): node Span 与 llm_call Span
			// 同属折叠后才有意义的事件链, 经折叠窗口门落盘 (窗口内排队, 关闭后放行)。
			e.Compactor.GateEventWrite(func() {
				e.TraceStore.Write(tracestore.Span{
					TraceID:   ids.RunID,
					SpanID:    internal_hook.GenerateUUID(),
					Kind:      "node",
					Name:      ids.NodeID,
					NodeID:    ids.NodeID,
					TurnID:    ids.TurnID,
					InputRef:  e.TraceStore.MakeRef(userPrompt),
					OutputRef: e.TraceStore.MakeRef(finalText),
					Attrs:     map[string]any{"status": status, "isolated": true},
					TS:        start.UnixMilli(),
					DurMS:     time.Since(start).Milliseconds(),
				})
			})
		}()
	}
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = 30
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 8192
	}
	client := opts.Client
	if client == nil {
		client = e.APIClient
	}
	if client == nil {
		return "", fmt.Errorf("no api client configured")
	}

	// ===== H3-2 中间态: 身份 → 恢复 → 起始消息 =====
	//
	// 身份取自**编排层声明**的稳定字段 (Stage/Role/Team/Workflow/Cwd) 与
	// Config, 不取任何运行时生成的值 —— 见 RunKey。关掉持久化 (runStateDir 为空)
	// 时下面整段退化为"构造一条 user 消息", 与改造前逐字节相同。
	identity := RunIdentity{
		Team:     e.Config.Team,
		Workflow: e.Config.Workflow,
		Stage:    opts.Stage,
		Role:     e.Config.Role,
		Cwd:      e.Config.Cwd,
	}
	runKey := RunKey(identity)
	runState := e.newRunState(runKey, identity, userPrompt, opts.MaxTurns)

	// H3-4 并发写占用: 本次 run 占住"我写过的路径", 与它同处一个进程的另一个并行
	// run 若碰同一份文件就被显式拒绝, 而不是静默覆盖。生命周期**必须**覆盖整个循环
	// 而不是单次写入 —— 占用做成一瞬间的锁只能让写变原子, 挡不住"两个写者各自基于旧
	// 内容算新内容, 先后覆盖" (见 pkg/tool/writeclaim.go 的设计说明)。
	//
	// 释放挂在循环**退出之后**: 这条路径有 8 处提前 return, 逐个加一句会漏, 而漏掉的
	// 那一处留下的占用不会自行消失 —— 那条路径在本进程内就永久不可写了, 正是这里要
	// 防的"比静默覆盖更糟"的那种失效。
	claim := tool.BeginWriter(identity.Label())
	defer claim.Release()

	// 1. 构造起始消息列表 (独立, 不污染 e.Messages)
	//
	// 有可恢复的中间态时**以它为起点**, 而不是新起一条 user 消息 —— 那正是本能力
	// 的全部意义: 进程被杀后重启, 从断点继续而不是从零重来。
	messages := []types.Message{
		{
			Type: types.MessageTypeUser,
			UUID: internal_hook.GenerateUUID(),
			Content: []types.ContentBlock{
				{Type: types.ContentBlockText, Text: userPrompt},
			},
			CreatedAt: time.Now(),
		},
	}
	if prev := e.loadRunState(runKey, identity); prev != nil {
		// 把本次的输入接在恢复历史之后 (带一段说明, 见 resumeNotice)。
		messages = appendResumeNotice(append(append([]types.Message(nil), prev.Messages...), messages...))
		runState.Folds = prev.Folds
		runState.Resumes = prev.Resumes + 1
		runState.Messages = append([]types.Message(nil), prev.Messages...)
		logging.For("engine").Info("隔离运行从落盘中间态恢复",
			"key", runKey, "turn", prev.Turn, "messages", len(prev.Messages),
			"resumes", runState.Resumes, "age", time.Since(prev.UpdatedAt).Round(time.Second))
	} else if body := e.loadHandoff(runKey); body != "" {
		// H3-3: 没有可恢复的中间态, 但上一次同身份的 run 留下了交接说明 ——
		// 那是上一次**跑到了上限** (而非被杀) 留下的, 两者互斥, 见 runner_handoff.go。
		//
		// 位置在 messages **构造之后**: 它不替代本次的输入, 而是在输入之前垫一层
		// "上次做到哪"。少了这次输入, 模型会不知道自己这一趟该干什么。
		messages = append([]types.Message{handoffMessage(body)}, messages...)
		runState.HandoffUsed = true
		logging.For("engine").Info("本次执行采纳了上一次留下的交接说明",
			"key", runKey, "bytes", len(body))
	}

	// 终态落盘。放在这里而不是各 return 点上: 这条路径有 8 处提前返回, 逐个加
	// 一句"记终态"必然会漏 —— 而漏掉的那一处恰好是"被杀死/出错"这类最需要留痕的
	// 情况 (这正是验收方法论里的"状态更新只改内存, 直到最后一次落库")。
	// 用命名返回值读终态 (finalText / finalErr), 两者在 return 处已经被赋值。
	defer func() {
		if e.runStateDir == "" {
			return
		}
		e.finishRunState(runState, finalText, finalErr)
	}()

	// 2. 组装 system prompts: SystemPrompt > PromptMgr default
	// DisableTools=true 时 (思考型角色), 若已提供专属 system prompt,
	// 不再追加包含大量工具定义的默认 system prompt, 避免模型被无关工具描述干扰.
	var systemPrompts []string
	if opts.SystemPrompt != "" {
		systemPrompts = append(systemPrompts, opts.SystemPrompt)
	}
	if e.PromptMgr != nil && !(opts.DisableTools && opts.SystemPrompt != "") {
		def := e.PromptMgr.BuildEffectiveSystemPrompt(e.Tools)
		systemPrompts = append(systemPrompts, def...)
	}

	// 3. 准备 APITools (经 toolExposed 统一过滤: DisabledTools 黑名单 +
	// AllowedTools 白名单). DisableTools 时直接置空, 不向模型暴露任何工具描述,
	// 进一步降低 MiniMax 等模型乱产 XML 工具调用的概率.
	var apiTools []types.APITool
	if !opts.DisableTools && e.Tools != nil {
		all := e.Tools.APITools()
		for _, t := range all {
			if !e.Config.toolExposed(t.Name) {
				continue
			}
			apiTools = append(apiTools, t)
		}
	}

	// 4. 循环
	//
	// hook Runner 在循环外**算一次**: 阶段声明的 hook 并入引擎既有的基线 (见
	// IsolatedRunOptions.Hooks 与 hooks.Runner.MergeConfigs 的论证)。空声明时
	// 直接用 e.HookRunner, 连一次拷贝都不做 —— 改造前是它、现在还是它。
	hookRunner := e.HookRunner
	if len(opts.Hooks) > 0 {
		hookRunner = hookRunner.MergeConfigs(opts.Hooks)
		if n := len(opts.Hooks); n > 0 {
			logging.Event(ctx, "stage.hooks_merged",
				"stage", opts.Stage, "declared", n, "total", hookRunner.Count())
		}
	}
	var lastAssistantText string
	// 运行中入站通道 (21.8.12 P5): 标记"这次 run 能接注入"。
	//
	// Enter/Leave 配平用 defer —— 这条路径有 8 处提前 return, 手写必然漏掉几条,
	// 而漏掉的后果是**队列永久认为有人接**: 之后每条注入都被收下、永远不被消费,
	// 没有任何信号 (与 queryLoop 那处同一条理由)。
	opts.Inject.Enter()
	defer opts.Inject.Leave()
	for turn := 0; turn < opts.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return lastAssistantText, ctx.Err()
		}

		// 运行中注入**必须**发生在压缩之前: 一条更正可能正是"别再按原来的方向做",
		// 而压缩会把旧方向折成摘要 ⇒ 模型先看到摘要、再看到更正, 反而更容易顺着
		// 旧方向继续。排在循环顶部还有第二个好处 —— 本轮新增的区域尚未发出, 已发出
		// 的前缀一个字节不动 (与 queryLoop 那处同口径, 前缀缓存安全)。
		//
		// 位置在 MaxTurns 计数**之内**: 注入不额外送一轮, 它搭下一个轮次的便车。
		// 一个已经跑满的 run 收到注入时不会被延长 —— 那是止损线的语义, 不该由一条
		// 外部消息改写。
		for _, m := range opts.Inject.Drain() {
			logging.For("engine").Info("运行中注入 user 消息 (隔离运行)",
				"turn", turn, "stage", opts.Stage, "len", len(m.Content))
			messages = append(messages, m)
		}

		// H3-2 折叠: 与主循环 PhasePreCompact 同序 (micro → fold)。放在**请求之前**
		// 是唯一正确的位置 —— 目的就是别让这一次请求撞上 prompt-too-long。
		//
		// 这里**不**再写 `!opts.DisableTools &&`: 那条曾经写着"单轮思考型运行不折,
		// 显式跳过省一次估算"。但它从来没有改变过任何行为 —— DisableTools 的 run
		// 只有 1 条消息 (永远走不到工具分支), 而 compactIsolated 的消息数下限
		// (isolatedFoldMinMessages) 早就把它挡住了。变异测试证实: 把它改成恒真,
		// 全部用例照样全绿。
		//
		// 一个永远为真的条件不是"保险", 是**假的安全感**: 它让读代码的人以为
		// DisableTools 这条路有专门的保护, 而实际上保护来自另一个常量; 哪天那个
		// 常量被调小, 这里不会跟着变, 而没有任何一处会报错。
		if e.Compactor != nil {
			if folded, ok := e.compactIsolated(ctx, messages, client.Model); ok {
				messages = folded
				runState.Folds++
			}
		}

		apiMessages := messagesToAPI(messages, nil)

		// Path B 相位路由 (与主循环 queryLoop 同口径): 工具消费回合
		// (尾部是 tool_result 的 user 消息) 路由到配置的快速执行模型。
		// RunIsolated 是团队 stage 的执行路径, 此前单模型 client 直调,
		// 配置了 ExecutionModel 也完全不生效 —— 操作型角色 (coder/tester/
		// reviewer) 的多轮工具循环全部跑在主模型上。补上后工具消费回合
		// 走 lfm 等快模型, 推理回合仍走主模型。
		//
		// Plan 模型路由优先于 ExecutionModel: 会话 plan flag ON (规划期) 时
		// 全程路由到独立 plan 客户端 (Tag=plan, 类似 advisor)。PlanClient=nil
		// 或 flag OFF 时此分支空转, 行为与旧版一致。
		turnClient := client
		if e.Config.PlanClient != nil && e.Config.DynamicPlanCheck != nil && e.Config.DynamicPlanCheck() {
			turnClient = e.Config.PlanClient
		} else if e.Config.ExecutionModel != "" && isToolExecTurn(messages) {
			turnClient = client.WithModel(e.Config.ExecutionModel)
		}

		callStart := time.Now()
		resp, err := turnClient.SendMessage(ctx, apiMessages, systemPrompts, apiTools, opts.MaxTokens)

		// 收集 assistant 的 content blocks, 区分 text / tool_use
		var (
			assistantBlocks []types.ContentBlock
			toolUseBlocks   []types.ContentBlock
		)
		if resp != nil {
			for _, blk := range resp.Content {
				b := blk
				if b.Type == types.ContentBlockToolUse {
					toolUseBlocks = append(toolUseBlocks, b)
				}
				assistantBlocks = append(assistantBlocks, b)
			}
		}

		// llm_call Span (design/03 §4.1 E1 第 4 种 Kind)。团队/stage 路径的绝大多数
		// LLM 流量走这里, 所以这一处比 queryLoop 那处更要紧。写在错误返回**之前**:
		// 失败的调用也是轨迹 (§1.3 要求能还原完整 prompt→response 链, 含失败)。
		if e.TraceStore != nil {
			var usage *types.Usage
			var stop string
			if resp != nil {
				usage, stop = resp.Usage, resp.StopReason
			}
			model := turnClient.Model // turnClient 已按 Path B 路由选好模型
			if resp != nil && resp.Model != "" {
				model = resp.Model // 网关可能换了实际服务模型, 以响应为准
			}
			e.writeLLMCallSpan(ctx, llmCallSpan{
				Start: callStart, Model: model, System: systemPrompts,
				Messages: apiMessages, Tools: apiTools,
				Output: internal_hook.JoinAssistantText(assistantBlocks),
				Usage:  usage, StopReason: stop, Err: err,
				Attempt: turn, Isolated: true,
			})
		}
		if err != nil {
			return lastAssistantText, err
		}
		if resp == nil {
			return lastAssistantText, fmt.Errorf("nil response")
		}

		// 5. XML / bracket 回退: 把文本里的工具调用补齐成 tool_use blocks; 即使
		// 没有提取出可执行的工具, internal_hook.MergeXMLToolCalls 也会把 <minimax:tool_call>
		// 等噪声标签从 text 块里剥离, 这一步必须早于 lastAssistantText 计算.
		mergedAssistant, mergedToolUse, _ := internal_hook.MergeXMLToolCalls(assistantBlocks, toolUseBlocks, fmt.Sprintf("iso_t%d", turn))
		assistantBlocks = mergedAssistant
		toolUseBlocks = mergedToolUse

		// DisableTools 模式下, 即使解出来工具调用也不执行: 丢弃 tool_use blocks,
		// 仅保留清洗后的文本. 这样思考型角色 (architect/researcher/planner)
		// 单轮就返回, 不会陷入多轮工具循环.
		if opts.DisableTools {
			toolUseBlocks = nil
			kept := assistantBlocks[:0]
			for _, b := range assistantBlocks {
				if b.Type == types.ContentBlockToolUse {
					continue
				}
				kept = append(kept, b)
			}
			assistantBlocks = kept
		}

		// 用清洗后的 assistantBlocks 拼接本轮 text. 这样最终返回值不会泄漏
		// 残留的 XML 包裹标签, 上层 (architect 校验器等) 看到的是纯净文本.
		// 同时收集 thinking 块文本 (MiniMax 等模型会把推理内容放在 thinking 块).
		var turnText strings.Builder
		for _, b := range assistantBlocks {
			if b.Type == types.ContentBlockText && b.Text != "" {
				turnText.WriteString(b.Text)
			}
			if b.Type == types.ContentBlockThinking && b.Thinking != "" {
				turnText.WriteString(b.Thinking)
			}
		}
		if turnText.Len() > 0 {
			lastAssistantText = turnText.String()
		}

		// 6. 构造 assistant 消息追加进 messages
		if len(assistantBlocks) > 0 {
			messages = append(messages, types.Message{
				Type:       types.MessageTypeAssistant,
				UUID:       internal_hook.GenerateUUID(),
				Content:    assistantBlocks,
				StopReason: resp.StopReason,
				CreatedAt:  time.Now(),
			})
		}

		// 7. 没有 tool_use → 完成, 返回最后文本.
		if len(toolUseBlocks) == 0 {
			// 如果整个 turn 既没有 text 也没有 tool_use, 给个明确错误.
			if lastAssistantText == "" {
				return "", fmt.Errorf("assistant returned empty content")
			}
			return lastAssistantText, nil
		}

		// 8. 有 tool_use → 跑工具, 把结果追加为 user 消息.
		if e.Tools == nil {
			return lastAssistantText, fmt.Errorf("tool registry not configured, cannot execute %d tool_use blocks", len(toolUseBlocks))
		}

		// 8a. 黑/白名单闸: RunIsolated 不经 HookChain, 在这里应用与主循环
		// ToolGateHook 相同的 toolExposed 语义 — 未获准的 tool_use (含 XML 回退
		// 提取的凭空调用) 替换为错误 tool_result, 绝不执行.
		gated, deniedResults := gateToolUses(e.Config, toolUseBlocks)
		messages = append(messages, deniedResults...)
		toolUseBlocks = gated
		if len(toolUseBlocks) == 0 {
			continue // 全部被拒: 错误结果已入上下文, 让模型重新选择
		}

		effectivePerm := e.Config.PermissionMode
		if e.Config.DynamicPlanCheck != nil && e.Config.DynamicPlanCheck() {
			effectivePerm = types.PermissionModePlan
		}
		tctx := &tool.ToolContext{
			Cwd:              e.Config.Cwd,
			PermissionMode:   effectivePerm,
			MainLoopModel:    e.Config.Model,
			IsNonInteractive: e.Config.IsNonInteractive,
			Debug:            e.Config.Debug,
			Messages:         messages,
			SessionID:        e.Config.SessionID,   // EnterPlanMode/ExitPlanMode 用会话级 plan flag
			PlanFileDir:      e.Config.PlanFileDir, // ExitPlanMode 将计划落盘供实施阶段读取
			Team:             e.Config.Team,        // 13.7-P1 池个性化身份 (与 queryLoop 同口径)
			Role:             e.Config.Role,
			GlobalPerm:       e.PermChecker,
			WriteClaim:       claim, // H3-4 并发写占用: 本次 run 的写者身份
		}
		// 阶段声明的 hook 在这里**并入**引擎既有的 Runner (见 IsolatedRunOptions.Hooks)。
		//
		// 合并放在循环外算一次 (hookRunner), 不在每轮拼 —— 拼的行为本身没副作用,
		// 但每轮拼一次意味着"这次运行挂了哪些 hook"在循环内部没有一个稳定读数,
		// 而排错时要问的恰恰是"它到底带了没带"。opts.Hooks 为空时 hookRunner 就是
		// e.HookRunner 本身 (逐字节等于改造前, 不多构造对象)。
		toolResults := tool.RunTools(ctx, toolUseBlocks, e.Tools, tctx, hookRunner)
		// RunTools 返回的是 user-role tool_result 消息, 直接追加
		messages = append(messages, toolResults...)

		// H3-2 中间态落盘。位置在**工具结果全部落进 messages 之后** ——
		// 那条"每个 tool_use 必须有对应 tool_result"的协议约定在这之前还没成立,
		// 此时落盘会把一段送不出去的历史写到磁盘上, 恢复后第一次请求必然 400。
		//
		// 每轮都写是刻意的: 中间态的价值全在"进程被杀的那一刻它有多新"。
		// 文件只有几十 KB 到几 MB, 而 150 轮的 run 每轮一次请求要几十秒 ——
		// 这点 I/O 在噪声里。
		if e.runStateDir != "" {
			e.runStateFrom(runState, messages, lastAssistantText, turn+1, turnClient.Model)
			e.persistRunState(runState)
		}

		// P0-1 run 级预算。团队 stage 路径的绝大部分 LLM 流量走这里 (见上方
		// llm_call Span 的注释), 所以这一处与 queryLoop 那处同等要紧 —— 只在
		// queryLoop 设闸门等于让跑得最贵的那条路没有止损线。
		//
		// 同样放在工具执行**之后**: 见 queryLoop 里同段注释 (悬空 tool_use 的历史
		// 送不出去)。这里还额外保证 RunTools 已把 tool_result 全部落进 messages。
		if e.RunBudget != nil {
			if resp != nil && resp.Usage != nil {
				e.chargeRunBudget(ctx, resp.Usage)
			}
			if e.RunBudget.Exhausted() {
				logging.For("engine").Warn("run 预算耗尽, 终止隔离循环",
					"limit", e.RunBudget.Limit(), "spent", e.RunBudget.Spent(), "turn", turn)
				// H3-3: 让出前留一份交接说明。**就在这条终止路径上**, 而不是放在
				// 循环外靠 err 反推 —— 那里已经分不清"预算耗尽"与"maxTurns 用光"
				// (两者都是循环外的同一个错误), 而这两件事写进交接说明的原因名不同,
				// 下一次的模型看到的东西也就不同。
				e.emitHandoff(ctx, handoffAt{
					Key: runKey, Identity: identity, Objective: userPrompt,
					Reason: HandoffByBudget,
					Detail: fmt.Sprintf(" (上限 %d, 实耗 %d)", e.RunBudget.Limit(), e.RunBudget.Spent()),
					Turn:   turn + 1, MaxTurns: opts.MaxTurns, Model: turnClient.Model,
					System: systemPrompts, Messages: messages, Client: turnClient, MaxTokens: opts.MaxTokens,
				})
				return lastAssistantText, &internal_hook.RunBudgetError{
					Limit: e.RunBudget.Limit(), Spent: e.RunBudget.Spent(),
				}
			}
		}
	}

	// H3-3: 轮数用光也是"被迫停止", 与预算耗尽同一条纪律 —— 留交接说明再走。
	//
	// 预算那条写在上面的循环体内, 这一条在循环外: 位置不同是因为判据不同 (前者
	// 每轮检查一次, 后者是循环的自然出口), 不是两种处理。**原因名必须分开** ——
	// 下一次读到交接说明的执行者若看到"预算耗尽"而实际是被 maxTurns 拦的, 它会
	// 去调一个不相干的配置。
	e.emitHandoff(ctx, handoffAt{
		Key: runKey, Identity: identity, Objective: userPrompt,
		Reason: HandoffByTurns, Detail: "",
		Turn: opts.MaxTurns, MaxTurns: opts.MaxTurns, Model: client.Model,
		System: systemPrompts, Messages: messages, Client: client, MaxTokens: opts.MaxTokens,
	})

	// 超过 MaxTurns 仍未完成: 返回目前为止的文本 + 错误.
	if lastAssistantText != "" {
		return lastAssistantText, fmt.Errorf("max_turns (%d) reached without final answer", opts.MaxTurns)
	}
	return "", fmt.Errorf("max_turns (%d) reached without any text response", opts.MaxTurns)
}

// gateToolUses 对 tool_use 块应用 Config.toolExposed (黑名单 + 白名单):
// 未获准的块被替换为错误 tool_result 消息以保持协议一致 (每个 tool_use 必须有
// 对应 tool_result), 获准的原样返回. RunIsolated 不走引擎 HookChain, 这是它的
// ToolGateHook 等价物.
func gateToolUses(cfg *Config, blocks []types.ContentBlock) (allowed []types.ContentBlock, denied []types.Message) {
	for _, b := range blocks {
		if cfg.toolExposed(b.Name) {
			allowed = append(allowed, b)
			continue
		}
		denied = append(denied, types.Message{
			Type: types.MessageTypeUser,
			UUID: internal_hook.GenerateUUID(),
			Content: []types.ContentBlock{{
				Type:      types.ContentBlockToolResult,
				ToolUseID: b.ID,
				Content:   fmt.Sprintf("工具 %s 不在本会话允许的工具清单内，已被拒绝。请只使用当前可见的工具。", b.Name),
				IsError:   true,
			}},
			CreatedAt: time.Now(),
		})
	}
	return allowed, denied
}

// MessagesToAPI 把内部 types.Message 列表转换成 API 协议的 APIMessage 列表
// (公开 messagesToAPI, 供外部包测试 / 集成).
func MessagesToAPI(messages []types.Message) []types.APIMessage {
	return messagesToAPI(messages, nil)
}

// 保证 json 包不会因为后续没用到 而被 goimports 删除 (这里没用, 留位).
var _ = json.RawMessage(nil)
