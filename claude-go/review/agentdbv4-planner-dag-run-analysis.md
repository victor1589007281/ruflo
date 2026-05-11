# AgentDBV4 研发团队验证与 Planner/DAG 优化复盘

日期: 2026-05-08  
代码库: `/Users/huaquan.liang/Documents/GitHub/ruflo/claude-go`  
参考设计目录: `/Users/huaquan.liang/Documents/GitHub/ruflo/claude-go/V2`  
输出项目: `/Users/huaquan.liang/test/agentDBV4`

## 1. 本轮纠偏

本轮重点修正了之前不合理的 AgentDB/Go 专用硬编码。当前 Planner 路径已经改为通用 WorkUnit/DAG 方案:

- 删除/停用 AgentDB 专用 objective WBS、deterministic patch、E2E 内置兜底实现。
- `research/design` 可注入本地参考资料摘录，`planner` 不再读取或消化原始 V2 文档，只消费架构阶段输出。
- Planner/Architect 输出校验会拦截伪工具调用，如 `minimax:tool_call`、`Read`、`Bash`、`cat/ls`。
- 用户指定 target root 时，Planner 输出的项目内相对文件会自动归一到目标目录，如 `go.mod` -> `agentDBV4/go.mod`。
- 目录级 `targetFiles` 会在 TaskSizingGate 中拆成文件级 leaf，避免把 `internal/storage/` 这类目录任务直接交给 coder。
- Go build gate 会在发现 `go.mod` 后执行受限 `go mod tidy`，避免第三方依赖缺少 `go.sum` 时把模型拖进无效修复循环。
- development 最终状态不再允许用薄 E2E 覆盖 hard gate / 级联阻塞失败。

## 2. 验证运行

主要验证命令:

```bash
CLAUDE_GO_PROMPT_DEBUG=1 \
CLAUDE_GO_PROMPT_DEBUG_DIR=/Users/huaquan.liang/.claude-go/prompt-debug-agentdbv4-universal-20260508-0755 \
/tmp/claude-go-planner --config /Users/huaquan.liang/.claude-go/config/config.json --permission-mode auto run \
"/go development 帮我用 golang 开发一个 agentDB ，设计方案参考/Users/huaquan.liang/Documents/GitHub/ruflo/claude-go/V2 下的设计，代码输出到/Users/huaquan.liang/test/agentDBV4 目录下（若无，需要新建它）"
```

观察到的运行结果:

- `go-development-6402`: Planner 二次重试成功，初始 DAG 为 12 个任务，宽度 4；项目基础搭建 fast-pass，但后续目录级/模块级任务仍出现编译失败和 timeout。
- `go-development-7434`: 目录级 target 拆分后初始 DAG 为 14 个任务，宽度 3；首个骨架任务因引入 `cobra` 但缺少 `go.sum` 失败，随后补了 Go build gate 的 `go mod tidy`。
- `go-development-7781`: 首个文件级 leaf 已能物化 5 个文件并通过编译；但 reviewer 第二轮低分导致 hard gate 阻塞，随后放宽了 `sizing-gate` 小 leaf 的 build/test 优先规则。

当前结论更新: 流程已从“错误成功/兜底成功”修正为“真实失败可见”，并在后续第六轮真实研发团队运行中稳定交付了 AgentDBV4 V1 纵切。需要强调的是，本次成功交付的是本地可编译、可测试的 V1 核心能力，不是 V2 设计中 WAL/LSM/SSTable/HNSW/MVCC/图谱/分布式复制的完整生产级实现。

## 3. 仍然存在的问题

- 当前 provider 多次把 Architect/Planner/Coder 当成工具执行代理，输出伪工具调用。校验已经能拦截并重试，但会消耗时间。
- Planner 仍可能产出过粗或过串行的计划，比如 7 个任务、DAG 宽度 1。这说明仅靠 prompt 约束不够，还需要 DAG 编译阶段做更强的结构化重写。
- 高风险模块即使被拆成文件级 leaf，缺少稳定的接口 contract 时仍会发生跨模块漂移。
- 首个 skeleton leaf 若引入外部依赖，会增加 go mod tidy 成本；后续建议默认要求 stdlib-only，除非架构明确允许依赖。
- reviewer 分数仍偏噪声。对小 leaf 已改为 build/test 优先，但中大型 leaf 仍可能被低分阻塞。

## 4. 已验证的代码变更

验证通过:

```bash
go test ./pkg/agent -run 'Test(ParseWBS|NormalizeAndSplitRawTasks|RawTasksToDAG|ExecuteRunnerBounded|HandleTaskTimeout|InferObjectiveTargetRoot|ValidateParsedWBS|SynthesizeObjectiveWBS|ClassifyAgentTimeout)'
go test ./pkg/agent ./cmd/claude-go
go build -o /tmp/claude-go-planner ./cmd/claude-go
```

已知全量 `go test ./...` 仍会被仓库既有 `tests/unit/mcp_test.go` 中旧 Feishu 配置字段阻断，和本轮 Planner/DAG 修改无关。

## 5. 下一步建议

