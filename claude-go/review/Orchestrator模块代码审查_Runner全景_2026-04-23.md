# Orchestrator 模块代码审查 — Runner 全景 + 流程解读

> 审查日期: 2026-04-23 | 版本: v2 (Runner 全景补充) | 代码分支: claude-go/pkg/orchestrator/
> 目录: `pkg/orchestrator/`
> 文件数: 11 个 Go 源文件 + 7 个测试文件
> 测试状态: ✅ 全部通过 (`go test ./pkg/orchestrator/...`)

---

## 一、一句话理解

> **用户/LLM 提交一个"要做哪些事、谁依赖谁"的清单，引擎自动并发执行、处理失败重试、共享中间结果，直到所有事做完。**

---

## 二、全景架构

### 2.1 分层架构图

```
┌──────────────────────────────────────────────────────────────────┐
│                     外部调用者 (LLM / 代码)                        │
└──────────────────────┬───────────────────────────────────────────┘
                       │  JSON / Go API
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│  Layer 1: AI Tool 层 (tool.go)                                   │
│  ┌───────────────────────────────────────────────────────────┐   │
│  │  ToolEngine                                                │   │
│  │  • 管理多个 Graph 生命周期 (类似连接池)                     │   │
│  │  • 13 种 action 路由 (create/run/cancel/status/...)        │   │
│  │  • 同步/异步执行支持                                       │   │
│  └───────────────────────────────────────────────────────────┘   │
└──────────────────────┬───────────────────────────────────────────┘
                       │  编程式调用
                       ▼
┌──────────────────────────────────────────────────────────────────┐
│  Layer 2: 编排核心 (engine.go)                                   │
│  ┌───────────────────────────────────────────────────────────┐   │
│  │  Engine.Run() — 主循环                                     │   │
│  │  ┌─────────┐  ┌──────────┐  ┌──────────┐  ┌────────────┐  │   │
│  │  │ 调度     │→│ 派发      │→│ 等待信号  │→│ 处理完成    │  │   │
│  │  │Schedule │→│go execute│→│doneCh     │→│handleDone  │  │   │
│  │  └─────────┘  └──────────┘  └──────────┘  └────────────┘  │   │
│  │  辅助: 停滞检测 | 检查点保存 | 级联失败处理                   │   │
│  └───────────────────────────────────────────────────────────┘   │
└─┬─────────┬─────────┬─────────┬─────────┬─────────┬────────────┘
  │         │         │         │         │         │
  ▼         ▼         ▼         ▼         ▼         ▼
┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐
│ 调度器  │ │ 黑板   │ │ 背压   │ │ 执行器  │ │ 钩子   │ │ 检查点  │
│schedul-│ │black-  │ │back-   │ │runner. │ │hooks.  │ │checkp- │
│er.go   │ │board.go│ │press.  │ │go      │ │go      │ │oint.go │
│        │ │        │ │go      │ │        │ │        │ │        │
│三阶段  │ │版本控制 │ │三层限  │ │注册表+ │ │事件通知│ │断点续跑│
│调度    │ │事件通知 │ │流控制  │ │组合执行│ │扇出   │ │序列化  │
└────────┘ └────────┘ └────────┘ └────────┘ └────────┘ └────────┘

┌──────────────────────┐  ┌──────────────────────┐
│ 图结构 (graph.go)     │  │ LLM 集成 (llm_runner) │
│ DAG + 拓扑排序        │  │ LLMRunner             │
│ 关键路径 + DAG宽度    │  │ AdversarialRunner     │
│ ──────────────────   │  │ LLMExpander           │
│                       │  │ 质量评分+自适应终止    │
│ 错误系统 (errors.go)  │  │                      │
│ 三级错误分类          │  │ 可观测性 (observer)    │
│ 重试决策 + 指数退避   │  │ MetricsHook           │
└──────────────────────┘  │ ConflictDetector      │
                          │ DynamicExpander        │
                          └──────────────────────┘
```

### 2.2 模块协作关系

```
                     ┌──────────────┐
                     │   Graph      │  ← 用户定义: 有哪些任务, 谁依赖谁
                     │ (数据结构)    │
                     └──────┬───────┘
                            │ Build() 验证拓扑
                            ▼
                     ┌──────────────┐
                     │    Engine    │  ← 驱动整个流程
                     │  (大脑)      │
                     └──┬──┬──┬──┬─┘
              ┌─────────┘  │  │  └─────────┐
              ▼            ▼  ▼            ▼
        ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐
        │Scheduler │ │Blackboard│ │ Runners  │ │Backpress │
        │(谁该执行) │ │(共享数据) │ │(怎么执行) │ │(限流保护) │
        └──────────┘ └──────────┘ └──────────┘ └──────────┘
              │                         │            │
              │                         ▼            │
              │                  ┌──────────┐       │
              │                  │LLMClient │       │
              │                  │(外部API)  │       │
              │                  └──────────┘       │
              │                                     │
              ▼                                     ▼
        ┌──────────┐                         ┌──────────┐
        │  Hooks   │                         │ Checkpnt │
        │(事件通知) │                         │(持久化)  │
        └──────────┘                         └──────────┘
```

---

## 三、完整执行流程 — 以 parenting 工作流为例

### 3.1 用户定义的工作流

```
                    intake (内容审核)
                       │
                       ▼
                safety-screen (安全筛查)
                       │
           ┌───────────┼───────────┬───────────┐
           ▼           ▼           ▼           ▼
    academic-tutor  psych-coach  parent-adv   dev-assess
           └───────────┼───────────┴───────────┘
                       ▼
                action-plan (行动计划)
                       │
                       ▼
            consultation-report (咨询报告)
```

### 3.2 完整时序图

