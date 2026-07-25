package graph

// join.go —— 可声明的汇聚 (join) 语义 (engine.go 语义2)。
//
// # 为什么默认必须仍是 OR
//
// OR-join 是本仓**现存全部图**的既有行为, 8+ 下游平台跑在上面。它当初被定为唯一语义
// 的理由仍然成立: 对抗模式"评分不过走重做边、过了走下一步边"两条边汇入同一下游时,
// 必然只有一条满足 —— AND 会把那个下游永远饿死。所以本文件只做一件事: 让**另一种**
// 语义可以被显式声明; 未声明时求值路径与改造前逐字节一致 (含 journal 里那句
// "or-join: 无满足的入边")。
//
// # 为什么需要 AND
//
// 汇总/报告类节点"拿不全上游就是废产出"。图层此前表达不了, 于是 M4 在 orchestrated 的
// runner 里**手工复刻了一份 AND-join**: 声明期上游只要有一个不在 PrevOutputs 里就直接
// 判失败、零 LLM 调用 (pkg/agent/orchestrated_runner.go missingDep)。那是补丁不是能力
// —— 它只在那一个 mode 里生效, 别的 mode 复用不了, 而且判据藏在 runner 里, 从图上
// 看不出这个节点是 AND 汇聚。
//
// # AND 不满足时落什么终态: 按成因分两种 (这是本文件最需要想清的一处)
//
//	上游没成功 (failed/skipped)      ⇒ failed  —— 材料缺了, 这个节点的产出必然是废的
//	上游都成功但某条边条件不成立      ⇒ skipped —— 图刻意路由掉了, "未走的分支不算失败"
//
// 都记成 skipped 的后果: 一个因上游失败而缺席的汇总节点表现为"整条分支莫名消失"
// (validate.go 那段注释警告的正是这个形态), 平台的阶段统计里它既不成功也不失败。
// 都记成 failed 的后果: 一条正常的条件路由就能把整图判死 —— graph_adapter 会把
// failed 阶段计入团队失败, 而"评分过关反而团队失败"这个 bug 已经修过一次。
//
// 两遍扫描的顺序也是刻意的: **先看上游成败, 再看边条件**。同一个节点可能既有失败的
// 上游又有不成立的条件, 若在一遍里边走边判, 最终落 failed 还是 skipped 就取决于边的
// 声明顺序 —— 那不是语义而是巧合。
//
// # 求解顺序
//
// 节点自己的 Join > 图级 GraphPolicies.DefaultJoin > JoinOr。
// 图级默认是**运行级**的 (与 MaxParallel/DefaultRetry 同一口径): 它同样覆盖组内、
// 派生子图、被引用子图的成员 —— 一次运行只有一套汇聚缺省, 不然"这个节点算 AND 还是
// OR"要先搞清它在第几层。被引用图自己声明的 policies 不参与 (见 subgraph.go)。

import "fmt"

// joinVerdict 一次 join 判定的结果。
type joinVerdict struct {
	mode   string // 实际生效的语义 (or|and), 进 journal 供归因
	status string // "" = 放行执行; NodeStatusSkipped/Failed = 不执行并落此终态
	reason string // 终态原因 (进 journal 的 reason/error)
}

// joinMode 本节点实际生效的汇聚语义。
func (dr *dagRun) joinMode(rc *runCtx, n NodeSpec) string {
	if n.Join != "" {
		return n.Join
	}
	if rc != nil && rc.policies.DefaultJoin != "" {
		return rc.policies.DefaultJoin
	}
	return JoinOr
}

// joinVerdict 判定一个**入边全部已达终态**的节点该不该执行。
// 调用方 (scheduleDAG 的 ready 扫描) 已保证每个前驱都在 dr.state 里。
func (dr *dagRun) joinVerdict(rc *runCtx, n NodeSpec) joinVerdict {
	mode := dr.joinMode(rc, n)
	preds := dr.preds[n.ID]
	if len(preds) == 0 {
		return joinVerdict{mode: mode} // 入度 0 直接就绪执行 (两种语义下都一样)
	}
	if mode == JoinAnd {
		// 第一遍: 上游成败。任一前驱未成功 ⇒ 级联失败 (零 LLM 调用)。
		for _, in := range preds {
			if pr := dr.state[in.from]; pr.Status != NodeStatusCompleted {
				return joinVerdict{mode: mode, status: NodeStatusFailed,
					reason: fmt.Sprintf("and-join: 上游 %q 未成功 (%s), 级联取消", in.from, pr.Status)}
			}
		}
		// 第二遍: 边条件。上游都成功但某条边不成立 ⇒ 被路由掉, 不算失败。
		for _, in := range preds {
			if !in.cond.Eval(dr.state[in.from]) {
				return joinVerdict{mode: mode, status: NodeStatusSkipped,
					reason: fmt.Sprintf("and-join: 入边 %s→%s 的条件 %s 不成立", in.from, n.ID, in.cond)}
			}
		}
		return joinVerdict{mode: mode}
	}
	// —— OR-join (缺省): 与改造前逐字节一致, 含 reason 文案 ——
	for _, in := range preds {
		pr := dr.state[in.from]
		if pr.Status == NodeStatusSkipped {
			continue // skipped 前驱: 本边不满足
		}
		if in.cond.Eval(pr) {
			return joinVerdict{mode: JoinOr}
		}
	}
	return joinVerdict{mode: JoinOr, status: NodeStatusSkipped, reason: "or-join: 无满足的入边"}
}
