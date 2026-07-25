---
name: technical-diagrams
description: 为技术文档制作彩色、可读、忠于源码的架构图与流程图（mermaid 语义配色八色板 + 手绘内联 SVG 主图 + 窄栏可读性 + 渲染实测验收）。
when_to_use: 需要画/改架构图、分层图、流程图、状态机、时序图、部署拓扑；给现有灰白 mermaid 上语义配色；修"图太宽看不清"；往手册/README/wiki 加图。即使没明说"图表"，只要任务是"写技术文档并配图""让文档不那么简陋""把架构讲清楚"也适用。
version: 1.0
---

# 技术文档图表制作法

三条铁律，违反任何一条图就白画了：

1. **忠于源码**——图上每个框、每条边都要能在代码里指出对应物。宁可少画，不可臆造。
2. **窄栏可读**——文档正文栏通常只有 700–800px。图的自然宽度必须 ≤1100px，否则被等比压缩到文字不可读。**彩色但看不清 = 没画**。
3. **必须实测渲染**——源码写对 ≠ 显示对，渲染管线有吞字符的坑（§5）。每张图都要量缩放比 + 抽样裁图目视。

## 1. 语义配色体系（复用，不要自创）

按节点在系统中的**角色**选色系，不是随机上色。浅填充 + 深描边 + 深文字，保证白底对比度：

```
classDef entry  fill:#dbeafe,stroke:#2563eb,stroke-width:2px,color:#1e3a8a    %% 入口/接入/用户层
classDef orch   fill:#ede9fe,stroke:#7c3aed,stroke-width:2px,color:#4c1d95    %% 编排/控制/调度
classDef exec   fill:#d1fae5,stroke:#059669,stroke-width:2px,color:#064e3b    %% 执行/运行时/worker
classDef data   fill:#e0f2fe,stroke:#0284c7,stroke-width:2px,color:#075985    %% 数据/存储/落盘
classDef learn  fill:#fef3c7,stroke:#d97706,stroke-width:2px,color:#78350f    %% 学习/记忆/辅助
classDef gate   fill:#ffe4e6,stroke:#e11d48,stroke-width:2px,color:#881337    %% 门禁/LLM/外部依赖
classDef dec    fill:#fff7ed,stroke:#ea580c,stroke-width:2px,color:#7c2d12    %% 判定/分支
classDef legacy fill:#f3f4f6,stroke:#9ca3af,stroke-width:1.5px,color:#6b7280,stroke-dasharray:5 3  %% 退役/未通电/断裂
```

用法：图末尾写 `classDef` 行，再 `class NodeA,NodeB orch`。subgraph 用
`style SubgraphId fill:#f8fafc,stroke:#94a3b8,stroke-width:1.5px`。加粗描边
（`stroke-width:2.5px`）强调关键节点。

判别力来自**色相 + 明度双重区分**，对色觉障碍读者同样可分辨。

### legacy 灰虚线是这套体系最有价值的一格

"已建成未通电""死代码""退役中""历史断点"一律用它。读者一眼看出哪些组件是摆设——
这是普通架构图给不了的信息密度。**把死代码画成活的是最严重的图表失真。**

## 2. 窄栏布局：图表达流，表格承载细节

宽图的根因几乎总是"想把细节塞进节点标签"。正确分工：

| 载体 | 承担 |
|---|---|
| 图 | **流**：谁到谁、什么条件、哪里分叉、哪里回环 |
| 紧邻表格 | **细节**：参数、阈值、file:line、各分支判据 |

手法按有效性排序：

1. **`flowchart LR` → `TB`**：通常直接把 scale 从 0.4 拉到 0.8+
2. **不要并排 subgraph**：mermaid 里 subgraph 内的 `direction LR` **常被忽略**，且多个 subgraph 横排会把图撑到几千 px。改单列纵向串联
3. 节点标签压成"概要 + `<br/>` 副标题"两行，条目移入表格
4. 横向最多 2–3 个节点，宁可多分层
5. **时序图/状态图**宽度由参与者数与转移标签长度决定：标签缩为 `① 动作` 编号式、判据下沉表格，必要时合并同进程参与者
6. `~~~` 隐形连线可堆叠本无依赖的节点组

## 3. 主图用手绘内联 SVG

分层架构总览这类"门面图"，mermaid 画不出该有的质感。手绘 SVG 换来渐变分层带、
精确对齐、右侧导轨（部署形态/图例/不变量）、徽章。

