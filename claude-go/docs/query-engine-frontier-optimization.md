# QueryEngine 前沿优化方案 v1.0

> 2026-04-18 | `claude-go/pkg/engine`
>
> 结合 **Claude 4.6/4.7, GLM 5.1, Kimi K2.5 / K2 Thinking, MiniMax M2.7, DeepSeek V3 / Prover V2 / R1, Qwen 3.6, GPT 5.6, Gemini Gamma 4, MAgICoRe, CaRT, Mem0, Letta** 等设计与论文,对当前从 Claude client 源码迁移而来的 QueryEngine 进行系统性优化。

---

## 0. TL;DR

当前 `engine.queryLoop` 忠实复刻了 Claude Code 客户端的 ReAct 循环。它实现了:

1. **ReAct 主循环** (think → tool → think)
2. **AutoCompact + MicroCompact** (两级上下文压缩)
3. **熔断器** (连续 5 次错误 → 停止)
4. **PTL 反应式压缩** (`prompt_too_long` → 压缩重试)
5. **Fallback Model** (主模型出错切备用)
6. **Withheld Error** (错误暂存继续等待恢复)
7. **max_tokens 自动续写** (模型截断时注入 "Continue...")
8. **Streaming** (token-by-token)
9. **DisabledTools 过滤** (飞书场景 LLM 自主调用拦截)
10. **记忆首轮注入** (TieredStore retrieve → system prompt)

**还有 10 个前沿能力未吸收**,它们分别对标各家论文中已经稳定落地的机制。本方案把它们设计为**可插拔组件**,在不破坏现有循环的前提下逐步引入。

---

## 1. 现状审视:当前循环的 10 个隐性缺口

| # | 缺口 | 影响 | 对标前沿 |
|---|---|---|---|
| **G1** | **Prompt 非 cache-friendly** — 每轮重新拼装系统提示 + 工具定义 + 记忆,未做稳定前缀分离 | cache_hit_rate ≈ 0,token 成本 ↑ 25-40% | Anthropic Prompt Caching, OpenAI cached_tokens, Kimi 128K |
| **G2** | **无 Token 预算分级** — 只有 AutoCompact@80% 单一阈值,90% 以上没有紧急降级 | 长会话偶发 OOC / PTL 抖动 | Kimi 溢出策略 (隐藏旧 tool 输出), MemGPT 滑动窗口 |
| **G3** | **工具循环未检测** — 同工具同参数连续调用不被识别 | 模型卡在搜索-阅读死循环,吞 token | GLM 5.1 Agentic 信用分配, Kimi 防伪并行 |
| **G4** | **错误处理一刀切** — `consecutiveErrors++` 不区分 429/5xx/400/timeout/PTL | 400 也算一次熔断额度,恢复慢 | Qwen 3.6 retry.ts 分族 budget, Claude 4.7 错误分类 |
| **G5** | **工具输入 JSON 脆弱** — `input_json_delta` 累积失败无兜底修复 | 流式截断/trailing comma → tool 执行失败 | Claude 4.6 Grammar-Constrained Decoding + JSON repair |
| **G6** | **无轨迹记忆** — 只存压缩前事实,不记录"这一轮什么工作 / 什么失败" | 无法学习"哪些 tool 序列高效" | MiniMax M2.7 失败轨迹挖掘,ReasoningBank |
| **G7** | **无 CaRT 式停止信号** — 模型自己决定停,常"过度搜索" | 冗余工具调用 +20%~30% | CaRT (arxiv:2510.08517) 反事实停止 |
| **G8** | **无 Heavy Mode** — 关键决策点只跑 1 路,没有 meta-judge 聚合 | 重要架构选择质量抖动 | Kimi Heavy Mode 8 路 rollout, MAgICoRe Hard 路径 |
| **G9** | **工具执行无推测** — 必须等模型完整输出才开始,流式期间 CPU 闲置 | 端到端延迟高 20-30% | GLM 5.1 异步 RL rollout, Speculative Decoding |
| **G10** | **无指标可观测** — 没有 cache_hit / loop_count / budget_spent 等暴露点 | 优化无据可依 | DeepSeek V3 可观测架构, Anthropic 客户端 telemetry |

