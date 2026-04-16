# 自动进化系统改进方案

> 参考 MiniMax M2.7 自进化循环, GLM 5.1 异步 Agent RL, DeepSeek 二值奖励

## 一、当前进化系统分析

### 现有流程
```
RecordTrajectory → LearnFromTeam(LLM/heuristic distill) → RetrieveFor(BM25) → RecordFeedback(EMA) → Consolidate
```

### 核心问题
1. **单向学习**: 仅 failure 触发 LearnFromStage, 成功阶段被忽略
2. **自由文本经验**: 结构松散, 检索精度低
3. **BM25 检索**: 无语义泛化, "错误处理" 搜不到 "error handling"
4. **无效果追踪**: 注入经验后不知道是否有帮助
5. **O(n²) 合并**: Consolidate 的 Jaccard 相似度两两比较
6. **无反事实学习**: 不知道 "如果采用另一种方法会怎样"

## 二、改进设计

### 2.1 双向学习 (成功+失败)

```go
// 现有: 仅 failure
func (ee *EvolutionEngine) LearnFromStage(traj Trajectory) {
    if !traj.Success { ... } // 只学失败
}

// 改进: 双向学习
func (ee *EvolutionEngine) LearnFromStage(traj Trajectory) {
    if traj.Success {
        // 提炼 what-worked: 方法摘要, 关键决策, 代码模式
        ee.distillSuccess(traj)
    } else {
        // 提炼 what-failed: 根因, 错误模式, 修复方向
        ee.distillFailure(traj)
    }
}
```

### 2.2 结构化经验 (替代自由文本)

```go
type StructuredExperience struct {
    Pattern       string   // "error-handling", "api-design", "testing"
    Context       string   // "Go HTTP handler", "React component"
    Strategy      string   // 具体做法
    Evidence      string   // "3 successes, 0 failures"
    Antidote      string   // 反面教训 (什么不该做)
    SuccessCount  int
    FailureCount  int
    LastUsed      time.Time
}
```

### 2.3 经验效果追踪 (A/B)

```go
type ExperienceInjection struct {
    ExperienceIDs []string  // 注入了哪些经验
    TaskID        string    // 对应的任务
    Injected      bool      // 是否实际注入
    Outcome       bool      // 任务成功/失败
}

// 计算经验 uplift = P(success|injected) - P(success|not_injected)
func (ee *EvolutionEngine) ComputeUplift(expID string) float64
```

### 2.4 失败轨迹反例库 (MiniMax 风格)

```go
type FailureCase struct {
    Symptom     string    // 表现症状 (编译错误/测试失败/...)
    RootCause   string    // 根因分析
    BadApproach string    // 错误方法 (不该做什么)
    FixStrategy string    // 修复方向
    MetricDelta float64   // 修复前后指标变化
    CreatedAt   time.Time
}
```

### 2.5 经验注入限流 (Bandit 选择)

```go
// 不是注入所有匹配经验, 而是 bandit 选择 top-K
// 基于 UCB (Upper Confidence Bound):
// score = quality + sqrt(2 * ln(total_uses) / exp_uses)
func (ee *EvolutionEngine) SelectExperiences(candidates []*Experience, budget int) []*Experience
```

## 三、实施步骤

1. `LearnFromStage` 双向学习 (成功也提炼)
2. 经验注入后记录 injectionID, task 结束时 RecordInjectionOutcome
3. `Consolidate` 增加成功/失败计数合并
4. Dreaming 触发时同步进化 consolidate
