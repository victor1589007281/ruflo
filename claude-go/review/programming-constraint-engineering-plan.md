# AI Agent 编程约束工程方案

> **目标**：解决 claude-go 研发团队（AI Agent 团队）在代码生成中出现的质量差、互相冲突、忽视性能/安全/并发等问题。
> **基础**：Commit `530d18c4` ("研发团队调试") 已引入契约系统、质量门、Skill 注入、ToolSkill 运行时、对抗评审、通用计划编译器。本方案在其之上构建五层约束防御体系。
> **日期**：2026-05-11

---

## 1. 执行摘要

Commit `530d18c4` 将 claude-go 的 orchestrator 从"Prompt 约束 + 运行时补救"升级为"契约优先、门控治理、DAG 编译"的多 Agent 编码流水线。然而，系统仍属于**事后检测型**——在补丁应用后才检查问题，且缺少安全、并发、性能三个关键维度，并行 Agent 的编辑冲突也只能保守串行化。

本方案提出**五层约束防御体系**（宪法层 → 计划层 → 生成层 → 验证层 → 评审层），并配套 9 个关键子系统：

1. **Go 代码宪法** — Prompt 层面的不可违背原则
2. **契约锁与版本化** — 接口级符号表的锁定与演进
3. **六维质量门 2.0** — 格式/编译/静态/测试/安全/并发/性能
4. **并行编辑合并引擎** — CRDT + 语法感知合并，释放 DAG 宽度
5. **TDD 多 Agent 循环** — 测试先写、代码后补，用测试锁死行为
6. **CriticGPT 式对抗评审** — 专门找 Bug、 race、漏洞的评审 Agent
7. **Go 技能库 (Voyager 风格)** — 可检索的可复用代码模式
8. **跨 Agent 一致性检查** — 全局语义一致性，而非单叶通过即可
9. **约束效果遥测** — 用数据驱动约束迭代优化

实施分为三阶段：Phase 1（立即，安全+并发+TDD+对抗评审）、Phase 2（短期，技能库+属性测试+Prompt 优化）、Phase 3（中期，并行编辑+性能门+模型微调）。

---

## 2. 问题定义与痛点

### 2.1 研发团队 = AI Agent 团队

在 claude-go 中，"研发团队"指由 orchestrator 调度的多 Agent 协作集群：Planner 拆任务 → Executor 写代码 → Validator 跑测试 → Adversarial Reviewer 做评审。这些 Agent 共享同一个代码库，但各自独立推理，导致以下系统性问题：

| 痛点 | 具体表现 | 当前commit是否解决 |
|------|----------|-------------------|
| **开发质量差** | Agent 生成代码后 `go test` 不通过；反复修反复错；引入无用抽象 | 部分解决：Validation Gate 强制 build/test 通过，但缺少安全/并发维度 |
| **互相冲突** | 两个并行 Agent 修改同一接口的不同实现文件，导致类型漂移；Planner 拆的任务粒度不均，后序任务依赖前序的不稳定中间状态 | 部分解决：Conflict-key 串行化 + PatchApplier 冲突检测，但只能拒绝无法合并 |
| **不考虑性能** | Agent 用 O(n²) 循环替代 O(n log n)；无 Benchmark 回归检查 | 未解决 |
| **不考虑安全** | 硬编码凭证、使用 `crypto/md5`、SQL 注入风险、路径遍历 | 未解决 |
| **不考虑并发** | `map` 并发读写 race、goroutine leak、channel 死锁、`sync.Mutex` 遗漏 | 未解决 |
| **接口漂移** | Agent A 改了接口签名，Agent B 的实现未同步更新，编译通过但运行期 panic | 部分解决：Contract Store 提供符号表，但无版本锁定与迁移机制 |

### 2.2 根因分析

1. **约束维度不完整**：现有 Validation Gate 只有 Format → Compile → Static → Test → Quality，缺少 Security、Concurrency、Performance 三大硬门。
2. **事后检测为主**：Patch 应用后才跑 gate，修复成本高。没有**预飞行检查**（pre-flight semantic diff、race prediction）。
3. **Agent 间无全局一致性**：每片叶子（task）单独通过 adversarial review 就算成功，但全局看接口可能已断裂。
4. **并行策略保守**：`conflictKey` 只要共享就串行，DAG 宽度被严重压缩，且无法安全地合并并行编辑。
5. **缺乏领域知识注入**：Agent 不会自动检索"如何正确写 Go HTTP server"或"如何处理 context cancellation"，每次都从零推理。
6. **无效果度量**：Skill（如 `simplicity-first`）注入后，没有 A/B 测试或指标追踪其实际效果。

---

## 3. 业界大模型编程约束机制调研

### 3.1 主流系统约束机制对比

| 系统 | 规则文件 | 钩子/Hook | 审批门 | Skill/Playbook | 子 Agent 隔离 | 沙箱 | 安全扫描 | 冲突解决 |
|------|---------|----------|--------|---------------|--------------|------|---------|---------|
| **Claude Code / Agent SDK** | ✅ CLAUDE.md, settings.json | ✅ PreToolUse, PostToolUse, Stop | ✅ Plan mode, acceptEdits, bypassPermissions | ✅ SKILL.md, agent definitions | ✅ Task tool 子 Agent | ⚠️ 进程级 | ❌ 无内置 | ❌ 无 |
| **OpenAI Codex CLI** | ✅ AGENTS.md | ⚠️ 部分 | ✅ 显式批准模式 | ⚠️ 指令文件 | ✅ 多 Agent | ✅ 容器沙箱 | ❌ 无内置 | ❌ 无 |
| **GLM 5.1 / CodeGeeX** | ⚠️ 系统提示+规则模板 | ⚠️ 工具调用前检查 | ⚠️ 人工确认（部分场景） | ⚠️ 角色模板 | ✅ 多 Agent 模式 | ⚠️ 基础隔离 | ❌ 无内置 | ❌ 无 |
| **Kimi K2 / Kimi-Coder** | ⚠️ 系统提示约束 | ⚠️ 执行前确认 | ⚠️ 敏感操作确认 | ❌ 无公开 Skill 机制 | ✅ 多 Agent 协作 | ⚠️ 基础隔离 | ❌ 无内置 | ❌ 无 |
| **Cursor** | ✅ .cursorrules, cursor rules | ❌ 无 | ⚠️ Yolo/Normal 模式 | ⚠️ 规则文件即 skill | ⚠️ Composer 多文件 | ❌ 无 | ❌ 无内置 | ⚠️ 文件级合并 |
| **Windsurf** | ⚠️ Memories, flows | ❌ 无 | ⚠️ 自动/手动 | ⚠️ 工作流模板 | ✅ Cascade 多 Agent | ❌ 无 | ❌ 无内置 | ⚠️ 文件级合并 |
| **Cline / Roo Code** | ✅ .clinerules / custom instructions | ❌ 无 | ✅ Auto-approve 可配置 | ⚠️ 系统提示 | ⚠️ 子任务 | ❌ 无 | ❌ 无内置 | ❌ 无 |
| **Aider** | ✅ CONVENTIONS.md, .aider.conf | ❌ 无 | ⚠️ 自动提交可配置 | ⚠️ 代码地图 | ⚠️ 多文件编辑 | ❌ 无 | ❌ 无内置 | ⚠️ Git 合并 |
| **Gemini / Jules** | ⚠️ 自定义指令 | ⚠️ 部分 | ⚠️ 敏感操作确认 | ❌ 无公开 | ✅ 多 Agent | ⚠️ 基础隔离 | ❌ 无内置 | ❌ 无 |
| **DeepSeek / Qwen / Doubao** | ⚠️ 系统提示 | ⚠️ 部分 | ❌ 弱 | ❌ 无公开 | ⚠️ 部分 | ❌ 无 | ❌ 无内置 | ❌ 无 |
| **GitHub Copilot Workspace** | ⚠️ 自定义指令 | ❌ 无 | ⚠️ 提交前确认 | ⚠️ 工作流 | ⚠️ 部分 | ❌ 无 | ❌ 无内置 | ⚠️ PR 合并 |

