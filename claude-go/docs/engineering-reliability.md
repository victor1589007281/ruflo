# 群体智能引擎工程可靠性优化方案 v1.0

> 2026-04-17 | claude-go/pkg/swarm_intel

## 1. 问题域与调研背景

### 1.1 当前痛点（9 种模拟全量运行实测）


| 问题                                                      | 影响                         | 波及范围                                         |
| ------------------------------------------------------- | -------------------------- | -------------------------------------------- |
| **上下文膨胀** — 多轮对话/辩论中 LLM 响应直接拼入下一轮 prompt               | social/montecarlo 超时(5min) | engine.go debateRound, simulation.go 全 9 种模式 |
| **JSON 解析脆弱** — LLM 输出 markdown 包裹/多 JSON 块/schema 偏离   | 2/9 模式场景=0                 | extractJSON 所有调用点                            |
| **无重试/限流** — 429/503 直接失败或 continue                     | API 限流即全链路失败               | 全部 SimpleComplete 调用                         |
| **串行 LLM 调用** — scout/predict/debate/montecarlo 各分支顺序执行 | 延迟线性叠加(300s+)              | engine.go + simulation.go                    |
| **超时控制缺失** — 无 per-call timeout, 依赖外部 ctx               | 单次 LLM 卡住则全流水线超时           | 全包                                           |


### 1.2 业界参考


| 来源                                                                         | 可借鉴设计                                                                             |
| -------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| **Claude Opus 4.6/4.7** — Structured Output + Grammar Constrained Decoding | 双层校验: wire-schema(模型侧) + app-schema(本地 Unmarshal); 失败分级: 语法修复→缩小 schema 重试→非结构化降级 |
| **Anthropic Prompt Caching**                                               | system+tools 稳定前缀缓存; 避免 thinking 参数变化导致 cache miss                                |
| **DeepSeek API Rate Limit**                                                | keep-alive 心跳感知; 10min 无推理即断连; 区分"排队中"与"真失败"                                      |
| **Qwen 3.6 Agent (qwen-code)**                                             | retry.ts 独立重试层: 7 次指数退避, 429/5xx 专轨, retry budget 按错误族隔离                          |
| **Kimi 128K/256K**                                                         | 输出上限 = context − prompt 约束; Partial 分块续写; token 预估+截断策略                           |
| **MiniMax M2.7**                                                           | 多模态链路: 网关先做载荷规范化, 再灌装文本上下文                                                        |
| **通用 Fan-out/Fan-in**                                                      | 有界 worker pool + per-request semaphore + 分支超时 + 总超时                               |


## 2. 架构设计

### 2.1 可靠性组件栈

```
┌─────────────────────────────────────────────────────┐
│                   Engine / Simulator                  │
├────────┬────────┬──────────┬──────────┬──────────────┤
│Context │Robust  │Resilient │Parallel  │Timeout       │
│Compress│JSON    │Caller    │Dispatcher│Budget        │
│Manager │Parser  │(重试/熔断)│(Fan-out) │(分级超时)    │
├────────┴────────┴──────────┴──────────┴──────────────┤
│                  LLMClient (SimpleComplete)            │
└───────────────────────────────────────────────────────┘
```

### 2.2 五大组件

#### 组件 A: `ContextCompressor` — 上下文压缩管理器

**核心理念**: 参考 Kimi 的 token 预估+截断策略和 Anthropic 的 prompt caching 分层思想。

```go
type ContextCompressor struct {
    maxPromptChars   int     // prompt 总字符上限 (默认 6000)
    maxSingleField   int     // 单字段最大字符 (默认 500)
    maxHistoryItems  int     // 历史列表最大条数 (默认 3)
    compressionRatio float64 // 摘要压缩比 (默认 0.3)
}
```

**方法**:

- `TruncateField(s string, hint string) string` — 单字段截断, 保留首尾关键信息
- `CompressHistory(items []string, maxTotal int) []string` — 只保留最近 N 条, 每条截断
- `CompressEvidence(evidence []string, maxTotal int) string` — 去重 + 截断 + 编号
- `CompressDebateView(predictions []AgentPrediction) string` — 辩论视图压缩: 只保留概率分布+摘要推理(≤80字)
- `BuildBudgetedPrompt(parts map[string]string, maxTotal int) string` — 按优先级分配 token 预算

**解决问题**:

- engine.go `debateRound` 的 `otherViews` 和 `Rationale` 无限增长
- engine.go `evidence` 的 `strings.Join` 无上限拼接
- simulation.go `allRounds` 随轮次线性增长
- engine.go `reasoningBank` 的 `entry.Reasoning` 可能很长
- simulation.go `parseSimulationResult` 失败时 `raw` 全文进 `Summary`

#### 组件 B: `RobustJSONParser` — 健壮 JSON 解析器

**核心理念**: 参考 Anthropic 的 Grammar Constrained Decoding + 社区 JSON repair 方案。

```go
type ParseResult[T any] struct {
    Value   T
    OK      bool
    Method  string // "direct" | "extracted" | "multi_merged" | "repaired" | "fallback"
    RawJSON string
}

type RobustJSONParser struct {
    // 配置
    enableRepair    bool // 尝试修复 JSON 语法错误
    enableMulti     bool // 支持提取多个 JSON 对象
    strictMode      bool // 严格 schema 校验
}
```

**解析管线** (fallback chain):

1. 直接 `json.Unmarshal(raw)` — 最快路径
2. `stripMarkdown(raw)` → `json.Unmarshal` — 去 `json` 包裹
3. `extractJSON(stripped)` → `json.Unmarshal` — 提取第一个 `{...}`
4. `repairJSON(extracted)` → `json.Unmarshal` — 修复尾逗号/缺引号/截断
5. `extractAllJSON(stripped)` → 逐个解析合并 — 多 JSON 块场景
6. 返回零值 + `ok=false` — 全部失败

