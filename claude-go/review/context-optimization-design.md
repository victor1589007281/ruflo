# 会话内上下文优化改造方案

> **版本**: 1.1（已实施部分标注版）
> **日期**: 2026-05-16
> **范围**: QueryEngine 消息历史管理、上下文压缩、Token 预算分配
> **目标**: 解决长会话上下文膨胀、重复 Token 浪费、工具结果过度保留问题

---

## 更新记录

| 日期 | 版本 | 变更 |
|------|------|------|
| 2026-05-16 | 1.0 | 初始方案 |
| 2026-05-16 | 1.1 | 标注已实施能力；补充额外实施的优化项（TaskInstruction 移动、Assistant 过滤、LLMlingua 压缩） |
| 2026-05-17 | 1.2 | 补充提示词拼接模板；确认当前 tool 注入与 message 截断现状 |
| 2026-05-17 | 1.3 | 新增 Metrics 指标设计（MessageMetricsHook）；实施 ToolResult 分级保留（ToolResultLevelHook）；更新收益汇总 |

---

## 0. 当前提示词拼接模板（基于源码）

### 0.1 完整拼接流程

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  Phase 1: 上下文压缩（可选）                                                  │
│  - AutoCompact: 触发阈值 0.8（~160K token）                                  │
│  - MicroCompact: 单条 tool_result 截断到 50K 字符                            │
│  - PreCompact Hook 可 block 压缩                                             │
└─────────────────────────────────────────────────────────────────────────────┘
                                    ↓
┌─────────────────────────────────────────────────────────────────────────────┐
│  Phase 2: System Prompt 构建（每轮重新组装）                                  │
│                                                                             │
│  1. BuildEffectiveSystemPrompt() 优先级链: override > coordinator > agent    │
│     > custom > default                                                      │
│                                                                             │
│  2. 默认 prompt 拼接顺序（buildDefaultSystemPrompt）:                         │
│     [1] 身份声明 ("You are Claude...")                                      │
│     [2] <environment> OS/日期/cwd/shell/git/模型                            │
│     [3] <available_tools> 工具列表 (name + description)                     │
│     [4] <tool_usage> 工具使用指南                                           │
│     [5] <mcp_tool_guidance> MCP 工具引导（如有 MCP 工具）                    │
│     [6] <guidelines> 行为准则（先读后写、不提交 secrets 等）                 │
│     [7] <plan_mode> Plan Mode 引导                                          │
│     [8] CLAUDE.md 记忆内容（MemoryLoader.LoadAll）                          │
│     [9] <long_term_memory> Dream 记忆（跨会话 MEMORY.md）                    │
│                                                                             │
│  3. + TaskInstruction（若非空，追加到末尾）                                   │
│     用途: 将 20K 任务描述从 user message 移到 system prompt，               │
│     使前缀享受 cache_control，避免 user message 每轮重复膨胀                 │
│                                                                             │
│  4. PhasePreRequest HookChain:                                              │
│     - MemoryInjectHook: 首轮从 MemoryStore/FactStore 检索相关记忆，          │
│       追加到 system prompt（仅 TurnCount==0 执行）                          │
│     - PromptCacheHook: 拆分 static/dynamic，追踪 cache 命中率               │
└─────────────────────────────────────────────────────────────────────────────┘
                                    ↓
┌─────────────────────────────────────────────────────────────────────────────┐
│  Phase 3: apiMessages 构建（messagesToAPI）                                   │
│                                                                             │
│  1. OnMessageFilter Hook 拦截（可 block 整个请求）                            │
│                                                                             │
│  2. FilterPureToolUseUnits(messages, keepRecent=6)                          │
│     → 删除不含 text/thinking 的古老 assistant 消息                          │
│     → 连同其后续纯 tool_result user 消息一起删除（原子单元）                  │
│     → 保留最近 6 个 assistant 原子单元                                       │
│                                                                             │
│  3. 遍历 messages，合并连续同角色消息（user/assistant 严格交替）              │
│                                                                             │
│  4. CompressMessageContent(msg) — 对尾部倒数第 4 条之前的 user message       │
│     → 删除空行、移除 21 个停用词、压缩代码注释                               │
│     → 压缩后长度未减少 30% 时回退到原文                                      │
└─────────────────────────────────────────────────────────────────────────────┘
                                    ↓
┌─────────────────────────────────────────────────────────────────────────────┐
│  Phase 4: API 调用                                                          │
│  StreamMessage(apiMessages, systemPrompt, apiTools, maxTokens)              │
│  参数:                                                                      │
│    - apiMessages: user/assistant 严格交替的消息链                            │
│    - systemPrompt: 字符串数组（ Anthropic API 的 system 参数）               │
│    - apiTools: 过滤掉 DisabledTools 后的工具 schema                          │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 0.2 最终发送给 LLM 的数据结构