```
用户/LLM                    ToolEngine                Engine                  Scheduler              Runner/BB
   │                           │                        │                        │                     │
   │ 1. create_graph           │                        │                        │                     │
   │──────────────────────────▶│                        │                        │                     │
   │                           │ NewGraph(id,name)      │                        │                     │
   │                           │────┐                   │                        │                     │
   │                           │◀───┘                   │                        │                     │
   │                           │                        │                        │                     │
   │ 2. add_task (7 次)        │                        │                        │                     │
   │──────────────────────────▶│                        │                        │                     │
   │                           │ g.AddTask(task)        │                        │                     │
   │ 3. add_edge (7 次)        │                        │                        │                     │
   │──────────────────────────▶│                        │                        │                     │
   │                           │ g.AddEdge(from,to)     │                        │                     │
   │                           │                        │                        │                     │
   │ 4. run_graph              │                        │                        │                     │
   │──────────────────────────▶│                        │                        │                     │
   │                           │ engine.Run(ctx, g)     │                        │                     │
   │                           │───────────────────────▶│                        │                     │
   │                           │                        │                        │                     │
   │                           │                    g.Build()                    │                     │
   │                           │                  (Kahn 拓扑排序)                │                     │
   │                           │                 检测环路, 初始化状态              │                     │
   │                           │                        │                        │                     │
   │                           │              ┌─────────▼─────────┐              │                     │
   │                           │              │   主循环开始        │              │                     │
   │                           │              │                   │              │                     │
   │                           │              │  Round 1:         │              │                     │
   │                           │              │  调度器:          │              │                     │
   │                           │              │  ReadyTasks()     │              │                     │
   │                           │              │  → [intake]       │              │                     │
   │                           │              │  Filter→Score     │              │                     │
   │                           │  Schedule()  │  → batch=[intake] │              │                     │
   │                           │◀─────────────│──────────────────▶│              │                     │
   │                           │              │                   │              │                     │
   │                           │              │  go executeTask(intake)           │                     │
   │                           │              │─────────────────────────────────────────────────────▶│
   │                           │              │                   │              │  taskCtx, backpress│
   │                           │              │                   │              │  runner.Execute()  │
   │                           │              │                   │              │  → LLM调用          │
   │                           │              │                   │              │  → 结果写入 BB      │
   │                           │              │                   │              │                     │
   │                           │              │  doneCh←taskDone  │              │                     │
   │                           │              │◀──────────────────────────────────────────────────────│
   │                           │              │                   │              │                     │
   │                           │              │  handleDone():    │              │                     │
   │                           │              │  1. intake→Completed              │                     │
   │                           │              │  2. bb.Write("intake/output", ..)│
   │                           │              │  3. unblockDownstream("intake")  │
   │                           │              │     ↓ safety-screen→Ready         │                     │
   │                           │              │  4. drain: 无额外信号              │                     │
   │                           │              │                   │              │                     │
   │                           │              │  Round 2:         │              │                     │
   │                           │              │  ReadyTasks()     │              │                     │
   │                           │              │  → [safety-screen]│              │                     │
   │                           │              │  调度 → 派发 → 执行 → 完成         │                     │
   │                           │              │  unblockDownstream:               │                     │
   │                           │              │    4个任务同时 Blocked→Ready       │                     │
   │                           │              │                   │              │                     │
   │                           │              │  Round 3:         │              │                     │
   │                           │              │  ReadyTasks()     │              │                     │
   │                           │              │  → [academic, psych, parent, dev] │                    │
   │                           │              │  Score:           │              │                     │
   │                           │              │  academic=1000    │              │                     │
   │                           │              │  dev=1000         │              │                     │
   │                           │              │  psych=500        │              │                     │
   │                           │              │  parent=500       │              │                     │
   │                           │              │  maxPar=2 → 取前2  │              │                     │
   │                           │              │  → dispatch(academic, dev)        │                     │
   │                           │              │                   │              │  并行执行...         │
   │                           │              │                   │              │                     │
   │                           │              │  doneCh←academic  │              │                     │
   │                           │              │  handleDone()     │              │                     │
   │                           │              │  drain→doneCh←dev │              │                     │
   │                           │              │                   │              │                     │
   │                           │              │  Round 4:         │              │                     │
   │                           │              │  剩余 2 个 (psych, parent)        │                     │
   │                           │              │  执行 → 完成       │              │                     │
   │                           │              │                   │              │                     │
   │                           │              │  4个都完成→        │              │                     │
   │                           │              │  action-plan→Ready│              │                     │
   │                           │              │  执行 → 完成       │              │                     │
   │                           │              │                   │              │                     │
   │                           │              │  consultation-report→Ready        │                     │
   │                           │              │  执行 → 完成       │              │                     │
   │                           │              │                   │              │                     │
   │                           │              │  isComplete(8) == true → break    │                     │
   │                           │              │                   │              │                     │
   │                           │              │  saveCheckpoint() │              │                     │
   │                           │              │  buildResult()    │              │                     │
   │                           │◀─────────────│                    │             │                     │
   │                           │  ExecutionResult                 │              │                     │
   │◀──────────────────────────│                                  │              │                     │
   │  {success:true, metrics..}│                                  │              │                     │
   │                           │                                  │              │                     │
```

### 3.3 函数调用链 — 一个任务的完整生命周期

