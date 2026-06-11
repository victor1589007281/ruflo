# Ruflo 蜂群编排效率分析

## 概述

Ruflo 的蜂群编排核心追求在**并行度、资源利用率、任务吞吐、延迟控制**之间取得平衡。本文分析其在拓扑设计、任务调度、负载均衡、并发执行、成本优化等方面的效率考量。

---

## 1. 拓扑灵活性——根据场景选择最优通信模式

`v3/swarm.config.ts` 定义了四种基础拓扑和一种混合拓扑，每种在通信开销和协调成本之间做不同权衡：

| 拓扑 | 连接方式 | 通信复杂度 | 适用场景 |
|------|----------|------------|----------|
| `hierarchical` | 严格层级，所有 Agent 通过 Queen 通信 | O(n) | 需要强一致性、防漂移的编码蜂群 |
| `mesh` | 全连接，所有 Agent 互相直连 | O(n²) | 需要高信息流通的探索/研究任务 |
| `hierarchical-mesh` | 域内 mesh + 域间通过 Queen 协调 | O(k² + d) | **默认拓扑**——兼顾效率与信息流通 |
| `centralized` | 全部通过中心节点 | O(1) per agent | 简单编排 |
| `adaptive` | 动态变更 | 自适应 | 按需 |

**`SwarmCoordinator.ts:421-444`** 实现了不同拓扑的连接建立逻辑：
- mesh 模式下新 Agent 连接所有其他 Agent
- hierarchical 模式下只连接 leader

**效率意义**：避免"一刀切"拓扑。编码任务使用 hierarchical 防止漂移，而搜索/研究使用 mesh 加速信息共享。

---

## 2. 基于能力匹配的负载均衡（Capability-Match）

`SwarmCoordinator.distributeTasks()` (`:145-188`) 实现了智能任务分配：

```typescript
// 1. 按优先级排序任务
const sortedTasks = Task.sortByPriority(tasks);

// 2. 按能力过滤合适的 Agent
const suitableAgents = agents.filter(agent =>
  agent.canExecute(task.type) && agent.status === 'active'
);

// 3. 选择当前负载最低的 Agent
let bestAgent = suitableAgents[0];
let lowestLoad = agentLoads.get(bestAgent.id) || 0;
```

**效率考量**：
- **优先级调度**：高优先级任务先分配（`Task.sortByPriority()`）
- **能力过滤**：避免将代码任务分配给 tester（`Agent.canExecute()`）
- **最小负载优先**：确保 Agent 间负载均匀分布，不做"热点集中"
- **状态感知**：只分配给 active 状态的 Agent

---

## 3. 并发执行模型

### 多任务并发

**`SwarmCoordinator.executeTasksConcurrently()`** (`:245-261`)：

```typescript
async executeTasksConcurrently(tasks: ITask[]): Promise<TaskResult[]> {
  const assignments = await this.distributeTasks(tasks);
  const results = await Promise.all(
    assignments.map(assignment => this.executeTask(...))
  );
  return results;
}
```

核心优化：`Promise.all` 并行执行所有已分配的任务。

### 分布式工作流

**`WorkflowEngine.executeDistributedWorkflow()`** (`:240-277`)：

```typescript
// 将任务分块到多个协调器上并行执行
const tasksPerCoordinator = Math.ceil(tasks.length / coordinators.length);
const taskChunks = [];
// ...分块逻辑
await Promise.all(taskChunks.map((tasks, index) => {
  const coordinator = coordinators[index % coordinators.length];
  // 每个协调器并行处理自己的任务块
}));
```

**效率意义**：多协调器水平扩展，突破单机 Agent 池瓶颈。

### 工作流内并行

`WorkflowEngine.runWorkflow()` (`:367-459`) 使用拓扑排序处理任务依赖，无依赖的任务在有序列表中连续执行（理论上可并行，当前实现为依序，为后续并行化留了接口如 `executeParallel()`）。

---

## 4. 拓扑排序的任务依赖解析

`Task.resolveExecutionOrder()` (`v3/src/task-execution/domain/Task.ts:171-201`)：

```typescript
static resolveExecutionOrder(tasks: Task[]): Task[] {
  // 拓扑排序
  while (remaining.length > 0) {
    const ready = tasks.filter(task =>
      task.areDependenciesResolved(resolvedIds)
    );
    if (ready.length === 0 && remaining.length > 0) {
      throw new Error('Circular dependency detected');
    }
    // 按优先级排序
    const sorted = Task.sortByPriority(ready);
    for (const task of sorted) { /* 加入 resolved */ }
  }
}
```

**效率考量**：
- 无依赖的任务在同批次内按优先级排序执行
- 环检测避免死锁
- 配合 WorkflowEngine 的 `rollbackOnFailure` 可在失败时回滚已完成的任务

---

## 5. 分阶段执行（Phase-Based Execution）

`v3/swarm.config.ts` 的 Phase 定义将 14 周的开发划分成 4 个阶段，每阶段激活不同域的 Agent：

```typescript
phases: [
  { id: 'phase-1-foundation', weeks: [1, 2],  activeDomains: ['security', 'core'] },
  { id: 'phase-2-core',       weeks: [3, 6],  activeDomains: ['core', 'quality'] },
  { id: 'phase-3-integration', weeks: [7, 10], activeDomains: ['integration', 'quality', 'performance'] },
  { id: 'phase-4-release',    weeks: [11, 14], activeDomains: ['security', 'core', 'integration', 'quality', 'performance', 'deployment'] }
]
```