```go
// System Prompt（字符串数组， Anthropic API system 参数）
[
    "<身份+环境+工具+指南...>",      // static，享受 prompt cache
    "<CLAUDE.md 记忆>",              // static（若不变则 cache 命中）
    "<TaskInstruction>",             // dynamic，每轮可能变化
    "<L1/L2 检索记忆>",              // dynamic，首轮注入
]

// Messages（user/assistant 严格交替）
[
    {role: "user",     content: "[用户原始请求 / 或 SubmitMessage 传空]"},
    {role: "assistant", content: "[text/thinking + tool_use blocks...]"},
    {role: "user",     content: "[tool_results (合并后的) + meta 消息]"},
    {role: "assistant", content: "[thinking + tool_use...]"},
    ...
    // 注: 古老的无 reasoning assistant 已被 FilterPureToolUseUnits 删除
    // 注: 古老的 user message 内容已被 CompressMessageContent 压缩
]

// Tools Schema
[tool1, tool2, ...]  // APITools() 过滤 DisabledTools
```

### 0.3 Tool 注入与 Tool Result 注入逻辑

**Tool Use 注入**（`engine.go:753-766`）:
- 流式解析 assistant 响应，遇到 `content_block_stop` + `ContentBlockToolUse`
- 收集 `toolUseBlocks []ContentBlock`（含 name + input JSON）
- assistant message 本身已 `append(messages, assistantMsg)`（`engine.go:1058`）

**Tool Result 注入**（`engine.go:1196-1245`）:
- `tool.RunTools(ctx, toolUseBlocks, e.Tools, tctx, e.HookRunner)` 返回 `[]types.Message`
- 每个 `toolResult` 是 `MessageTypeUser`，`Content` 包含 `ContentBlockToolResult`
- `messages = append(messages, result)`（`engine.go:1245`）
- 如果多个 tool 并行执行，它们的 tool_result 作为**同一条 user message 的不同 content block** 合并

**关键特性**:
- `messages` 数组只增不减（在 `queryLoop` 内没有任何删除操作）
- `messagesToAPI` 是对 `messages` 的**只读转换**（深拷贝 + 过滤 + 压缩），不修改原始 `messages`
- 所有截断/过滤/压缩发生在 `messagesToAPI` 阶段，不影响引擎内部状态

### 0.4 当前 Message 截断现状（确认）

| 处理类型 | 实现状态 | 作用范围 | 源码位置 |
|---------|---------|---------|---------|
| **滑动窗口截断**（丢弃整条消息） | ❌ **未实现** | — | 文档 3.x 节标注 `[未实现]` |
| **纯 tool_use 原子单元过滤** | ✅ 已实施 | 删除古老无 reasoning 的 assistant + 对应 tool_result | `message_filter.go:13` |
| **LLMlingua 内容压缩** | ✅ 已实施 | 压缩古老 user message（tool_result）内容 | `message_filter.go:94` |
| **AutoCompact 上下文压缩** | ✅ 已实施 | 触发阈值 0.8，保留最近 6 条，摘要化早期消息 | `compact.go` |
| **MicroCompact 单条截断** | ✅ 已实施 | 单条 tool_result 截断到 50K 字符 | `compact.go` |
| **TaskInstruction 移动** | ✅ 已实施 | 20K 任务描述从 user msg 移到 system prompt | `engine.go:578` |
| **Read 哈希缓存** | ✅ 已实施 | 重复读取相同文件返回 "unchanged" 标记 | `fileread.go` |

**结论**: 当前**没有**基于消息数量的滑动窗口截断（如保留最近 12 条、丢弃其余）。`messages` 数组在 `queryLoop` 内部持续增长，仅在 `messagesToAPI` 阶段做**过滤**（删除无 reasoning 的古老 assistant）和**压缩**（压缩古老 tool_result 内容）。这意味着 100+ 轮后 `messages` 仍可能包含 99 条消息，只是其中部分被过滤/压缩后发给 API。

---

## 1. 现状诊断

### 1.1 核心问题

当前 `QueryEngine.queryLoop()` 中，对话历史 `messages` 只增不减，存在以下系统性缺陷：

| 问题 | 源码位置 | 影响 | 状态 |
|------|---------|------|------|
| 消息数组线性增长 | `engine.go:863` `messages = append(messages, assistantMsg)` | 100+ 轮后单条消息链即占满 200K 上下文 | **部分修复**（已实施纯 tool_use 原子单元过滤） |
| AutoCompact 触发门槛过高 | `compact.go` `TokenThreshold = 0.8` | 160K 才触发，qwen3.6-plus 平均 72K，几乎从不触发 | 待改造 |
| MicroCompact 仅截断单条 tool_result | `compact.go` 截断到 50K 字符 | 不改变消息数量，只是每条变短 | 待改造 |
| 用户消息重复发送 | `messagesToAPI()` 传递全部消息 | 20K 任务描述每轮重复，用户消息无 prompt caching | **已修复**（TaskInstruction 移到 system prompt） |
| Tool 结果全量保留 | `messagesToAPI()` 合并后全部发送 | 成功确认消息长期占用上下文 | **部分修复**（LLMlingua 压缩 + 纯 tool_use 单元过滤） |
| 无会话状态摘要 | 仅有 `MemoryStore` 追加到 system prompt | 但只检索历史记忆，不对当前会话内的执行过程做摘要 | 待实现 |

### 1.2 实测数据

基于 Run #11 的 prompt debug 日志：
- 平均输入 Token: **72K**
- 输出 Token: **~1.2K**
- 输入占比: **98.3%**
- 最大消息链长度: **99 条**（死亡循环）
- System prompt: **~8K**
- 重复用户消息（任务描述）: **~20K/轮**

---

## 1.3 Metrics 指标设计（基于 Hook 实现）

### 1.3.1 设计原则