**关键发现**：
- **规则文件**是业界最普遍的约束形式（CLAUDE.md、AGENTS.md、.cursorrules），但都是**文本级软约束**，Agent 可以忽略。
- **审批门**（Plan mode、acceptEdits）是 Claude Code 和 Codex CLI 的核心差异化能力，能在执行前拦截危险操作。
- **Skill/Playbook** 只有 Claude Code（SKILL.md）和 OpenHands（microagents）有成熟的文件级技能系统。
- **安全扫描、并发检查、冲突解决**是所有系统的共同盲区——没有任何一个系统内建 `go test -race` 或 `gosec` 级别的硬约束。
- **子 Agent 隔离**普遍不足：即使有多 Agent，也缺乏写锁、分支隔离或 CRDT 合并机制。

### 3.2 对 claude-go 的启示

1. **Claude Code 的 Plan mode + acceptEdits** 模式应被借鉴：在 Executor 真正写文件前，增加一个"预提交审查"阶段，展示 diff 并要求显式确认（或由自动化规则确认）。
2. **OpenHands 的 microagents** 概念可直接映射到 claude-go 的 Skill 系统：让 Go 相关的最佳实践（如 `context.Context` 传播、error wrap 模式）成为可自动加载的微 Agent。
3. **Codex CLI 的容器沙箱**提示我们：ToolSkill Runtime 的 `ExecAndParse` 目前直接在宿主机运行，应增加轻量级沙箱（如 runc 或现有 sandbox 模块）来隔离 `go:generate` 等风险。
4. **Aider 的 repomap** 与 claude-go 的 Contract Store 目标一致，但 Aider 使用 tree-sitter 而非 go/ast，可支持多语言且更准确。Contract Store 应考虑引入 tree-sitter-go 作为第二解析器。

---

## 4. 学术界关键方案总结

### 4.1 十大研究领域与代表性工作

| 领域 | 代表性工作 | 对当前系统的核心启示 |
|------|-----------|---------------------|
| **Constitutional AI / 代码宪法** | Bai et al. (2022) Constitutional AI; Kyrychenko et al. (2025) C3AI; Kumar et al. (2026) Evolving Constitutions | 将编码规范写成"宪法"注入所有 Agent Prompt；可进化宪法比人工编写效果更好 |
| **自反思与对抗评审** | Madaan et al. (2023) Self-Refine; Shinn et al. (2023) Reflexion; McAleese et al. (2024) CriticGPT; Du et al. (2023) Multi-Agent Debate | Executor 内部可跑 Self-Refine；Adversarial Reviewer 应升级为 CriticGPT 专用模型；Planner 前可加 Debate 阶段 |
| **多 Agent 架构** | Hong et al. (2023) MetaGPT; Qian et al. (2023) ChatDev; Yang et al. (2024) SWE-Agent; Wang et al. (2024) OpenHands | 用结构化输出（JSON schema）替代自由文本通信；限制 Executor 的工具集（ACI）而非给裸 bash |
| **TDD / 规格驱动** | Huang et al. (2023) AgentCoder; Piya & Sullivan (2023) LLM4TDD; He et al. (2025) PGS | Planner 必须先生成测试，Executor 再写实现；测试是客观质量门，Agent 无法绕过 |
| **静态分析闭环** | Li et al. (2024) IRIS; Blyth et al. (2025) SelectIssues; Dolcetti et al. (2024) Testing+Static Analysis | 不要信任 Agent 自评，必须用外部工具（go vet, gosec, golangci-lint）给出客观反馈 |
| **安全生成** | He & Vechev (2023) SVEN; He et al. (2024) SafeCoder; Tony et al. (2024) Secure Prompting; Cheng et al. (2026) DRV | CWE 命名式 Prompt 比"写安全代码"有效 2-3 倍；Detect-Repair-Verify 是最佳实践 |
| **并发正确性** | Jin et al. (2023/2025) DR.FIX (Uber); Huang et al. (2026) CONCUR; Chen et al. (2023) Race Detection | 并发代码的 LLM 生成正确率仅 13.5%（CONCUR）；必须跑 `go test -race`；可用 RAG 修复历史 race |
| **并行编辑冲突解决** | Gentle & Kleppmann (2024) Eg-walker; Pugachev (2025) CodeCRDT; Delpeuch (2024) Mergiraf; Dong et al. (2023) MergeGen | CRDT 可消除文本级冲突；语法感知合并（tree-sitter）可消除结构级假冲突；LLM 辅助 3-way merge 用于语义冲突 |
| **质量门 / CI 闭环** | SWE-bench Verified (2024); PatchPilot (2025); TrustBench (2025) | 用 SWE-bench 风格回归集防止系统退化；Runtime Verification 在动作执行前拦截违规操作 |
| **技能库与 Prompt 优化** | Wang et al. (2023) Voyager; Khattab et al. (2023) DSPy; Yuksekgonul et al. (2024) TextGrad | 建立可复用 Go 模式库；用 DSPy/TextGrad 自动优化 Prompt 模板 |

### 4.2 技术排名：最优先引入的 15 项技术

| 排名 | 技术 | 针对痛点 | 实施难度 |
|------|------|---------|---------|
| 1 | **TDD 多 Agent 循环** | 低质量 | 中 |
| 2 | **静态分析反馈闭环** (go vet + gosec + golangci-lint) | 低质量 + 安全 | 低 |
| 3 | **CriticGPT 式对抗评审** | 低质量 + 安全 | 中 |
| 4 | **代码宪法** | 低质量 + 冲突 | 低 |
| 5 | **CRDT 并行编辑** | 冲突 | 高 |
| 6 | **语法感知语义合并** | 冲突 | 中 |
| 7 | **Self-Refine + 编译器反馈** | 低质量 | 低 |
| 8 | **CWE 感知 Prompt** | 安全 | 低 |
| 9 | **Race Detector + GenAI 修复** | 并发 | 中 |
| 10 | **性能基准门** (Benchmark gate) | 性能 | 中 |
| 11 | **Voyager 风格技能库** | 低质量 | 中 |
| 12 | **DSPy / TextGrad Prompt 优化** | 低质量 | 高 |
| 13 | **多 Agent Debate** | 冲突 | 中 |
| 14 | **Fuzzing 反馈** (OSS-Fuzz-Gen 风格) | 安全 + 质量 | 中 |
| 15 | **SWE-bench Verified 回归集** | 低质量 | 中 |

---

## 5. 现有系统能力评估（基于 530d18c）

### 5.1 已建立的坚实基础

Commit `530d18c4` 引入了以下 7 大子系统，构成本方案的基座：