---

## 2. 设计原则

1. **零破坏** — 所有新组件为可选字段,`cfg.Enable*` 开关控制,默认兼容当前行为。
2. **组合式** — 每个组件单文件、接口化,可独立测试、独立开关。
3. **指标先行** — 每个组件必须暴露 Metrics,便于 A/B 效果验证。
4. **渐进降级** — 新组件故障时自动回退到基线行为,不引入新的失败面。
5. **贴近论文** — 命名、阈值、算法严格对齐论文表述,便于后续对照复核。

---

## 3. 组件蓝图

```
┌────────────────────────── QueryEngine v2 ──────────────────────────┐
│                                                                    │
│   ┌──────────────────────── queryLoop ─────────────────────────┐   │
│   │                                                            │   │
│   │   [1] PromptCacheBuilder    ← G1 稳定前缀 + hash 追踪      │   │
│   │   [2] TokenBudgetManager    ← G2 四级阈值 + 紧急降级       │   │
│   │   [3] LoopDetector          ← G3 (toolName,inputHash)×N    │   │
│   │   [4] ErrorClassifier       ← G4 HTTP 分族 + 隔离 budget   │   │
│   │   [5] JSONRepair            ← G5 fallback chain            │   │
│   │   [6] TrajectoryMemory      ← G6 每轮 {plan,tools,result}  │   │
│   │   [7] StopSignalDetector    ← G7 CaRT 反事实判定           │   │
│   │   [8] HeavyMode (opt-in)    ← G8 N 路 rollout + meta-judge │   │
│   │   [9] SpeculativeTool(opt)  ← G9 流式期间工具预取           │   │
│   │   [10] EngineMetrics        ← 全组件共享的统一指标          │   │
│   │                                                            │   │
│   └────────────────────────────────────────────────────────────┘   │
│                                                                    │
└────────────────────────────────────────────────────────────────────┘
```

### 3.1 PromptCacheBuilder (G1)

**对标**: Anthropic Prompt Caching + Kimi 前缀固定 + OpenAI cached_tokens。

**思路**: 将 prompt 分为 **静态前缀** (system + tools + project context + memory@turn0) 和 **动态后缀** (user + tool_results)。只要前缀哈希不变,就可以复用 API 缓存。

```go
type PromptCacheBuilder struct {
    prefixHash string
    cacheHits  atomic.Int64
    cacheMiss  atomic.Int64
    lastBuiltAt time.Time
}

func (b *PromptCacheBuilder) Build(staticParts, dynamicParts []string) (system []string, cacheHit bool) {
    h := sha256(strings.Join(staticParts, "\n"))
    hit := (h == b.prefixHash)
    if hit { b.cacheHits.Add(1) } else { b.cacheMiss.Add(1); b.prefixHash = h }
    return append(staticParts, dynamicParts...), hit
}
```

**收益**:
- 长会话 cache_hit_rate 预期 > 70%
- Token 成本下降 25-40%
- 指标: `prompt_cache_hit_rate`, `prompt_cache_miss_count`

### 3.2 TokenBudgetManager (G2)

**对标**: Kimi 128K/256K 溢出策略 + MemGPT 分层记忆 + Anthropic TaskBudget。

**思路**: 对当前 estimate_tokens 设定 4 级阈值,每级采取不同降级策略:

| 占比 | 等级 | 策略 |
|---|---|---|
| < 60% | **Green** | 正常运行 |
| 60-80% | **Yellow** | 启用增量摘要 (tool_result > 10KB 自动摘要) |
| 80-90% | **Orange** | 触发 AutoCompact (现有行为) |
| 90-95% | **Red** | Micro-emergency: 隐藏 3 轮之前的 tool_result,只保留 `[tool_result: 文件路径/结论]` 摘要 |
| > 95% | **Critical** | 强制 reactive compact + 拒绝新 tool_use 直到降级成功 |

