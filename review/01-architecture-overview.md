# Ruflo V3 架构总览

> **Ruflo v3.5** — 原名 "Claude Flow"，5,900+ 提交，20 个核心包，259 MCP 工具，60+ Agent 类型

## 1. 项目定位

Ruflo V3 是一个 **AI Agent 编排平台**，核心理念是：

- **Claude Code 执行，Ruflo 协调** — Ruflo 不直接生成代码，而是作为 Claude Code / Codex 的 **编排层** 和 **学习层**
- **MCP-First 设计** (ADR-005) — 所有业务逻辑通过 MCP 工具暴露，CLI 仅是薄包装
- **自进化智能** — 通过 SONA/ReasoningBank/EWC++ 实现跨会话的模式学习和自适应

## 2. 整体架构图

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '16px', 'fontFamily': 'Arial', 'primaryColor': '#e8f4fd', 'primaryTextColor': '#1a1a2e', 'primaryBorderColor': '#4a90d9', 'lineColor': '#5b6abf', 'secondaryColor': '#f0f7e8', 'tertiaryColor': '#fff5e6'}}}%%
graph TB
    subgraph USER["<b>用户层</b>"]
        CC["<b>Claude Code / Cursor</b><br/>执行代码、文件操作"]
        CODEX["<b>OpenAI Codex</b><br/>双模式协作"]
        CLI_USER["<b>CLI 用户</b><br/>npx claude-flow"]
    end

    subgraph CLI_LAYER["<b>CLI 入口层</b> (@claude-flow/cli)"]
        PARSER["<b>CommandParser</b><br/>26 命令 · 140+ 子命令"]
        MCP_CLIENT["<b>MCP Client</b><br/>TOOL_REGISTRY<br/>callMCPTool()"]
    end

    subgraph MCP_LAYER["<b>MCP 工具层</b> (259 工具)"]
        AGENT_T["<b>agent-tools</b><br/>spawn/list/status"]
        SWARM_T["<b>swarm-tools</b><br/>init/status/stop"]
        MEMORY_T["<b>memory-tools</b><br/>store/search/retrieve"]
        HOOKS_T["<b>hooks-tools</b><br/>pre/post-task/route"]
        NEURAL_T["<b>neural-tools</b><br/>train/predict/patterns"]
        GUIDANCE_T["<b>guidance-tools</b><br/>compile/enforce/evolve"]
        OTHER_T["<b>其他 25+ 工具模块</b>"]
    end

    subgraph CORE_PACKAGES["<b>核心包层</b>"]
        MEMORY["<b>@claude-flow/memory</b><br/>UnifiedMemoryService<br/>AgentDB + HNSW"]
        HOOKS["<b>@claude-flow/hooks</b><br/>HookRegistry · HookExecutor<br/>ReasoningBank · Workers"]
        SWARM["<b>@claude-flow/swarm</b><br/>UnifiedSwarmCoordinator<br/>15-Agent 分层网格"]
        SECURITY["<b>@claude-flow/security</b><br/>InputValidator · PathValidator<br/>SafeExecutor · CVE 修复"]
        GUIDANCE["<b>@claude-flow/guidance</b><br/>GuidanceControlPlane<br/>编译 · 分片 · 执行门"]
        PROVIDERS["<b>@claude-flow/providers</b><br/>ProviderManager<br/>6+ LLM 提供商"]
        NEURAL_PKG["<b>@claude-flow/neural</b><br/>SONA · MoE · EWC++"]
    end

    subgraph INFRA["<b>基础设施层</b>"]
        EMBEDDINGS["<b>@claude-flow/embeddings</b><br/>MiniLM-L6 · sql.js 缓存"]
        SHARED["<b>@claude-flow/shared</b><br/>共享类型 · 事件 · 工具"]
        PLUGINS["<b>@claude-flow/plugins</b><br/>15 可选插件<br/>IPFS 注册表"]
        CODEX_PKG["<b>@claude-flow/codex</b><br/>DualModeOrchestrator"]
    end

    subgraph EXTERNAL["<b>外部服务</b>"]
        ANTHROPIC["<b>Anthropic API</b><br/>Claude 3.5/Opus/Haiku"]
        OPENAI["<b>OpenAI API</b><br/>GPT-4o/o1/o3"]
        GOOGLE["<b>Google API</b><br/>Gemini 2.0"]
        OLLAMA["<b>Ollama</b><br/>本地 LLM"]
    end

    CC --> MCP_CLIENT
    CODEX --> CLI_USER
    CLI_USER --> PARSER
    PARSER --> MCP_CLIENT

    MCP_CLIENT --> AGENT_T
    MCP_CLIENT --> SWARM_T
    MCP_CLIENT --> MEMORY_T
    MCP_CLIENT --> HOOKS_T
    MCP_CLIENT --> NEURAL_T
    MCP_CLIENT --> GUIDANCE_T
    MCP_CLIENT --> OTHER_T

    AGENT_T --> SWARM
    SWARM_T --> SWARM
    MEMORY_T --> MEMORY
    HOOKS_T --> HOOKS
    NEURAL_T --> NEURAL_PKG
    GUIDANCE_T --> GUIDANCE

    HOOKS --> MEMORY
    GUIDANCE --> HOOKS
    SWARM --> MEMORY
    NEURAL_PKG --> MEMORY

    PROVIDERS --> ANTHROPIC
    PROVIDERS --> OPENAI
    PROVIDERS --> GOOGLE
    PROVIDERS --> OLLAMA

    MEMORY --> EMBEDDINGS
    HOOKS -.-> PROVIDERS
    CODEX_PKG --> CLI_USER

    style USER fill:#e8f4fd,stroke:#4a90d9,stroke-width:2px
    style CLI_LAYER fill:#f0f7e8,stroke:#5a9e3f,stroke-width:2px
    style MCP_LAYER fill:#fff5e6,stroke:#d4a017,stroke-width:2px
    style CORE_PACKAGES fill:#f5eef8,stroke:#8e44ad,stroke-width:2px
    style INFRA fill:#fef9e7,stroke:#f39c12,stroke-width:2px
    style EXTERNAL fill:#fdedec,stroke:#e74c3c,stroke-width:2px
