# 通用 Planner 粒度控制与 DAG 编译正式方案

日期: 2026-05-07  
范围: `claude-go` 研发团队、飞书 Bot `/go development`、nested agent、未来多语言 coding workflow  
实施要求: 不分优先级，一次性调整到位

## 1. Executive Summary

当前 Planner 的核心问题不是“缺少更多提示词规则”，而是缺少一个语言无关、可校验、可编译的计划中间表示。只靠 LLM 输出 WBS，再在 Orchestrator 里用 `estimatedMinutes`、`targetFiles`、`parallelGroup` 和 timeout 做补救，会天然出现两类问题:

- 初始 Plan 过粗: leaf 仍可能包含多个能力、多个接口、多个文件簇、探索工作和模糊验收。
- 运行期误判: timeout 被当成粒度过大信号，但 timeout 也可能来自 provider、限流、prompt 膨胀、测试卡死、sandbox、调度器等。

正式方案是引入一套通用 Planning IR:

```text
Request
  -> Repo/Workspace Index
  -> Capability Plan
  -> Contract Plan
  -> Work Unit Plan
  -> Plan Readiness Gate
  -> DAG Compiler
  -> Execution
  -> Execution Issue Classifier
  -> DAG Patch only when needed
```

关键原则:

- 粒度控制必须发生在初始 Plan 阶段和 DAG 入库前，不能依赖 timeout 后拆分。
- `timeout` 只是运行期症状，必须先分类，只有 `true_oversize` 才允许 split。
- 核心抽象必须语言无关；Go、Python、TypeScript、Rust、Java、C/C++、.NET 只通过 LanguageAdapter 提供索引、符号、构建、测试和契约校验能力。
- AgentDB / Go 只作为示例和验收 smoke case，不应写死在 planner 核心里。

## 2. Design Goals

- 通用性: 支持新项目、已有仓库、单语言、多语言、多服务、多包、多模块。
- 前置粒度控制: Planner 输出后，必须通过 Leaf Definition of Ready，否则不允许进入执行。
- 可并发: DAG 宽度由 capabilities、contracts、read/write sets、conflict keys 推导，不由 LLM 随意串行。
- 可校验: contract、work unit、DAG 都有 schema、validator 和 diagnostics。
- 可恢复: 动态 DAG 用于运行期事实变化，不用于弥补初始 Plan 不合格。
- 可观测: metrics 能解释“为什么拆、为什么串行、为什么超时、是否真的粒度大”。
- 一步到位: 一次合入完整链路，而不是按 P0/P1/P2 分批。

## 3. Non-goals

- 不要求一次支持每门语言的深度 AST contract checker，但核心接口必须能承载。
- 不把 AgentDB、Go、MVCC 写死成通用 planner 规则。
- 不把所有 timeout 转成 split。
- 不让 Planner 直接决定最终 DAG 边；LLM 只声明事实，DAG Compiler 负责确定性边和冲突。

## 4. Core Concepts

### 4.1 Capability

Capability 是用户需求中的独立能力，不等于代码文件，也不等于开发阶段。

示例:

- `kv-store`
- `file-object-store`
- `vector-search`
- `graph-query`
- `auth-token-refresh`
- `api-pagination`
- `frontend-filter-panel`
- `schema-migration`

Capability 用于决定 fan-out，并发宽度应优先来自 capability，而不是来自 coder 数量。

### 4.2 Contract

Contract 是跨 leaf、跨 agent 的稳定边界，语言无关表达为:

- public API / interface / class / function / route / CLI command / data schema
- input/output/error
- invariants
- ownership
- allowed modifications

不同语言的落地不同:

- Go: interface、struct、func/method、package API。
- TypeScript: exported type/interface/function/class、API route。
- Python: class/function/protocol/dataclass/public module API。
- Rust: trait、struct、impl、module pub API。
- Java/Kotlin: interface/class/public method/package boundary。
- C/C++: header declarations、namespace、class、function signatures。
- SQL/data: table schema、migration、query contract。
- Frontend: component props、state contract、route contract。

### 4.3 Work Unit