```go
type BudgetLevel int
const (
    BudgetGreen BudgetLevel = iota
    BudgetYellow
    BudgetOrange
    BudgetRed
    BudgetCritical
)

type TokenBudgetManager struct {
    Budget   int
    Current  int
}

func (m *TokenBudgetManager) Level() BudgetLevel { ... }
func (m *TokenBudgetManager) Degrade(msgs []Message, level BudgetLevel) []Message { ... }
```

### 3.3 LoopDetector (G3)

**对标**: Kimi 防伪并行 + GLM 5.1 Agentic 信用分配。

**思路**: 记录最近 N 次 `(toolName, normalizedInputHash)` 调用,连续 3 次命中 → 判定死循环,注入反向 prompt:

> "系统检测到你连续 3 次调用 `<tool>`,输入相同。请:①说明你从结果中学到了什么,②采取不同的策略,或③如信息已足够,直接给出最终答复。"

```go
type LoopDetector struct {
    window      int // 默认 8
    threshold   int // 默认 3 连续相同
    history     []callSignature
    mu          sync.Mutex
}

type callSignature struct {
    Tool, Hash string
    At         time.Time
}

func (d *LoopDetector) Observe(name string, input []byte) (looped bool, suggestion string)
```

**进一步**: 检测到 `Read(file=x) → Edit(file=x) → Read(file=x) → Edit(file=x)` 这种 2-gram / 3-gram 循环。

### 3.4 ErrorClassifier (G4)

**对标**: Qwen 3.6 retry.ts + Claude 4.7 + DeepSeek 限流感知。

**思路**: 把 `consecutiveErrors` 扩展为按错误族分桶:

```go
type ErrorFamily int
const (
    ErrFamilyRateLimit ErrorFamily = iota // 429
    ErrFamilyOverload                     // 503
    ErrFamilyServer                       // 5xx
    ErrFamilyTimeout                      // 408 / context deadline
    ErrFamilyBadRequest                   // 400 (立即放弃,不重试)
    ErrFamilyPTL                          // prompt_too_long
    ErrFamilyNetwork
)

type ErrorClassifier struct {
    budgets map[ErrorFamily]int      // 各族独立 budget
    counter map[ErrorFamily]int
    mu      sync.Mutex
}

func (c *ErrorClassifier) Classify(err error) ErrorFamily
func (c *ErrorClassifier) ShouldAbort(fam ErrorFamily) bool
func (c *ErrorClassifier) Backoff(fam ErrorFamily, attempt int) time.Duration
```

**默认预算**:
- RateLimit: 5 次 (指数 2-4-8-16-32s + jitter)
- Overload: 3 次 (5-15-30s)
- Server: 3 次 (立即-1s-2s)
- Timeout: 2 次 (缩短 prompt 后重试,配合 BudgetManager)
- BadRequest: 0 次 (立即 abort)
- PTL: 1 次 (触发 reactive compact)

### 3.5 JSONRepair (G5)

**对标**: Claude 4.6 Grammar-Constrained Decoding + 社区 json-repair。

**思路**: 在 `content_block_stop` 收到 tool_use 后,尝试 `json.Valid(input)`,失败时走修复链:

```
raw → strip markdown → 补尾括号 → 修尾逗号 → 单引号→双引号 → 返回修复版或原始
```

如果修复成功但内容改变,在工具执行后注入 post-hoc note:

> "注意:你发送的 tool_use input 有 JSON 语法问题,已自动修复。请下次输出严格 JSON。"

### 3.6 TrajectoryMemory (G6)

**对标**: MiniMax M2.7 自进化循环 + ReasoningBank + CaRT 反事实轨迹对。

**思路**: 每完成一个 user turn,产出结构化 trajectory 条目:

```go
type Trajectory struct {
    TurnID     string
    UserIntent string
    Plan       string     // 从模型 thinking / 首条 assistant 文本提取
    ToolCalls  []ToolSig  // [{name,input_hash,result_ok,latency}]
    StopReason string
    Verdict    string     // "success" | "partial" | "fail" | "aborted"
    Summary    string
    At         time.Time
}
```

- **成功轨迹** → 存入 `TieredStore` (Importance=0.85, Source=traj_success)
- **失败轨迹** → 存入 (Importance=0.75, Source=traj_fail) + 打标签 `{symptom, root_cause, patch, metric_delta}`
- 下次 turn 0 召回时作为 `<prior_experience>...</prior_experience>` 注入

### 3.7 StopSignalDetector (G7)

**对标**: CaRT (Counterfactual Retrieval and Termination, arxiv:2510.08517)。

**思路**: 当连续工具调用信息收益递减 (后续搜索结果与前序高度重叠、读取文件内容被引用 < 5%) 时,主动注入 "you may have enough evidence" 提示,鼓励模型收敛。

```go
type StopSignalDetector struct {
    readFiles   map[string]int  // path → read count
    searchHits  []string        // 最近搜索词
    diminishing int             // 连续低收益计数
}

func (s *StopSignalDetector) Observe(toolName, input, result string) (suggest bool, reason string)
```

**判定规则** (任一命中即 suggest):
- 同一文件被读 ≥ 3 次,内容未变
- 最近 3 次 grep 返回结果集合 Jaccard > 0.8
- 累计 tool 调用 > 20 且未修改任何文件

### 3.8 HeavyMode (G8, 可选)

**对标**: Kimi Heavy Mode 8 路并行 + MAgICoRe Hard 分支 + best-of-N + meta-judge。

**思路**: 对被标记为 `critical_decision=true` 的 user intent (如 "设计架构" "调试根因"),并行跑 N=3 路完整 queryLoop,随后用一个 **meta-judge prompt** 选最优:

```
给定用户问题 <Q>,以下是 3 位工程师的独立解决方案:
<A>... <B>... <C>...
请从 {正确性, 完整性, 可维护性, 性能} 四个维度打分并给出最终推荐方案。
输出格式: JSON {winner: "A|B|C", rationale: "..."}
```

默认关闭,通过 `cfg.HeavyMode.Enabled + Criteria` 启用。

### 3.9 SpeculativeTool (G9, 可选,实验性)

**对标**: GLM 5.1 异步 RL + Speculative Decoding 思路泛化。

**思路**: 在流式接收 `input_json_delta` 时,如果累计 JSON 已足够判定工具 + 主要参数,且是并发安全只读工具 (Read/Glob/Grep),提前启动预取。当真正 `content_block_stop` 到达时,若参数匹配则直接用预取结果,否则丢弃。

默认关闭 (避免引入复杂性),仅在 benchmark 证明收益后启用。

### 3.10 EngineMetrics (共享)

```go
type EngineMetrics struct {
    TurnsTotal              atomic.Int64
    TurnsSuccess            atomic.Int64
    TurnsAborted            atomic.Int64
    ToolCallsTotal          atomic.Int64
    ToolLoopsDetected       atomic.Int64
    JSONRepairsApplied      atomic.Int64
    CacheHits               atomic.Int64
    CacheMisses             atomic.Int64
    BudgetDegradations      map[BudgetLevel]int64
    ErrorsByFamily          map[ErrorFamily]int64
    StopSuggestionsEmitted  atomic.Int64
    AvgTurnLatencyMs        atomic.Int64
}

func (m *EngineMetrics) Snapshot() map[string]any
```

在 `GetContextUsage()` 基础上增加 `GetEngineMetrics()` 供 dashboard 查询。

---

## 4. 集成到 queryLoop

