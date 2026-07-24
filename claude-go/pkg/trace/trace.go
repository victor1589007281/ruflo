// Package trace 提供贯穿全链路的 trace-id 四元组 (design/03 §4.1 E0):
//
//	RunID  — 一次团队执行 / 一次会话任务 (episode 粒度)
//	NodeID — 图节点 / 工作流 stage 粒度
//	TurnID — QueryEngine 单轮 (一次 LLM 交互 + 工具执行)
//	CallID — 单次 LLM HTTP 调用 (由 api.Client 生成)
//
// 历史缺陷: llm.jsonl 无 run/turn 关联键, 与 transcript / 团队轨迹只能按时间戳
// 粗对齐, 无法重建 (state, action, reward) 三元组 (design/03 §1.3)。
// 本包是修复的地基: 上游 (teams/workflow/engine) 逐层 With 注入, 下游
// (api.Client / evolution / metrics) 统一 From 读取。
//
// 设计约束: 零依赖 (仅标准库), 任何层都可安全 import。
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// IDs trace 四元组。空字段表示该粒度尚未进入 (如非团队路径无 RunID)。
type IDs struct {
	RunID  string `json:"run_id,omitempty"`
	NodeID string `json:"node_id,omitempty"`
	TurnID string `json:"turn_id,omitempty"`
	CallID string `json:"call_id,omitempty"`
}

type ctxKey struct{}

// With 合并注入: 仅覆盖 ids 中非空的字段, 保留 ctx 中已有的其余粒度。
// 例: teams 层注入 RunID 后, workflow 层 With(IDs{NodeID: stage}) 不会丢 RunID。
func With(ctx context.Context, ids IDs) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	cur := From(ctx)
	if ids.RunID != "" {
		cur.RunID = ids.RunID
	}
	if ids.NodeID != "" {
		cur.NodeID = ids.NodeID
	}
	if ids.TurnID != "" {
		cur.TurnID = ids.TurnID
	}
	if ids.CallID != "" {
		cur.CallID = ids.CallID
	}
	return context.WithValue(ctx, ctxKey{}, cur)
}

// From 读取当前 trace 四元组; ctx 无标记时返回零值。
func From(ctx context.Context) IDs {
	if ctx == nil {
		return IDs{}
	}
	if v, ok := ctx.Value(ctxKey{}).(IDs); ok {
		return v
	}
	return IDs{}
}

// NewID 生成短随机 id, 形如 "<prefix>-9f3a1c" (6 hex 字符, 碰撞域按单机日量级足够)。
func NewID(prefix string) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败时退化为时间戳, 保证 id 永不为空
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e9)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// NewRunID 生成 episode 级 RunID。约定形如 "run-<name>-<unix>-<rand>",
// name 便于人读检索 (团队名/会话短id), unix 便于按时间排序。
func NewRunID(name string) string {
	return fmt.Sprintf("run-%s-%d-%s", name, time.Now().Unix(), NewID("")[1:])
}
