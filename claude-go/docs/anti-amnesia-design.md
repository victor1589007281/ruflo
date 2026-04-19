# 反失忆系统设计方案 (Anti-Amnesia Architecture)

> **版本**: 1.0 | **日期**: 2026-04-19
> **目标**: 综合业界最新研究成果，构建多层次、自适应的记忆持久化体系，
> 从根本上解决 AI Agent 的失忆问题。

---

## 一、业界调研综述

### 1.1 大模型原生记忆方案

| 模型 | 上下文长度 | 核心记忆策略 | 关键创新 | 局限性 |
|:--:|:--:|:--:|:--:|:--:|
| **Kimi K2.6** | 256K | MLA + YaRN 位置编码 + 渐进式训练 | 减少注意力头(64)提升长序列稳定性 | 仅 within-session |
| **GLM 5.1** | 200K | 智能缓存 + 异步 RL | 8 小时持续执行、600+ 轮迭代 | 缓存机制未开源 |
| **MiniMax M2.7** | - | MaxClaw 内置长期记忆 | 跨天/周保留偏好、自进化模型 | 闭源 |
| **DeepSeek V4** | 1M | Engram 条件记忆 (Hash O(1)) | 哈希映射实现常数时间知识检索 | **会话间不持久** |
| **Qwen 3.6** | 1M | preserve_thinking + 混合注意力 | 保存中间推理过程减少 token 消耗 | 仅 API 产品 |
| **Claude 4.6/4.7** | 1M | 服务端压缩 + thinking 块剥离 | Task Budget (token 倒计时) | 无跨会话持久记忆 |
| **GPT-6** | - | 持久记忆 + 主动 Agent | 记住写作偏好/项目上下文/沟通风格 | 2026 年底发布 |
| **Gemini** | - | Always-On Memory Agent | SQLite + 3 子 Agent (摄入/整合/查询) | 无向量索引 |

### 1.2 开源框架方案

