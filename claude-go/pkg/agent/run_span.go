// run_span.go —— KindRun 的产生方 (design/03 §4.1 第 6 种 Kind)。
//
// ---------------------------------------------------------------------------
// 为什么推翻"刻意预留"这个判断
// ---------------------------------------------------------------------------
//
// 常量注释原本写着「预留: 当前由 TraceID 隐含」。逐条核对后, 这句话只对了一半:
// TraceID 隐含的是**身份**(这条 span 属于哪次 run), 隐含不了**事实**——
// 这次 run 的 objective 是什么、跑的哪个工作流、最后是成是败、跑了多久、几个阶段。
// 这些一条都不在 TraceStore 里。
//
// 后果很具体: §4.1 说「Journal 是执行真源, TraceStore 是学习视图」, 而
// `Store.ReadRun(runID)` 读回来的东西**无法自述**——想知道这次 run 成没成功, 必须
// 再去 join trajectories.json / rewards.jsonl / team.json 三份**不同格式、不同生命
// 周期**的文件。E5 的语料导出、§4.5 的回放任务集沉淀、以及"按 run 终态筛轨迹"这类
// 最基本的操作, 全都因此要跨库拼。一条 span 就能消掉这个 join。
//
// 另外一个此前不成立、现在成立的前提: `beginRun` 是**所有**团队执行路径的唯一入口
// (专用编排器 swarm/predict 也从它下面走), 且它已经返回一个 defer 收尾闭包。也就是
// 说现在有一个"每次 run 恰好一次、且一定会执行"的挂点 —— 预留当初找不到的正是它。
//
// ---------------------------------------------------------------------------
// 为什么挂在 beginRun 的收尾闭包, 而不是运行级拦截器链
// ---------------------------------------------------------------------------
//
// 链上的 finalize 环拿得到更全的信息 (报告/阶段统计), 但它**跑不到失败的 run**:
// 门禁环判失败会中止链, computeRunDelivery 发现阻塞性失败阶段会直接 return, 未知
// 工作流更是连链都进不去。而失败的 run 恰恰是学习最想要的那一批。
//
// defer 闭包是唯一"无论怎么退出都会走一遍"的位置。代价是拿到的是 team 的落定字段
// 而不是运行报告 —— 够用: 终态/耗时/阶段数都在 team 上。
package agent

import (
	"context"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// writeRunSpan 写一条 run Span (一次 episode 的自述)。
//
// fail-open 且零分配退出: 底座未装配时直接返回, 与其余采集点一致 (采集是观测不是治理)。
func (ptm *ProductionTeamManager) writeRunSpan(ctx context.Context, team *ProductionTeam, start time.Time) {
	if ptm == nil || ptm.traceStore == nil || team == nil {
		return
	}
	runID := trace.From(ctx).RunID
	if runID == "" {
		return // 无 trace 的执行 (测试/裸调) 不产 run span, 免得写出挂不上任何桶的孤儿
	}

	// 读一次快照就放锁: 下面的 MakeRef 可能落 Blob, 不该拿着团队锁做 IO。
	team.mu.Lock()
	status := string(team.Status)
	objective := team.Objective
	workflow := team.Workflow
	errText := team.Error
	stages := len(team.Stages)
	failed := 0
	for _, s := range team.Stages {
		if s.Status == TaskFailed {
			failed++
		}
	}
	team.mu.Unlock()

	attrs := map[string]any{
		"team":          team.Name,
		"workflow":      workflow,
		"status":        status,
		"stages":        stages,
		"stages_failed": failed,
	}
	if errText != "" {
		attrs["error"] = truncateResult(errText, 500)
	}
	ptm.traceStore.Write(tracestore.Span{
		TraceID: runID,
		SpanID:  newSpanID(),
		Kind:    tracestore.KindRun,
		Name:    team.Name,
		// 刻意不设 ParentID: run 是这棵树的根。也刻意不去回填各 node/turn span 的
		// ParentID —— 那要改四个采集点且它们互相看不见 run 的 SpanID, 收益 (树形展示)
		// 远小于风险。现状下按 TraceID 分桶已经够把一次 run 的 span 全捞出来。
		InputRef:  ptm.traceStore.MakeRef(objective),
		OutputRef: ptm.traceStore.MakeRef(errText),
		Attrs:     attrs,
		TS:        start.UnixMilli(),
		DurMS:     time.Since(start).Milliseconds(),
	})
}
