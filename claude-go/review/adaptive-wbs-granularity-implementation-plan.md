# 动态 WBS 粒度预算实现记录

## 目标

本次改造把 `/go development` 的 WBS 从固定任务数量约束，改为 `Macro Task + Leaf Task + Verification Task` 的动态粒度控制。核心目标不是“任务越少越好”或“任务越细越好”，而是让每个可执行 Leaf 都满足时间、文件、验证和阻塞预算，避免 MVCC、事务、锁、调度、索引等核心模块被单个 coder agent 执行到 6 分钟超时。

## 已实现代码调整

- Planner prompt 已支持 `taskType`、`parentId`、`estimatedMinutes`、`riskLevel`、`verifyCommand`、`parallelGroup`、`blockingPolicy`、`splitReason`，并移除了“普通应用固定 4-8、禁止超过 10”的绝对规则。
- WBS parser 和 `TaskNode/rawTask/wbsJSONTask` 已兼容新字段，同时保留旧 JSON、Markdown 表格、编号列表的默认 leaf 兼容路径。
- Orchestrator 在 WBS 写入 DAG 前执行 `TaskSizingGate`。Macro、高风险 Leaf、预计超过 4 分钟、目标文件超过 3 个、命中 MVCC/事务/锁/并发/索引等风险关键词的任务，会被自动拆成可验证 Leaf DAG。
- MVCC 类任务会被确定性拆成 6 个微里程碑: 数据结构与事务状态、生命周期、ReadView/可见性、写写冲突、版本链/GC、集成验证。
- 同一 `parallelGroup` 或共享 `targetFiles` 的任务会自动串行化，避免多个 agent 并发写同一核心状态或同一文件。
- `agent execution timeout after 6m0s` 已变成 typed timeout。超时后触发 `split-on-timeout`，优先调用 LLM Split Planner 基于 timeout summary 生成严格 JSON 子 WBS；校验通过后在线把超时 Leaf 替换为子 Leaf DAG，并把原下游依赖改挂到最后一个子 Leaf 上，不再原样递归重试同一个过粗 prompt。
- LLM Split Planner 失败、超时或输出不合法时，自动退回确定性拆分模板。MVCC 仍有 6 段兜底拆分，普通复杂任务有 3 段兜底拆分。
- 对抗循环结束后，如果 review/test hard gate 未通过且 `blockingPolicy=fail_blocks_dependents`，Leaf 会标记 failed 并级联阻塞下游，不再以 completed-with-warning 继续污染后续任务。
- `verification` task 继续走本地 build/test/TODO scan，不调用 tester LLM。

## 新增 Metrics

新增以下 team 级指标，写入 `.claude-go/metrics/*.jsonl`，用于解释 WBS 粒度与超时来源:

- `wbs_task_estimated_minutes`
- `wbs_task_actual_duration_sec`
- `wbs_split_count`
- `wbs_timeout_split_count`
- `wbs_leaf_files`
- `wbs_parallel_group_size`
- `wbs_failed_blocked_dependents`

建议后续压测时重点对比 `wbs_split_count`、`wbs_timeout_split_count` 与 `wbs_task_actual_duration_sec`。如果 MVCC 场景仍出现 6 分钟 timeout，说明 Planner 或 sizing gate 没有提前把高风险任务拆成足够小的 Leaf。

运行时 split 指标会额外带 `split_source` 标签:

- `llm-split-planner`: timeout 后由 planner LLM 动态生成子 WBS。
- `deterministic-fallback`: LLM planner 不可用、超时、解析失败或输出不合法时使用模板兜底。

## 运行策略

普通 CLI、小型 CRUD、配置类任务仍可以保持少量 Leaf 快速闭环。复杂核心模块则按风险动态增加 Leaf，并默认串行核心状态微里程碑。这样牺牲一部分并发峰值，换取更低的单 agent 超时率、更清晰的失败定位，以及更小的回滚范围。