所有新增指标通过 `InternalHook` 机制采集，遵循以下原则：
1. **纯观测，零干预**：Metrics Hook 只读分析 `messages`，不修改内容
2. **Lock-free 写入**：所有计数器使用 `atomic.Int64`，无锁竞争
3. **低开销**：字符统计 + 简单遍历，单次分析 < 1ms
4. **可开关**：通过 `Config.EnableMetrics` 统一控制

### 1.3.2 指标清单与含义

**实现**: `pkg/engine/internal_hook/hook_message_metrics.go` — `MessageMetricsHook`（PhasePreRequest, Priority 48）

| 指标名 | 类型 | 含义 | 写入时机 | 用途 |
|--------|------|------|---------|------|
| `turns_total` | counter | 总轮次 | PhasePostTurn | 会话长度分布 |
| `turns_success` | counter | 成功完成轮次 | PhasePostTurn | 成功率 |
| `turns_aborted` | counter | 中止轮次 | PhasePostTurn | 异常率 |
| `turns_error` | counter | 错误轮次 | PhasePostTurn | 错误率 |
| `tool_calls_total` | counter | 工具调用总数 | PhasePostToolUse | 工具使用频率 |
| `tool_loops_detected` | counter | 循环检测触发数 | PhasePostToolUse | 循环频率 |
| `cache_hits` / `cache_misses` | counter | Prompt Cache 命中/未命中 | PhasePreRequest | 缓存效率 |
| `avg_turn_latency_ms` | gauge | 平均轮次延迟 | PhasePostTurn | 性能基线 |
| **--- 以下为本次新增 ---** | | | | |
| `msg_chain_total_recorded` | counter | 消息链分析次数 | PhasePreRequest | 采样基数 |
| `msg_chain_total_tokens` | counter | 消息链估算 token 总数 | PhasePreRequest | Token 膨胀趋势 |
| `msg_chain_max_length` | gauge | 历史最大消息链长度 | PhasePreRequest | 峰值压力 |
| `msg_chain_max_tokens` | gauge | 历史最大消息链 token 数 | PhasePreRequest | 峰值 token |
| `tool_results_total` | counter | 累计 tool_result 块数 | PhasePreRequest | tool_result 密度 |
| `tool_results_success` | counter | 成功 tool_result 数 | PhasePreRequest | 成功比例 |
| `tool_results_error` | counter | 错误 tool_result 数 | PhasePreRequest | 错误比例 |
| `tool_results_total_chars` | counter | tool_result 总字符数 | PhasePreRequest | 平均大小 |
| `filter_deleted_units` | counter | FilterPureToolUseUnits 删除数 | PhasePreRequest | 过滤效果 |
| `compress_total_before_chars` | counter | 压缩前总字符数 | PhasePreRequest | 压缩收益 |
| `compress_total_after_chars` | counter | 压缩后总字符数 | PhasePreRequest | 压缩收益 |
| `memgpt_working_tokens` | counter | Working Memory token（预留）| — | MemGPT 实施后填充 |
| `memgpt_archival_tokens` | counter | Archival Memory token（预留）| — | MemGPT 实施后填充 |
| `memgpt_recall_hits` | counter | Recall 命中（预留）| — | MemGPT 实施后填充 |

### 1.3.3 关键衍生指标（通过 Snapshot 计算）

```go
// 平均每轮消息链长度
avgMsgChainLength = msg_chain_total_tokens / msg_chain_total_recorded

// tool_result 平均字符数
toolResultAvgChars = tool_results_total_chars / tool_results_total

// 过滤删除率
filterDeleteRatio = filter_deleted_units / msg_chain_total_recorded

// 压缩率
compressRatio = (compress_total_before_chars - compress_total_after_chars) / compress_total_before_chars

// 成功/错误比例
toolResultSuccessRatio = tool_results_success / tool_results_total
toolResultErrorRatio   = tool_results_error / tool_results_total
```

### 1.3.4 基于 Metrics 的 MemGPT ROI 预判

运行一周后，通过 Metrics 数据可精确计算 MemGPT 预期收益：

```python
# 假设 metrics 采集到以下数据（单次会话平均）
avg_turns = 15.2                    # turns_total / sessions
avg_msg_tokens = 4800               # msg_chain_total_tokens / msg_chain_total_recorded
p99_msg_tokens = 68000              # msg_chain_max_tokens 的 P99
avg_tr_chars = 2400                 # tool_results_total_chars / tool_results_total
tr_success_ratio = 0.72             # tool_results_success / tool_results_total

# MemGPT 节省估算
working_memory_tokens = 6 * 3000    # 6 轮 × 每轮 3K
archival_memory_tokens = avg_msg_tokens * 0.15  # 早期摘要约占原长的 15%
toolresult_saving = avg_msg_tokens * 0.20       # ToolResult 分级节省约 20%

estimated_saving = 1 - (working_memory_tokens + archival_memory_tokens + toolresult_saving) / avg_msg_tokens
```

---

## 2. 改造总览

```
┌─────────────────────────────────────────────────────────────────────┐
│                        上下文优化三层架构                              │
├─────────────────────────────────────────────────────────────────────┤
│  P0 (本周): 消息滑动窗口 + ToolResult 分级 + Read Hash 缓存           │
│  P1 (2周):  会话状态摘要 + 分层工具暴露 + 智能压缩                     │
│  P2 (1月):  MemGPT 式记忆分层 + 向量召回增强                          │
└─────────────────────────────────────────────────────────────────────┘
```