Work Unit 是最小可执行开发交易，不等于“一个任务标题”。它必须可落盘、可验证、可回滚。

一个 implementation work unit 必须满足:

- 只实现一个 capability 的一个行为簇。
- 默认只写 1 个实现文件 + 1 个测试文件；只读文件不限但要声明。
- 默认只新增或修改 1 个 public contract symbol；普通 implementation 不允许修改 locked contract。
- 估算 changed LOC 默认 50-180，hard max 250。
- 验收命令本地、短时、可 sandbox。
- 不包含 discovery 动词: 分析、调研、找出、设计、决定。
- 不包含多个 capability 名词: KV + Vector + Graph + Index + Router 等。

### 4.4 DAG Edge

DAG 边由确定性规则生成:

- `requires -> provides`: 符号依赖。
- `writeSet conflict`: 写同一文件、同一 symbol、同一 generated artifact。
- `contract lock`: 修改 contract 的 migration 阻塞所有依赖。
- `integration fan-in`: integration 依赖相关 capability terminal units。
- `verification fan-in`: verification 依赖 integration 或全部 terminal units。

LLM 可以建议 `dependsOn`，但不能作为唯一事实源。

## 5. Universal Planning IR

### 5.1 PlanEnvelope

Planner 最终输出统一 envelope:

```json
{
  "schemaVersion": "planner.envelope.v1",
  "languageProfiles": [],
  "repoIndexRef": "artifact://repo-index/current",
  "capabilities": [],
  "contracts": [],
  "workUnits": [],
  "plannerDiagnostics": []
}
```

兼容旧 WBS:

- 旧 `tasks` 可解析为 `workUnits`。
- 旧 `targetFiles` 映射为 `writeFiles`。
- 旧 `targetPackages` 映射为 `verifyScopes`。
- 旧 `parallelGroup` 映射为 `conflictKeys`。
- 旧 `designRef` 只作为说明，不再作为 contract 源。

### 5.2 LanguageProfile

```json
{
  "id": "go-main",
  "language": "go",
  "root": "agentDBV1",
  "packageManager": "go",
  "buildCommands": ["cd agentDBV1 && go test ./..."],
  "testCommands": ["cd agentDBV1 && go test ./..."],
  "lintCommands": [],
  "artifactPatterns": ["**/*.go", "go.mod", "go.sum"]
}
```

多语言项目可以有多个 profile:

```json
[
  {"id": "api", "language": "python", "root": "backend"},
  {"id": "web", "language": "typescript", "root": "frontend"},
  {"id": "infra", "language": "terraform", "root": "infra"}
]
```

### 5.3 CapabilityPlan

```json
{
  "id": "vector-search",
  "title": "Vector search capability",
  "description": "Store embeddings and retrieve top-k nearest items",
  "languageProfile": "go-main",
  "ownerRole": "coder",
  "dependsOnCapabilities": [],
  "integrationPoints": ["agentdb-facade"],
  "risk": "medium",
  "parallelGroupHint": "capability:vector-search"
}
```

### 5.4 ContractPlan

```json
{
  "id": "contract-vector-store",
  "capabilityId": "vector-search",
  "languageProfile": "go-main",
  "kind": "interface",
  "symbol": "VectorStore",
  "file": "agentDBV1/contracts.go",
  "signature": "language-specific signature or normalized text",
  "inputs": ["id", "vector", "metadata"],
  "outputs": ["VectorHit[]", "error"],
  "errors": ["ErrDimensionMismatch", "ErrNotFound"],
  "invariants": ["SearchTopK returns deterministic order for equal scores"],
  "ownedBy": "contract-skeleton",
  "lockedAfter": "contract-skeleton",
  "mayBeModifiedBy": ["contract-migration"]
}
```

### 5.5 WorkUnit

