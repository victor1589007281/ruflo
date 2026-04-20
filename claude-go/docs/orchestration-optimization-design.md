# 编排调度系统优化方案

> **版本**: 1.0 | **日期**: 2026-04-20
> **严重性**: 🔴 高 — 直接影响研发团队可用性

---

## 一、问题诊断

### 1.1 根因分析

| **问题** | **根因** | **影响** | **严重性** |
|:--------:|:--------:|:--------:|:----------:|
| 团队卡住不恢复 | `adversarial_dev` 绕过 Coordinator 重试 | 任务失败后无法 resume | 🔴 |
| Orchestrator 停滞 | 仅 log 不恢复, 10min 后强制退出丢弃剩余任务 | DAG 残留任务被遗弃 | 🔴 |
| 心跳无效 | `checkTeamHealth` 仅写 log, 不通知/不恢复 | 用户无感知 | 🟡 |
| 调度效率低 | `filterParallel` 要求依赖集完全相同才并行 | 错失并行机会 | 🟡 |
| 无团队级 watchdog | 团队长时间无进展无任何处理 | 团队永远 running | 🔴 |
| idle 检测为单次 | 仅检查单次输出, 无跨轮状态 | 误判/漏判 | 🟡 |

### 1.2 核心链路问题

```
┌─────────────── 当前链路 ──────────────────────────────────────┐
│                                                               │
│  RunWithRecovery                                              │
│  ├─ adversarial_dev → executor.Execute (无 Coordinator 重试!) │
│  │   ├─ runDesignPhase                                        │
│  │   ├─ runOrchestratedPhase → Orchestrator.Execute           │
│  │   │   ├─ 停滞超时: 10min → 强制 break (丢弃剩余任务)     │
│  │   │   └─ 无恢复尝试                                        │
│  │   └─ runFinishPhase                                        │
│  └─ heartbeatLoop → 仅 log, 不通知/不恢复                    │
│                                                               │
│  问题: 任何 Phase 卡住 → 整个团队永远 running                │
└───────────────────────────────────────────────────────────────┘
```

## 二、业界参考

| **方案** | **核心思想** | **借鉴** |
|:--------:|:----------:|:--------:|
| Temporal Activity Timeout | 分层超时: Schedule-To-Start / Start-To-Close / Heartbeat | 分层超时 |
| K8s Liveness Probe | 进程活着但无进展 → 重启 | watchdog 模式 |
| DynTaskMAS (ICAPS 2025) | 动态 DAG + 异步并行 + 自适应工作流 | 并行调度优化 |
| AgentOrchestra | 监督协议 + 失败恢复路径 | 恢复策略 |
| Airflow trigger_rule | 上游失败时下游的条件执行 | DAG 残留处理 |
| 指数退避 + 抖动 | base 1s, factor 2, cap 60s, full jitter | 重试策略 |

## 三、优化方案

### 3.1 团队级 Watchdog (最关键)

```
┌─────────────── 新增: TeamWatchdog ──────────────────────────┐
│                                                              │
│  teamWatchdog (后台 goroutine, 60s 检查一次)                │
│  ├─ 检查: 最后活动时间 > staleThreshold (5min)              │
│  │   ├─ 通知用户: "⚠️ 团队 X 已 5 分钟无进展"              │
│  │   └─ 更新 team.LastActivity                               │
│  ├─ 检查: 最后活动时间 > criticalThreshold (15min)          │
│  │   ├─ 通知用户: "🔴 团队 X 已 15 分钟无进展, 尝试恢复"    │
│  │   └─ 触发 resumeStaleTeam()                              │
│  └─ 检查: 所有 Agent idle + 剩余未完成任务 > 0              │
│      └─ 触发 forceScheduleRemaining()                       │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 3.2 adversarial_dev 重试包装

```go
// 修改 RunWithRecovery: adversarial_dev 也套 Coordinator 重试
case "adversarial_dev":
    return c.executeWithWatchdog(ctx, wf, objective, team, executor)