**JSON 修复策略**:

- 尾逗号: `",}` → `"}`
- 截断闭合: 补全缺失的 `}` / `]`
- 单引号: `'key': 'val'` → `"key": "val"`

#### 组件 C: `ResilientCaller` — LLM 调用弹性层

**核心理念**: 参考 Qwen-code 的分层 retry budget, DeepSeek 的 keep-alive 心跳, Anthropic SDK 的 429/5xx 退避。

```go
type ResilientCaller struct {
    inner       LLMClient
    maxRetries  int           // 最大重试次数 (默认 3)
    baseDelay   time.Duration // 基础退避 (默认 2s)
    maxDelay    time.Duration // 最大退避 (默认 30s)
    perCallTTL  time.Duration // 单次调用超时 (默认 90s)

    // 熔断器状态
    consecutiveFails int
    circuitOpen      bool
    circuitOpenUntil time.Time

    // 可观测
    notify NotifyFunc
    stats  CallerStats
}

type CallerStats struct {
    TotalCalls   int64
    Retries      int64
    Failures     int64
    CircuitTrips int64
    AvgLatencyMs int64
}
```

**错误分类**:


| HTTP 码      | 分类   | 策略                                      |
| ----------- | ---- | --------------------------------------- |
| 429         | 限流   | 指数退避(2s→4s→8s) + jitter, 尊重 Retry-After |
| 500         | 服务器  | 立即重试 1 次, 之后退避                          |
| 503         | 过载   | 长退避(5s→15s→30s)                         |
| 408/timeout | 超时   | 缩短 prompt 后重试 (配合 ContextCompressor)    |
| 400         | 请求错误 | 不重试, 直接返回                               |


**熔断机制**: 连续 5 次失败 → 熔断 30s → 半开尝试 → 成功则恢复

#### 组件 D: `ParallelDispatcher` — 并行调度器

**核心理念**: 已有 `fanout.go` 提供 `FanOutCollect` / `FanOutFirstN`, 需要扩展并集成到业务层。

**可并行化清单** (审计结果):


| 位置                                 | 当前   | 改造            | 预期收益   |
| ---------------------------------- | ---- | ------------- | ------ |
| `engine.go:430-463` scout          | 串行循环 | FanOutCollect | 2x     |
| `engine.go:488-522` predict        | 串行循环 | FanOutCollect | 3-4x   |
| `engine.go:588-628` debateRound    | 串行循环 | FanOutCollect | 3-4x   |
| `simulation.go:194-212` montecarlo | 串行循环 | FanOutCollect | **4x** |


**并发控制策略**:

- 默认 `MaxConcurrency = 3` (避免 API 限流)
- `PerBranchTimeout = 90s`
- `TotalTimeout` = 调用者的 ctx deadline

#### 组件 E: `TimeoutBudget` — 分级超时控制

**核心理念**: 参考 Anthropic 的 task budget 和 DeepSeek 的分层 deadline 判定。

```go
type TimeoutBudget struct {
    totalDeadline time.Time
    phases        map[string]time.Duration // 各阶段预算
    spent         map[string]time.Duration
}
```

**预算分配** (Predict 7 阶段):


| 阶段             | 预算占比 | 默认(5min总预算) |
| -------------- | ---- | ----------- |
| decompose      | 10%  | 30s         |
| scout          | 10%  | 30s         |
| predict        | 30%  | 90s         |
| debate         | 25%  | 75s         |
| fuse+calibrate | 5%   | 15s         |
| summary        | 10%  | 30s         |
| persist        | 10%  | 30s         |


**预算分配** (Simulate 模式):


| 模式                                      | LLM 调用数    | 建议总预算 |
| --------------------------------------- | ---------- | ----- |
| social (多轮)                             | 2+rounds+1 | 3min  |
| montecarlo (并行)                         | 4+1        | 2min  |
| game/crisis/org/market/policy/tech (单次) | 1          | 90s   |
| creative (2-3次)                         | 3          | 3min  |


## 3. 实现计划

### 3.1 文件结构

```
pkg/swarm_intel/
├── reliable.go          # ContextCompressor + RobustJSONParser + TimeoutBudget
├── resilient_caller.go  # ResilientCaller (LLM 弹性层)
├── fanout.go            # ParallelDispatcher (已有, 扩展)
├── engine.go            # 集成所有组件
└── simulation.go        # 集成所有组件
```

### 3.2 集成方式

**Before** (当前):

```go
resp, err := e.llm.SimpleComplete(ctx, system, prompt)
if err != nil {
    continue
}
jsonStr := extractJSON(resp)
json.Unmarshal([]byte(jsonStr), &parsed)
```

**After** (改造后):

```go
// 1. 上下文压缩
prompt := cc.BuildBudgetedPrompt(parts, 6000)

// 2. 弹性调用 (自动重试/限流/熔断)
resp, err := rc.Call(ctx, system, prompt)
if err != nil {
    // 已重试多次仍失败
    continue
}

// 3. 健壮 JSON 解析 (多级 fallback)
result := jp.Parse[MyStruct](resp)
if !result.OK {
    // 降级处理
}
```

## 4. 验收标准


| 指标              | 当前         | 目标    |
| --------------- | ---------- | ----- |
| social 场景输出     | 0 (超时)     | ≥2 场景 |
| montecarlo 场景输出 | 0→2 (修复后)  | ≥3 场景 |
| 平均模拟延迟          | 144.8s     | ≤90s  |
| JSON 解析成功率      | ~78% (7/9) | ≥95%  |
| 429 恢复率         | 0%         | ≥90%  |
| 上下文膨胀导致超时       | 2/9        | 0/9   |