| 子系统 | 文件 | 作用 | 评估 |
|--------|------|------|------|
| **契约系统** | `contract/types.go`, `agent/contract_store.go` (846L), `toolskill/contract_builder.go` | 项目级符号表，防止接口漂移 | ⭐⭐⭐ 好，但缺泛型、缺版本、缺分布式合并 |
| **质量门** | `agent/validation_gate.go` (368L), `toolskill/quality_gate.go`, `toolskill/static_check.go`, `toolskill/diagnose.go` | Format→Compile→Static→Test→Quality 顺序执行 | ⭐⭐⭐ 好，但缺安全/并发/性能维度 |
| **Skill 注入** | `skills/builtin/*/{SKILL.md}` (5 个 skill) | Prompt 级文化约束 | ⭐⭐ 可用，但无法程序化强制，缺效果度量 |
| **ToolSkill 运行时** | `toolskill/{schema,adapter,adapters,detect,parser,language,format,import_fix,runtime}.go` | 将 go vet/build/fmt 包装为结构化 LLM 可调技能 | ⭐⭐⭐ 好，但缺沙箱、缺缓存、缺增量测试 |
| **执行器** | `agent/executor.go` (538L) | 契约优先 Prompt + Think 块 + 修复循环 | ⭐⭐⭐ 好，但缺 Self-Refine 内循环 |
| **Patch 应用器** | `agent/patch_apply.go` (403L) | AST 感知补丁 + 冲突检测 + 语法校验 | ⭐⭐⭐ 好，但缺语义合并、缺三向合并 |
| **对抗评审** | `agent/adversarial.go` | 显式 Pass 判定 + 反馈文本分析 | ⭐⭐ 可用，但缺并发/安全专项评审能力 |
| **通用计划编译器** | `agent/planning_universal.go`, `orchestrator.go` (大幅重构) | DAG 编译、预算门、契约依赖注入、设计完备阶段屏障 | ⭐⭐⭐ 好，但冲突键过于保守、无运行时 DAG 修补 |

### 5.2 十大关键缺口

| 缺口 | 风险 | 优先级 |
|------|------|--------|
| **无并发安全门** | Agent 引入 data race、goroutine leak、channel 死锁 | P0 |
| **无安全扫描** | 硬编码凭证、弱加密、注入攻击 | P0 |
| **无性能回归门** | O(n²) 替代 O(n log n)、内存泄漏 | P1 |
| **无语义/三向合并** | 并行 Agent 无法安全共编辑同一文件，DAG 宽度受限 | P1 |
| **无跨 Agent 全局一致性检查** | 单叶通过但全局接口断裂 | P1 |
| **契约无泛型支持** | `go/ast` 无法解析 type parameter，导致假"missing method" | P2 |
| **无 Go 代码宪法** | 没有统一的、不可违背的编码原则文档 | P2 |
| **无技能效果遥测** | 不知道 `simplicity-first` 是否真的减少了行数 | P2 |
| **ToolSkill 无沙箱** | `go:generate` 可被恶意利用 | P2 |
| **无确定性骨架生成** | Universal V1 fallback 仍是 LLM 生成，首叶变异导致下游漂移 | P2 |

---

## 6. 总体方案：五层约束防御体系

```
┌─────────────────────────────────────────────────────────────┐
│  L5 评审层 (Review Layer)                                    │
│  CriticGPT 对抗评审 → 跨 Agent 一致性检查 → 人工最终审批        │
├─────────────────────────────────────────────────────────────┤
│  L4 验证层 (Validation Layer)                                │
│  Format → Compile → Static → Test → Security → Concurrency   │
│  → Performance → Quality                                     │
├─────────────────────────────────────────────────────────────┤
│  L3 生成层 (Generation Layer)                                │
│  Go 代码宪法 + Skill 库 + 契约上下文 + Think-Phase +          │
│  Self-Refine 内循环                                          │
├─────────────────────────────────────────────────────────────┤
│  L2 计划层 (Planning Layer)                                  │
│  DAG 编译器：契约依赖注入 → 设计完备屏障 → 预算门 →           │
│  冲突键优化 → 并行编辑区划分                                   │
├─────────────────────────────────────────────────────────────┤
│  L1 宪法层 (Constitutional Layer)                            │
│  Go 代码宪法 + CWE 感知 Prompt + 角色宪法 + 可进化宪法          │
└─────────────────────────────────────────────────────────────┘
```

**设计原则**：
1. **预防优于检测**：越靠近 L1/L2，约束成本越低。能在计划阶段阻止的，不在验证阶段发现。
2. **客观工具优于主观自评**：L4 必须依赖外部工具（go vet, gosec, go test -race, benchcmp），不能依赖 Agent 自我判断。
3. **并行优于串行**：L2 应通过智能冲突键划分和 CRDT 合并最大化并行度，而非粗暴串行。
4. **可度量优于信仰**：每一层约束都必须有遥测指标，用数据证明其有效性。

---

## 7. 关键子系统设计

### 7.1 Go 代码宪法 (Go Code Constitution)

**定位**：L1 宪法层。一份不可违背的编码原则文档，以 Constitutional AI 方式注入所有 Agent 的系统提示。

**内容草案**（应存入 `pkg/skills/builtin/go-constitution/SKILL.md`）：

```markdown
# Go 代码宪法 v1.0

## 不可违背原则 (Hard Rules)
1. **错误处理**：所有返回 `error` 的函数调用必须立即检查；禁止 `_, _ = fn()` 静默丢弃错误。
2. **并发安全**：任何跨 goroutine 共享的可变状态必须使用 `sync.Mutex`、`sync.RWMutex`、`sync/atomic` 或 channel；禁止裸读写 `map`/`slice`。
3. **Context 传播**：所有 I/O 操作、HTTP handler、goroutine 入口必须接受 `context.Context` 并向下传播；禁止 `context.Background()` 在业务代码中硬编码。
4. **资源释放**：所有 `os.Open`、`net.Dial`、`http.Request.Body` 必须有 `defer Close()` 或等效保证。
5. **安全默认**：禁止 `crypto/md5`/`sha1` 用于安全场景；禁止 `eval`/`exec` 拼接用户输入；禁止日志打印凭证。
6. **性能底线**：禁止在热路径分配闭包捕获变量；禁止在循环内重复 `reflect.TypeOf`；优先使用 `sync.Pool` 管理高频临时对象。

## 协调原则 (Coordination Rules)
7. **契约优先**：修改任何接口前，必须先更新 `contract_store.go` 中的契约定义，并通知所有实现者。
8. **最小修改**：每次变更不得超过 200 行；超过必须拆分为子任务。
9. **测试锁死**：任何功能变更必须伴随测试变更；修改测试通过逻辑来让测试通过是严重违规。
```

**注入机制**：
- 修改 `agent/executor.go` 的 `buildGenerationPrompt()`（约 line 280），将宪法内容作为 system prompt 的前缀（而非后缀，确保不被上下文截断淹没）。
- 修改 `agent/roles.go` 的 Planner 角色提示，要求 Planner 在拆任务时检查任务是否违反宪法（如"并发访问共享状态"必须标记为需要 `sync.Mutex`）。

**效果度量**：
- 在 `validation_gate.go` 增加 `ConstitutionComplianceCheck`：扫描生成的代码中是否出现 `crypto/md5`、裸 map 读写、`context.Background()` 硬编码等模式（regex/AST 扫描）。

### 7.2 契约锁与版本化 (Contract Locking & Versioning)

**定位**：L2 计划层 + L3 生成层。解决"接口漂移"和"跨 Agent 契约冲突"。

**当前问题**：
- `contract_store.go` 的 `implements()`（line 714）只用参数/返回值数量匹配，不检查类型兼容性。
- 无泛型支持。
- 两个并行 Agent 扩展同一接口时，后写入者覆盖前者。

**改进方案**：

1. **引入 tree-sitter-go 作为第二解析器**
   - 在 `contract_store.go` 旁新增 `contract/tree_parser.go`，用 `smacker/go-tree-sitter` 解析 Go 源码。
   - tree-sitter 比 go/ast 更准确，支持泛型语法节点，且能处理不完整的代码片段。
   - 解析结果与 go/ast 做交叉验证，不一致时告警。

