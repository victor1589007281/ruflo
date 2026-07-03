# Advisor Tool 设计方案

> 调研来源：Claude Code 2.1.120 官方二进制逆向（strings 提取，147 处 advisor 引用）+ claude-go 源码架构分析。
> 日期：2026-07-03

---

## 一、官方 Claude Code Advisor 实现调研

### 1.1 定位

Advisor 是 Claude Code 2.1.x 的实验性功能（内部代号 `tengu_sage_compass2`）：**让较弱/较便宜的主模型在任务关键时刻咨询一个更强的 reviewer 模型**，获得战略性指导后继续执行。

官方推荐配置与卖点（原文）：

> "Sonnet as the main model with Opus as the advisor. For certain workloads this gives near-Opus performance with reduced token usage."

触发场景（官方 UI 描述原文）：

> "a complex decision, an ambiguous failure, a problem it's circling without progress — it escalates to the advisor model for guidance, then resumes. The advisor runs server-side and uses additional tokens."

关键定性：**pull 模式** —— 由主模型自主决定何时调用（通过 system prompt 引导），而非 harness 周期性强制审查。

### 1.2 协议机制（server-side tool）

官方 advisor 是 **服务端工具**（与 web_search 同类），CLI 只负责声明与渲染：

| 环节 | 实现 |
|------|------|
| 启用 | anthropic-beta header `advisor-tool-2026-03-01` |
| 工具声明 | tools 数组追加 `{type:"advisor_20260301", name:"advisor", model:"<advisor模型>"}` |
| 调用 | 主模型发出 `server_tool_use` block（name=advisor，**无输入参数**）；服务端自动把**整个会话历史**转发给 advisor 模型 |
| 返回 | 同一 stream 内返回 `advisor_tool_result` block，content 类型三种：`advisor_result`（建议文本）/ `advisor_redacted_result` / `advisor_tool_result_error`（带 error_code） |
| 计量 | usage.iterations 中 `advisor_message` 条目单独计 token 与成本（遥测 `tengu_advisor_tool_token_usage`） |
| 配置 | userSettings.`advisorModel`；`/advisor <model|off>` 斜杠命令；`--advisor <model>` CLI flag（隐藏） |
| 模型白名单 | 主模型与 advisor 模型都必须是 opus-4-7 / opus-4-6 / sonnet-4-6 |
| 门禁 | `CLAUDE_CODE_DISABLE_ADVISOR_TOOL` 关 / `CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL` 强开 / 否则 statsig gate；仅 firstParty 认证 |
| 容错 | 400 "Advisor tool result content could not be processed" → 从历史中剥离 advisor blocks 后重试（`retry:advisor-strip`）；advisor 结果块只含 thinking/空文本时替换为 `[Advisor response]` 占位符再回传 |
| UI | 折叠显示 "✓ Advisor has reviewed the conversation and will apply the feedback"，verbose 展开建议全文；错误显示 "Advisor unavailable (code)" |

### 1.3 Prompt 策略（设计精华，二进制提取全文）

启用后向主模型 system prompt 注入 `# Advisor Tool` 段落，全文：

```
# Advisor Tool
You have access to an `advisor` tool backed by a stronger reviewer model. It takes
NO parameters -- when you call advisor(), your entire conversation history is
automatically forwarded. They see the task, every tool call you've made, every
result you've seen.

Call advisor BEFORE substantive work -- before writing, before committing to an
interpretation, before building on an assumption. If the task requires orientation
first (finding files, fetching a source, seeing what's there), do that, then call
advisor. Orientation is not substantive work. Writing, editing, and declaring an
answer are.

Also call advisor:
- When you believe the task is complete. BEFORE this call, make your deliverable
  durable: write the file, save the result, commit the change. The advisor call
  takes time; if the session ends during it, a durable result persists and an
  unwritten one doesn't.
- When stuck -- errors recurring, approach not converging, results that don't fit.
- When considering a change of approach.

On tasks longer than a few steps, call advisor at least once before committing to
an approach and once before declaring done. On short reactive tasks where the next
action is dictated by tool output you just read, you don't need to keep calling --
the advisor adds most of its value on the first call, before the approach
crystallizes.

Give the advice serious weight. If you follow a step and it fails empirically, or
you have primary-source evidence that contradicts a specific claim (the file says
X, the paper states Y), adapt. A passing self-test is not evidence the advice is
wrong -- it's evidence your test doesn't check what the advice is checking.

If you've already retrieved data pointing one way and the advisor points another:
don't silently switch. Surface the conflict in one more advisor call -- "I found X,
you suggest Y, which constraint breaks the tie?" The advisor saw your evidence but
may have underweighted it; a reconcile call is cheaper than committing to the
wrong branch.
```

