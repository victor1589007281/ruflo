package harnesspro

// graph.go —— 进化工作流的 DAG 装配 (出卷期一张图, 每轮一张图)。
//
// 图形态 (与论文一/三节逐节点对应):
//
//	出卷期:  testsynth ──map(每训练任务一片, LLM 出卷)──▶ selfcheck(reduce, 冻结+校验)
//	每轮:    rollouts ──map(候选×任务, 执行体 rollout×k)──▶ roll_agg(reduce, concat)
//	          ──▶ analyze ──map(每候选一片, LLM 诊断)──▶ diag_all(reduce, concat)
//	          ──▶ plan ──map(每候选一片, LLM×3指令)──▶ plans_all(reduce, concat)
//	          ──▶ edit ──map(每候选一片=3计划, LLM 动刀+git)──▶ eval ──map(每候选一片)
//	          ──▶ select(reduce, 确定性: 地板→帕累托→三档)
//
// 阶段间全量交接用 reduce(concat); map 间直连的"摊平"由分片 runner 内部循环完成
// (引擎校验 reduce.from 必须是 map 类直接前驱, 不允许 concat-reduce → runner-reduce
//  的摊平链). 全部证据经 concat 汇总进 PrevOutputs, 满足论文
// "planner 每轮读到全部候选证据" 的要求。

import (
	"fmt"

	"github.com/anthropic/claude-go/pkg/graph"
)

// retryPolicy LLM 节点重试 (论文: infra_error 重跑; 单层化是引擎纪律)。
func retryPolicy() *graph.RetryPolicy {
	return &graph.RetryPolicy{MaxRetries: 2, BackoffSec: 2}
}

// nodeTimeoutSec 节点总预算 (秒): 出卷/诊断/出案/动刀单次 LLM 调用的墙钟上限。
const (
	timeoutTestSynth = 600
	timeoutAnalyze   = 600
	timeoutPlan      = 900 // 一份出案含 3 次顺序调用
	timeoutEdit      = 600
	timeoutRollout   = 900 // 一个分片含 k 次顺序调用
	timeoutEval      = 900 // 一个分片含 len(valid) 次顺序调用
)

// BuildTestSynthGraph 出卷期图: 每个训练任务一个出卷分片。
func BuildTestSynthGraph(train []Task) graph.GraphSpec {
	shards := make([]string, 0, len(train))
	for _, t := range train {
		shards = append(shards, t.ID)
	}
	return graph.GraphSpec{
		Name:    "harnesspro-testsynth",
		Version: "1",
		Nodes: []graph.NodeSpec{{
			ID:   "testsynth",
			Kind: graph.NodeKindMap,
			Agent: graph.AgentSpec{
				Role:   "testsynth",
				Prompt: "为每个训练任务合成 10 条可执行冻结测试",
			},
			Map: &graph.MapPolicy{
				Source:    graph.SourceParamPrefix + "testsynth_shards",
				Split:     graph.SplitJSONArray,
				MaxShards: 64,
			},
			Retry:      retryPolicy(),
			TimeoutSec: timeoutTestSynth,
		}, {
			ID:   "selfcheck",
			Kind: graph.NodeKindReduce,
			Agent: graph.AgentSpec{
				Role:          "selfcheck",
				Deterministic: true,
			},
			Reduce: &graph.ReducePolicy{
				From:     []string{"testsynth"},
				Strategy: graph.ReduceRunner,
			},
		}},
		Edges: []graph.EdgeSpec{
			{From: "testsynth", To: "selfcheck"},
		},
		Policies: graph.GraphPolicies{
			MaxParallel:  4,
			DefaultRetry: retryPolicy(),
		},
	}
}

