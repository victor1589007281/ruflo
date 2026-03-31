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
    participant OPT as ⚡ Provider<br/>Optimizations
    participant LLM as 🤖 LLM API
    participant POST as 🪝 postLLMCallHook
    participant RB as 🧠 ReasoningBank
    participant METRICS as 📊 Metrics

    CALLER->>PRE: preLLMCallHook(payload, context)

    PRE->>CACHE: generateCacheKey(provider, model, request)
    alt 缓存命中
        CACHE-->>PRE: cached response
        PRE-->>CALLER: {continue: false,<br/>cachedResponse: ...}
        Note over CALLER: 跳过 LLM 调用<br/>直接使用缓存
    else 缓存未命中
        CACHE-->>PRE: undefined
        PRE->>OPT: loadProviderOptimizations(provider)
        Note over OPT: anthropic: temperature=0.7<br/>openai: temperature=0.8<br/>"Be concise and direct"
        OPT-->>PRE: optimized request
        PRE->>METRICS: 记录 llm.calls.{provider}.{model}
        PRE-->>CALLER: {continue: true, payload: optimized}
    end

    CALLER->>LLM: actual API call

    LLM-->>CALLER: response

    CALLER->>POST: postLLMCallHook(payload, context)
    POST->>CACHE: setCache(key, response)
    POST->>METRICS: 记录延迟、token 使用、成本

    alt 响应长度 > 阈值
        POST->>RB: extractPatternFromResponse(response)
        RB->>RB: reasoningBank.storePattern()
        Note over RB: 从长回复中提取<br/>有价值的模式<br/>存入向量数据库
    end

    POST-->>CALLER: {continue: true, sideEffects}
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
    participant OPT as 🔄 OptimizerLoop

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
    LEDGER-->>OPT: 违规数据

    OPT->>OPT: 分析违规排名
    OPT->>OPT: 提升胜出的本地规则到根规则
    Note over OPT: CLAUDE.local.md → CLAUDE.md<br/>规则自进化
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
        CC->>T3: Task({model: "sonnet",...})<br/>$0.003-$0.015, 2-5s
    end
```