2. **契约版本化**
   - 在 `contract/types.go` 新增：
     ```go
     type ContractVersion struct {
         Version   int       // 单调递增
         Author    string    // Agent ID
         Timestamp time.Time
         Diff      string    // 与上一版的 diff
         Breaking  bool      // 是否破坏兼容性
     }
     type InterfaceContract struct {
         Name      string
         Versions  []ContractVersion
         Current   MethodSig   // 最新版
         Locked    bool        // 锁定后不可随意修改
     }
     ```
   - `ContractStore` 增加 `LockInterface(name string, agentID string) error`：当一个 Agent 正在修改某接口时，其他 Agent 必须等待或申请显式解锁。
   - Orchestrator 的 `addContractFirstRawDeps()`（line 1351）在注入依赖时，若发现契约被锁定，自动将该任务串行化到锁持有者之后。

3. **泛型支持**
   - tree-sitter 可解析 `type Store[T any] interface { ... }`。
   - `MethodSig` 增加 `TypeParams []TypeParam` 字段。
   - `implements()` 在存在泛型时，回退到 `golang.org/x/tools/go/packages` 做类型级精确检查（慢但准）。

4. **契约迁移助手**
   - 当接口发生 Breaking Change 时，自动生成"迁移任务"：为所有实现者创建子任务，要求它们同步更新。
   - 在 `orchestrator.go` 的 DAG 编译阶段，若检测到 `Breaking=true`，自动注入 `migration` 类型叶子节点。

**对接文件**：
- `pkg/contract/types.go`：新增版本化类型
- `pkg/agent/contract_store.go`：新增 Lock/LockedBy 逻辑、tree-sitter 解析器桥接
- `pkg/agent/orchestrator.go`：在 `addContractFirstRawDeps` 和 `rawTasksToDAG` 中处理 Locked 契约的串行化

### 7.3 六维质量门 2.0 (Six-Dimensional Validation Gate)

**定位**：L4 验证层。将现有 5 阶段 gate 扩展为 8 阶段。

**现有阶段**（530d18c）：Format → Compile → Static → Test → Quality

**新阶段**：Format → Compile → Static → Security → Concurrency → Test → Performance → Quality

```go
// pkg/agent/validation_gate.go
func DefaultGateConfigV2() *GateConfig {
    return &GateConfig{
        stages: []ValidationStage{
            {Name: "format",   Hard: false, AutoFix: true},
            {Name: "compile",  Hard: true,  AutoFix: false},
            {Name: "static",   Hard: true,  AutoFix: false},
            {Name: "security", Hard: true,  AutoFix: false}, // 新增
            {Name: "concurrency", Hard: true, AutoFix: false}, // 新增
            {Name: "test",     Hard: true,  AutoFix: false},
            {Name: "performance", Hard: false, AutoFix: false}, // 新增 (soft gate，不阻塞但记录)
            {Name: "quality",  Hard: true,  AutoFix: false},
        },
        maxRounds: 5,
    }
}
```

**各阶段实现**：

| 阶段 | 工具/方法 | 失败处理 |
|------|----------|---------|
| **Format** | `gofmt`, `goimports` | 自动修复并重试 |
| **Compile** | `go build ./...` | ContextEngine 查契约 → 修复 Prompt → 重试 |
| **Static** | `staticcheck` > `golangci-lint` > `go vet` | 同上 |
| **Security** | `gosec` + 自定义 secret scan + CWE regex | 安全失败不可自动修复；必须 Agent 显式确认或人类介入 |
| **Concurrency** | `go test -race ./...` + `go vet -lostcancel` + `staticcheck` 并发检查 | Race 失败提供 RAG 修复建议（见 7.9） |
| **Test** | `go test ./...` | 同上 |
| **Performance** | `go test -bench=. -benchmem`（对比基线） | 不阻塞，但记录 regression；P95 延迟 > 基线 20% 则标记为 `DeliveredWithRemediation` |
| **Quality** | 综合评分（行数、复杂度、测试覆盖率） | 同上 |

**关键技术点**：

1. **Security Gate**
   - 新增 `pkg/toolskill/security_scan.go`：
     - 集成 `gosec`（`github.com/securego/gosec/v2/cmd/gosec`）
     - 若 gosec 不可用，回退到 regex 扫描：硬编码密码模式、`crypto/md5` 导入、`os/exec` 拼接用户输入、`sql.Open` 字符串拼接等。
   - 新增 `pkg/toolskill/secret_scan.go`：扫描 AWS key、GitHub token、private key 等模式。
   - Security 是 **Hard Gate**：一旦触发，当前 round 立即停止，Executor 必须生成一个专门的 "security remediation" patch，而非在普通修复循环中顺带修改。

2. **Concurrency Gate**
   - 在 `go test -race` 前，先跑 `go vet` 的已知并发检查（如 `-lostcancel`, `-shadow`）。
   - 若 race detector 报告 data race，提取 race 的栈信息和涉及的变量名，传给 ContextEngine。
   - ContextEngine 新增 `concurrency_diagnosis.go`：查询 Contract Store 中涉及变量的类型，判断应使用 `sync.Mutex`、`sync.Map`、`atomic.Value` 还是 channel。

3. **Performance Gate**
   - 在 `pkg/toolskill/benchmark_gate.go` 中实现：
     - 维护一个 `benchmark_baseline.json`，记录关键包的基准数据。
     - 每次变更后跑 `go test -bench=. -benchmem -count=5`。
     - 用 `golang.org/x/perf/benchstat` 做统计比较。
     - 若 `ns/op` 或 `allocs/op` 回归超过阈值（默认 20%），标记 `performance_regression=true`。
   - Performance Gate 是 **Soft Gate**：不阻塞交付，但强制将交付状态降级为 `DeliveredWithRemediation`，并在 review 报告中高亮。

**对接文件**：
- `pkg/agent/validation_gate.go`：扩展 stages，增加 security/concurrency/performance 处理逻辑
- `pkg/toolskill/security_scan.go`（新建）
- `pkg/toolskill/benchmark_gate.go`（新建）
- `pkg/agent/context_engine.go`：新增并发诊断策略

### 7.4 并行编辑合并引擎 (Parallel Edit Merge Engine)

**定位**：L2 计划层。解决"冲突键过于保守导致 DAG 宽度不足"的问题。

**当前问题**：
- `rawTasksToDAG` 中，只要两个任务共享 `conflictKey` 就建立串行边。
- PatchApplier 检测到同一文件的冲突只能拒绝。

**改进方案——三级合并策略**：

```
Level 1: 文件级隔离（无冲突）
    └── 两个 Agent 编辑不同文件 → 完全并行，直接应用

Level 2: CRDT 文本合并（结构不冲突）
    └── 两个 Agent 编辑同一文件的不同区域 → 用 Yjs / Eg-walker 合并文本
    └── 合并后 AST 语法校验 → 通过则接受

Level 3: 语法感知语义合并（结构冲突）
    └── Agent A 移动了函数，Agent B 修改了该函数 → Mergiraf (tree-sitter + GumTree)
    └── 若 Mergiraf 可自动解决 → 接受
    └── 若仍冲突 → 触发 LLM 辅助 3-way merge（MergeGen 风格）

Level 4: 人工/协调员仲裁（语义冲突）
    └── LLM 合并后编译/测试失败 → 降级为串行执行（后执行者基于前执行者的结果重新生成）
```

**技术实现**：

1. **CRDT 集成（Level 2）**
   - 引入 `yjs` 的 Go port 或基于 Eg-walker 的简化实现。
   - 在每个 Agent 的工作目录中，文件编辑不以 `EditSnippet` 直接写磁盘，而是先写入一个内存中的 CRDT 文档。
   - `PatchApplier` 新增 `ApplyCRDT()` 方法：接收多个 Agent 的编辑操作，返回合并后的文本。
   - 合并后必须跑 `validateSyntax()`（已有）和新增的 `validateImports()`（确保合并后 import 块无重复/遗漏）。

