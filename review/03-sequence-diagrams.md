# Ruflo V3 时序图详解

## 1. Claude Code 发起 MCP 工具调用（核心流程）

这是最核心的交互流程：Claude Code 通过 MCP 协议调用 Ruflo 的工具。

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '13px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6', 'noteTextColor': '#1a1a2e', 'noteBorderColor': '#d4a017'}}}%%
sequenceDiagram
    participant CC as 🖥️ Claude Code<br/>(Cursor IDE)
    participant MCP_STDIO as 📡 MCP stdio 循环<br/>(bin/cli.js)
    participant REGISTRY as 📦 TOOL_REGISTRY<br/>(mcp-client.ts)
    participant HANDLER as ⚙️ Tool Handler<br/>(mcp-tools/*.ts)
    participant DISK as 💾 磁盘存储<br/>(.claude-flow/)

    Note over CC: 用户在 Cursor 中提问<br/>Claude Code 决定调用 MCP 工具

    CC->>MCP_STDIO: JSON-RPC 请求<br/>{"method":"tools/call","params":{"name":"memory_search","arguments":{...}}}

    Note over MCP_STDIO: bin/cli.js 检测到 stdin 非 TTY<br/>进入 JSON-RPC stdio 模式

    MCP_STDIO->>REGISTRY: callMCPTool('memory_search', args)
    REGISTRY->>REGISTRY: TOOL_REGISTRY.get('memory_search')
    REGISTRY->>HANDLER: tool.handler(input, context)

    Note over HANDLER: memory-tools.ts 处理器<br/>懒加载 memory-initializer

    HANDLER->>DISK: 读取 .claude-flow/memory.db
    DISK-->>HANDLER: 查询结果
    HANDLER-->>REGISTRY: { content: [{type:'text', text: JSON结果}] }
    REGISTRY-->>MCP_STDIO: MCP 响应
    MCP_STDIO-->>CC: JSON-RPC 响应<br/>{"result":{"content":[...]}}

    Note over CC: Claude Code 将结果<br/>整合到回答中
```

## 2. 提示词注入与上下文构建流程

展示 Claude Code 如何通过 Ruflo 构建增强的上下文和提示词。

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '13px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6', 'noteTextColor': '#1a1a2e', 'noteBorderColor': '#d4a017'}}}%%
sequenceDiagram
    participant USER as 👤 用户
    participant CC as 🖥️ Claude Code
    participant GUIDE as 📜 Guidance<br/>Control Plane
    participant HOOKS as 🪝 Hooks<br/>System
    participant MEM as 🧠 Memory<br/>Service
    participant LLM as 🤖 Claude API

    USER->>CC: 输入任务请求

    Note over CC: 1️⃣ 加载 CLAUDE.md 作为系统提示<br/>这是 Cursor/Claude Code 内建机制<br/>不经过 Ruflo

    rect rgb(240, 247, 232)
    Note over CC, HOOKS: 2️⃣ Pre-Task 钩子阶段
    CC->>HOOKS: callMCPTool('hooks_pre-task',<br/>{description: "用户任务"})
    HOOKS->>HOOKS: 复杂度评估
    HOOKS->>HOOKS: Agent 路由建议<br/>(AGENT_PATTERNS 正则匹配)
    HOOKS->>HOOKS: 3 层模型路由<br/>(ADR-026 Tier 1/2/3)
    HOOKS-->>CC: {suggestedAgents, complexity,<br/>modelRouting: "haiku/sonnet/opus"}
    end

    rect rgb(232, 240, 247)
    Note over CC, MEM: 3️⃣ 内存检索阶段
    CC->>MEM: callMCPTool('memory_search',<br/>{query: "相关关键词"})
    MEM->>MEM: 生成查询嵌入向量<br/>(MiniLM-L6 384维)
    MEM->>MEM: HNSW 最近邻搜索<br/>(150x-12500x 加速)
    MEM-->>CC: {results: [{key, value,<br/>similarity: 0.85}]}
    end

    rect rgb(247, 232, 240)
    Note over CC, GUIDE: 4️⃣ 治理检索阶段
    CC->>GUIDE: callMCPTool('guidance_retrieve',<br/>{task: "任务意图"})
    GUIDE->>GUIDE: ShardRetriever.retrieve()<br/>按意图分类提取规则分片
    GUIDE-->>CC: {shards: [相关治理规则],<br/>constitution: "核心约束"}
    end

    Note over CC: 5️⃣ Claude Code 组装最终消息<br/>= 系统提示(CLAUDE.md)<br/>+ 治理分片<br/>+ 内存模式<br/>+ 用户输入<br/>+ 工具定义

    CC->>LLM: Anthropic API /v1/messages<br/>{system: "CLAUDE.md + 治理规则",<br/> messages: [用户输入 + 上下文],<br/> tools: [MCP 工具定义]}

    LLM-->>CC: 模型响应 + tool_use

    Note over CC: 6️⃣ 可能触发更多 MCP 调用<br/>(递归工具使用循环)
```

## 3. LLM 调用的钩子拦截流程

展示 Hooks 系统如何在 LLM 调用前后进行优化和学习。

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '13px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6'}}}%%
sequenceDiagram
    participant CALLER as 📞 调用方
    participant PRE as 🪝 preLLMCallHook
    participant CACHE as 💿 Response Cache
    participant OPTIM as ⚡ Provider<br/>Optimizations
    participant LLM as 🤖 LLM API
    participant POSTHOOK as 🪝 postLLMCallHook
    participant RB as 🧠 ReasoningBank
    participant METRICS as 📊 Metrics

    CALLER->>PRE: preLLMCallHook payload context

    PRE->>CACHE: generateCacheKey provider model request
    alt 缓存命中
        CACHE-->>PRE: cached response
        PRE-->>CALLER: continue false cachedResponse
        Note over CALLER: 跳过 LLM 调用<br/>直接使用缓存
    else 缓存未命中
        CACHE-->>PRE: undefined
        PRE->>OPTIM: loadProviderOptimizations
        Note over OPTIM: anthropic temperature 0.7<br/>openai temperature 0.8<br/>Be concise and direct
        OPTIM-->>PRE: optimized request
        PRE->>METRICS: 记录 llm.calls.provider.model
        PRE-->>CALLER: continue true payload optimized
    end

    CALLER->>LLM: actual API call

    LLM-->>CALLER: response

    CALLER->>POSTHOOK: postLLMCallHook payload context
    POSTHOOK->>CACHE: setCache key response
    POSTHOOK->>METRICS: 记录延迟 token使用 成本

    alt 响应长度大于阈值
        POSTHOOK->>RB: extractPatternFromResponse
        RB->>RB: reasoningBank.storePattern
        Note over RB: 从长回复中提取<br/>有价值的模式<br/>存入向量数据库
    end

    POSTHOOK-->>CALLER: continue true sideEffects
```

## 4. Agent Swarm 编排流程

展示从 Claude Code 发起 Swarm 到 Agent 并行执行的完整流程。

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '13px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6'}}}%%
sequenceDiagram
    participant USER as 👤 用户
    participant CC as 🖥️ Claude Code
    participant SWARM as 🐝 Swarm Tools
    participant AGENT as 🤖 Agent Tools
    participant HOOKS as 🪝 Hooks Tools
    participant MEM as 🧠 Memory Tools
    participant DISK as 💾 .claude-flow/

    USER->>CC: "实现用户认证功能"

    Note over CC: 检测到复杂任务<br/>自动触发 Swarm 协议

    rect rgb(240, 247, 232)
    Note over CC, DISK: 步骤 1: 初始化 Swarm 协调
    CC->>SWARM: callMCPTool('swarm_init',<br/>{topology:'hierarchical',<br/>maxAgents:8, strategy:'specialized'})
    SWARM->>DISK: 写入 .claude-flow/swarm/swarm-state.json
    SWARM-->>CC: {swarmId, status: 'initialized'}
    end

    rect rgb(232, 240, 247)
    Note over CC, DISK: 步骤 2: 注册 Agent
    par 并行注册
        CC->>AGENT: agent_spawn({type:'coordinator'})
        AGENT->>DISK: 写入 .claude-flow/agents/store.json
    and
        CC->>AGENT: agent_spawn({type:'coder'})
    and
        CC->>AGENT: agent_spawn({type:'tester'})
    and
        CC->>AGENT: agent_spawn({type:'reviewer'})
    end
    AGENT-->>CC: {agents: [coord, coder, tester, reviewer]}
    end

    rect rgb(247, 240, 232)
    Note over CC, MEM: 步骤 3: 搜索历史模式
    CC->>MEM: memory_search({query: "authentication"})
    MEM-->>CC: 相似模式结果
    end

    rect rgb(247, 232, 240)
    Note over CC: 步骤 4: 并行生成子 Agent<br/>通过 Claude Code Task 工具

    par Claude Code 内部并行
        CC->>CC: Task(Architect, "设计认证架构")
    and
        CC->>CC: Task(Coder, "实现认证逻辑")
    and
        CC->>CC: Task(Tester, "编写测试")
    and
        CC->>CC: Task(Reviewer, "代码审查")
    end
    end

    rect rgb(232, 247, 240)
    Note over CC, MEM: 步骤 5: 记录完成
    CC->>HOOKS: hooks_post-task({taskId, success: true})
    CC->>MEM: memory_store({key:'pattern-auth',<br/>value:'成功方案', namespace:'patterns'})
    end
```

## 5. 治理门控执行流程

展示 Guidance 如何在操作执行前进行安全门控检查。

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '13px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6'}}}%%
sequenceDiagram
    participant CC as 🖥️ Claude Code
    participant HOOK_REG as 📋 HookRegistry
    participant GATES as 🚧 EnforcementGates
    participant RETRIEVER as 🔍 ShardRetriever
    participant LEDGER as 📔 RunLedger
    participant OPTLOOP as 🔄 OptimizerLoop

    Note over CC: Claude Code 准备执行命令<br/>例如: rm -rf /important

    CC->>HOOK_REG: PreCommand 事件触发

    HOOK_REG->>GATES: evaluateCommand("rm -rf /important")
    GATES->>GATES: 检查危险命令列表
    GATES->>GATES: 检查秘密泄露
    GATES-->>HOOK_REG: GateResult: {decision: 'block',<br/>reason: '危险的破坏性命令',<br/>remediation: '请使用安全的替代方案'}

    HOOK_REG->>HOOK_REG: gateResultsToHookResult()
    Note over HOOK_REG: block > require-confirmation<br/>> warn > allow

    HOOK_REG-->>CC: HookResult: {success: false,<br/>abort: true, error: '...'}

    Note over CC: 操作被阻止 ❌

    CC->>HOOK_REG: PreTask 事件 (新任务开始)
    HOOK_REG->>RETRIEVER: retrieve({intent: 'security'})
    RETRIEVER->>RETRIEVER: 意图分类<br/>语义嵌入搜索
    RETRIEVER-->>HOOK_REG: RetrievalResult:<br/>{shards: [安全相关规则]}
    HOOK_REG-->>CC: 注入相关治理分片

    Note over CC: 任务完成后

    CC->>HOOK_REG: PostTask 事件
    HOOK_REG->>LEDGER: finalizeEvent(runEvent)
    LEDGER->>LEDGER: 评估器打分<br/>(TestsPass, ForbiddenCmd, DiffQuality...)
    LEDGER-->>OPTLOOP: 违规数据

    OPTLOOP->>OPTLOOP: 分析违规排名
    OPTLOOP->>OPTLOOP: 提升胜出的本地规则到根规则
    Note over OPTLOOP: CLAUDE.local.md 到 CLAUDE.md<br/>规则自进化
```

## 6. 自进化学习循环（SONA Pipeline）

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '13px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6'}}}%%
sequenceDiagram
    participant TASK as 📝 任务执行
    participant SONA as 🧠 SONA<br/>Coordinator
    participant RB as 📚 ReasoningBank
    participant HNSW as 🔍 HNSW Index
    participant EWC as 🛡️ EWC++<br/>Consolidator
    participant PERSIST as 💾 patterns.json

    Note over TASK, PERSIST: 4 步智能流水线: RETRIEVE → JUDGE → DISTILL → CONSOLIDATE

    rect rgb(232, 247, 240)
    Note over TASK, HNSW: 1️⃣ RETRIEVE — 检索相关模式
    TASK->>RB: findSimilarPatterns(embedding)
    RB->>HNSW: search(embedding, k=10)
    Note over HNSW: O(log n) 搜索<br/>150x-12,500x 加速
    HNSW-->>RB: 最近邻结果
    RB-->>TASK: 相似模式 + 相似度分数
    end

    rect rgb(247, 240, 232)
    Note over TASK, SONA: 2️⃣ JUDGE — 判定与奖励
    TASK->>TASK: 执行任务...
    TASK->>SONA: endTrajectory(verdict, bank)
    Note over SONA: verdict: success/failure<br/>+ 奖励信号 shaping
    SONA->>SONA: recordTrajectory({<br/>  steps, outcome, reward<br/>})
    end

    rect rgb(240, 232, 247)
    Note over SONA, PERSIST: 3️⃣ DISTILL — 提炼学习
    SONA->>SONA: distillLearning()
    Note over SONA: LoRA 风格置信度更新:<br/>pattern.confidence +=<br/>learningRate * reward * (1 - confidence)
    SONA->>RB: storePattern(newPattern)
    RB->>HNSW: addPoint(embedding)
    RB->>PERSIST: 持久化模式
    end

    rect rgb(247, 232, 240)
    Note over SONA, EWC: 4️⃣ CONSOLIDATE — 防止遗忘
    SONA->>EWC: consolidatePatterns(patterns)
    Note over EWC: Elastic Weight Consolidation:<br/>保护重要模式不被覆盖<br/>Fisher 信息矩阵近似
    EWC->>PERSIST: 更新 patterns.json
    end

    Note over TASK, PERSIST: ✅ 循环完成<br/>下次任务将受益于<br/>本次学习的模式
```

## 7. MCP 工具注册与双入口路由

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '13px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6'}}}%%
sequenceDiagram
    participant CC as 🖥️ Claude Code
    participant MCP_ADD as claude mcp add
    participant BIN as bin/cli.js
    participant STDIN as stdin 检测
    participant MCP_LOOP as MCP stdio 循环
    participant CLI_CLASS as CLI.run()
    participant REG as TOOL_REGISTRY

    Note over CC, MCP_ADD: 一次性注册 MCP 服务器
    CC->>MCP_ADD: claude mcp add claude-flow<br/>-- npx -y @claude-flow/cli@latest

    Note over CC, REG: 后续每次调用

    CC->>BIN: 通过 stdin 管道发送 JSON-RPC

    BIN->>STDIN: process.stdin.isTTY?

    alt stdin 是管道（MCP 模式）
        STDIN->>MCP_LOOP: 启动 JSON-RPC 处理器
        MCP_LOOP->>REG: import listMCPTools, callMCPTool
        Note over MCP_LOOP: 读取 stdin 行<br/>解析 JSON-RPC<br/>分发到工具处理器
        MCP_LOOP->>REG: callMCPTool(name, args)
        REG-->>MCP_LOOP: 工具结果
        MCP_LOOP-->>CC: JSON-RPC 响应写入 stdout
    else stdin 是 TTY（CLI 模式）
        STDIN->>CLI_CLASS: new CLI().run()
        CLI_CLASS->>CLI_CLASS: CommandParser.parse(argv)
        CLI_CLASS->>REG: 命令最终也调用 callMCPTool
        REG-->>CLI_CLASS: 结果
        CLI_CLASS-->>BIN: 格式化输出到终端
    end
```

## 8. 3 层模型路由决策流程（ADR-026）

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '13px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6'}}}%%
sequenceDiagram
    participant CC as 🖥️ Claude Code
    participant HOOK as 🪝 hooks_pre-task
    participant ROUTER as 🔀 Enhanced<br/>Model Router
    participant T1 as ⚡ Tier 1<br/>Agent Booster
    participant T2 as 🚀 Tier 2<br/>Haiku
    participant T3 as 🧠 Tier 3<br/>Sonnet/Opus

    CC->>HOOK: hooks_pre-task({description: "变量改 const"})
    HOOK->>ROUTER: route(task)

    ROUTER->>ROUTER: 意图检测<br/>(正则/关键词匹配)

    alt 简单转换 (var→const, add-types...)
        ROUTER-->>HOOK: [AGENT_BOOSTER_AVAILABLE]<br/>intent: 'var-to-const'
        HOOK-->>CC: Tier 1: 跳过 LLM
        CC->>T1: 直接使用 Edit 工具<br/>0 成本, <1ms
    else 低复杂度 (<30%)
        ROUTER-->>HOOK: [TASK_MODEL_RECOMMENDATION]<br/>model: "haiku"
        HOOK-->>CC: Tier 2: 使用 Haiku
        CC->>T2: Task({model: "haiku",...})<br/>$0.0002, ~500ms
    else 高复杂度 (>30%)
        ROUTER-->>HOOK: [TASK_MODEL_RECOMMENDATION]<br/>model: "sonnet" / "opus"
        HOOK-->>CC: Tier 3: 使用 Sonnet/Opus
        CC->>T3: Task model sonnet<br/>$0.003-$0.015, 2-5s
    end
```

## 9. 完整端到端流程：用户发送"开发一个数据库系统"（深度解析版）

本节展示从用户一句话到最终交付的 **全链路运作机制**，深度剖析以下核心问题：

1. **Swarm 只是记录配置** — Claude 如何做任务拆分和分配？
2. **哪些 Agent 是 Claude 内部的**？它们怎么跟 MCP 交互？
3. **Agent 如何使用 LLM**？怎么注入提示词与上下文历史？
4. **Agent 间如何共享传递信息**？协调者是不是也有个 Agent？
5. **任务依赖、进度检测、失败处理、持续决策**的完整机制

---

### 9.1 架构真相：谁负责什么

> **核心真相**：Ruflo MCP 工具**只负责状态记录与建议**（Agent 注册、内存、模式搜索），**不执行代码**。
> 真正的任务拆分、Agent 创建、LLM 调用都由 **Claude Code 客户端内部**完成。

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '14px', 'fontFamily': 'Arial, sans-serif', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6', 'noteTextColor': '#1a1a2e', 'noteBorderColor': '#d4a017'}}}%%
flowchart TB
    subgraph Claude_Internal ["**Claude Code 内部（真实执行层）**"]
        direction TB
        QLoop["**query.ts queryLoop**<br/>主 Agentic 循环"]
        TaskTool["**Task 工具**<br/>创建子 Agent（子进程/InProcess）"]
        SubAgent1["**子 Agent: Architect**<br/>独立 queryLoop + LLM"]
        SubAgent2["**子 Agent: Coder**<br/>独立 queryLoop + LLM"]
        SubAgent3["**子 Agent: Tester**<br/>独立 queryLoop + LLM"]
        Mailbox["**teammateMailbox**<br/>文件 JSON 邮箱"]
        TaskList["**tasks.ts**<br/>文件锁共享任务列表"]
    end
    subgraph MCP_Server ["**Ruflo MCP 服务器（状态记录层）**"]
        direction TB
        HooksTool["**hooks_pre-task**<br/>复杂度+Agent推荐+路由"]
        MemTool["**memory_store/search**<br/>共享内存命名空间"]
        SwarmTool["**swarm_init**<br/>拓扑+协调器配置"]
        AgentTool["**agent_spawn**<br/>Agent 注册记录"]
        Queen["**UnifiedSwarmCoordinator**<br/>心跳+健康度监控"]
    end

    QLoop -->|"tool_use"| HooksTool
    QLoop -->|"tool_use"| MemTool
    QLoop -->|"tool_use"| SwarmTool
    QLoop -->|"tool_use"| AgentTool
    QLoop -->|"生成子 Agent"| TaskTool
    TaskTool -->|"spawn"| SubAgent1
    TaskTool -->|"spawn"| SubAgent2
    TaskTool -->|"spawn"| SubAgent3
    SubAgent1 -->|"memory_store"| MemTool
    SubAgent2 -->|"memory_search"| MemTool
    SubAgent3 -->|"memory_search"| MemTool
    SubAgent1 -.->|"文件邮箱"| Mailbox
    AgentTool -->|"RegisterAgent"| Queen

    style Claude_Internal fill:#f0f7e8,stroke:#6aa84f,stroke-width:2px
    style MCP_Server fill:#e8f0f7,stroke:#4a90d9,stroke-width:2px
```

---

### 9.2 Claude Code 如何做任务拆分和分配

**任务拆分不是算法自动完成的，而是 LLM 推理决策的结果。** 整个过程如下：

1. Claude Code 加载 **CLAUDE.md** 作为 system prompt，其中包含完整的编排规则（如"检测到复杂任务时自动触发 Swarm 协议"）
2. Claude Code 的 **queryLoop**（`query.ts`）将用户消息 + 系统提示 + MCP 工具定义发给 Claude API
3. LLM 基于系统提示中的规则**推理判断**：这是复杂任务 → 应该调 `hooks_pre-task` → 应该初始化 Swarm → 应该创建多个子 Agent
4. LLM 返回 `tool_use` 块，Claude Code 的 **`runTools`** 执行这些 MCP 调用
5. MCP 工具返回建议（推荐 Agent 类型、复杂度、模型路由），**LLM 读取这些建议做出最终决策**
6. LLM 决定生成 N 个 **Task 工具调用**，每个 Task 包含完整的子任务描述

**关键：`CLAUDE.md` 中的规则（如 Agent 路由表、复杂度检测、Swarm 配方）是"驱动 LLM 行为的提示词"，不是可执行代码。**

---

### 9.3 子 Agent 的真实身份

**子 Agent 是 Claude Code 的 Task 工具创建的子进程或同进程实例**，不是 MCP 中的记录。

| 概念 | MCP 侧（Ruflo） | Claude 内部 |
|------|------------------|-------------|
| `agent_spawn` | JSON 注册记录 + 路由元数据 | — |
| Task 工具 | — | 创建真实子 Agent（`runAgent` → `queryLoop`） |
| 子 Agent 进程 | — | InProcess（同进程 AsyncLocalStorage 隔离）或 Tmux/iTerm 独立进程 |
| 子 Agent 的 LLM | — | 每个子 Agent 独立调用 Claude API，有自己的 `messages[]` |

**子 Agent 的提示词注入方式**（源码：`utils/swarm/inProcessRunner.ts`）：
1. **系统提示** = `buildEffectiveSystemPrompt()`，优先级链：override > coordinator > agent定义 > default
2. **default** 部分由 `getSystemPrompt()` 拼装，包含：session guidance + memory prompt + env + MCP 工具说明 + token budget
3. **队友追加段** = `TEAMMATE_SYSTEM_PROMPT_ADDENDUM`（强调必须用 `SendMessage` 通信）
4. **首条用户消息** = 主 Agent 给出的**完整任务描述 + 上下文**（含 MCP 搜索到的历史模式）

---

### 9.4 完整时序图（深度版）

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '12px', 'fontFamily': 'Arial', 'actorBkg': '#e8f4fd', 'actorTextColor': '#1a1a2e', 'actorBorder': '#4a90d9', 'noteBkgColor': '#fff5e6', 'noteTextColor': '#1a1a2e', 'noteBorderColor': '#d4a017'}}}%%
sequenceDiagram
    participant U as 👤 用户
    participant CC as 🖥️ Claude Code<br/>queryLoop
    participant LLM as 🤖 Claude API
    participant HOOK as 🪝 Hooks MCP
    participant MEM as 🧠 Memory MCP
    participant SW as 🐝 Swarm MCP
    participant AG as 🤖 Agent MCP
    participant COORD as 📋 Coordinator<br/>(Go后台goroutine)
    participant W1 as 🏗️ Architect<br/>子Agent(Task)
    participant W2 as 💻 Coder<br/>子Agent(Task)
    participant W3 as 🧪 Tester<br/>子Agent(Task)
    participant DISK as 💾 存储层

    U->>CC: "开发一个数据库系统"

    Note over CC: ❶ 系统上下文加载<br/>① buildEffectiveSystemPrompt()<br/>  = getSystemPrompt(tools, model) + claudemd<br/>② 发现所有 MCP 工具 (tools/list)<br/>③ 构建 messages = [system + user_input]

    rect rgb(240, 247, 232)
    Note over CC,MEM: ❷ Agentic Loop 第1轮 — LLM 推理 + 预处理
    CC->>LLM: callModel({system, messages, tools})
    Note over LLM: LLM 读取 CLAUDE.md 中的规则:<br/>"复杂任务自动触发 Swarm 协议"<br/>"AUTO-INVOKE SWARM when 3+ files"<br/>判定: 这是复杂任务 → 先调 hooks

    LLM-->>CC: tool_use: hooks_pre-task<br/>{description: "开发数据库系统"}
    Note over CC: runTools() 执行 MCP 调用

    CC->>HOOK: hooks_pre-task(description)
    HOOK->>HOOK: defaultPreTaskHandler:<br/>① 关键词匹配 → suggested_agents<br/>② 复杂度评估 → 0.85 (HIGH)<br/>③ ADR-026 路由 → tier:3 sonnet/opus<br/>④ ReasoningBank 模式检索<br/>⑤ 风险识别: 涉及数据库变更
    HOOK-->>CC: {suggested_agents, complexity:0.85,<br/>model_routing:{tier:3, model:"sonnet/opus"},<br/>risks, patterns}

    Note over CC: tool_result 回到 messages<br/>进入下一轮 queryLoop

    CC->>LLM: [system + user + hook结果]
    LLM-->>CC: tool_use: memory_search<br/>{query: "数据库系统 存储引擎 索引"}

    CC->>MEM: memory_search(query, namespace=patterns)
    MEM->>DISK: HNSW 向量搜索 / SQLite
    DISK-->>MEM: 匹配结果
    MEM-->>CC: {results: [{key:"pattern-db",<br/>value:"B+Tree+WAL", score:0.78}]}
    end

    rect rgb(232, 240, 247)
    Note over CC,AG: ❸ Agentic Loop 第2轮 — Swarm 初始化 + Agent 注册
    CC->>LLM: [system + user + hook + memory结果]
    Note over LLM: LLM 根据 hooks 建议决定:<br/>1. 初始化 hierarchical swarm<br/>2. 注册 3 个 Agent<br/>3. 创建 3 个带依赖的 task

    LLM-->>CC: tool_use x7: swarm_init +<br/>agent_spawn x3 + task_create x3

    par 并行 MCP 调用 (partitionToolCalls 只读并行)
        CC->>SW: swarm_init(topology=hierarchical,<br/>maxAgents=8, strategy=specialized)
        SW->>COORD: NewUnifiedSwarmCoordinator<br/>→ Initialize → healthMonitorLoop
        SW->>DISK: 写入 swarm-state.json
        SW-->>CC: {swarmId, status: initialized}
    and
        CC->>AG: agent_spawn(type=architect, name=arch)
        AG->>AG: 生成 agentId, 写 agents/store.json
        AG->>COORD: coord.RegisterAgent(agent)
        AG-->>CC: {agentId:arch-1, coordinator_sync:synced}
    and
        CC->>AG: agent_spawn(type=coder, name=coder)
        AG->>COORD: coord.RegisterAgent(agent)
        AG-->>CC: {agentId:code-1}
    and
        CC->>AG: agent_spawn(type=tester, name=tester)
        AG->>COORD: coord.RegisterAgent(agent)
        AG-->>CC: {agentId:test-1}
    end

    Note over CC: 串行创建任务 (带依赖关系)

    CC->>SW: task_create(title=设计存储引擎架构)
    SW->>HOOK: hookExec.Execute(PreTask)
    SW-->>CC: {taskId:task-1, analysis:{...}}

    CC->>SW: task_create(title=实现核心代码,<br/>depends_on=[task-1])
    SW-->>CC: {taskId:task-2, depends_on:[task-1]}

    CC->>SW: task_create(title=编写测试套件,<br/>depends_on=[task-1])
    SW-->>CC: {taskId:task-3, depends_on:[task-1]}
    end

    rect rgb(247, 232, 240)
    Note over CC,W3: ❹ 任务拆分 — Claude Code Task 工具生成子 Agent
    CC->>LLM: [全部 MCP 结果 + 继续推理]
    Note over LLM: LLM 产出 3 个 Task 工具调用<br/>每个 Task 包含完整的子任务提示词:<br/>= 任务描述 + 历史模式 + 约束规则<br/>Task 工具是 Claude 内建的, 不走 MCP

    LLM-->>CC: tool_use: Task x3

    par Claude Code 内部 Task 并行生成
        Note over W1: 【Architect 子 Agent 内部流程】<br/>① spawnInProcess/Tmux<br/>② 独立 queryLoop + 独立 LLM 会话<br/>③ system prompt = default + teammate追加段<br/>④ 首条消息 = 主 Agent 给的任务描述

        CC->>W1: Task("设计数据库存储引擎架构,<br/>参考历史模式: B+Tree+WAL,<br/>存入 memory namespace=collaboration")

        W1->>LLM: 独立 callModel<br/>{system: 子Agent专用prompt,<br/> messages: [任务描述]}
        LLM-->>W1: B+Tree 存储引擎方案

        Note over W1: 子 Agent 也有自己的 agentic loop<br/>可调用 MCP 工具

        W1->>MEM: memory_store(key=db-design,<br/>value=B+Tree+WAL方案,<br/>namespace=collaboration)
        MEM->>DISK: SQLite 持久化

        W1->>SW: task_assign(id=task-1, agent_id=arch-1)
        Note over SW: 验证无依赖阻塞 → Agent→Busy

        W1->>SW: task_update(id=task-1, progress=100)
        W1->>SW: task_complete(id=task-1)
        Note over SW: task→Succeeded, Agent→Idle

        W1-->>CC: 架构设计文档
    and
        CC->>W2: Task("按架构实现核心代码,<br/>先 memory_search 获取设计方案,<br/>namespace=collaboration")

        W2->>MEM: memory_search(query=db-design,<br/>namespace=collaboration)
        MEM-->>W2: Architect 存入的设计方案

        Note over W2: 尝试分配 task-2

        W2->>SW: task_assign(id=task-2, agent_id=code-1)
        Note over SW: 检查 depends_on:[task-1]<br/>若 task-1 未完成 → blocked:true<br/>Coder 轮询等待

        W2->>LLM: 根据设计方案生成代码
        LLM-->>W2: 存储引擎实现

        W2->>SW: task_update(id=task-2, progress=100)
        W2->>SW: task_complete(id=task-2)
        W2-->>CC: src/engine.ts src/index.ts
    and
        CC->>W3: Task("编写数据库系统测试套件,<br/>先 memory_search 获取设计方案")

        W3->>MEM: memory_search(query=数据库测试模式)
        MEM-->>W3: 历史测试模式

        W3->>SW: task_assign(id=task-3, agent_id=test-1)
        Note over SW: 检查 depends_on:[task-1]<br/>task-1 完成 → 允许分配

        W3->>LLM: 生成测试代码
        LLM-->>W3: 测试套件
        W3->>SW: task_complete(id=task-3)
        W3-->>CC: tests/engine.test.ts
    end
    end

    rect rgb(232, 247, 240)
    Note over CC,DISK: ❺ 协调器后台监控（与上述并行运行）
    Note over COORD: Go goroutine 后台运行:<br/>healthMonitorLoop (HeartbeatMS间隔)<br/>① 遍历 c.agents 检查 LastHeartbeat<br/>② 检查 stale agent (超过阈值)<br/>③ 计算 domainHealth / agentHealth<br/>④ 检测队列深度瓶颈<br/>⑤ 生成 HealthReport

    COORD->>COORD: tickHealth()<br/>status: 所有Agent=Idle<br/>无超时/无失败
    end

    rect rgb(247, 247, 232)
    Note over CC,DISK: ❻ 结果整合 + SONA 学习闭环
    Note over CC: 3 个子 Agent 全部返回<br/>queryLoop 收集所有 tool_result

    CC->>LLM: [system + 全部历史 + 3个Agent结果]
    LLM-->>CC: 综合回答 + 代码文件清单

    CC->>HOOK: hooks_post-task(success=true)
    HOOK->>HOOK: defaultPostTaskHandler:<br/>① SONA.RecordSignal(post_task_success)<br/>② 学习记录: patterns_updated, confidence

    CC->>MEM: memory_store(namespace=patterns,<br/>key=pattern-db-system,<br/>value=B+Tree+WAL成功方案)
    MEM->>DISK: 持久化成功模式
    end

    CC-->>U: 完成! 已创建数据库系统<br/>包含存储引擎 B+Tree索引 WAL日志<br/>以及完整测试套件

    Note over U,DISK: ━━━━━━━━━━ 用户反馈 ━━━━━━━━━━

    U->>CC: "查询性能太差了 需要优化"

    rect rgb(255, 243, 224)
    Note over CC,DISK: ❼ 反馈驱动 — 动态追加 Agent

    CC->>LLM: 用户反馈 + 完整历史上下文
    LLM-->>CC: tool_use: hooks_route

    CC->>HOOK: hooks_route(task=优化查询性能)
    HOOK->>HOOK: HashEmbed384 → RouteTask<br/>ReasoningBank 向量相似度搜索<br/>匹配: performance-engineer (0.89)
    HOOK-->>CC: {agent:performance-engineer,<br/>tier:3, confidence:0.89}

    CC->>MEM: memory_search(query=数据库性能优化)
    MEM-->>CC: 历史优化模式: 缓存+索引优化

    CC->>AG: agent_spawn(type=performance-engineer)
    AG->>COORD: coord.RegisterAgent(perf-agent)
    AG-->>CC: {agentId:perf-1, coordinator_sync:synced}

    Note over CC: LLM 决定创建新 Task<br/>携带历史模式 + 反馈上下文

    CC->>W2: Task("优化数据库查询性能,<br/>参考模式: 缓存+索引优化,<br/>用户反馈: 查询太慢")
    W2->>LLM: 优化请求 + 完整上下文
    LLM-->>W2: 查询计划缓存 + 索引提示优化
    W2-->>CC: 优化后的代码

    CC->>HOOK: hooks_post-task(success=true)
    HOOK->>HOOK: SONA.RecordSignal<br/>学习: performance优化模式
    CC->>MEM: memory_store(key=pattern-db-perf,<br/>value=查询计划缓存3x提升)
    end

    CC-->>U: 已优化! 添加了查询计划缓存<br/>预计提升3x查询性能
```

---

### 9.5 关键机制深度解析

#### A. Claude 如何做任务拆分

| 步骤 | 执行者 | 机制 | 源码位置 |
|------|--------|------|----------|
| 加载编排规则 | Claude Code | `CLAUDE.md` 中的 Swarm 配方和 Agent 路由表作为 system prompt | `utils/claudemd.ts` |
| 复杂度判定 | LLM | 基于 system prompt 中的规则推理，决定是否触发 Swarm | `query.ts` queryLoop |
| 预处理建议 | Ruflo MCP | `hooks_pre-task` 返回推荐 Agent、复杂度、模型路由 | `default_hooks.go` |
| 最终拆分决策 | LLM | 读取 MCP 建议，决定子任务数量和分配 | `query.ts` queryLoop |
| 子 Agent 创建 | Claude Code Task 工具 | `spawnInProcess` / `TmuxBackend`，每个子 Agent 有独立 queryLoop | `inProcessRunner.ts` |

#### B. 子 Agent 的 LLM 与提示词

```
子 Agent 提示词构成:
┌──────────────────────────────────────────────────┐
│ buildEffectiveSystemPrompt():                    │
│   ├── getSystemPrompt(tools, model)              │
│   │     ├── session guidance                     │
│   │     ├── loadMemoryPrompt() // memdir 记忆    │
│   │     ├── env info                             │
│   │     ├── MCP 工具说明                          │
│   │     ├── output style + FRC                   │
│   │     └── token budget                         │
│   ├── TEAMMATE_SYSTEM_PROMPT_ADDENDUM            │
│   │     └── "必须用 SendMessage 通信"              │
│   └── appendSystemPrompt (可选)                   │
├──────────────────────────────────────────────────┤
│ 首条用户消息 (主 Agent 传入):                      │
│   "设计数据库存储引擎架构,                          │
│    参考历史模式: B+Tree+WAL,                       │
│    存入 memory namespace=collaboration"            │
└──────────────────────────────────────────────────┘
```

每个子 Agent 有自己的 `messages[]` 状态，**独立调用 Claude API**，与主 Agent 完全隔离。

#### C. Agent 间信息共享

**三种通信方式**（按使用频率排序）：

| 方式 | 机制 | 适用场景 |
|------|------|----------|
| **共享内存命名空间** | `memory_store/search(namespace=collaboration)` via MCP | 任务数据传递：Architect 写设计，Coder 读设计 |
| **文件邮箱** | `teammateMailbox.ts`：`~/.claude/teams/{team}/inboxes/{agent}.json` | 控制面消息：权限请求、shutdown、进度通知 |
| **共享任务列表** | `tasks.ts`：文件+lockfile，`getTaskListId()` 保证团队一致 | 任务状态同步：leader 和 teammates 看到同一列表 |

**内存总线模式**：Architect 写入 `memory_store(key=db-design, namespace=collaboration)`，Coder 在稍后通过 `memory_search(namespace=collaboration)` 读取。这是**异步的**，不是实时消息传递。

#### D. 协调者角色

| 层面 | 角色 | 是否是 Agent | 实现 |
|------|------|-------------|------|
| **Claude 客户端** | 主 queryLoop | 不是独立 Agent，是主进程 | `query.ts` |
| **Ruflo MCP** | UnifiedSwarmCoordinator | Go 后台 goroutine，不是 MCP Agent | `pkg/swarm/coordinator.go` |
| **概念上的 Queen** | 健康监控 + 心跳检测 | 库内服务类，操作 coordinator 的 agents Map | `queen-coordinator.ts` (V3) / `coordinator.go` (Go) |

**协调器工作方式**：
1. `swarm_init` 创建 `UnifiedSwarmCoordinator`，启动后台 `healthMonitorLoop` goroutine
2. `agent_spawn` 调用 `coord.RegisterAgent()` 把 Agent 注册到协调器
3. 协调器按 `HeartbeatMS` 间隔 `tickHealth()`，遍历 `c.agents` 检查 `LastHeartbeat`
4. 协调器**不主动分配任务**，只做监控和状态报告
5. **真正的任务编排由 Claude Code 主 queryLoop（LLM 推理）驱动**

#### E. 任务依赖关系处理

```
task-1: 设计架构 (无依赖)
task-2: 实现代码 (depends_on: [task-1])
task-3: 编写测试 (depends_on: [task-1])

执行顺序:
  Level 0: [task-1]           ← Architect 立即执行
  Level 1: [task-2, task-3]   ← 等 task-1 完成后并行执行

task_assign 时的依赖检查:
  ① 遍历 td.DependsOn
  ② 依赖任务不存在 → missing_task_ids
  ③ 依赖任务未 Succeeded → pending_task_ids
  ④ 有阻塞 → 返回 blocked:true，不修改状态
```

#### F. 失败处理与状态流转

```
Agent 生命周期:
  idle → busy (task_assign) → idle (task_complete/cancel)
                             → stopped (agent_terminate)

Task 生命周期:
  pending → queued (task_assign) → succeeded (task_complete)
                                 → cancelled (task_cancel)
                                 → blocked (依赖未满足)

失败时:
  ① Claude LLM 收到子 Agent 错误返回
  ② LLM 推理决定: 重试/换 Agent/放弃
  ③ 可能: agent_spawn 新 Agent + task_create 新任务
  ④ hooks_post-task(success=false) → SONA 记录失败信号
```

---

### 9.6 阶段总览表

| 阶段 | 驱动者 | LLM 调用 | MCP 工具 | 内存操作 | 学习操作 |
|------|--------|----------|----------|---------|---------|
| **❶ 上下文加载** | Claude Code | — | tools/list | claudemd 加载 | — |
| **❷ 预处理** | Claude queryLoop | 主 LLM 推理 + tool_use | hooks_pre-task, memory_search | 查历史模式 | 路由建议 |
| **❸ Swarm 初始化** | Claude queryLoop | 主 LLM 决定拓扑 | swarm_init, agent_spawn x3, task_create x3 | — | Agent 注册 |
| **❹ 并行执行** | 子 Agent (Task) | 每个子 Agent 独立 LLM | memory_store/search, task_assign/complete | 共享命名空间 | — |
| **❺ 协调监控** | Coordinator goroutine | 不调 LLM | — | 心跳状态 | 健康度 |
| **❻ 整合学习** | Claude queryLoop | 主 LLM 综合 | hooks_post-task, memory_store | 存储模式 | SONA信号 |
| **❼ 反馈调整** | Claude queryLoop | 主 LLM 路由 | hooks_route, agent_spawn, memory_search | 搜索优化模式 | 新模式 |
