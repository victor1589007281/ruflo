# Claude-Go Dashboard 可视化方案设计

> **版本**: v1.0 | **日期**: 2026-04-18
> **定位**: 只读的本地 Web Dashboard, 专注于 claude-go 运行数据的可视化、分析与诊断
> **目标用户**: 使用 claude-go CLI/飞书模式的开发者/运营人员

---

## 1. 背景与目标

### 1.1 现状盘点

`claude-go` 目前已经沉淀了丰富的可观测数据, 但都散落在文件系统中, 缺少统一的可视化入口:

| 模块 | 数据路径 (默认 `.claude-go/` 下) | 内容 |
|:---|:---|:---|
| Teams | `teams/<name>/team.json` + `blackboard.json` + `checkpoints.json` + `REPORT.md` | 团队状态/阶段/Agent 结果/黑板 |
| Metrics | `metrics/<module>.jsonl` | 持续观测指标 (team/evolution/dreaming/memory/task) |
| Evolution | `evolution/experiences.json` + `trajectories.json` | 经验库、轨迹、质量分、使用率 |
| Cron | `cron/cron_jobs.json` | 定时任务定义 + 执行历史 |
| Dreaming | `memory/dreaming/dream-*.md` + `memory/index.md` | 睡眠整理产出 |
| Memory | `memory/*.md` + `memory/consolidated.md` | 记忆条目 |
| Tasks | `tasks/tasks.json` | V2 Task DAG |
| Logs | `logs/team-runs.jsonl` + `events.jsonl` | 结构化运行日志 |

**痛点**:
- 开发者通过 `cat` / `jq` 翻 JSON 才能看运行情况
- 跨模块指标无法横向对比 (团队质量 vs 进化使用率 vs 梦境压缩率)
- 对抗循环 5 轮的质量变化曲线需要手工拼数据
- 新用户看不到 claude-go "活着"的感觉 (Cron/Dreaming/Evolution 默默运行)

### 1.2 设计目标

| 目标 | 说明 |
|:---|:---|
| **零部署** | `claude-go dashboard` 一条命令拉起, 无需额外依赖/配置 |
| **只读安全** | 仅读取本地文件系统数据, 不暴露任何写操作到外部 |
| **离线可用** | 前端所有资源 embed 进二进制, 无 CDN 依赖, 离线可跑 |
| **实时刷新** | 关键页面 5s 轮询, 保证看到最新的团队/指标 |
| **可分析** | 不只是列数据, 要提供趋势图、对比、质量分析 |
| **低侵入** | 不改动现有数据写入逻辑, 只从文件系统扫描读取 |

---

## 2. 业界调研与论文参考

### 2.1 可视化工具借鉴

| 工具 | 借鉴点 | 应用到 claude-go |
|:---|:---|:---|
| **Grafana** | 时序图表 + 多维聚合 + 时间范围 | Metrics 页的多指标趋势图 |
| **MLflow Tracking UI** | Run 对比 + Parameter/Metric 侧边栏 | 团队 Run 列表 + 逐轮指标对比 |
| **Weights & Biases** | 训练过程可视化 + 指标告警 | 对抗循环质量曲线 + 退化告警 |
| **LangSmith / LangFuse** | LLM Agent trace + token 成本 | 团队阶段 timeline + 产出预览 |
| **Airflow UI** | DAG 图 + Gantt 图 | Workflow Stage DAG + 执行 Gantt |
| **Kubernetes Dashboard** | 资源树 + 状态颜色编码 | 团队列表 + Agent 状态卡片 |
| **Weights & Biases Runs** | Parallel Coordinates | 多维度团队对比 (workflow × round × pass_rate) |

### 2.2 论文参考

