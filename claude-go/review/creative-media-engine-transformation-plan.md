# Claude-Go 创意媒体引擎改造方案

> **版本**: 1.1 | **日期**: 2026-05-23
> **范围**: 基于 `html-anything` 创意团队实现、HyperFrames/Remotion 源码分析、2024-2025 学界业界前沿（LAVE / VideoGrain / OpenCodeInterpreter / Self-Refine / VisualWebArena / LayoutFormer++），以及视频/图片剪辑最佳实践，对 `claude-go` 现有 `pkg/media` + `creative-v2` 工作流进行全面改造。

---

## 1. 现状审计：claude-go 创意媒体能力的 6 大短板

### 1.1 Skill/模板系统过于简陋
- 当前 `pkg/skills` 只有基础的 `SKILL.md` 解析（YAML frontmatter + body），**没有**模板注册表、没有示例预览、没有设计系统约束。
- 对比 `html-anything`：75 个 skill 文件夹，每个含 `SKILL.md` + `example.html` + `example.md`，支持 `recommended` 排序、`aspect_hint`、场景标签，前端可实时预览。
- 对比 `gsap-skills`：遵循 **Agent Skills 标准**（agentskills.io），每个 `SKILL.md` 含 `name` / `description`（含触发词 trigger terms）/ `license` frontmatter；通过 `skills/llms.txt` 建立 Agent 发现索引；skill 间通过 "Related skills" 建立分层依赖（core → timeline → scrolltrigger → plugins → react → performance → frameworks）；每个 skill 含明确的 **Do/Do Not** 反模式清单；支持 `npx skills add` 统一安装并兼容 Claude Code / Cursor / Copilot / Codex / Antigravity 等 40+ Agent。

### 1.2 HTML→Video 能力处于 Demo 级别
- `pkg/media/engine.go` 的 `renderVideo()` 使用固定 5 秒时长、15fps、硬编码的渐入兜底动画，**不具备**：
  - 帧精确时间控制（无 `data-duration` / `data-start` 解析）
  - CSS 动画 seek（未使用 `animation-play-state: paused + animation-delay` 精确控制）
  - 音频混合
  - 多场景拼接
- 对比 HyperFrames：单时钟传输、三级媒体同步、Frame Adapter 模式、六阶段渲染管线。

### 1.3 无 Diff-Edit 与迭代精化机制
- `workflow_creative_v2.go` 虽有 AdaptiveTerminator 对抗循环（1-5 轮），但每轮都是**全量重写 HTML**。
- 对比 `html-anything` 的 `diff-edit` 模式：基于 `(baseContent, baseHtml)` 做最小化差异编辑，保留设计系统，节省 60%+ token。
- 对比业界 OpenCodeInterpreter / Self-Refine：无 "生成→执行→反馈→修正" 的结构性闭环。

### 1.4 无视觉接地与精准元素操控
- Agent 生成 HTML 是"盲写"——无法看到当前帧内容的视觉状态，无法精准修改"左边标题"或"第三张卡片"。
- 对比 VisualWebArena 的 **Set-of-Marks**：在截图上为元素打边界框+ID，Agent 输出离散精确动作。
- 对比 VideoGrain 的 **ST-Layout Attention**：text-to-region 精确绑定。

### 1.5 无 HTML 自动修复与约束满足
- `extractHTMLFromOutput()` 只是从 LLM 输出中提取 HTML 代码块，**不做任何验证**。
- 无 lint、无 AST 解析、无约束检查、无自动补丁。
- 对比 HyperFrames：59+ 条 lint 规则（即使只是诊断）。
- 对比 OpenCodeInterpreter：执行反馈（编译器/运行时错误）直接回流到生成循环。

### 1.6 导出能力薄弱
- 当前导出：PNG（截图）、PDF（PrintToPDF）、MP4（简陋帧捕获）、PPTX（图片幻灯片）。
- 缺失：
  - 微信/知乎/X 等平台的**格式适配导出**（juice 内联、LaTeX 占位符、2× PNG 剪贴板）
  - HyperFrames 原生兼容输出（`HYPERFRAMES_META` JSON + 帧级元数据）
  - Remotion 项目导出

---

## 2. 改造总纲：引入 "Creative Media Engine v2"

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    Creative Media Engine v2 (CMEv2)                     │
│                                                                         │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐              │
│  │  Template    │───▶│   Prompt     │───▶│   Agent      │              │
│  │  Registry    │    │   Assembler  │    │  Invocation  │              │
│  │  (Skill+)    │    │  (Diff-Edit) │    │  (Multi-CLI) │              │
│  └──────────────┘    └──────────────┘    └──────┬───────┘              │
│                                                  │                      │
│  ┌───────────────────────────────────────────────┘                      │
│  │                                                                       │
│  ▼                                                                       │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐              │
│  │   HTML AST   │───▶│  Constraint  │───▶│  Auto-Fix    │              │
│  │   Parser     │    │  Validator   │    │  (Lint→Patch)│              │
│  └──────────────┘    └──────────────┘    └──────┬───────┘              │
│                                                  │                      │
│  ┌───────────────────────────────────────────────┘                      │
│  │                                                                       │
│  ▼                                                                       │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐              │
│  │   Visual     │───▶│  Frame-Acc   │───▶│  Multi-Format│              │
│  │   Grounding  │    │  Renderer    │    │   Exporter   │              │
│  │  (SoM/VLM)   │    │(HyperFrames) │    │(WeChat/X/PDF)│              │
│  └──────────────┘    └──────────────┘    └──────────────┘              │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 3. 模块一：Template Registry（模板注册表）

### 3.1 借鉴 html-anything + gsap-skills 的文件夹化 Skill 设计