### 2.1 改造原则

1. **可观测**: 每次截断/压缩必须记录日志，保留前后 token 数对比
2. **可回退**: 压缩策略必须支持开关，异常时一键回退到全量消息
3. **保关键**: 用户原始意图、最近 3 轮对话、失败重试信息不得丢失
4. **渐进式**: 不一次性替换整个消息管道，分层灰度启用

---

## 3. P0: 消息滑动窗口（Message Sliding Window）`[未实现]`

### 3.1 问题定位

```go
// engine.go:526
apiMessages := messagesToAPI(messages)
// messagesToAPI 只合并连续同角色，不丢弃任何消息
```

当前 `messages` 包含：
- 用户原始请求（可能 20K）
- 数十轮 assistant tool_use
- 数十轮 tool_result
- meta 恢复消息（max_tokens 继续等）

### 3.2 方案设计

引入 `MessageWindow` 组件，在 `messagesToAPI` 之前做截断：

```
原始 messages: [U0, A0, T0, A1, T1, A2, T2, ..., A49, T49] (100 条)
                   ↓ MessageWindow 截断
API messages:    [Summary, U0, A45, T45, A46, T46, A47, T47, A48, T48, A49, T49]
                   ↑ 保留最近 N=6 条原始消息
                   ↑ 早期历史压缩为摘要
```

**具体规则**:

| 保留策略 | 消息类型 | 保留数量 | 理由 |
|---------|---------|---------|------|
| **永远保留** | 首条用户消息 | 1 条 | 包含用户原始意图，被其他方案引用 |
| **永远保留** | 最近 2 轮 user+assistant+tool 完整链 | 6 条 | 保证模型有即时上下文 |
| **摘要替代** | 更早的 assistant+tool | 全部压缩为 1 条 summary | 保留关键事实，丢弃过程噪音 |
| **丢弃** | 纯成功确认类 tool_result | 直接移除 | Write/Edit 的"Successfully..."无信息价值 |

### 3.3 源码修改点

**新增文件**: `pkg/engine/message_window.go`

```go
package engine

const (
    maxMessagesInWindow = 12   // 最多保留 12 条原始消息
    protectedTail       = 6    // 尾部 6 条不动
)

type MessageWindow struct {
    MaxMessages int
    ProtectedTail int
}

func (w *MessageWindow) Apply(messages []types.Message) []types.Message {
    if len(messages) <= w.MaxMessages {
        return messages
    }
    // 保留首条用户消息 + 尾部 protectedTail 条
    // 中间部分提取关键事实，生成 summary message
    // ...
}
```

**修改 `messagesToAPI`**:

```go
func messagesToAPI(messages []types.Message, window *MessageWindow) []types.APIMessage {
    if window != nil {
        messages = window.Apply(messages)
    }
    // 原有合并逻辑不变
}
```

**修改 `QueryEngine`**:

```go
// engine.go 中新增字段
MessageWindow *MessageWindow

// queryLoop 中调用前
apiMessages := messagesToAPI(messages, e.MessageWindow)
```

### 3.4 预期收益

- 100 条消息 → 12 条原始 + 1 条摘要
- 假设平均每条消息 2K token，从 200K → ~30K，**节省 85%**
- 对于 Run #11 的 99 轮场景，可将输入从 140K+ 降到 25K 以内

---

## 4. 额外实施（不在原始 P0/P1/P2 中）：Assistant 纯 tool_use 原子单元过滤 `[已实现]`

### 4.1 问题

assistant 消息仅包含 tool_use block 时，text 为空，没有任何 reasoning 价值。长会话中这类消息大量堆积。

### 4.2 实现

**文件**: `pkg/engine/engine.go`

新增 `filterPureToolUseUnits(messages, 6)`：
- 从尾部保留最近 **6 个 assistant 原子单元**
- 对于更旧的 assistant，若**不含 text/thinking**（纯 tool_use），则连同其后续纯 `tool_result` user 消息**整体删除**
- 以原子单元操作，避免产生孤儿 tool_result

```go
func filterPureToolUseUnits(messages []types.Message, keepRecent int) []types.Message
func hasReasoningContent(msg types.Message) bool
func isPureToolResultMessage(msg types.Message) bool
```

### 4.3 收益

- 删除大量无 reasoning 的历史 turn，每轮可减少 3-8K token

---

## 5. P0: ToolResult 分级保留与渐进降级 `[已实施]`

### 5.1 问题定位

当前所有 tool_result 一视同仁，长期保留：

```go
// filewrite.go:80
return &tool.ToolResult{Content: fmt.Sprintf("Successfully wrote %d bytes to %s", len(in.Contents), filePath)}

// bash.go:228
return &tool.ToolResult{Content: fmt.Sprintf("Exit code: 0\n\n%s", text)}

// fileread.go 返回完整文件内容 + 行号
```

成功写入的确认消息、历史 Bash 输出、已读的旧文件内容——这些在 10 轮后就完全失去价值，但仍占用上下文。

### 5.2 方案设计: ToolResult 四级保留策略

根据消息"年龄"（距离当前轮次）和"重要性"实施分级：

