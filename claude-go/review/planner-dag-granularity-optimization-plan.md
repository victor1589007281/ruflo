# Planner DAG 粒度与并发优化方案

日期: 2026-05-07  
范围: `/Users/huaquan.liang/Documents/GitHub/ruflo/claude-go`

> 更新: 本文是问题分析与演进记录。正式的语言无关方案与一步到位实施方案见 `review/universal-planner-granularity-dag-formal-plan.md`。

## 结论

当前 `claude-go` 的 Planner 已经从“自由文本计划”演进到严格 JSON WBS、Macro/Leaf/Verification、TaskSizingGate、split-on-timeout 和本地 verification gate。但它本质上仍是“提示词约束 + 运行时补救”，不是“设计合约先行 + DAG 编译器 + 并发优化器”。

这会导致三个直接问题:

- 粒度仍偏大: Planner 只被要求给 `targetFiles` 和 `estimatedMinutes`，没有被强制拆到“一个接口/一个符号/一个行为簇”的 edit primitive。
- 并发度不高: `parallelGroup` 和共享 `targetFiles` 会串行化任务；当前 fallback/split 大量继承父任务的 group/files，导致本可并行的模块被压成链。
- 动态 DAG 缺少整体设计参考: timeout split 会注入当前 API 摘要，但初始 plan 和 sizing split 还没有一个机器可检查的 interface contract / module DAG 作为源头约束。

建议下一步不是继续加 prompt 规则，而是引入 `ContractPlan -> WorkPlan -> DAGPlan` 三层规划，并用确定性的 DAG compiler 在执行前校验、拒绝、拆分和重排任务。

## 2026-05-07 修正: timeout 不是粒度判断器

用户反馈是对的: 如果初始 Plan 合理，常规路径不应该等到 6 分钟超时后才动态拆 DAG。`split-on-timeout` 只能是异常恢复手段，不应该成为粒度控制主路径。

更准确的设计应区分两类问题:

- `plan_granularity_problem`: 初始 leaf 本身不是可执行 edit primitive，包含多个 capability、多个接口、多个文件簇、未知探索、过大 LOC 或模糊验收。
- `execution_runtime_problem`: leaf 粒度合理，但因为 provider 延迟、429 重试、prompt/context 过大、测试卡死、依赖安装、sandbox 冷启动、调度 batch wait、死锁、OOM 等原因超时。

因此新的目标是:

- 在 Planner 输出后、DAG 入库前，用 `Leaf Definition of Ready` 和 `Plan Readiness Gate` 拒绝过粗任务。
- 让 80% 以上拆分发生在 plan/compiler 阶段，而不是 timeout 后。
- timeout 先进入 `TimeoutClassifier`，只有 `true_oversize` 才触发 split；其它原因走对应恢复策略。
- 动态 DAG 只用于运行期事实变化，例如 API drift、重复定义、测试暴露的新依赖、真实文件结构与计划不符，不用于弥补初始 Plan 过粗。

## 开发粒度的真实定义

当前文档里的 “1-3 个 targetFiles / 2-4 分钟” 还不够。开发粒度不能只看时间和文件数，而要定义成一个可落盘、可验证、可回滚的 code edit transaction。

一个 `leaf` 必须满足:

- 单一 capability: 只实现一个能力片段，例如 `VectorStore.SearchTopK`，不能是“实现向量存储和混合查询”。
- 单一接口面: 最多新增或修改 1 个 public interface/type；普通实现 leaf 默认不得修改 contract。
- 单一写入簇: 默认最多 1 个实现文件 + 1 个测试文件；只读文件不算冲突。
- 单一行为预算: 最多 1-2 个核心行为，测试覆盖这些行为即可。
- 单一验收命令: 必须有确定命令，且该命令不依赖外网、长服务或人工输入。
- 单一 diff 预算: 默认 50-180 changed LOC；超过 250 LOC 必须拆分。
- 无探索遗留: leaf 不能包含“分析/调研/找出/设计/决定”等动词；这些必须是前置 discovery 或 contract task。
- 无模糊依赖: `requires` 必须被 contract 或上游 leaf `provides` 满足。

对应的反例:

- “实现 KV/File/Vector/Graph/Index 存储”不是 leaf，是 capability macro。
- “实现 AgentDB 核心引擎”不是 leaf，是 macro。
- “实现文件存储和向量检索并完成集成测试”不是 leaf，是至少 3 个 leaf。
- “根据现有代码找出需要修改的地方并实现”不是 coder leaf，是 discovery + edit leaf。
- “修复所有编译错误”不是 leaf，除非错误已归类到具体文件和符号。

## Plan Readiness Gate

Planner 输出后先进入 `Plan Readiness Gate`，不通过则 repair/replan，不进入 Orchestrator 执行。

建议 Gate 规则:

| Gate | 检查 | 不通过处理 |
|---|---|---|
| `G1 capability_singleton` | title/acceptance 不能同时包含多个 capability: KV、File、Vector、Graph、Index、Router、Facade 等 | 拆成 capability leaf fan-out |
| `G2 write_budget` | `writeFiles <= 2`，或同一 symbol cluster | 按文件/符号拆 |
| `G3 loc_budget` | `estimatedChangedLOC <= 180`，hard max 250 | 按 symbol 或 behavior 拆 |
| `G4 contract_locked` | 非 contract/migration leaf 不得修改 `mustNotModifySymbols` | 插入 contract migration 或重写 leaf |
| `G5 requires_resolved` | 每个 `requires` 都来自 contract 或上游 `provides` | 插入 contract/skeleton leaf |
| `G6 no_exploration_in_leaf` | coder leaf 不含“调研/分析/找出/决定/设计” | 拆为 discovery + implementation |
| `G7 verify_bounded` | verifyCommand 必须本地、短时、可沙盒运行 | 换成本地 scoped test |
| `G8 width_floor` | 多 capability 项目初始 DAG width 不能低于 3 | 移除多余串行边或 replan |
| `G9 critical_path_budget` | critical path 预计时间不能接近总任务时间 | 触发 parallelism optimizer |
| `G10 prompt_budget` | 单 leaf prompt 预计上下文小于预算，如 8-12KB | 改成 artifact refs + readFiles |

这一步应该替代现在“先入 DAG，运行中再发现大”的做法。换句话说，`shouldSplitRawTask()` 不应只是 normalize 阶段的软拆分，而要升级成 plan compiler 的 hard gate。

## Timeout 分类矩阵

超时只是症状，不是根因。建议新增 `TimeoutClassifier`，先分类再处理。

| kind | 判断信号 | 处理策略 | 是否 split |
|---|---|---|---|
| `true_oversize` | changed LOC 超预算、输出接近上限、同一 leaf 多轮仍在新增大块代码、实际执行超过 estimated 2x | 拆 leaf 或 DAGPatch | 是 |
| `provider_latency` | LLM 请求耗时长但无输出、HTTP 超时、provider 慢 | 模型 fallback、重试、降并发 | 否 |
| `rate_limit_retry` | 429/限流等待占主要耗时 | rate limiter/backoff/换 provider | 否 |
| `prompt_bloat` | prompt component ledger 显示 system/tools/messages 过大 | 上下文引用化、压缩 prompt | 否 |
| `agent_loop` | reviewer/tester 对抗轮过多，反复改同类问题 | 降轮数、fast-pass、聚焦 must-fix | 不一定 |
| `build_test_hang` | LLM 已完成，build/test 超时 | sandbox timeout、测试命令限时、跳过慢测 | 否 |
| `dependency_install` | go mod/npm/pip 拉依赖耗时 | dependency-sync task、cache、允许网络 profile | 否 |
| `sandbox_cold_start` | Docker pull/启动耗时 | 预热镜像、复用 cache | 否 |
| `scheduler_wait` | ready 有任务但 batch barrier 等慢任务 | worker refill 调度 | 否 |
| `deadlock_or_zombie` | in_progress 长时间无 LLM/tool 活动 | heartbeat recovery | 否 |
| `oom_or_output_limit` | sandbox OOM/output limit | 缩小测试/修代码/限制输出 | 不一定 |

只有 `true_oversize` 和少数 `agent_loop` 才进入 split。其它 timeout 继续 split 只会制造更多任务，不能解决根因。

## 初始 Plan 应如何避免粒度过大

新的初始 Plan 不能直接输出“任务列表”，而要走下面的前置过程:

### 1. Capability Decomposition

先把需求拆成 capability，而不是拆成开发阶段。

AgentDB 示例:

```text
capabilities:
  contract/core API
  KV store
  file object store
  vector store
  graph store
  inverted index
  query router
  snapshot/persistence
  facade/integration
```

每个 capability 再拆成:

```text
contract refs -> implementation leaf -> focused tests -> integration edge
```

### 2. Interface-first Skeleton

第一批 leaf 只做:

- `go.mod`
- `contracts.go`
- `errors.go`
- 最小 `agentdb.go`
- compile-time assertions 或空实现

这一步的目的不是实现功能，而是把所有 agent 后续要遵守的 API 锁定住。

### 3. Capability Fan-out

Contract skeleton 通过后，不同 capability 默认并发。只有满足以下条件才串行:

- 写同一文件。
- 修改同一 public symbol。
- 共享同一 mutable state。
- 一个 capability 显式 requires 另一个 capability 的 concrete implementation，而不仅是 interface。

### 4. Integration Fan-in

集成 leaf 只做 wiring，不允许实现核心逻辑。它依赖所有 capability terminal leaf。

### 5. Verification

verification 只跑本地 build/test/TODO scan，不调用 tester LLM。

## 对原方案的修正

原方案偏向“出现 timeout 后再用 DAGPatch 修”。修正后应改成:

- `DAGPatch` 仍保留，但定位为异常恢复。
- 粒度控制前移到 `Plan Readiness Gate`。
- `ContractPlan` 不只是防接口漂移，也用于计算 capability fan-out 和 leaf 边界。
- `DAGCompiler` 不只是补依赖边，还必须拒绝不可执行 leaf。
- `TimeoutClassifier` 决定 timeout 的根因和策略，禁止所有 timeout 都 split。

验收指标也要调整:

- `plan_repair_split_count` 应高于 `timeout_split_count`。
- `timeout_split_count / total_leaf_count` 应低于 5%-10%。
- `timeout_true_oversize_ratio` 应被单独统计，不能把 provider/build/sandbox timeout 混在一起。
- `initial_dag_width` 和 `actual_ready_width` 应接近 capability 数，而不是长期为 1。