```
以一个任务 (如 academic-tutor) 为例, 从被调度到完成的全过程:

1. [调度] Engine.Run() 主循环
   └── Scheduler.Schedule(ctx, avail)              ← 三阶段调度
       ├── Graph.ReadyTasks()                      ← 找出所有 Ready 状态的任务
       ├── Phase 1: Filter 过滤
       │   ├── DependencyFilter.Filter()           ← 检查: 所有上游都 Completed 吗?
       │   └── ResourceFilter.Filter()             ← 检查: 背压队列还能塞新任务吗?
       ├── Phase 2: Score 打分
       │   ├── PriorityScore.Score()               ← Priority × 100
       │   ├── CriticalPathScore.Score()           ← 在关键路径上? +500
       │   └── FairnessScore.Score()               ← -Retries × 50
       └── Phase 3: Dispatch 截取前 N 个

2. [派发] Engine.Run() 主循环
   ├── Task.setStateInternal(TaskRunning)           ← 状态标记
   ├── BackpressureCtrl.IncrQueue()                 ← 队列计数 +1
   ├── LifecycleHook.OnTaskStart(t)                 ← 通知外部
   └── go executeTask(ctx, g, t, doneCh)            ← 新 goroutine 启动!

3. [执行] Engine.executeTask() (独立 goroutine)
   ├── context.WithTimeout(taskCtx, timeout)        ← 超时控制
   ├── BackpressureCtrl.AcquireAll()                ← 获取背压许可
   │   ├── TokenBucket.Acquire()                    ← L1: 获取 RPM 令牌 (可能阻塞)
   │   └── AdaptiveSemaphore.Acquire()              ← L2: 获取并发许可 (可能阻塞)
   ├── RunnerRegistry.Get(t.Runner)                 ← 查找对应的执行器
   ├── TaskRunner.Execute(ctx, t, bb)               ← 真正干活!
   │   └── [以 LLMRunner 为例]
   │       ├── task.Config["system_prompt"]         ← 读取配置
   │       ├── task.Config["user_prompt"]           ← 读取配置
   │       ├── resolveTemplate()                    ← 替换模板变量
   │       │   ├── bb.Read(depID + "/output")       ← 从黑板读取上游输出
   │       │   ├── "{prev_result}" → 所有上游拼接    │
   │       │   └── "{dep:taskID}" → 指定上游输出     │
   │       └── LLMClient.SimpleComplete(...)        ← 调用 LLM API
   ├── BackpressureCtrl.ReleaseConc(success)        ← 释放并发, AIMD 调整
   └── doneCh ← taskDone{taskID, output, err}       ← 向主循环发送完成信号

4. [处理完成] Engine.Run() 主循环
   ├── case done := <-doneCh                        ← 收到完成信号
   └── handleDone(g, done)
       └── [成功路径]
           ├── Task.setStateInternal(TaskCompleted)  ← 标记完成
           ├── Blackboard.Write(taskID+"/output", ..) ← 输出写黑板
           ├── LifecycleHook.OnTaskComplete()        ← 通知外部
           ├── unblockDownstream(taskID)             ← 解锁下游
           │   └── for each downstream task:
           │       └── check: 所有上游都 Completed?
           │           └── YES → Blocked → Ready     ← 下游变就绪!
           │           └── NO  → 保持 Blocked
           └── maybeCheckpoint()                     ← 检查是否需要保存检查点
       └── [失败路径]
           └── handleFailure(task, err)
               ├── RetryPolicy.ShouldRetry(err)     ← 决策: 能重试吗?
               │   ├── ClassifyErrorMsg(err)         ← 分类: Fatal/Transient/Permanent
               │   ├── Fatal → 级联取消所有下游
               │   ├── Transient → 挂起 (Suspended)
               │   └── Permanent → 重试 or 失败+级联
               ├── cascadeFailure(taskID)            ← 递归取消所有下游
               │   └── for each downstream:
               │       └── mark Cancelled + recurse

5. [停滞检测] Engine.Run() 主循环
   ├── case <-stallTicker.C                          ← 定时触发
   └── checkStall(g)
       └── 检查: 有 Running/Suspended 但无进展?
           └── YES → attemptStallRecovery()
               ├── Suspended → Ready (清空重试计数)
               └── Running 超 2 倍超时 → Ready (强制重置)
```

### 3.4 关键信号流

```
任务完成信号的流转 (这是整个引擎最核心的通信机制):

executeTask goroutine                    主循环 (Engine.Run)
       │                                        │
       │ 任务执行成功/失败                       │
       │        │                               │
       │        ▼                               │
       │  doneCh <- taskDone{...}               │  ← 通道! 这是 goroutine 间通信的唯一方式
       │        │                               │
       │        ├──────────────────────────────▶│
       │                                        │
       │                         select {       │
       │                         case done :=   │
       │                              <-doneCh  │  ← 收到!
       │                                        │
       │                         handleDone()   │
       │                         handleDone()   │  ← drain 批量处理
       │                         handleDone()   │     (如果同时完成多个)
       │                                        │
       │                         Schedule()     │  ← 下一轮调度
       │                         go execute...  │     可能派发新任务
       │                                        │
```

---

## 四、任务状态机 — 完整生命周期

### 4.1 状态转换图

```
                   添加任务
                      │
                      ▼
                  ┌─────────┐
                  │ Pending │  ← 刚创建, 等待 Build()
                  └────┬────┘
                       │ Build() 完成
                       │
              ┌────────▼────────┐
              │    判断有上游?    │
              └───┬────────┬────┘
                  │        │
              无上游      有上游
                  │        │
                  ▼        ▼
            ┌─────────┐ ┌─────────┐
            │  Ready  │ │ Blocked │  ← 上游依赖未满足
            └────┬────┘ └────┬────┘
                 │           │
           被调度器选中       │ 上游完成后 unblockDownstream()
                 │           │
                 ▼           ▼
            ┌─────────────────┐
            │    Running      │  ← 正在执行中
            └────┬──┬──┬─────┘
                 │  │  │
          成功   │  │  │  永久失败(重试耗尽)
          ┌──────┘  │  └──────┐
          │         │         │
          ▼         │         ▼
    ┌─────────┐     │   ┌─────────┐
    │Completed│     │   │ Failed  │
    └─────────┘     │   └─────────┘
                    │
                    │  瞬态失败(429/网络超时)
                    │  → Suspended → stallRecovery → Ready
                    │
                    ▼
              ┌───────────┐
              │ Suspended │  ← 瞬态错误挂起, 保留重试额度
              └─────┬─────┘
                    │ stallRecovery 恢复
                    ▼
               (回到 Ready)

  此外: 上下文取消 / 上游级联失败 → Cancelled
```

### 4.2 三种错误类型的处理路径