2. **语法感知合并（Level 3）**
   - 引入 `mergiraf` 作为外部工具（或将其 tree-sitter + GumTree 算法用 Go 重写）。
   - 当 CRDT 合并产生冲突标记（`<<<<<<`）时，调用 Mergiraf：
     ```bash
     mergiraf merge --language=go base.go left.go right.go -o merged.go
     ```
   - Mergiraf 利用 tree-sitter 解析 Go AST，用 GumTree 做模糊匹配，可消除"移动+编辑"的假冲突。

3. **冲突键智能优化（减少不必要的串行化）**
   - 当前 `rawTasksToDAG` 的 conflict-key 逻辑在 `orchestrator.go` 中。
   - 新增 `ConflictAnalyzer`：在 DAG 编译阶段，对共享 conflictKey 的任务做**读写分析**。
     - 若两个任务都是 Read-Only（如读取同一接口做查询），不建串行边。
     - 若一个 Read、一个 Write，且 Write 的是不同符号（不同函数），尝试建立细粒度符号级依赖而非文件级。
   - 在 `pkg/agent/orchestrator.go` 的 `rawTasksToDAG` 中替换粗粒度 conflict-key 逻辑。

**对接文件**：
- `pkg/agent/patch_apply.go`：新增 `ApplyCRDT()`, `ApplyMergiraf()`
- `pkg/agent/orchestrator.go`：`rawTasksToDAG` 冲突键优化
- 新增 `pkg/merge/crdt.go`, `pkg/merge/mergiraf.go`

### 7.5 TDD 多 Agent 循环 (Test-Driven Multi-Agent Loop)

**定位**：L2 计划层 + L4 验证层。让测试成为不可绕过的客观约束。

**设计**：

```
Planner (Test-Designer Agent)
    ├── 接收任务目标
    ├── 查询 Contract Store 获取接口签名
    └── 输出：测试文件 + 测试期望行为（JSON/Testify）
            └── 测试必须先编译通过（即使实现是 stub）

Executor (Programmer Agent)
    ├── 读取 Test-Designer 输出的测试
    ├── 读取 Contract Store 上下文
    ├── 生成实现代码（目标：让测试通过）
    └── 输出：实现代码 + 最小化变更

Validator (Test Executor)
    ├── 跑 `go test -race -bench=.`
    ├── 跑 Security / Concurrency gates
    └── 输出：通过 / 失败 + 诊断报告

若失败 → 反馈给 Executor 做 Self-Refine 修复
若通过 → 进入 Adversarial Reviewer
```

**关键机制**：

1. **测试先行强制**
   - 在 `orchestrator.go` 的 `normalizeAndSplitRawTasks()` 中，对每个实现叶子节点，检查其前置依赖中是否有 `test_` 前缀的叶子。
   - 若没有，自动插入一个 `test_design` 叶子作为前置依赖。
   - `test_design` 叶子的 `WorkUnitType` 为 `test`，由专门的 Test-Designer 角色 Agent 执行。

2. **Stub 编译保证**
   - Test-Designer 生成的测试必须能够编译通过，即使被测函数是 `panic("not implemented")` stub。
   - 在 `executor.go` 中，Executor 的第一步不是直接写实现，而是检查测试是否已编译通过；若不通过，先修复测试的编译错误（可能是接口签名不匹配）。

3. **属性测试（Property-Based Testing）**
   - Test-Designer 在生成 example-based 测试之外，对纯函数（如 parser、transformer）额外生成 property-based 测试：
     - 使用 `testing/quick` 或 `pgregory/rapid`。
     - 属性示例：`f(f^-1(x)) == x`（round-trip）、`sort(data)` 的结果必须有序且长度不变。
   - Property test 失败通常意味着更深层的不变量被破坏，比 example test 更有价值。

**对接文件**：
- `pkg/agent/roles.go`：新增 `test-designer` 角色
- `pkg/agent/orchestrator.go`：`normalizeAndSplitRawTasks` 自动注入 test 依赖
- `pkg/agent/executor.go`：执行前检查测试编译状态
- 新增 `pkg/toolskill/property_test.go`：属性测试生成与执行

### 7.6 CriticGPT 式对抗评审 (CriticGPT-Style Adversarial Review)

**定位**：L5 评审层。将现有 Adversarial Reviewer 升级为专项漏洞猎人。

**当前问题**：
- Adversarial Reviewer 只在单叶级别运行，不做全局评审。
- 评审维度是通用评分（分数+反馈），没有专项安全/并发/性能检查清单。
- `feedbackContainsBlockingFailure()` 用关键词匹配，精度有限。

**改进方案**：

1. **多维度评审清单**
   将评审拆分为 5 个维度，每个维度独立评分：
   - **Correctness**：功能正确性、边界条件、错误处理
   - **Security**：OWASP Top 10、Go 常见漏洞（CWE-362 race, CWE-89 注入, CWE-798 硬编码凭证）
   - **Concurrency**：data race 风险、goroutine leak、channel 死锁、context 传播
   - **Performance**：算法复杂度、内存分配、热点路径优化
   - **Idiomatic**：Go 惯用法、命名规范、代码简洁度

   每个维度输出 `score` (0-10) + `pass` (bool) + `findings` ([]Finding)。
   最终通过条件：所有 Hard 维度（Correctness, Security, Concurrency）必须 `pass=true`，且总分 >= 阈值。

2. **安全/并发专项 Prompt**
   - 为 Security 和 Concurrency 维度设计专项系统提示：
     - Security：要求 Agent 模拟攻击者视角，检查输入验证、密码学误用、路径遍历、日志泄漏。
     - Concurrency：要求 Agent 分析所有 `go func()` 调用，检查共享变量访问、channel 关闭责任、`select` 的 `default` 分支。
   - 提示中注入相关 CWE 描述（如 CWE-362: Concurrent Execution using Shared Resource with Improper Synchronization）。

3. **全局一致性评审**
   - 新增 `GlobalConsistencyReviewer`：在所有叶子执行完成后，对整个变更集做一次全局评审。
   - 检查项：
     - 接口签名一致性（所有实现者与契约定义匹配）
     - 跨文件引用一致性（没有"undefined"的跨包引用）
     - 测试覆盖一致性（新增代码是否有对应测试）
     - 无重复实现（两个 Agent 是否实现了相同功能的两个版本）
   - 评审输入不是单文件，而是整个 PR diff（用 `git diff` 生成）。

4. **评审 Agent 专业化**
   - 不再使用同一个 Adversarial Agent 评审所有维度。
   - 引入 3 个评审子 Agent：
     - `security-reviewer`：专门找安全漏洞
     - `concurrency-reviewer`：专门找 race 和 leak
     - `idiomatic-reviewer`：专门检查 Go 惯用法和代码质量
   - 每个子 Agent 可并行运行（因为它们读但不写）。

**对接文件**：
- `pkg/agent/adversarial.go`：扩展 `EvalScore` 为多维度；新增 `GlobalConsistencyReviewer`
- `pkg/agent/roles.go`：新增 `security-reviewer`, `concurrency-reviewer`, `idiomatic-reviewer` 角色定义
- `pkg/skills/builtin/security-review/SKILL.md`（新建）
- `pkg/skills/builtin/concurrency-review/SKILL.md`（新建）

### 7.7 Go 技能库 (Go Skill Library)

**定位**：L3 生成层。Voyager 风格的可复用代码模式库，解决"每次都从零推理"的问题。

**设计**：

