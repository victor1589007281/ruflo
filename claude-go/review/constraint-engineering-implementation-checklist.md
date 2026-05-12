# 编程约束工程实施检查清单

> 对应方案：`programming-constraint-engineering-plan.md`
> 用途：每日站会或周会跟踪实施进度

---

## Phase 1：立即实施（0-2 周）

### 1.1 Go 代码宪法
- [ ] 起草 `pkg/skills/builtin/go-constitution/SKILL.md`
- [ ] 定义 6 条 Hard Rules + 3 条 Coordination Rules
- [ ] 修改 `executor.go` `buildGenerationPrompt()` 注入宪法前缀
- [ ] 修改 `roles.go` Planner 角色提示检查宪法合规
- [ ] 实现 `ConstitutionComplianceCheck`（regex/AST 扫描 `crypto/md5`、裸 map 读写、`context.Background()`）
- [ ] 验收：100% 发现测试样本中的违规项

### 1.2 六维质量门 2.0 — Security Gate
- [ ] 新建 `pkg/toolskill/security_scan.go`
- [ ] 集成 `gosec`（优先）或 regex 回退扫描
- [ ] 新建 `pkg/toolskill/secret_scan.go`（AWS key、GitHub token、private key 模式）
- [ ] 修改 `validation_gate.go` 插入 `security` stage（Hard Gate）
- [ ] 安全失败不可 auto-fix，必须显式 remediation patch
- [ ] 验收：Security Gate 触发率可度量，失败任务不进入 review

### 1.3 六维质量门 2.0 — Concurrency Gate
- [ ] 修改 `validation_gate.go` 插入 `concurrency` stage（Hard Gate）
- [ ] `go test -race ./...` 集成到 gate
- [ ] `go vet -lostcancel` 集成到 gate
- [ ] ContextEngine 新增并发诊断：`context_engine.go` + `concurrency_diagnosis.go`
- [ ] race 诊断建议：Mutex / sync.Map / atomic / channel 选择逻辑
- [ ] 验收：并发代码必须经过 race detector

### 1.4 六维质量门 2.0 — Performance Gate（Soft）
- [ ] 新建 `pkg/toolskill/benchmark_gate.go`
- [ ] 维护 `benchmark_baseline.json`
- [ ] 集成 `benchstat` 比较逻辑
- [ ] `validation_gate.go` 插入 `performance` stage（Soft，标记但不阻塞）
- [ ] 回归 > 20% 降级为 `DeliveredWithRemediation`
- [ ] 验收：Dashboard 可见性能回归率

### 1.5 CriticGPT 式对抗评审
- [ ] 扩展 `adversarial.go` `EvalScore` → `MultiDimEvalScore`（5 维度）
- [ ] 新建 `pkg/skills/builtin/security-review/SKILL.md`
- [ ] 新建 `pkg/skills/builtin/concurrency-review/SKILL.md`
- [ ] `roles.go` 新增 `security-reviewer`、`concurrency-reviewer`、`idiomatic-reviewer`
- [ ] 验收：评审输出包含 Security + Concurrency 维度

### 1.6 TDD 多 Agent 循环
- [ ] `roles.go` 新增 `test-designer` 角色
- [ ] `orchestrator.go` `normalizeAndSplitRawTasks()` 自动注入 `test_design` 前置依赖
- [ ] `executor.go` 执行前检查测试是否编译通过
- [ ] 验收：所有 implementation 叶子有对应测试叶子

### 1.7 全局编译/测试门
- [ ] `teams.go` `executeWorkflow` 结束前插入 Global Compile Gate
- [ ] `teams.go` `executeWorkflow` 结束前插入 Global Test Gate（含 `-race`）
- [ ] 验收：单叶通过但全局编译失败时，workflow 标记 Failed

---

## Phase 2：短期优化（2-6 周）

### 2.1 契约锁与版本化
- [ ] `contract/types.go` 新增 `ContractVersion`、`Locked` 字段
- [ ] `contract_store.go` 新增 `LockInterface` / `UnlockInterface`
- [ ] `contract_store.go` 新增 tree-sitter-go 桥接（`contract/tree_parser.go`）
- [ ] `contract_store.go` 全局一致性扫描 `GlobalConsistencyScan()`
- [ ] `orchestrator.go` 处理 Locked 契约的串行化
- [ ] `orchestrator.go` Breaking Change 自动注入 migration 任务
- [ ] 验收：两个 Agent 无法同时修改同一接口

### 2.2 Go 技能库（首批 10 个）
- [ ] 新建 `pkg/skills/runtime/go/` 目录
- [ ] 编写 10 个技能模板（HTTP server、worker pool、rate limiter、error wrap、JSON API、DB transaction、mock、table-driven test、sync.Map vs RWMutex、channel pipeline）
- [ ] 新建 `pkg/skills/retrieval.go`（关键词 + embedding 检索）
- [ ] `executor.go` `buildGenerationPrompt()` 注入技能上下文
- [ ] `context_engine.go` 错误诊断时注入修复技能
- [ ] 验收：任务关键词匹配准确率 > 70%