```
Level 0 (当前轮):      完整保留，原始长度
Level 1 (前 1-2 轮):   截断保留，最多 500 字符
Level 2 (前 3-5 轮):   仅保留状态标记（成功/失败 + 一句话摘要）
Level 3 (> 5 轮):      成功类完全移除；失败类保留 200 字符摘要
```

**消息重要性分类**:

| 重要性 | 消息特征 | 降级策略 |
|--------|---------|---------|
| **高** | `IsError=true` 的 tool_result、编译失败、权限被拒绝 | 永远不被完全删除（最多 Level 2 标记化） |
| **中** | Read 返回的关键文件内容（被后续 Edit 引用过） | Level 1 后截断，Level 2 后标记 |
| **低** | Write/Edit 成功确认、Bash `ls/cat/pwd` 输出、Grep 结果 | Level 2 后仅保留标记，Level 3 完全移除 |

### 5.3 源码实现

**文件**: `pkg/engine/internal_hook/hook_toolresult_level.go`

**实现方式**: `ToolResultLevelHook`（InternalHook，PhasePreRequest，Priority 38），在 `MessageFilterHook(40)` 之前执行。

```go
// 核心逻辑
func (h *ToolResultLevelHook) Execute(ctx *HookContext) (*HookResult, error) {
    // 1. 计算每条消息所属的轮次（从尾部倒数，以 assistant 为边界）
    msgRound := computeMessageRounds(messages)

    // 2. 对每条消息的 tool_result content blocks 按轮次年龄分级处理
    for i := range messages {
        round := msgRound[i]
        level := determineLevel(round) // Level0/1/2/3
        for _, b := range messages[i].Content {
            if b.Type == ContentBlockToolResult {
                degraded := degradeBlock(b, level)
                // Level3 成功类返回 nil → 完全移除该 block
            }
        }
    }
}
```

**降级规则详情**:

| 等级 | 成功类（IsError=false） | 错误类（IsError=true） |
|------|------------------------|----------------------|
| **Level 0** | 完整保留 | 完整保留 |
| **Level 1** | 截断到 500 字符 + "... (truncated by level1)" | 完整保留（`PreserveErrors=true`） |
| **Level 2** | `[历史操作结果: 成功]` | `[历史错误] 前300字符摘要` |
| **Level 3** | `nil`（完全移除该 block） | `[历史错误摘要] 前200字符` |

**安全设计**:
- `IsError=true` 永远不被完全删除（最多 Level 2 标记化，保留 300 字符）
- 以 `assistant + 其后的 tool_result user 消息` 为原子轮次判定
- 配置可定制：`ToolResultLevelConfig` 支持调整各级保留轮数、截断阈值

### 5.4 与现有压缩的协同

ToolResultLevelHook(38) → MessageFilterHook(40) → MessageMetricsHook(48) 的执行顺序：

1. **ToolResultLevelHook**: 先对古老 tool_result 做分级降级（从内容层面压缩）
2. **MessageFilterHook**: 再删除不含 reasoning 的古老 assistant 原子单元（从消息数量层面压缩）
3. **MessageMetricsHook**: 最后记录处理后的指标，用于观测效果

**收益叠加**:
- ToolResult 分级：对 tool_result 内容做年龄感知降级 → 节省 ~20-30%
- MessageFilter：删除无 reasoning 的 assistant 单元 → 额外节省 ~10-20%
- LLMlingua 压缩：对剩余古老 user message 做停用词压缩 → 额外节省 ~5-15%

### 5.5 关键优化: 成功确认类消息去重

Write/Edit 的成功确认（"Successfully wrote..."）在 Level 2 后被替换为：

```
[历史操作结果: 成功]
```

在 Level 3 后完全移除（因为同一条 user message 中可能还有其他 tool_result 需要保留，只移除该 block 不破坏消息结构）。

这种压缩通过 Hook 实现，不修改 `messages` 原始数组（HookResult.Messages 返回新切片），引擎内部状态不受影响。

---

## 6. 额外实施（不在原始 P0/P1/P2 中）：LLMlingua 式 Prompt 压缩 `[已实现]`

### 6.1 实现

已在 `messagesToAPI` 中集成：
- 仅压缩古老的 tool_result（尾部倒数第 4 条之前）
- 不影响最近轮次的上下文
- 自然语言部分移除停用词；代码块保留语法

### 6.2 收益

- 对长 tool_result（如 Bash 输出、Grep 结果）可压缩 20-40%

---

## 7. P0: Read 工具哈希缓存（会话级） `[已实现]`

### 7.1 问题定位

```go
// fileread.go
func (t *FileReadTool) Call(...) {
    data, err := os.ReadFile(filePath)
    // 每次 Call 都读磁盘 + 加行号 + 全量返回
}
```

模型在长会话中可能反复读取同一个文件（尤其是被频繁引用的接口文件）。当前每次都从磁盘读取并塞入上下文。

### 7.2 方案设计

在 `FileReadTool` 级别维护 `lastReadHashes map[string]string`（**只存 hash，不含内容**）：

**命中规则**:
- 同一文件路径，如果磁盘内容 hash 未变 → 返回 `<file path unchanged since last read (hash: abc123)>`
- 如果内容已变 → 重新读取、更新缓存

**内存控制**:
- 仅存储 path → hash 映射（无内容缓存）
- FileReadTool 实例级有效（随 QueryEngine 生命周期）

### 7.3 源码实现