设计要点解读：

1. **无参数设计**：advisor() 不接受任何输入 → 主模型无法用带偏见的问题框架引导 advisor，也无法偷懒只转发部分上下文；调用成本（对主模型而言）为零，降低"懒得问"的门槛。
2. **调用时机三分法**：orientation（找文件/读源码）不算实质工作可以先做；写入/编辑/下结论前必须先问；宣布完成前必须再问一次。
3. **durability-first**：宣布完成前先把交付物落盘再调 advisor —— advisor 调用耗时，会话若中途死掉，落盘的结果还在。
4. **建议权重规则**：advice 默认优先；只有经验性失败或一手证据矛盾才可偏离；"自测通过"不构成反证。
5. **冲突协调协议**：证据与建议冲突时禁止 silently switch，必须发起一次 reconcile call 摊牌。

---

## 二、claude-go 现状与约束

### 2.1 关键差异

claude-go 后端是 kimi 等 OpenAI 兼容模型，**没有 Anthropic 服务端 advisor 可用** → 必须客户端模拟：harness 自己实现"转发全部历史给强模型"这一步。

### 2.2 现有可复用的基础设施（源码确认）

| 能力 | 位置 | 说明 |
|------|------|------|
| 工具接口 | `pkg/tool/tool.go:33` `Tool` interface | Name/Description/InputSchema/Call/IsConcurrencySafe/CheckPermissions/IsReadOnly |
| **历史透传** | `pkg/tool/tool.go` `ToolContext.Messages` | queryLoop 构造 tctx 时已传入完整 `messages`（`pkg/engine/engine.go:1209-1218`），advisor 工具零成本拿到全量轨迹 |
| 主循环模型名 | `ToolContext.MainLoopModel` | 同上已透传 |
| LLM 独立调用 | `pkg/api/client.go:239` `NewClient` / `:983` `SendMessage` / `:1459` `SimpleComplete` | pkg/compact 已有"组件持有独立 Client 发总结请求"的先例（`pkg/compact/compact.go:38,128`） |
| token 估算 | `pkg/compact/compact.go:207` `estimateTokens` | transcript 预算控制可复用同思路 |
| 遥测 | `pkg/api/client.go:77` `LLMCallRecord.Purpose` | 打 `purpose="advisor"` 即接入现有 dashboard LLM 指标 |
| 配置 | `pkg/settings/settings.go:15` `Settings` / `pkg/feishu/types.go` `AISection`(alias/providers 模式) | 新增 advisor 段，复用 alias→provider 解析构造 advisor Client |
| 工具注册 | `pkg/tool/builtin/register.go` `RegisterBaseToolsWithStore` | 追加可选注册 |
| system prompt 组装 | `pkg/prompt/prompt.go:76` `BuildEffectiveSystemPrompt` | 工具描述自动进 system prompt；`# Advisor Tool` 策略段可注入 |
| 内置 Hook 链 | `pkg/engine/internal_hook/` HookChain（PreTurn/PostResponse 等阶段） | Phase 3 push 模式挂载点；G3 LoopDetector 可联动 |
| 既有 reviewer 机制 | `pkg/orchestrator/llm_runner.go:225` AdversarialRunner、`pkg/agent/adversarial.go`、vision_critic | 产物级/多轮对抗审查，与 advisor（轨迹级、单次、进行中）互补，不冲突 |

### 2.3 与现有 reviewer 机制的分工

- **AdversarialRunner**：生成→审查→反馈→再生成的重循环，作用于**产物**，编排器驱动。
- **Vision Critic / Global Consistency Reviewer**：产物质检。
- **Advisor（本方案）**：作用于**进行中的工作轨迹**，主模型主动、单次调用、轻量返回方向性建议。填补"任务进行中战略方向审视"空档。

---

## 三、实现方案

### 3.1 总体架构

**Pull 模式为主（对齐官方）**：注册一个无参数 builtin 工具 `advisor`；主模型调用时，harness 把 `tctx.Messages` 序列化为 transcript，用独立的 advisor `api.Client` + advisor 人设 system prompt 发起一次**无工具**调用，建议文本作为 tool_result 返回主循环。

**Push 模式为辅（Phase 3，claude-go 特色）**：长自治任务（cron/team/dreaming）中由 internal hook 在 LoopDetector 报警或每 N 轮强制触发一次 advisor 审视。官方没有这一层（靠模型自觉），claude-go 的自治场景（7 天续会式长任务）恰好需要。