```go
func (e *QueryEngine) queryLoop(ctx, messages, ch, streamCh) {
    for {
        // [1] Abort
        if ctx.Err() != nil { return aborted }

        // [2] 预算分级 + 降级
        if e.Budget != nil {
            level := e.Budget.LevelFor(messages)
            if level >= BudgetRed { messages = e.Budget.Degrade(messages, level) }
        }

        // [3] AutoCompact (保留)
        if e.Compactor != nil { ... }

        // [4] MicroCompact (保留)
        messages = compact.MicroCompact(messages, microCompactMaxChars)

        // [5] 构建 cache-friendly prompt
        static := e.buildStaticPrefix(turnCount)          // sys + tools + mem@t0
        dynamic := e.buildDynamicSuffix(messages)          // 最后 K 条
        system, cacheHit := e.PromptCache.Build(static, dynamic)
        if e.Metrics != nil { e.Metrics.RecordCache(cacheHit) }

        // [6] 调用 API
        eventCh, errCh := e.APIClient.StreamMessage(...)

        // ... 收集 assistantBlocks, toolUseBlocks ...

        // [7] 错误分类
        if streamErr := <-errCh; streamErr != nil {
            fam := e.ErrClassifier.Classify(streamErr)
            if fam == ErrFamilyBadRequest { return model_error }
            if e.ErrClassifier.ShouldAbort(fam) { return circuit_breaker }
            delay := e.ErrClassifier.Backoff(fam, attempt)
            time.Sleep(delay)
            // PTL 特殊分支 (保留现有 reactive compact)
            continue
        }

        // [8] 收到 tool_use 后立即 JSON 校验/修复
        for i, blk := range toolUseBlocks {
            if repaired, ok := e.JSONRepair.Try(blk.Input); ok {
                toolUseBlocks[i].Input = repaired
                e.Metrics.RecordRepair()
            }
        }

        // [9] 循环检测
        for _, blk := range toolUseBlocks {
            if looped, suggestion := e.LoopDetector.Observe(blk.Name, blk.Input); looped {
                // 注入反向提示替代工具执行
                messages = append(messages, makeAssistantNote(suggestion))
                continue outer
            }
        }

        // [10] max_tokens 续写 (保留)
        // [11] Post-sampling hooks (保留)
        // [12] 若无 tool_use → stop hooks → return (保留)

        // [13] 停止信号检测 (在决定是否终止前)
        if e.StopDetector != nil {
            if s, reason := e.StopDetector.CheckMessages(messages); s {
                // 注入 soft suggestion,让模型下一轮决定是否停
                softHint := makeSoftStopHint(reason)
                messages = append(messages, softHint)
            }
        }

        // [14] 执行工具 (保留 PartitionToolCalls)
        toolResults := tool.RunTools(...)
        // ...

        // [15] 每完成一个 turn 记录 trajectory
        if e.TrajMem != nil {
            e.TrajMem.RecordTurn(userIntent, plan, toolUseBlocks, toolResults, stopReason)
        }
    }
}
```

---

## 5. 文件结构

```
claude-go/pkg/engine/
├── engine.go              # 主循环 (保留, 集成新组件调用点)
├── metrics.go             # [新] EngineMetrics
├── cache.go               # [新] PromptCacheBuilder
├── budget.go              # [新] TokenBudgetManager
├── loop_detector.go       # [新] LoopDetector
├── error_classifier.go    # [新] ErrorClassifier
├── json_repair.go         # [新] JSON fallback chain
├── trajectory.go          # [新] TrajectoryMemory
├── stop_signal.go         # [新] StopSignalDetector (CaRT)
└── heavy_mode.go          # [新, 可选] N-way rollout + meta-judge
```

测试:
```
claude-go/tests/unit/
├── engine_cache_test.go
├── engine_budget_test.go
├── engine_loop_detector_test.go
├── engine_error_classifier_test.go
├── engine_json_repair_test.go
└── engine_trajectory_test.go
```

---

## 6. 验收指标