**文件**: `pkg/tool/builtin/fileread.go`

```go
type FileReadTool struct {
    mu             sync.Mutex
    lastReadHashes map[string]string // path -> content hash (不含内容)
}

func (t *FileReadTool) Call(...) {
    // ... 读取文件后 ...
    currentHash := hashBytes(data)
    t.mu.Lock()
    lastHash, exists := t.lastReadHashes[filePath]
    t.lastReadHashes[filePath] = currentHash
    t.mu.Unlock()
    if exists && lastHash == currentHash {
        return &tool.ToolResult{Content: fmt.Sprintf(
            "<file %s unchanged since last read (hash: %s)>",
            filePath, currentHash[:8],
        )}, nil
    }
    // 正常格式化返回...
}
```

### 7.4 收益

- 重复读取相同文件时，从 25K token 降到 ~50 token
- 对频繁引用的接口文件尤其有效

---

## 8. P1: 会话状态摘要（Session State Summary）`[未实现]`

### 8.1 问题定位

当前 `messages` 保留的是完整对话过程，而非"已完成的任务 + 已知事实"。模型在 50 轮后需要反复从冗长历史中推理出"我已经完成了哪些工作"。

### 8.2 方案设计: 自动会话摘要

每 N 轮（如每 5 轮）或当消息数超过阈值时，触发一次**会话摘要生成**：

```
摘要内容（作为 system prompt 的最后一个 block 注入）:
---
本次会话已完成的操作:
- 读取了 pkg/api/client.go, pkg/engine/engine.go
- 修改了 api/client.go 中的 fallback URL 逻辑
- 验证了 go build ./... 编译通过

已确认的事实:
- MaxParallel 从 resolvedModel 读取，不再硬编码
- cache_control 放在 system content blocks[0]

待办/未完成:
- 备用模型名称映射 bug 待修复
---
```

**生成方式**:
- 轻量版：基于规则提取（扫描 tool_result 的成功/失败标记，提取文件路径和操作类型）
- 重量版：调用一次廉价模型（Haiku 级别）对历史消息做摘要

### 8.3 与现有 MemoryStore 的区别

| 特性 | MemoryStore (已有) | Session State Summary (新增) |
|------|-------------------|-----------------------------|
| 范围 | 跨会话历史 | 仅当前会话 |
| 触发 | 用户消息 BM25 检索 | 每 N 轮自动摘要 |
| 内容 | 用户偏好、关键事实 | 当前任务执行进度 |
| 位置 | system prompt 动态块 | system prompt 最后一个 block |

### 8.4 源码修改点

**新增**: `pkg/engine/session_summary.go`

```go
type SessionSummary struct {
    CompletedOps []string  // 已完成的操作
    KnownFacts   []string  // 已确认的事实
    PendingItems []string  // 待办项
    LastTurn     int       // 上次摘要的轮次
}

func (ss *SessionSummary) UpdateFromMessages(messages []types.Message) {
    // 规则提取：扫描 tool_result，归类操作
}

func (ss *SessionSummary) ToPromptBlock() string {
    // 格式化为 system prompt 内容块
}
```

**修改 `queryLoop` Phase 2 (system prompt 组装)**:

```go
systemPrompt := e.PromptMgr.BuildEffectiveSystemPrompt(e.Tools)

// 注入会话状态摘要
if e.SessionSummary != nil && turnCount > 0 && turnCount%5 == 0 {
    e.SessionSummary.UpdateFromMessages(messages)
    summaryBlock := e.SessionSummary.ToPromptBlock()
    if summaryBlock != "" {
        systemPrompt = append(systemPrompt, summaryBlock)
    }
}
```

---

## 9. P1: 分层工具暴露（Progressive Tool Loading）`[未实现]`

### 9.1 问题定位

当前 `Coding` profile 一次性暴露 10+ 个工具：

```go
// profile.go:55-71
registerFileMutationTools(reg)  // Write, Edit
registerReadSearchTools(reg)    // Read, Glob, Grep
reg.Register(NewBashTool())
reg.Register(NewTodoWriteTool())
// ... 共 10+ 个
```

模型在探索阶段（前 3 轮）通常只需要 Read/Grep/Glob，却同时看到 Write/Edit/Bash 的完整 schema，增加决策噪音。

### 9.2 方案设计: 按阶段动态调整可用工具

```
Phase 0 (探索, 前 2 轮):  Read, Grep, Glob, EnterPlanMode
Phase 1 (规划, 第 3 轮):  + TodoWrite, ExitPlanMode
Phase 2 (执行, 第 4 轮+): + Write, Edit, Bash, TaskOutput, StructuredOutput
Phase 3 (验证):            + 所有工具
```

**实现方式**:

不修改 Registry 的注册逻辑，而是在 `queryLoop` 构建 `apiTools` 时做过滤：

```go
// engine.go:527
apiTools := e.Tools.APITools()
if e.ToolPhaseFilter != nil {
    apiTools = e.ToolPhaseFilter.Filter(apiTools, turnCount)
}
```

### 9.3 预期收益

- 减少 schema token：每轮 tools 描述约 2-5K token，阶段过滤后初期可减少 50%
- 降低模型误操作：避免模型在信息不足时急于写文件
- 与 Anthropic "extended thinking" 模式对齐：先思考再行动

---