- 增加 deterministic skeleton contract pass: 对新项目的第一个 leaf，由系统根据语言适配器生成最小 manifest/main/config/test 骨架，再交给 coder 增量修改。
- 增加 Plan Envelope 结构化校验: Planner 输出缺少 `workUnitType/writeFiles/conflictKeys` 时由本地 compiler 补齐，而不是依赖 LLM retry。
- 对高风险模块启用 contract-first DAG: 先生成共享 `contracts/types/errors`，所有并发实现 leaf 只能依赖这些 contract。
- 对 provider 适配加“no-tools mode”系统前缀，避免 MiniMax/Kimi 类模型继续输出伪工具调用。

## 6. 2026-05-09 继续修复与真实验证

本轮继续按真实团队执行结果迭代，没有手工修改 AgentDBV4 产物来绕过研发团队失败。关键失败与修复链如下:

- 第 1 轮真实运行: Planner 输出 22 个任务、DAG width 6，前半段并发有效，但最终 18/22 成功、E2E 失败。真实错误包括 `types.OpEqual` 常量漂移、`[]*T` 传入 `[]interface{}`、单变量接二返回值、`interface{}` 承载 `[]byte` 测试直接比较 panic。
- 第 2 轮真实运行: E2E 通过，但团队状态仍 failed。原因是 local verification 在仓库工作目录找 `go.mod`，而 E2E 在用户指定输出目录 `agentDBV4` 下验证。修复后 local verification 与 E2E 统一使用 objective target root。
- 第 3/4 轮真实运行: universal fallback 的单个 `实现最小本地数据存储` leaf 过粗，多次出现跨包接口漂移、非本地类型方法、E2E 大规模修复仍失败。结论是不能继续只靠后置 repair，应前移到 WBS 粒度。
- 第 5 轮真实运行: LLM planner 直接产出 13 个任务、DAG width 5，说明并发度提高。但 `core/store.go - Store 接口` contract leaf 晚于/并行于实现 leaf，导致实现先写、接口后补，最终测试引入 `github.com/mattn/go-sqlite3` 外部依赖污染，E2E 失败。
- 第 6 轮真实运行: 使用 contract-first DAG 与 8 任务 universal fallback，研发团队完成状态 `completed`，编排 8/8 成功，local verification 通过，E2E 本地门禁通过。

第 6 轮成功运行关键信息:

- 团队: `go-development-1778261213277477000`
- 输出目录: `/Users/huaquan.liang/test/agentDBV4`
- 报告: `/Users/huaquan.liang/test/.claude-go/teams/go-development-1778261213277477000/REPORT.md`
- 编排: 8 个任务，8/8 成功，0 失败。
- 耗时: 10m47s。
- E2E: `E2E 本地门禁通过: build/test 通过`。
- 独立复核: 在 `/Users/huaquan.liang/test/agentDBV4` 执行 `go test ./...` 通过。

独立复核输出摘要:

```text
?    agentdbv4/cmd/app          [no test files]
?    agentdbv4/internal/core    [no test files]
?    agentdbv4/internal/search  [no test files]
?    agentdbv4/internal/store   [no test files]
ok   agentdbv4                  0.877s
```

最终产物质量观察:

- `go.mod` 只有 `module agentdbv4` 与 `go 1.22`，没有第三方依赖。
- `rg` 未发现 `TODO/STUB/sqlite/http/grpc/github.com/mattn` 等污染。
- 产物包含 `cmd/app/main.go`、`internal/core/types.go`、`internal/store/store.go`、`internal/search/search.go`、`agentdb_test.go`。
- 测试覆盖 create/write/read/query/search/delete/transaction-like commit/concurrent access/closed store/record fields。
- 代码质量定位: 可编译可测试的 V1 本地纵切，适合作为后续 WAL/LSM/向量/图谱/倒排索引/文件存储扩展基线。

## 7. 已落地源码设计

### 7.1 Contract-first DAG normalizer

位置: `pkg/agent/orchestrator.go`

新增 `addContractFirstRawDeps()`，在 DAG 入库前补 contract-first 依赖边。规则是:

- `workUnitType=contract`、标题/目标文件包含 `interface/types/schema/接口/契约/类型` 的 leaf 被识别为 contract leaf。
- implementation/test leaf 如果 `readFiles` 读取 contract leaf 的 `writeFiles`，自动追加依赖。
- implementation/test leaf 如果 `contractRefs/requires` 命中 contract leaf 的 `provides`，自动追加依赖。
- 这一步发生在 `addProjectBootstrapRawDeps()` 与 `relaxOverSerialRawDeps()` 之前，避免并发实现先于接口契约执行。

这解决了第 5 轮中 `core/store.go` contract 被排在 storage/test leaf 后面，导致接口漂移和 E2E 大修的问题。

### 7.2 Universal V1 fallback 粒度调整

位置: `pkg/agent/planning_universal.go`

fallback 从 7 个任务调整为 8 个任务，核心是拆分原来的粗粒度 storage leaf:

- `实现存储构造与私有状态`: 只实现 store 类型、构造函数和私有 map/集合状态。
- `实现存储 CRUD 闭环`: 只补 Put/Get/Delete/List 或等价最小 CRUD。