```
skills/
  runtime/
    go/
      http-server-graceful-shutdown.md      # 技能 1
      worker-pool-with-context.md           # 技能 2
      rate-limiter-token-bucket.md          # 技能 3
      error-wrap-and-chain.md               # 技能 4
      json-api-response-pattern.md          # 技能 5
      database-transaction-pattern.md       # 技能 6
      mock-generation-interfaces.md         # 技能 7
      table-driven-test-pattern.md          # 技能 8
      sync-map-vs-rwmutex.md                # 技能 9
      channel-pipeline-pattern.md           # 技能 10
```

每个技能文件包含：
- **场景描述**：何时使用此技能
- **代码模板**：可直接复用的代码骨架
- **反模式警告**：常见错误及后果
- **契约要求**：使用此技能需要哪些接口/类型已存在
- **测试模板**：如何测试遵循此技能的代码

**检索与注入机制**：

1. **任务级自动检索**
   - 在 `executor.go` 的 `buildGenerationPrompt()` 中，根据任务描述做关键词匹配 + 语义检索。
   - 例如任务包含"HTTP server" → 检索 `http-server-graceful-shutdown.md`。
   - 语义检索可用简单的 embedding 匹配（先用关键词过滤，再用 cosine similarity）。

2. **错误驱动检索**
   - 当 `ContextEngine` 诊断到特定错误时，检索相关技能。
   - 例如：race detector 报错 → 检索 `sync-map-vs-rwmutex.md` 和 `channel-pipeline-pattern.md`。

3. **技能积累**
   - 当 Executor 成功完成一个任务后，提取其代码模式（去掉业务逻辑，保留结构），存入技能库。
   - 由 `orchestrator.go` 的 `post-task hook` 触发（见 7.9）。

**对接文件**：
- 新建 `pkg/skills/runtime/` 目录存放运行时技能
- 新建 `pkg/skills/retrieval.go`：关键词 + embedding 检索器
- `pkg/agent/executor.go`：`buildGenerationPrompt` 注入技能上下文
- `pkg/agent/context_engine.go`：错误诊断时注入修复技能

### 7.8 跨 Agent 一致性检查 (Cross-Agent Consistency Checker)

**定位**：L5 评审层。解决"单叶通过、全局断裂"的问题。

**当前问题**：
- `hasBlockingDevelopmentFailure()`（`teams.go` line 776）只检查单个 Agent 的失败状态，不检查全局一致性。
- 没有机制检测：Agent A 改了接口，Agent B 的实现文件虽然编译通过但行为已错。

**改进方案**：

1. **全局编译门 (Global Compile Gate)**
   - 在所有叶子执行完成后（`teams.executeWorkflow` 结束前），强制对整个仓库跑 `go build ./...`。
   - 即使每个叶子单独编译通过，全局编译仍可能失败（因为叶子间的依赖关系可能在执行期间被改变）。
   - 这是 **Hard Gate**：全局编译失败 → 整个 workflow 标记为 Failed。

2. **全局测试门 (Global Test Gate)**
   - 全局编译通过后，跑 `go test ./...`（含 `-race`）。
   - 这是 **Hard Gate**。

3. **接口一致性扫描**
   - 用 `ContractStore` 的全量符号表扫描：
     - 所有 `InterfaceContract` 的实现者是否都满足最新版契约？
     - 有没有新增的实现者未注册到 `Implementors` 索引？
     - 有没有被删除的方法仍有调用者？
   - 不一致时生成 `consistency_repair` 子任务，自动分配给 Executor。

4. **变更影响分析 (Impact Analysis)**
   - 新增 `pkg/agent/impact_analyzer.go`：
     - 输入：两个 commit 之间的 diff（或当前工作树与 base 的 diff）。
     - 输出：被修改的符号 → 所有引用这些符号的文件/测试。
     - 检查：所有受影响文件是否都被某个 Agent 审查过？
   - 若某文件被修改但其引用者未经过测试，标记为 `impact_uncovered`。

**对接文件**：
- `pkg/agent/teams.go`：`executeWorkflow` 结束前插入 Global Compile/Test Gate
- 新增 `pkg/agent/consistency_checker.go`
- 新增 `pkg/agent/impact_analyzer.go`
- `pkg/agent/contract_store.go`：增加全局一致性扫描方法

### 7.9 约束效果遥测 (Constraint Telemetry)

**定位**：跨所有层的度量基础设施。解决"不知道约束是否有效"的问题。

**设计**：

1. **指标定义**
   ```go
   // pkg/metrics/constraint_metrics.go
   type ConstraintMetrics struct {
       GatePassRate      map[string]float64   // 每个 gate 的通过率
       GateFailureReason map[string]map[string]int // gate -> reason -> count
       SkillEffectiveness map[string]float64   // skill -> 平均修复轮数降低比例
       ConstitutionViolation map[string]int     // 宪法条款 -> 违规次数
       ConflictRate      float64               // 并行任务冲突率
       MergeSuccessRate  float64               // CRDT/Mergiraf 合并成功率
       AvgRepairRounds   float64               // 平均修复轮数
       RaceDetectionRate float64               // race detector 触发率
       SecurityFindingRate float64             // security gate 触发率
       PerformanceRegressionRate float64       // performance gate 回归率
       DeliveryStatusDistribution map[string]int // Completed / DeliveredWithRemediation / Failed
   }
   ```

2. **采集点**
   - `validation_gate.go`：`Validate()` 返回时记录各 stage 结果。
   - `adversarial.go`：评审完成后记录各维度评分。
   - `patch_apply.go`：冲突检测时记录冲突类型（文本冲突 vs 结构冲突）。
   - `teams.go`：workflow 结束时记录最终交付状态。
   - `executor.go`：每轮修复后记录轮数。

3. **A/B 测试框架**
   - 对 Skill 效果进行 A/B 测试：
     - 对照组：不注入 `simplicity-first`
     - 实验组：注入 `simplicity-first`
     - 比较：平均生成行数、平均修复轮数、通过率
   - 由 `orchestrator.go` 在任务分配时随机选择（基于 task hash 决定分组，保证可复现）。

4. **Dashboard 与告警**
   - 复用现有 Grafana dashboard（`grafana-dashboard-claude-go.json`）。
   - 新增 panels：
     - Gate Pass Rate by Stage
     - Constitution Violation Heatmap
     - Parallel Edit Conflict Rate
     - Skill Effectiveness Trend
   - 告警规则：
     - Security Gate 触发率 > 5% → P1 告警
     - Race Detector 触发率 > 10% → P1 告警
     - 平均修复轮数 > 3 → P2 告警

**对接文件**：
- 新增 `pkg/metrics/constraint_metrics.go`
- 修改 `pkg/agent/validation_gate.go`、`adversarial.go`、`patch_apply.go`、`teams.go`、`executor.go` 增加 metrics 上报点
- 修改 `grafana-dashboard-claude-go.json` 新增 panels

---

## 8. 与 530d18c 的具体对接与改造点

### 8.1 文件级改造清单