```json
{
  "id": "vector-search-impl",
  "type": "implementation",
  "role": "coder",
  "languageProfile": "go-main",
  "capabilityId": "vector-search",
  "contractRefs": ["contract-vector-store"],
  "provides": ["MemoryVectorStore", "cosineSimilarity"],
  "requires": ["VectorStore", "VectorHit", "ErrDimensionMismatch"],
  "readFiles": ["agentDBV1/contracts.go"],
  "writeFiles": ["agentDBV1/vector.go", "agentDBV1/vector_test.go"],
  "mayModifySymbols": ["MemoryVectorStore", "cosineSimilarity"],
  "mustNotModifySymbols": ["VectorStore", "DB"],
  "conflictKeys": ["capability:vector-search"],
  "parallelSafety": "independent",
  "estimatedMinutes": 2,
  "estimatedChangedLOC": 160,
  "verifyCommands": ["cd agentDBV1 && go test ./..."],
  "acceptanceChecks": [
    "implements declared contract",
    "dimension mismatch test exists",
    "no writes outside writeFiles"
  ],
  "artifactRefs": []
}
```

### 5.6 DAGPlan

`DAGPlan` 不由 LLM 直接输出，而由 compiler 生成:

```json
{
  "nodes": [
    {"id": "contract-skeleton", "workUnitId": "contract-skeleton"},
    {"id": "vector-search-impl", "workUnitId": "vector-search-impl"}
  ],
  "edges": [
    {
      "from": "contract-skeleton",
      "to": "vector-search-impl",
      "reason": "requires:VectorStore"
    }
  ],
  "diagnostics": {
    "maxWidth": 5,
    "criticalPathMinutes": 9,
    "parallelismEfficiency": 2.8,
    "serialBottlenecks": []
  }
}
```

## 6. Leaf Definition of Ready

所有 implementation/test/integration work unit 在执行前必须满足:

| Rule | 通用检查 | 语言 Adapter 辅助 |
|---|---|---|
| `single_capability` | 只绑定一个 capability | capability classifier |
| `bounded_write_set` | 默认 `writeFiles <= 2` | file role classifier |
| `bounded_symbol_set` | 默认 public symbols <= 1 | symbol extractor |
| `bounded_loc` | `estimatedChangedLOC <= 180`, hard max 250 | historical LOC estimator |
| `contract_resolved` | `contractRefs/requires` 可解析 | contract index |
| `no_locked_contract_change` | 不改 locked contract | AST/API diff |
| `local_verification` | verify command 本地可执行 | toolchain adapter |
| `no_discovery` | 不含探索性动词 | prompt/title classifier |
| `prompt_budget` | 单 leaf prompt 预计 chars/token 不超预算 | prompt ledger estimator |
| `sandbox_compatible` | 命令可被 sandbox 限制 | sandbox profile resolver |

不通过时:

- `repairable`: 自动拆分、收窄 write set、补 contract。
- `needs_replan`: 要求 planner 重新输出 capability/work units。
- `blocked`: 缺语言 adapter、缺本地验证命令、缺目标目录等，阻断执行。

## 7. Plan Readiness Gate

`Plan Readiness Gate` 在 DAG 入库前运行，一次性完成:

1. Schema validation。
2. LanguageProfile validation。
3. Contract validation。
4. WorkUnit readiness validation。
5. DAG compile。
6. DAG diagnostics validation。
7. Repair/replan loop。

Gate 不通过不进入 Orchestrator。这样粒度问题在初始 Plan 阶段解决，而不是等 timeout。

### 7.1 Gate 输出

```json
{
  "status": "accepted|repaired|rejected",
  "issues": [
    {
      "code": "leaf.too_many_capabilities",
      "workUnitId": "agentdb-core",
      "message": "work unit mixes kv, vector, graph, inverted-index",
      "suggestedSplit": ["kv-store", "vector-search", "graph-store", "inverted-index"]
    }
  ],
  "compiledDAG": {}
}
```

### 7.2 Repair Loop

Repair prompt 不再让 LLM“重新写计划”，而是给出结构化 issues:

```text
The following work units are not executable leaves.
For each issue, split or rewrite only the invalid work unit.
Do not add implementation details.
Do not add unnecessary serial dependencies.
Return PlanEnvelope JSON.
```

最多 2 轮 repair。仍失败则阻断并返回明确错误。

