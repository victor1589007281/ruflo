# Ruflo LLM 质量把控分析

## 概述

Ruflo（原名 Claude Flow v3.5）是一个基于 Claude Code 构建的多智能体 AI 编排框架。其对 LLM 输出的质量控制贯穿全链路，从任务路由、执行监控、代码评审到模式学习形成闭环。以下是核心机制分析。

---

## 1. 分层任务路由（3-Tier Model Routing, ADR-026）

最基础的质量控制手段——将不同复杂度的任务路由到合适的 LLM 层级，避免低质模型处理高复杂度任务，也避免高成本模型处理简单操作。

| 层级 | 处理器 | 延迟 | 成本 | 适用场景 |
|------|--------|------|------|----------|
| Tier 1 | Agent Booster (WASM) | <1ms | $0 | 简单变换（var→const、加类型、加错误处理），完全跳过 LLM |
| Tier 2 | Haiku | ~500ms | $0.0002 | 简单任务，复杂度 <30% |
| Tier 3 | Sonnet/Opus | 2-5s | $0.003-0.015 | 复杂推理、架构设计、安全审计 |

**源码依据**：`CLAUDE.md` 中明确规定了 3-Tier 路由策略，并在每次 spawn agent 前检查 `[AGENT_BOOSTER_AVAILABLE]` 或 `[TASK_MODEL_RECOMMENDATION]` 标签来决策路由。

---

## 2. 防漂移编码蜂群（Anti-Drift Coding Swarm）

这是 Ruflo 最核心的代码质量控制手段。当处理多文件、新功能、跨模块重构等复杂任务时，自动启动防漂移蜂群：

**拓扑**：`hierarchical`（层级式，中心化协调防止漂移）
**最大 Agent 数**：6-8（小团队=更少漂移）
**策略**：`specialized`（明确角色边界，无重叠）
**共识**：`raft`（Leader 维护权威状态）
**检查点**：通过 `post-task` hooks 频繁检查

```
mcp__ruv-swarm__swarm_init({
  topology: "hierarchical",
  maxAgents: 8,
  strategy: "specialized"
})
```

**源码依据**：`CLAUDE.md` 的 Swarm Configuration & Anti-Drift 章节。

---

## 3. 专业化 Agent 角色与评审机制

Ruflo 定义了 60+ 种 Agent 类型，每种有明确的职责边界。质量控制相关的核心 Agent：

| Agent | 职责 |
|-------|------|
| `reviewer` | 代码评审、质量分析、最佳实践检查 (`agents/reviewer.yaml`) |
| `security-architect` | 威胁建模、漏洞分析、安全审查 (`agents/security-architect.yaml`) |
| `tester` | TDD London School 方法、90%+ 覆盖率、mock-first |
| `security-auditor` | 安全审计 |
| `tdd-london-swarm` | 专职 TDD 测试蜂群 |

**任务执行时自动编排评审流程**（`CLAUDE.md` Agent Routing）：
- Bug Fix 路由：coordinator → researcher → coder → **tester**
- Feature 路由：coordinator → architect → coder → tester → **reviewer**
- 安全性路由：coordinator → **security-architect** → **auditor**

---

## 4. 质量度量与监控

`SwarmCoordinator` (`v3/src/coordination/application/SwarmCoordinator.ts`) 为每个 Agent 维护实时指标：

```typescript
agentMetrics.set(agent.id, {
  agentId: agent.id,
  tasksCompleted: 0,
  tasksFailed: 0,
  averageExecutionTime: 0,
  successRate: 1.0,   // 成功率实时计算
  health: 'healthy'    // healthy | degraded | unhealthy
});
```

每次任务执行后自动更新（`:193-220`），指标用于：
- 健康检测（health check interval: 5s）
- 负载均衡决策（capability-match 策略）
- 熔断——unhealthy 的 Agent 不再分配任务

---

## 5. 插件扩展点质量拦截

`PluginManager` (`v3/src/infrastructure/plugins/PluginManager.ts`) 通过扩展点机制实现生命周期内的质量拦截：

```typescript
// 标准扩展点（ExtensionPoint.ts）
task.beforeExecute   // 执行前验证
task.afterExecute    // 执行后检查
task.validate        // 任务验证
workflow.beforeExecute
workflow.afterExecute
workflow.onError     // 错误处理
memory.beforeStore   // 存储前过滤
```