改造 `pkg/skills` 从简单注册表升级为 **Template Registry**，同时吸收 `gsap-skills` 的 **Agent Skills 标准**：

```
.claude-go/templates/
├── deck-swiss-international/
│   ├── SKILL.md          # frontmatter + prompt body（遵循 Agent Skills spec）
│   ├── example.md        # 示例输入
│   ├── example.html      # 预渲染预览（前端/CLI 可直接展示）
│   ├── design.json       # 设计令牌约束（颜色、字体、栅格）
│   └── constraints.yaml  # 结构化约束（如 "必须含 data-start"）
├── video-hyperframes/
│   ├── SKILL.md
│   ├── example.html
│   └── hyperframes-meta.json
├── video-gsap-timeline/
│   ├── SKILL.md          # 含 GSAP Timeline 时间轴编排指令
│   ├── example.html      # 含 gsap.timeline() 的示例
│   └── example.json      # 帧时间规格
└── ...
```

**新增数据结构** (`pkg/templates/registry.go`)：

```go
type TemplateDef struct {
    ID            string
    Name          string
    Category      string          // deck | frame | poster | doc | social | prototype
    Scenario      string          // marketing | engineering | personal
    AspectHint    string          // "1920x1080", "390x844", "A4"
    Recommended   int             // Featured 排序
    Tags          []string
    TriggerTerms  []string        // 触发词（借鉴 gsap-skills）—— Agent 根据用户输入自动匹配
    RelatedSkills []string        // 关联 skill ID（借鉴 gsap-skills "Related skills"）
    License       string          // MIT / Apache-2.0
    DesignTokens  DesignTokens    // 颜色、字体、间距系统
    Constraints   []Constraint    // 结构化约束规则
    Body          string          // Prompt body（给 LLM 的指令）
    ExampleMd     string
    ExampleHtml   string
    DoNot         []string        // 反模式清单（借鉴 gsap-skills）
}

type DesignTokens struct {
    PrimaryColor  string
    FontStack     []string
    BaseGrid      int             // 8px
    MaxContentWidth string        // 65ch
}

type Constraint struct {
    Type      string  // "required_attr", "forbid_pattern", "max_count"
    Selector  string  // CSS selector
    Rule      string
    Severity  string  // error | warning
    AutoFix   bool    // 是否支持自动修复
}
```

**Agent Skills 标准吸收（源自 gsap-skills）**：

| 机制 | gsap-skills 实现 | CMEv2 集成方案 |
|------|-----------------|----------------|
| **Frontmatter** | `name`, `description`, `license` | TemplateDef 扩展 `TriggerTerms`, `RelatedSkills`, `License` |
| **触发词索引** | `skills/llms.txt` 列出所有 skill 名称+摘要+触发词 | `pkg/templates/llms.txt`（或 `registry.json`）作为 Agent 路由索引 |
| **Skill 依赖** | "Related skills" 建立分层（core→timeline→scrolltrigger） | `RelatedSkills []string` 建立模板依赖图（如 deck 模板依赖 animation 模板） |
| **Do/Do Not** | 每个 SKILL.md 含明确的禁忌列表 | `DoNot []string` 注入 Prompt，约束 Agent 行为 |
| **多 Agent 兼容** | `npx skills add` + 各 Agent 专属目录 | `pkg/agent/protocol/` 统一分发（见 Module 7） |

### 3.2 Prompt Assembler：统一设计指令 + 模板 Body + 用户内容

仿照 `html-anything/next/src/lib/templates/shared.ts` 的 `SHARED_DESIGN_DIRECTIVES`：

```go
const SharedDesignDirectives = `
你是世界级视觉设计师 + 资深前端工程师。请输出自包含单文件 HTML：

【硬性规则】
- 禁止使用 Write/Edit/Bash 等文件工具，HTML 必须直接在回复正文流式输出
- 文档以 <!DOCTYPE html> 开头，以 </html> 结束
- <head> 中通过 CDN 引入 Tailwind v3 Play 与 Google Fonts
- 不引用外部图片 URL（优先 CSS/SVG 内联）
- 输出纯 HTML，不要用 markdown 围栏包裹

【设计准则】
- 中文优先 Noto Sans SC / Noto Serif SC，英文 Inter / Manrope
- 1 主色 + 2 中性色 + 至多 1 强调色；大胆留白；不用纯黑纯白
- 8px 基线；段落最大宽度 65ch；标题与正文有清晰层级
- 圆角统一 (rounded-xl/2xl)，投影柔和
- 颜色对比度 ≥ 4.5

【内容驱动数量 — 最高优先级】
- 模板只定义"可用版面/风格/配色/字体/组件库"，不定义 slide/帧/卡片数量
- 输出数量完全由用户内容实际长度决定，必须完整覆盖每个要点
- 宁可多页也不要把多个独立要点硬塞进一页
`
```

**assemblePrompt()** 函数拼接：
`SharedDesignDirectives + Template.Body + 输入格式 + 用户内容`

---

## 4. 模块二：Diff-Edit & 迭代精化引擎

### 4.1 核心机制：最小化差异编辑

仿照 `html-anything` 的 `buildEditPrompt` 和 `use-convert.ts` 中的 diff-edit 逻辑：

```go
// pkg/media/diff_engine.go

type DiffEditRequest struct {
    TemplateName    string
    TemplateAspect  string
    OldContent      string
    NewContent      string
    OldHTML         string
    Format          string
}

func BuildDiffEditPrompt(req DiffEditRequest) string {
    return fmt.Sprintf(`你正在执行一次**最小化差异编辑** (diff-edit)，不是从 0 重新生成。