```

## 3. 包依赖关系图

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '14px', 'fontFamily': 'Arial', 'primaryColor': '#e8f4fd', 'primaryTextColor': '#1a1a2e'}}}%%
graph LR
    CLI["<b>@claude-flow/cli</b>"]
    MEMORY["<b>@claude-flow/memory</b>"]
    HOOKS["<b>@claude-flow/hooks</b>"]
    SWARM["<b>@claude-flow/swarm</b>"]
    SECURITY["<b>@claude-flow/security</b>"]
    GUIDANCE["<b>@claude-flow/guidance</b>"]
    PROVIDERS["<b>@claude-flow/providers</b>"]
    NEURAL["<b>@claude-flow/neural</b>"]
    EMBEDDINGS["<b>@claude-flow/embeddings</b>"]
    SHARED["<b>@claude-flow/shared</b>"]
    PLUGINS["<b>@claude-flow/plugins</b>"]
    CODEX["<b>@claude-flow/codex</b>"]
    CLAIMS["<b>@claude-flow/claims</b>"]
    MCP["<b>@claude-flow/mcp</b>"]
    BROWSER["<b>@claude-flow/browser</b>"]

    CLI --> SHARED
    CLI --> MCP
    HOOKS --> SHARED
    HOOKS -.->|可选| MEMORY
    HOOKS -.->|可选| EMBEDDINGS
    GUIDANCE --> HOOKS
    SWARM --> SHARED
    MEMORY --> EMBEDDINGS
    MEMORY --> SHARED
    NEURAL --> MEMORY
    PROVIDERS --> SHARED
    PLUGINS --> SHARED
    CODEX --> CLI
    MCP --> SHARED
    MCP -.->|采样| PROVIDERS
    CLAIMS --> SHARED
    BROWSER --> SHARED

    style CLI fill:#d5f5e3,stroke:#27ae60,stroke-width:3px
    style SHARED fill:#fdebd0,stroke:#e67e22,stroke-width:2px
    style MEMORY fill:#d6eaf8,stroke:#2980b9,stroke-width:2px
    style HOOKS fill:#fadbd8,stroke:#e74c3c,stroke-width:2px
    style GUIDANCE fill:#e8daef,stroke:#8e44ad,stroke-width:2px
```