| 指标 | 当前 | 目标 |
|---|---|---|
| prompt cache hit rate (长会话) | 0% | ≥ 70% |
| token 成本节省 (每 session avg) | 0% | ≥ 25% |
| 工具死循环捕获率 | 0% | ≥ 95% |
| tool_use input JSON 解析成功率 | ~92% | ≥ 99% |
| 错误族隔离 budget 命中率 | N/A | ≥ 90% |
| 长会话 PTL 率 (> 50 轮) | ~5% | ≤ 1% |
| 冗余工具调用率 (CaRT 开启后) | N/A | ≤ 20% |
| 引擎级指标可观测 | ✗ | ✓ |

---

## 7. 实施优先级 & 回滚策略

| 优先级 | 组件 | 默认开关 | 风险 |
|---|---|---|---|
| **P0** | EngineMetrics | **ON** | 无 (只读) |
| **P0** | ErrorClassifier | **ON** | 低 (替代原 consecutive 计数) |
| **P0** | JSONRepair | **ON** | 极低 (失败回退原值) |
| **P0** | LoopDetector | **ON** | 低 (阈值保守) |
| **P1** | TokenBudgetManager | **ON** (Green-Yellow 静默) | 中 (Red/Critical 会丢旧 tool_result) |
| **P1** | PromptCacheBuilder | **ON** | 低 (只排 prompt 顺序) |
| **P1** | TrajectoryMemory | **ON** (依赖 MemoryStore) | 低 |
| **P2** | StopSignalDetector | **OFF** (opt-in) | 中 (可能误判) |
| **P3** | HeavyMode | **OFF** (opt-in) | 高 (成本 3x) |
| **P3** | SpeculativeTool | **OFF** (实验性) | 高 |

每个组件通过 `cfg.Engine.*` 独立开关,出问题时设为 false 即可回退。

---

## 8. 对齐各家论文的一句话映射

| 论文/模型 | 本方案吸收点 |
|---|---|
| **Claude 4.6/4.7** | Grammar Constrained Decoding → JSONRepair; Prompt Caching → PromptCacheBuilder |
| **GLM 5.1** | Agentic 信用分配 → LoopDetector; 异步 rollout → SpeculativeTool |
| **Kimi K2.5** | 防伪并行 → LoopDetector 的 artifact 校验; 溢出策略 → Budget Red/Critical |
| **Kimi K2 Thinking** | 假设-证据-验证 → HeavyMode meta-judge; 长思考 session → Budget + Trajectory |
| **MiniMax M2.7** | 失败轨迹挖掘 → TrajectoryMemory; Keep/Revert → ErrorClassifier 隔离 budget |
| **DeepSeek V3** | 负载感知 → ErrorClassifier (503 长退避) |
| **DeepSeek Prover V2** | 子目标验证 → 后续版本可加 subgoal verifier |
| **Qwen 3.6** | retry budget 分族 → ErrorClassifier |
| **GPT 5.6** | cached_tokens 透传 → EngineMetrics.CacheHits |
| **Gemini Gamma 4** | TaskBudget → TokenBudgetManager |
| **MAgICoRe** | Hard 分支 → HeavyMode |
| **CaRT** | 停止信号 → StopSignalDetector |
| **Mem0 / Letta** | Anchored summary + 滑动窗口 → Budget Yellow/Orange 摘要策略 |

---

## 9. 后续扩展 (v2)

- 子目标验证器 (DeepSeek Prover V2 风格) — 与 TodoWrite 工具联动
- 多路 rollout 的 RL 信用分配 (PARL 风格奖励) — 需训练数据
- PromptCacheBuilder 与 Anthropic `cache_control` 字段打通 — 需 API 侧支持
- TrajectoryMemory 与 RAPTOR 层级聚合 — 长期演进

---

> 本方案以"加能力、不破主链"为核心,所有优化点可独立开关、独立回滚、独立度量。实现见 `pkg/engine/*.go`。