**效率意义**：
- 避免早期资源浪费在不需要的域上
- 保证依赖先就绪（quality 必须在 core 之后激活）
- `getActiveAgentsForPhase()` 只 spawn 当前阶段需要的 Agent

---

## 6. 成本与延迟分层的 3-Tier 路由（ADR-026）

这是 Ruflo 最重要的**经济效率考量**：

```
Tier 1: Agent Booster (WASM) → <1ms, $0      —— 简单操作免费处理
Tier 2: Haiku               → ~500ms, $0.0002 —— 常规任务低成本处理
Tier 3: Sonnet/Opus         → 2-5s, $0.003-0.015 —— 复杂任务交高成本模型
```

实现路径：通过 `[AGENT_BOOSTER_AVAILABLE]` 标签检测，使用 `Edit` 工具而非 spawn LLM agent 来处理简单的代码变换（var-to-const、add-types、add-error-handling、async-await 等）。

**效率意义**：大量简单操作绕过 LLM 调用，延迟从秒级降至毫秒级，成本降为 0。

---

## 7. 动态扩缩容（Horizontal Scaling）

`SwarmCoordinator.scaleAgents()` (`:319-343`)：

```typescript
async scaleAgents(config: { type: string; count: number }): Promise<void> {
  if (config.count > 0) {
    // Scale up
    for (let i = currentCount; i < targetCount; i++) {
      await this.spawnAgent({ ... });
    }
  } else {
    // Scale down: remove oldest agents of type
    const toRemove = existingOfType.slice(0, Math.abs(config.count));
    for (const agent of toRemove) {
      await this.terminateAgent(agent.id);
    }
  }
}
```

**效率意义**：
- 正数扩缩：按需增加特定类型 Agent
- 负数缩容：移除最早创建的 Agent（FIFO）
- 动态适配工作负载变化

---

## 8. 后台工作系统（12 Workers）

`CLAUDE.md` 定义了 12 种后台 Worker，在蜂群主流程之外并行运行：

| Worker | 优先级 | 职责 |
|--------|--------|------|
| `ultralearn` | normal | 深度知识获取 |
| `optimize` | **high** | 性能优化 |
| `consolidate` | low | 记忆整合 |
| `audit` | **critical** | 安全分析 |
| `preload` | low | 资源预加载 |
| `deepdive` | normal | 深度分析 |

**效率意义**：将非阻塞任务异步化（audit、consolidate 等），不占用主执行路径的 Agent 资源。

---

## 9. Headless 后台实例

利用 `claude -p`（print/pipe mode）spawn 无头 Claude 实例进行并行后台工作：

```bash
# 并行多个后台分析任务
claude -p "Analyze src/auth/ for vulnerabilities" &
claude -p "Write tests for src/api/endpoints.ts" &
claude -p "Review src/models/ for performance issues" &
wait
```

**效率意义**：不占用交互式会话的 Agent 资源，实现"横向扩展"；
同时可以通过 `--model haiku` 将简单任务路由到更快的模型。

---

## 10. 内存与模式缓存减少重复计算

### HNSW 索引加速搜索（150x-12,500x）

AgentDB 的 HNSW 索引使得 Agent 在检索历史模式时：
- 无需重新推理
- 直接复用已验证的成功模式
- 减少重复的 LLM 调用

### HybridBackend.hybridSearch()

先向量检索再过滤，而非全表扫描（`HybridBackend.ts:122-159`）。

---

## 11. 连接池与 MCP 优化

MCP Server (`v3/src/infrastructure/mcp/MCPServer.ts`) 和 connection-pool 的设计：

- **目标响应时间**：<100ms
- **工具注册表**：`toolRegistry` 在 `start()` 时预构建，运行时 O(1) 查找
- **错误处理链**：provider 依次尝试，一个失败自动尝试下一个（`:73-96`）

---

## 12. 会话持久化与恢复

`WorkflowEngine.restoreWorkflow()` (`:327-343`) 支持从 MemoryBackend 恢复工作流状态：

```typescript
const stateMemory = await this.memoryBackend.retrieve(`workflow-state-${workflowId}`);
return JSON.parse(stateMemory.content);
```

**效率意义**：会话中断不丢失进度，避免从头重算。

---

## 总结

Ruflo 的蜂群编排效率可以概括为 **"路由节省成本、并行提升吞吐、分级控制质量、后台消除阻塞"**：

```
                   成本控制
                3-Tier 路由 → 简单操作 WASM 免费处理
                    ↓
                负载均衡 → 能力匹配 + 最小负载
                    ↓
  ┌─────── 并行执行 ───────┐
  │  Promise.all 多任务并行  │
  │  分布式跨协调器并行       │
  │  12 Workers 异步后台     │
  └────────────────────────┘
                    ↓
                拓扑排序 → 依赖解耦
                    ↓
                分阶段执行 → 资源按需激活
                    ↓
                模式复用 → HNSW 减少重算
```

关键设计取舍：
- **层级 vs 全连接**：默认 `hierarchical-mesh` 是平衡点——域内高效通信 + 域间有序协调
- **成本 vs 质量**：3-Tier 路由确保不把简单任务浪费在高成本模型上
- **资源 vs 并发**：Agent 数量限制 + 动态扩缩容防止资源耗尽
- **实时 vs 后台**：12 Workers 将非关键路径异步化