```html
<svg viewBox="0 0 1060 700" width="100%" role="img" aria-label="五层架构图"
     style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans CJK SC','PingFang SC','Microsoft YaHei',sans-serif">
  <defs>
    <linearGradient id="gL5" x1="0" y1="0" x2="1" y2="0">
      <stop offset="0" stop-color="#eff6ff"/><stop offset="1" stop-color="#dbeafe"/>
    </linearGradient>
    <marker id="ah" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7"
            orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="#64748b"/></marker>
  </defs>
  <!-- 层带: 每层一个渐变矩形 + 层内白底小圆角矩形组件 -->
  <rect x="16" y="60" width="820" height="98" rx="10" fill="url(#gL5)" stroke="#2563eb" stroke-width="2"/>
  <text x="32" y="84" font-size="14" font-weight="700" fill="#1e3a8a">L5　用户层 / Access</text>
  <rect x="32" y="112" width="132" height="34" rx="6" fill="#fff" stroke="#60a5fa"/>
  <text x="98" y="133" text-anchor="middle" font-size="11.5" fill="#1e3a8a">组件名</text>
  <!-- 右侧导轨放部署形态/图例/不变量; 一定要有图例说明色系与虚线含义 -->
</svg>
```

要点：`viewBox` + `width="100%"` 保证响应式；必须带 `role="img"` 与 `aria-label`；
**必须有图例**说明每个色系与虚线框含义。

## 4. 每张图都要有 figcaption

```html
<figure><div class="mermaid">…</div>
<figcaption style="font-size:12px;color:#64748b;margin-top:6px">图讲什么 + 配色含义 + 本图的关键设计</figcaption></figure>
```

figcaption 不是重复标题，而是回答**"读者该从这张图带走什么"**——点出图里最易被忽略的
那个设计决策。图外正文也必须有一段解释，不要让图自己站着。

## 5. 渲染坑：源码写对 ≠ 显示对

以下两坑在"HTML 片段 + 前端用 `textContent` 取 mermaid 源码 + mermaid htmlLabels 渲染"
的管线里必然出现（某手册全站 100+ 张图曾长期中招而无人发觉）：

| 坑 | 症状 | 成因 | 解法 |
|---|---|---|---|
| `<br/>` 被吞 | `A["第一行<br/>第二行"]` 渲染成 `第一行第二行` | HTML 里的 `<br/>` 已被浏览器解析成 BR 元素，`textContent` 直接丢弃 | 取源码前克隆节点、把 BR 替换回字面量 `<br/>`；或内容侧双转义 `&amp;lt;br/&amp;gt;` |
| `<xxx>` 占位符被吞 | `teams/<name>/` 显示成 `teams//` | htmlLabels 把标签文本当 HTML 注入，浏览器遇未知标签整段吞掉 | 提取后把形如 `<stage>` 的重新转义成实体（`<br/>` 例外）。正则要求 `<` 后紧跟字母，故 `-->` / `<--` 箭头不受影响 |

**一般化教训**：任何"文本经多层解析器传递"的图表管线都要端到端实测，不能只看源码。

## 6. 验收流程（不做完不算画完）

```bash
# ① 量缩放比 scale = 渲染宽/自然宽。<0.6 偏小、<0.35 不可读。目标全部 ≥0.6
node measure.mjs <slug>...
# ② 确认渲染无错: mermaidSvgs 必须 == mermaidBlocks 且 mermaidErrors == 0
node shot.mjs <url> out.png
# ③ 目视: 裁图后真的看一眼，别只信数字
python3 -c "from PIL import Image; Image.open('out.png').crop((240,200,1180,900)).save('c.png')"
```

**②通过但①失败的图最危险**——它"渲染成功"但读者看不清。量测脚本核心逻辑：

```js
// 每张图的自然宽取 viewBox[2]，渲染宽取 getBoundingClientRect().width
const scale = vb[2] > 0 ? +(rect.width / vb[2]).toFixed(2) : null;
```

## 7. 画图是核对实现的最佳时机

画图会迫使你逐行读源码，这是发现文档与实现偏差的高产环节：

- 数字（工作流数、端点数、相位数）**当场 grep 核**，绝不抄旧文档——旧文档的行号锚点尤其容易漂移
- 发现内容偏差**不要静默改**，记录后交决策者；但**明显事实错误必须改并说明**
- 设计规划的能力与已实现的能力**必须在图上可区分**（虚线框 / 标注里程碑范围），
  否则图会把"计划"渲染成"现状"