## 8. Timeout Classification

超时必须先分类:

| timeoutKind | 典型信号 | 处理 | 是否 DAG split |
|---|---|---|---|
| `true_oversize` | output/LOC 接近上限、多 capability、actual > estimated * 2、持续新增大代码 | DAGPatch split | 是 |
| `provider_latency` | 无输出、HTTP timeout、provider 慢 | fallback provider / retry policy | 否 |
| `rate_limit_retry` | 429/backoff 占用耗时 | 降并发 / 切模型 / 排队 | 否 |
| `prompt_bloat` | prompt ledger 超预算 | compaction / artifact refs | 否 |
| `agent_loop` | 多轮对抗反复修同类问题 | 降轮数 / focus patch / human-visible diagnostic | 视情况 |
| `build_test_hang` | LLM 已完成，命令卡住 | sandbox kill / scope tests | 否 |
| `dependency_install` | 安装依赖耗时 | dependency-sync + cache | 否 |
| `sandbox_cold_start` | pull/startup 耗时 | prewarm / cache | 否 |
| `scheduler_wait` | ready queue 有任务但被 batch wait 拖住 | worker refill scheduler | 否 |
| `deadlock_or_zombie` | in_progress 无心跳 | recovery / mark stale | 否 |
| `oom_output_limit` | OOM/output limit | fix code/tests/output | 视情况 |

`split-on-timeout` 只能在 `timeoutKind=true_oversize` 时发生。

## 9. DAG Compiler

DAG Compiler 是确定性组件，输入 PlanEnvelope，输出 DAGPlan。

### 9.1 Edge Rules

- `contract -> implementation`: implementation 依赖其 contract skeleton。
- `provides/requires`: requires symbol 的 producer 必须先完成。
- `write conflict`: 相同 write file/symbol/generated artifact 串行。
- `exclusive conflictKey`: 相同 exclusive conflict key 串行。
- `shared read`: 多个 leaf 共享 readFiles 不串行。
- `test -> implementation`: focused test 可与 implementation 同 leaf，也可单独依赖 implementation。
- `integration -> terminal capabilities`: integration 依赖相关 terminal implementation。
- `verification -> integration or all terminals`: verification 最后运行。

### 9.2 Diagnostics

必须输出:

- `maxWidth`
- `widthByLevel`
- `criticalPathMinutes`
- `totalEstimatedMinutes`
- `parallelismEfficiency`
- `serialBottlenecks`
- `rejectedEdges`
- `addedEdges`
- `removedSuggestedEdges`

### 9.3 DAG Acceptance

通用验收:

- 多 capability 项目 `maxWidth >= min(availableIndependentCapabilities, configuredMaxParallel)`，除非有明确共享状态。
- `parallelismEfficiency >= 1.5`，复杂项目目标 >= 2.0。
- `criticalPathMinutes <= totalEstimatedMinutes * 0.65`，复杂项目目标 <= 0.55。
- 无未知 requires。
- 无 contract cycle。
- 无 work unit 超 readiness budget。

## 10. Language Adapter

核心接口:

```go
type LanguageAdapter interface {
    Name() string
    Detect(ctx context.Context, root string) (*LanguageProfile, bool, error)
    Index(ctx context.Context, profile LanguageProfile) (*RepoIndex, error)
    NormalizeContract(ctx context.Context, c ContractDecl) (ContractDecl, error)
    ValidateContract(ctx context.Context, c ContractDecl, index RepoIndex) []PlanIssue
    ExtractSymbols(ctx context.Context, files []string) ([]SymbolDecl, error)
    ClassifyFiles(files []string) []FileRole
    DefaultVerifyCommands(profile LanguageProfile, scope VerifyScope) []string
    EstimateChangedLOC(unit WorkUnit, index RepoIndex) int
}
```

### 10.1 Adapter Coverage

一次性实现 core + adapters:

| Adapter | Index 能力 | Contract 校验 | Verify 默认 |
|---|---|---|---|
| generic | 文件树、扩展名、README/配置 | 名称/文件/冲突基础校验 | 无，必须 planner 提供 |
| go | package、type、interface、func、method、imports | `go/parser` 签名与重复符号 | `go test ./...` |
| python | module、class、function、import、pyproject/pytest | AST public symbol | `pytest` 或 `python -m pytest` |
| typescript/javascript | exports、interface/type/class/function、package scripts | tsserver/tsc 或 parser 基础校验 | `npm test` / `pnpm test` / `npm run build` |
| rust | crate、mod、pub trait/struct/fn | `cargo check` + syntax | `cargo test` |
| java/kotlin | package、class/interface/public methods | javac/gradle/maven 基础 | `mvn test` / `gradle test` |
| cpp/c | headers、symbols、cmake targets | header parse + compile_commands | `cmake --build` / `ctest` |
| shell/infra | scripts、terraform/k8s/yaml | schema/lint | `shellcheck` / `terraform validate` |

如果语言 adapter 不足，仍可用 generic adapter，但 readiness gate 会要求更保守的 write/LOC budget。

## 11. Planner Prompt Contract

Planner prompt 应改成两段式，而不是一次输出 WBS。

### 11.1 Contract Prompt

要求:

- 输出 `capabilities` 和 `contracts`。
- 不输出 execution tasks。
- 每个 capability 必须有明确 owner、files hint、contract refs、风险和并发安全说明。
- 不确定的需求用 `openQuestions`，不得塞进 coder leaf。

### 11.2 WorkUnit Prompt

输入:

- objective
- RepoIndexSummary
- accepted ContractPlan
- language profiles

要求:

- 输出 WorkUnits。
- 每个 implementation work unit 只能绑定一个 capability。
- 每个 work unit 必须列出 read/write files、requires/provides、verifyCommands、LOC 预算。
- 不允许写 dependsOn 来表达“开发习惯上的顺序”；依赖由 compiler 计算。
- 不允许写“实现所有/完整/整体/核心/基础能力”这类过粗标题。

## 12. Execution Model

### 12.1 Orchestrator 执行输入

Orchestrator 不再直接消费 planner 的 WBS，而是消费 compiled DAG:

```text
CompiledDAGNode {
  id
  workUnit
  dependencies
  conflictKeys
  allowedReadFiles
  allowedWriteFiles
  lockedSymbols
  verifyCommands
  sandboxProfile
}
```

### 12.2 Prompt Assembly

每个 coder prompt 只注入:

- objective 摘要。
- 当前 work unit。
- contract refs 摘要。
- readFiles 摘要或 artifact refs。
- allowed write files。
- must not modify symbols。
- focused acceptance。

不注入完整 plan，不注入无关 capability 正文。

### 12.3 Materialization Gate

落盘后立即检查:

- 是否写出 `writeFiles`。
- 是否越权写文件。
- 是否修改 locked symbols。
- changed LOC 是否超预算。
- verify command 是否通过。

超预算不直接重试大 prompt，而是生成 `execution_issue` 给 classifier。

## 13. Dynamic DAG Policy

动态 DAG 的合法触发:

- contract mismatch discovered by real repo index。
- duplicate symbol。
- missing symbol。
- true oversize。
- new dependency exposed by tests。
- user changes workspace while team running。
- stale/incomplete generated artifact。

动态 DAG 的非法触发:

- 单纯 provider timeout。
- 单纯 429。
- sandbox cold start。
- prompt 太大。
- batch scheduler 等待。
- 缺本地依赖安装。

DAGPatch schema:

```json
{
  "schemaVersion": "planner.dagPatch.v1",
  "reason": "true_oversize|api_drift|duplicate_symbol|missing_contract|workspace_changed",
  "replaceWorkUnits": [],
  "addWorkUnits": [],
  "removeWorkUnits": [],
  "addEdges": [],
  "removeEdges": [],
  "contractChanges": [],
  "diagnostics": []
}
```

Patch 必须通过同一个 Plan Readiness Gate 和 DAG Compiler。

## 14. Implementation Plan

这是一次性实施方案，不拆优先级。可以按提交内的工作包组织，但最终验收必须完整链路可用。

