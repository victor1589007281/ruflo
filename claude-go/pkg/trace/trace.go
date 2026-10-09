// Package trace 提供贯穿全链路的 trace-id 四元组 (design/03 §4.1 E0):
//
//	RunID  — 一次团队执行 / 一次会话任务 (episode 粒度)
//	NodeID — 图节点 / 工作流 stage 粒度
//	TurnID — QueryEngine 单轮 (一次 LLM 交互 + 工具执行)
//	CallID — 单次 LLM HTTP 调用 (由 api.Client 生成)
//
// 外加两个**跨粒度的业务索引键** (SessionID / TicketID+InstanceID), 它们不对应
// 调用层级, 而是回答"这条 trace 属于哪个业务对象"。四元组是自上而下的定位, 两个
// 索引键是自业务对象反向的定位 —— 运维现场问的通常是后者。
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

	// SessionID 是**跨 run 稳定**的会话键。它与 RunID 是两件事, 刻意分开:
	//
	//	RunID     —— 一次运行的 episode 键。必须每次运行唯一, 否则奖励归因
	//	             (team.LastRunID / 进化账本 / AggregateRewards) 会把两次运行
	//	             的账记到一起, 而这正是"为什么不能直接复用 RunID"。
	//	SessionID —— 上游 (opencode zen/go) 的**提示缓存分片键 + 路由亲和键**
	//	             (见 pkg/api/sessionshard.go 包注释)。它要的恰好相反: 同一个
	//	             业务对象反复跑时分片要稳定, 否则每次都全额重算。
	//
	// 一个键兼不了这两个角色, 所以不能拿 RunID 顶替 SessionID。空 = 不显式指定,
	// 出站会话头退回按 RunID 分片 (改造前行为, 逐字节不变)。
	SessionID string `json:"session_id,omitempty"`

	// TicketID / InstanceID 是**运维工单与实例**的索引维度, 与上面几个粒度正交。
	//
	// 为什么不能拿 RunID / SessionID 顶替: 它们回答的是"这次运行 / 这段对话是
	// 什么", 而运维现场的检索习惯是"这张工单当时怎么判的""这个实例上周发生了什么"。
	// 一张工单会跨多次运行 (崩溃重跑、多员工会诊、跨阶段推进), 所以工单与 run 是
	// 一对多; 一个实例会承载大量工单, 实例与工单又是一对多。两个都不能折叠 ——
	// 折叠之后"按实例反查"就退化成"先找到会话再翻会话", 而会话 ID 是人事后
	// 最不可能记得的那一个。
	//
	// 空 = 本次运行不属任何工单 / 不属于某个实例 (交互式使用), 是正常状态而非缺失。
	TicketID   string `json:"ticket_id,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

// HasOpsIndex reports 是否带了运维索引维度。
//
// 单列一个判断是因为"没有工单"有两种含义且处置不同: 交互式使用 (本就该没有) 与
// 工单驱动的运行漏注入了 (索引断链, 事后按实例反查会查不到任何东西)。调用方据此
// 决定是静默还是告警。
func (ids IDs) HasOpsIndex() bool {
	return ids.TicketID != "" || ids.InstanceID != ""
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
	if ids.SessionID != "" {
		cur.SessionID = ids.SessionID
	}
	if ids.TicketID != "" {
		cur.TicketID = ids.TicketID
	}
	if ids.InstanceID != "" {
		cur.InstanceID = ids.InstanceID
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