### 2.3 Self-Refine 内循环
- [ ] `executor.go` 新增 `SelfRefine(diags []Diagnostic) error`
- [ ] 在提交给 Validator 前，Executor 内部尝试 1 轮自修复
- [ ] 验收：平均修复轮数降低 20%

### 2.4 属性测试生成
- [ ] 新建 `pkg/toolskill/property_test.go`
- [ ] 对纯函数自动生成 `testing/quick` 或 `pgregory/rapid` 属性测试
- [ ] 属性示例：round-trip、排序不变性、长度守恒
- [ ] 验收：纯函数覆盖率提升 15%

### 2.5 约束效果遥测
- [ ] 新建 `pkg/metrics/constraint_metrics.go`
- [ ] `validation_gate.go` 采集各 stage pass/fail
- [ ] `adversarial.go` 采集各维度评分
- [ ] `patch_apply.go` 采集冲突类型
- [ ] `teams.go` 采集交付状态分布
- [ ] `executor.go` 采集修复轮数
- [ ] Grafana dashboard 新增 panels
- [ ] 告警规则：Security Gate 触发率 > 5%（P1）、Race 触发率 > 10%（P1）
- [ ] 验收：Dashboard 实时可见约束指标

### 2.6 A/B 测试框架（Skill 效果）
- [ ] `orchestrator.go` 任务分配时基于 task hash 随机分组
- [ ] 对比组：不注入 `simplicity-first`；实验组：注入
- [ ] 比较指标：平均生成行数、平均修复轮数、通过率
- [ ] 验收：可量化 Skill 效果

---

## Phase 3：中期演进（6-12 周）

### 3.1 CRDT 并行编辑
- [ ] 新建 `pkg/merge/crdt.go`（Yjs/Eg-walker 简化实现）
- [ ] `patch_apply.go` 新增 `ApplyCRDT()`
- [ ] 编辑先写入内存 CRDT 文档，再合并落盘
- [ ] 合并后 `validateSyntax()` + 新增 `validateImports()`
- [ ] 配置开关：保守回退模式（关闭 CRDT 时回到串行）
- [ ] 验收：并行编辑不同区域 0 冲突，语法 100% 通过

### 3.2 语法感知语义合并
- [ ] 新建 `pkg/merge/mergiraf.go`（调用 mergiraf CLI 或移植算法）
- [ ] tree-sitter-go + GumTree 模糊匹配
- [ ] `patch_apply.go` `ApplyMergiraf(base, left, right)`
- [ ] 验收：消除 80% "移动+编辑"假冲突

### 3.3 冲突键智能优化
- [ ] 新建 `pkg/agent/conflict_analyzer.go`
- [ ] Read-Only 共享任务去除串行边
- [ ] 符号级依赖替代文件级依赖
- [ ] `orchestrator.go` `rawTasksToDAG` 替换粗粒度逻辑
- [ ] 验收：Read-Only 并行率提升 50%

### 3.4 性能回归门硬化
- [ ] `benchmark_gate.go` 完善 benchstat 统计比较
- [ ] 阈值可配置（默认 ns/op +20%，allocs/op +20%）
- [ ] 回归触发时自动生成优化子任务
- [ ] 验收：P95 回归自动标记 `DeliveredWithRemediation`

### 3.5 Prompt 自动优化
- [ ] 新建 `pkg/prompt/optimizer.go`
- [ ] 收集历史任务（prompt → 结果 → 评分）作为训练集
- [ ] 实现简单贝叶斯优化或 TextGrad 风格梯度传播
- [ ] 验证集：SWE-bench 子集或内部回归集
- [ ] 验收：Planner/Executor prompt 在验证集 pass@1 提升 10%

### 3.6 本地模型安全微调（可选）
- [ ] 收集 SafeCoder / Secure-Instruct 数据集
- [ ] 使用 QLoRA 微调本地 7B 模型（如 Qwen-Coder-7B）
- [ ] 评估：安全漏洞率降低 30%
- [ ] 验收：微调模型作为 Executor 可选后端

---

## 验收总标准

- [ ] 安全漏洞在 AI 生成代码中的引入率 < 2%（由 gosec + secret scan 度量）
- [ ] Race condition 在 AI 生成代码中的引入率 < 3%（由 `go test -race` 度量）
- [ ] 平均修复轮数 < 2.0（由遥测系统度量）
- [ ] 并行 Agent 编辑冲突率 < 10%（由 merge 系统度量）
- [ ] 全局编译/测试通过率 > 95%（由 teams.go 门控度量）
- [ ] 代码交付状态为 `Completed`（无 remediation）的比例 > 70%