模板风格: %s (%s)
输入格式: %s

【硬性规则】
1. 仅输出完整的、修改后的 HTML。第一个字符必须是 <，最后必须是 </html>。
2. 不要用 markdown 围栏包裹，不要任何解释性文字。
3. 禁止使用 Write / Edit / MultiEdit / Bash 等文件工具。
4. 保留原 HTML 的 <head>（CDN / 字体 / 样式 / meta），保留所有不需要变化的 DOM 结构。
5. 仅根据"旧内容 vs 新内容"的差异，替换或调整对应的文字/数据节点。
6. 如果新旧内容只差几个字，也只改那几个字 —— 不要顺手"优化"或"重排"。
7. 不要捏造数据。新内容里没有的就不要写。

【旧内容】
%s

【新内容】
%s

【已有 HTML —— 请基于此修改，输出完整的修改后版本】
%s
`, req.TemplateName, req.TemplateAspect, req.Format,
   req.OldContent, req.NewContent, req.OldHTML)
}
```

### 4.2 状态管理：BaseLine 机制

在 `pkg/agent/workflow_creative_v2.go` 的 `executeCreativeMedia` 中引入：

```go
type CreativeTaskBaseline struct {
    Content string    // 上次输入内容
    HTML    string    // 上次输出 HTML
    Format  string    // 输入格式
    TemplateID string
}

func (we *WorkflowExecutor) shouldUseDiffEdit(
    baseline *CreativeTaskBaseline,
    newContent string,
) bool {
    if baseline == nil || baseline.HTML == "" {
        return false
    }
    // 内容变化比例 < 50% 且 HTML 存在时启用 diff-edit
    similarity := textSimilarity(baseline.Content, newContent)
    return similarity > 0.5 && similarity < 1.0
}
```

### 4.3 迭代精化闭环：生成 → 验证 → 修正

借鉴 OpenCodeInterpreter 的 `Codeₙ₊₁ = F(Codeₙ, Feedbackₙ)`：

```go
// pkg/media/refinement_loop.go

type RefinementLoop struct {
    MaxRounds   int
    Validators  []Validator
}

type Validator interface {
    Validate(html string) ValidationResult
}

type ValidationResult struct {
    Pass      bool
    Score     float64
    Feedback  string          // 自然语言反馈，回流给 LLM
    Patches   []HTMLPatch     // 结构化补丁（如可用）
}

func (rl *RefinementLoop) Run(
    ctx context.Context,
    initialHTML string,
    generator func(ctx context.Context, feedback string) (string, error),
) (string, error) {
    html := initialHTML
    for round := 1; round <= rl.MaxRounds; round++ {
        var feedbacks []string
        for _, v := range rl.Validators {
            result := v.Validate(html)
            if !result.Pass {
                feedbacks = append(feedbacks, result.Feedback)
            }
        }
        if len(feedbacks) == 0 {
            return html, nil // 全部通过
        }
        // 将验证反馈拼接为 prompt，请求 LLM 修正
        combinedFeedback := strings.Join(feedbacks, "\n---\n")
        var err error
        html, err = generator(ctx, combinedFeedback)
        if err != nil {
            return html, err
        }
    }
    return html, fmt.Errorf("达到最大精化轮数 %d，仍未通过验证", rl.MaxRounds)
}
```

**内置 Validators**：
1. **HTMLSyntaxValidator**：检查是否完整 `<!DOCTYPE html>...<html>`，无未闭合标签
2. **ConstraintValidator**：检查 Template 定义的约束（如必须含 `data-start`）
3. **LintValidator**：调用 HyperFrames lint 规则（若适用）
4. **PreviewValidator**：chromedp 截图后检查黑帧/空白/溢出（像素级阈值）

---

## 5. 模块三：HTML AST & 约束满足系统

### 5.1 引入 goquery / html-parse 做 DOM 操作

当前 `extractHTMLFromOutput()` 和 `splitSlides()` 使用正则，**极易出错**。引入 `PuerkitoBio/goquery` 或 `golang.org/x/net/html`：

```go
// pkg/media/html_ast.go

import "golang.org/x/net/html"

type HTMLDocument struct {
    Doc       *html.Node
    Raw       string
    Head      *html.Node
    Body      *html.Node
}

func ParseHTMLDocument(raw string) (*HTMLDocument, error) {
    doc, err := html.Parse(strings.NewReader(raw))
    if err != nil {
        return nil, err
    }
    // 提取 head/body
    return &HTMLDocument{Doc: doc, Raw: raw}, nil
}

func (d *HTMLDocument) QuerySelector(selector string) []*html.Node {
    // 使用 css selector 库（如 github.com/andybalholm/cascadia）
}

func (d *HTMLDocument) ApplyPatch(patch HTMLPatch) error {
    // DOM-diff 后应用最小化修改
}
```

### 5.2 约束解码与回溯（Constraint Decoding）

借鉴 LayoutFormer++ 的约束序列化 + 解码时剪枝思想，在**生成后**强制执行：

```go
// pkg/media/constraints.go

type ConstraintEngine struct {
    Rules []ConstraintRule
}

func (ce *ConstraintEngine) Enforce(doc *HTMLDocument) (bool, []HTMLPatch) {
    var patches []HTMLPatch
    pass := true
    for _, rule := range ce.Rules {
        ok, fix := rule.Check(doc)
        if !ok {
            pass = false
            if fix != nil {
                patches = append(patches, *fix)
            }
        }
    }
    return pass, patches
}

// 示例规则
func RuleRequireDataStart() ConstraintRule {
    return ConstraintRule{
        Name: "require-data-start",
        Check: func(doc *HTMLDocument) (bool, *HTMLPatch) {
            sections := doc.QuerySelector("section.frame")
            for _, sec := range sections {
                if getAttr(sec, "data-start") == "" {
                    return false, &HTMLPatch{
                        Op: "addAttr", Target: sec, Attr: "data-start", Value: "0",
                    }
                }
            }
            return true, nil
        },
    }
}
```

