# 上下文退化与记忆召回改进方案

> 参考 MemGPT, MiniMax 短期记忆, CaRT 停止策略, MAgICoRe 步级监督

## 一、上下文退化问题分析

### 当前退化模式
1. **信息丢失**: prevResults map 只保留最后一个 stage 的输出
2. **摘要失真**: HandoffContext 截断到 1500 字符, 可能丢失关键细节
3. **记忆冲突**: dreaming 整合的记忆可能与 git 实际状态不一致
4. **重复遗忘**: 同一错误被修复后, 记忆中不记录修复方法

### Kimi K2 的应对策略
- 上下文溢出时隐藏旧 tool 输出, 保留结论
- 按需检索而非全量保留

## 二、Dreaming 系统改进

### 2.1 重要性加权合并 (替代简单去重)

当前 `localConsolidate` 按精确匹配去重。改进为:

```go
func (d *Dreamer) importanceWeightedConsolidate(sessions []SessionRecord) error {
    // 1. 高重要性 (≥0.7): 保留完整细节
    // 2. 中重要性 (0.4-0.7): 压缩为关键点
    // 3. 低重要性 (<0.4): 仅保留一句话摘要
    // 4. 冲突检测: 同一主题的新旧记忆不一致时, 标注冲突
}
```

### 2.2 睡眠时合成 (Sleep-time Synthesis)

不只是拼接, 而是从记忆中提取模式:

```go
type SynthesizedMemory struct {
    Pattern     string   // 识别到的模式
    Instances   []string // 出现在哪些会话中
    Frequency   int      // 出现频次
    LastSeen    time.Time
    Actionable  bool     // 是否可执行
    Action      string   // 推荐行动
}
```

### 2.3 矛盾检测

```go
func (d *Dreamer) detectContradictions(memories []string) []Contradiction {
    // 检测逻辑:
    // 1. 同一文件/模块的不同描述
    // 2. "已修复" vs "仍存在" 的状态冲突
    // 3. 设计决策被后续决策推翻但未更新
    // 解决: trust git/tests > prose memory
}
```

### 2.4 记忆分层 (MemGPT 风格)

```
Working Memory (当前任务)
├── 当前 DAG 节点上下文
├── 直接依赖的产出
└── 最近 3 轮反馈

Episodic Memory (近期会话)
├── 最近 N 个 session summaries
├── 重要决策记录
└── 错误修复记录

Semantic Memory (长期模式)
├── Evolution 经验库
├── Dreaming 整合的模式
└── 项目约定/规范
```

### 2.5 记忆强化与衰减

```go
// 被召回并使用的记忆 → 强化权重
func (d *Dreamer) reinforceMemory(memoryID string) {
    memory.Weight *= 1.2  // 使用后强化 20%
    memory.LastUsed = time.Now()
}

// 长期未用的记忆 → 自然衰减
func (d *Dreamer) decayUnused(memories []Memory, since time.Duration) {
    for _, m := range memories {
        if time.Since(m.LastUsed) > since {
            m.Weight *= 0.8  // 每个周期衰减 20%
        }
    }
}
```

## 三、CaRT 风格停止策略

防止 agent 陷入无限搜索/阅读循环:

```go
// 在 researcher/analyst 的 tool 循环中:
// 如果连续 N 次搜索未产生新信息 → 停止搜集, 开始综合
type InformationGainTracker struct {
    RecentSearches []SearchResult
    NewInfoCount   int
    TotalSearches  int
}

func (t *InformationGainTracker) ShouldStop() bool {
    if t.TotalSearches < 3 { return false }
    recentGain := float64(t.NewInfoCount) / float64(t.TotalSearches)
    return recentGain < 0.2 // 信息增益 <20% 时停止
}
```

## 四、实施步骤

1. dreamer.go: importanceWeightedConsolidate (替代简单去重)
2. dreamer.go: 睡眠合成 — 提取 pattern + frequency
3. dreamer.go: 矛盾检测 — 同主题冲突标注
4. evolution.go: 成功阶段也学习 (双向学习)
5. 测试验证