## 10. P1: 智能上下文压缩（Importance-Based Compaction）`[未实现]`

### 10.1 问题定位

当前 `AutoCompact` 基于**消息总 token 数**触发，不是基于**消息重要性**。当触发时，保护最后 6 条消息，对前面的消息做摘要——但前面的消息可能包含关键的编译错误或用户约束。

### 10.2 方案设计: 消息重要性评分

每条消息在创建时或压缩前计算 `importanceScore`（0-100）：

| 信号 | 加分 |
|------|------|
| `IsError=true` | +50 |
| 包含"编译失败"/"test failed" | +40 |
| 用户明确约束（"必须..."/"不要..."） | +30 |
| 被后续 tool_use 引用过 | +20 |
| 文件写入操作 | +10 |
| 纯成功确认（"Successfully..."） | -30 |
| Bash `ls/cat/pwd` 输出 | -20 |
| 超过 5 轮前 | 每轮 -5 |

压缩时按分数排序，保留高分消息，对低分消息做截断/移除。

### 10.3 源码修改点

**修改 `types.Message`**:

```go
type Message struct {
    // ... 已有字段 ...
    Importance int `json:"importance,omitempty"` // 0-100, 由生成者或后处理设置
}
```

**修改 `AutoCompact`**:

```go
func (c *Compactor) AutoCompact(ctx context.Context, messages []types.Message, model string) ([]types.Message, error) {
    // 先计算/更新重要性分数
    scored := scoreMessages(messages)
    // 按分数排序，保留前 N 条，其余做摘要
}
```

---

## 11. 额外实施（不在原始 P0/P1/P2 中）：TaskInstruction 移到 system prompt `[已实现]`

### 11.1 问题

Agent Team 场景中，`buildTaskPrompt` 生成的 20K 任务描述通过 `SubmitMessage` 作为 `msg[0]`（user message）传入。user message 不参与 prompt caching，且每轮重复发送。

### 11.2 实现

**文件**: `pkg/engine/engine.go` + `pkg/feishu/session.go`

1. `QueryEngine` 新增 `TaskInstruction string` 字段
2. `queryLoop` 中，system prompt 组装完成后追加 TaskInstruction：

```go
systemPrompt := e.PromptMgr.BuildEffectiveSystemPrompt(e.Tools)
if e.TaskInstruction != "" {
    systemPrompt = append(systemPrompt, e.TaskInstruction)
}
```

3. `sessionAgentRunner.Execute` 中：

```go
eng.TaskInstruction = userPrompt  // 20K 任务描述移到 system prompt
for msg := range eng.SubmitMessage(ctx, "") {  // user message 传空
```

### 11.3 收益

- 20K 任务描述从 user message（无缓存）移到 system prompt 末尾
- system prompt 前缀仍享受 `cache_control` 缓存
- 每轮减少 20K 重复 token

---

## 12. P2: MemGPT 式记忆分层（长期方向）`[未实现]`

### 12.1 架构

```
┌─────────────────────────────────────────┐
│  System Prompt (缓存命中)                │
│  - 身份声明、环境、工具指南               │
├─────────────────────────────────────────┤
│  工作记忆 (Working Memory) — 20K token   │
│  - 最近 3 轮完整对话                     │
│  - 会话状态摘要                          │
├─────────────────────────────────────────┤
│  召回记忆 (Recall Memory) — 按需加载     │
│  - 向量检索: "之前怎么修复的类似bug？"    │
│  - 从 MemoryStore / FactStore 检索       │
├─────────────────────────────────────────┤
│  存档记忆 (Archival Memory) — 摘要形式   │
│  - 早期对话的压缩摘要                     │
│  - 类似当前 AutoCompact 的蒸馏结果       │
└─────────────────────────────────────────┘
```

### 12.2 与现有组件的关系

- **已有**: `MemoryStore`（L1 情景记忆）、`FactStore`（L2 结构化记忆）、`Ingestor`（记忆摄入）
- **已有**: `AutoCompact`（上下文压缩）、`PreCompact` 事实提取
- **新增**: 显式的 Working Memory 边界控制、自动的 Recall 触发

### 12.3 实现要点

```go
// 在 queryLoop 中，不是传递全部 messages，而是构建分层上下文
workingMessages := extractWorkingMemory(messages)    // 最近 N 条
archivalSummary := buildArchivalSummary(messages)      // 早期摘要
recalledFacts := e.MemoryStore.Retrieve(userIntent, 5) // 向量检索

// 组装 apiMessages:
// systemPrompt + recalledFacts + archivalSummary + workingMessages
```

---

## 13. 实施路线图

### 第一阶段: P0 紧急止血（本周）

- [x] **Assistant 纯 tool_use 过滤**: `filterPureToolUseUnits` 已合入 `engine.go`
- [x] **LLMlingua 式压缩**: `compressMessageContent` + `compressText` 已合入 `engine.go`
- [x] **Read 哈希缓存**: `FileReadTool.lastReadHashes` 已合入 `fileread.go`
- [x] **TaskInstruction 移动**: `QueryEngine.TaskInstruction` + `sessionAgentRunner` 已合入
- [ ] **消息滑动窗口**: 实现 `MessageWindow`，保留首条用户消息 + 最近 6 条 + 摘要化中间部分
- [ ] **ToolResult 四级保留**: 扩展 `MicroCompact`，按消息年龄实施 4 级保留策略
- [ ] **指标埋点**: 每次压缩/截断记录前后 token 数，导出到 Metrics