| 论文 | 核心思想 | 可视化启示 |
|:---|:---|:---|
| **AgentBoard** (NeurIPS 2024) | 多维度 Agent 能力评估 | 团队评估 Radar Chart (Correctness/Completeness/Security/Quality/Design) |
| **Tree of Thought** | 思考过程树状展开 | Swarm 分解树可视化 |
| **Reflexion** (2303.11366) | 言语反射 + 情景记忆 | Evolution 经验使用率热力图 |
| **Self-Refine** (2303.17651) | 迭代反馈曲线 | 对抗循环 best-of-N 质量变化图 |
| **Live-Evo** (2026) | 经验权重动态衰减 | Experience Quality 分布直方图 + EMA 趋势 |
| **SIGIR: MARU** (2024) | Multi-Agent Run Understanding | 时间轴视图 + Agent 间消息流 |
| **人脑海马体重放假说** | SWS/REM 睡眠巩固记忆 | Dreaming 周期甘特图 + 重要性分布 |

### 2.3 关键设计原则 (从论文提炼)

1. **Signal-over-noise**: 首页只展示"值得行动"的数据 (在跑的团队 / 最新告警 / 关键指标), 其他进二级页
2. **Time as primary axis**: 几乎所有视图默认时间轴横向 (Timeline/Gantt/Trend 三种形态)
3. **Comparability**: 同一指标支持跨 run / 跨 workflow / 跨 round 对比
4. **Drill-down**: Overview → Module → Run → Stage → Agent 五层下钻路径
5. **Color semantic consistency**: 绿=成功 红=失败 黄=进行中 蓝=信息 紫=学习/进化

---

## 3. 架构设计

### 3.1 总体架构

```
┌───────────────────────────────────────────────────────────────────────┐
│                    claude-go dashboard (单进程)                       │
│                                                                       │
│  ┌──────────────────────────────────────────────────────────────┐     │
│  │                   HTTP Server (net/http)                     │     │
│  │                                                              │     │
│  │   /                → 前端 SPA (embed)                        │     │
│  │   /static/*        → CSS/JS/Font (embed)                     │     │
│  │   /api/overview    → 汇总数据                                │     │
│  │   /api/teams       → 团队列表/详情                           │     │
│  │   /api/metrics     → 时序指标                                │     │
│  │   /api/cron        → Cron 任务                               │     │
│  │   /api/dreaming    → 梦境记录                                │     │
│  │   /api/evolution   → 经验/轨迹                               │     │
│  │   /api/tasks       → Task DAG                                │     │
│  └──────────────────────────────┬───────────────────────────────┘     │
│                                 │                                     │
│  ┌──────────────────────────────▼───────────────────────────────┐     │
│  │            DataProvider (只读聚合器, 内建 TTL 缓存)            │     │
│  │                                                              │     │
│  │   TeamLoader   MetricsLoader   EvolutionLoader               │     │
│  │   CronLoader   DreamingLoader  MemoryLoader                  │     │
│  └──────────────────────────────┬───────────────────────────────┘     │
└─────────────────────────────────┼─────────────────────────────────────┘
                                  │
                                  ▼
                    ┌─────────────────────────────┐
                    │  <project>/.claude-go/       │
                    │    ├── teams/                │
                    │    ├── metrics/              │
                    │    ├── evolution/            │
                    │    ├── cron/                 │
                    │    ├── memory/               │
                    │    └── tasks/                │
                    └─────────────────────────────┘
```

**关键点**:
- **只读**: DataProvider 仅调用 `os.ReadFile` / `os.ReadDir`, 绝不写入
- **内存缓存**: 每个 loader 带 2s TTL 避免高频扫描
- **前端内嵌**: 使用 `embed.FS` 将所有 HTML/CSS/JS 打包进二进制
- **单端口**: 默认 `localhost:7777`, 避免暴露到公网

### 3.2 目录结构 (代码)

```
claude-go/pkg/dashboard/
├── server.go          # HTTP 服务 + 路由
├── handlers.go        # API handler
├── provider.go        # DataProvider 接口 + 缓存
├── loader_teams.go    # 从 teams/ 读取
├── loader_metrics.go  # 从 metrics/*.jsonl 读取
├── loader_evolution.go
├── loader_cron.go
├── loader_dreaming.go
├── loader_memory.go
├── types.go           # API 响应类型
└── web/
    ├── static.go      # embed.FS
    ├── index.html     # 入口 HTML
    ├── app.js         # SPA 逻辑
    ├── style.css      # 样式
    └── chart.min.js   # Chart.js 本地副本 (~55KB)
```