```

### 3.3 Orchestrator 停滞恢复

停滞时不直接退出, 而是尝试恢复:
1. 识别卡住的任务
2. 取消超时任务
3. 重新注入上下文重试
4. 3 次恢复失败后才退出 + 通知

### 3.4 filterParallel 修复

当前: 要求所有 ready 阶段的依赖集字符串完全相同才并行
修复: 所有标记 `Parallel=true` 的 ready 阶段都应并行

### 3.5 DAG 残留任务清理

Orchestrator 退出后, 检测并标记所有未完成任务, 通知用户。

## 四、实现优先级

| **#** | **改动** | **影响** | **优先** |
|:-----:|:--------:|:--------:|:--------:|
| 1 | 团队级 Watchdog + 用户通知 | 解决"团队永远running"问题 | P0 ✅ |
| 2 | adversarial_dev 重试包装 | 解决"设计阶段失败无恢复" | P0 ✅ |
| 3 | Orchestrator 停滞恢复 | 解决"DAG 残留任务被丢弃" | P0 ✅ |
| 4 | filterParallel 修复 | 提升调度效率 | P1 ✅ |
| 5 | heartbeat 增强 (通知+检测) | 提升可观测性 | P1 ✅ |
| 6 | DAG 残留任务清理 | 提升健壮性 | P1 ✅ |

---

## 四、第二轮深度优化 (2026-04-20)

### 4.1 核心 Bug 修复

| **Bug** | **根因** | **影响** | **文件** |
|:--------:|:--------:|:--------:|:--------:|
| DAG 阻塞传播 | `SetTaskStatusAndUnblock` 只在 `completed` 时 unblock 后续任务 | 一个任务 `failed` 后所有下游永远 `blocked` | `tasktools.go` |
| 竞态条件 | `SetTaskStatusAndUnblock` 非原子 (两次 lock/unlock) | 并发更新可能丢失 unblock | `tasktools.go` |

**修复方案**: 瞬态/永久错误分治 + 条件级联

```
错误发生
  ├─ 瞬态? (429/网络/超时)
  │   ├─ 额外 3 次重试 (共 5 次, 含指数退避)
  │   └─ 全部耗尽: 仅标记自身 failed, 下游保持 blocked
  │       └─ stall recovery 检测到后:
  │           重置 Retries=0, 状态→pending, 重新调度
  │
  └─ 永久? (验证/代码质量)
      ├─ 标准 2 次重试
      └─ 全部耗尽: 级联标记所有下游为 failed
          └─ 避免浪费 LLM 调用
```

关键设计:
- `isTransientError()` — 匹配 429/rate/timeout/网络错误/503/529 等模式
- 瞬态失败不级联 → 下游保持 blocked → stall recovery 有机会恢复
- 永久失败级联 → 递归标记下游 failed → `completedCount + failedCount == totalCount`
- `attemptStallRecovery` 增强: 检测瞬态失败任务, 重置重试计数器, 重新调度

### 4.2 效率优化 (不妥协质量)

| **优化** | **根因** | **节省** | **文件** |
|:--------:|:--------:|:--------:|:--------:|
| ~~评审达标跳过 micro-test~~ | ~~已撤回~~ | ~~micro-test 是独立质量柱, 不能跳过~~ | - |
| 流式调度替代 batch barrier | `wg.Wait()` 整批等待, 最慢任务拖住后续 | 任务完成立即触发新调度 | `orchestrator.go` |
| 缩短轮询间隔 2s→500ms | 空就绪队列轮询过慢 | 依赖解除后最多 500ms 调度 | `orchestrator.go` |
| Orchestrator→Coordinator 活动回传 | watchdog 误报"无进展" | 消除假阳性告警 | `orchestrator.go` |

### 4.3 实测验证 (go-development-6054, qwen3.6-plus)

| **任务** | **轮数** | **耗时** | **首轮→终轮评分** | **结果** |
|:--------:|:--------:|:--------:|:--------:|:--------:|
| 初始化项目骨架与 main 入口 | **1** | 2m42s | 正确9/完整8/安全8/质量9 | ✅ 首轮达标 |
| 实现 Domain 层 | **1** | 2m26s | 正确9/完整8/安全10/质量8 | ✅ 首轮达标 |
| 封装 Delivery 基础工具 | **1** | 3m59s | 正确7/完整8/安全7/质量8 | ✅ 首轮达标 |
| 实现 Infrastructure 层 | **3** | 12m37s | 5/6/8/6 → 8/8/9/9 | ✅ 对抗提升质量 |
| 实现 UseCase 层 | **5** | 22m54s | 3/6/7/4 → 9/8/8/8 | ✅ 5轮对抗, 充分迭代 |
| UseCase 单元测试 | **1** | 3m27s | 正确6/完整6/安全9/质量7 | ✅ 首轮达标 |
| Delivery Handler | **1** | 3m35s | 正确8/完整8/安全5/质量8 | ✅ 首轮达标 |
| main.go 依赖组装 | **2** | 6m46s | 4/6/8/6 → 9/8/7/9 | ✅ 第2轮达标 |
| HTTP+SQLite 集成测试 | 2 (中断) | - | 4/6/7/6 → 进行中 | ⏳ 进程退出 |
| 静态检查与CI配置 | blocked | - | - | ⏳ 等待前置 |

**关键验证结论**:
- ✅ **DAG 调度正常**: 10 个任务按依赖关系正确调度, **无卡住/不调度问题**
- ✅ **并行调度生效**: 依赖满足的任务立即并行执行 (如 Domain+Delivery, UseCase+Infrastructure)
- ✅ **对抗质量保证**: 达标立即终止, 不达标继续迭代 (UseCase 从正确=3提升到9)
- ✅ **AdaptiveTerminator 全功能**: 策略转换、revert、退化检测全部正常触发
- ⚠️ **Watchdog 假阳性**: Orchestrator 活跃期间 Coordinator 误报"无进展" (已修复)