### 第二阶段: P1 结构性优化（2 周内）

- [ ] **会话状态摘要**: 每 5 轮自动生成执行进度摘要，注入 system prompt
- [ ] **分层工具暴露**: `Coding` profile 按 turn 数动态过滤可用 tools
- [ ] **智能压缩**: 基于重要性评分的 `AutoCompact` 替代方案
- [ ] **A/B 验证**: 对比全量消息 vs 优化后的 token 消耗和任务成功率

### 第三阶段: P2 记忆分层（1 月+）

- [ ] **MemGPT 架构**: 显式区分 Working/Recall/Archival 三层
- [ ] **自动 Recall 触发**: 当模型询问"之前..."时自动从 MemoryStore 检索
- [ ] **向量增强**: 对 tool_result 做嵌入，支持语义级历史检索

---

## 14. 与现有代码的集成点

| 修改文件 | 修改内容 | 状态 | 影响范围 |
|---------|---------|------|---------|
| `pkg/engine/engine.go` | `messagesToAPI` + `filterPureToolUseUnits` + `compressMessageContent` | **已实施** | 核心流程 |
| `pkg/engine/engine.go` | `QueryEngine.TaskInstruction` + `queryLoop` 注入 | **已实施** | 核心流程 |
| `pkg/feishu/session.go` | `sessionAgentRunner.Execute` 设置 TaskInstruction | **已实施** | Agent Team 流程 |
| `pkg/tool/builtin/fileread.go` | `FileReadTool.lastReadHashes` 缓存 | **已实施** | 工具层 |
| `pkg/engine/loop_detector.go` | 产出级循环检测（编辑震荡 + 编译错误指纹） | **已实施** | 死循环防护 |
| `pkg/engine/message_window.go` | 新增 MessageWindow | 未实施 | 纯新增，无侵入 |
| `pkg/compact/compact.go` | `MicroCompact` 增加分级逻辑 | 未实施 | 压缩模块 |
| `pkg/engine/session_summary.go` | 新增 | 未实施 | 纯新增，无侵入 |
| `pkg/types/types.go` | Message 增加 Importance 字段 | 未实施 | 数据模型 |
| `pkg/engine/metrics.go` | 增加压缩相关指标 | **部分实施** | 可观测性 |

---

## 15. 风险与回退

| 风险 | 影响 | 缓解措施 | 状态 |
|------|------|---------|------|
| TaskInstruction 移到 system prompt 后模型不执行 | 高 | 保留空 user message 作为触发；若异常回退到原模式 | **已验证通过编译** |
| 纯 tool_use 过滤导致丢失关键 tool_result | 中 | 只删除不含 text/thinking 的 assistant；保留最近 6 个单元 | **已实施** |
| LLMlingua 过度压缩损失信息 | 低 | 压缩率未达 30% 时回退原文；不压缩最近 3 轮 | **已实施** |
| Read 缓存返回"unchanged"后模型误解 | 低 | 提示中明确说明"文件未变更，内容与前次读取一致" | **已实施** |
| 滑动窗口截断关键信息 | 高 | 保留"首条用户消息 + 最近 6 条"作为硬保护；摘要化前可配置白名单 | 未实施 |
| ToolResult 过度压缩导致丢失错误详情 | 中 | `IsError=true` 的消息永远不全量删除，最多做行数限制 | 未实施 |
| 会话摘要生成增加 API 调用成本 | 中 | 摘要使用规则提取（本地），不调用模型；可选降级到纯规则版 | 未实施 |
| 分层工具暴露导致模型困惑 | 低 | 在 system prompt 中说明"可用工具随阶段变化"；保留 ToolSearch | 未实施 |
| 新组件引入 bug | 中 | 所有改造通过 `Config.Enable*` 开关控制，默认关闭，灰度验证后开启 | 部分实施 |

---

## 16. 预期收益汇总

| 优化项 | 输入 Token 节省 | 对 Run #11 的预估影响 | 状态 |
|--------|---------------|---------------------|------|
| TaskInstruction 移动 | -20K/轮 | 任务描述不再重复发送 | **已实施** |
| Assistant 纯 tool_use 过滤 | -10~20% | 删除无 reasoning 的历史 turn | **已实施** |
| LLMlingua 压缩 | -5~15% | 古老 tool_result 压缩 20-40% | **已实施** |
| Read 哈希缓存 | -5~10% | 避免重复读取相同文件 | **已实施** |
| **ToolResult 分级** | **-20~30%** | 成功确认类 1 轮后标记化，3 轮后移除 | **已实施** (hook_toolresult_level.go) |
| **MessageMetrics** | **观测型** | 消息链/token/tool_result 分布采集 | **已实施** (hook_message_metrics.go) |
| 消息滑动窗口 | -60~80% | 99 轮从 140K → 30K | 未实施 |
| 会话状态摘要 | -10~15% | 用摘要替代早期消息链 | 未实施 |
| 分层工具暴露 | -5~10% (初期) | 前 3 轮 schema 减半 | 未实施 |
| **已实施合计** | **-50~70%** | **200K 上下文可支撑 150+ 轮** | |
| **全部实施后合计** | **-75~90%** | **200K 上下文可支撑 250+ 轮** | |