### 3.3 CLI 集成

```
claude-go dashboard [--port 7777] [--state-dir .claude-go] [--open]
```

| Flag | 默认 | 说明 |
|:---|:---|:---|
| `--port` | 7777 | HTTP 端口 |
| `--state-dir` | `$CWD/.claude-go` | 数据目录 (通常无需指定) |
| `--open` | false | 启动后自动在浏览器打开 |
| `--no-cache` | false | 关闭 TTL 缓存 (调试用) |

---

## 4. 页面设计 (核心视图)

### 4.1 Overview (首页)

**信息密度目标**: 30 秒看懂系统当前状态

```
┌──────────────────────────────────────────────────────────────────┐
│  🤖 Claude-Go Dashboard              ● Online  |  .claude-go/    │
├──────────────────────────────────────────────────────────────────┤
│  ┌───────────┐ ┌───────────┐ ┌───────────┐ ┌───────────┐          │
│  │ 团队总数   │ │ 运行中     │ │ Cron      │ │ 经验库     │          │
│  │   24      │ │    2      │ │   5       │ │   128     │          │
│  │ ↑3 this wk │ │ 2 success │ │ 3 active  │ │ q=0.72    │          │
│  └───────────┘ └───────────┘ └───────────┘ └───────────┘          │
│                                                                  │
│  📊 最近 7 天团队运行趋势 (每日成功/失败/平均耗时)                  │
│  ┌──────────────────────────────────────────────────────────────┐ │
│  │  [stacked bar + line chart]                                  │ │
│  └──────────────────────────────────────────────────────────────┘ │
│                                                                  │
│  🔥 正在运行  ┃  🕒 最近完成  ┃  ⚠️ 告警/降级                      │
│  ─────────    ─────────         ─────────                        │
│  go-dev-7143  go-trading-v2-9 … ⚠ team_build_pass_rate=0         │
│  10 分钟      10:40            5 rounds all failed to compile    │
└──────────────────────────────────────────────────────────────────┘
```

**数据来源**:
- `teams/*/team.json` 扫描
- `metrics/team.jsonl` 按日聚合
- `metrics/*.jsonl` 的 TrendAlert

### 4.2 Teams — 团队运行 (核心页, 两栏)

**左侧 Master**: 团队列表 (过滤/搜索/状态徽章)
**右侧 Detail**: 选中团队详情 (多 tab)

```
┌────────────────────────┬─────────────────────────────────────────┐
│ 🔍 搜索  [all ▾]       │ go-dev-7143  ✅ completed  41m          │
├────────────────────────┤ workflow=dev  objective=...             │
│ ✅ go-trading-v2-9181   ├─────────────────────────────────────────┤
│   trading-v2 13m       │ [Timeline] [Stages] [Agents] [Eval]     │
│                        │ [Blackboard] [Report] [Metrics]         │
│ ❌ go-dev-7143          │                                         │
│   dev 41m  4轮全失败   │ ┌─────────────────────────────────────┐ │
│                        │ │ ═══ Gantt 时间轴 ═══                │ │
│ 🔄 go-creative-v2-3237  │ │ design-api     ▮▮▮    12s          │ │
│   creative-v2 running  │ │ implement      ▮▮▮▮▮▮▮▮  2m 20s    │ │
│                        │ │ review         ▮▮  8s              │ │
│                        │ │ build-gate     ▮▮▮▮  18s           │ │
│ ...                    │ └─────────────────────────────────────┘ │
└────────────────────────┴─────────────────────────────────────────┘
```

**Tab 设计**:

| Tab | 可视化 | 数据源 |
|:---|:---|:---|
| **Timeline** | Gantt 图 (stage × 时间) | `team.json.Stages[].StartedAt/Duration` |
| **Stages** | 表格 + 产出预览 + 展开 | `team.json.Stages[]` |
| **Agents** | 卡片 (头像/角色/状态/最终产出) | `team.json.Agents` |
| **Eval** | 对抗循环 Radar + 逐轮曲线 | `metrics/team.jsonl` by `run_id` |
| **Blackboard** | 键值时间线 | `blackboard.json` |
| **Report** | Markdown 渲染 | `REPORT.md` |
| **Metrics** | 本 run 的全部指标 | `metrics/team.jsonl` filter |