### 14.1 新增 package

建议新增:

```text
pkg/agent/planning/
  schema.go
  language_adapter.go
  adapters_generic.go
  adapters_go.go
  adapters_python.go
  adapters_typescript.go
  adapters_rust.go
  adapters_java.go
  adapters_cpp.go
  repo_index.go
  capability.go
  contract_validator.go
  readiness_gate.go
  dag_compiler.go
  timeout_classifier.go
  prompt_builder.go
  diagnostics.go
  metrics.go
```

如果要减少包迁移成本，可先放在 `pkg/agent` 下，但类型应保持 language-neutral。

### 14.2 Schema Integration

修改:

- `pkg/agent/orchestrator.go`
- `pkg/agent/workflow.go`
- `pkg/agent/orchestrator_wbs_test.go`

要求:

- `TaskNode/rawTask/wbsJSONTask` 兼容旧字段，同时新增 WorkUnit 字段。
- `ParsePlanToDAGWithRepair()` 改为 `ParsePlanEnvelopeToDAGWithRepair()`。
- 旧 WBS 进入 compatibility parser，然后转换为 PlanEnvelope。
- `synthesizeObjectiveWBS()` 替换为 `synthesizeObjectivePlanEnvelope()`。
- Orchestrator nodes 保存 compiled DAG metadata。

### 14.3 Planning Pipeline

修改 `runOrchestratedPhase()`:

```text
planOutput
  -> parse PlanEnvelope or legacy WBS
  -> repo index
  -> contract validation
  -> readiness gate
  -> repair loop
  -> DAG compiler
  -> DAG diagnostics
  -> DAG入库
  -> execute
```

如果 plan 阶段仍输出旧 JSON WBS:

- adapter 转换为 PlanEnvelope。
- readiness gate 发现缺 contracts 时，可生成 minimal inferred contracts。
- 如果无法推断，走 repair prompt。

### 14.4 Prompt Changes

修改 `developmentWorkflow` 的 plan prompt:

- 不再要求直接输出 WBS tasks。
- 要求输出 PlanEnvelope，包含 capabilities/contracts/workUnits。
- 强制说明 leaf ready 条件。
- 明确 timeout 不等于粒度问题。
- 要求 `expectedDagWidth` 和 `parallelismRationale`。

### 14.5 Readiness Gate

实现:

- `CheckCapabilitySingleton`
- `CheckWriteBudget`
- `CheckSymbolBudget`
- `CheckLOCBudget`
- `CheckContractResolved`
- `CheckNoLockedContractChange`
- `CheckLocalVerification`
- `CheckNoDiscoveryInLeaf`
- `CheckPromptBudget`
- `CheckSandboxCompatible`

输出 issues，支持自动 split hints。

### 14.6 DAG Compiler

实现:

- `CompileRequiresProvidesEdges`
- `CompileWriteConflictEdges`
- `CompileContractLockEdges`
- `CompileIntegrationFanInEdges`
- `CompileVerificationFanInEdges`
- `RemoveRedundantSuggestedEdges`
- `ComputeWidth`
- `ComputeCriticalPath`
- `ComputeParallelismEfficiency`
- `ExplainSerialBottlenecks`

### 14.7 Timeout Classifier

修改:

- `executeRunnerBounded()`
- `handleTaskTimeout()`
- `addTimeoutSplitChildren()`

新增 `TimeoutClassifier` 输入:

- LLM call duration / status / retries / HTTP status。
- prompt component ledger。
- sandbox run metrics。
- command duration。
- output bytes。
- materialized files and changed LOC。
- work unit estimatedMinutes / estimatedChangedLOC。
- scheduler ready/active stats。

输出:

```go
type TimeoutClassification struct {
    Kind string
    Confidence float64
    Evidence []string
    Action string
}
```

只有 `Kind == "true_oversize"` 才调用 DAGPatch planner。

### 14.8 Scheduler Refill

修改 `Orchestrator.Execute()`:

当前 batch barrier:

```text
launch batch -> wait all -> next ready
```

改为 worker refill:

```text
for unfinished:
  fill idle worker slots from ready queue
  wait one completion or state event
  update DAG
  refill immediately
```

记录:

- ready queue width。
- active workers。
- wait waste seconds。
- task completion event latency。

### 14.9 Materialization and Contract Gate

修改:

- `MaterializeCode`
- `enforceTargetFileScope`
- `missingTargetFilesError`
- deterministic contract patch 逻辑

新增:

- write set enforcement。
- locked symbol enforcement。
- changed LOC budget。
- adapter-specific symbol diff。
- contract violation report。

### 14.10 Metrics

新增 metrics:

```text
planner_plan_envelope_count
planner_contract_count
planner_work_unit_count
planner_readiness_reject_count
planner_readiness_repair_count
planner_readiness_block_count
planner_capability_count
planner_expected_dag_width
dag_width_actual
dag_width_by_level
dag_critical_path_minutes
dag_total_estimated_minutes
dag_parallelism_efficiency
dag_serial_bottleneck_count
dag_compiler_added_edge_count
dag_compiler_removed_edge_count
dag_patch_count
dag_patch_apply_count
workunit_estimated_changed_loc
workunit_actual_changed_loc
workunit_write_file_count
workunit_read_file_count
workunit_contract_violation_count
timeout_classified_count
timeout_true_oversize_count
timeout_provider_latency_count
timeout_rate_limit_retry_count
timeout_prompt_bloat_count
timeout_build_test_hang_count
timeout_sandbox_cold_start_count
timeout_scheduler_wait_count
scheduler_ready_width
scheduler_active_workers
scheduler_wait_waste_sec
```

### 14.11 Config

新增配置:

```json
{
  "planner": {
    "mode": "contract_dag",
    "legacyWBSCompat": true,
    "maxRepairRounds": 2,
    "leafBudget": {
      "maxWriteFiles": 2,
      "maxPublicSymbols": 1,
      "maxChangedLOC": 180,
      "hardMaxChangedLOC": 250,
      "maxPromptChars": 12000,
      "maxCapabilities": 1
    },
    "dag": {
      "minParallelismEfficiency": 1.5,
      "complexMinParallelismEfficiency": 2.0,
      "maxCriticalPathRatio": 0.65,
      "complexMaxCriticalPathRatio": 0.55
    },
    "timeoutClassifier": {
      "splitOnlyOnTrueOversize": true,
      "minConfidenceForSplit": 0.7
    },
    "languageAdapters": {
      "go": true,
      "python": true,
      "typescript": true,
      "rust": true,
      "java": true,
      "cpp": true,
      "generic": true
    }
  }
}
```

### 14.12 Compatibility

- 默认接受旧 WBS，并转换为 PlanEnvelope。
- 旧字段继续写入 metrics。
- 若旧 WBS 缺少 contracts，则生成 inferred contract 或进入 repair。
- 若 repair 失败，阻断执行并给用户可读报告，不再用粗 objective fallback 直接跑。

## 15. Universal Examples

### 15.1 AgentDB / Go 示例

仅作为引导，不写死:

```text
contract-skeleton
  -> kv-store
  -> file-store
  -> vector-search
  -> graph-store
  -> inverted-index
  -> query-router
  -> snapshot
facade-integration
local-verification
```

最大并发来自 capability fan-out，而不是 timeout split。

### 15.2 Web App / TypeScript 示例

```text
contract-skeleton
  -> api-client-types
  -> state-store
  -> list-component
  -> filter-component
  -> form-component
route-integration
build-and-test
```

并发条件:

- 不同 component 写不同文件。
- shared types 只读。
- route integration 等 component terminal。

### 15.3 Python Data Pipeline 示例

```text
schema-contract
  -> loader
  -> transformer
  -> validator
  -> sink
pipeline-integration
pytest-verification
```

并发条件:

- loader/transformer/validator 若只依赖 schema，可并行实现。
- integration 负责 wiring。

### 15.4 Multi-service 示例

```text
api-contract
  -> backend-handler
  -> frontend-client
  -> docs-openapi
  -> contract-tests
integration
```

