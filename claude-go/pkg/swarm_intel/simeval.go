// simeval.go — ② 回溯评测 harness: 拿真实事件的实际结局校验模拟的预测力。
//
// 动机 (M5 验收需要): "结果怎么衡量是好的"不能只靠叙事, 要和基线比。
// 本 harness 把一次社会模拟的 plurality 立场簇当成"群体预测" (sign + 强度),
// 与事后登记的 ActualStance 对比: 符号一致且 |plurality|≥0.1 记命中。
// 规模 (probability=簇占比) 不进入 hit 判据 —— 那是校准问题 (Brier), 另算。
package swarm_intel

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// EvalEvent 一条待评测事件: 模拟时的目标 + 事后才知道的真实结局。
// ActualStance ∈ [-1,1], 语义与内核立场一致 (负=反向/悲观/反对, 正=正向/乐观/支持)。
type EvalEvent struct {
	ID           string  `json:"id"`            // 业务键 (如 "ev-2026-us-election"), 幂等去重用
	Objective    string  `json:"objective"`     // 模拟目标 (内核 spawn 的议题)
	ActualStance float64 `json:"actual_stance"` // 真实结局的立场编码 -1..1
}

// EvalResult 单事件的评测结果。Hit 判据 = sign(pluralityMean)==sign(actual) 且 |pluralityMean|>=0.1。
type EvalResult struct {
	ID            string          `json:"id"`
	Objective     string          `json:"objective"`
	Mode          string          `json:"mode"`
	PluralityMean float64         `json:"plurality_mean"` // plurality 簇 (最大簇) 的立场均值 = 群体预测
	ActualStance  float64         `json:"actual_stance"`
	Hit           bool            `json:"hit"`
	Margin        float64         `json:"margin"` // |pluralityMean - actual| 距离, 越小越准
	Clusters      []stanceCluster `json:"-"`      // 终局聚类 (内部用, 不序列化)
	Emergent      []string        `json:"emergent"`
	RoundsRun     int             `json:"rounds_run"`
	StopReason    string          `json:"stop_reason"`
	Error         string          `json:"error,omitempty"`
}

// Scorecard 一批事件的汇总。HitRate 是主指标; MeanMargin 是辅助 (分辨"险胜"与"碾压")。
type Scorecard struct {
	Total      int          `json:"total"`
	Hits       int          `json:"hits"`
	HitRate    float64      `json:"hit_rate"`
	MeanMargin float64      `json:"mean_margin"`
	Results    []EvalResult `json:"results"`
	Evaluated  time.Time    `json:"evaluated"`
}

// EvalBatch 逐事件跑内核模拟并对照实际结局。单事件失败不中断整批 (Error 字段记原因)。
// minPlurality=0.1: |群体立场| <0.1 视为"骑墙", 无论符号是否一致都不算命中
// —— 没有立场的"预测"不产信息, 比瞎猜强不了多少。
func (s *Simulator) EvalBatch(ctx context.Context, events []EvalEvent, cfg SimulationConfig) *Scorecard {
	sc := &Scorecard{Total: len(events), Evaluated: time.Now()}
	if cfg.Detail == "" {
		cfg.Detail = "kernel"
	}
	for _, ev := range events {
		res := EvalResult{ID: ev.ID, Objective: ev.Objective, ActualStance: ev.ActualStance}
		simCfg := cfg
		simCfg.Mode = pickMode(ev, cfg)
		out, err := s.Simulate(ctx, "", ev.Objective, simCfg)
		if err != nil {
			res.Error = truncate(err.Error(), 200)
			sc.Results = append(sc.Results, res)
			continue
		}
		res.Mode = out.Mode
		res.Emergent = out.Emergent
		res.StopReason = stopReasonOf(out)
		res.RoundsRun = out.Rounds
		clusters := finalClustersOf(out)
		res.Clusters = clusters
		// plurality 簇 = 占比最大的终局立场簇, 其均值即"群体预测"。
		var plu stanceCluster
		for _, cl := range clusters {
			if len(cl.AgentIDs) > len(plu.AgentIDs) {
				plu = cl
			}
		}
		res.PluralityMean = plu.Mean
		res.Hit = stanceSign(plu.Mean) == stanceSign(ev.ActualStance) && math.Abs(plu.Mean) >= 0.1
		res.Margin = math.Abs(plu.Mean - ev.ActualStance)
		if res.Hit {
			sc.Hits++
		}
		sc.MeanMargin += res.Margin
		sc.Results = append(sc.Results, res)
	}
	if sc.Total > 0 {
		sc.HitRate = float64(sc.Hits) / float64(sc.Total)
		sc.MeanMargin /= float64(sc.Total)
	}
	return sc
}

// finalClustersOf 从 SimulationResult 重建终局聚类。
// kernel 模式直接重算 gap 聚类 (终局立场在 agents 里); 其他模式无轨迹, 返回空。
func finalClustersOf(out *SimulationResult) []stanceCluster {
	// SimulationResult 只带 trajectory (agentID→每轮立场), 末位即终局。
	// 用与内核相同的 gap 聚类 (>0.3 间隙切开) 保证口径一致。
	if len(out.Trajectory) == 0 {
		return nil
	}
	type pair struct {
		id string
		st float64
	}
	pairs := make([]pair, 0, len(out.Trajectory))
	for id, traj := range out.Trajectory {
		if len(traj) == 0 {
			continue
		}
		pairs = append(pairs, pair{id, traj[len(traj)-1]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].st < pairs[j].st })
	var out2 []stanceCluster
	var cur stanceCluster
	var vals []float64
	flush := func() {
		if len(cur.AgentIDs) == 0 {
			return
		}
		cur.Min, cur.Max = vals[0], vals[len(vals)-1]
		var s float64
		for _, v := range vals {
			s += v
		}
		cur.Mean = s / float64(len(vals))
		out2 = append(out2, cur)
		cur, vals = stanceCluster{}, nil
	}
	for i, p := range pairs {
		if i > 0 && p.st-pairs[i-1].st > 0.3 {
			flush()
		}
		cur.AgentIDs = append(cur.AgentIDs, p.id)
		vals = append(vals, p.st)
	}
	flush()
	return out2
}

// stopReasonOf 从 kernel_detail 取停止原因 (非 kernel 模式返回空)。
func stopReasonOf(out *SimulationResult) string {
	if out.KernelDetail == nil {
		return ""
	}
	return out.KernelDetail.StopReason
}

// pickMode 事件未指定模式时用 cfg.Mode, 都没有回退 social。
func pickMode(ev EvalEvent, cfg SimulationConfig) string {
	if cfg.Mode != "" {
		return cfg.Mode
	}
	return "social"
}

// sign 符号函数: >=0 记 +1 (含 0, 与"正向结局"一致), <0 记 -1。
func stanceSign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// Describe 给人看的两行汇总 (CLI --json 之外的人类输出)。
func (sc *Scorecard) Describe() string {
	return fmt.Sprintf("评测 %d 事件: 命中 %d (%.0f%%), 平均距离 %.2f",
		sc.Total, sc.Hits, sc.HitRate*100, sc.MeanMargin)
}