### 5.3 Auto-Fix：Lint → Patch → Apply → Re-validate

对比 HyperFrames 的 diagnostic-only lint，CMEv2 实现**自动修复**：

```go
func AutoFixHTML(doc *HTMLDocument, engine *ConstraintEngine) (string, error) {
    for attempt := 0; attempt < 3; attempt++ {
        pass, patches := engine.Enforce(doc)
        if pass {
            return doc.Serialize(), nil
        }
        for _, p := range patches {
            if err := doc.ApplyPatch(p); err != nil {
                return "", err
            }
        }
    }
    return "", fmt.Errorf("无法自动修复所有约束违反")
}
```

---

## 6. 模块四：Frame-Accurate 媒体渲染引擎

### 6.1 视频渲染：从 "截图拼凑" 升级到 "确定性帧捕获"

当前 `renderVideo()` 的痛点：
- 固定 5 秒，无视实际动画时长
- 无 `data-duration` 解析
- 无精确 seek，靠 `Sleep` 让动画自然播放，帧间时间不精确
- 无音频

**改造方案**：引入 **GSAP Timeline 作为视频时间轴核心基础设施**（借鉴 gsap-skills 的 timeline 精确控制能力）：

```go
// pkg/media/video_renderer.go

type VideoRenderer struct {
    FPS         int     // 默认 30
    OutputDir   string
    ChromePath  string
}

type FrameSpec struct {
    Index       int
    TimeMs      int64   // 该帧在 timeline 上的精确时间
    DurationMs  int64   // 该帧（场景）的持续时长
    Transition  string  // fade | slide | none
}

// AnimationTimeline 抽象：支持 CSS Animation、GSAP Timeline、WAAPI 的统一时间轴控制
type AnimationTimeline struct {
    TotalDurationMs int64
    Frames          []FrameSpec
}

func (vr *VideoRenderer) Render(ctx context.Context, htmlContent string) (*RenderResult, error) {
    // 1. 解析时间轴规格：HyperFrames META / data-duration / 或 GSAP Timeline 元数据
    spec := parseFrameSpec(htmlContent)

    // 2. 启动 chromedp，注入确定性时间控制 JS
    // 参考 HyperFrames: 使用 animation-play-state: paused + animation-delay: -{time}s
    // 参考 Remotion: 精确 seek 到每一帧的时间点
    // 参考 GSAP: globalTimeline.time() 提供毫秒级精确 seek

    // 3. 逐帧 captureScreenshot
    for _, frame := range spec.Frames {
        js := fmt.Sprintf(`
            (function() {
                const t = %d; // ms
                const timeSec = t / 1000;

                // A. CSS Animation 精确 seek（HyperFrames / Remotion 方案）
                document.querySelectorAll('*').forEach(el => {
                    const style = window.getComputedStyle(el);
                    if (style.animationName !== 'none') {
                        el.style.animationPlayState = 'paused';
                        el.style.animationDelay = '-' + timeSec + 's';
                    }
                });

                // B. GSAP Timeline 精确 seek（gsap-skills 核心能力）
                // GSAP 提供 globalTimeline.time(sec) 可一次性 seek 到任意时间点
                // 优于 CSS animation 的 per-element 控制，支持嵌套 timeline、
                // position parameter、labels、ease 等复杂编排
                if (window.gsap) {
                    window.gsap.globalTimeline.time(timeSec);
                }

                // C. WAAPI (Web Animations API) 精确 seek
                document.getAnimations().forEach(anim => {
                    anim.currentTime = t;
                });
            })()
        `, frame.TimeMs)

        // 执行 JS → 等待渲染（requestAnimationFrame）→ screenshot
    }

    // 4. FFmpeg 编码（复用现有逻辑，但支持更多参数）
    return vr.encodeWithFFmpeg(spec)
}
```

**GSAP Timeline 作为视频时间轴基础设施（源自 gsap-skills 分析）**：

`gsap-skills` 的 `gsap-timeline` skill 揭示了 GSAP Timeline 相比 CSS Animation 在视频渲染中的独特优势：

| 维度 | CSS Animation | GSAP Timeline |
|------|--------------|---------------|
| **Seek 精度** | 需逐元素设置 `animationDelay` | `globalTimeline.time(sec)` 单点控制所有动画 |
| **时间编排** | 有限的 `animation-delay` 链 | position parameter (`+=0.5`, `<`, `label+=0.3`) 精确到 0.001s |
| **嵌套能力** | 无原生嵌套 | Timeline 可包含子 Timeline，master timeline 统一控制 |
| **Labels** | 无 | `addLabel("intro", 0)` + `play("intro")` 实现章节跳转，天然映射到视频场景 |
| **运行时控制** | `animation-play-state` 仅 pause/play | `pause() / play() / reverse() / progress(0.5) / time(2)` 完整控制 |
| **Easing** | 预定义 `cubic-bezier` | 20+ 内置 ease + CustomEase 插件（SVG path 定义任意曲线） |
| **Stagger** | 无原生支持 | `stagger: 0.1` 或 `{amount: 0.3, from: "center"}` |
| **Transform 顺序** | 不可控 | 统一顺序：translate → scale → rotationX/Y → skew → rotation |

**CMEv2 的 GSAP 集成策略**：