**Eval Tab (重点)**:
对于 dev 工作流的对抗循环, 绘制:
```
┌─ Correctness / Completeness / Security / CodeQuality / DesignAlign 5 维度 ─┐
│                                                                            │
│  Round 1  ●────●                                                           │
│  Round 2     ●─●              ← 质量下降                                   │
│  Round 3     ●─●                                                           │
│  Round 4     ●●                                                            │
│  Round 5     ●●   best-of-N → Round 1 (⏪ rollback)                        │
└────────────────────────────────────────────────────────────────────────────┘
```

### 4.3 Metrics — 持续观测指标

| 区块 | 说明 |
|:---|:---|
| Module Selector | Tab 切换: team / evolution / dreaming / memory / task |
| Time Range | 1h / 24h / 7d / 30d / all |
| 指标网格 | 每个指标一个卡片 (sparkline + Last/Avg/Min/Max + Trend 箭头) |
| 大图 | 点击卡片展开大图 (Chart.js line) |
| Alerts | 右侧显示 TrendAlert (如 "team_build_pass_rate 显著下降") |

### 4.4 Cron — 定时任务

```
┌──── 任务列表 ────┐  ┌──── 选中任务详情 ────┐
│ ✅ dev-daily      │  │ dev-daily (cron-1)  │
│   0 9 * * 1-5    │  │ 每工作日 9:00       │
│ ✅ wiki-clean     │  │                     │
│   0 2 * * *      │  │ 类型: workflow      │
│ ⏸️ debug         │  │ 工作流: dev         │
│   */5 * * * *    │  │ 目标: 日报整理      │
└──────────────────┘  │                     │
                      │ 统计 (柱状)          │
                      │ 本周 5 次 → 100% 成功│
                      │ 本月 21 次 → 95% 成功│
                      │                     │
                      │ 最近 10 次执行 (列表)│
                      └─────────────────────┘
```

### 4.5 Dreaming — 记忆整理机制

```
┌──── 整体状态 ────┐
│ Enabled: ✅       │
│ Last dream: 3h ago│
│ Sessions buffered: 12/5 min │
│ Is dreaming: false│
└──────────────────┘

📊 历次 Dreaming 耗时 / 压缩率 趋势
┌──────────────────────────────────────────────┐
│ [dual-axis line chart]                        │
│ Y1: duration (sec)   Y2: compression ratio    │
└──────────────────────────────────────────────┘

📚 Dream Logs (按时间倒序)
┌─────────────────────────────────────────────┐
│ 20260418-092301 (6s, 14 sessions)           │
│   ▸ 高重要性: 3  中: 5  普通: 6              │
│   ▸ [查看整理后的 consolidated.md]           │
└─────────────────────────────────────────────┘

🧠 当前记忆库
┌─── by topic (treemap) ───┐
│  trading 12  dev 8  ...  │
└──────────────────────────┘
```

### 4.6 Evolution — 进化机制

```
┌── 总览卡片 ──┐
│ Experiences  │  Trajectories  │  Success Rate  │  Utilization
│    128       │     456        │     78%        │     54%
└──────────────┘

📈 经验质量分布 (Histogram, 0.0-1.0)
┌──────────────────────────────────────────────┐
│  ▁ ▁ ▂ ▃ ▅ ▇ █ █ ▇ ▅ ▃  (bell curve)        │
└──────────────────────────────────────────────┘

🔝 Top 经验 (按使用率 × 成功率)
┌─────────────────────────────────────────────┐
│ #42 [coder] 实现 HTTP handler 时应用 context 传播    │
│    usage=24  success=22  quality=0.92               │
│ #17 [general] 并行任务注意资源竞争...                 │
│    usage=18  success=15  quality=0.85               │
└─────────────────────────────────────────────┘

📋 Trajectories 时间流 (列表, 按 TeamName 分组)
┌─────────────────────────────────────────────┐
│ go-trading-v2-9181                          │
│   ✅ scout / researcher / 1m 12s             │
│   ✅ debate / debater / 45s                 │
│   ✅ fuse / analyst / 2m                    │
└─────────────────────────────────────────────┘

📊 Category × Role 使用热力图
             coder  architect  tester  reviewer
role         ▇▇▇    ▅          ▂       ▃
error        ▅      ▇▇         ▃       ▂
general      ▂      ▂          ▂       ▂
```

