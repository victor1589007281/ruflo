# Agent Notes — claude-go 决策记录

[English](README.en.md) | 中文

这里只放一种文档。**Agent Note** 记录一项影响本代码库的裁定或提案——代码与设计文档承载不了的 *为什么* 和 *放弃了什么*。本文件定义 note 放在哪、何时写、以及 [文件内格式](#文件内格式)。

## 布局与命名

每份 note 有两个轴, 都编码在**路径**里: `{lifecycle}/{class}/yyyy-mm-dd-topic-title.md`:

- **Lifecycle** (顶层目录) 是 note 的状态, 随状态变化在目录间移动:
  - **`implemented/`** — 已交付的裁定。文件记录决定了什么、否决了什么, 并**随实际交付保持最新**: 代码后续移动文件、改名包、改关键默认值时, 同一变更内更新 note 的事实性内容 (只改事实——路径、名字、结构——不改裁定本身)。
  - **`proposed/`** — 实施前评审的提案; 尚未构建 (或只部分构建)。
  - **`rejected/`** — 提案已被考虑并否决。只在它的理由能阻止一个诱人的、有意义的错误时保留; 否则连同双语对一起删除。
- **Class** (嵌套目录) 是裁定的**种类**, 封闭集见下表。新增 class 须同时更新 `tests/unit/agent_notes_test.go` 的封闭集清单。

文件名日期是话题**首次提出**的日期。note 之间的交叉引用一律用相对 markdown 链接 (`[话题](../../implemented/architecture/2026-…-….md)`)——绝不用裸文字或编号——这样链接可机械校验、目录间移动时不断链。

活跃 lifecycle 树就是工作清单: 浏览或搜索即可, 不建集中式 `INDEX.md`。

## 分类 (封闭集)

| Class | 覆盖什么 |
|---|---|
| `feature` | 新的用户或模型可见能力。 |
| `bug-fix` | 修正缺陷或补上复盘暴露的缺口。 |
| `simplification` | 移除代码、行为或表面积, 不新增能力。 |
| `architecture` | 对**已交付源码**的结构性裁定——包如何关联、运行时词汇是什么。 |
| `process` | 代码**周边**的工具、策略或工作流——门禁、脚本、目录规范——不是运行时行为。 |
| `testing` | 测试基础设施与策略。 |

`architecture` / `process` 的分界线: **architecture** 说的是我们发布的源码; **process** 说的是周边工具与工作流。

## 何时写一份

每项非平凡变更**必须**在同一批改动里新增或更新至少一份 Agent Note。非平凡指: 改变行为、架构、跨文件或跨包的契约、流程或工具、测试策略、磁盘/线上/配置格式, 或任何维护者可能合理重访的裁定。已定的裁定直接写进 `implemented/`; 未定的提案进 `proposed/`。选择与裁定匹配的 class 目录。

更新已拥有该裁定的既有 note 即满足本规则; 不得制造重复。纯机械或局部、不改变行为/契约/结构/流程/理由的编辑豁免。note 绝不会被编辑成**另一个裁定**: 被取代时新建 note 并交叉链接。

## 文件内格式

每份活跃 note 的前三行严格为:

```markdown
# Agent Note: <标题>

Status: <状态>
```

`Status:` 取值与所在 lifecycle 目录必须一致 (校验测试交叉核对):

- `Status: implemented`
- `Status: proposed`
- `Status: rejected — <一行理由>`

### 双语对

每份 note 是**中英双语对**: 主文件 (中文, 本仓工作语言) + `.en.md` 副本 (英文), 两者承载**同等权威**、内容结构一一对应 (标题翻译、章节同名、决策清单同序)。改任何一侧必须同批带上另一侧。三份文件 (主文件、`.en.md`、行尾的配对记录) 一起移动或删除。

### 正文骨架

正文以 `## Problem` 开头 (动机, 不看方案也能读懂), 随后是 `## Decision` (裁定与被否方案)。生命周期的惯用节名: `## Verification` (交付证据)。 genuinely 定制的节 (包拓扑、wire 契约) 可自由插在必需节之间。

### 配对记录

文件末尾注释记录双语对的 git blob hash 与上次确认一致的状态:

```
<!-- pairing: <name>.en.md@<blob-hash-short> confirmed 2026-09-07 -->
```

## 校验

`tests/unit/agent_notes_test.go` 机械校验: 前三行格式、Status 与目录一致、封闭 class 集、双语对完整 (每个 note 都有 `.en.md` 副本)、相对链接指向真实存在的文件。**没有牙的纪律不是纪律**——这是 F9 (dsh 吸收项, docforge planning-dsh-adopt 13.6.2 C 组) 的兑现形态: claude-go 的 design/ + PROGRESS.md 已承担决策记录职能且诚实度更高, 吸收点仅是**粒度**——重大裁定 (如本批 F10-F13 的五项) 有独立 note 页, 不再散落在复核日志里。