1. **Prompt 层**：在 Template Registry 中增加 `video-gsap-timeline` 模板，SKILL.md 指导 Agent 使用 GSAP Timeline 而非零散 CSS animation 来编排帧动画。示例：
   ```javascript
   const master = gsap.timeline({ defaults: { ease: "power2.inOut" } });
   master.addLabel("intro", 0)
             .to(".title", { y: 0, autoAlpha: 1, duration: 0.8 }, "intro")
             .to(".subtitle", { y: 0, autoAlpha: 1, duration: 0.6 }, "intro+=0.3")
             .addLabel("scene2", "+=0.5")
             .to(".bg", { scale: 1.1, duration: 2 }, "scene2");
   // master.duration() 自动计算总时长，用于视频渲染
   ```

2. **渲染层**：chromedp 注入 `gsap.globalTimeline.time(t)` 实现帧精确 seek，替代逐元素修改 CSS animation 的低效方式。

3. **约束层**：Template 的 `Constraints` 可强制要求 `"必须使用 gsap.timeline() 编排动画"`，避免 Agent 生成无法精确 seek 的 CSS animation。

**关键升级点**：

| 能力 | 当前 | 改造后 |
|------|------|--------|
| 时长控制 | 固定 5s | 解析 `data-duration` / `HYPERFRAMES_META` / `gsap.timeline().duration()` |
| 帧率 | 15fps | 可配置，默认 30fps |
| Seek 精度 | Sleep 等待 | CSS `animation-delay` + `animation-play-state: paused` + GSAP `globalTimeline.time()` 三重精确 seek |
| 时间轴编排 | 无 | GSAP Timeline position parameter + labels，天然视频场景映射 |
| GSAP 支持 | 无（仅兜底检测） | 核心基础设施，Prompt 引导 + 渲染层原生支持 |
| 音频 | 无 | 支持 `<audio data-start>` 提取并与视频混合 |
| 并行捕获 | 单线程 | 多 worker 并行（参考 HyperFrames producer） |

### 6.2 HyperFrames 原生兼容输出

新增 `RenderHyperFrames()` 函数，输出可直接喂给 `hyperframes render`：

```go
func (e *Engine) RenderHyperFrames(ctx context.Context, htmlContent, name string) RenderResult {
    // 1. 确保 HTML 包含标准 HyperFrames 结构
    //    <section class="frame" data-duration="3000" data-transition="fade">
    // 2. 在 </html> 前注入 HYPERFRAMES_META JSON 注释
    // 3. 输出到 .html 文件，用户可直接运行 hyperframes render
}
```

### 6.3 PPTX 升级：从图片幻灯片到原生文本+图

当前 `buildSimplePPTX()` 是纯图片背景幻灯片，**无法编辑文本**。

**中期改造**：引入 `unioffice` 或保持图片方式但提升质量：
- 短期：保留图片方式，但支持每页独立 `slide_*.html` 的完整渲染（含字体、动画帧选择）
- 中期：解析 HTML 中的文本节点，生成 OpenXML 的 `<a:p>` / `<a:r>` 原生文本框

---

## 7. 模块五：视觉接地与精准操控（Visual Grounding）

### 7.1 Set-of-Marks (SoM) 帧标注

当用户说"把左边标题改红"时，Agent 需要知道"左边标题"对应哪个 DOM 节点。

**实现**：

```go
// pkg/media/visual_grounding.go

func GenerateSoMFrame(htmlContent string) ([]byte, error) {
    // 1. chromedp 渲染 HTML 到截图
    // 2. 注入 JS 为每个带 id 或 data-element-id 的元素绘制边界框+数字标签
    // 3. 同时输出 accessibility tree（元素 ID → selector 映射）
    // 4. 返回：带标记的 PNG + 元素映射表
}

type SoMMap struct {
    MarkID    int
    Selector  string
    TagName   string
    Text      string
    Bounds    image.Rectangle
}
```

**Agent 使用方式**：
- Agent 接收带 SoM 标记的截图 + 元素列表
- Agent 输出操作如 `{"action": "setColor", "target": "#mark-3", "value": "#ff0000"}`
- 系统将该操作翻译为 DOM patch：`document.querySelector('[data-som-id="3"]').style.color = '#ff0000'`

### 7.2 与 Diff-Edit 结合

Diff-edit 模式下，SoM 让 Agent 只做**局部修改**而非重写整个文档：

```
用户: "把第三页的背景换成深蓝"
系统: 生成带 SoM 的第三页截图 → Agent 识别 mark-7 是背景层
Agent 输出: {op: "setAttr", selector: "[data-som-id='7']", attr: "style.background", value: "#001a33"}
系统: 应用 patch → 重新渲染第三页预览
```

---

## 8. 模块六：多格式导出引擎（Export Pipeline）

仿照 `html-anything/next/src/lib/export/` 的导出矩阵：

```go
// pkg/media/export/ 子包

package export

type Exporter interface {
    Name() string
    Export(ctx context.Context, html string, opts ExportOptions) (ExportResult, error)
}

type ExportOptions struct {
    Width       int
    Height      int
    Quality     int     // JPEG quality / PNG compression
    InlineCSS   bool    // juice 内联（用于微信）
    DPI         int     // PDF 用
}
```

| 导出器 | 实现方案 | 关键依赖 |
|--------|----------|----------|
| `WeChatExporter` | `juice` 内联 CSS → `ClipboardItem({'text/html','text/plain'})` | `github.com/Automattic/juice`（Go 可用 goquery 模拟） |
| `ZhihuExporter` | `<mjx-container>` → `data-eeimg` 占位符 | 正则替换 |
| `ImageExporter` | `chromedp` screenshot → 2× PNG → `ClipboardItem` | chromedp |
| `PDFExporter` | `chromedp PrintToPDF` | chromedp |
| `VideoExporter` | Frame-accurate capture → FFmpeg | chromedp + ffmpeg |
| `PPTXExporter` | 图片幻灯片 / 原生文本（中期） | archive/zip |
| `HyperFramesExporter` | 输出标准 HF HTML + META JSON | 纯文本 |
| `RemotionExporter` | 生成 Remotion 项目骨架（tsx + 帧数据） | 模板引擎 |