### 4.7 Quality (可选, 二期)

跨模块综合质量报告, 周报形态:
- Metric trend 的 7-day delta
- Top 3 degrading metrics
- Top 3 successful teams (by stage_pass_rate × output_avg_len)
- Dreaming 压缩效率变化
- Evolution 利用率变化

---

## 5. API 设计

所有 API 返回 JSON, 约定:
- 成功: `{"data": {...}, "meta": {...}}`
- 失败: `{"error": "message"}` + HTTP 状态码

### 5.1 Endpoint 清单

| Method | Path | 说明 | 响应类型 |
|:---|:---|:---|:---|
| GET | `/api/overview` | 首页数据 | `OverviewResp` |
| GET | `/api/teams` | 团队列表 (轻量) | `[]TeamSummary` |
| GET | `/api/teams/:name` | 团队详情 | `TeamDetail` |
| GET | `/api/teams/:name/blackboard` | 黑板 | `Blackboard` |
| GET | `/api/teams/:name/report` | REPORT.md | `{content: string}` |
| GET | `/api/metrics` | 所有模块摘要 | `[]ModuleSummary` |
| GET | `/api/metrics/:module` | 某模块时序原始 | `[]MetricEvent` |
| GET | `/api/cron` | Cron 任务 | `[]CronJob` |
| GET | `/api/dreaming` | Dreaming 状态 + 历史 | `DreamingResp` |
| GET | `/api/evolution` | 经验库 + 轨迹 | `EvolutionResp` |
| GET | `/api/tasks` | Task DAG | `[]Task` |
| GET | `/api/health` | 健康检查 | `{ok: true}` |

### 5.2 关键 Response 结构

```go
type OverviewResp struct {
    StateDir       string            `json:"stateDir"`
    TotalTeams     int               `json:"totalTeams"`
    RunningTeams   int               `json:"runningTeams"`
    TotalExps      int               `json:"totalExperiences"`
    ActiveCrons    int               `json:"activeCrons"`
    RecentRuns     []TeamSummary     `json:"recentRuns"`
    DailyRuns      []DailyBucket     `json:"dailyRuns"` // 最近7天
    Alerts         []string          `json:"alerts"`
}

type TeamSummary struct {
    Name       string     `json:"name"`
    Workflow   string     `json:"workflow"`
    Status     string     `json:"status"`
    Objective  string     `json:"objective"`
    CreatedAt  time.Time  `json:"createdAt"`
    DurationSec float64   `json:"durationSec"`
    StagesTotal int       `json:"stagesTotal"`
    StagesDone  int       `json:"stagesDone"`
}
```

---

## 6. 前端设计

### 6.1 技术栈选择理由

| 候选 | 优点 | 缺点 | 决定 |
|:---|:---|:---|:---|
| React + Vite | 生态丰富 | 构建复杂, 难 embed | ❌ |
| Vue SFC | 组件化强 | 需要 SSR/build | ❌ |
| **Vanilla JS + Web Components** | 零依赖, embed 简单, ~20KB gzipped | 工程化弱 | ✅ |
| Chart.js (vendored) | 单文件, 55KB, 够用 | 非 D3 那么强大 | ✅ |

**选型**: Vanilla JS + Chart.js (vendored) + 手写 Router
理由: 本产品 UI 复杂度中等, 避免 npm/build 带来的分发负担; 所有资源 embed 进 Go 二进制。

### 6.2 视觉风格

