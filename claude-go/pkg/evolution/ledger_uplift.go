// ledger_uplift.go —— 13.8.4「注入配对 uplift」：配对归因的账本折叠实现。
//
// 规划原文（planning-evo-track.html §13.8.4）：injection_uplift 配对化——现指标只有
// "注入后的表现"，无对照组。通电后按 (节点，目标特征签名) 分桶，同桶内"有注入 run"
// 对"无注入 run"求差——这是 SkillAudit 论文（arXiv:2606.14239）配对审计思想在 run 级
// 的最小实现，替代"感觉变好了"。
//
// 与既有 InjectionUpliftGlobal（pkg/agent）的差异：全局版是 injRate − EMA 基线，基线
// 被全局成功率污染（不同节点的难度天差地别）；本实现把对照限制在**同节点同目标签名**
// 的 run 之间，才是"这条经验注入后到底有没有用"的可归因差值。
//
// 数据源（全部既有账本，无新采集管线）：
//
//	<state>/evolution/injections.json   注入登记（13.8-P0 起 RecordInjectionScoredRun 带 RunID）
//	<state>/evolution/trajectories.json 每阶段轨迹（run/node/objective/success）
//	<state>/evolution/rewards.jsonl     节点级奖励（可选：同桶两侧都有时用连续分）
//
// 两条如实的边界：
//  1. 无 RunID 的注入记录（13.8-P0 之前的老数据）无法配对，跳过不猜；相应轨迹行会被
//     当作基线臂计入——这是配对法上线过渡期的已知偏置，记录数随老数据滚出窗口而衰减。
//  2. "无注入 run"是自然对照不是随机对照：检索器是否命中经验本身与目标相关。桶内
//     差值回答"有经验时是不是更好"，回答不了"该不该注入"——后者是 #10 真 A/B 的活。
package evolution

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/learners"
)

// injectionRec injections.json 一行的最小字段（与 pkg/agent.InjectionRecord 的磁盘
// 契约对齐）。本包不能反向 import pkg/agent——pkg/agent 已 import 本包，会成环；
// 理由与 learners 包头"格式即契约"一节相同。
type injectionRec struct {
	TaskID    string    `json:"taskId"`
	Success   bool      `json:"success"`
	Score     float64   `json:"score"`
	RunID     string    `json:"runId"`
	Timestamp time.Time `json:"timestamp"`
}