### 8.1 微信导出关键技术点

公众号编辑器只支持内联样式，不支持 `<style>` 标签和外部 CSS。解决方案：

```go
func InlineCSS(html string) (string, error) {
    // 使用 goquery 读取 <style> 内容，通过 csstree 或简单规则匹配
    // 将每个 CSS 规则的内联样式写到对应元素的 style 属性上
    // 保留 @media（若平台支持）或移除
}
```

> 注：Go 生态无 `juice` 的直接移植，可：
> 1. 调用 Node.js 子进程运行 `juice` CLI（claude-go 已有子进程调用能力）
> 2. 或限制模板只使用 Tailwind CDN，生成时用 `tailwindcss` CLI 编译为内联 utility classes

---

## 9. 模块七：Agent 协议抽象层（多 CLI 支持）

`html-anything` 的核心创新之一是支持 8+ 种本地 Coding Agent CLI。`gsap-skills` 则展示了 **Agent Skills 标准**在多 Agent 生态中的分发能力。claude-go 目前只调用单一 LLM 后端。

**中期目标**：在 Go 侧实现类似的 Agent 协议抽象 + Skills 分发：

```go
// pkg/agent/protocol/

package protocol

type AgentProtocol interface {
    Name() string
    Detect() (bool, string)           // 是否可用，二进制路径
    BuildArgv(prompt string, opts InvokeOpts) []string
    MakeParser() LineParser
    EnvVars() map[string]string
    SkillDir() string                 // 该 Agent 的 skill 安装目录（借鉴 gsap-skills）
}

type InvokeOpts struct {
    Model       string
    Timeout     time.Duration
    CWD         string
    Format      string  // "stream-json" | "json" | "plain"
}

type LineParser interface {
    Parse(line string) []ParseEvent
}

type ParseEvent struct {
    Type string  // "delta" | "html" | "meta" | "done" | "error"
    Text string
    Key  string
    Value interface{}
}
```

**支持协议**：

| CLI | Protocol | Skill 安装路径（借鉴 gsap-skills） | 状态 |
|-----|----------|-----------------------------------|------|
| Claude Code | `claude -p --output-format stream-json` | `~/.claude/skills/` | P0 |
| OpenAI Codex | `codex exec --json` | `~/.codex/skills/` | P1 |
| Cursor Agent | `cursor-agent --print --output-format stream-json` | `~/.cursor/skills/` | P1 |
| Gemini CLI | `gemini --output-format stream-json` | `~/.gemini/antigravity/skills/` | P1 |
| OpenCode | `opencode run --format json` | `~/.config/opencode/skills/` | P2 |
| Qwen Coder | `qwen --yolo` | `~/.qwen/skills/` | P2 |

**关键能力**：`rescueHtmlFromToolUse` — 当 Agent 用 Write 工具写文件时，从 tool_use input 中抢救 HTML。

**Skills 分发机制（借鉴 gsap-skills + npx skills CLI）**：

```bash
# 统一安装 CMEv2 模板到当前 Agent 的 skill 目录
npx skills add https://github.com/ruflo/claude-go-templates

# 或在 Claude Code 内
/plugin marketplace add ruflo/claude-go-templates
```

claude-go 可作为 skills registry 服务端：维护模板仓库，提供 `llms.txt` 索引，Agent 根据用户输入的 trigger terms 自动拉取对应模板。

---

## 10. 工作流改造：creative-v2 → creative-v3

### 10.1 新工作流架构

```
Phase 1: 意图解析 + 模板匹配
  ├─ intent-recognizer: 识别 creative / creative-v2 / creative-v3 需求
  ├─ template-matcher: 从 Template Registry 选最佳模板（基于关键词 + 场景）
  └─ 输出: selectedTemplateID + designTokens

Phase 2: 策划 + 拆解 (可并行)
  ├─ creative-planner: 理解需求，产出创意方案
  └─ task-decomposer: 拆解为子任务，决定是否需要多页/多帧

Phase 3: 内容生成 (Diff-Edit 优先)
  ├─ 若存在 baseline: diff-edit 模式（最小化修改）
  └─ 若全新任务: full-generation 模式
  
Phase 4: 验证 + 自动修复
  ├─ HTMLSyntaxValidator
  ├─ ConstraintValidator（模板约束）
  ├─ LintValidator（HyperFrames 规则）
  └─ AutoFixEngine（应用补丁或回流 LLM）

Phase 5: 视觉审查 (对抗循环，最多 3 轮)
  ├─ art-director: 5 维度评分
  ├─ 若未通过: feedback → 回到 Phase 3（diff-edit 修正）
  └─ 若通过: 进入 Phase 6

Phase 6: 媒体输出
  ├─ Frame-Accurate Renderer（视频）
  ├─ Multi-Format Exporter（PNG/PDF/PPTX/WeChat/Zhihu/X）
  └─ HyperFrames / Remotion 兼容输出

Phase 7: 交付 + 基线固化
  ├─ media-producer: 整理交付清单
  └─ commitBaseFor: 将 (content, html) 存为下次 diff-edit 的 baseline
```

### 10.2 并行化改造

利用 claude-go 已有的 orchestrator DAG 能力：

