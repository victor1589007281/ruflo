# 应聘招聘团队设计方案

> **版本**: 1.0 | **日期**: 2026-04-21
> **定位**: AI 驱动的求职准备全流程平台 (JD 分析 → 准备计划 → 模拟面试)

---

## 一、调研来源

| 领域 | 参考 | 核心启发 |
|:----:|:----:|:--------:|
| **JD 分析** | Jobscan, ResumeWorded | ATS 关键词, 需求解析 |
| **面试系统** | Google Interview Warmup, Pramp, Exponent | 行为/技术/Case 三类 |
| **简历优化** | STAR 框架, Zety, Resume.io | 量化成果, 结构优化 |
| **AI 招聘** | FAccT/CHI 公平性研究, EU AI Act | 偏见检测, 公平审计 |
| **学习路径** | 间隔重复, 刻意练习理论 | 高效备面策略 |

---

## 二、设计原则

```
┌─────────────────────────────────────────────────────────────┐
│  P1: JD 深度解析 — 自动提取硬/软技能, 优先级, 隐含要求        │
│  P2: GAP 分析 — 匹配简历与 JD, 找出差距和优势                │
│  P3: 行动计划 — 日历化的学习/准备计划                        │
│  P4: 模拟面试 — 行为/技术/Case 三种模式, 评分反馈             │
│  P5: 公平性 — 不做歧视性建议, 不虚构经历                     │
└─────────────────────────────────────────────────────────────┘
```

---

## 三、工作流 DAG

```
ParseJD ──┬──▶ ExtractRequirements ──┐
           │                          ├──▶ GapAnalysis ──▶ PrepPlanGen ──┬──▶ ResumeOptimize
ParseResume┘                          │                                  ├──▶ StoryBank (STAR)
                                      │                                  └──▶ StudyPlan
                                      │
                                      └──▶ MockInterview ──┬──▶ BehavioralRound
                                                           ├──▶ TechnicalRound
                                                           └──▶ CaseRound
                                                                    │
                                                              ScoreAndFeedback ──▶ ImprovementReport
```

### 阶段详解

| # | 阶段 | 角色 | 说明 |
|:-:|:-----|:-----|:-----|
| 1 | **ParseJD** | jd_analyst | 解析职位描述, 提取岗位信息 |
| 2 | **ParseResume** | resume_parser | 解析简历, 提取技能和经历 |
| 3 | **ExtractRequirements** | requirement_extractor | 提取必备/加分技能, 经验要求, 文化匹配 |
| 4 | **GapAnalysis** | gap_analyzer | 技能/经验差距分析, 生成匹配度评分 |
| 5 | **PrepPlanGen** | planner | 生成备面总体计划 |
| 6 | **ResumeOptimize** | resume_coach | 简历优化: 关键词匹配, STAR 重写, ATS 友好 |
| 7 | **StoryBank** | story_coach | 构建 STAR 故事库: 每个能力点准备 2-3 个故事 |
| 8 | **StudyPlan** | study_planner | 技术/知识补充计划: 日历化+间隔重复 |
| 9 | **MockInterview** | interviewer | 模拟面试调度器 |
| 10 | **BehavioralRound** | behavioral_coach | 行为面试: 情景题+追问+评分 |
| 11 | **TechnicalRound** | technical_coach | 技术面试: 算法/系统设计/代码审查 |
| 12 | **CaseRound** | case_coach | 案例面试: 商业分析/估算/框架 |
| 13 | **ScoreAndFeedback** | evaluator | 综合评分: 表达/逻辑/专业/沟通 |
| 14 | **ImprovementReport** | reporter | 输出改进报告 + 后续练习计划 |

### 模拟面试评分维度

| 维度 | 权重 | 评分标准 |
|:----:|:----:|:--------:|
| 专业知识 | 30% | 回答的技术准确性和深度 |
| 逻辑思维 | 25% | 结构化思考, 问题分解能力 |
| 沟通表达 | 20% | 清晰度, 简洁性, 条理性 |
| 行为素质 | 15% | STAR 完整性, 真实性, 反思 |
| 文化匹配 | 10% | 价值观/工作风格/团队适配 |

---

## 四、输出格式

### GAP 分析报告
```json
{
  "match_score": 78,
  "strengths": ["5年 Go 开发", "分布式系统", "团队管理"],
  "gaps": [
    {"skill": "Kubernetes", "priority": "high", "study_time": "2 weeks"},
    {"skill": "CICD Pipeline", "priority": "medium", "study_time": "1 week"}
  ],
  "hidden_requirements": ["需要英文工作环境", "创业文化偏好"]
}
```

### 面试评估报告
```json
{
  "overall_score": 72,
  "dimensions": {
    "professional": 80,
    "logic": 75,
    "communication": 65,
    "behavioral": 70,
    "culture_fit": 72
  },
  "top_improvements": ["回答过长, 练习 2 分钟限制", "STAR 缺少量化结果"],
  "practice_plan": [...]
}
```

---

## 五、编排引擎集成

- ParseJD 和 ParseResume 并行 (无依赖)
- 3 种面试模式并行, 由 DAG 自动调度
- `CompositeRunner` 实现面试多轮追问 (追问 → 回答 → 评分 → 决定是否继续)
- Blackboard 存储候选人画像, 各阶段共享