## LLM Split Planner 设计

触发条件:

- 单个 coder/reviewer 阶段中的 coder 调用达到 `coderCallTimeout=6m`。
- 该 timeout 不再进入普通 transient retry，因为原样重试大概率继续超时并放大 token 消耗。

执行流程:

1. Orchestrator 生成 timeout summary，包含目标、超时任务标题、role、taskType、estimatedMinutes、riskLevel、acceptance、constraints、targetFiles、targetPackages、最近 test/result 摘要。
2. 调用 `planner` 角色，限定 `splitPlannerCallTimeout=2m`，要求只输出严格 JSON WBS。
3. 校验输出: 禁止 macro；最多 12 个 task；每个 Leaf 2-4 分钟；dependsOn 只能引用本次输出中更早的 id；默认 `blockingPolicy=fail_blocks_dependents`。
4. 自动修正预算: 超过 4 分钟的 estimatedMinutes 会被 clamp 到 4；缺失 targetFiles/targetPackages 时继承父任务；缺失 verification 时自动追加本地 verification task。
5. 在线热插入 DAG: 子任务依赖超时父任务或前置子任务；同一 `parallelGroup`/共享文件继续串行；原下游依赖改挂到最后一个子 task。
6. 父任务标记为 completed macro，占位说明已拆分；真正执行由新 Leaf DAG 继续推进。

失败兜底:

- planner 创建失败、LLM 超时、JSON 解析失败、包含 macro、依赖引用未来/未知 id 时，不阻塞流程，直接退回确定性模板。
- 兜底模板仍保留 MVCC 专用 6 段拆分和通用 3 段拆分，保证当前运行不中断。

## 待观察点

- LLM Split Planner 已落地，但真实供应商 429/timeout 时会走 `deterministic-fallback`。压测时需要统计 `split_source` 比例，判断 planner LLM 的可用性和收益。
- `verifyCommand` 字段已进入 schema 和 prompt，目前 verification task 仍复用本地语言级 build/test 入口，避免直接执行 planner 生成的任意 shell。
- Prometheus bridge 对新 WBS 指标可后续补 dashboard 专门面板；JSONL 与 catalog 已具备审计能力。

## 验证计划

- WBS parser 测试覆盖新字段并兼容旧字段。
- SizingGate 测试覆盖 MVCC macro 拆分、高风险超预算 leaf 拆分。
- Concurrency 测试覆盖共享 `targetFiles` 自动串行化。
- Timeout 测试覆盖 typed timeout，确保不再被普通 transient retry 混淆。
- LLM Split Planner 测试覆盖: planner JSON 成功时走 `llm-split-planner`，并把下游依赖改挂到 LLM verification child；planner 不可用时走 `deterministic-fallback`。
- Smoke 场景建议继续使用 `/go development 开发 MVCC 事务管理器`，观察是否不再出现单任务 `agent execution timeout after 6m0s`。

## 本次验证结果

- `go test ./pkg/agent -run 'Test(ParseWBSFromJSONNewFields|NormalizeAndSplitRawTasks|RawTasksToDAGSerializesSharedTargetFiles|ExecuteRunnerBoundedReturnsTypedTimeout|HandleTaskTimeout)'` 通过，包含 LLM Split Planner 成功路径和 deterministic fallback 路径。
- `go test ./pkg/metrics ./pkg/agent ./pkg/tool/builtin ./pkg/api ./pkg/mcp ./pkg/engine ./pkg/skills ./cmd/claude-go ./pkg/feishu ./pkg/dashboard ./pkg/observability` 通过。
- `go build -o /tmp/claude-go-wbs ./cmd/claude-go` 通过。
- `git diff --check` 通过。
- `go test ./... -run '^$'` 仍受既有 `tests/unit/mcp_test.go` 影响，报 `feishu.AISection` 不存在旧字段 `Model/APIKey/BaseURL/MaxTokens` 等错误；该问题与本次 WBS 改造无关。
