# Novel-v2 小说写作团队设计文档

> 版本: 1.0 | 日期: 2026-04-05 | 状态: 设计完成

## 1. 业界调研总结

### 1.1 学术研究

| 论文/框架 | 来源 | 核心贡献 |
|:---|:---|:---|
| **Agents' Room** | ICLR 2025 | 多Agent协作叙事生成, Freytag金字塔分阶段（Exposition→Rising→Climax→Resolution→Falling）, 共享草稿板 |
| **DOC** | ACL 2023 | 层次化大纲控制: Detailed Outliner→Detailed Controller, 比 Re3 提升 22.5% plot coherence |
| **RecurrentGPT** | ICLR 2024 | 模拟 RNN 长短期记忆机制, 自然语言摘要替代向量状态, 支持无限长度续写 |
| **TreeWriter** | arXiv 2025 | 树状层次编辑器, 多粒度规划 (Book→Part→Chapter→Scene) |
| **Survey on LLMs for Story Generation** | EMNLP 2025 | 两大范式: 独立生成 vs 作者辅助协作, 核心挑战: 叙事连贯性/角色一致性/情节多样性 |
| **WritingBench** | NeurIPS 2025 | 6大写作域+100子域评估基准, 动态实例评分标准 |

### 1.2 开源项目

| 项目 | 语言 | 架构特点 | 亮点 |
|:---|:---|:---|:---|
| **NovelGenerator v4.1** | TypeScript | 三Agent: Structure→Character→Scene, Slot填充+合成 | 实时验证/PDF+EPUB导出 |
| **Morpheus** | TS+Python | 多Agent协作, 三层记忆(L1/L2/L3), 知识图谱 | 批量生成/章节编辑/追溯重放 |
| **Postwriter** | Python | 多遍工程: 规划→并行起草→5硬验+10软评→修复循环→54种文学手法分析 | 80K字长篇/PostgreSQL+Redis |
| **ainovel-cli** | **Go** | Novel Harness: Coordinator→Architect→Writer→Editor, 滚动弧计划 | **500+章/7维审查/检查点恢复** |

### 1.3 商业方案

| 平台 | 架构 | 效果 |
|:---|:---|:---|
| **马良写作** | 7Agent (主管/结构/蓝图/生成/设定/一致性/纠错) | 设定一致性+67%, 情节连贯+43% |
| **腾讯 WorkBuddy** | 子Agent异步启动+独立会话, send_message通信 | 企业级多Agent持久化状态 |

### 1.4 经典小说理论

| 理论 | 要点 | LLM 映射 |
|:---|:---|:---|
| **三幕式** | Setup(25%)→Confrontation(50%)→Resolution(25%) | 阶段权重/节奏控制 |
| **英雄之旅** | 12阶段: 普通世界→召唤→拒绝→导师→跨越→考验→深渊→回报→归来→蜕变 | 情节节点规划 |
| **雪花法** | 分形展开: 一句话→段落→大纲→场景→正文 | Outliner迭代展开 |
| **Freytag金字塔** | Exposition→Rising Action→Climax→Falling Action→Dénouement | 章节张力曲线 |
| **Save the Cat** | 15拍节奏板: Opening Image→Theme→Catalyst→Debate→Break2→B-Story→Midpoint→All is Lost→Finale | 精确节奏锚点 |

---

## 2. 设计方案

### 2.1 核心理念

**Novel Harness + Evolution = 自演进长篇小说引擎**

- 借鉴 ainovel-cli 的 **Novel Harness** 架构 (scaffolding/runtime 分离)
- 融合现有框架的 **EvolutionEngine** (轨迹记录→经验提炼→检索注入)
- 复用现有基础设施: CheckpointStore / AdaptiveTerminator / AgentPool / Blackboard