```go
// workflow_creative_v3.go

var creativeV3Stages = []StageDef{
    // Phase 1
    {Name: "intent-parse", Role: "intent-recognizer"},
    {Name: "template-match", Role: "creative-planner", DependsOn: []string{"intent-parse"}},
    
    // Phase 2 (并行)
    {Name: "creative-plan", Role: "creative-planner", DependsOn: []string{"template-match"}},
    {Name: "task-decompose", Role: "creative-planner", DependsOn: []string{"template-match"}, Parallel: true},
    
    // Phase 3-5 (生成-验证-审查循环，由 executeCreativeMediaV3 内部驱动)
    {Name: "content-generate", Role: "html-developer", DependsOn: []string{"creative-plan", "task-decompose"}},
    
    // Phase 6 (媒体输出可与审查并行启动)
    {Name: "media-render", Role: "media-producer", DependsOn: []string{"content-generate"}, Parallel: true},
    
    // Phase 7
    {Name: "final-delivery", Role: "media-producer", DependsOn: []string{"media-render", "visual-review"}},
}
```

---

## 11. 实现路线图

### Phase A: 立即可做（1-2 周）

| 任务 | 文件 | 说明 |
|------|------|------|
| Template Registry 改造 | `pkg/templates/` | 新建包，支持文件夹化模板 + frontmatter + example.html |
| Prompt Assembler | `pkg/templates/assembler.go` | 统一 SharedDesignDirectives + Template.Body + 用户内容 |
| Diff-Edit 引擎 | `pkg/media/diff_engine.go` | 最小化差异编辑 prompt 构建 + baseline 状态管理 |
| HTML AST 迁移 | `pkg/media/html_ast.go` | 引入 `golang.org/x/net/html`，替换正则解析 |
| Constraint Validator | `pkg/media/constraints.go` | 基础约束规则（required_attr, forbid_pattern） |
| **Trigger Terms + llms.txt 索引** | `pkg/templates/discovery.go` | Agent 根据用户输入关键词自动匹配模板（借鉴 gsap-skills） |
| **Do/Do Not 约束注入** | `pkg/templates/prompt.go` | 将 TemplateDef.DoNot 自动拼接进 Prompt |
| **GSAP Timeline 模板** | `templates/video-gsap-timeline/` | 引导 Agent 用 GSAP Timeline 编排帧动画的 SKILL.md + example |

### Phase B: 短期（3-4 周）

| 任务 | 文件 | 说明 |
|------|------|------|
| Frame-Accurate Video | `pkg/media/video_renderer.go` | 精确 seek（CSS animation-delay + GSAP globalTimeline.time() + WAAPI）、解析 data-duration / GSAP timeline duration、多帧并行捕获 |
| **GSAP 时间轴基础设施** | `pkg/media/gsap_timeline.go` | chromedp 注入 GSAP seek 逻辑；Template 约束强制要求 timeline 编排 |
| Auto-Fix 引擎 | `pkg/media/autofix.go` | Lint → Patch DSL → HtmlPatcher → Re-validate |
| SoM 视觉接地 | `pkg/media/visual_grounding.go` | 截图 + 元素边界框标注 + accessibility tree |
| Export Pipeline | `pkg/media/export/` | WeChat、Zhihu、X、PNG、PDF、PPTX、HyperFrames |
| **Skills 多 Agent 分发** | `pkg/agent/protocol/skills.go` | 实现 `npx skills add` 兼容的模板分发；生成 `llms.txt` 索引 |
| 工作流升级 V3 | `pkg/agent/workflow_creative_v3.go` | 引入 Template Registry + Diff-Edit + Refinement Loop |

### Phase C: 中期（1-2 月）

| 任务 | 说明 |
|------|------|
| Agent 协议抽象层 | 支持 Claude Code / Codex / Cursor 等本地 CLI，复用 html-anything 的协议定义 |
| HyperFrames 深度集成 | 直接调用 `hyperframes lint` + `hyperframes render`，而非仅输出兼容 HTML |
| Remotion 项目导出 | 将 CMEv2 的帧数据导出为 Remotion 的 TSX 组件 + 序列数据 |
| VLM 在线验证 | 引入视觉模型（如 GPT-4V / Qwen-VL）做帧内容语义验证（"画面是否符合创意意图"） |
| 音频混合 | 支持 `<audio>` 标签提取与 FFmpeg 混音 |

### Phase D: 长期（3 月+）

| 任务 | 说明 |
|------|------|
| 约束解码生成 | 在 LLM 生成阶段即通过 constrained decoding 避免错误产生（非事后修复） |
| 子树级局部编辑 | 将 composition 建模为 Timeline AST，NL 调整映射为子树操作 |
| 视频编辑 Agent | 支持 "把第 3 帧延长 2 秒"、"在 A 和 B 之间加过渡" 等时序级 NL 指令 |
| 多模态创意输入 | 支持 "参考这张海报的风格"（图片输入 → 风格提取 → HTML 生成） |

---

## 12. 关键依赖新增

```go
// go.mod 新增
require (
    // HTML AST 与 CSS 选择器
    github.com/PuerkitoBio/goquery v1.9.0
    github.com/andybalholm/cascadia v1.3.2
    golang.org/x/net v0.25.0
    
    // 图像处理（SoM 标注、截图后处理）
    github.com/disintegration/imaging v1.6.2
    
    // YAML 解析（constraints.yaml）
    gopkg.in/yaml.v3 v3.0.1
    
    // 已有
    github.com/chromedp/chromedp v0.10.0
    github.com/chromedp/cdproto v0.0.0-2024...
)
```

