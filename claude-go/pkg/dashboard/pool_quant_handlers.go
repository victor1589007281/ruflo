// v1.6 新增 handlers (13.7.9 池观测 + 13.8.5 进化量化驾驶舱):
//   - GET /api/pool             -> 池摘要聚合 (worker 注册表 + 本进程观测池)
//   - GET /api/evolution/quant  -> FoldSnapshot 折叠快照 (L1 健康度 + L2 效果 + 阈值告警位)
//
// 数据源注解:
//   /api/pool 的 worker 侧摘要经 PoolLister【解析器】注入 (SetPoolLister) ——
//   control 进程装配时才有 cluster Registry; 独立 dashboard (:7777) 未注入,
//   解析器为 nil, 该字段返回 null (前端照常渲染"控制面未装配"而非报错)。
//   本进程观测池经 tool.ObservedPool() 包级取数, dashboard 单独跑时与主进程
//   不共享内存 —— 注入器 PodLister 为 nil 时本进程字段照常返回 (通常零值)。
package dashboard

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution"
	"github.com/anthropic/claude-go/pkg/tool"
)

// =====================================================================
// GET /api/pool -> 池摘要 (13.7.9)
// =====================================================================

// PoolAggResp 池聚合应答: workers 各带 worker 名 + 心跳池摘要; local 为本进程
// 最近观测池 (tool.ObservedPool), 两个进程模型 (独立 dashboard / 合并 wiki API)
// 下都有定义; enabledEnv 是 CLAUDE_GO_TOOLS_POOL 环境位, 供前端区分
// "开关关着"与"开关开着但尚无会话装配过池"。
type PoolAggResp struct {
	EnvEnabled bool            `json:"envEnabled"`
	Local      *tool.PoolSnapshot `json:"local"`
	Workers    []PoolWorkerAgg `json:"workers"`
}

// PoolWorkerAgg 一个 worker 的池摘要 (+观测时间, 控制面心跳周期 30s)。
type PoolWorkerAgg struct {
	Name      string             `json:"name"`
	Caps      []string           `json:"caps,omitempty"`
	TasksDone int                `json:"tasksDone"`
	LastBeat  int64              `json:"lastBeat"`
	Pool      *tool.PoolSnapshot `json:"pool"`
}