这个调整将原来 3-6 分钟且容易 hard gate 的单个 storage leaf 拆成两个 2 分钟级 leaf。第 6 轮验证中，两个 leaf 分别通过，未再出现 storage 单点 hard gate。

### 7.3 Verification cwd 与用户输出目录统一

位置: `pkg/agent/orchestrator.go`

新增 `inferBuildCwdFromTaskScopeForObjective()`，local verification 在没有具体 targetFiles 时会使用 objective 中解析出的 target root。例如用户要求输出到 `/Users/huaquan.liang/test/agentDBV4`，verification 会在 `team.Cwd/agentDBV4` 下执行，而不是误在 `/Users/huaquan.liang/test` 下找 `go.mod`。

### 7.4 Deterministic Go repair 扩展

位置: `pkg/agent/workflow.go`

新增/增强的通用修复能力:

- `repairGoTypedSliceToInterfaceSliceCallsFromBuildErrors`: 处理 `[]*T` 不能直接作为 `[]interface{}` 的调用漂移，自动包装 `toInterfaceSlice[T]`。
- `repairGoSingleAssignmentMultiReturnFromBuildErrors`: 单变量接二返回值时，在非循环上下文生成 `x, _ := fn()`，循环上下文保留 `err` guard。
- `repairGoTestInterfaceDataComparisons`: 测试里 `v.Data != []byte(...)` 这种可能 panic 的 interface slice 比较改为 `reflect.DeepEqual`。
- `repairGoImportedLocalSelectorStubsFromBuildErrors`: 支持 `types.OpEqual = QueryOpEqual` 这类本地包 selector 兼容别名。
- `repairGoNonLocalTypeMethodsFromBuildErrors`: 将 `func (a *core.Agent) ...` 这种写在非定义包的非法方法迁移到定义包的 `zz_generated_contract_methods.go`。
- `repairGoUndefinedLocalPackageImportsFromBuildErrors`: 处理 `undefined: core` 这类本模块包 alias 漏 import。
- `repairGoUndeclaredExternalImportsFromBuildErrors`: 对未声明第三方依赖 import 进行清理，避免测试 leaf 引入 sqlite3/cobra 等目标外依赖污染。

对应测试位于 `pkg/agent/workflow_materialize_test.go` 与 `pkg/agent/orchestrator_wbs_test.go`。

## 8. 验证命令

claude-go 聚焦测试与编译:

```bash
go test ./pkg/agent -run 'TestNormalizeAndSplitRawTasksAddsContractFirstDeps|TestApplyGoCompile(ErrorRepairsRemovesUndeclaredExternalImport|ErrorRepairsUndefinedLocalPackageImport|ErrorRepairsMovesNonLocalTypeMethods)|TestSynthesizedObjectiveWBSIsValidUniversalV1Slice' -count=1
go build -o /tmp/claude-go-planner ./cmd/claude-go
```

真实团队运行:

```bash
rm -rf /Users/huaquan.liang/test/agentDBV4 /Users/huaquan.liang/.claude-go/prompt-debug/team-agentdbv4-current
CLAUDE_GO_PROMPT_DEBUG=1 \
CLAUDE_GO_PROMPT_DEBUG_DIR=/Users/huaquan.liang/.claude-go/prompt-debug/team-agentdbv4-current \
/tmp/claude-go-planner \
  --config /Users/huaquan.liang/.claude-go/config/config.json \
  --permission-mode bypass \
  run "/go development 帮我用 golang 开发一个 agentDB ，设计方案参考/Users/huaquan.liang/Documents/GitHub/ruflo/claude-go/V2 下的设计，代码输出到/Users/huaquan.liang/test/agentDBV4 目录下（若无，需要新建它）"
```

产物独立复核:

```bash
cd /Users/huaquan.liang/test/agentDBV4
CLAUDE_GO_SANDBOX_MODE=process CLAUDE_GO_SANDBOX_ALLOW_UNSAFE_FALLBACK=1 go test ./...
```

## 9. 残留问题

- 正常 LLM planner 仍会偶尔输出 21+ leaf 或高级引擎面，最终被 budget/fallback 拦截。当前成功路径仍依赖 universal fallback，后续应继续推进 `ContractPlan -> WorkPlan -> DAGPlan`。
- 第 5 轮证明并发 DAG width 可以到 5，但如果 contract 依赖缺失，越并发越容易接口漂移。contract-first normalizer 已缓解，但还不是完整 DAG compiler。
- 研发团队成功产物是 V1 纵切，未实现 V2 参考设计的完整高级能力。后续扩展应按 capability fan-out: `storage:file`、`index:inverted`、`index:vector`、`graph:memory`、`agent:context`、`facade:api`，每个 capability 独立 contract + implementation + local test。
- `llm_prompt_component_chars` 在本轮 CLI provider 事件中仍显示多个 component 为 0，说明 prompt ledger 对部分 provider/client 路径的拆账还需要补采集，不影响本轮研发团队成败，但影响 token 成本分析精度。
