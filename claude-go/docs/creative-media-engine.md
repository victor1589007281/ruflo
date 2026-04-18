# 创意媒体引擎设计文档 — Creative Media Engine

> **版本**: 1.0 | **日期**: 2026-04-18
> **目标**: 扩展创意团队, 使其具备 HTML 网页开发 + 多格式媒体输出能力

---

## 1. 需求概述

### 1.1 核心能力

| 能力 | 输入 | 输出格式 | 优先级 |
|:---|:---|:---|:---:|
| **HTML 网页开发** | 用户创意需求 | 完整 HTML 文件 | P0 |
| **HTML → 图片** | HTML 文件 | PNG, JPEG, SVG | P0 |
| **HTML → PDF** | HTML 文件 | PDF | P0 |
| **HTML → 视频** | HTML (含 CSS 动画) | MP4 | P1 |
| **PPT 制作** | 结构化内容 | PPTX → PDF | P1 |
| **长时间任务拆解** | 复杂创意需求 | 多阶段子任务 | P0 |

### 1.2 参考架构

| 项目 | 核心思想 | 借鉴点 |
|:---|:---|:---|
| **HyperFrames** | HTML-native 视频合成 (CDP BeginFrame + FFmpeg) | 确定性帧捕获、帧复用、音频混合 |
| **Remotion** | React → 帧序列 → FFmpeg 编码 | 帧级控制、流式编码 |
| **chromedp** | Go 原生 CDP 客户端 | Screenshot/PDF/DOM 操作 |
| **CREA** (arXiv:2504.05306) | 多 Agent 协作创意 (概念→生成→评审→优化) | Agent 角色分工 |

---

## 2. 系统架构

```
┌────────────────────────────────────────────────────────────┐
│                    创意团队 (Creative Team)                  │
│                                                              │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
│  │创意策划   │→│任务拆解   │→│内容生成   │→│视觉审查   │   │
│  │Planner   │  │Decomposer│  │Generator │  │Reviewer  │   │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘   │
│                                    │                         │
│                                    ▼                         │
│  ┌──────────────────────────────────────────────────────┐   │
│  │              媒体输出引擎 (MediaEngine)                │   │
│  │                                                        │   │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐            │   │
│  │  │ HTML→PNG  │  │ HTML→PDF  │  │ HTML→MP4  │            │   │
│  │  │ chromedp  │  │ PrintToPDF│  │ frames+   │            │   │
│  │  │           │  │           │  │ ffmpeg    │            │   │
│  │  └──────────┘  └──────────┘  └──────────┘            │   │
│  │                                                        │   │
│  │  ┌──────────┐  ┌──────────┐                           │   │
│  │  │ PPT→PDF   │  │ SVG 导出  │                           │   │
│  │  │ slides    │  │ extract   │                           │   │
│  │  └──────────┘  └──────────┘                           │   │
│  └──────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘
```

---

## 3. 媒体输出引擎 (MediaEngine)

### 3.1 技术栈选择

| 组件 | 工具 | 理由 |
|:---|:---|:---|
| **浏览器渲染** | chromedp (Go CDP) | 纯 Go、无 CGO、成熟稳定 |
| **截图** | CDP Page.captureScreenshot | PNG/JPEG, 支持全页面 |
| **PDF** | CDP Page.printToPDF | 矢量质量, 支持分页 |
| **视频编码** | FFmpeg (exec.Command) | 业界标准, 支持 MP4/WebM |
| **SVG 提取** | 正则 + DOM 解析 | 从 LLM 输出中提取 SVG |
| **PPT** | HTML slides → 截图 → PPTX | 实用 + 高保真 |

### 3.2 渲染流水线

```
HTML 文件
  │
  ├─→ [chromedp] → Screenshot → PNG/JPEG
  │
  ├─→ [chromedp] → PrintToPDF → PDF
  │
  ├─→ [chromedp] → 逐帧截图 → [ffmpeg] → MP4
  │    (CSS 动画: 每帧 seek + screenshot)
  │
  ├─→ [正则提取] → SVG 文件
  │
  └─→ [HTML slides] → 逐页截图 → PPTX (图片幻灯片)
```

### 3.3 关键算法

#### 帧捕获 (HTML → 视频)