## Planner 修改历史

从 `git log` 和 `git blame` 看，Planner/Orchestrator 主要经历了这些阶段:

| 时间 | Commit | 变化 | 影响 |
|---|---|---|---|
| 2026-04-15 | `a6b2c237e` | `planer抽离`，把 planner 从设计/实现链路里独立出来 | 形成 Plan-then-Execute 基础，planner 负责设计评审和 WBS |
| 2026-04-16 | `2e2e27fb1` | WBS 改为严格 JSON，解析优先 JSON，失败再 fallback | 降低自由文本不可解析问题，但 schema 仍偏任务清单 |
| 2026-04-18 | `2f44922f4` | 粒度精细化、多语言支持，加入 title/acceptance/priority/complexity | 开始对任务质量做 prompt 约束 |
| 2026-05-01 | `fd5d9c500` | 禁止 Orchestrator silent fallback、任务级编译隔离、Plan 上下文预算 | 让 plan 缺失/解析失败更显性，build/test 更接近任务作用域 |
| 2026-05-06 | `77d6a2d0f` | Macro/Leaf/Verification、TaskSizingGate、split-on-timeout、LLM Split Planner、objective fallback | 解决超时原样重试，但引入了大量串行兜底任务 |
| 2026-05-06 | `fbd1c1f81` 之后 | sandbox build/test | 解决坏测试 OOM，不直接改善 planner DAG 质量 |

当前关键代码位置:

- Planner prompt: `pkg/agent/workflow.go` 的 `developmentWorkflow` plan 阶段。
- WBS schema: `pkg/agent/orchestrator.go` 的 `TaskNode`、`rawTask`、`wbsJSONTask`。
- sizing gate: `normalizeAndSplitRawTasks()`、`shouldSplitRawTask()`、`expandRawTask()`。
- DAG 入库与并发约束: `rawTasksToDAG()`。
- 超时动态拆分: `handleTaskTimeout()`、`addTimeoutSplitChildren()`、`buildTimeoutSplitPlannerPrompt()`。

## 当前实现的关键不足

### 1. Prompt 有“接口检查”，但没有机器可检查的接口合约

Planner prompt 要求“接口定义完整”“接口签名与设计一致”，但 WBS task 只保存了 `designRef`、`constraints`、`acceptance` 这些文本字段。Orchestrator 无法判断:

- 这个 leaf 提供了哪些接口、类型、方法。
- 它依赖哪些接口、类型、方法。
- 它是否允许修改这些接口。
- 它是否重复定义了已经存在的符号。
- 它是否破坏了其他 leaf 依赖的 contract。

AgentDB 的接口漂移、重复定义和 deterministic contract patch，本质上就是“合约没先落地、任务只靠自然语言对齐”的结果。

### 2. fallback 强行串行化新 Go 项目

`synthesizeObjectiveWBS()` 对新 Go 项目直接生成稳定内置 Leaf DAG，AgentDB 示例是:

```text
1 初始化骨架
2 KV/File
3 Vector
4 Graph
5 Inverted Index
6 Facade/E2E
7 Verification
```

其中 `2 -> 3 -> 4 -> 5 -> 6 -> 7` 全部串行。但 AgentDB 这类系统天然可以在 contract skeleton 之后并行:

```text
contract skeleton
  -> KV/File
  -> Vector
  -> Graph
  -> InvertedIndex
  -> Memory/Session
  -> Router
integration facade
verification
```

当前 fallback 保守但牺牲了并发宽度。它提高了成功兜底率，却没有体现动态 DAG 的优势。

2026-05-09 更新: universal fallback 已从 7 个任务调整为 8 个任务，把原来的粗粒度 `实现最小本地数据存储` 拆成 `实现存储构造与私有状态` 和 `实现存储 CRUD 闭环`。AgentDBV4 第六轮真实研发团队运行中，这两个 leaf 分别收敛并通过，最终 8/8 编排成功、E2E 通过、独立 `go test ./...` 通过。这证明“前置拆粗 leaf”比等 E2E/timeout 救火更稳定。

### 3. generic split 是固定三段串行

`expandGenericRawTask()` 只有:

```text
接口与数据结构边界 -> 核心行为实现 -> 本地验证
```

这适合单模块任务，不适合多能力系统。对于“实现 AgentDB 的文件、向量、图谱、倒排索引、普通存储”，generic split 不会按 capability fan-out，而是继续形成串行链。

### 4. `parallelGroup` 粒度过粗会吞掉并发

`rawTasksToDAG()` 会对相同 `parallelGroup` 自动串行化。timeout split 和 sizing split 又倾向继承父任务 `parallelGroup`。如果父任务 group 是 `agentdb-core`、`storage-core` 这种粗粒度标签，那么所有 child 会被强制串行。

正确做法是把 group 拆成更小的 conflict key:

- `contract:agentdb`
- `storage:kv`
- `storage:file`
- `index:vector`
- `index:inverted`
- `graph:memory`
- `facade:agentdb`

只有同一个 conflict key 或同一 write file 才串行。

### 5. targetFiles 继承过宽

timeout split 中，如果 child 没有 `targetFiles`，会继承 parent 的 `TargetFiles`。这会带来两个问题:

- 子任务全部共享同一批文件，`rawTasksToDAG()` 因共享文件把它们串行化。
- coder 得到的写入范围仍然过大，没被约束到具体文件/符号。

### 6. 调度器仍有 batch wait 损耗

`Execute()` 每轮取 ready batch，启动 goroutine 后等待整批完成，再回到主循环检查新的 ready task。这样如果一个快任务完成并释放了下游，仍需等待同批慢任务结束后下游才会被调度。注释说“流式调度”，但实现还是 batch barrier。

这会降低 DAG 的实际并发效率，尤其在 mixed-duration leaf 下更明显。

## 业界和论文参考

### Plan-then-Execute 只解决“先规划”，不解决“可执行粒度”

[Plan-and-Solve Prompting](https://arxiv.org/abs/2305.04091) 把任务先分成子任务再执行，用来减少 missing-step errors。它给我们的启发是: 需要明确 plan 阶段，但 plan 的输出还必须被约束成可验证、可重放、可调度的结构。

### Decomposed Prompting 支持递归拆分和专用子 prompt

[Decomposed Prompting](https://arxiv.org/abs/2210.02406) 强调复杂任务可以拆成更小子任务，并对难子任务继续递归拆分。对 claude-go 来说，sizing gate 不能只有固定模板，应按 capability、接口、文件和行为类型递归拆。

### ReAct 强调计划在执行中被 observation 修正

[ReAct](https://arxiv.org/abs/2210.03629) 的核心是 reasoning/action/observation 交替，让计划能跟随外部观察更新。对应到研发团队，planner 不应只在初始和 timeout 时工作，还应该在每个 leaf 后基于 AST/API diff、build/test 结果更新 DAG。

### CodePlan 把 repo-level coding 当成依赖/影响分析问题

[CodePlan](https://arxiv.org/abs/2309.12499) 的重要经验是: 仓库级修改不能直接靠 LLM 一次完成，而要结合 incremental dependency analysis、change may-impact analysis 和 adaptive planning。它不是按“模块标题”拆，而是按 code location、依赖和影响传播拆。claude-go 应引入 repo/API index，让 DAG 由依赖图和 write set 推导，而不是只靠 planner 文本。

### MetaGPT 通过 SOP 和中间产物降低级联幻觉

[MetaGPT](https://arxiv.org/abs/2308.00352) 把软件公司 SOP 编成 prompt sequence，要求角色交接和中间产物验证。对 claude-go 来说，`design -> plan -> code` 中间应新增机器可读的 `ContractPlan`，并让 reviewer/tester 验证 contract，而不是只读自然语言设计。

### AutoGen 和 Claude Code 强调 agent 能力边界

[AutoGen](https://arxiv.org/abs/2308.08155) 强调可定制 agent、工具、人类输入和交互模式。 [Claude Code subagents](https://code.claude.com/docs/en/sub-agents) 明确 subagent 的 tools、skills、memory、permission、worktree isolation、background 等可配置，且 subagent 不继承完整父上下文。claude-go 的 WBS 应携带 role profile、tool profile、skill refs、workspace/isolation 需求，避免所有 leaf 用同一类 coder 上下文。

### LangGraph 强调持久状态、确定性和 idempotent task

[LangGraph durable execution](https://docs.langchain.com/oss/python/langgraph/durable-execution) 要求长流程 checkpoint，并把有副作用/非确定性操作包进 task，保证恢复时不重复副作用。claude-go 已有 checkpoint/DAG，但 DAG 动态改写还需要版本化计划、任务幂等 key、artifact refs，否则 replan/rewire 容易污染历史状态。

### TDAG 强调动态任务分解和细粒度进度评估

[TDAG](https://arxiv.org/abs/2402.10178) 针对复杂任务执行中适应性不足、错误传播等问题，使用动态任务分解和 agent generation。claude-go 的 split-on-timeout 是这个方向的第一步，但触发太晚；应该在 build/test/API drift 观察到风险时提前 replan。

### Codex/Jules 类 coding agent 的共同点

[OpenAI Codex](https://openai.com/index/introducing-codex/) 和 [Codex cloud docs](https://developers.openai.com/codex/cloud) 强调每个任务在独立环境中读写代码、运行测试、给出 terminal/test evidence，并支持多任务并行。[Google Jules](https://jules.google/docs/) 明确“先给 plan，用户审核后再改代码”。这些方案的共同点不是任务越多越好，而是每个任务有清晰环境、可验证证据和可审查计划。

## 目标架构: ContractPlan -> WorkPlan -> DAGPlan

建议把 Planner 输出拆成三层，每层都有 schema 和 validator。

### 1. ContractPlan

ContractPlan 是设计和实现之间的机器合约，来源于 design stage + repo AST/API scan。

示例字段:

```json
{
  "schemaVersion": "planner.contract.v1",
  "projectRoot": "agentDBV1",
  "language": "go",
  "modules": [
    {
      "id": "vector",
      "package": "agentdbv1",
      "files": ["vector.go", "vector_test.go"],
      "provides": ["VectorStore", "VectorStore.Add", "VectorStore.SearchTopK"],
      "requires": ["RecordID", "ErrNotFound"],
      "invariants": ["dimension mismatch returns error", "search is deterministic"],
      "writeConflictKey": "index:vector"
    }
  ],
  "interfaces": [
    {
      "name": "VectorStore",
      "file": "contracts.go",
      "signature": "type VectorStore interface { Add(ctx context.Context, id string, vector []float64, metadata map[string]string) error; SearchTopK(ctx context.Context, query []float64, k int) ([]VectorHit, error) }"
    }
  ],
  "dependencyEdges": [
    {"from": "facade", "to": "kv"},
    {"from": "facade", "to": "vector"},
    {"from": "facade", "to": "graph"}
  ]
}
```

关键规则:

- ContractPlan 必须先落盘为 `contracts.go` 或 `internal/contracts/*.go`，并通过 `go test ./...` 的空实现编译。
- 后续 leaf 默认只能实现 contract，不能修改 contract。
- 修改 contract 只能通过 `contract-migration` 类型任务，并会阻塞依赖它的实现任务。

### 2. WorkPlan

WorkPlan 是面向 agent 的工作单元，不直接等于 DAG 边。它描述每个 leaf 的行为边界。

新增/强化字段:

```json
{
  "id": "vector-impl",
  "taskType": "leaf",
  "workUnitType": "implementation",
  "role": "coder",
  "contractRefs": ["VectorStore", "VectorHit"],
  "provides": ["MemoryVectorStore", "NewMemoryVectorStore"],
  "requires": ["VectorStore", "RecordID"],
  "readFiles": ["agentDBV1/contracts.go"],
  "writeFiles": ["agentDBV1/vector.go", "agentDBV1/vector_test.go"],
  "mayModifySymbols": ["MemoryVectorStore", "cosineSimilarity"],
  "mustNotModifySymbols": ["VectorStore", "DB"],
  "conflictKeys": ["index:vector"],
  "parallelSafety": "independent",
  "estimatedMinutes": 2,
  "estimatedChangedLOC": 180,
  "verifyCommand": "cd agentDBV1 && go test ./...",
  "acceptanceChecks": [
    "implements VectorStore",
    "dimension mismatch covered by test",
    "no changes outside writeFiles"
  ]
}
```

建议新增字段:

- `workUnitType`: `contract|skeleton|implementation|test|integration|migration|verification`。
- `contractRefs`: 当前 leaf 必须遵守的接口/类型。
- `provides` / `requires`: 用符号级依赖生成 DAG 边。
- `readFiles` / `writeFiles`: 区分读写，避免把只读文件当并发冲突。
- `mayModifySymbols` / `mustNotModifySymbols`: AST 级约束，防接口漂移。
- `conflictKeys`: 比 `parallelGroup` 更细的冲突域。
- `parallelSafety`: `independent|shared_read|exclusive|requires_merge`。
- `estimatedChangedLOC`: 防止一个 leaf 暗中变成大改。
- `artifactRefs`: 上游产物引用，不把大段正文复制进 prompt。

### 3. DAGPlan

DAGPlan 由确定性 compiler 从 ContractPlan + WorkPlan 生成，不能完全信任 LLM 的 `dependsOn`。

DAG compiler 负责:

- 基于 `requires/provides` 补依赖边。
- 基于 `writeFiles/conflictKeys/mustNotModifySymbols` 补冲突边。
- 删除不必要的串行边。
- 计算 critical path、max width、parallelism efficiency。
- 如果宽度过低且存在独立 capability，触发 replan。

## 优化后的 Planner 流程

### Step 0: Repo/API Indexer

在 planner 前运行轻量 indexer:

- Go: 解析 `go list ./...`、`go/packages` 或 `go/parser`，提取 package、type、interface、func、method、imports。
- 新项目: 生成空 repo index，但保留目标目录、语言、可用测试命令。
- 输出 `RepoIndexSummary`，限制在 2-5KB，给 Planner 和 DAG compiler 使用。

### Step 1: Contract Planner

LLM 只负责补齐架构合约，不直接排执行任务:

- 输出模块、接口、数据模型、错误、依赖方向。
- 每个模块必须有 `provides/requires/invariants/files/conflictKey`。
- 如果是新 Go 项目，第一批任务必须只创建 `go.mod`、`contracts.go`、`README`、空实现或 compile-time assertion。

### Step 2: Contract Validator

确定性校验:

- contract 是否缺失目标目录。
- 是否有循环依赖。
- 是否有多个模块 claim 同一 symbol。
- 是否有多个模块写同一文件。
- Go signature 是否能被 AST parser 解析。
- 新项目是否有 `go.mod` 和 package 名。

不通过时走 repair prompt，不进入 coder。

### Step 3: Work Planner

基于 ContractPlan 生成 WorkPlan:

- 不允许把 “实现 KV、文件、向量、图、倒排索引” 放在一个 leaf。
- 每个 leaf 默认 1 个 capability + 1 个实现文件 + 1 个测试文件。
- integration leaf 只能 wiring，不允许再实现核心算法。
- verification leaf 只跑本地命令，不调用 tester LLM。

### Step 4: DAG Compiler

确定性生成 DAG:

```text
provided symbols -> required symbols -> dependency edges
write files/conflict keys -> serialization edges
contract migration -> blocks dependents
verification -> depends on all terminal implementation leaves
```

然后计算:

- `criticalPathEstimatedMinutes`
- `dagMaxWidth`
- `dagWidthByLevel`
- `parallelismEfficiency = sum(leafEstimatedMinutes) / criticalPathEstimatedMinutes`
- `serialBottleneckReasons`

若 `parallelismEfficiency < 1.6` 且 leaf 总数大于 5，自动触发并发优化。

### Step 5: Parallelism Optimizer

优化器只做确定性重排，不让 LLM 直接决定并发。

规则:

- 共享 `readFiles` 不冲突。
- 不同 `writeFiles` 且不同 `conflictKeys` 可并发。
- 只依赖共同 contract 的实现 leaf 可并发。
- integration/facade 依赖所有终端 capability。
- verification 依赖 integration 或所有终端 leaf。
- 对同一 package 下不同文件的 Go 实现，默认可并发，但需要后置 package build gate。

### Step 6: Execution-time Classifier + Replanner

动态 DAG 不应该由 timeout 直接触发，而应该由结构化诊断触发。建议新增 `ExecutionIssueClassifier`，把 timeout/build/test/API drift 统一归类，再决定是否 replan。

建议触发:

- `api_drift`: AST 检测到 leaf 修改了 `mustNotModifySymbols`。
- `duplicate_symbol`: Go build 报重复定义。
- `missing_contract`: leaf 使用了未声明接口/类型。
- `changed_loc_over_budget`: materialized diff 超预算。
- `actual_duration > estimated * 2`。
- `ready_width_low`: 多轮 ready queue 宽度为 1，但剩余任务存在独立 conflictKeys。
- `timeout_true_oversize`: timeout classifier 识别为真实粒度过大。

replanner 输入必须包含:

- 当前 ContractPlan。
- 当前 AST/API diff。
- 失败 leaf 的 writeFiles/conflictKeys。
- 真实 build/test 错误。
- 还未执行任务的 WorkPlan。

输出不是简单 child tasks，而是 `DAGPatch`:

```json
{
  "addTasks": [],
  "removeTasks": [],
  "replaceTasks": [],
  "addEdges": [],
  "removeEdges": [],
  "contractChanges": [],
  "reason": "duplicate_symbol"
}
```

禁止策略:

- `provider_latency`、`rate_limit_retry`、`sandbox_cold_start`、`build_test_hang` 不允许直接生成 split DAG。
- `prompt_bloat` 先走 prompt compaction，不拆任务。
- `scheduler_wait` 先修调度器或提高 worker refill，不拆任务。
- `true_oversize` 才能进入 `replaceTasks/addTasks`。

## AgentDB 优化 DAG 示例

当前 fallback 是 7 个任务串行。建议改为:

```text
L0:
  T1 contract-skeleton
    write: go.mod, README.md, contracts.go, errors.go

L1 并发:
  T2 kv-store
    requires: KVStore, Record
    write: kv.go, kv_test.go
    conflictKey: storage:kv

  T3 file-store
    requires: FileStore, FileObject
    write: files.go, files_test.go
    conflictKey: storage:file

  T4 vector-store
    requires: VectorStore, VectorHit
    write: vector.go, vector_test.go
    conflictKey: index:vector

  T5 graph-store
    requires: GraphStore, Node, Edge
    write: graph.go, graph_test.go
    conflictKey: graph:memory

  T6 inverted-index
    requires: InvertedIndex
    write: inverted.go, inverted_test.go
    conflictKey: index:inverted

L2:
  T7 query-router
    requires: KVStore, VectorStore, GraphStore, InvertedIndex
    write: query.go, query_test.go

  T8 persistence-snapshot
    requires: KVStore, FileStore
    write: snapshot.go, snapshot_test.go

L3:
  T9 facade-integration
    write: agentdb.go, agentdb_test.go, example_test.go

L4:
  T10 local-verification
    verifyCommand: cd agentDBV1 && go test ./...
```

这样最大宽度可达到 5，而不是 1。即使每个 leaf 2-4 分钟，总 critical path 也会从 18-25 分钟降到 8-12 分钟。

## 代码改造建议

### P0: 新增 planner contract schema

文件建议:

- `pkg/agent/planner_contract.go`
- `pkg/agent/planner_contract_test.go`

核心类型:

```go
type ContractPlan struct {
    SchemaVersion string
    ProjectRoot   string
    Language      string
    Modules       []ContractModule
    Interfaces    []ContractInterface
    Types         []ContractType
    Edges         []ContractEdge
}

type ContractModule struct {
    ID               string
    Package          string
    Files            []string
    Provides         []string
    Requires         []string
    Invariants       []string
    ConflictKey      string
    ParallelSafe     bool
}
```

### P0: 新增 Leaf Readiness Gate

文件建议:

- `pkg/agent/leaf_readiness.go`
- `pkg/agent/leaf_readiness_test.go`

核心接口:

```go
type LeafReadinessGate struct {
    Contract ContractPlan
    Repo     RepoIndex
    Budget   LeafBudget
}

type LeafBudget struct {
    MaxWriteFiles      int
    MaxPublicSymbols   int
    MaxChangedLOC      int
    MaxPromptChars     int
    MaxCapabilities    int
}

func (g *LeafReadinessGate) Check(tasks []rawTask) (accepted []rawTask, rejected []ReadinessIssue)
```

这一步必须发生在 DAG 入库前。Planner 输出如果不满足 leaf ready 条件，应该走 repair/replan，而不是先执行再等 timeout。

### P0: 扩展 WBS schema 为 WorkPlan

在 `TaskNode/rawTask/wbsJSONTask` 增加:

- `WorkUnitType`
- `ContractRefs`
- `Provides`
- `Requires`
- `ReadFiles`
- `WriteFiles`
- `MayModifySymbols`
- `MustNotModifySymbols`
- `ConflictKeys`
- `ParallelSafety`
- `EstimatedChangedLOC`
- `ArtifactRefs`

兼容策略:

- 旧 `targetFiles` 映射为 `writeFiles`。
- 旧 `parallelGroup` 映射为单个 `conflictKey`。
- 旧 `designRef` 保留，但不再作为唯一 contract 依据。

### P0: 新增 DAG compiler

文件建议:

- `pkg/agent/dag_compiler.go`
- `pkg/agent/dag_compiler_test.go`

核心接口:

```go
type DAGCompiler struct {
    Contract ContractPlan
    Repo     RepoIndex
}

func (c *DAGCompiler) Compile(tasks []rawTask) ([]rawTask, DAGDiagnostics)
```

Compiler 负责:

- 根据 `requires/provides` 自动补边。
- 根据 `writeFiles/conflictKeys/mustNotModifySymbols` 自动补冲突边。
- 计算 width/critical path。
- 输出 `DAGDiagnostics`。

### P0: 改造 objective fallback

删除 `synthesizeObjectiveWBS()` 里的 AgentDB 专用串行链，改为:

- `synthesizeObjectiveContractPlan(objective)` 生成 ContractPlan。
- `synthesizeWorkPlanFromContract(contract)` 生成可并行 WorkPlan。
- 再由 DAG compiler 生成最终 DAG。

特别是 AgentDB 能力必须 fan-out:

- KV/File
- Vector
- Graph
- Inverted Index
- Query Router
- Snapshot/Persistence
- Facade

### P1: 改造 sizing split

`expandGenericRawTask()` 不应固定三段串行。新策略:

- 如果 parent 有多个 `contractRefs` 或多个 `writeFiles`，按 contract/file cluster 拆分并行 children。
- 如果 parent 是单 contract 但多个行为，按 `skeleton -> behavior leafs -> tests -> verification` 拆。
- 如果 parent 只有一个文件但 changed LOC 预计过大，按 symbol 拆。

### P1: timeout split 输出 DAGPatch

把 `addTimeoutSplitChildren()` 从“把 parent 替换为 child chain”改成:

- 先运行 `TimeoutClassifier`。
- 只有 `true_oversize` 才进入 DAGPatch。
- 收集 current API summary。
- 收集 ContractPlan + WorkPlan。
- LLM 输出 `DAGPatch`。
- compiler 校验 patch。
- 只有 patch 合法才应用。

这样能避免 child 继续继承错误接口和粗粒度 targetFiles。

### P1: AST/API contract checker

Go 场景先实现:

- 提取文件中的 type/interface/func/method。
- 检查 `mustNotModifySymbols` 是否被改。
- 检查重复定义。
- 检查 implementation leaf 是否满足 interface。
- build error 中的 duplicate symbol / undefined symbol 转成结构化 `api_drift`。

### P1: 调度器从 batch barrier 改成 worker refill

当前 `Execute()` 是:

```text
read ready batch -> launch N goroutines -> wait all N -> next ready batch
```

建议改为:

```text
while unfinished:
  fill available workers from ready queue
  wait one task completion
  update DAG
  immediately refill one worker
```

这样快任务释放的下游可以立刻执行，不被同批慢任务拖住。

### P2: prompt 改为“规划器不能发明依赖，只能声明 contract”

Planner prompt 应简化:

- 第一次只输出 ContractPlan。
- 第二次基于 ContractPlan 输出 WorkPlan。
- 明确禁止用自然语言 `designRef` 代替 `contractRefs/provides/requires/writeFiles`。
- 要求每个并发 leaf 有不同 `conflictKeys`。
- 要求输出 `parallelismGoal` 和 `expectedDagWidth`。

## 验证计划

### Unit tests

- `TestContractPlanValidateRejectsDuplicateSymbol`
- `TestContractPlanValidateRejectsCycle`
- `TestDAGCompilerAddsRequiresProvidesEdges`
- `TestDAGCompilerSerializesWriteConflictOnly`
- `TestDAGCompilerAllowsSharedReadParallel`
- `TestObjectiveFallbackAgentDBHasWidthAtLeast4`
- `TestGenericSplitFansOutByContractRefs`
- `TestTimeoutDAGPatchCannotModifyLockedContract`
- `TestSchedulerRefillsWorkerOnSingleCompletion`

### Smoke tests

- `/go development 开发 golang ToDo 应用`: 仍保持 4-8 个 leaf，宽度 1-2 即可。
- `/go development 开发 MVCC 事务管理器`: contract skeleton 后核心状态串行微里程碑，但测试和文档可独立。
- `/go development 开发 AgentDB`: contract skeleton 后 KV/File/Vector/Graph/Inverted 并发，最终 facade 和 verification 串行。

### Metrics

新增:

- `planner_contract_count`
- `planner_contract_validation_failed_count`
- `planner_unknown_contract_ref_count`
- `dag_width_target`
- `dag_width_actual`
- `dag_critical_path_minutes`
- `dag_parallelism_efficiency`
- `dag_serial_bottleneck_count`
- `dag_replan_count`
- `dag_patch_apply_count`
- `timeout_classified_count`
- `timeout_true_oversize_count`
- `timeout_provider_latency_count`
- `timeout_rate_limit_retry_count`
- `timeout_prompt_bloat_count`
- `timeout_build_test_hang_count`
- `timeout_sandbox_cold_start_count`
- `timeout_scheduler_wait_count`
- `plan_readiness_reject_count`
- `plan_readiness_repair_count`
- `plan_repair_split_count`
- `leaf_write_files_count`
- `leaf_read_files_count`
- `leaf_changed_loc`
- `leaf_api_drift_count`
- `leaf_contract_violation_count`
- `scheduler_batch_wait_waste_sec`
- `scheduler_ready_width`

验收目标:

- AgentDB 初始 DAG 最大宽度 ≥ 4。
- AgentDB critical path 预计时间 ≤ 总 leaf 预计时间的 55%。
- 单 leaf 实际耗时 P90 < 4 分钟。
- `duplicate_symbol` / `interface drift` 失败率下降。
- `split-on-timeout` 占比下降，更多拆分发生在 planner/compiler 阶段。
- `timeout_split_count / total_leaf_count < 10%`，且每次 split 都必须带 `timeout_kind=true_oversize`。
- `plan_repair_split_count > timeout_split_count`，说明粒度问题被前置拦截。

## 推荐实施顺序

1. 新增 `LeafReadinessGate`，先把不可执行 leaf 拦在 DAG 入库前。
2. 新增 ContractPlan/WorkPlan schema 和 validator，不改执行路径。
3. 改 objective fallback，让 AgentDB 生成 contract-first 并行 DAG。
4. 新增 DAG compiler，以确定性边覆盖 Planner 的 `dependsOn`。
5. 改 generic split，让多 contract/file/capability 任务 fan-out。
6. 新增 `TimeoutClassifier`，禁止所有 timeout 都 split。
7. 改 scheduler 为 worker refill。
8. 接入 AST/API drift checker。
9. 把 timeout split 升级为 `timeout_kind=true_oversize` 下的 DAGPatch。
10. 加 metrics 和 dashboard 面板。

这套顺序先解决“初始 Plan 粒度过大”的根因，再解决 AgentDB 并发宽度低，最后才把动态 DAG 作为异常恢复机制补强。

## 2026-05-09 已落地与验证

本轮根据 AgentDBV4 连续真实团队运行结果，优先落地了几项小步但有效的 DAG compiler 能力:

- 已落地 `contract-first` 边补齐: `pkg/agent/orchestrator.go` 新增 `addContractFirstRawDeps()`，在 DAG 入库前让 implementation/test leaf 自动依赖其读取或引用的 contract leaf。
- 已落地 objective fallback 粒度前移: `pkg/agent/planning_universal.go` 将 storage 粗 leaf 拆成构造/状态与 CRUD 两个 leaf。
- 已落地 verification cwd 统一: local verification 使用 objective target root，避免 E2E 通过但 verification 在父目录误报 `go.mod` 缺失。
- 已落地多类 Go deterministic repair: typed slice to `[]interface{}`、单变量接二返回、测试 `reflect.DeepEqual`、导入包 operator alias、非本地类型方法迁移、本地包 import 漏加、未声明外部依赖 import 清理。

真实运行结论:

- 第 5 轮 LLM planner 曾给出 13 任务、DAG width 5，证明并发潜力存在；失败原因是 `core/store.go` contract leaf 晚于实现/测试，接口后补导致 E2E 大修并引入外部依赖污染。
- 第 6 轮 universal fallback 8 任务串行执行，8/8 成功，E2E 通过，独立 `go test ./...` 通过。
- 因此短期稳定路径是 `budget gate -> universal V1 fallback -> contract-first deps -> deterministic Go repair -> local verification/E2E`。
- 中期仍应实现完整 `ContractPlan -> WorkPlan -> DAGPlan`，让正常 LLM planner 的 13+ 任务也能稳定并发，而不是回退到串行 fallback。

本轮新增测试覆盖:

```text
TestNormalizeAndSplitRawTasksAddsContractFirstDeps
TestSynthesizedObjectiveWBSIsValidUniversalV1Slice
TestApplyGoCompileErrorRepairsRemovesUndeclaredExternalImport
TestApplyGoCompileErrorRepairsUndefinedLocalPackageImport
TestApplyGoCompileErrorRepairsMovesNonLocalTypeMethods
TestApplyGoCompileErrorRepairsTypedSliceToInterfaceSlice
TestApplyGoCompileErrorRepairsSingleAssignmentTwoReturnOutsideLoop
TestApplyGoCompileTextRepairsTestInterfaceDataComparison
TestInferBuildCwdFromTaskScopeForObjectiveUsesRequestedOutputRoot
```

后续优先继续做两件事:

- 将第 5 轮失败的 13 任务计划转化为 contract-aware DAG，而不是 fallback 到 8 任务串行 DAG。
- 在 Planner 输出后增加 `ContractCoverageGate`: implementation/test leaf 如果写入 `storage/search/api` 但没有明确依赖 `core/types/store/errors` contract，应在入 DAG 前自动补边或拒绝计划。