```
主模型(kimi) ── tool_use: advisor() ──▶ AdvisorTool.Call()
                                          │ tctx.Messages
                                          ▼
                                    BuildTranscript()   ← token 预算截断
                                          │
                                          ▼
                              advisorClient.SendMessage()  ← 强模型, 无工具, purpose="advisor"
                                          │
                                          ▼
                              tool_result: 方向判断/风险/下一步  ──▶ 回主循环
```

### 3.2 新增文件与改动点

#### (1) `pkg/tool/builtin/advisor.go` — AdvisorTool

```go
type AdvisorTool struct {
    client   *api.Client   // advisor 模型专用 client（模型/baseURL 可与主模型不同）
    opts     AdvisorOptions
    mu       sync.Mutex
    calls    int           // 本会话已调用次数
    lastTurn int           // 冷却控制
}

type AdvisorOptions struct {
    MaxCallsPerSession  int // 默认 8
    CooldownTurns       int // 默认 2：两次调用间至少间隔的 assistant 轮数
    MaxTranscriptTokens int // 默认 60000，按 advisor 模型上下文调
    MaxOutputTokens     int // 默认 2000
}
```

- `Name()` = `"advisor"`；`InputSchema()` = `{"type":"object","properties":{}}`（**无参数，对齐官方**）
- `IsReadOnly()` = true；`IsConcurrencySafe()` = **false**（语义是"停下来咨询"，且建议必须基于最新历史，不应与写工具并发）
- `CheckPermissions()` = nil（无需确认）
- `Call()` 流程：
  1. 护栏检查（见 3.4），超限返回**非 error** 的 tool_result："advisor budget exhausted; proceed with your own judgment"（避免主模型把 error 当环境故障重试）。
  2. `BuildTranscript(tctx.Messages, opts.MaxTranscriptTokens)` 序列化历史。
  3. `ctx = api.WithLLMMetrics(ctx, source, "advisor")` 打遥测标签，`client.SendMessage(ctx, []APIMessage{transcript 单条 user}, advisorSystemPrompt, nil /*无工具→天然防递归*/, opts.MaxOutputTokens)`。
  4. 失败 → `ToolResult{IsError:true, Content:"Advisor unavailable (<原因>)"}`，**绝不中断主循环**（对齐官方 `advisor_tool_result_error`）。
  5. 成功 → 建议文本作为 Content 返回。

#### (2) Transcript 序列化（advisor.go 内 `BuildTranscript`）

把 `[]types.Message` 渲染为纯文本轨迹：

- 头部：任务指令（engine 的 TaskInstruction / 首条 user message 全文保留）。
- 逐条：`[user]` / `[assistant]` 文本；`[tool_use] Name(input 截断至 ~500 字符)`；`[tool_result] (截断至 ~2000 字符，尾部保留)`；thinking 块跳过；图片/多模态块降级为 `[image]` 占位。
- 预算控制：`estimateTokens` 超出 `MaxTranscriptTokens` 时**保头（任务定义）+ 保尾（最近轨迹）**，中段折叠为 `[... N messages omitted ...]`。这是官方"整个会话历史自动转发"在有限上下文下的务实近似——官方服务端同样受 advisor 模型上下文约束。

#### (3) Advisor 人设 system prompt（advisor.go 内常量）

```
你是一位资深技术顾问。另一个 AI agent 正在执行任务，它把完整工作轨迹转发给你，
请求方向性指导。你看到的是它的任务目标、每一次工具调用和每一个结果。

审视并简洁输出（≤400字）：
1. 方向判断：当前路径是否通向任务目标？有没有更省的路？
2. 风险：它正在依赖的哪个假设最可疑？哪个证据被它低估/误读了？
3. 下一步：接下来 1-3 步具体应该做什么（可执行，不要泛泛而谈）。
4. 若它认为任务已完成：指出遗漏的验证或未覆盖的边界，或确认可以收工。

不要复述轨迹。不要客套。直接给判断。
```

#### (4) 主模型引导 prompt

官方 `# Advisor Tool` 段落（1.3 全文）是这个功能的核心 IP，直接移植：

- 最小改动路径：整段策略文本放进 `AdvisorTool.Description()` —— `prompt.Manager.BuildEffectiveSystemPrompt`（`pkg/prompt/prompt.go:76`）会把工具描述带入 system prompt，无需改 prompt 包。
- 若描述过长影响工具列表可读性，退一步：Description() 放一句话摘要，完整策略段在 `buildDefaultSystemPrompt` 中检测到 advisor 已注册时追加（仿 `buildMCPGuidance`，`pkg/prompt/prompt.go:332`）。
- 文本保留官方英文原文（主模型对英文指令遵循更稳），或按 kimi 实测效果中文化。

#### (5) 配置 — `pkg/settings/settings.go` + feishu 配置

`Settings` / `AISection` 平级新增：

