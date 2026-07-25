package worker

// factory.go —— 控制面接线点: RuntimeRegistry → 既有 CreateAgentFunc 契约。
//
// # 为什么接在 CreateAgentFunc 而不是 graph.NodeRunner
//
// 图节点的执行链是:
//
//	graph.Engine.execNode ──(记 journal: node.started/completed)──▶
//	  pkg/agent.stageNodeRunner.RunNode ──(门禁/分片 prompt/回灌/动态展开解析)──▶
//	    WorkflowExecutor.ExecuteSingleStage ──▶ runAgent ──▶
//	      we.factory(stageCtx, role, "")  ← **本文件替换的就是这一处**
//	        └─ 生产实现: feishu.SessionManager.CreateAgentRunner
//
// 换掉 factory 而不是换掉 NodeRunner, 有三个不可让的好处:
//
//  1. **journal 记账天然一致**: 事件是引擎在 RunNode 外面记的 (engine.go:689 前后),
//     远程执行走的是同一个调用点, 于是 node.started/node.completed(带 output)
//     与本地执行一字不差 —— resume 时远程节点的状态不再是空的。
//     若自己写一个 NodeRunner 去旁路 stageNodeRunner, 就得自己复制 journal 语义。
//  2. **零重复**: gate 节点评分、map 分片 prompt 拼装、reduce 聚合输入、loop 回灌、
//     动态展开解析全部留在 stageNodeRunner 里, 一行都不用抄。抄一份就是两份会
//     漂移的 prompt 组装逻辑。
//  3. **pkg/graph 零改动**。
//
// # 放置策略从哪来 (诚实说明)
//
// design/01 §4.9 设想 `NodeSpec.Agent.Placement`, 但 `graph.AgentSpec` 目前**没有**
// 这个字段 (spec.go:197), `NodeExecHints` 也不携带它 —— 所以**逐节点**放置声明
// 现在无法从图规格传到这里。本文件退一步: 取进程级默认 Placement (由部署方注入),
// 并在 Affinity 非空时用团队名补 AffinityKey (团队亲和是真的, 逐节点约束不是)。
// 补齐所需的改动见 report 的"需要接的线"。

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/trace"
)

// RuntimeFactory 把 RuntimeRegistry 包装成 agent.CreateAgentFunc。
//
// def 为进程级默认放置策略 (nil → Prefer:"local", 即"未显式声明就走本地",
// 与改造前行为一致)。注意 Prefer 是**软**偏好: 本地 runtime 未注册时, Pick 仍会
// 选中远程 worker; 反之远程全部掉线时会落回本地。硬约束 (Require) 不满足则
// Pick 返回 ErrNoRuntime, 这里**直接失败**——不许悄悄降级到能力不足的 runtime。
func RuntimeFactory(reg agent.RuntimeRegistry, def *agent.Placement) agent.CreateAgentFunc {
	return func(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
		if reg == nil {
			return nil, fmt.Errorf("worker: RuntimeFactory 未注入 RuntimeRegistry")
		}
		if ctx == nil {
			ctx = context.Background()
		}
		// 创建时捕获: Execute 时的 ctx 已被 runAgent 换过 (见
		// feishu/session.go:1035 同款注释), 那时 hints/trace 拿不到。
		return &runtimeRunner{
			reg:          reg,
			role:         role,
			systemPrompt: systemPrompt,
			hints:        agent.NodeExecHintsFromContext(ctx),
			ids:          trace.From(ctx),
			meta:         agent.RunMetadataFromContext(ctx),
			def:          def,
		}, nil
	}
}

// runtimeRunner 一次 stage 执行的 AgentRunner: Pick 一个 runtime 并把节点交给它。
type runtimeRunner struct {
	reg          agent.RuntimeRegistry
	role         string
	systemPrompt string
	hints        agent.NodeExecHints
	ids          trace.IDs
	meta         agent.RunMetadata
	def          *agent.Placement
}

// Placement 求解本次执行的放置策略 (导出给测试断言 AffinityKey 的补全)。
func (r *runtimeRunner) Placement() *agent.Placement {
	if r.def == nil {
		// 默认偏好本地: 未显式声明放置的节点行为与改造前一致。
		return &agent.Placement{Prefer: "local"}
	}
	p := *r.def // 值拷贝: 绝不能就地改调用方的默认策略 (它被所有节点共享)
	if p.Affinity != "" && p.AffinityKey == "" {
		// 团队亲和的分组键: 团队名。产码工作流靠它把同团队节点钉在同一 cwd。
		if k := strings.TrimSpace(r.meta.Team); k != "" {
			p.AffinityKey = k
		} else if k := strings.TrimSpace(r.meta.Purpose); k != "" {
			p.AffinityKey = k
		} else if k := strings.TrimSpace(r.ids.RunID); k != "" {
			p.AffinityKey = k
		} else {
			// 没有任何分组键时关掉亲和: 带 Affinity 但 key 为空会让所有团队
			// 共用同一条亲和记录, 反而把不相关的团队钉到一起。
			p.Affinity = ""
		}
	}
	return &p
}

func (r *runtimeRunner) Execute(ctx context.Context, userPrompt string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p := r.Placement()
	rt, err := r.reg.Pick(p)
	if err != nil {
		// fail-closed: 约束无解就失败。回落到"随便找个能跑的"会让声明了
		// browser/gpu 的节点在没有该能力的机器上跑出假产出。
		return "", fmt.Errorf("worker: 放置求解失败 (role=%s require=%v prefer=%s): %w",
			r.role, p.Require, p.Prefer, err)
	}

	ids := r.ids
	if cur := trace.From(ctx); cur.RunID != "" || cur.NodeID != "" {
		if cur.RunID != "" {
			ids.RunID = cur.RunID
		}
		if cur.NodeID != "" {
			ids.NodeID = cur.NodeID
		}
	}
	nodeID := ids.NodeID
	if nodeID == "" {
		nodeID = r.hints.Node
	}

	task := agent.RuntimeNodeTask{
		RunID:        ids.RunID,
		NodeID:       nodeID,
		Role:         r.role,
		SystemPrompt: r.systemPrompt,
		UserPrompt:   userPrompt,
		ToolProfile:  r.hints.ToolProfile,
		MaxTurns:     r.hints.MaxTurns,
		Placement:    p,
	}

	// hints/trace 回填 ctx: 命中**本地** runtime 时, 它会把这个 ctx 直接交给宿主
	// factory, 而宿主 factory 是从 ctx 读 tool_profile / max_turns 的。不回填就等于
	// 接上 runtime 层之后把节点声明丢了 (远程路径靠任务字段传, 不受影响)。
	if r.hints != (agent.NodeExecHints{}) {
		ctx = agent.WithNodeExecHints(ctx, r.hints)
	}
	if ids.RunID != "" || ids.NodeID != "" {
		ctx = trace.With(ctx, ids)
	}

	ch, err := rt.Execute(ctx, task)
	if err != nil {
		return "", fmt.Errorf("worker: runtime %s 执行 %s 失败: %w", rt.Name(), r.role, err)
	}
	// CollectRuntimeOutput 会把通道读到关闭 (否则生产侧 goroutine 永久阻塞)。
	return agent.CollectRuntimeOutput(ch)
}