// BuildEpochGraph 一轮进化的 DAG (静态骨架)。
// 分片内容不进图: rollout/candidate 分片清单由 EpochParams 在 Run 时注入
// (编排器在轮次边界已知幸存者集与任务集 —— 这正是"编排层是确定性代码"的落点)。
func BuildEpochGraph(epoch int) graph.GraphSpec {
	nodes := []graph.NodeSpec{{
		ID:   "rollouts",
		Kind: graph.NodeKindMap,
		Agent: graph.AgentSpec{
			Role:   "rollout-executor",
			Prompt: "执行体 rollout: 候选×任务×k, 机械统计",
		},
		Map: &graph.MapPolicy{
			Source:    graph.SourceParamPrefix + "rollout_shards",
			Split:     graph.SplitJSONArray,
			MaxShards: 256,
		},
		TimeoutSec: timeoutRollout,
	}, {
		ID:   "roll_agg",
		Kind: graph.NodeKindReduce,
		Agent: graph.AgentSpec{
			Role:          "roll_agg",
			Deterministic: true,
		},
		Reduce: &graph.ReducePolicy{
			From:     []string{"rollouts"},
			Strategy: graph.ReduceConcat,
		},
	}, {
		ID:   "analyze",
		Kind: graph.NodeKindMap,
		Agent: graph.AgentSpec{
			Role:   "analyzer",
			Prompt: "读全部 rollout 轨迹, 产出结构化诊断 (失败侧+浪费侧)",
		},
		Map: &graph.MapPolicy{
			Source:    graph.SourceParamPrefix + "candidate_shards",
			Split:     graph.SplitJSONArray,
			MaxShards: 16,
		},
		Retry:      retryPolicy(),
		TimeoutSec: timeoutAnalyze,
	}, {
		ID:   "diag_all",
		Kind: graph.NodeKindReduce,
		Agent: graph.AgentSpec{
			Role:          "diag_all",
			Deterministic: true,
		},
		Reduce: &graph.ReducePolicy{
			From:     []string{"analyze"},
			Strategy: graph.ReduceConcat,
		},
	}, {
		ID:   "plan",
		Kind: graph.NodeKindMap,
		Agent: graph.AgentSpec{
			Role:   "planner",
			Prompt: "读诊断+指标+跨候选证据, 出 3 份侧重不同的改进计划",
		},
		Map: &graph.MapPolicy{
			Source:    graph.SourceParamPrefix + "candidate_shards",
			Split:     graph.SplitJSONArray,
			MaxShards: 16,
		},
		Retry:      retryPolicy(),
		TimeoutSec: timeoutPlan,
	}, {
		ID:   "plans_all",
		Kind: graph.NodeKindReduce,
		Agent: graph.AgentSpec{
			Role:          "plans_all",
			Deterministic: true,
		},
		Reduce: &graph.ReducePolicy{
			From:     []string{"plan"},
			Strategy: graph.ReduceConcat,
		},
	}, {
		ID:   "edit",
		Kind: graph.NodeKindMap,
		Agent: graph.AgentSpec{
			Role:   "editor",
			Prompt: "把计划落地为完整新 harness, 独立工作区 + git commit",
		},
		// plans_all = 每候选一行的 JSON 数组 (3 份计划); 行切分后每片内含 3 计划,
		// edit runner 片内循环落地 (reduce.from 校验要求 map 的直接前驱为 map 类,
		// 不能再插一层 runner-reduce 摊平 —— 摊平职责下沉到分片内部)。
		Map: &graph.MapPolicy{
			Source:    graph.SourcePrevPrefix + "plans_all",
			Split:     graph.SplitLines,
			MaxShards: 64,
		},
		Retry:      retryPolicy(),
		TimeoutSec: timeoutEdit,
	}, {
		ID:   "eval",
		Kind: graph.NodeKindMap,
		Agent: graph.AgentSpec{
			Role:   "evaluator",
			Prompt: "验证集每任务 1 次, 冻结测试卷判定, 机械统计",
		},
		// edit 的 map 产出 = 每片一行的 Harness JSON 数组; eval 片内循环评测。
		Map: &graph.MapPolicy{
			Source:    graph.SourcePrevPrefix + "edit",
			Split:     graph.SplitLines,
			MaxShards: 64,
		},
		TimeoutSec: timeoutEval,
	}, {
		ID:   "select",
		Kind: graph.NodeKindReduce,
		Agent: graph.AgentSpec{
			Role:          "selector",
			Deterministic: true,
		},
		Reduce: &graph.ReducePolicy{
			From:     []string{"eval"},
			Strategy: graph.ReduceRunner, // runner 内是确定性 Go (零 LLM)
		},
	}}
	edges := []graph.EdgeSpec{
		{From: "rollouts", To: "roll_agg"},
		{From: "roll_agg", To: "analyze"},
		{From: "analyze", To: "diag_all"},
		{From: "diag_all", To: "plan"},
		{From: "plan", To: "plans_all"},
		{From: "plans_all", To: "edit"},
		{From: "edit", To: "eval"},
		{From: "eval", To: "select"},
	}
	return graph.GraphSpec{
		Name:     fmt.Sprintf("harnesspro-epoch-%d", epoch),
		Version:  "1",
		Nodes:    nodes,
		Edges:    edges,
		Policies: graph.GraphPolicies{MaxParallel: 6, DefaultRetry: retryPolicy()},
	}
}

// TestSynthParams 出卷期图参数。
func TestSynthParams(train []Task) map[string]string {
	shards := make([]string, 0, len(train))
	for _, t := range train {
		shards = append(shards, t.ID)
	}
	return map[string]string{"testsynth_shards": mustJSON(shards)}
}

// EpochParams 一轮进化图的参数 (编排器在 Run 时注入)。
func EpochParams(survivors []string, train []Task) map[string]string {
	rolloutShards := make([]rolloutShardIn, 0, len(survivors)*len(train))
	for _, c := range survivors {
		for _, t := range train {
			rolloutShards = append(rolloutShards, rolloutShardIn{C: c, T: t.ID})
		}
	}
	candShards := make([]string, 0, len(survivors))
	for _, c := range survivors {
		candShards = append(candShards, c)
	}
	return map[string]string{
		"rollout_shards":   mustJSON(rolloutShards),
		"candidate_shards": mustJSON(candShards),
	}
}