```
任务执行返回错误
        │
        ▼
  ClassifyErrorMsg(err)
        │
   ┌────┼────┐
   ▼    ▼    ▼
 Fatal  Transient  Permanent
 (致命)   (瞬态)    (永久)
   │      │         │
   │      │  重试次数 < MaxRetries
   │      │         │
   │      │    ┌────┴────┐
   │      │    YES       NO (重试耗尽)
   │      │    │          │
   │      │    ▼          │
   │      │  等待退避      │
   │      │  (Full Jitter)│
   │      │  重新执行      │
   │      │    │          │
   │      │    └──────────┤
   │      │               │
   │      │  重试次数 < MaxRetries+MaxTransient
   │      │               │
   │      │          ┌────┴────┐
   │      │          YES       NO (全部耗尽)
   │      │          │          │
   │      │          ▼          ▼
   │      │      继续等待    Suspended (挂起)
   │      │      重新执行     ↓ 等待 stallRecovery
   │      │                  ↓ 重置为 Ready
   │      │
   │      ▼
   │   永不重试
   │   标记 Failed
   │   cascadeFailure()
   │   → 递归取消所有下游 → Cancelled
   │
   ▼
永不重试
标记 Failed
cascadeFailure()
→ 递归取消所有下游 → Cancelled
```

---

## 五、关键调用链深入

### 5.1 调度管线 — Filter→Score→Dispatch

```
每轮调度执行一次完整管线:

                    Engine.Run()
                         │
                         │  avail = maxPar - runningCount()  (计算还有几个空位)
                         ▼
              Scheduler.Schedule(ctx, avail)
                         │
                         ▼
              ┌──────────────────────────┐
              │  Phase 1: Filter 过滤     │
              │  ┌────────────────────┐  │
              │  │ 遍历所有 Ready 任务  │  │
              │  │   ↓                 │  │
              │  │ DependencyFilter    │  │  检查: 所有上游都 Completed 了吗?
              │  │   ├─YES → 继续      │  │
              │  │   └─NO  → 排除 ❌    │  │
              │  │                     │  │
              │  │ ResourceFilter      │  │  检查: 背压队列还有空间吗?
              │  │   ├─YES → 通过 ✓    │  │
              │  │   └─NO  → 排除 ❌    │  │
              │  │                     │  │
              │  │ feasible = [通过的任务]│
              │  └────────────────────┘  │
              └──────────┬───────────────┘
                         │
              ┌──────────▼───────────────┐
              │  Phase 2: Score 打分     │
              │  ┌────────────────────┐  │
              │  │ 对每个 feasible 任务 │  │
              │  │                     │  │
              │  │ total = 0           │  │
              │  │ total += Priority×100│  ← PriorityScore
              │  │ total += 500?       │  ← CriticalPathScore (在关键路径上?)
              │  │ total += -Retries×50│  ← FairnessScore (重试惩罚)
              │  │                     │  │
              │  │ 按 total 降序排序    │  │
              │  └────────────────────┘  │
              └──────────┬───────────────┘
                         │
              ┌──────────▼───────────────┐
              │  Phase 3: Dispatch 截取  │
              │  ┌────────────────────┐  │
              │  │ 取前 avail 个任务   │  │  其余留在 Ready, 等下一轮
              │  │                     │  │
              │  │ batch = [task1,task2]│  │
              │  └────────────────────┘  │
              └──────────┬───────────────┘
                         │
                         ▼
              Engine.Run() 拿到 batch
              → 遍历 batch, go executeTask()
```

### 5.2 事件驱动解锁 — unblockDownstream()

```
当任务 T 完成时, 立即解锁其下游:

  unblockDownstream("safety-screen")
        │
        │ downstream["safety-screen"] = [academic-tutor, psych-coach, parent-advisor, dev-assessor]
        │
        ▼
  ┌────────────────────────────────────────────────────────────┐
  │ academic-tutor:                                            │
  │   DependsOn = [safety-screen]                              │
  │   → 检查: safety-screen.State() == Completed? → YES        │
  │   → 所有上游都完成了!                                       │
  │   → academic-tutor.State: Blocked → Ready ✓                │
  │                                                            │
  │ psych-coach:                                               │
  │   DependsOn = [safety-screen]                              │
  │   → 同样 → Blocked → Ready ✓                               │
  │                                                            │
  │ parent-advisor:                                            │
  │   DependsOn = [safety-screen]                              │
  │   → 同样 → Blocked → Ready ✓                               │
  │                                                            │
  │ dev-assessor:                                              │
  │   DependsOn = [safety-screen]                              │
  │   → 同样 → Blocked → Ready ✓                               │
  └────────────────────────────────────────────────────────────┘

  下一轮调度时, Scheduler.Schedule() 就能拾取这 4 个新 Ready 的任务
```

### 5.3 失败级联 — cascadeFailure()

```
当任务 T 永久失败 (重试耗尽):

  cascadeFailure("intake")
        │
        │ downstream["intake"] = [safety-screen]
        │
        ▼
  safety-screen:
    → 标记为 Cancelled
    → 递归: cascadeFailure("safety-screen")
        │
        │ downstream["safety-screen"] = [academic-tutor, psych-coach, parent-advisor, dev-assessor]
        │
        ▼
  academic-tutor:    → Cancelled
  psych-coach:       → Cancelled
  parent-advisor:    → Cancelled
  dev-assessor:      → Cancelled
    │
    │ (递归) 它们的下游...
    │
    ▼
  action-plan:       → Cancelled
    │
    ▼
  consultation-report → Cancelled

  结果: 根任务失败, 全图取消 (6 个任务被级联取消)
```

### 5.4 三层背压 — 请求通过的完整路径

```
一个任务要执行, 必须依次通过三层背压:

                    executeTask() 开始
                          │
                          ▼
              ┌───────────────────────┐
              │ L1: TokenBucket       │
              │ Acquire()             │  ← 令牌桶限流 (RPM 控制)
              │                       │
              │ rate=10/s, capacity=5 │
              │                       │
              │ 桶里有令牌?            │
              │  ├─YES → tokens--     │  ← 立即通过
              │  └─NO → 等待令牌补充   │  ← 阻塞, sync.Cond 通知式等待
              └───────────┬───────────┘
                          │ 通过 ✓
                          ▼
              ┌───────────────────────┐
              │ L2: AdaptiveSemaphore │
              │ Acquire()             │  ← 并发控制 (AIMD 动态调整)
              │                       │
              │ current < limit?      │
              │  ├─YES → current++    │  ← 立即通过
              │  └─NO → 等待有空位     │  ← 阻塞, chan 等待
              └───────────┬───────────┘
                          │ 通过 ✓
                          ▼
              ┌───────────────────────┐
              │ Runner.Execute()      │  ← 真正执行
              │ (如调用 LLM API)       │
              └───────────┬───────────┘
                          │ 完成
                          ▼
              ┌───────────────────────┐
              │ ReleaseConc(success)  │
              │                       │
              │ success=true?         │
              │  ├─YES → limit++      │  ← 加性增加 (+1), 缓慢扩容
              │  └─NO  → limit/=2     │  ← 乘性降低 (/2), 快速收缩
              └───────────────────────┘

  第三层 (L3: QueueDepth) 在调度端:
    Scheduler 调用 ResourceFilter.Filter()
    → 检查当前 Ready 队列是否超过上限
    → 超过则排除该任务, 不派发
```