### 2.2 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                     novel-v2 工作流                          │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  Phase 1: 世界构建 (World Building)                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐     │
│  │ story-planner │→│ world-builder │→│ char-designer │     │
│  │ 故事策划师    │  │ 世界观构建师  │  │ 角色设计师    │     │
│  └──────────────┘  └──────────────┘  └──────────────┘     │
│                                                             │
│  Phase 2: 大纲设计 (Outline Design)                         │
│  ┌──────────────────────────────────────────────┐          │
│  │ outline-architect (大纲架构师)                 │          │
│  │ 层次化展开: 一句话→卷→章→场景, Freytag张力曲线 │          │
│  └──────────────────────────────────────────────┘          │
│                                                             │
│  Phase 3: 章节写作循环 (Chapter Writing Loop)               │
│  ┌──────────┐     ┌──────────┐     ┌──────────────┐       │
│  │ novelist  │────→│ editor   │────→│ continuity-  │       │
│  │ 小说家    │←────│ 编辑     │←────│ checker      │       │
│  │ (Writer)  │     │ (Review) │     │ (一致性检查)  │       │
│  └──────────┘     └──────────┘     └──────────────┘       │
│       ↑ AdaptiveTerminator 控制对抗轮数                     │
│       ↑ 每完成一章 → CheckpointStore 保存                   │
│       ↑ Evolution: 风格/节奏经验 → 下一章注入               │
│                                                             │
│  Phase 4: 全书整合 (Book Assembly)                          │
│  ┌──────────────────────────────────────────────┐          │
│  │ book-assembler (全书整合师)                    │          │
│  │ 统稿/前后呼应/伏笔回收/节奏调整/最终输出       │          │
│  └──────────────────────────────────────────────┘          │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### 2.3 角色定义 (6个专业角色)

| 角色 | 职责 | 关键能力 |
|:---|:---|:---|
| **story-planner** | 解读用户需求, 确定题材/风格/基调/篇幅/目标读者 | 文学素养/商业嗅觉/需求分析 |
| **world-builder** | 构建世界观: 时代/地理/社会/魔法体系/科技/规则 | 世界设定一致性/设定文档 |
| **char-designer** | 设计角色: 核心设定/性格弧光/关系网络/成长轨迹 | 角色心理学/人物弧光/对话风格 |
| **outline-architect** | 设计大纲: 卷→章→场景, 张力曲线, 伏笔网络 | 三幕式/雪花法/节奏控制 |
| **novelist** | 核心写作: 将大纲场景展开为正文, 包含叙事/对话/描写/心理 | 文学创作/风格把控/节奏感 |
| **editor** | 审查+修改建议: 文学质量/一致性/节奏/可读性 | 编辑判断力/7维审查标准 |

### 2.4 执行模式: `novel_writing`

新增工作流模式 `novel_writing`, 由专用编排器 `executeNovelWriting` 驱动。

#### Phase 1: 世界构建 (串行, 每阶段依赖前一阶段)

1. **story-plan**: 策划师输出故事蓝图 (JSON)
   - 题材/风格/篇幅/章节数/目标字数
   - 叙事视角 (第一/第三人称, 全知/限制)
   - 核心冲突/主题
2. **world-build**: 世界观构建 (Markdown)
   - 时空背景/社会结构/科技或魔法体系/日常生活
3. **char-design**: 角色设计 (JSON Array)
   - 每个角色: 姓名/年龄/外貌/性格/动机/弱点/说话方式/关系

#### Phase 2: 大纲设计 (依赖 Phase 1 全部完成)

4. **outline-design**: 层次化大纲 (JSON)
   - 卷级: 每卷的核心冲突+解决
   - 章级: 每章的场景列表+POV角色+情绪弧线+张力值(1-10)
   - 伏笔网络: {伏笔ID, 埋入章, 回收章, 状态}

#### Phase 3: 章节写作循环 (核心对抗循环)

对每一章执行:
1. **novelist** 根据大纲+上下文写出本章正文 (2000-3000字)
2. **editor** 7维审查 → JSON 评分
3. AdaptiveTerminator 判断是否通过/继续/终止
4. 通过 → CheckpointStore 保存 → Evolution 记录
5. 进入下一章

**滚动上下文策略** (参考 RecurrentGPT + ainovel-cli):
- 全局: 世界观+角色卡+大纲 (始终注入)
- 近期: 前2章全文 (sliding window)
- 远期: 更早章节的摘要 (progressive summarization)
- 状态: 角色当前状态追踪表 (位置/情绪/物品/关系变化)

#### Phase 4: 全书整合

5. **book-assembler** 通读所有章节, 输出:
   - 统一风格/修正用词不一致
   - 伏笔回收验证
   - 章节间过渡平滑度
   - 最终完整稿件

### 2.5 自演进机制

```
┌──────────────┐
│ 完成每一章后: │
│              │
│ 1. RECORD    │→ 记录本章轨迹 (novelist 输入/输出, editor 评分)
│ 2. DISTILL   │→ 提炼写作经验 ("这种场景适合慢节奏叙事")
│ 3. RETRIEVE  │→ 下一章写作前, 检索相关写作经验注入
│ 4. EVOLVE    │→ 根据 editor 评分更新经验质量分
└──────────────┘
```