```
1. 启动 headless Chrome (chromedp)
2. 导航到 HTML 文件
3. 读取动画总时长 (data-duration 或 JS 查询)
4. for frame = 0; frame < totalFrames; frame++:
   a. 注入 JS: 设置动画进度 = frame/fps/duration
   b. 等待渲染完成
   c. captureScreenshot → PNG buffer
   d. 写入帧文件或管道
5. ffmpeg -framerate {fps} -i frame_%06d.png
         -c:v libx264 -pix_fmt yuv420p output.mp4
```

#### PPT 生成

```
1. LLM 生成 N 页 HTML (每页一个 <section>)
2. 为每页截图 (1920×1080 PNG)
3. 构建 PPTX:
   a. 创建幻灯片
   b. 插入全页面背景图
   c. 可选: 添加文本框覆盖
4. 输出 .pptx 文件
```

---

## 4. 创意团队工作流扩展

### 4.1 新工作流模式: `creative_media`

替代现有 `adversarial_dev`, 使用专用 `executeCreativeMedia()`:

```
Phase 1: 创意策划 + 任务拆解
  ├─ creative-planner: 理解需求, 产出创意方案
  └─ task-decomposer: 将复杂任务拆分为可执行子任务

Phase 2: 内容生成 (可并行)
  ├─ html-developer: 编写 HTML/CSS/JS 代码
  ├─ svg-artist: 生成 SVG 矢量图形
  └─ content-writer: 撰写文案/标题/描述

Phase 3: 视觉审查 + 迭代 (对抗循环)
  ├─ visual-reviewer: 审查视觉质量, 给出修改意见
  └─ html-developer: 根据反馈修改代码

Phase 4: 媒体输出
  ├─ MediaEngine.RenderHTML → PNG/JPEG
  ├─ MediaEngine.RenderPDF → PDF
  ├─ MediaEngine.RenderVideo → MP4 (如有动画)
  └─ MediaEngine.RenderPPTX → PPTX (如需)
```

### 4.2 任务拆解策略

针对长时间复杂创意任务, 引入智能拆解:

| 任务类型 | 拆解策略 | 示例 |
|:---|:---|:---|
| **多页网站** | 按页面拆分 | "做一个5页的产品官网" → 5个子任务 |
| **PPT 演示** | 按幻灯片拆分 | "做20页的汇报PPT" → 分批次生成 |
| **动画视频** | 按场景拆分 | "做一个产品介绍视频" → 多个场景HTML |
| **品牌套件** | 按资产类型拆分 | "设计品牌全套" → Logo/名片/海报 |

### 4.3 新增角色

| 角色 | 职责 |
|:---|:---|
| `html-developer` | 编写高质量 HTML/CSS/JS 网页代码 |
| `creative-planner` | 创意策划 + 复杂任务拆解 |
| `slide-designer` | PPT 幻灯片设计 (每页独立 HTML) |
| `media-producer` | 调用 MediaEngine 输出最终格式 |

---

## 5. 实现计划

### 新增文件

| 文件 | 内容 |
|:---|:---|
| `pkg/media/engine.go` | MediaEngine: HTML→PNG/PDF/MP4/PPTX |
| `pkg/media/chromedp.go` | chromedp 浏览器管理 + 渲染 |
| `pkg/media/ffmpeg.go` | FFmpeg 视频编码 |
| `pkg/media/pptx.go` | PPTX 生成 (截图方式) |
| `pkg/agent/workflow_creative_v2.go` | creative_media workflow |

### 修改文件

| 文件 | 变更 |
|:---|:---|
| `pkg/agent/workflow.go` | 注册 creative-v2 workflow |
| `pkg/agent/roles.go` | 新增 html-developer 等角色 |
| `pkg/agent/coordinator.go` | 支持 creative_media mode |
| `pkg/agent/teams.go` | 媒体输出集成 |
| `pkg/agent/intent.go` | 意图识别: HTML/PPT/视频关键词 |

---

## 6. 参考文献

| 来源 | 核心思想 |
|:---|:---|
| HyperFrames (heygen-com/hyperframes) | HTML-native 视频合成, CDP BeginFrame |
| Remotion (remotion-dev/remotion) | React 驱动的程序化视频 |
| chromedp (chromedp/chromedp) | Go CDP 客户端 |
| CREA (arXiv:2504.05306) | 多 Agent 协作创意工作流 |
| Creativity in LLM MAS (EMNLP 2025) | LLM 多 Agent 创意系统综述 |