**前端依赖（通过 CDN 在生成 HTML 中引入）**：
- **GSAP** (`https://cdn.jsdelivr.net/npm/gsap@3/dist/gsap.min.js`) — 时间轴编排核心
- **GSAP ScrollTrigger** (`gsap/ScrollTrigger`) — 滚动驱动动画（如需要）
- **GSAP SplitText / MorphSVG / DrawSVG** — 文本/SVG 高级动画（如模板需要）

GSAP 为纯前端库，无需 Go 依赖；渲染时通过 chromedp 注入 `gsap.globalTimeline.time()` 实现精确 seek。

---

## 13. 参考文献与借鉴来源

| 来源 | 核心借鉴 |
|------|----------|
| **html-anything** (nexu-io) | 文件夹化 Skill Registry、Diff-Edit 模式、Agent 协议抽象、Export Pipeline、Write 工具抢救 |
| **HyperFrames** (heygen-com) | 单时钟传输、Frame Adapter、三级媒体同步、HYPERFRAMES_META、lint 规则设计 |
| **Remotion** (remotion-dev) | 帧级控制、React→帧序列→FFmpeg 管线 |
| **LAVE** (IUI 2024) | Plan-and-Execute Agent、结构化 Action JSON、人在回路 |
| **VideoGrain** (ICLR 2025) | ST-Layout Attention、细粒度空间-时序控制、实例/部件级编辑 |
| **VisualWebArena** (ACL 2024) | Set-of-Marks (SoM) 视觉接地、多模态 Agent 精确交互 |
| **OpenCodeInterpreter** (ACL 2024) | 生成-执行-反馈-修正闭环、Code-Feedback 数据集、执行信号回流 |
| **Self-Refine** (2023) | 自我批评→自我重写、无需外部监督的迭代精化 |
| **LayoutFormer++** (CVPR 2023) | 约束序列化 + 解码时剪枝/回溯、布局约束满足 |
| **AutoStructGUI** (IUI 2025) | 结构化 GUI 树生成、子树级局部编辑 |
| **CREA** (arXiv:2504.05306) | 多 Agent 协作创意（概念→生成→评审→优化） |
| **gsap-skills** (greensock/gsap-skills) | Agent Skills 标准格式、触发词系统、llms.txt 索引、Do/Do Not 反模式、GSAP Timeline 精确时间轴控制、多 Agent CLI 兼容分发 |

---

## 附录 A：gsap-skills 仓库分析摘要

> 由于网络限制，仓库通过 GitHub API 拉取并保存于 `/home/victor/base/git/temp/ruflo/gsap-skills/`。

### A.1 架构概览

```
gsap-skills/
  README.md
  AGENTS.md              # Agent 编辑规范（目录名=skill name，<500 行，第三人称）
  .github/
    copilot-instructions.md    # Copilot 全局指令
    instructions/              # 路径级指令（react.instructions.md 等）
  .claude-plugin/          # Claude Code plugin 配置
  .cursor-plugin/          # Cursor plugin 配置
  skills/
    llms.txt               # Agent 发现索引：名称+摘要+触发词
    gsap-core/SKILL.md     # 核心 API（to/from/fromTo/ease/stagger）
    gsap-timeline/SKILL.md # 时间线（position parameter/labels/nesting）
    gsap-scrolltrigger/SKILL.md  # 滚动驱动（pin/scrub/toggleActions）
    gsap-plugins/SKILL.md  # 插件（Flip/Draggable/SplitText/MorphSVG/DrawSVG）
    gsap-utils/SKILL.md    # 工具函数（clamp/mapRange/interpolate）
    gsap-react/SKILL.md    # React 集成（useGSAP/context/cleanup）
    gsap-performance/SKILL.md    # 性能优化（transform/will-change/quickTo）
    gsap-frameworks/SKILL.md     # Vue/Svelte 生命周期
  examples/               # vanilla + React 最小示例
```

### A.2 对 CMEv2 的核心启示

1. **Agent Skills 是跨 Agent 生态的"通用技能包"**：一个仓库通过 `npx skills add` 即可分发给 Claude/Cursor/Copilot/Codex/Antigravity 等 40+ Agent，解决了模板/技能的多平台分发问题。

2. **触发词 + llms.txt 实现动态 Skill 路由**：Agent 无需硬编码 skill 列表，通过匹配用户输入中的 trigger terms 自动加载对应 skill。这对 CMEv2 的 Template Matcher 是重要参考——可实现"用户说'做个滚动动画'→自动加载 gsap-scrolltrigger 模板"。

3. **GSAP Timeline 是动画领域的"SQL"**：position parameter（`+=0.5` / `<` / `label+=0.3`）提供了声明式的时间编排能力，比 CSS animation 的 `animation-delay` 链强大一个数量级。对视频渲染而言，这意味着：
   - Agent 生成的是**可精确 seek 的时间轴定义**，而非脆弱的 CSS 规则
   - `gsap.globalTimeline.time(t)` 提供单点控制，无需逐元素操作 DOM
   - Labels 天然映射到视频场景（chapter/shot），便于 NL 调整（"把 intro 延长 2 秒"）

4. **Do/Do Not 是 Prompt 工程的结构化表达**：将禁忌从"隐式设计原则"转化为"显式约束清单"，显著降低 Agent 犯错概率。CMEv2 的 TemplateDef.DoNot 字段直接吸收此模式。

5. **性能优先的设计哲学**：`transform > layout`、`autoAlpha > opacity`、`will-change` 提示、`quickTo()` 高频更新——这些原则同样适用于 HTML→Video 渲染（减少 layout thrashing = 更稳定的帧捕获）。

---

*本方案为 claude-go 创意媒体引擎的系统性改造蓝图，可直接作为工程实施的顶层设计和任务拆解依据。*