| 文件 | 当前行数 | 改造内容 | 预估新增行数 |
|------|---------|---------|------------|
| `pkg/contract/types.go` | 79 | 新增 ContractVersion、Locked 字段 | +30 |
| `pkg/agent/contract_store.go` | 846 | 新增 Lock/Unlock、tree-sitter 桥接、全局一致性扫描 | +200 |
| `pkg/agent/validation_gate.go` | 368 | 扩展为 8 阶段 gate；新增 Security/Concurrency/Performance 处理 | +250 |
| `pkg/agent/executor.go` | 538 | 注入宪法前缀；注入技能库；Self-Refine 内循环；测试编译预检 | +150 |
| `pkg/agent/orchestrator.go` | ~4744 | 冲突键智能优化；自动注入 test 依赖；契约锁定串行化；一致性检查 | +400 |
| `pkg/agent/patch_apply.go` | 403 | 新增 ApplyCRDT、ApplyMergiraf、validateImports | +200 |
| `pkg/agent/adversarial.go` | 73 | 扩展为多维度评审；新增 GlobalConsistencyReviewer | +150 |
| `pkg/agent/roles.go` | ~76 | 新增 test-designer、security-reviewer、concurrency-reviewer 角色 | +80 |
| `pkg/agent/teams.go` | ~47 | 全局编译/测试门；一致性检查调用 | +60 |
| `pkg/agent/context_engine.go` | 428 | 新增并发诊断策略；技能检索注入 | +100 |
| `pkg/toolskill/security_scan.go` | — | 新建：gosec 集成 + secret scan + CWE regex | +200 |
| `pkg/toolskill/benchmark_gate.go` | — | 新建：benchstat 比较 + 回归检测 | +150 |
| `pkg/skills/runtime/go/*.md` | — | 新建：10+ Go 技能模板 | +800 |
| `pkg/skills/retrieval.go` | — | 新建：技能检索器 | +100 |
| `pkg/merge/crdt.go` | — | 新建：CRDT 文档与合并 | +250 |
| `pkg/merge/mergiraf.go` | — | 新建：Mergiraf 调用封装 | +80 |
| `pkg/agent/consistency_checker.go` | — | 新建：全局一致性扫描 | +150 |
| `pkg/agent/impact_analyzer.go` | — | 新建：变更影响分析 | +120 |
| `pkg/metrics/constraint_metrics.go` | — | 新建：约束指标定义与采集 | +100 |
| `grafana-dashboard-claude-go.json` | 140834 | 新增约束相关 panels | — |

### 8.2 接口变更

```go
// pkg/contract/types.go
+ type ContractVersion struct { ... }
+ type InterfaceContract struct {
+     ...
+     Versions []ContractVersion
+     Locked   bool
+ }

// pkg/agent/contract_store.go
+ func (cs *ContractStore) LockInterface(name, agentID string) error
+ func (cs *ContractStore) UnlockInterface(name, agentID string) error
+ func (cs *ContractStore) GlobalConsistencyScan() ([]Inconsistency, error)

// pkg/agent/validation_gate.go
+ type ValidationStage struct {
+     Name    string // "security", "concurrency", "performance"
+     Hard    bool
+     AutoFix bool
+ }

// pkg/agent/executor.go
+ func (ce *CodeExecutor) SelfRefine(diags []Diagnostic) error
+ func (ce *CodeExecutor) PreFlightCompileCheck() error

// pkg/agent/orchestrator.go
+ func (o *Orchestrator) AutoInjectTestDeps(raw []RawTask) []RawTask
+ func (o *Orchestrator) SmartConflictKeys(raw []RawTask) []RawTask

// pkg/agent/adversarial.go
+ type MultiDimEvalScore struct {
+     Correctness  DimensionScore
+     Security     DimensionScore
+     Concurrency  DimensionScore
+     Performance  DimensionScore
+     Idiomatic    DimensionScore
+ }
+ type GlobalConsistencyReviewer struct { ... }
```

### 8.3 关键算法伪代码

**8.3.1 六维质量门执行流程**

```go
func (vg *ValidationGate) ValidateV2(workspace string) (*ValidationReport, error) {
    report := &ValidationReport{StartedAt: time.Now()}
    for _, stage := range vg.config.stages {
        result := vg.runStage(stage, workspace)
        report.Stages = append(report.Stages, result)

        if !result.Pass && stage.Hard {
            if stage.Name == "security" {
                // 安全失败不可自动修复，必须显式确认
                return report, ErrSecurityViolation{Findings: result.Findings}
            }
            if stage.Name == "concurrency" && vg.config.AutoRaceFix {
                // 并发失败可尝试 RAG 修复（见 7.9）
                suggestions := vg.contextEngine.DiagnoseRace(result.RawOutput)
                return report, ErrConcurrencyViolation{Suggestions: suggestions}
            }
            return report, ErrHardGateFailed{Stage: stage.Name}
        }

        if !result.Pass && stage.Name == "performance" {
            // 性能回归软门，标记但不阻塞
            report.HasPerformanceRegression = true
        }
    }
    return report, nil
}
```

**8.3.2 并行编辑合并流程**

```go
func (pa *PatchApplier) MergeParallelEdits(edits []EditSnippet) (*MergeResult, error) {
    // 1. 按文件分组
    byFile := groupByFile(edits)

    // 2. 单文件内合并
    for file, fileEdits := range byFile {
        if len(fileEdits) == 1 {
            // 只有一个编辑，直接应用
            pa.applyToFile(file, fileEdits[0])
            continue
        }

        // Level 2: CRDT 合并
        crdtDoc := pa.crdtLoader.Load(file)
        for _, e := range fileEdits {
            crdtDoc.Apply(e.StartMarker, e.EndMarker, e.Replacement)
        }
        mergedText := crdtDoc.Text()

        // Level 3: 语法校验
        if err := pa.validateSyntax(file, mergedText); err != nil {
            // AST 失败，尝试 Mergiraf
            base := pa.vcs.Read(file)
            left := pa.applyEdits(base, fileEdits[:len(fileEdits)/2])
            right := pa.applyEdits(base, fileEdits[len(fileEdits)/2:])
            mergedText, err = pa.mergiraf.Merge(base, left, right)
            if err != nil {
                return nil, ErrMergeConflict{File: file}
            }
        }

        // Level 4: Import 校验
        if err := pa.validateImports(file, mergedText); err != nil {
            return nil, ErrImportConflict{File: file}
        }

        pa.writeFile(file, mergedText)
    }

    return &MergeResult{Success: true}, nil
}
```

**8.3.3 TDD 循环任务注入**

```go
func (o *Orchestrator) AutoInjectTestDeps(raw []RawTask) []RawTask {
    taskMap := make(map[string]RawTask)
    for _, t := range raw {
        taskMap[t.ID] = t
    }

    var out []RawTask
    for _, t := range raw {
        out = append(out, t)
        if t.WorkUnitType == "implementation" {
            // 检查是否已有 test 前置依赖
            hasTestDep := false
            for _, depID := range t.DependsOn {
                if strings.HasPrefix(taskMap[depID].WorkUnitType, "test") {
                    hasTestDep = true
                    break
                }
            }
            if !hasTestDep {
                testTask := RawTask{
                    ID:           t.ID + "_test",
                    WorkUnitType: "test_design",
                    CapabilityID: "test-designer",
                    ReadFiles:    t.ReadFiles,
                    WriteFiles:   []string{inferTestFile(t.WriteFiles)},
                    DependsOn:    t.DependsOn, // 与实现任务共享前置依赖
                }
                t.DependsOn = append(t.DependsOn, testTask.ID)
                out = append(out, testTask)
            }
        }
    }
    return out
}
```

---

## 9. 实施路线图

### Phase 1：立即实施（0-2 周）—— 堵住安全与并发黑洞

| 任务 | 文件 | 工作量 | 验收标准 |
|------|------|--------|---------|
| 编写 Go 代码宪法 | `pkg/skills/builtin/go-constitution/SKILL.md` | 0.5 人天 | 宪法注入后，`ConstitutionComplianceCheck` 发现 100% 的 `crypto/md5` 和裸 map 读写 |
| 扩展 Validation Gate 至 8 阶段 | `pkg/agent/validation_gate.go` + 新建 `security_scan.go`/`benchmark_gate.go` | 3 人天 | `go test -race` 和 `gosec` 集成到 gate，失败的 MR 无法进入 review |
| 新增安全/并发评审 Agent | `pkg/agent/roles.go` + 新建 `security-review/SKILL.md`/`concurrency-review/SKILL.md` | 2 人天 | 评审输出包含 Security 和 Concurrency 维度评分 |
| TDD 循环任务注入 | `pkg/agent/orchestrator.go` | 2 人天 | 所有 implementation 叶子自动有 test_design 前置依赖 |
| 全局编译/测试门 | `pkg/agent/teams.go` | 1 人天 | 所有叶子完成后强跑 `go build ./...` 和 `go test ./...` |

