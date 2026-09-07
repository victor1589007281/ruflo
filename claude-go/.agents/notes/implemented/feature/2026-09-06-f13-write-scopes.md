# Agent Note: F13 writeScopes — 声明式写域与规划层串行化

Status: implemented

[English](2026-09-06-f13-write-scopes.en.md) | 中文

## Problem

编排器已有两类写冲突防线: `conflictKeys` (命名冲突键) 与 `writeFiles` (文件路径)。但规划层声明"我要改共享状态/契约/manifest"这类**领域级写意图**时没有统一落点——要么硬塞进 conflictKeys, 要么靠人工协调。规格 (docforge planning-dsh-adopt F13) 要求任务可声明写域, 供串行化与观测。

## Decision

**`writeScopes` 是第三类声明, 与 conflictKeys 同款 last-writer 串行化, 只读任务豁免。**

1. **声明链全通**: WBS JSON 的 `tasks[].writeScopes` → `parseWBSFromJSON` → `rawTask.writeScopes` → `RuntimeTask.WriteScopes` (orchestrator.go) / `TaskSpec.WriteScopes` (workflow.go, round-trip) → `AddTaskFull(..., writeScopes, ...)`。
2. **串行化语义** (orchestrator.go): 非只读任务 (`isReadOnlyRawTask` 为假) 对每个声明 scope, 向**上一个声明同 scope 的任务**追加依赖 (v2ID 与 depNum 双轨, 与 conflictKeys 同款 last-writer 模式)——同写域任务串行, 不同写域照常并行。空字符串 scope 跳过; 只读任务跳过。
3. **round-trip 纪律**: `writeScopes` 进 WBS 序列化输出 (`writeScopes,omitempty`), 规划模型声明什么、编排器就拿到什么, 不做隐式推断。写域是**领域键** (共享状态/契约/manifest 等抽象名), 与 writeFiles (具体路径) 互补而非重叠。

### 被否方案

把领域写意图折进 conflictKeys——语义混淆 (冲突键是"会冲突的事实", 写域是"我打算写的领域"), 且无法在观测面区分"规划层声明的意图"与"历史经验里的冲突模式"。独立字段让 13.8 的归因管线能直接按 scope 聚合。

## Verification

- `pkg/agent/orchestrator_writescopes_test.go` + `pkg/tool/builtin/tasktools_writescopes_test.go` (-race 绿): 同 scope 追加依赖、不同 scope 不串行、只读任务豁免、WBS round-trip 保真。
- 全仓 `go test ./...` 零失败 (2026-09-07)。

<!-- pairing: 2026-09-06-f13-write-scopes.en.md@pending confirmed 2026-09-07 -->