// readInjections 读注入登记（数组整体 MarshalIndent 的单文件，非 jsonl）。
func readInjections(path string) []injectionRec {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []injectionRec
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// InjectionUpliftPaired 配对 uplift 折叠结果。
type InjectionUpliftPaired struct {
	// Uplift 跨桶加权配对差值（[-1,1]；桶内 = 注入臂均值 − 基线臂均值，两臂口径均为
	// [0,1] 的"成功度"）。NaN = 无可判定桶（键缺失语义：调用方不落 0）。
	Uplift float64
	// Buckets 两侧都有样本的 (节点,签名) 桶数。0 = 无数据；个位数 = 别当结论用。
	Buckets int
	// InjRuns/BaseRuns 判定桶内两臂样本数合计（诊断配对余量用）。
	InjRuns  int
	BaseRuns int
}

// FoldInjectionUplift 按 (节点, 目标特征签名) 分桶的配对 uplift。
//
// 桶键 = (stage 名, learners.ObjectiveSignature(目标))——签名复用 AWM 归纳的同一函数，
// 两处对"同一类任务"的认知天然一致。
// 结果口径：跨桶按 min(n_inj, n_base) 加权平均（桶不均衡时小桶不被大桶淹没）。
func FoldInjectionUplift(stateDir string) InjectionUpliftPaired {
	recs := readInjections(filepath.Join(stateDir, "evolution", "injections.json"))
	trajs := learners.LoadTrajectories(filepath.Join(stateDir, "evolution", "trajectories.json"))
	if len(recs) == 0 || len(trajs) == 0 {
		return InjectionUpliftPaired{Uplift: math.NaN()}
	}

	// 注入臂登记：node → runID 集合。同 (run,node) 重复记录取最新（重试重写场景）。
	injRunsByNode := map[string]map[string]bool{}
	for _, r := range recs {
		if r.RunID == "" || r.TaskID == "" {
			continue // 老/坏记录无法配对，不猜
		}
		m := injRunsByNode[r.TaskID]
		if m == nil {
			m = map[string]bool{}
			injRunsByNode[r.TaskID] = m
		}
		m[r.RunID] = true
	}
	if len(injRunsByNode) == 0 {
		return InjectionUpliftPaired{Uplift: math.NaN()}
	}

	// 节点级奖励分（可选口径）：run → node → score（[-1,1]，ScoreRuns 已 clamp）。
	// 只对带 node_id 的奖励行有意义；run 级奖励（episode/全局门禁）不进这里——
	// 把它们摊到阶段桶上会把"别的阶段的成败"记到本阶段头上（stageFeedbackScore 同款约束）。
	nodeScores := map[string]map[string]float64{}
	if rows := learners.LoadRewards(filepath.Join(stateDir, "evolution", "rewards.jsonl")); len(rows) > 0 {
		for runID, sc := range learners.ScoreRuns(rows) {
			if len(sc.NodeScores) > 0 {
				nodeScores[runID] = sc.NodeScores
			}
		}
	}

	// 轨迹行 → 桶。每行保留两种口径的取值，桶内统一选一种（见下）。
	type outcome struct {
		injected  bool
		hasScore  bool
		score01   float64 // 节点级奖励分映到 [0,1]
		success01 float64 // 轨迹成败映到 {0,1}
	}
	buckets := map[string][]outcome{}
	for _, tr := range trajs {
		if tr.RunID == "" || tr.StageName == "" {
			continue
		}
		o := outcome{success01: 0}
		if tr.Success {
			o.success01 = 1
		}
		if ns := nodeScores[tr.RunID]; ns != nil {
			if s, ok := ns[tr.StageName]; ok {
				o.hasScore = true
				o.score01 = (clamp01(s) + 1) / 2
			}
		}
		if m := injRunsByNode[tr.StageName]; m != nil && m[tr.RunID] {
			o.injected = true
		}
		key := tr.StageName + "\x00" + learners.ObjectiveSignature(tr.Objective)
		buckets[key] = append(buckets[key], o)
	}

	// 逐桶求差。桶内口径必须统一：全部行都有节点级奖励分 → 用连续分（对注入是否有用
	// 更敏感）；否则全部退回成败二值。混口径的桶（一半有分一半没有）会把"有没有接奖励
	// 源"当成"注入有没有用"。
	var wSum, diffSum float64
	res := InjectionUpliftPaired{}
	for _, rows := range buckets {
		scored := true
		for _, o := range rows {
			if !o.hasScore {
				scored = false
				break
			}
		}
		var nInj, nBase int
		var sumInj, sumBase float64
		for _, o := range rows {
			v := o.success01
			if scored {
				v = o.score01
			}
			if o.injected {
				nInj++
				sumInj += v
			} else {
				nBase++
				sumBase += v
			}
		}
		if nInj == 0 || nBase == 0 {
			continue // 无对照的桶不判定——这正是配对法与旧全局法的分野
		}
		w := minInt(nInj, nBase)
		diffSum += float64(w) * (sumInj/float64(nInj) - sumBase/float64(nBase))
		wSum += float64(w)
		res.Buckets++
		res.InjRuns += nInj
		res.BaseRuns += nBase
	}
	if wSum == 0 {
		return InjectionUpliftPaired{Uplift: math.NaN()}
	}
	res.Uplift = diffSum / wSum
	return res
}

// clamp01 [-1,1] → 钳制（防脏数据；合法输入下是恒等）。
func clamp01(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
