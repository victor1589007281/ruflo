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
	"path/filepath"
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
	// DisableDefaultPrompt 为 true 时**只**注入 SystemPrompt，不再追加 PromptMgr 的
	// 全局/CustomPrompt 与 MCP 指引。
	//
	// 窄面 agent（显式 tools 白名单的只读 agent）用的就是这一档：全局提示词会声称
	// Bash/Write/Edit 可用，而工具面里根本没有它们；MCP 指引也会按注册表全集生成，
	// 与本次可见工具不一致。提示词与真实工具面互相矛盾时，模型会花轮次试探不存在的
	// 工具。SystemPrompt 为空时本字段没有意义（不会凭空造出提示词）。
	DisableDefaultPrompt bool
	// MaxTurns 最多循环多少轮, <=0 时使用 30.
	MaxTurns int
	// MaxTokens 单次请求最大输出 tokens, <=0 时使用 8192.
	MaxTokens int
	// FinalizeTurns 最后 N 轮强制收口, <=0 时关闭 (逐字节走改造前行为)。
	//
	// 语义分两段:
	//   - 剩余轮数 == FinalizeTurns 时, 注入一条"停止检索、开始成文"的用户消息;
	//   - 最后一轮不再向模型暴露任何工具, 并丢弃响应里的 tool_use 块, 只剩文本。
	//
	// 为什么必须在执行层做: "最后 2 轮只写报告"写在提示词里只是建议。实测 CBAM
	// 这类多 PDF 任务会继续读文件直到 MaxTurns 用光, 拿回一份没有结论的半成品,
	// 而 run 还会报 max_turns 错误。提示词无法让"工具不可用"这件事成立。
	FinalizeTurns int
	// FinalizeInstruction 可选的调用方交付契约, 只在 FinalizeTurns>0 时生效。
	// 空 = 沿用默认的 Markdown 报告文案; 非空 = 保留"停止检索/末轮无工具"的
	// 执行约束, 但把输出格式交给调用方。一次性 JSON agent 必须走这一条:
	// 默认报告文案与 max_turns 后的 handoff 一样, 都会覆盖它的结构化输出契约。
	FinalizeInstruction string
	// MergeBashTargets 启用 run 级 Bash 目标合并闸 (见 pkg/tool/bashguard.go)。
	//
	// 为 true 时, 同一份 PDF/URL 被拆成多条单独命令重复访问会被拒绝, 模型收到的
	// tool_result 要求把下载/解析/提取合并成一条命令。按**目标**判重, 不是按
	// Bash 调用次数设上限 —— 多目标并行的任务不会被误伤。
	MergeBashTargets bool
	// RestrictWritesToCwd 为 true 时, Write/Edit 只能落在 e.Config.Cwd 内。
	//
	// 调研角色只该产出工作目录里的 evidence.md, 但 bypass 模式下模型可以把
	// 绝对路径写到任何地方 (实测写成了 /home/victor/evidence.md)。并发任务还会
	// 因此互相覆盖同一份文件。默认 false = 改造前行为。
	RestrictWritesToCwd bool
	// FinalResponse 可选的窄面 agent 交付契约。首次结构/证据引用不达标时
	// 撤销工具并完整重写一次；仍不达标则返回错误，不静默交付草稿。
	FinalResponse *FinalResponseContract
	// CitationGate 为 true 时, 模型第一次想结束 (没有 tool_use) 会做一次引用核验:
	// 若最终文本里过半的 URL 只在搜索结果中出现、从未成功抓取, 就注入一条修订
	// 指令并让循环再跑一轮 (只给一次机会)。
	//
	// 为什么需要: 提示词已经写了"摘录只能来自实际打开的页面", 实测 biomed 任务
	// 16 条引用里 11 条从未打开 (investor.lilly.com / PubMed 都有), 摘录却写成
	// 引文。收口提醒也没用 —— 那次 run 18 轮就结束, 根本没到收口阈值。
	CitationGate bool
	// NormalizeEvidence 为 true 时, 运行结束把 cwd/evidence.md 表格
	// 「来源 URL」列里的"同上"展开成上一条真实 URL (见 evidence.go)。
	//
	// 只做机械展开: 不改结论、不改其他列、没有前序 URL 时原样保留。
	NormalizeEvidence bool
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
	// 不会触发。此处补 episode 级 node span；LLM 与工具调用在执行点分别补
	// llm_call/tool_call span，工具耗时按单次调用计时而不是并发批次总耗时。
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
					Attrs: map[string]any{
						"status": status, "isolated": true,
						"permission_mode":   string(e.Config.PermissionMode),
						"allowed_tools_set": e.Config.AllowedTools != nil,
						"allowed_tools":     sortedKeys(e.Config.AllowedTools),
						"disabled_tools":    sortedKeys(e.Config.DisabledTools),
					},
					TS:    start.UnixMilli(),
					DurMS: time.Since(start).Milliseconds(),
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
	// FinalizeTurns 不能超过总轮数: 否则 "剩余 == FinalizeTurns" 这个阈值
	// 永远命不中, 收口提醒一次都不会注入 —— 一个静默失效的配置。
	finalizeTurns := opts.FinalizeTurns
	if finalizeTurns > opts.MaxTurns {
		finalizeTurns = opts.MaxTurns
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
	// DisableDefaultPrompt 是调用方的显式声明：这个 agent 的提示词就是全部，不要再
	// 追加会与它的工具面矛盾的全局人格/MCP 指引。与 DisableTools 那条短路并列而不是
	// 合并：思考型角色关工具，窄面只读 agent 仍要工具。
	if e.PromptMgr != nil && !opts.DisableDefaultPrompt && !(opts.DisableTools && opts.SystemPrompt != "") {
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
	// 检索预算软提醒: 只数 WebSearch 调用, 不数 WebFetch —— 打开一个页面是
	// 兑现证据, 继续换关键词才是扩大检索面。实测 solid 任务 26 次搜索只换回
	// 4 个可打开页面, 轮次全花在"再搜一次"上, 轮到成文时证据表只有 7 行。
	const searchSoftBudget = 18
	webSearchCalls := 0
	searchBudgetWarned := false
	// 引用账本: 运行中收集工具实际返回/请求过的 URL, 收口时交给模型,
	// 结束后审计最终文本 (见 citation.go)。用户 prompt 里的 URL 也算已见 ——
	// 那是调用方给的来源, 引用它不算编造。
	ledger := newCitationLedger()
	ledger.observeText(userPrompt)
	// 必须包一层闭包: `defer ledger.audit(lastAssistantText)` 会在 defer 语句
	// 处就求值参数, 那时 lastAssistantText 还是空串 —— 审计永远看不到报告.
	// 审计对象是命名返回值 finalText, 不是 lastAssistantText: 报告清理 defer
	// 注册得更晚、执行得更早, 这里拿到的才是**实际交付**的文本。
	defer func() { ledger.audit(finalText) }()
	// 交付前清掉起草旁白 (Hmm / Let me ... / 诚实：) 并补齐缺失的一级标题;
	// 同时修补 evidence.md (展开「同上」, 空文件用报告证据表回填)。
	//
	// 只对调研类角色生效: 编码/评审 agent 的最终文本里,"Hmm" 可能是正文的一部分,
	// 不能按报告体例改写。注册在审计 defer 之后, 因此先执行 —— 审计与 trace
	// 记录的都是清理后的文本。evidence.md 的回填也依赖这个顺序: 它要拿最终报告
	// 里的证据表, 而不是带草稿旁白的原文。
	if opts.NormalizeEvidence {
		defer func() {
			raw := finalText
			finalText = EnsureReportTitle(CleanReportText(raw), userPrompt)
			// 交付报告的证据表也要展开"同上": 提示词已禁止, 但实测 vdb 仍有 7 行
			// 这么写。只动 URL 列, 时间列的"同上"保留。
			if expanded, n := expandSameAsAbove(finalText); n > 0 {
				finalText = expanded
				logging.For("engine").Info("最终报告证据表的「同上」已展开", "rows", n)
			}
			// 清理前后各量一次正文字数: 审计日志只报交付文本, 没有这条就无法
			// 解释"报告 2310 字、审计 2464 字"到底是清理删了旁白还是统计口径漂移。
			if raw != finalText {
				rawBody, _, _ := reportShape(raw)
				finalBody, _, _ := reportShape(finalText)
				logging.For("engine").Info("最终报告已清理",
					"raw_bytes", len(raw), "final_bytes", len(finalText),
					"raw_body_cjk", rawBody, "final_body_cjk", finalBody)
			}
			if strings.TrimSpace(e.Config.Cwd) == "" {
				return
			}
			path := filepath.Join(e.Config.Cwd, "evidence.md")
			normalizeEvidenceFile(path)
			backfillEvidenceFile(path, finalText)
		}()
	}
	// Bash 目标合并闸的账本必须是 **run 级**: 每次 RunIsolated 一个新实例。
	// 挂在引擎或 BashTool 上都会让并发 run 共享"已访问目标", 表现是 B 任务
	// 第一次访问某份 PDF 就被拒 (而 A 任务确实访问过它) —— 一条无从归因的误拒。
	var bashMerge *tool.BashMergeGuard
	if opts.MergeBashTargets {
		bashMerge = tool.NewBashMergeGuard()
	}
	// 写入根同样按 run 固定: 用 e.Config.Cwd 的绝对形式, 避免相对 cwd 与
	// expandPath 出来的绝对路径做 Rel 时永远不匹配 (那会让限制变成全拒)。
	writeRoot := ""
	if opts.RestrictWritesToCwd && strings.TrimSpace(e.Config.Cwd) != "" {
		writeRoot = e.Config.Cwd
		if abs, err := filepath.Abs(writeRoot); err == nil {
			writeRoot = abs
		}
	}
	// 引用核验闸只触发一次: 第二次仍不达标就交付 (审计日志已记下未抓取清单),
	// 反复要求修订会把一个"证据不足"的报告变成无限循环。
	citationGateUsed := false
	responseRepairUsed := false
	responseRepairPending := false
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
		responseRepairTurn := responseRepairPending
		responseRepairPending = false

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

		// 强制收口 (FinalizeTurns): 提醒放在压缩**之后**、请求之前。
		//
		// 不能放在注入/压缩之前: 这轮收口提醒正是"别再按旧方向做"的最高优先级
		// 指令, 被折进摘要就等于没发。放在压缩后也保证它一定是历史里的最后一条。
		remaining := opts.MaxTurns - turn
		finalTurn := finalizeTurns > 0 && remaining == 1
		if finalizeTurns > 0 {
			// 优先给"成功抓取过"的 URL; 一条都没抓到时退回"见过"的清单
			// (至少不让模型凭空拼路径)。
			verified := ledger.sampleFetched(60)
			if len(verified) == 0 {
				verified = ledger.sample(60)
			}
			switch {
			case finalTurn:
				messages = append(messages, finalizeNotice(finalTurn, remaining, verified, opts.FinalizeInstruction))
			case remaining == finalizeTurns:
				messages = append(messages, finalizeNotice(false, remaining, verified, opts.FinalizeInstruction))
			}
		}
		// 检索预算软提醒只发一次, 且不能和收口提醒抢最后一格: 收口提醒
		// (含最后一轮的禁用工具) 优先级更高, 它已经要求停止检索了。
		// 条件写 remaining > finalizeTurns 而不是 !finalTurn: 收口提醒在
		// remaining == finalizeTurns 那一轮就发了, 此时再追加"去打开页面"
		// 会把它覆盖成相反指令。
		if remaining > finalizeTurns && !searchBudgetWarned && webSearchCalls >= searchSoftBudget {
			messages = append(messages, searchBudgetNotice(webSearchCalls))
			searchBudgetWarned = true
			logging.Event(ctx, "search.soft_budget_notice",
				"websearch_calls", webSearchCalls, "turn", turn, "remaining", remaining)
			// 提醒进了历史, 但服务端按工具结果轮次缓存前缀; 这条消息只发一次,
			// 不会每轮破坏缓存。
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

		// 最后一轮把工具面整个撤掉: 模型即使还想检索, 也没有可调用的工具。
		// 只靠提示词做不到这件事 —— 提示词是建议, 空工具表是约束。
		turnTools := apiTools
		if finalTurn || responseRepairTurn {
			turnTools = nil
		}

		callStart := time.Now()
		resp, err := turnClient.SendMessage(ctx, apiMessages, systemPrompts, turnTools, opts.MaxTokens)

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
				Messages: apiMessages, Tools: turnTools,
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
		// WebFetch 的目标 URL 只在入参里; 漏记会让"真的打开过"的页面在收口
		// 清单里消失, 模型于是被引导去引用搜索结果页而不是原文。
		ledger.observeToolUses(toolUseBlocks)

		// DisableTools / 最后一轮收口: 即使解出来工具调用也不执行, 丢弃 tool_use
		// blocks, 仅保留清洗后的文本.
		//
		// 最后一轮必须双保险: 工具表已置空, 但 XML 回退能从文本里"凭空"解析出
		// tool_use (DeepSeek/MiniMax 系会这么干). 只撤工具表的话, 这些块会进
		// toolUseBlocks 分支并被执行 —— 最后一轮的"禁用工具"就被绕过了.
		if opts.DisableTools || finalTurn || responseRepairTurn {
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
		//
		// thinking 块只在**本轮没有任何 text 块**时兜底使用 (MiniMax 等模型会把
		// 整段回答放在 thinking 里)。不能无条件拼接: 实测 DeepSeek 在最后一轮
		// 同时返回 thinking 和 text, thinking 里是一份带标题的写作提纲, 拼在
		// 正文前会让交付文本出现两个 H1、清理器把提纲当正文保留。CLI 事后虽然
		// 会再 sanitize 一次, 但引擎内的引用审计、evidence.md 回填与 trace 都已
		// 经看到污染后的文本 —— 审计字数比交付报告多 64 字、来源域名多 1 个,
		// 都是从这里来的。
		var turnText, thinkingText strings.Builder
		for _, b := range assistantBlocks {
			if b.Type == types.ContentBlockText && b.Text != "" {
				turnText.WriteString(b.Text)
			}
			if b.Type == types.ContentBlockThinking && b.Thinking != "" {
				thinkingText.WriteString(b.Thinking)
			}
		}
		if turnText.Len() == 0 {
			turnText = thinkingText
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
			// 最后一轮仍然没有文本 = 收口失败. 必须报出来, 不能把上一轮的
			// 中间产物当成最终答案静默返回 —— 那正是"跑完了但交的是半成品"
			// 的形态, 上游看不出任何异常.
			if (finalTurn || responseRepairTurn) && turnText.Len() == 0 {
				return lastAssistantText, fmt.Errorf("finalization turn produced no text")
			}
			// 如果整个 turn 既没有 text 也没有 tool_use, 给个明确错误.
			if lastAssistantText == "" {
				return "", fmt.Errorf("assistant returned empty content")
			}
			if opts.FinalResponse != nil {
				known := finalResponseEvidenceIDs(messages)
				if validationErr := validateFinalResponse(lastAssistantText, opts.FinalResponse, known); validationErr != nil {
					if !responseRepairUsed && opts.MaxTurns-turn > 1 {
						responseRepairUsed = true
						responseRepairPending = true
						messages = append(messages, finalResponseRepairNotice(opts.FinalResponse, validationErr))
						logging.For("engine").Warn("最终答案契约未通过，撤销工具并要求完整重写一次",
							"reason", validationErr.Error(), "turn", turn)
						continue
					}
					return lastAssistantText, fmt.Errorf("final response contract: %w", validationErr)
				}
			}
			// 引用核验闸: 只给一次机会, 且必须还有下一轮可用 (否则改完没地方写).
			// 至少留 6 轮才值得开核验: 实测 23/28 轮才触发时, 模型没有余量补抓,
			// 直接把 6 条引用删掉把比例"修"达标, 报告从 9 个来源缩到 3 个 ——
			// 时为修一个指标把内容修没了. 宁可跳过并让审计日志留痕.
			if opts.CitationGate && !citationGateUsed && opts.MaxTurns-turn >= 6 {
				unfetched, cited := ledger.unfetched(lastAssistantText)
				if cited >= 3 && len(unfetched)*2 >= cited {
					citationGateUsed = true
					messages = append(messages, citationGateNotice(unfetched, cited))
					logging.For("engine").Warn("最终报告有过半引用未成功抓取, 要求修订一次",
						"cited", cited, "seen_not_fetched", len(unfetched), "turn", turn)
					continue
				}
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
		for _, b := range toolUseBlocks {
			if b.Name == "WebSearch" {
				webSearchCalls++
			}
		}
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
			BashMerge:        bashMerge,
			WriteRoot:        writeRoot,
		}
		// 阶段声明的 hook 在这里**并入**引擎既有的 Runner (见 IsolatedRunOptions.Hooks)。
		//
		// 合并放在循环外算一次 (hookRunner), 不在每轮拼 —— 拼的行为本身没副作用,
		// 但每轮拼一次意味着"这次运行挂了哪些 hook"在循环内部没有一个稳定读数,
		// 而排错时要问的恰恰是"它到底带了没带"。opts.Hooks 为空时 hookRunner 就是
		// e.HookRunner 本身 (逐字节等于改造前, 不多构造对象)。
		var toolObservations chan tool.ToolCallObservation
		if e.TraceStore != nil {
			toolObservations = make(chan tool.ToolCallObservation, len(toolUseBlocks))
			tctx.ObserveToolCall = func(obs tool.ToolCallObservation) { toolObservations <- obs }
		}
		toolResults := tool.RunTools(ctx, toolUseBlocks, e.Tools, tctx, hookRunner)
		if toolObservations != nil {
			close(toolObservations)
			ids := trace.From(ctx)
			for obs := range toolObservations {
				status := "completed"
				if obs.IsError {
					status = "error"
				}
				e.TraceStore.Write(tracestore.Span{
					TraceID: ids.RunID, SpanID: internal_hook.GenerateUUID(), ParentID: ids.TurnID,
					Kind: tracestore.KindToolCall, Name: obs.ToolName, NodeID: ids.NodeID, TurnID: ids.TurnID,
					InputRef: e.TraceStore.MakeRef(string(obs.Input)), OutputRef: e.TraceStore.MakeRef(obs.Output),
					Attrs: map[string]any{
						"status": status, "is_error": obs.IsError, "isolated": true,
						"permission_mode": string(tctx.PermissionMode), "tool_use_id": obs.ToolUseID,
					},
					TS: obs.StartedAt.UnixMilli(), DurMS: obs.Duration.Milliseconds(),
				})
			}
		}
		ledger.observeMessages(toolResults)
		// 入参+结果配对, 只把成功返回的 WebFetch / curl 记为"已抓取"。
		ledger.observeFetches(toolUseBlocks, toolResults)
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

// finalizeNotice 构造强制收口的用户消息。
//
// last=true 时工具面已经撤掉, 文案必须说清"工具不可用", 否则模型会继续输出
// tool_use 等待结果, 最后一轮白跑。默认文案面向 Markdown 报告；调用方提供
// instruction 时只替换交付格式部分，不追加报告章节或证据表假设。这样严格 JSON
// agent 也能复用同一条"末轮无工具"路径，而不会又被提示词拉回 Markdown。
func finalizeNotice(last bool, remaining int, verified []string, instruction string) types.Message {
	var text string
	if custom := strings.TrimSpace(instruction); custom != "" {
		text = fmt.Sprintf("【收口提醒】本次运行只剩 %d 轮。立即停止检索，基于已有材料交付；未完成的部分如实写入调用方约定的 warnings/局限字段，不得编造。\n%s", remaining, custom)
		if last {
			text = "【最后一轮】工具已经禁用，本轮只能输出文本。不要再调用任何工具，也不要输出计划或交接说明。必须严格遵守以下最终交付契约：\n" + custom
		}
	} else {
		text = fmt.Sprintf("【收口提醒】本次运行只剩 %d 轮。立即停止一切检索与工具调用: 不要再发起 WebSearch / WebFetch / Bash, 不要再补证据。请直接用已经收集到的材料写出完整报告正文: 第一行是具体的一级标题 (# 开头), 之后是结论、分析、证据表和局限。材料不足的部分如实写进「局限」, 不要编造来源或数字。", remaining)
		if last {
			text = "【最后一轮】工具已经禁用, 本轮只能输出文本。不要再调用任何工具, 也不要输出计划或过程说明。现在直接写出最终报告正文: 第一行是具体的一级标题 (# 开头), 随后是完整结论、分析、证据表与局限。材料不足处写进「局限」, 不要臆造。"
		}
		if len(verified) > 0 {
			var b strings.Builder
			b.WriteString(text)
			b.WriteString("\n\n本轮工具实际返回/请求过的 URL 如下。证据表**只能**引用其中出现过的 URL; 不在清单内的链接一律不要写 (宁可写进「局限」)。不要根据申请号/DOI/日期自己拼接路径:\n")
			for _, u := range verified {
				b.WriteString("- ")
				b.WriteString(u)
				b.WriteString("\n")
			}
			text = b.String()
		}
	}
	return types.Message{
		Type:      types.MessageTypeUser,
		UUID:      internal_hook.GenerateUUID(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: text}},
		CreatedAt: time.Now(),
	}
}

// searchBudgetNotice 构造"检索预算"软提醒 (每次 run 至多发一次)。
//
// 它和收口提醒的分工: 收口提醒说"没轮次了, 停手"; 这条说"搜索次数已经很多,
// 但轮次还有 —— 别再换关键词, 去打开最有希望的结果"。实测失败模式不是搜不到,
// 而是把 20 多次搜索花在同一个方向上, 最后证据表只有 1-2 个域名。
func searchBudgetNotice(calls int) types.Message {
	text := fmt.Sprintf("【检索预算提醒】本次运行已发起 %d 次 WebSearch。停止扩大检索面: 不要再换关键词搜索, 从已有结果里挑 3-5 个最相关的 URL 用 WebFetch 打开, 拿到原文后再成文。证据表需要至少 4 个独立域名; 如果已有结果不足以支撑, 就在「局限」里如实说明, 不要用搜索摘要冒充实读原文。", calls)
	return types.Message{
		Type:      types.MessageTypeUser,
		UUID:      internal_hook.GenerateUUID(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: text}},
		CreatedAt: time.Now(),
	}
}

// citationGateNotice 构造"引用核验"修订指令 (只注入一次)。
//
// 文案必须给出**两条可执行出路**: 打开页面, 或删除/降级该行。只说"不要引用
// 未抓取 URL"会让模型把整段结论删掉 —— 而结论本身可能是有依据的, 只是来源
// 写成了搜索结果。同时把格式要求再点一遍: 这是模型最后一次成文机会。
func citationGateNotice(unfetched []string, cited int) types.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "【引用核验】交付前自检: 最终报告引用了 %d 个 URL, 其中 %d 个从未成功抓取, 不能支撑「原文逐字摘录」。请处理后**重新输出完整最终报告**:\n", cited, len(unfetched))
	b.WriteString("1) **首选保留事实行**, 把该行「置信度」改为「仅搜索摘要, 未核验」, 并在「局限」逐条列出这些 URL;\n")
	b.WriteString("2) 只有能用已抓取来源替代、或该行本身不支撑结论时, 才删除/替换该行;\n")
	b.WriteString("3) 若某条证据不可或缺, 最多补抓 3 个 URL; 不要开启新的检索方向。\n")
	b.WriteString("**不要为了降低未抓取比例而删证据**: 修订后的证据表行数与引用 URL 数都不得少于修订前。\n")
	b.WriteString("不要凭 snippet 编造引文。需要处理的 URL:\n")
	for _, u := range firstNStrings(unfetched, 12) {
		b.WriteString("- ")
		b.WriteString(u)
		b.WriteString("\n")
	}
	b.WriteString("\n另: 最终回答第一行必须是 \"# \" 开头的一级标题, 正文不超过 2500 字; 证据表每行写完整 URL, 不要写「同上」。")
	return types.Message{
		Type:      types.MessageTypeUser,
		UUID:      internal_hook.GenerateUUID(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: b.String()}},
		CreatedAt: time.Now(),
	}
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