**主题**: 深色 (default) + 浅色 (切换)
**色板**:
- 背景: `#0e1117` (GitHub dark 类风格)
- 卡片: `#161b22`
- 强调色: `#3b82f6` (blue) / `#10b981` (green) / `#ef4444` (red) / `#a78bfa` (purple)
- 字体: system-ui, -apple-system, 'PingFang SC', 'Microsoft YaHei'

**布局**: 左侧导航 (72px icon-only) + 顶部工具栏 + 主内容区。移动端折叠为底部 tab。

### 6.3 刷新策略

| 页面 | 刷新方式 | 频率 |
|:---|:---|:---|
| Overview | 轮询 | 10s |
| Teams 列表 | 轮询 | 5s |
| 运行中的 Team 详情 | 轮询 | 3s |
| 已完成的 Team 详情 | 手动 | - |
| Cron | 轮询 | 30s |
| Metrics | 手动/切换时间窗自动 | - |
| Dreaming/Evolution | 轮询 | 30s |

---

## 7. 安全与非目标

### 7.1 安全

- **Bind 到 localhost**: 默认仅监听 `127.0.0.1:7777`, 避免外网暴露
- **只读**: 不提供任何 mutating API (不能通过 dashboard 删除团队/停止 cron/触发 dream)
- **无鉴权**: 本地单用户工具, 不引入 auth 复杂度; 如需远程访问用户自行 `ssh -L` 转发

### 7.2 非目标 (本期不做)

- ❌ 触发 workflow / 停止团队 (留给 CLI)
- ❌ 编辑 cron / 清理记忆 (留给 CLI)
- ❌ 多项目聚合 (本期只读当前 cwd 的 state-dir)
- ❌ 移动端深度适配 (最小化适配, 主打桌面)
- ❌ 鉴权 / 多租户

---

## 8. 实施计划

| 阶段 | 任务 | 产出 |
|:---|:---|:---|
| **P0** | 后端骨架 | `dashboard/server.go` + `/api/health` + embed 占位 |
| **P1** | 核心 API | `/api/overview` + `/api/teams` + `/api/metrics` |
| **P2** | 前端 MVP | Overview + Teams 列表 + Teams 详情 (Timeline/Stages/Report) |
| **P3** | 二级视图 | Cron + Dreaming + Evolution + Metrics 详情 |
| **P4** | 打磨 | Chart.js 图表 + 深色主题 + 告警 |
| **P5** | 验证 | `go build` + `claude-go dashboard` + 浏览器验证 |

---

## 9. 验收标准

1. ✅ `go build ./...` 通过, 所有前端资源 embed 进单二进制
2. ✅ `claude-go dashboard` 能在不破坏现有功能的前提下启动
3. ✅ `curl http://127.0.0.1:7777/api/health` 返回 `{"ok":true}`
4. ✅ 浏览器打开 `http://127.0.0.1:7777` 能看到 Overview, 展示真实团队数据
5. ✅ 能下钻到团队详情, 看到 Stages / Report / Blackboard
6. ✅ Metrics 页能渲染 `metrics/team.jsonl` 中的多指标时序曲线
7. ✅ Cron / Dreaming / Evolution 三个页面都有数据且可视化合理
8. ✅ 无任何写操作命中文件系统 (可通过 `lsof` / 只读挂载验证)

---

## 10. 后续演进 (Roadmap)

| 版本 | 特性 |
|:---|:---|
| v1.1 | Markdown 预览 / 代码高亮 / REPORT.md 目录导航 |
| v1.2 | Parallel Coordinates (团队多维对比) + CSV 导出 |
| v1.3 | SSE 实时推送 (代替轮询) + Team Stage 实时流 |
| v2.0 | "Ask Claude" 面板: 点一下让 claude-go 自己分析数据并给建议 |
| v2.1 | 多项目聚合 (扫描 `~/.claude-go/projects/*`) |

---

**结论**: Dashboard 的定位是"只读诊断镜像", 通过一条命令把分散的可观测数据聚合起来,
为用户提供 "Overview → Module → Run → Stage" 的四级下钻体验。
选择 Vanilla JS + embed 路线, 保证 `claude-go dashboard` 保持"一个二进制、一条命令"的极简分发特性。