### 5.5 黑板 — 数据如何在任务间传递

```
任务 A 完成 → 写黑板 → 任务 B 读取 A 的输出:

  1. handleDone(taskA) 成功:
     └── bb.Write("intake/output", "审核结果内容", WriteMeta{...})
         ├── entries["intake/output"] = Entry{value, version=1, author, ...}
         └── 通知所有 Watch("intake/") 的监听者

  2. 下一轮, 任务 B (academic-tutor) 被调度:
     └── LLMRunner.Execute(taskB, bb):
         ├── resolveTemplate(prompt):
         │   ├── 遇到 "{prev_result}" → 读取所有上游输出
         │   │   └── bb.Read("intake/output")
         │   │       └── entries["intake/output"].Value = "审核结果内容"
         │   │
         │   └── 遇到 "{dep:intake}" → 读取指定上游输出
         │       └── bb.Read("intake/output")
         │           └── 返回 "审核结果内容"
         │
         └── LLM 调用: systemPrompt + 替换后的 userPrompt → 输出

  3. 任务 C 完成后:
     └── bb.Write("academic-tutor/output", "辅导内容", ...)
         ├── version=2 (递增)
         └── 触发 Watch 事件

  数据隔离: 使用 "taskID/output" 命名空间, 避免冲突
  版本控制: 每次 write version++, 可用于冲突检测
  持久化: 可选 2s debounce 落盘 (原子写: tmp → rename)
```

---

## 六、Runner 全景 — 执行器家族手册

本节梳理引擎中所有 Runner 类型, 包括它们的职责、适用场景、相互关系和运作时序。

### 6.1 Runner 家族总览表

| Runner | 文件 | 定位 | 复杂度 | 典型场景 |
|--------|------|------|--------|----------|
| **LLMRunner** | llm_runner.go | LLM API 调用封装 | ⭐ | 单步 AI 任务: 审核、生成、摘要 |
| **AdversarialRunner** | llm_runner.go | 多角色对抗循环 | ⭐⭐⭐⭐ | 代码生成→多角色审查→迭代改进 |
| **CompositeRunner** | runner.go | 通用组合迭代执行 | ⭐⭐⭐ | 任何需要多步骤迭代循环的场景 |
| **FuncRunner** | runner.go | Go 函数包装器 | ⭐ | 快速原型、测试桩、本地计算 |
| **NoopRunner** | runner.go | 空操作 / 同步点 | ⭐ | DAG 汇合节点、占位符 |
| **PooledRunner** | llm_runner.go | 并发池装饰器 | ⭐⭐ | 按角色控制特定 Runner 的并发度 |

### 6.2 Runner 继承/组合关系图

```
                    ┌──────────────────┐
                    │  TaskRunner 接口  │
                    │  Name() string   │
                    │  Execute(...)     │
                    └──────┬───────────┘
                           │ implements
              ┌────────────┼────────────────┬────────────┐
              │            │                │            │
              ▼            ▼                ▼            ▼
        ┌──────────┐ ┌──────────┐    ┌──────────┐ ┌──────────┐
        │LLMRunner │ │FuncRunner│    │NoopRunner│ │Composite │
        │(LLM调用)  │ │(函数包装) │    │(空操作)   │ │(组合迭代) │
        └──────────┘ └──────────┘    └──────────┘ └────┬─────┘
                                                       │
                                                       │ AdversarialRunner 是
                                                       │ CompositeRunner 的特化版:
                                                       │ - generator + reviewers
                                                       │ - 质量评分自适应终止
                                                       │ - 反馈注入机制
                                                       ▼
                                                ┌──────────────┐
                                                │Adversarial   │
                                                │Runner        │
                                                │(对抗循环)     │
                                                └──────────────┘

              ┌──────────────────────────────────────┐
              │  装饰器模式 (包装任何 TaskRunner)      │
              │                                      │
              │  ┌──────────────┐   wraps   ┌───────┐│
              │  │PooledRunner  │───────────▶│ 任意  ││
              │  │(并发池管控)  │           │Runner ││
              │  └──────────────┘           └───────┘│
              └──────────────────────────────────────┘
```

### 6.3 每个 Runner 详解

#### ① LLMRunner — 基础 LLM 调用执行器

```go
NewLLMRunner(name, llmClient)
```

- **作用**: 调用 LLM API, 从 task.Config 读取 system_prompt / user_prompt
- **黑板交互**: 读取上游输出 (`bb.Read`) 替换模板变量 `{prev_result}` / `{dep:taskID}`
- **输出**: LLM 原始文本响应 (字符串)
- **何时使用**:
  - 需要 LLM 执行单次任务的场景: 内容审核、文本生成、摘要、翻译等
  - 任务之间无迭代反馈关系, 就是 "输入 → LLM → 输出"
- **示例**: 工作流中的 `intake` (内容审核)、`action-plan` (行动计划) 等独立 LLM 调用任务
- **注意事项**: 通过 `RunnerPool` 控制 RPM 并发, 否则容易触发限流

#### ② AdversarialRunner — 多角色对抗循环执行器 ✅

```go
NewAdversarialRunner(name, generator, []reviewers, qualityPolicy, maxRounds)
```