| 框架 | 记忆机制 | 优势 | 问题 |
|:--:|:--:|:--:|:--:|
| **OpenClaw** | MEMORY.md + 日记 + DREAMS.md | 简单直观 | 压缩时静默丢失工作上下文 (Bug #25633) |
| **openclaw-memory-final** | 每日增量蒸馏 + 每周整合 + QMD 索引 | 生产级可靠性 | 社区方案，非官方 |
| **openclaw-memory-architecture** | 12 层知识图谱 + 多语义搜索 | 3K+ 事实存储 | 复杂度高 |
| **Mem0** | 向量 DB + 自动捕获 | 保证持久化 | 外部依赖 |
| **Letta (MemGPT)** | 分层读写 + 显式记忆操作 | 理论完善 | 工程复杂 |

### 1.3 学术论文关键发现

| 论文 | 年份 | 核心贡献 |
|:--:|:--:|:--:|
| **MemoryOS** (EMNLP 2025) | 2025 | 三层存储 (STM/MTM/LPM) + 分段页面组织 → F1 提升 49% |
| **FluxMem** | 2026 | 自适应记忆结构选择 → 长程任务提升 6-9% |
| **LLM Agent Memory Survey** | 2026.03 | 三范式统一分类: 自然语言 / 中间表示 / 参数化 |
| **Memory in the Age of AI Agents** | 2026.01 | 三维度: 形式(token/参数/潜在)、功能(事实/经验/工作)、动态(形成/演化/检索) |
| **Anatomy of Agentic Memory** | 2026 | 四种 MAG 结构: 轻量语义 / 实体中心 / 情景反思 / 结构层级 |

### 1.4 遗忘曲线与间隔重复

**Ebbinghaus 公式**: `R = e^(-t/S)` (R=保留率, t=时间, S=记忆强度)

不同记忆类型的衰减率:

| 类型 | 衰减系数 | 示例 |
|:--:|:--:|:--:|
| 事实 (Fact) | 0.01 | API 签名、架构决策 |
| 偏好 (Preference) | 0.05 | 用户编码风格、命名约定 |
| 目标 (Goal) | 0.15 | 当前冲刺目标、里程碑 |
| 事件 (Event) | 0.25 | 某次部署、某个 bug 修复 |
| 上下文 (Context) | 0.60 | 临时讨论、中间调试信息 |

---

## 二、claude-go 现有基础设施分析

### 2.1 当前记忆层次

```
┌─────────────────────────────────────────────────────────────────┐
│                     claude-go 记忆架构 (现状)                     │
├──────────────┬──────────────────────────────────────────────────┤
│   层级        │  实现                                            │
├──────────────┼──────────────────────────────────────────────────┤
│ 工作记忆      │ engine.go: []types.Message (会话消息链)            │
│ 静态记忆      │ memory.Loader: CLAUDE.md / rules / MEMORY.md     │
│ 情景记忆      │ memory.TieredStore: BM25+衰减 (episodic_memory)  │
│ 长期记忆      │ dreaming: consolidated.md / memory-NNN.md        │
│ 团队记忆      │ Blackboard: 键值对 + 分类                         │
│ 进化记忆      │ evolution: experiences.json + trajectories.json   │
│ 会话存档      │ session.SessionStore: JSONL 全量转录              │
└──────────────┴──────────────────────────────────────────────────┘
```

### 2.2 关键缺陷

| # | 缺陷 | 影响 |
|:-:|:-:|:-:|
| 1 | **Dreaming 与 TieredStore 断裂** | 整合后的记忆不回注情景存储 |
| 2 | **Evolution 与 Dreaming 独立** | 团队经验和会话记忆不交叉学习 |
| 3 | **CLI 模式 Dreaming 未接入** | 非飞书路径无记忆整合 |
| 4 | **Compact 丢失关键信息** | AutoCompact 的有损摘要可能删除重要决策 |
| 5 | **Token 估计粗糙** | char/4 估算导致压缩时机偏差 |
| 6 | **无分类衰减** | 所有记忆使用统一衰减参数 |
| 7 | **跨团队记忆隔离** | 不同团队的经验无法共享 |
| 8 | **Blackboard 无持久语义索引** | 只有键值查找，无语义检索 |

---

## 三、Anti-Amnesia 架构设计

### 3.1 总体架构

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    Anti-Amnesia Memory System                          │
│                                                                         │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐               │
│  │  摄入层   │  │  工作层   │  │  整合层   │  │  检索层   │               │
│  │ Ingest   │→│ Working  │→│Consolidate│→│ Retrieve │               │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘               │
│       ↑                                         │                       │
│       │              ┌──────────┐               │                       │
│       └──────────────│  存储层   │←──────────────┘                       │
│                      │ Storage  │                                       │
│                      └──────────┘                                       │
│                           │                                             │
│              ┌────────────┼────────────┐                                │
│              ↓            ↓            ↓                                │
│        ┌──────────┐ ┌──────────┐ ┌──────────┐                          │
│        │  热存储   │ │  温存储   │ │  冷存储   │                          │
│        │  (RAM)   │ │ (SQLite) │ │   (MD)   │                          │
│        │ ≤1h 对话  │ │ ≤30d 索引 │ │ 永久归档  │                          │
│        └──────────┘ └──────────┘ └──────────┘                          │
└─────────────────────────────────────────────────────────────────────────┘
```

### 3.2 五层记忆模型 (借鉴 MemoryOS + Google AOM + DeepSeek Engram)

| 层级 | 名称 | 容量 | 衰减率 | 持久化 | 来源 |
|:--:|:--:|:--:|:--:|:--:|:--:|
| **L0** | 即时工作记忆 | 当前对话 | 无衰减 | 无 | 当前会话消息 |
| **L1** | 短期情景记忆 | 最近 100 条 | 快 (0.3/天) | RAM + JSON | BM25 提取的关键事实 |
| **L2** | 中期结构记忆 | 最近 30 天 | 中 (0.05/天) | SQLite | Dreaming 整合输出 |
| **L3** | 长期语义记忆 | 无限 | 极慢 (0.005/天) | Markdown | 架构决策、偏好、模式 |
| **L4** | 进化经验记忆 | 200 条上限 | Ebbinghaus | JSON | 团队轨迹、成功模式 |

### 3.3 摄入层 (MemoryIngestor)

每次 Agent 交互后自动摄入:

```go
type MemoryIngestor struct {
    classifier  MemoryClassifier    // 分类: fact/preference/goal/event/context
    extractor   KeyFactExtractor    // 关键事实提取 (复用 SmartExtractKeyFacts)
    scorer      ImportanceScorer    // 重要性评分 (0-1)
    tagger      TopicTagger         // 主题标签
}

type IngestResult struct {
    Facts       []MemoryFact        // 提取的事实
    Category    MemoryCategory      // 记忆分类
    Importance  float64             // 重要性分数
    Topics      []string            // 主题标签
    DecayRate   float64             // 分类衰减率
    Source      string              // 来源 (chat/team/dream)
}
```

**分类规则** (借鉴 FluxMem 自适应结构选择):

| 信号 | 分类 | 衰减率 |
|:--:|:--:|:--:|
| 包含 API/接口/类型签名 | Fact | 0.01 |
| 包含 "我喜欢/我习惯/我偏好" | Preference | 0.05 |
| 包含 TODO/目标/计划 | Goal | 0.15 |
| 包含时间戳/部署/发布 | Event | 0.25 |
| 包含调试/临时/试试 | Context | 0.60 |

### 3.4 整合层 (MemoryConsolidator) — Dreaming 增强

借鉴 Google Always-On Memory Agent 的三子代理架构，将 Dreaming 从"简单拼接"升级为"认知整合":

#### 阶段 1: 增量蒸馏 (Daily Distillation)

```go
type DailyDistillation struct {
    Date        time.Time
    NewFacts    []MemoryFact         // 今日新增事实
    Updated     []MemoryFact         // 被强化的旧事实
    Deprecated  []MemoryFact         // 被矛盾替代的旧事实
    Patterns    []RecognizedPattern  // 识别到的模式
    Connections []FactConnection     // 事实间的关联
}
```

每次 Dreaming 触发时:
1. **提取**: 从 L1 短期记忆中提取自上次整合以来的所有条目
2. **去重**: 语义相似度 > 0.85 的条目合并 (非精确匹配)
3. **矛盾检测**: 同主题的新旧事实对比，标记矛盾并以新版为准
4. **模式识别**: 识别重复出现的行为模式（如"用户总是先写测试再写实现"）
5. **关联发现**: 构建事实间的关联图（如"限流 → 熔断 → 降级"属同一领域）

#### 阶段 2: 周期整合 (Periodic Consolidation)

```go
type ConsolidationCycle struct {
    Interval    time.Duration        // 默认 7 天
    Strategy    string               // "hierarchical" | "thematic" | "temporal"
}
```

- **层级整合**: 多日蒸馏结果按主题归类合并
- **重要性重新评估**: 长期未被召回的记忆降低重要性
- **存储降级**: L1→L2→L3 逐级沉降

#### 阶段 3: 遗忘与归档 (Forgetting & Archival)

```go
type ForgetPolicy struct {
    // 分类衰减 (借鉴 Ebbinghaus + 间隔重复)
    CategoryDecayRates map[MemoryCategory]float64
    
    // 间隔重复增强: 每次被成功召回，Strength +0.2
    RetrievalStrengthBonus float64
    
    // 永久豁免: importance ≥ 0.9 的事实永不遗忘
    EvergreenThreshold float64
    
    // 归档阈值: retention < 0.1 时从活跃存储移除，写入冷存储
    ArchiveThreshold float64
}
```

### 3.5 检索层 (MemoryRetriever)

借鉴 DeepSeek V4 Engram 的思路，实现**混合检索**:

```go
type HybridRetriever struct {
    bm25      *BM25Index          // 关键词匹配 (现有 TieredStore)
    semantic  *SemanticIndex      // 语义相似度 (可选，依赖 embedding)
    recency   *RecencyScorer      // 时间衰减加权
    frequency *FrequencyScorer    // 访问频次加权
}

// 综合评分 = α×BM25 + β×Semantic + γ×Recency + δ×Frequency
// 其中 α+β+γ+δ=1, 默认 α=0.3, β=0.3, γ=0.25, δ=0.15
```

检索触发时机:
1. **每轮对话开始**: 从 L1-L3 检索与用户输入相关的记忆
2. **团队阶段开始**: 从 L4 检索相关经验
3. **Compact 前**: 将即将被压缩的关键信息提取并持久化到 L1

### 3.6 存储层改造

#### SQLite 中期存储 (新增, 借鉴 Google AOM)

```sql
CREATE TABLE memory_facts (
    id          TEXT PRIMARY KEY,
    content     TEXT NOT NULL,
    category    TEXT NOT NULL,     -- fact/preference/goal/event/context
    importance  REAL DEFAULT 0.5,
    strength    REAL DEFAULT 1.0,  -- 记忆强度 (间隔重复)
    decay_rate  REAL DEFAULT 0.1,
    topics      TEXT,              -- JSON array
    source      TEXT,              -- chat/team/dream/evolution
    created_at  DATETIME,
    last_access DATETIME,
    access_count INTEGER DEFAULT 0,
    consolidated BOOLEAN DEFAULT 0, -- 是否已被整合到 L3
    archived    BOOLEAN DEFAULT 0
);

CREATE TABLE memory_connections (
    fact_id_a   TEXT REFERENCES memory_facts(id),
    fact_id_b   TEXT REFERENCES memory_facts(id),
    relation    TEXT,              -- "related"/"contradicts"/"causes"/"part_of"
    strength    REAL DEFAULT 0.5,
    created_at  DATETIME
);

CREATE TABLE consolidation_log (
    id          TEXT PRIMARY KEY,
    cycle_date  DATETIME,
    facts_input INTEGER,
    facts_merged INTEGER,
    patterns_found INTEGER,
    contradictions INTEGER,
    duration_ms INTEGER
);
```

#### 与现有系统集成

```
┌───────────────┐     ┌────────────────┐     ┌──────────────────┐
│ TieredStore   │ ──→ │  SQLite L2     │ ──→ │  Markdown L3     │
│ (L1: 情景)    │     │  (中期结构)      │     │  (长期语义)       │
│ BM25 + 衰减   │     │  facts + conn  │     │  consolidated.md │
└───────────────┘     └────────────────┘     └──────────────────┘
        ↑                     ↑                      ↑
        │                     │                      │
   每次对话后            Dreaming 触发           周期整合
   自动摄入             增量蒸馏                 归档沉降
```

---

## 四、Dreaming 机制增强

### 4.1 现有 Dreaming 的问题

| 问题 | 当前行为 | 改进方向 |
|:--:|:--:|:--:|
| **触发条件单一** | minHours + minSessions 双门槛 | 增加"重要事件触发"和"知识量触发" |
| **整合质量差** | 本地模式仅拼接+去重 | 分类蒸馏 + 矛盾检测 + 模式识别 |
| **不回注 TieredStore** | 整合结果只写 markdown | 整合后同步回写 L1/L2 |
| **CLI 模式缺失** | 仅飞书路径完整接入 | 统一 Dreaming 入口 |
| **无周期整合** | 每次独立整合 | 增加每日蒸馏 + 每周深度整合 |

### 4.2 增强的 Dreaming 流程

```
                    ┌─────────────────────┐
                    │  触发条件检查         │
                    │  • 时间间隔 ≥ MinHours│
                    │  • 会话数 ≥ MinSess   │
                    │  • 重要事件发生        │
                    │  • L1 容量 > 80%      │
                    └──────────┬──────────┘
                               │
                    ┌──────────▼──────────┐
                    │  Phase 1: 摄入分类    │
                    │  • 提取 L1 新条目     │
                    │  • 分类 + 评分        │
                    │  • 打标签             │
                    └──────────┬──────────┘
                               │
                    ┌──────────▼──────────┐
                    │  Phase 2: 蒸馏整合    │
                    │  • 语义去重           │
                    │  • 矛盾检测           │
                    │  • 模式识别           │
                    │  • 关联发现           │
                    └──────────┬──────────┘
                               │
                    ┌──────────▼──────────┐
                    │  Phase 3: 持久化      │
                    │  • L2 写入 SQLite     │
                    │  • L3 更新 markdown   │
                    │  • 回注 L1 增强索引    │
                    │  • 记录整合日志        │
                    └──────────┬──────────┘
                               │
                    ┌──────────▼──────────┐
                    │  Phase 4: 遗忘管理    │
                    │  • Ebbinghaus 衰减    │
                    │  • 间隔重复强化        │
                    │  • 低保留归档          │
                    │  • 永久豁免检查        │
                    └─────────────────────┘
```

### 4.3 智能触发策略

```go
type DreamTrigger struct {
    // 原有条件
    MinHours    float64   // 最小间隔小时数 (默认 12, 从 24 降低)
    MinSessions int       // 最小会话数 (默认 3, 从 5 降低)
    
    // 新增: 事件驱动触发
    ImportantEventThreshold float64  // 重要性 ≥ 0.8 的事件自动触发
    
    // 新增: 容量驱动触发
    L1CapacityThreshold float64     // L1 使用率 > 80% 时触发
    
    // 新增: Compact 前置触发
    PreCompactDream bool            // AutoCompact 前先做一次快速蒸馏
}
```

---

## 五、跨系统统一

### 5.1 Evolution × Dreaming 交叉学习

```go
// 团队完成后，成功经验自动进入 Dreaming 流程
func (e *EvolutionEngine) LearnFromTeam(team *ProductionTeam) {
    // 现有: 记录 trajectory + experience
    
    // 新增: 将高质量经验注入 Dreaming 的 SessionRecord
    for _, exp := range highQualityExperiences {
        dreamer.RecordSession(SessionRecord{
            Summary:    exp.Content,
            Topics:     exp.Tags,
            Importance: exp.Quality,
            Source:     "evolution:" + team.Name,
        })
    }
}
```

### 5.2 Blackboard → L2 持久化

团队执行过程中 Blackboard 的关键决策自动沉降到 L2:

```go
func (bb *Blackboard) FlushToMemory(store *MemoryStore) {
    for _, entry := range bb.entries {
        if entry.Category == "decision" || entry.Category == "artifact" {
            store.Ingest(MemoryFact{
                Content:    entry.Value,
                Category:   categorize(entry),
                Importance: estimateImportance(entry),
                Source:     "blackboard:" + bb.teamName,
                Topics:     []string{entry.Key},
            })
        }
    }
}
```

### 5.3 CLI 模式完整接入

```go
// cmd/claude-go/main.go — buildEngine() 中增加 Dreaming 初始化
func buildEngine() (*engine.QueryEngine, error) {
    // ... 现有代码 ...
    
    // 新增: 统一 Dreaming 入口
    dreamer := dreaming.New(dreaming.DreamConfig{
        Enabled:    true,
        MinHours:   12,
        MinSessions: 3,
        MemoryDir:  filepath.Join(stateDir, "memory"),
    })
    if apiClient != nil {
        dreamer.SetAPIClient(apiClient)
    }
    eng.SetDreamer(dreamer)
}
```

---

## 六、Compact 防失忆增强

### 6.1 Compact 前快速蒸馏 (PreCompact Distillation)

在 AutoCompact 触发前，先提取即将被压缩掉的消息中的关键信息:

```go
func (c *Compactor) AutoCompact(messages []types.Message, ...) {
    // 新增: 压缩前蒸馏
    if c.memoryStore != nil {
        facts := SmartExtractKeyFacts(messagesToCompress)
        for _, fact := range facts {
            c.memoryStore.Ingest(MemoryFact{
                Content:    fact,
                Category:   "context",
                Importance: 0.6,
                Source:     "pre_compact",
            })
        }
    }
    
    // 现有: 执行压缩
    // ...
}
```

### 6.2 Compact 摘要质量提升

```go
type CompactConfig struct {
    // 现有
    TokenThreshold float64
    
    // 新增: 保护性压缩
    ProtectedCategories []string  // 这些类别的消息不压缩: ["decision", "error_fix"]
    MinRetainMessages   int       // 至少保留最近 N 条完整消息 (默认 6, 从 4 提升)
    FactExtractionMode  string    // "smart" (LLM) | "heuristic" (规则) | "hybrid"
}
```

---

## 七、实现计划

### Phase 1: 基础设施 (本次实现)

| 组件 | 文件 | 描述 |
|:--:|:--:|:--:|
| MemoryFact 类型 | `pkg/memory/fact.go` | 统一记忆单元 + 分类 + 衰减 |
| MemoryStore | `pkg/memory/store.go` | SQLite 中期存储 + CRUD |
| MemoryIngestor | `pkg/memory/ingestor.go` | 自动摄入 + 分类 + 评分 |
| Dreaming 增强 | `pkg/dreaming/consolidator.go` | 增量蒸馏 + 矛盾检测 |
| Ebbinghaus 衰减 | `pkg/memory/decay.go` | 分类衰减 + 间隔重复 |
| CLI 接入 | `cmd/claude-go/main.go` | 统一 Dreaming 入口 |
| PreCompact | `pkg/compact/compact.go` | 压缩前蒸馏 |

### Phase 2: 进阶功能 (后续迭代)

| 组件 | 描述 |
|:--:|:--:|
| 语义检索 | embedding 向量索引 (HNSW) |
| 跨团队共享 | 全局 L3 知识库 |
| 模式识别 | LLM 驱动的行为模式发现 |
| 关联图谱 | memory_connections 图遍历 |

---

## 八、预期效果

| 指标 | 当前 | 目标 |
|:--:|:--:|:--:|
| **跨会话记忆保留率** | ~0% (CLI) / ~40% (飞书) | **≥80%** |
| **Compact 信息丢失率** | ~30% (有损摘要) | **≤5%** (预提取) |
| **记忆检索准确率** | BM25 only (~60%) | **混合检索 ≥85%** |
| **Dreaming 整合质量** | 简单拼接 | **分类蒸馏 + 矛盾检测** |
| **遗忘管理** | 统一阈值 | **分类 Ebbinghaus 衰减** |
| **团队经验复用** | 隔离 | **自动交叉学习** |

---

## 参考文献

1. MemoryOS: Hierarchical Memory for AI Agents (EMNLP 2025)
2. FluxMem: Choosing How to Remember — Adaptive Memory Structures for LLM Agents (2026)
3. LLM Agent Memory: A Survey from a Unified Representation–Management Perspective (2026.03)
4. Memory in the Age of AI Agents (2026.01)
5. Google Always-On Memory Agent (2026.03, open-sourced)
6. OpenClaw Memory Architecture (MEMORY.md + DREAMS.md)
7. DeepSeek V4 Engram Conditional Memory (2026.02)
8. Qwen 3.6-Plus preserve_thinking (2026.03)
9. MiniMax M2.7 Self-Evolving Memory (2026)
10. GLM 5.1 Intelligent Caching for 8-Hour Execution (2026.04)
