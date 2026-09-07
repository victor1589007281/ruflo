# Agent Note: F11 asOf 投影 — tracestore 上的纯 fold 与时间旅行

Status: implemented

[English](2026-09-06-f11-asof-projection.en.md) | 中文

## Problem

TraceStore 支持按 run 还原 Span 流, 但"复现当时上下文"只能靠每次手写 span 拼装——学习管线想要"第 N 轮时的会话视图", 就得重新写一遍逐 Kind 的重组逻辑; 拼装逻辑散落、无水位、无正确性定义。

这是 dsh 吸收调研的 F11 项: dsh `packages/session/session-projection` 有 `ProjectionDefinition` fold + `asOfSeq` 时间旅行 (任意历史时刻的会话视图) + checkpoint 续放。

## Decision

**`pkg/evolution/tracestore/projection.go`: 泛型 fold + 前缀重放 + checkpoint 续放, 三个语义全部锚在 dsh 上, 一处有意分歧。**

1. **`ProjectionDefinition[S, E]`**: `{Key, StateVersion, Init, Apply}` —— 一个纯同步 fold 单元。`Apply` 必须是纯函数 (不碰 IO/时间/全局), asOf 重放的正确性完全押在它上面。这是 dsh `ProjectionDefinition` 的直译; dsh 的 whole-value rule (状态事件必带完整后状态) 在本仓不需要单独机制——Span 天然是整值事件, fold 的幂等可重放性由纯函数直接保证。
2. **`AsOf(traceID, asOf)` 前缀重放**: 取 TraceID 全部 Span, apply 到第 asOf 条 (0 基) 为止。时间旅行 = 只重放前缀: append-only log 的写入序稳定, 同一切点重放必然得到同一状态。`Projected[S]` 返回 `{AsOfSeq, State, Count}` ——值与水位来自同一日志切点, 视图自述自己看到了哪条事件为止 (dsh asOfSeq 语义)。asOf<0/超界 → 全量。
3. **`Restore` checkpoint 续放**: `(Key → {ver, seq, val})` 版本匹配且水位可用则从 checkpoint 续放后缀, 否则**静默降级全量重放**。
4. **装配错误显式失败**: nil store → `ErrNoProjection` 哨兵; 缺 Key/Init/Apply → 装配即报错。装配错误不静默。

### 有意分歧: Restore 不 fail-loud

dsh 在 baseSeq>0 且 checkpoint 不可用时 **fail-loud 抛错**——因为它的持久化行是唯一非重放来源。本仓 Span 全文都在 Log 里, **全量重放永远是可行兜底**, "静默降级为重放"比"报错让调用方重来"更贴合本仓"轨迹缺失不影响交付"的既有纪律 (与 dsh 的分歧在 projection.go 注释里明文记录)。

另: 本仓 Span 无显式 seq 字段, asOf 以 0 基写入序号代之——Log.Append 顺序即全局序, 语义与 dsh seq 一致。

## Verification

- `pkg/evolution/tracestore/projection_test.go` 4 测试 (-race 绿): 会话视图重放与手写拼装一致 (这正是 F11 要消灭的那类拼装)、asOf 时间旅行 (切点 N 视图 == 只写前 N 条的全量视图, k=0..3 全对比)、checkpoint 续放 (ver=1 续放 == 全量重放; ver=0/seq=99 降级仍一致)、装配纪律 (nil store/缺 fold/nil 投影全部显式报错)。

<!-- pairing: 2026-09-06-f11-asof-projection.en.md@pending confirmed 2026-09-07 -->