```json
"advisor": {
  "enabled": true,
  "modelAlias": "kimi-k2-thinking",
  "maxCallsPerSession": 8,
  "cooldownTurns": 2,
  "maxTranscriptTokens": 60000,
  "maxOutputTokens": 2000
}
```

- `modelAlias` 走现有 alias→ProvidersSection 解析，构造独立 `api.Client`（advisor 可指向不同 provider/baseURL）。
- CLI：`--advisor <alias>` flag（`cmd/claude-go/main.go` 全局标志区，对齐官方）。
- 校验：alias 解析失败 → 启动时 warn 并禁用 advisor（不 fail）；advisor 模型与主模型相同 → warn 但允许（self-consult 仍有"跳出当前思路"价值，官方不允许是因为白名单，claude-go 无此约束）。

#### (6) 注册接线

- `RegisterBaseToolsWithStore` 增加可选参数或独立 `RegisterAdvisorTool(reg, client, opts)`；
- 各装配点（`cmd/claude-go` chat/run、feishu bot 会话工厂、team/orchestrator 的 agent 装配）读 settings，enabled 且 client 构造成功才注册——**未注册时主模型完全看不到该工具**，行为与现状零差异。

### 3.3 会话流与渲染

- advisor 的 tool_use/tool_result 走标准消息流 → session JSONL 天然持久化，compact 时同普通工具结果一起被压缩，无特殊处理。
- feishu/dashboard 渲染：tool_use name=advisor → 折叠显示 `🧭 已咨询 advisor (<model>)`；verbose/dashboard 展开建议全文（对齐官方 "✓ Advisor has reviewed..."）。

### 3.4 护栏（官方由服务端承担，claude-go 必须自己做）

| 风险 | 护栏 |
|------|------|
| advisor 依赖循环（每轮都问） | maxCallsPerSession + cooldownTurns；超限返回非 error 的"预算耗尽，自行判断" |
| 递归（advisor 再调工具/advisor） | advisor 调用 tools=nil，结构上不可能递归 |
| advisor 挂掉拖死主循环 | 独立超时（复用 Client.CallTimeout）；任何失败 → IsError tool_result，主循环继续 |
| transcript 超 advisor 上下文 | 3.2(2) 保头保尾截断；收到 prompt_too_long 类错误再对半折减重试一次 |
| 成本失控 | purpose="advisor" 遥测单列；dashboard 可观测（对齐官方 tengu_advisor_tool_token_usage） |
| OpenAI 兼容模型对空参数工具调用积极性异常（不调/狂调） | 引导 prompt 中的频率规则 + cooldown 兜底；E2E 实测 kimi 行为后调参 |

### 3.5 Phase 3（可选）：Push 模式 — 自治场景强制审视

官方没有、但契合 claude-go 自治长任务的增量：

- 新增 `internal_hook.AdvisorCheckpointHook`，挂 PhasePostResponse：
  - 触发条件（或）：G3 LoopDetector 报警（工具循环）；连续 M 轮（默认 20）未调用过 advisor 且任务仍在进行；PostCompact 后首轮。
  - 动作：直接执行一次 advisor 调用，把建议以 user message（`<system-reminder>` 风格前缀）注入下一轮——不依赖主模型自觉。
- 团队场景：orchestrator 阶段间插入 advisor 审查步（与 AdversarialRunner 互补：advisor 单次轻量看方向，adversarial 多轮重度磨产物）。

### 3.6 分期与验收

**Phase 1（MVP）**
- advisor.go（工具 + transcript + 人设 prompt + 护栏）、settings、注册接线、引导 prompt 注入。
- 单测：transcript 截断预算（构造超长历史验证保头保尾）、护栏（超限/冷却/失败降级各返回值）、空历史。
- E2E：chat 会话给 kimi 主模型一个多步任务，验证 (a) 它会在动手前调 advisor，(b) advisor 建议出现在轨迹中且主模型行为受其影响，(c) advisor client 断网时主循环正常继续。

**Phase 2**
- feishu/dashboard 渲染、`/advisor` 运行时开关命令、purpose 遥测面板、prompt 按 kimi 实测调优、参数精调。

**Phase 3**
- AdvisorCheckpointHook push 模式 + LoopDetector 联动 + cron/team 自治场景接入。

### 3.7 明确不做

- 不实现 `advisor_20260301` 服务端协议 block 类型（claude-go 消息流里它就是普通 tool_use/tool_result，无需新 content block 类型，也就无需官方的 advisor-strip 重试逻辑）。
- 不做模型白名单（claude-go 后端异构，交给配置校验 + warn）。
- 不做 `advisor_redacted_result`（无服务端脱敏语义）。
