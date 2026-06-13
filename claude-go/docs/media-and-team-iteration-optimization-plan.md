# 媒体产出 & 团队"运行后持续优化"——现状分析与优化方案

> **实现状态 (2026-06-13 已全部落地, `go build ./...` / `go vet ./...` / 受影响包测试全绿)**
>
> **媒体侧**
> - A 确定性视频: `pkg/media/video.go` 用 Web Animations API 逐帧 seek (`getAnimations().currentTime`) 替代墙钟 Sleep 截图; `engine.go renderVideo` 改为复用确定性捕获。实测 10帧@10fps → MP4 时长精确 1.000000s (无漂移)。`FPS`/`DurationSec` 可配, 不再写死 5s/15fps。
> - B 动图 GIF: `video.go encodeFramesToGIF` (gifski 优先, 回退 ffmpeg 两遍 palettegen, gifsicle 可选优化); 新增模型工具 `GenerateGIF`。
> - C 可编辑 PPTX: `pkg/media/pptx_editable.go BuildEditablePPTX` 生成真实 `<a:t>` 文本框(标题/要点/备注 sidecar); 新增双模式工具 `GeneratePPTX` (slides=可编辑 / html=高保真截图)。
> - D 工程: `pkg/media/pool.go` 进程级常驻 Chrome 池 (engine 复用); `pkg/wechat/assets.go` 内嵌 `mermaid.min.js` (file:// 加载, CDN 回退, 实测离线渲染 53KB PNG); `pkg/media/chart.go` + 工具 `GenerateChart` (内嵌 ECharts); `video.go MuxAudio` 音轨混流。
> - 新工具注册: `pkg/tool/builtin/register.go` (GenerateGIF/GeneratePPTX/GenerateChart)。新增内嵌资源 `pkg/wechat/assets/mermaid.min.js`(3.3M)、`pkg/media/assets/echarts.min.js`(1M)。
>
> **团队侧**
> - P0 RefineTeam: `pkg/agent/teams.go` 新增 `TeamStatusRefining` 状态、`RefineTeam(name,feedback,targetStage)`、`ForkTeam`、`LatestFinishedTeam`、`RefineEntry`/`PendingFeedback`/`FeedbackTarget` 字段、`finishRefine` 闭环; `pkg/agent/coordinator.go InvalidateCheckpoints` (增量失效); `pkg/agent/workflow.go executeStage` 注入 `{user_feedback}`; CLI `pkg/commands/team_commands.go` 加 `/team refine`、`/team fork`。
> - P1 内容质量门禁: `pkg/agent/content_gate.go` (白名单 techblog/research, SimpleComplete 评审打分, 阈值75, 未达标自动修订≤1轮); 接入 `executeWorkflow`。
> - P2 HITL + 闭环: `pkg/agent/intent.go RecognizeRefine` + `pkg/feishu/bot.go` 完成态消息→精修路由(仅当会话有已结束团队才触发, 不劫持普通聊天) + `/team refine|fork` 飞书命令; `finishRefine` 把已采纳反馈喂回记忆/Evolution。
> - 测试: `pkg/agent/refine_test.go` (stagesFromTarget/InvalidateCheckpoints/extractJSONObject/primaryContentStageIndex/门禁白名单)、`pkg/media/*_test.go` (可编辑PPTX合法性/ECharts/真机GIF+MP4)、`pkg/wechat/mermaid_render_test.go` (本地资源真机渲染)。
> - **注意**: 改动均未提交; 工作区原有未提交改动(token 修复、quote_tool.go 等)仍在, commit 前先 `git status` 理清。
>
> 下文为原始设计与业界对照, 保留作为决策依据。
>
> ---
>
> 2026-06-13 · 针对 claude-go（Go 版 Claude Code 多智能体编排系统）
> 范围：① HTML→图片/PPT/动图/视频 的真实能力边界；② 业界最佳实践对照与媒体侧优化选项；③ **团队跑完后无法持续迭代优化**的根因与解决方案（本文重点）。
> 注：工作区有未提交改动（token 失控修复、`quote_tool.go` 等），若据本文动手实现，git 状态偏脏，先理清再改。

---

## 一、媒体产出现状（代码级确认）

claude-go 的媒体范式是正确的："**文本模型写代码（SVG/HTML/Python）→ 专用工具渲染成真实媒体**"，而不是让文本模型直接吐二进制。核心实现在 `pkg/media/engine.go` + `pkg/media/pptx.go` + `pkg/tool/builtin/media_gen.go`。

| 能力 | 是否具备 | 实现路径 | 触发方式 | 能力边界/问题 |
|---|---|---|---|---|
| **HTML/SVG → 图片(PNG/JPEG)** | ✅ 完整 | chromedp 无头 Chrome `CaptureScreenshot`（`engine.go:115 renderImage`） | 模型工具 `GenerateImage`（`media_gen.go:53`）+ creative-v2 工作流 | 工作正常。每次渲染**新起一个 Chrome 实例**，无池化复用，慢。 |
| **HTML → PDF** | ✅ 完整 | chromedp `PrintToPDF`（`engine.go:159`） | 仅内部 `RenderAll(...,"pdf")` | OK |
| **HTML → PPT(PPTX)** | ⚠️ 仅"整页截图" | 按 `<section>`/`.slide` 切片→逐页截图 PNG→`buildSimplePPTX` 把 PNG 当**整页背景图**塞进 OOXML（`engine.go:361 renderPPTX` + `pptx.go:25`） | **无模型工具**，只能由 creative-v2 Phase4 内部触发（`workflow_creative_v2.go:216`） | **不可编辑**：每页是一张图，无文字框/备注/形状/母版。无法在 PowerPoint 里改字。 |
| **动图(GIF/动态图)** | ❌ 完全没有 | — | — | 无任何 GIF/WebP/APNG/Lottie 输出。 |
| **HTML/CSS动画 → 视频(MP4)** | ⚠️ 非确定性 | chromedp **逐帧 `Sleep`+截图** → ffmpeg libx264（`engine.go:224 renderVideo`） | creative-v2（检测到 `@keyframes` 自动加 mp4） | 见下方"问题"。固定 5s/15fps，无音轨。 |
| **Python脚本 → 视频(MP4)** | ✅ 可用 | 模型写 matplotlib/ffmpeg 脚本→执行→ffprobe 校验（`media_gen.go:143 GenerateVideo`） | 模型工具 | 这条路是好的（确定性由脚本保证）。 |
| **文本 → 语音(WAV)** | ✅ 可用 | piper 神经 TTS / espeak-ng（`media_gen.go:260`） | 模型工具 `GenerateSpeech` | 中文走 espeak-ng（机械音）。 |
| **Mermaid → 图表PNG** | ✅ 可用，但独立栈 | **另一套** chromedp + CDN 加载 `mermaid@10`（`pkg/wechat/mermaid.go:100`），3x DPR 高清，LLM 修复 3 轮 | wechat 链路 | 依赖 `cdn.jsdelivr.net`，离线/被代理挡时会失败（与记忆里 push2his 同类坑）。与 `media.Engine` 完全两套浏览器栈，重复。 |
| **媒体输入(多模态ingest)** | ✅ 新增 | 图片 base64 / 视频均匀抽帧 / 音频 faster-whisper 转写（`pkg/media/ingest.go`，6月新增） | 附件输入 | 这是输出端的对称输入端，闭环合理。 |

### HTML→视频这条路的具体问题（`engine.go:224 renderVideo`）

`engine.go` 顶部注释自称参考了 **HyperFrames（CDP BeginFrame）+ Remotion**，但**实际代码并没有用 BeginFrame**，而是：

```go
// engine.go:300-305 —— 用墙钟 Sleep 控制帧间隔
if hasRealAnimation {
    actions = append(actions, chromedp.Sleep(time.Duration(1000/fps)*time.Millisecond)) // 睡 1/fps
} else {
    // 静态页：人为注入 opacity+translateY 假动画
}
// 然后 page.CaptureScreenshot 截当前帧
```

后果（对作者来说是"保真度低/非确定性"，不是"坏"）：
1. **时间漂移**：每帧实际耗时 = `Sleep(1/fps)` + 截图本身的延迟（几十~上百 ms），所以真实经过的动画时间 > 名义时间 → **成片播放比真实动画快**，且帧间隔不均。
2. **丢帧/抖动**：浏览器实时渲染循环与截图不同步，重负载下掉帧。
3. **写死 5s@15fps**（`engine.go:240-241`），无法表达任意时长/帧率，无音轨混流、无转场。
4. 静态页"假动画"只是 body 整体 opacity+平移，观感廉价。

---

## 二、媒体侧优化方案（业界对照，按性价比排序）

> 用户问的是"看下有什么优化方案"——这里给**选项 + 收益**，不是必须全做。

### 选项 A（高收益）：HTML→视频改成确定性渲染（BeginFrame + 虚拟时间）

**业界做法**（HyperFrames / Replit 视频引擎 / WebVideoCreator / Remotion 共识）：不让浏览器跟着墙钟跑，而是"**对浏览器谎报时间**"——

- 开 Chrome **deterministic mode** + `HeadlessExperimental.beginFrame`：每调用一次 `beginFrame(screenshot=true)`，Chrome 只跑**一个** layout→paint→composite 周期并把该帧截图随结果返回，其余时间浏览器是冻结的。
- 配合 `Emulation.setVirtualTimePolicy`（或 beginFrame 的 `frameTimeTicks`）让 `setTimeout/requestAnimationFrame/CSS animation` 只在你指定的时间点推进。
- 逐帧循环：`for i in frames { t = i/fps; beginFrame(at=t) → 拿到确定的第 i 帧 → 喂给 ffmpeg }`。帧与时间一一对应，**无漂移、无丢帧、可任意时长/帧率、可逆序渲染**。

**落地**：chromedp 已经能发 `cdproto/headlessexperimental` 的 `BeginFrame`。把 `renderVideo` 的 Sleep 循环换成 beginFrame 循环即可；ffmpeg 编码段不动。这正好兑现 `engine.go` 注释里已经写下的设计意图。收益最大、改动集中在一个函数。

### 选项 B（低成本快赢）：补上动图(GIF)——你已经有 95% 的零件

GIF 不是"缺的一根支柱"，而是**顺手就能加**：`renderVideo`/beginFrame 已经在产帧序列，ffmpeg 已在用。只差一个编码分支。

**业界最佳实践**（高质量 GIF）：
- **ffmpeg 两遍调色板**：`palettegen` 先从帧生成最优 256 色调色板，再 `paletteuse`（带 dithering）合成 → 体积/质量平衡最好、纯 ffmpeg 无新依赖。
- **gifski**（pngquant 跨帧调色板 + 时间抖动）质量更高但体积略大；`gifsicle --optimize=3 --lossy=80` 可再压 30-60%。
- 建议：默认 ffmpeg palettegen（零新依赖），`gifski` 作为可选高质量后端。新增模型工具 `GenerateGIF`（HTML/帧→GIF）或给 `GenerateVideo` 加 `format: gif`。

### 选项 C（中收益）：可编辑 PPTX——摆脱"整页截图"

现状 `pptx.go` 是整页图当背景，**不可编辑**。三档可选，按"可编辑度 vs 保真度"权衡：

| 方案 | 可编辑 | 保真 | 依赖 | 适配 claude-go |
|---|---|---|---|---|
| 现状（整页截图） | ❌ | ★★★ | 纯 Go | 已实现 |
| **Marp CLI `--pptx-editable`** | ✅ 文本可改 | ★★ | Node + marp-cli | 模型产 Markdown/HTML→marp 导出；与现有"模型写源码→工具渲染"范式一致 |
| **python-pptx / pptxgenjs** | ✅ 完全可编辑(文本框/表格/形状) | ★ 需自己排版 | python/node | 让模型产**结构化 slide JSON**→脚本生成真正的 OOXML 元素 |

注意 Marp 官方也说明：可编辑 PPTX 牺牲保真、不支持备注。建议**保留截图版作为"高保真"模式**，新增"可编辑模式"让用户二选一（关键词/参数切换）。

### 选项 D（工程优化，跨能力共用）
1. **浏览器池化**：`media.Engine` 每次渲染新起 Chrome（`engine.go:94 newBrowserCtx`）。`wechat/mermaid.go` 已有 `RenderMermaidBatch` 复用浏览器的范式——把它抽成共享的 browser pool，图片/PPT/视频/mermaid 共用一个常驻 allocator，渲染延迟大降。
2. **mermaid 本地化**：把 `mermaid@10` 从 CDN 改为**内嵌本地 JS**（`go:embed`），消除离线/代理失败（与记忆里行情源被代理挡同类坑）。顺便统一到选项 D-1 的共享浏览器栈，干掉重复的第二套 chromedp。
3. **视频补音轨/字幕**：beginFrame 出画后，用 `GenerateSpeech` 产 WAV，ffmpeg `-i video -i audio -c:a aac -shortest` 混流；字幕走 `subtitles` filter。让"图文→带解说的短视频"闭环。
4. **图表扩展**：除 mermaid 外，常用的是 ECharts/Chart.js——本质同 mermaid（HTML+JS→截图），可复用同一渲染栈，扩 `RenderChart(lib, spec)`。

**媒体侧建议优先级**：A（确定性视频）> B（GIF，低成本）> D-2/D-1（mermaid 本地化+浏览器池）> C（可编辑PPT）> D-3/D-4。

---

## 三、团队"运行后持续优化"——根因与解决方案（重点）

### 3.1 痛点复述
团队（workflow）跑完产出结果后，往往还有问题需要持续调整优化；但当前实现一旦进入成功终态就**冻结**，没法带着新反馈继续迭代。

### 3.2 代码级根因（就三个"缺的连接件"，底座其实都在）

**根因——三处把"一次性"写死了：**

1. **成功终态不可逆**：`isSuccessfulTeamStatus`（`teams.go:846`）= `completed || delivered_with_remediation`；`ResumeTeam` 在 `teams.go:1298/1320` 两处**硬拒绝**成功团队（"已完成，无需恢复"）。状态机是单向的 `created→running→completed`，没有回到可执行态的边。
2. **没有"完成+反馈→再跑"的入口**：只有 `RunTeam(name, objective)`（启动 created/failed）和 `ResumeTeam(name)`（恢复 failed/stopped，且**沿用原目标、不接收反馈**）。缺一个接收反馈的 `RefineTeam`。
3. **pipeline 类工作流根本没有评审环**：`techblog/research/finance` 是 `Mode:"pipeline"`，串行跑完即止；`tech-critic` 打完分**不阻塞、不回修**（`workflow_techblog.go`）。代码类才有编译/测试门禁（`workflowProducesCode` 白名单，`teams.go:852`）。

**关键认知：迭代所需的底座 claude-go 已经有了，只是只在"运行期间"用、且只服务"下一个团队"，没接到"运行后"这条线上：**

| 迭代要素 | LangGraph 对应 | claude-go **已存在**的件 | 现状缺口 |
|---|---|---|---|
| 持久化检查点 | durable checkpointer | `coordinator.go:692-727` 把 `checkpoints.json` 写/读到每团队 `dataDir`；`restoreCheckpoints`（`workflow.go:396-420`）能跳过已完成阶段只重跑后续 | 成功后没人再进来用它；`ClearCheckpoints`（`coordinator.go:748`）在新工作流开始时会清掉 |
| 反馈注入 | interrupt + `Command(resume=feedback)` | **`{adversarial_feedback}` 占位符是真的**：`workflow_adversarial_dev.go:651` 把反馈 `ReplaceAll` 进 stage prompt，658-660 还注入到 `role.SystemPrompt`；`buildFeedbackSection`（685）负责构造 | 只在对抗工作流内部循环用，外部用户反馈进不来 |
| 评审→修订环 | evaluator-optimizer / Reflexion | 对抗循环已是完整的 generator↔evaluator（`runGeneratorRound`/`runEvaluatorRound`），代码类还有门禁自动修复（`tryGateWithRemediation`，`teams.go:955`） | 仅 `adversarial*` 模式有；pipeline 类没有 |
| 经验沉淀 | 长期记忆 | Evolution/Dreaming/Memory（`evolution.go`/`pkg/dreaming`），团队完成后 DISTILL 经验 | 全是 write-only，只喂**下一个**团队，不改当前产出 |

> 一句话：**评审-修订循环、持久化检查点、反馈注入管线——三样都已经实现了，只差把它们从"运行内/服务下个团队"接到"运行后/服务当前团队"。这是扩展，不是重写。**

### 3.3 解决方案（claude-go 原生扩展，分 P0/P1/P2）

#### P0 —— 让成功团队可带反馈重入，且只重跑受影响阶段（核心）

**(1) 状态机加一条边**：新增 `TeamStatusRefining = "refining"`，允许 `completed/delivered_with_remediation → refining → running → completed`。`isSuccessfulTeamStatus` 保持语义，但在新入口里显式放行。

**(2) 新 API `RefineTeam(name, feedback string, targetStage ...string)`**（teams.go 新增）：
```
1. 取出已完成团队，断言状态 ∈ {completed, delivered_with_remediation}
2. team.Status = refining；把 feedback 追加进 team.RefineHistory（新字段，留痕可追溯）
3. 决定重入点 targetStage：
   - 显式指定则用之；否则用一个轻量 LLM 路由（"这条反馈该改哪个阶段？"）或默认末位产出阶段
4. 增量失效：保留 < targetStage 的 checkpoints，失效 >= targetStage 的（**不要调 ClearCheckpoints**）
5. 把 feedback 通过 {adversarial_feedback}/{user_feedback} 注入 targetStage 的 prompt
   （复用 workflow_adversarial_dev.go:651 的 ReplaceAll 机制 + buildFeedbackSection）
6. executeWorkflow(ctx, team, isResume=true) —— restoreCheckpoints 自动注入前序阶段输出，
   只有 targetStage 及其下游重跑 → 省 token，不从头再生成
```
这把"LangGraph time-travel：在某 checkpoint 编辑 state 再 replay"用 claude-go 已有的 checkpoint-restore 实现了。

**(3) 通用 `{user_feedback}` 占位符**：在 `WorkflowExecutor` 填 prompt 的地方，除 `{adversarial_feedback}` 外加一个 `{user_feedback}`，让任意工作流（不止对抗类）都能在重跑时收到用户反馈。各角色 SystemPrompt 里按需放该占位符。

**(4) CLI/入口**：`pkg/commands/team_commands.go` 加 `/team refine <name> "<反馈>" [--stage 阶段名]`；可选 `/team fork <name>` 复制一个已完成团队再迭代（保留原产出做对比）。

#### P1 —— 给"无评审环"的 pipeline 工作流补上 critic + 可选质量门禁

让 techblog/research 这类一次性工作流也具备 Self-Refine 能力：

1. **通用 critic 阶段**：在 pipeline 末尾后挂一个**带门禁语义**的评审（复用现有 `tech-critic` 角色思路，但让它输出结构化评分 + 是否达标 + 针对性 issues）。
2. **自适应回修**：评分未达阈值 → 自动把 critic 的 issues 当作 `{user_feedback}` 注入写作阶段重跑 1-2 轮（复用 P0 的增量重入），达标或到轮次上限即止。本质是把代码类的 `tryGateWithRemediation`（`teams.go:955`）泛化成"内容类质量门禁"。
3. **阈值/轮次可配**，默认对内容类保守（避免 token 失控，呼应已修复的 maxTurns/clampTurns 事故）。

#### P2 —— 人在回路 & 经验闭环

1. **HITL 反馈直达**：飞书/微信链路里，团队完成后用户发来的消息若指向某团队，路由成 `RefineTeam` 反馈（而不是新建团队）。现有 Mailbox/Blackboard（`teams.go:390+`）是被动存储，给它加一个"完成态收到消息→触发 refine"的消费者即可。
2. **把 refine 结果喂回 Evolution**：每次 refine 的 (反馈→修订→是否被采纳) 作为高质量轨迹喂 `RecordFeedback`/`LearnFromTeam`，让"哪类反馈对应哪类修订"成为可检索经验——下一个团队首跑质量更高，refine 次数下降。这才真正闭环（当前 Evolution 只学不改当前团队）。

### 3.4 改动清单（file:line 锚点）

| 改动 | 位置 | 类型 |
|---|---|---|
| 新增 `TeamStatusRefining` + 允许的状态迁移 | `pkg/agent/teams.go:171` 枚举区 | 小 |
| 放行成功团队重入 / 新 `RefineTeam` | `pkg/agent/teams.go`（仿 `ResumeTeam:1282`） | 中 |
| **不**清 checkpoint，按 targetStage 增量失效 | 复用 `coordinator.go:686 GetCheckpoint` / 避开 `ClearCheckpoints:748` | 中 |
| `{user_feedback}` 通用占位符注入 | `pkg/agent/workflow.go` 填 prompt 处（仿 `workflow_adversarial_dev.go:651`） | 小 |
| pipeline 通用 critic + 内容类质量门禁 | 仿 `tryGateWithRemediation`（`teams.go:955`）+ `workflow_techblog.go` | 中 |
| CLI `/team refine` `/team fork` | `pkg/commands/team_commands.go:14` | 小 |
| 完成态消息→refine 消费者 | `pkg/feishu/`、`teams.go` Mailbox 区 | 中 |
| refine 轨迹喂 Evolution | `evolution.go` `RecordFeedback`/`LearnFromTeam` | 小 |

### 3.5 为什么这样做（设计取舍）

- **复用而非重写**：评审环、检查点、反馈注入都已实现且经过测试，新增的是"连接件"和"对外入口"，风险低、与现有缓存/前缀稳定性约束兼容。
- **增量重入省 token**：只重跑受影响阶段，避免小说/视频类从第一章/分镜重头再生成（正面回应 token 失控的历史教训）。
- **对内容类补门禁**：把代码类已验证的"门禁+自动修复"模式平移到 techblog/creative，覆盖当前最大的质量盲区。
- **留痕可追溯**：`RefineHistory` + Evolution 闭环，让"持续优化"既可人工驱动也可自我改进。

---

## 四、一页纸结论

1. **媒体现状**：HTML→图片 ✅；PPT ⚠️（整页截图、不可编辑）；动图 ❌（完全没有）；视频 ⚠️（HTML 路用 Sleep 截图、非确定性、写死 5s/15fps，Python 脚本路 OK）。
2. **媒体优化**（选做）：A 确定性视频(BeginFrame+虚拟时间) > B 加 GIF(低成本快赢) > mermaid 本地化+浏览器池 > 可编辑 PPT > 音轨/图表扩展。
3. **团队持续优化**（重点，强烈建议做）：根因是状态机单向 + 缺 refine 入口 + pipeline 无评审环；但**检查点/反馈注入/评审环底座都已存在**。方案 = `RefineTeam` 带反馈增量重入（P0）+ 给 pipeline 补 critic 门禁（P1）+ HITL 直达与经验闭环（P2）。**是扩展不是重写。**
