# 提示词缓存 + 长时间编程效率优化方案

> 参考 Anthropic Prompt Caching, OpenAI Cached Tokens, Kimi K2 溢出策略, MemGPT

## 一、Prompt 缓存机制

### 核心原理
Anthropic/OpenAI 的 prompt caching: 将稳定前缀标记为可缓存, 复用 KV cache。

### 在 claude-go 中的实现策略

#### 1.1 Prompt 布局优化
```
[缓存区 — 不变内容, 放在最前]
├── System Prompt (角色定义)
├── Tool Definitions
├── 项目上下文 (repo 结构, 技术栈, 约束)
├── 设计文档摘要 (design doc)
└── 历史经验 (evolution experiences)

[变化区 — 每次变化, 放在最后]
├── 当前任务描述
├── 上轮反馈 (adversarial feedback)
├── 当前代码片段
└── micro-test 结果
```

#### 1.2 PromptBuilder 组件
```go
type PromptBuilder struct {
    staticPrefix  string    // 不变前缀
    prefixHash    string    // SHA256 用于缓存追踪
    cacheHits     int64     // 命中次数
    cacheMisses   int64     // 未命中
}

func (pb *PromptBuilder) Build(dynamic string) string {
    if pb.prefixHash == hash(pb.staticPrefix) {
        pb.cacheHits++  // 前缀未变, 可能命中 API 缓存
    }
    return pb.staticPrefix + "\n\n" + dynamic
}
```

#### 1.3 缓存指标追踪
```go
// 新增 Metrics 常量
const (
    MetricPromptCacheHitRate  = "prompt_cache_hit_rate"
    MetricPromptCacheTokens   = "prompt_cached_tokens"
    MetricPromptTotalTokens   = "prompt_total_tokens"
)
```

## 二、长时间编程效率优化

### 2.1 渐进式摘要 (Progressive Summarization)

旧的 tool 输出/上轮结果 → 结构化摘要:
```go
type SummarizedOutput struct {
    Role      string   // 产出角色
    FilesChanged []string // 涉及文件
    KeyDecisions []string // 关键决策
    Issues     []string  // 未解决问题
    FullOutput string   // 完整输出 (仅最近 N 轮保留)
}
```

### 2.2 上下文滑动窗口
```
┌─────────────────────────────────────────┐
│ 完整保留 (最近 3 轮)                      │
│ ├── 当前轮完整输出                        │
│ ├── 上轮完整输出                          │
│ └── 前轮完整输出                          │
├─────────────────────────────────────────┤
│ 摘要保留 (更早的轮次)                     │
│ ├── 第 N-3 轮: 文件列表+关键决策           │
│ ├── 第 N-4 轮: 文件列表+关键决策           │
│ └── ...                                  │
├─────────────────────────────────────────┤
│ 永久保留 (不衰减)                         │
│ ├── 设计文档                             │
│ ├── 约束清单                             │
│ └── 测试失败日志 (永远保留直到修复)         │
└─────────────────────────────────────────┘
```

### 2.3 工作集选择 (Working Set)
```go
// 根据当前 DAG 路径, 只保留相关上下文
func (o *Orchestrator) buildWorkingSet(node *TaskNode) []string {
    // 1. 当前节点的直接依赖输出
    // 2. 设计文档中与 DesignRef 匹配的章节
    // 3. 相关的 Evolution 经验
    // 4. 不包含无关节点的输出
}
```

### 2.4 Kimi 风格溢出降级
```go
// 上下文即将溢出时:
// 1. 隐藏旧 tool 输出 (保留结论)
// 2. 压缩 prevResults (只保留文件名+状态)
// 3. 最后手段: 截断最早的完整轮次
func (pb *PromptBuilder) DegradeIfNeeded(totalTokens, budget int) {
    if totalTokens > budget * 0.9 {
        // 触发降级
    }
}
```

## 三、实施步骤

1. PromptBuilder: staticPrefix/dynamicSuffix 分离
2. workflow.go 和 orchestrator.go 使用 PromptBuilder
3. 指标追踪: cache hit rate, token savings
4. 渐进式摘要: prevResults 超过阈值时自动摘要
5. 工作集选择: orchestrator buildTaskPrompt 只取相关上下文
