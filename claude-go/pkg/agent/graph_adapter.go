package agent

// graph_adapter — WorkflowDef→GraphSpec 直译器 + 图引擎接入 (design/01 §五 P1.4)。
//
// 迁移策略 (strangler): 图引擎不重写 stage 执行, 而是经 stageNodeRunner 复用
// WorkflowExecutor.ExecuteSingleStage 的全部既有能力 (角色 prompt 组装/黑板交接/
// 进化注入/重试/超时) —— 图引擎只接管**调度与恢复** (ready-set 并行 + Journal
// 事件溯源取代 checkpoints.json)。
//
// 接入两路:
//  1. wf.Mode == "graph": 显式图模式 (动态工作流可直接声明)
//  2. CLAUDE_GO_GRAPH_ENGINE=1: pipeline/fanout 全量切图引擎 (灰度开关,
//     行为等价验收后成默认, design/01 M1)

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/trace"
)

// TranslateWorkflow 把 WorkflowDef 直译为 GraphSpec (零损耗: Stages/DependsOn 一一对应)。
// Parallel 字段无需翻译 —— ready-set 调度天然并行无依赖节点。
func TranslateWorkflow(wf *WorkflowDef) (graph.GraphSpec, error) {
	if wf == nil || len(wf.Stages) == 0 {
		return graph.GraphSpec{}, fmt.Errorf("graph_adapter: 空工作流")
	}
	spec := graph.GraphSpec{
		Name:    wf.Name,
		Version: "wfdef-v1",
		Meta: graph.GraphMeta{
			ProducesCode: wf.ProducesCode,
			QualityGate:  wf.QualityGate,
		},
	}
	for _, st := range wf.Stages {
		spec.Nodes = append(spec.Nodes, graph.NodeSpec{
			ID:   st.Name,
			Kind: graph.NodeKindAgent,
			Agent: graph.AgentSpec{
				Role:   st.Role,
				Prompt: st.Prompt,
			},
		})
		for _, dep := range st.DependsOn {
			spec.Edges = append(spec.Edges, graph.EdgeSpec{From: dep, To: st.Name})
		}
	}
	if err := spec.Validate(); err != nil {
		return graph.GraphSpec{}, fmt.Errorf("graph_adapter: 直译结果非法: %w", err)
	}
	return spec, nil
}

// stageNodeRunner 图节点执行器: 复用 ExecuteSingleStage (design/01 §4.9 local runner)。
type stageNodeRunner struct {
	we        *WorkflowExecutor
	team      *ProductionTeam
	objective string
}

func (r *stageNodeRunner) RunNode(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	stage := StageDef{
		Name:   node.ID,
		Role:   node.Agent.Role,
		Prompt: node.Agent.Prompt,
	}
	objective := r.objective
	if in.Feedback != "" {
		// loop 回灌: 作为用户反馈注入 (复用 {user_feedback} 管线语义)
		objective = objective + "\n\n## 上一轮反馈\n" + in.Feedback
	}
	sr := r.we.ExecuteSingleStage(ctx, stage, objective, in.PrevOutputs, r.team)
	res := graph.NodeResult{Output: sr.Output, Err: sr.Error}
	if sr.Status == TaskCompleted {
		res.Status = graph.NodeStatusCompleted
	} else {
		res.Status = graph.NodeStatusFailed
	}
	return res
}

// executeGraph 图引擎执行入口 (wf.Mode=="graph" 或灰度开关命中时由 Execute/executePipeline 转入)。
// Journal 落 <team.dataDir>/graph-journal.jsonl; Resume 恒开 —— 重放即恢复,
// 取代 pipeline 路径的 checkpoints.json (design/01 §4.3)。
func (we *WorkflowExecutor) executeGraph(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	spec, err := TranslateWorkflow(wf)
	if err != nil {
		return nil, err
	}
	// NewFileJournal 收目录名, 内部落 <dir>/journal.jsonl
	journalDir := filepath.Join(team.dataDir, "graph-journal")
	journal, err := graph.NewFileJournal(journalDir)
	if err != nil {
		return nil, fmt.Errorf("graph_adapter: 打开 journal 失败: %w", err)
	}
	eng := &graph.Engine{
		Runner:  &stageNodeRunner{we: we, team: team, objective: objective},
		Journal: journal,
	}
	runID := trace.From(ctx).RunID
	if runID == "" {
		runID = trace.NewRunID(team.Name)
	}
	logging.Event(ctx, "graph.run.start", "team", team.Name, "workflow", wf.Name, "nodes", fmt.Sprintf("%d", len(spec.Nodes)))
	rr, runErr := eng.Run(ctx, spec, graph.RunOpts{
		RunID:     runID,
		Objective: objective,
		Resume:    true, // 重放即恢复: journal 有已完成节点则直接吃缓存
	})

	// RunResult → []StageResult (按 spec 节点声明序, 与 pipeline 产出形态一致)
	results := make([]StageResult, 0, len(spec.Nodes))
	for _, n := range spec.Nodes {
		nr, ok := rr.Nodes[n.ID]
		if !ok {
			continue // 未被调度 (取消等), 不占位
		}
		sr := StageResult{
			Name:     n.ID,
			Role:     n.Agent.Role,
			Output:   nr.Output,
			Error:    nr.Err,
			Duration: "", // 图引擎粒度的时长在 journal 事件里, StageResult 不重复记
		}
		switch nr.Status {
		case graph.NodeStatusCompleted:
			sr.Status = TaskCompleted
		case graph.NodeStatusSkipped:
			sr.Status = TaskFailed
			if sr.Error == "" {
				sr.Error = "skipped: 前置条件未满足"
			}
		default:
			sr.Status = TaskFailed
		}
		results = append(results, sr)
		// 黑板回写与 pipeline 路径对齐 (下游 harvest/handoff 依赖 <stage>-result 键)
		if team.Blackboard != nil && nr.Status == graph.NodeStatusCompleted {
			team.Blackboard.Write(n.ID+"-result", nr.Output, n.Agent.Role, "result")
		}
	}
	if runErr != nil {
		return results, runErr
	}
	if rr.Status == "failed" {
		return results, fmt.Errorf("graph_adapter: 图执行失败 (无节点完成)")
	}
	logging.Event(ctx, "graph.run.finish", "team", team.Name, "status", rr.Status)
	return results, nil
}

// graphEngineEnabled 灰度开关 (design/01 M1 验收后默认开)。
func graphEngineEnabled() bool {
	v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_ENGINE"))
	return v == "1" || strings.EqualFold(v, "true")
}

// 编译期占位引用, 防止未来重构误删 (executeGraph 由 Execute switch 与 executePipeline 灰度共用)。
var _ = time.Now