- **作用**: 实现**真正的多角色对抗循环**, 带反馈回路和自适应终止。这是你问的**多角色对抗 Runner**
- **核心机制**:
  - **Generator**: 生成器 (如 coder、writer), 产出初版内容
  - **Reviewers**: 多个审查者 (如 reviewer、skeptic、red-team), 可配置多个角色从不同视角评审。当前实现是**串行执行** (保证确定性), 但有审查者可并行的潜力 (见审查发现 #4)
  - **QualityTermination**: 质量评分自适应终止策略, 支持三种终止条件:
    1. 质量达标 (分数 >= 阈值)
    2. 收敛检测 (连续 3 轮变化 < 阈值)
    3. 退化检测 (连续 2 轮下降)
  - **反馈注入**: 每轮审查意见通过 `adversarial_feedback` 和 `adversarial_round` 注入到 Generator 的下轮输入中。
  - **轮间写入**: 使用 `WritableBlackboard` 在 `adv/{taskID}/round-N/output` 和 `adv/{taskID}/round-N/feedback` 写入中间状态。
- **何时使用**:
  - 代码生成 + 多角色审查 (生成→审查→改进→再审查...)
  - 内容创作 + 质量把关 (写作→审稿→修改→定稿...)
  - 任何需要 "生成-评估-反馈-改进" 循环的场景。
- **与 CompositeRunner 的区别**:
  - AdversarialRunner 是 CompositeRunner 的特化版, 专为对抗循环优化。
  - CompositeRunner 是通用的 "步骤A → 步骤B → ..." 迭代, 而 AdversarialRunner 有明确的 generator/reviewer 角色划分和反馈注入机制。
  - AdversarialRunner 有质量评分终止策略, CompositeRunner 的终止策略由调用方自定义。
- **注意区分**: `pkg/agent/adversarial.go` 中也有一个同名 `AdversarialRunner`, 但它是** agent 层的类型**, 实现了不同的接口 (`Execute(objective, team) → Outcome`), 不属于 `TaskRunner` 体系。两者设计理念类似 (生成→评估→反馈→迭代), 但 orchestator 版本更通用, 支持多 Reviewer 角色和 LLM 质量评分终止策略。

#### ③ CompositeRunner — 通用组合迭代执行器

```go
NewCompositeRunner(name, []steps, policy, maxIter)
```

- **作用**: 按顺序链式调用多个 Runner, 循环迭代执行, 直到终止策略判定或达到最大轮数。
- **何时使用**:
  - 需要多个步骤按顺序迭代执行的场景 (如: 编译 → 测试 → 修复 → 编译 → 测试 ...)
  - 不局限于 LLM 对抗, 任何多步骤循环都可以用它。
- **终止策略**: 通过 `TerminationPolicy` 接口自定义, 内置 `MaxIterTermination` (固定轮数)
- **注意**: 所有步骤共享同一个 `ReadOnlyBlackboard`, 步骤之间通过黑板共享状态。
- **任何一步失败, 整个复合任务立即失败。**
- **示例用法**:
  ```go
  steps := []TaskRunner{coder, reviewer, tester}
  policy := &MaxIterTermination{Max: 5}
  cr := NewCompositeRunner("build-loop", steps, policy, 5)
  ```

#### ④ FuncRunner — Go 函数包装器

```go
NewFuncRunner(name, func(ctx, task, bb) (any, error) { ... })
```

- **作用**: 将任意 Go 函数包装为 TaskRunner
- **何时使用**:
  - 快速原型开发, 不想写完整 Runner 类型时。
  - 测试桩 (mock runner)
  - 本地计算任务 (如数据处理、文件操作等非 LLM 任务)
- **示例**:
  ```go
  runner := NewFuncRunner("data-process", func(ctx context.Context, task *Task, bb ReadOnlyBlackboard) (any, error) {
      val, _, _ := bb.Read("input/data")
      result := processData(val)
      return result, nil
  })
  ```

#### ⑤ NoopRunner — 空操作 / 同步点

```go
&NoopRunner{}
```

- **作用**: 什么都不做, 立即返回成功。引擎启动时自动注册。
- **何时使用**:
  - DAG 中的汇合/同步节点 (如: A 和 B 都完成后才继续, 但不需要执行任何逻辑)
  - 占位符 (先定义图结构, 后续再替换 runner)
- **注意**: NoopRunner 不消耗任何资源, 执行时间为 0。
- **示例**: 如果需要一个 "等所有测试都通过后再继续" 的汇合点, 可以用 NoopRunner 作为纯依赖节点。

#### ⑥ PooledRunner — 并发池装饰器 (装饰器模式)

```go
NewPooledRunner(innerRunner, runnerPool)
```

- **作用**: 包装任何 TaskRunner, 为其添加按角色 (runner name) 的并发控制。
- **何时使用**:
  - 某个 Runner 类型有并发上限 (如 LLM 调用受 RPM 限制)
  - 同一种 Runner 的多个实例共享并发池。
- **与全局 Backpressure 的区别**:
  - Backpressure (TokenBucket + AdaptiveSemaphore) 是全局的, 所有任务共享。
  - RunnerPool 是按 Runner 角色独立控制的, 比如 `llm` 角色限并发 5, `data-process` 角色不限。
- **示例**:
  ```go
  pool := NewRunnerPool()
  pool.SetLimit("llm", 5)   // LLM 类任务最多 5 并发。
  pool.SetLimit("data", 0)  // 数据类任务不限制。
  pooledLLM := NewPooledRunner(baseLLM, pool)
  eng.Runners().Register(pooledLLM)
  ```

### 6.4 有没有 "Planner" 这种能派生新 DAG 执行计划的 Runner?

**答: 有类似能力, 但不是通过 Runner 实现的, 而是通过 `DynamicExpander` + `ExpanderHook` 机制。**

引擎中存在一个专门的 **动态 DAG 扩展接口**: `DynamicExpander`, 它配合 `ExpanderHook` (实现了 `LifecycleHook` 接口) 使用, 在任务完成时根据输出动态添加新任务和新边。
```
  DynamicExpander 接口 (observer.go):
    OnTaskComplete(task *Task, output any) → ([]*Task, []Edge)
  
  ExpanderHook 钩子 (observer.go):
    当 OnTaskComplete 被触发时:
      1. 调用 expander.OnTaskComplete() 获取新任务和边。
      2. graph.AddTask() 添加新任务。
      3. graph.AddEdge() 添加新依赖边。
      4. graph.Build() 重新验证拓扑。
      5. 引擎主循环下一轮调度时, 新任务自动参与调度。
```

而 `LLMExpander` 就是 `DynamicExpander` 的一个具体实现:
```
  LLMExpander 工作流程 (llm_runner.go):
    1. 任务完成时, 检查 task.Config["expandable"] 是否为 true
    2. 将任务输出发送给 LLM 分析
    3. LLM 判断是否需要拆分子任务, 返回新的 Task 列表
    4. ExpanderHook 将新任务注入图中, 引擎继续调度。
```

**典型场景**:
- 测试团队: 测试计划任务 → LLM 分析后拆分为多个具体测试函数任务 → 分别执行。
- 代码审查: 分析任务 → LLM 决定需要哪些专项审查 (安全审查、性能审查等) → 动态创建子任务。

**但是, 引擎中没有一个独立的 "PlannerRunner" 类型。** Planner 的能力是通过 `LLMExpander` (动态图扩展) + 任务的 `expandable` 标记组合实现的, 它不是一个 Runner, 而是一个 **生命周期钩子 (LifecycleHook)**。

### 6.5 Runner 完整运作时序图 (以 AdversarialRunner 为例)

```
  Engine.Run() 主循环        AdversarialRunner         Generator (LLMRunner)    Reviewers (×N)       Blackboard
        │                          │                          │                       │                    │
        │ 派发 adversarial task    │                          │                       │                    │
        │─────────────────────────▶│                          │                       │                    │
        │                          │                          │                       │                    │
        │                          │ Round 1 开始             │                       │                    │
        │                          │                          │                       │                    │
        │                          │ 1. Generator.Execute()   │                       │                    │
        │                          │─────────────────────────▶│                       │                    │
        │                          │                          │ LLM 调用, 生成初版     │                    │
        │                          │                          │◀──────────────────────│                    │
        │                          │                          │                       │                    │
        │                          │ 2. 将生成结果写入黑板     │                       │                    │
        │                          │─────────────────────────────────────────────────────────────────────▶│
        │                          │                          │ bb.Write("adv/round-1/output", genOutput) │
        │                          │                          │                       │                    │
        │                          │ 3. 遍历 Reviewers (串行)  │                       │                    │
        │                          │                          │                       │                    │
        │                          │ 3a. Reviewer[0].Execute()│                       │                    │
        │                          │────────────────────────────────────────────────▶│                    │
        │                          │                          │                       │ LLM 评审, 返回意见  │
        │                          │                          │                       │◀───────────────────│
        │                          │                          │                       │                    │
        │                          │ 3b. Reviewer[1].Execute()│                       │                    │
        │                          │─────────────────────────────────────────────────────────────────▶│    │
        │                          │                          │                       │ LLM 评审, 返回意见  │
        │                          │                          │                       │◀────────────────────│
        │                          │                          │                       │                    │
        │                          │ 4. 合并所有评审意见        │                       │                    │
        │                          │ mergedFeedback = join(reviews)                   │                    │
        │                          │─────────────────────────────────────────────────────────────────────▶│
        │                          │                          │ bb.Write("adv/round-1/feedback", merged)  │
        │                          │                          │                       │                    │
        │                          │ 5. QualityTermination.    │                       │                    │
        │                          │    ShouldTerminate()      │                       │                    │
        │                          │    ├─ 质量达标? → break   │                       │                    │
        │                          │    ├─ 收敛? → break       │                       │                    │
        │                          │    └─ 退化? → break       │                       │                    │
        │                          │                          │                       │                    │
        │                          │ Round 2 开始 (未终止时)   │                       │                    │
        │                          │ 注入反馈:                 │                       │                    │
        │                          │ task.Config["adversarial_feedback"] = merged     │                    │
        │                          │ task.Config["adversarial_round"] = 2             │                    │
        │                          │                          │                       │                    │
        │                          │ 1. Generator.Execute()   │                       │                    │
        │                          │    (携带上轮反馈)          │                       │                    │
        │                          │─────────────────────────▶│                       │                    │
        │                          │                          │ LLM 调用, 改进版       │                    │
        │                          │                          │◀──────────────────────│                    │
        │                          │ ... (重复上述流程)        │                       │                    │
        │                          │                          │                       │                    │
        │                          │ 终止条件满足, 跳出循环    │                       │                    │
        │                          │◀─────────────────────────│                       │                    │
        │                          │                          │                       │                    │
        │ 返回最终输出 (lastOutput) │                          │                       │                    │
        │◀─────────────────────────│                          │                       │                    │
        │                          │                          │                       │                    │
        │ handleDone(): 标记完成, 写入黑板, 解锁下游           │                       │                    │
```

### 6.6 Runner 选用决策树 (什么时候用哪个?)
```
  你的任务需要什么?
  │
  ├─ 只是调用 LLM, 拿到结果就完事?
  │   └─ YES → 使用 LLMRunner
  │
  ├─ 需要多个步骤按顺序循环执行, 并且需要自定义终止条件?
  │   └─ YES → 使用 CompositeRunner
  │
  ├─ 需要 "生成→审查→改进" 的多角色对抗循环?
  │   └─ YES → 使用 AdversarialRunner
  │           (它比 CompositeRunner 多了反馈注入和质量评分终止)
  │
  ├─ 只是想快速包装一个 Go 函数? (测试、原型、本地计算)
  │   └─ YES → 使用 FuncRunner。
  │
  ├─ 需要一个不执行任何操作的同步点/汇合节点?
  │   └─ YES → 使用 NoopRunner
  │
  └─ 需要控制某个 Runner 的并发度 (比如 LLM 限流)?
      └─ YES → 用 PooledRunner 包装你的 Runner。
```

### 6.7 Runner 与引擎其他组件的关联矩阵。
```
  Runner             │ Engine │ Scheduler │ Blackboard │ Backpressure │ Hooks │ DynamicExpander
  ──────────────────┼────────┼───────────┼────────────┼──────────────┼───────┼─────────────────
  LLMRunner          │   ✅   │    ✅      │   Read ✅   │   RPM ✅      │  ✅    │ LLMExpander ✅
  AdversarialRunner  │   ✅   │    ✅      │ Write ✅    │   RPM ✅      │  ✅    │  N/A
  CompositeRunner    │   ✅   │    ✅      │   Read ✅   │   RPM ✅      │  ✅    │  N/A
  FuncRunner         │   ✅   │    ✅      │   Read ✅   │   RPM ✅      │  ✅    │  N/A
  NoopRunner         │   ✅   │    ✅      │   N/A      │   N/A        │  ✅    │  N/A
  PooledRunner       │   ✅   │    ✅      │   Read ✅   │   Pool ✅     │  ✅    │  N/A
```

---

## 七、审查发现

### 🔴 严重问题

#### 1. `engine.go`: `checkStall` 检测间隔硬编码 30s

**位置**: `engine.go` 主循环
**问题**: 配置 `StallTimeout=5min` 但检测间隔硬编码 30s，不读配置。
**修复**: ✅ 已改为 `StallTimeout / 10`，最少 5s。

#### 2. `engine.go`: `drain` 循环可能遗漏部分完成信号

**问题**: 非阻塞 drain 只能处理已到达通道缓冲区的信号。高并发下，正在发送的信号可能被遗漏。
**修复**: ✅ 已添加 5ms timeout drain 排空。

### 🟡 中等问题

#### 3. `backpressure.go`: `TokenBucket.Acquire()` busy-loop

**问题**: busy-wait loop 高并发时不精确。
**修复**: ✅ 改用 `sync.Cond` 通知式等待。

#### 4. `llm_runner.go`: `AdversarialRunner` 审查者串行

**问题**: 注释说"保证确定性串行"，但实际上审查者之间无数据依赖，可并行。
**状态**: ⏸ 保留 (设计决策，后续可加配置开关)

#### 5. `errors.go`: `math/rand` 种子未初始化

**问题**: 默认种子=1，多实例退避模式同步。
**修复**: ✅ `init()` 初始化种子。

#### 6. `graph.go`: Kahn 算法代码重复 3 处

**问题**: `Build()`, `CriticalPath()`, `DAGWidth()` 各自实现，维护成本高。
**状态**: ⏸ `DAGWidth()` 需分层 BFS，逻辑不同，提取反而更复杂。

### 🟢 轻微问题

#### 7. `blackboard.go`: Watch 通道满时丢弃事件无日志

**修复**: ✅ 添加 `log.Printf` 警告。

#### 8. `tool.go`: 异步执行错误被忽略

**修复**: ✅ 添加 `log.Printf` 记录。

#### 9. `llm_runner.go`: `LLMExpander` prompt 缺少最大输出限制

**修复**: ✅ prompt 中加入 `maxExpand` 约束。

---

## 八、测试覆盖评估

| 文件 | 测试文件 | 覆盖情况 |
|------|----------|----------|
| engine.go | engine_test.go | ✅ 基本执行流 |
| graph.go | graph_test.go | ✅ 构建+环路+关键路径 |
| scheduler.go | scheduler_test.go | ✅ 调度+评分 |
| backpressure.go | backpressure_test.go | ✅ 背压+AIMD |
| errors.go | errors_test.go | ✅ 错误分类+重试 |
| blackboard.go | blackboard_test.go | ✅ 读写+持久化 |
| runner.go | — | ⚠️ 被间接覆盖 |
| llm_runner.go | llm_runner_test.go | ✅ LLM runner |
| hooks.go | — | ⚠️ 太简单无测试 |
| checkpoint.go | — | ⚠️ 无独立测试 |
| observer.go | observer_test.go | ✅ 指标收集 |
| tool.go | tool_test.go | ✅ JSON 接口 |

---

## 九、扩展指南

### 想加 X 功能，改哪里

| 需求 | 改哪里 |
|------|--------|
| 添加新任务类型 (如 Docker 执行) | `runner.go`: 实现 `TaskRunner` 接口，`Register` |
| 自定义调度策略 (按标签过滤) | `scheduler.go`: 实现 `FilterPlugin` 或 `ScorePlugin` |
| 添加飞书/钉钉通知 | `hooks.go`: 实现 `LifecycleHook`，`SetHook` |
| 增加 Prometheus 指标 | `observer.go`: 实现 `Observer`，`NewMetricsHook` |
| 接入外部数据库检查点 | `checkpoint.go`: 实现 `CheckpointStore` |

### 已知的坑

1. **`CriticalPathScore` 只计算一次** — `sync.Once` 导致 DAG 动态扩展后不会重算
2. **`AdversarialRunner` 需要 `WritableBlackboard`** — 默认 `ReadOnlyBlackboard` 接口不够
3. **`ToolEngine` 异步执行忽略 error** — 已修复为日志记录
4. **背压 RPM 令牌桶初始满桶** — 启动时允许突发

---

## 十、修复记录

| # | 级别 | 问题 | 修复方案 | 文件 |
|---|------|------|----------|------|
| 1 | 🔴 | `checkStall` 间隔硬编码 | 改为 `StallTimeout / 10` | `engine.go` |
| 2 | 🔴 | `drain` 遗漏信号 | 添加 5ms timeout drain | `engine.go` |
| 3 | 🟡 | `TokenBucket` busy-loop | 改用 `sync.Cond` | `backpressure.go` |
| 4 | 🟡 | `math/rand` 种子未初始化 | `init()` 初始化 | `errors.go` |
| 5 | 🟢 | Watch 丢弃无日志 | 添加 `log.Printf` | `blackboard.go` |
| 6 | 🟢 | 异步错误吞没 | 添加错误日志 | `tool.go` |
| 7 | 🟢 | LLMExpander 无输出限制 | prompt 加约束 | `llm_runner.go` |

**测试验证**: `go test ./pkg/orchestrator/... -count=1` ✅ 全部通过