- **跨章学习**: novelist 写第 N+1 章时, 注入第 N 章被 editor 高评分的写作模式
- **风格校准**: 如果连续多章出现"对话过多/描写不足", 自动调整 prompt 强调平衡
- **角色一致性**: editor 检查时与角色设定卡做交叉验证, 不一致则强制修正

### 2.6 七维审查标准 (Editor)

| 维度 | 权重 | 说明 |
|:---|:---|:---|
| **narrative_quality** | 20% | 叙事技巧、文笔流畅度、修辞手法 |
| **character_consistency** | 20% | 角色言行与设定卡一致性 |
| **plot_coherence** | 15% | 情节逻辑、因果关系、大纲对齐 |
| **pacing** | 15% | 节奏控制: 张弛有度, 不拖沓不仓促 |
| **dialogue_quality** | 10% | 对话个性化、自然度、推进情节 |
| **world_consistency** | 10% | 世界观设定一致性 |
| **emotional_arc** | 10% | 情感张力曲线是否符合预期 |

通过门槛: 所有维度 >= 6 且加权平均 >= 7

### 2.7 输出格式

- 每章: `chapter_{N}.md` (正文+章节标题)
- 全书: `NOVEL.md` (合并稿)
- 元数据: `novel_meta.json` (世界观/角色/大纲/章节状态)
- 审查报告: `review_report.md` (各章评分汇总)

---

## 3. 与现有架构的整合

### 3.1 复用的核心机制

| 机制 | 来源 | 用途 |
|:---|:---|:---|
| CheckpointStore | workflow.go | 每章完成保存检查点, 支持中断恢复 |
| AdaptiveTerminator | adversarial.go | 控制 novelist↔editor 对抗轮数 (1-3轮/章) |
| AgentPool.AutoScale | pool.go | 动态调整 Agent 池 (6角色) |
| Blackboard | teams.go | Agent 间共享上下文 (世界观/角色/大纲) |
| EvolutionEngine | evolution.go | 跨章写作经验学习 |
| executeStageWithRetry | workflow.go | 阶段级重试+指数退避 |
| SummarizeOldOutput | workflow.go | 远期章节渐进式摘要 |

### 3.2 新增代码

| 文件 | 内容 |
|:---|:---|
| `workflow_novel_v2.go` | novel-v2 WorkflowDef + executeNovelWriting 编排器 + 7维评分解析 |
| `roles.go` (追加) | 6个小说写作角色定义 |
| `workflow.go` (修改) | GetWorkflow/ListWorkflows 注册 novel-v2 |
| `intent.go` (修改) | wfDetect 添加小说相关关键词 |
| `teams.go` (修改) | CreateTeam 错误提示添加 novel-v2 |

### 3.3 意图关键词

```go
"novel-v2": {"写小说", "小说创作", "创作小说", "长篇小说", "短篇小说",
    "故事创作", "写故事", "编故事", "网文", "写网文", "连载",
    "科幻小说", "言情小说", "悬疑小说", "奇幻小说", "武侠小说",
    "小说续写", "续写", "接着写", "写下一章"}
```

---

## 4. 安全与限制

- **内容安全**: novelist prompt 包含内容安全约束 (不产生违规内容)
- **Token 控制**: 每章限制 3000 字, 防止单轮 token 爆炸
- **上下文管理**: 渐进式摘要防止 context window 溢出
- **成本估算**: 10章小说 ≈ 60-80次 LLM 调用, 约 $2-5 (按 Claude Haiku 估算)

---

## 5. 参考文献

1. Huot et al., "Agents' Room: Narrative Generation through Multi-Step Collaboration", ICLR 2025
2. Yang et al., "DOC: Improving Long Story Coherence with Detailed Outline Control", ACL 2023
3. Zhou et al., "RecurrentGPT: Interactive Generation of Arbitrarily Long Text", ICLR 2024
4. Teleki et al., "A Survey on LLMs for Story Generation", EMNLP 2025 Findings
5. Ma et al., "Text-to-Text Automatic Story Generation: A Survey", EACL 2026
6. KazKozDev/NovelGenerator: https://github.com/KazKozDev/NovelGenerator
7. voocel/ainovel-cli: https://github.com/voocel/ainovel-cli
8. papysans/Morpheus: https://github.com/papysans/Morpheus
9. X-PLUG/WritingBench, NeurIPS 2025