func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	resp := PoolAggResp{
		EnvEnabled: tool.PoolEnabled(os.Getenv),
		Local:      localPoolSnapshot(),
	}
	if s.poolLister != nil {
		if workers, err := s.poolLister(); err == nil && workers != nil {
			for _, wi := range workers {
				resp.Workers = append(resp.Workers, PoolWorkerAgg{
					Name: wi.Name, Caps: wi.Caps, TasksDone: wi.TasksDone,
					LastBeat: wi.LastBeat, Pool: wi.Pool,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// localPoolSnapshot 本进程最近观测池的只读快照 (nil 安全)。
func localPoolSnapshot() *tool.PoolSnapshot {
	if p := tool.ObservedPool(); p != nil {
		snap := p.Snapshot()
		return &snap
	}
	return nil
}

// =====================================================================
// GET /api/evolution/quant -> 进化能力量化驾驶舱 (13.8.5)
// =====================================================================

// EvoQuantDTO evolution.Snapshot 的 JSON 投影。
//
// 不直接 json.Marshal(evolution.Snapshot): ① 结构体字段无 json tag, 序列化出
// Go 命名; ② NaN 必须转 null (encoding/json 会报错 "unsupported value") ——
// RewardDistKS / InjectionUplift.Uplift 的"样本不足/无可判定桶"键缺失语义靠它。
// 派生比率 (canaryWinRate / quants) 在这里补算 —— FoldSnapshot 只填 wins/total。
type EvoQuantDTO struct {
	Now string `json:"now"`

	// L1 健康度
	LearnLLMTokens1h  float64 `json:"learnLLMTokens1h"`
	TotalLLMTokens1h  float64 `json:"totalLLMTokens1h"`
	LearningCostRatio float64 `json:"learningCostRatio"`
	LearningCostOver  bool    `json:"learningCostOver"`
	RewardDistKS      *float64 `json:"rewardDistKS"` // null = 样本不足 (不判漂移)
	RewardDistKSHot   bool    `json:"rewardDistKSHot"`

	// L2 效果
	CanaryWins         int      `json:"canaryWins"`
	CanaryTotal        int      `json:"canaryTotal"`
	CanaryWinRate      *float64 `json:"canaryWinRate"`      // null = 无实验
	CanaryWinRateLow   bool     `json:"canaryWinRateLow"`   // < 0.4 (有实验才判)
	Promoted           int      `json:"promoted"`
	Survived           int      `json:"survived"`
	PromoteSurvival    *float64 `json:"promoteSurvival"`    // null = 无晋升
	PromoteSurvivalLow bool     `json:"promoteSurvivalLow"` // < 0.7 (有晋升才判)
	RollbackCount7d    float64  `json:"rollbackCount7d"`

	// 13.8.4 配对注入 uplift (null = 无可判定桶)
	InjectionUplift *float64 `json:"injectionUplift"`
	InjBuckets      int      `json:"injBuckets"`
	InjRuns         int      `json:"injRuns"`
	BaseRuns        int      `json:"baseRuns"`

	// Alerts 阈值告警位的人类可读面 (与位字段一一对应, 便于前端直接渲染横幅)。
	Alerts []string `json:"alerts,omitempty"`
}

func (s *Server) handleEvolutionQuant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	snap := evolution.FoldSnapshot(s.cfg.StateDir, time.Now())
	writeJSON(w, http.StatusOK, foldSnapshotToDTO(snap))
}

// foldSnapshotToDTO Snapshot → EvoQuantDTO (NaN→null + 派生比率 + 告警横幅)。
func foldSnapshotToDTO(snap evolution.Snapshot) EvoQuantDTO {
	dto := EvoQuantDTO{
		Now:               snap.Now.Format(time.RFC3339),
		LearnLLMTokens1h:  snap.LearnLLMTokens1h,
		TotalLLMTokens1h:  snap.TotalLLMTokens1h,
		LearningCostRatio: snap.LearningCostRatio,
		LearningCostOver:  snap.LearningCostOver,
		RewardDistKSHot:   snap.RewardDistKSHot,
		CanaryWins:        snap.CanaryWins,
		CanaryTotal:       snap.CanaryTotal,
		Promoted:          snap.Promoted,
		Survived:          snap.Survived,
		PromoteSurvivalLow: snap.PromoteSurvivalLow,
		RollbackCount7d:   snap.RollbackCount7d,
		InjBuckets:        snap.InjectionUplift.Buckets,
		InjRuns:           snap.InjectionUplift.InjRuns,
		BaseRuns:          snap.InjectionUplift.BaseRuns,
	}
	if !math.IsNaN(snap.RewardDistKS) {
		v := snap.RewardDistKS
		dto.RewardDistKS = &v
	}
	if snap.CanaryTotal > 0 {
		v := float64(snap.CanaryWins) / float64(snap.CanaryTotal)
		dto.CanaryWinRate = &v
		dto.CanaryWinRateLow = v < evolution.CanaryWinRateFloor
	}
	if snap.Promoted > 0 {
		v := snap.PromoteSurvival
		dto.PromoteSurvival = &v
	}
	if !math.IsNaN(snap.InjectionUplift.Uplift) {
		v := snap.InjectionUplift.Uplift
		dto.InjectionUplift = &v
	}

	// 告警横幅: 位字段 → 文案 (与 13.8.5 阈值表一致)。
	switch {
	case dto.LearningCostOver:
		dto.Alerts = append(dto.Alerts, "学习成本占比超 10% (近 1h)")
	}
	if dto.RewardDistKSHot {
		dto.Alerts = append(dto.Alerts, "奖励分布漂移 KS>0.3 (单日) — 评估器被投机嫌疑")
	}
	if dto.CanaryWinRateLow {
		dto.Alerts = append(dto.Alerts,
			"灰度胜率 <0.4 — 进化方向性问题 (连续 2 周需人工复核)")
	}
	if dto.PromoteSurvivalLow {
		dto.Alerts = append(dto.Alerts, "晋升 30 天生存率 <0.7 — 晋升闸过松")
	}
	return dto
}