### Phase 2：短期优化（2-6 周）—— 提升质量与效率

| 任务 | 文件 | 工作量 | 验收标准 |
|------|------|--------|---------|
| 契约锁与版本化 | `pkg/contract/types.go` + `contract_store.go` | 3 人天 | 两个 Agent 不能同时修改同一接口；Breaking Change 自动触发迁移任务 |
| Go 技能库（首批 10 个技能） | `pkg/skills/runtime/go/*.md` + `retrieval.go` | 4 人天 | 任务匹配准确率 > 70%（关键词+embedding） |
| Self-Refine 内循环 | `pkg/agent/executor.go` | 2 人天 | 编译错误在提交给 Validator 前，Executor 内部尝试 1 轮自修复 |
| 属性测试生成 | 新建 `pkg/toolskill/property_test.go` | 3 人天 | 对纯函数自动生成 `testing/quick` 属性测试，覆盖率提升 15% |
| 约束效果遥测 | 新建 `pkg/metrics/constraint_metrics.go` + Grafana panels | 3 人天 | Dashboard 可查看各 gate 通过率和 Skill 效果趋势 |

### Phase 3：中期演进（6-12 周）—— 释放并行与智能

| 任务 | 文件 | 工作量 | 验收标准 |
|------|------|--------|---------|
| CRDT 并行编辑 | 新建 `pkg/merge/crdt.go` + `patch_apply.go` | 5 人天 | 并行 Agent 编辑不同区域时 0 冲突，合并后语法 100% 通过 |
| 语法感知语义合并 | 新建 `pkg/merge/mergiraf.go` | 4 人天 | Mergiraf 消除 80% 的"移动+编辑"假冲突 |
| 冲突键智能优化 | `pkg/agent/orchestrator.go` | 3 人天 | Read-Only 共享任务不再串行；符号级依赖替代文件级 |
| 性能回归门硬化 | `pkg/toolskill/benchmark_gate.go` | 2 人天 | P95 延迟回归 > 20% 时自动降级为 `DeliveredWithRemediation` |
| Prompt 自动优化（DSPy/TextGrad 风格） | 新建 `pkg/prompt/optimizer.go` | 5 人天 | Planner 和 Executor 的 prompt 在验证集上 pass@1 提升 10% |
| 本地模型安全微调（可选） | 外部训练流水线 | 10 人天 | 使用 SafeCoder/Secure-Instruct 数据集微调本地 7B 模型，安全漏洞率降低 30% |

---

## 10. 风险与缓解

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| **质量门过于严格导致任务失败率上升** | 高 | 初期 Security/Concurrency 设为 Soft Gate，观察 2 周后再硬化；提供明确的修复模板 |
| **CRDT 合并引入不可预期的语义错误** | 高 | 合并后必须跑全局编译+测试；保留"保守回退"开关，可在配置中关闭 CRDT |
| **技能库检索噪声大** | 中 | 先用关键词硬匹配过滤，再用 embedding 排序；人工审核首批技能 |
| **契约锁定导致 DAG 串行化严重** | 中 | 锁粒度控制在接口级别而非文件级别；锁超时自动释放（默认 5 分钟） |
| **多维度评审增加 Token 成本** | 中 | Security/Concurrency 评审只在可疑代码路径触发（由静态分析预筛）；Idiomatic 评审抽样执行 |
| **tree-sitter 引入 C 依赖** | 低 | 使用纯 Go 的 tree-sitter binding（如 `smacker/go-tree-sitter`），或作为可选插件 |

---

## 11. 参考来源

### 业界调研
- Anthropic Claude Code: https://docs.anthropic.com/en/docs/agents-and-tools/claude-code/overview
- OpenAI Codex CLI: https://github.com/openai/codex
- Cursor Rules: https://docs.cursor.com/context/rules
- OpenHands / OpenDevin: https://github.com/All-Hands-AI/OpenHands
- Aider Conventions: https://aider.chat/docs/usage/conventions.html
- Cline / Roo Code: https://github.com/cline/cline
- Windsurf: https://docs.codeium.com/windsurf
- Kimi / Moonshot: https://www.moonshot.cn/
- GLM / Zhipu: https://www.zhipu.ai/

### 学术论文
- Constitutional AI (Bai et al., 2022): https://arxiv.org/abs/2212.08073
- C3AI (Kyrychenko et al., 2025): https://arxiv.org/abs/2502.15861
- Evolving Constitutions (Kumar et al., 2026): https://arxiv.org/abs/2602.00755
- Self-Refine (Madaan et al., 2023): https://arxiv.org/abs/2303.17651
- Reflexion (Shinn et al., 2023): https://arxiv.org/abs/2303.11366
- CriticGPT (McAleese et al., 2024): https://arxiv.org/abs/2407.00215
- Multi-Agent Debate (Du et al., 2023): https://arxiv.org/abs/2305.14325
- MetaGPT (Hong et al., 2023): https://arxiv.org/abs/2308.00352
- ChatDev (Qian et al., 2023): https://arxiv.org/abs/2307.07924
- SWE-Agent (Yang et al., 2024): https://arxiv.org/abs/2405.15793
- AgentCoder (Huang et al., 2023): https://arxiv.org/abs/2312.13010
- LLM4TDD (Piya & Sullivan, 2023): https://arxiv.org/abs/2312.04687
- IRIS (Li et al., 2024): https://arxiv.org/abs/2405.17238
- SelectIssues (Blyth et al., 2025): https://arxiv.org/abs/2508.14419
- SafeCoder (He et al., 2024): https://arxiv.org/abs/2402.09497
- Secure-Instruct (Li et al., 2025): https://arxiv.org/abs/2510.07189
- DR.FIX / GenAI Datarace Fix (Jin et al., Uber): https://arxiv.org/abs/2504.15637
- CONCUR (Huang et al., 2026): https://arxiv.org/abs/2603.03683
- ENAMEL (Qiu et al., 2024): https://arxiv.org/abs/2406.06647
- Eg-walker (Gentle & Kleppmann, 2024): https://arxiv.org/abs/2409.14252
- CodeCRDT (Pugachev, 2025): https://arxiv.org/abs/2510.18893
- Mergiraf (Delpeuch, 2024): https://mergiraf.org/
- MergeGen (Dong et al., 2023): https://yilinglou.github.io/papers/ASE23_merge.pdf
- Voyager (Wang et al., 2023): https://arxiv.org/abs/2305.16291
- DSPy (Khattab et al., 2023): https://arxiv.org/abs/2310.03714
- TextGrad (Yuksekgonul et al., 2024): https://arxiv.org/abs/2406.07496
- SWE-bench Verified (2024): https://www.swebench.com/

---

> **结语**：Commit `530d18c4` 已经搭建了 contract-first、gate-governed、DAG-compiled 的骨架。本方案在此基础上，用"五层防御体系"补齐安全、并发、性能三大维度，用"并行编辑合并引擎"释放多 Agent 并行潜力，用"TDD 多 Agent 循环"和"CriticGPT 式对抗评审"把质量内建于流程之中。最终目标是让 AI Agent 团队产出的代码，在质量、安全、并发、性能四个维度上，达到甚至超过资深人类工程师的平均水平。
