# Agent Note: F9 Agent Notes — 每特性一页双语决策记录

Status: implemented

[English](2026-09-07-agent-notes-discipline.en.md) | 中文

## Problem

claude-go 的决策记录职能由 design/ (系统性设计) + design/PROGRESS.md (带日期的复核账, 含三轮标注失误的自我更正记录) 承担, 诚实度高但有**粒度缺口**: 一次重大裁定 (如"孤儿子系统退役"、本批 F10-F13 的语义分歧) 的完整推理链——动机、被否方案、验证证据——散落在复核日志的长段落里, 检索靠人肉, 没有机械约束保证"改了行为就得更新记录"。

这是 dsh 吸收调研的 F9 项 (process 类): dsh `.agents/notes/` 每项特性一份中英对照架构决策记录, 含提案→裁定→验证链, 且有 `verify-agent-note-format` 机械门禁。

## Decision

**`.agents/notes/{lifecycle}/{class}/yyyy-mm-dd-topic.md` 布局 + 中英双语对 + 机械校验测试。三处与 dsh 的有意分歧:**

1. **主侧是中文** (dsh 主侧英文 + `.zh.md` 副本)——本仓工作语言是中文, design/ 与 PROGRESS.md 全是中文; 双语对仍保留, `.en.md` 承载同等权威, 服务跨语言检索。
2. **校验是 Go 测试** (`tests/unit/agent_notes_test.go`), 不是独立脚本——本仓没有 pnpm/脚本门禁基建, 测试进 `go test ./...` 主链路, **没有牙的纪律不是纪律**。
3. **配对记录是文件尾注释** (dsh 是独立 `.i18n.yaml` 记 git blob hash)——本仓 note 数量级小 (十位而非 dsh 的 618), 文件尾注释省一个文件、且测试可直接读。

沿用 dsh 的部分: lifecycle 三态 (implemented/proposed/rejected, Status 行与目录交叉核对)、class 封闭集 (feature/bug-fix/simplification/architecture/process/testing)、相对链接交叉引用、implemented note 随交付保持最新 (只改事实不改裁定)、无集中 INDEX。dsh 的 archived 冻结树不吸收——本仓 note 量级远不需要。

### 分工边界

Agent Note 记**单点裁定的为什么与放弃了什么**; design/ 记系统性设计; PROGRESS.md 记按日复核账与自我纠错。三者互补不重叠: 一项重大裁定 = design/ 里的设计段落 + 一页 note 里的推理链 + PROGRESS.md 里的一行复核记录。

## Verification

- `tests/unit/agent_notes_test.go` (-race 绿): 前三行格式、Status 与目录一致、class 封闭集、双语对完整、相对链接可解析。
- 本批六份 note (F8-F13 各一页) 即首个样本: 每份都走 Problem → Decision → Verification 骨架。

<!-- pairing: 2026-09-07-agent-notes-discipline.en.md@self confirmed 2026-09-07 -->
