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
// # 放置策略从哪来
//
// 两级, 逐字段合并 (细节与论证见 runtimeRunner.Placement 的注释):
//
//	进程级默认 (RuntimeFactory 的 def 参数, 由部署方经 --placement-prefer 注入)
//	  ← 逐字段被 **节点级声明** 覆盖
//	    graph.AgentSpec.Placement → NodeExecHints.Placement → 这里
//
// 于是"需要 browser 的那个节点去 browser 池"是可表达的 (design/01 §4.9 逐节点放置)。
// Affinity 非空且分组键为空时用团队名补 AffinityKey。

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
//
// ws 为 cwd 档位策略 (见 workspace.go), 与 Broker 用同一个实例。
// nil / Mode 空 ⇒ **任务不带工作区声明**, 与改造前一字不变。
// 非空时工厂会把团队 cwd (RunMetadata.Cwd) 填进 RuntimeNodeTask.Workspace ——
// 这是控制面"代码该落在哪"这句话第一次真的说出口 (改造前这个字段从来没人填,
// 于是远程 worker 一律在自己的目录里产码, 控制面的编译门禁什么都看不见)。
func RuntimeFactory(reg agent.RuntimeRegistry, def *agent.Placement, ws *WorkspacePolicy) agent.CreateAgentFunc {
	return func(ctx context.Context, role, systemPrompt string) (agent.AgentRunner, error) {
		if reg == nil {
			return nil, fmt.Errorf("worker: RuntimeFactory 未注入 RuntimeRegistry")
		}
		if err := ws.Validate(); err != nil {
			return nil, err
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
			ws:           ws,
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
	ws           *WorkspacePolicy
}

// Placement 求解本次执行的放置策略 (导出给测试断言 AffinityKey 的补全)。
//
// 优先级: **节点声明 (hints.Placement) 逐字段压过进程级默认 (r.def)**。
//
// 为什么是逐字段合并而不是"节点声明了就整份替换"(后者更直觉):
// 进程级默认里的 Affinity:"team" 是产码工作流的命脉 —— 同团队节点必须落同一
// runtime 才共享 cwd, 否则上一阶段写的代码在下一阶段消失 (编译门禁在 <cwd>/go.mod
// 上跑)。一个只想声明 `require:["browser"]` 的渲染节点若因此丢掉团队亲和, 就会被
// 派到另一台机器的另一个工作区, 症状是"渲染节点看不见前面生成的 HTML", 而作者
// 完全不会想到是自己那行 require 造成的。所以: 节点没提的字段一律继承默认,
// 提了的字段 (含显式关掉亲和写 affinity:"") 才覆盖。
func (r *runtimeRunner) Placement() *agent.Placement {
	var p agent.Placement
	switch {
	case r.def != nil:
		p = *r.def // 值拷贝: 绝不能就地改调用方的默认策略 (它被所有节点共享)
	case r.hints.Placement != nil:
		// 有节点声明但无进程级默认: 不再补 Prefer:"local" —— 节点显式声明了约束,
		// 替它塞一个本地偏好等于悄悄改它的放置。
	default:
		// 默认偏好本地: 未显式声明放置的节点行为与改造前一致。
		return &agent.Placement{Prefer: "local"}
	}
	if h := r.hints.Placement; h != nil {
		if len(h.Require) > 0 {
			// 硬约束整份替换: 与默认求并集会让"节点想放宽"变成"节点想加严", 而
			// Require 是 fail-closed 的 —— 多一条不满足的标签直接让节点无处可跑。
			p.Require = append([]string(nil), h.Require...)
		}
		if h.Prefer != "" {
			p.Prefer = h.Prefer
		}
		if h.Affinity != "" {
			p.Affinity = h.Affinity
			p.AffinityKey = h.AffinityKey // 亲和与分组键成对覆盖, 否则会拿旧键去配新口径
		}
	}
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
	// cwd 档位启用时才声明工作区 (未启用 ⇒ 字段留空 ⇒ worker 侧走原来那条判定)。
	// 这里填的是**控制面的团队 cwd**: local/pvc 档要求 worker 提供同一路径,
	// git 档把它作为控制面侧的同步落点 (编译门禁就在那里跑)。
	if r.ws.Enabled() {
		task.Workspace = strings.TrimSpace(r.meta.Cwd)
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