并发条件:

- backend/frontend/docs 都依赖 API contract，但互不写同一文件。

## 16. Acceptance Criteria

一次性完成后必须满足:

- Planner 可输出或兼容转换 PlanEnvelope。
- Plan Readiness Gate 能拒绝包含多个 capability 的粗 leaf。
- DAG Compiler 能从 `requires/provides/writeFiles/conflictKeys` 生成边。
- AgentDB case 初始 DAG width >= 4，不依赖 timeout split。
- TypeScript/Python/Rust 至少通过 generic + language profile smoke。
- timeout split 只在 `timeoutKind=true_oversize` 且 confidence 达标时触发。
- provider/rate-limit/prompt/sandbox/test/scheduler timeout 不触发 split。
- Orchestrator scheduler 不再 batch barrier。
- Metrics 能解释每个 leaf 被拆、被串行、被拒绝、被 timeout 的原因。
- 旧 WBS 兼容路径可用，但不允许绕过 readiness gate。

## 17. Test Plan

### 17.1 Unit Tests

```text
TestPlanEnvelopeParseLegacyWBS
TestPlanEnvelopeValidateLanguageProfiles
TestReadinessRejectsMultiCapabilityLeaf
TestReadinessRejectsExplorationLeaf
TestReadinessRejectsTooManyWriteFiles
TestReadinessRejectsTooManyChangedLOC
TestDAGCompilerAddsRequiresProvidesEdges
TestDAGCompilerAllowsSharedReadParallel
TestDAGCompilerSerializesWriteConflict
TestDAGCompilerComputesCriticalPath
TestTimeoutClassifierProviderLatencyNoSplit
TestTimeoutClassifierRateLimitNoSplit
TestTimeoutClassifierPromptBloatNoSplit
TestTimeoutClassifierTrueOversizeSplit
TestSchedulerRefillsAfterOneCompletion
TestContractGateRejectsLockedSymbolModification
```

### 17.2 Smoke Tests

```text
/go development 开发 golang ToDo 应用
/go development 开发 TypeScript Todo Web UI
/go development 开发 Python 数据清洗 CLI
/go development 开发 Rust KV CLI
/go development 开发 AgentDB V1
```

### 17.3 Regression Tests

- 旧 JSON WBS 仍可解析。
- 旧 `targetFiles/parallelGroup` 不绕过 readiness gate。
- objective fallback 不再生成单链 AgentDB。
- timeout 非 oversize 不 split。
- build/test hang 被 sandbox 超时处理。

## 18. Rollout as One Complete Change

虽然不分优先级，但实现可以在一个 feature branch 内按工作包并行开发，最终一次性启用:

1. Planning IR schema。
2. Language adapters。
3. Repo index。
4. Contract validator。
5. Readiness gate。
6. DAG compiler。
7. Prompt rewrite。
8. Orchestrator integration。
9. Timeout classifier。
10. Scheduler refill。
11. Materialization contract gate。
12. Metrics/dashboard。
13. Tests and smoke.

启用开关:

- 默认 `planner.mode=contract_dag`。
- 保留 `legacyWBSCompat=true`。
- 如必须回滚，可设置 `planner.mode=legacy_wbs`，但团队自动研发应提示该模式缺少前置粒度保障。

## 19. Success Metrics

- `timeout_split_count / total_workunit_count < 5%`。
- `timeout_true_oversize_count / timeout_classified_count` 单独可见。
- `plan_readiness_repair_count > timeout_split_count`。
- 多 capability 项目 `dag_parallelism_efficiency >= 2.0`。
- AgentDB 初始 DAG width >= 4。
- leaf P90 actual duration < 4 分钟。
- contract violation 和 duplicate symbol 下降。
- delivered_with_remediation 占比下降。

## 20. Final Position

这套方案的核心不是“把任务拆得更多”，而是“只允许真正 ready 的 leaf 进入执行”。初始 Plan 应该通过 capability、contract、work unit 和 DAG compiler 一次性决定粒度和并发。动态 DAG 是运行期安全阀，不是计划质量的主路径。