插件按优先级排序执行，即使某个插件处理失败也不影响其他插件继续执行（`:174-195`），保证质量检查链的健壮性。

---

## 6. 事件溯源（Event Sourcing, ADR-007）

所有任务执行、Agent 生命周期、工作流状态变更均通过 EventEmitter 发布事件，并持久化到内存后端：

```typescript
this.eventBus.emit('agent:spawned', { agentId, type });
this.eventBus.emit('agent:terminated', { agentId });
this.eventBus.emit('workflow:taskComplete', { workflowId, taskId });
```

每个 workflflow 维护完整的 `eventLog` 和 `memorySnapshots`，支持事后审计和 Debug（`WorkflowEngine.ts:306-322`）。

---

## 7. 混合记忆系统与模式学习（RuVector Intelligence）

### Hybrid Memory Backend（ADR-009）

`HybridBackend` (`v3/src/memory/infrastructure/HybridBackend.ts`) 结合 SQLite（结构化查询）和 AgentDB（向量检索, HNSW 索引, 150x-12,500x 加速），使 Agent 能从历史模式中学习：

- 任务完成后的 pattern store → 后续相似任务检索复用
- 向量相似度搜索（cosine similarity）匹配历史成功/失败模式
- Hybrid search：先向量检索再按元数据过滤（`:122-159`）

### 4 步智能流水线（RuVector）

```
RETRIEVE (HNSW) → JUDGE (verdict) → DISTILL (LoRA) → CONSOLIDATE (EWC++)
```

| 组件 | 功能 | 质量意义 |
|------|------|----------|
| SONA | <0.05ms 自适应优化 | 模式学习最优路径 |
| EWC++ | Fisher 矩阵防遗忘 | 不因新学习覆盖旧知识 |
| MoE | 8 Expert 门控路由 | 将任务路由到最擅长该领域的 expert |
| LoRA | 128x 压缩微调 | 快速适应新模式 |
| HNSW | 150x-12,500x 加速 | 海量模式中快速检索 |

---

## 8. Hooks 系统质量控制（17 Hooks + 12 Workers）

通过 `plugin/hooks/hooks.json` 配置的 Claude Code Hooks 在工具调用前后注入质量检查：

| Hook 事件 | 触发器 | 质量控制动作 |
|-----------|--------|-------------|
| PreToolUse (Edit) | 修改文件前 | `pre-edit` 备份并校验 |
| PostToolUse (Edit) | 修改文件后 | `post-edit` 格式化和 pattern 学习 |
| PreToolUse (Task) | Spawn 子 agent | `pre-task` 记录任务描述 |
| PostToolUse (Task) | 任务完成 | `post-task` 性能分析、pattern 训练 |
| Stop | 会话结束 | 评估任务是否完整完成 |
| SubagentStop | 子 agent 结束 | 评估子任务是否达标 |

---

## 9. 共识机制（Consensus）

多种共识策略用于 Agent 间的决策质量控制（`SwarmCoordinator.ts:348-374`）：

| 策略 | 容错 | 适用场景 |
|------|------|----------|
| Raft | f < n/2 | Leader 维护权威状态（anti-drift 默认） |
| Byzantine (BFT) | f < n/3 | 容忍恶意/故障节点 |
| Gossip | 最终一致性 | 大规模集群 |
| CRDT | 无冲突 | 分布式冲突消解 |
| Quorum | 可配 | 按需表决 |

---

## 10. 代码质量约束（CLAUDE.md V3）

- **文件上限**：500 行/文件
- **类型要求**：所有公有 API 使用 typed interfaces
- **测试方法论**：TDD London School（mock-first）
- **状态管理**：Event Sourcing
- **输入校验**：系统边界处进行校验
- **无硬编码密钥**：禁止提交 secrets/.env

---

## 总结

Ruflo 的 LLM 质量把控体系可以概括为 **"路由分层 → 角色专业化 → 执行监控 → 模式学习"** 四层闭环：

```
任务输入 → 3-Tier 路由 → 防漂移蜂群执行
                  ↓
            Reviewer/安全检查 ← 插件扩展点拦截
                  ↓
         Agent Metrics 成功/失败统计
                  ↓
        RuVector 模式学习（存储成功模式）
                  ↓
        下次相似任务复用模式 → 质量持续提升
```

这一体系的核心设计哲学是：**不在一个环节做完美过滤，而是在全链路各环节分层设卡，让质量问题无处可逃**。