## 4. 20 个核心包一览

| 包名 | 路径 | 职责 | 关键类/接口 |
|------|------|------|-----------|
| **@claude-flow/cli** | `v3/@claude-flow/cli/` | CLI 入口、命令注册、MCP 工具分发 | `CLI`, `CommandParser`, `TOOL_REGISTRY` |
| **@claude-flow/memory** | `v3/@claude-flow/memory/` | 统一内存服务、向量搜索 | `UnifiedMemoryService`, `AgentDBAdapter`, `HNSWIndex` |
| **@claude-flow/hooks** | `v3/@claude-flow/hooks/` | 钩子注册/执行、ReasoningBank、Workers | `HookRegistry`, `HookExecutor`, `ReasoningBank`, `WorkerManager` |
| **@claude-flow/swarm** | `v3/@claude-flow/swarm/` | Swarm 编排、拓扑管理、共识算法 | `UnifiedSwarmCoordinator`, `TopologyManager`, `ConsensusEngine` |
| **@claude-flow/security** | `v3/@claude-flow/security/` | 输入验证、路径安全、CVE 修复 | `InputValidator`, `PathValidator`, `SafeExecutor` |
| **@claude-flow/guidance** | `v3/@claude-flow/guidance/` | 治理控制面、规则编译、执行门 | `GuidanceControlPlane`, `GuidanceCompiler`, `EnforcementGates` |
| **@claude-flow/providers** | `v3/@claude-flow/providers/` | LLM 提供商抽象、负载均衡 | `ProviderManager`, `AnthropicProvider`, `OpenAIProvider` |
| **@claude-flow/neural** | `v3/@claude-flow/neural/` | 神经网络训练、模式学习 | SONA, MoE, EWC++ 算法 |
| **@claude-flow/embeddings** | `v3/@claude-flow/embeddings/` | 向量嵌入、sql.js 缓存 | MiniLM-L6 384 维、文档分块 |
| **@claude-flow/shared** | `v3/@claude-flow/shared/` | 共享类型、事件系统、工具 | 核心接口定义、事件总线 |
| **@claude-flow/plugins** | `v3/@claude-flow/plugins/` | 插件系统、IPFS 注册表 | `PluginManager`, `PluginRegistry` |
| **@claude-flow/codex** | `v3/@claude-flow/codex/` | Claude + Codex 双模式编排 | `DualModeOrchestrator`, `CollaborationTemplates` |
| **@claude-flow/mcp** | `v3/@claude-flow/mcp/` | MCP 协议、传输层、会话管理 | `MCPServer`, `SamplingManager` |
| **@claude-flow/claims** | `v3/@claude-flow/claims/` | 基于声明的授权 | Claims API |
| **@claude-flow/browser** | `v3/@claude-flow/browser/` | 浏览器自动化 | Browser Agent |
| **@claude-flow/deployment** | `v3/@claude-flow/deployment/` | 部署管理 | Deploy/Rollback |
| **@claude-flow/integration** | `v3/@claude-flow/integration/` | 集成桥接（agentic-flow） | Token Optimizer |
| **@claude-flow/testing** | `v3/@claude-flow/testing/` | 测试工具、Mock、V2 兼容 | Fixtures, Helpers |
| **@claude-flow/aidefence** | `v3/@claude-flow/aidefence/` | AI 防御 | 安全实体/服务 |
| **@claude-flow/performance** | `v3/@claude-flow/performance/` | 性能基准、分析框架 | Benchmark Suite |
