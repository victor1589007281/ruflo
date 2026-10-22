// recall.go 把 RetrieveFor 的"BM25 一步直出"改造成借鉴 TencentDB Agent Memory 的
// 两段式召回：BM25 粗筛候选 → Jev（决策模型）精判"该召回哪几条" → 命中才注入正文。
//
// 借鉴来源（docforge 第 19 章 + TencentDB 2026-10 实践）：
// TencentDB 把"选不选、选哪条"抽象成决策接口，候选只给 ID+说明，Jev 结构化判断命中后
// 才加载内容——实测冗余注入 token 降 40%、SWE-bench Pro 难题通过率 30%→40%。
// claude-go 的 RetrieveFor 现状是 BM25 × roleBoost × qualityWeight × successBoost ×
// upliftBoost × lifecycleBoost 五系数相乘的未校准启发式（见 RetrieveFor 内），与
// TencentDB 改造前的形态同构，因此这一改造是对口的。
//
// 关键工程纪律：
//  1. 双跑默认：未显式启用前，Jev 只判不动——返回的仍是 BM25 topK，Jev 的判断只进
//     agentDB 流水用于离线对照（攒数据 → ConformalCalibrator 标定 → 再启用）。
//  2. fail-open：决策模型超时/503/解析失败，一律回退 BM25 topK，主路径零影响。
//  3. 候选项只喂 "ID + Symptom + Tags"（最短最有判别力的描述），不喂完整 Content——
//     这是"快路径管时机、慢路径管内容"的省 token 关键。
//
// 配置（env，与 statestore 的 CLAUDE_GO_* 约定一致）：
//   CLAUDE_GO_DECISION_URL     Ollaya 端点，默认 http://127.0.0.1:11435
//   CLAUDE_GO_DECISION_RECALL  "1" 启用 Jev 精判生效；其它值 = 双跑（只判不动）
//   CLAUDE_GO_AGENTDB_URL      agentDB serve 地址（复用既有 env），空则不写流水
package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/decision"
)

// recallCandidateCap 是 BM25 粗筛后交给 Jev 精判的候选上限。
// 经验库上限 200 条；粗筛到 ~8 个候选远在决策模型 255 选项上限内，
// 也给 Jev 留出了"从略多于 topK 的集合里挑"的判断空间。
const recallCandidateCap = 8

// recallConfig 是 P0 召回精判的运行配置，从 env 读取。
type recallConfig struct {
	// Enabled 为 true 时 Jev 精判真正决定返回哪些经验；false 时双跑（只记录不改变行为）。
	Enabled bool
	// DecisionURL 是 Ollaya 端点。
	DecisionURL string
	// LogURL 是 agentDB serve 地址；空则不写流水。
	LogURL string
}

// recallConfigFromEnv 读取配置。禁用的判定：只有显式 "1" 才启用 Jev 生效。
func recallConfigFromEnv() recallConfig {
	return recallConfig{
		Enabled:     os.Getenv("CLAUDE_GO_DECISION_RECALL") == "1",
		DecisionURL: getenvDefault("CLAUDE_GO_DECISION_URL", decision.DefaultBaseURL),
		LogURL:      os.Getenv("CLAUDE_GO_AGENTDB_URL"),
	}
}

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// candidateDescriptor 是喂给 Jev 的候选项描述（不含完整 Content，省 token）。
type candidateDescriptor struct {
	ID      string `json:"id"`
	Symptom string `json:"symptom,omitempty"` // 现象（三段式之一，最短最有判别力）
	Role    string `json:"role,omitempty"`
	Tags    string `json:"tags,omitempty"`
}

// describeCandidate 从 Experience 提取候选描述。Symptom 为空时退用 Content 截断。
func describeCandidate(e *Experience) candidateDescriptor {
	d := candidateDescriptor{ID: e.ID, Symptom: e.Symptom, Role: e.Role, Tags: strings.Join(e.Tags, ",")}
	if d.Symptom == "" {
		s := e.Content
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		d.Symptom = s
	}
	return d
}

// jevRefineRecall 用决策模型对 BM25 粗筛出的候选做精判。
//
// 返回值：
//   - picked: Jev 认为该召回的经验（按 confidence 排序截到 topK）。双跑模式下不使用它改行为。
//   - judged: 是否真正调到了决策模型（false = 回退，调用方走 BM25）。
//   - 流水记录始终尽力写入（fail-open）。
//
// 设计：这是一次 Choice 判断。候选以 "编号 → 描述" 作为选项，问题问"这个任务该召回哪几条"。
// 决策模型一次前向给出每个选项的概率，我们取概率 ≥ 阈值的候选（阈值由调用方/校准器定）。
func (ee *EvolutionEngine) jevRefineRecall(
	ctx context.Context,
	role, objective string,
	candidates []*Experience,
	topK int,
	cfg recallConfig,
) (picked []*Experience, judged bool) {
	start := time.Now()
	rec := decision.Record{
		Kind:  "recall",
		Model: decision.ModelRecall,
		Extra: map[string]any{
			"role": role, "top_k": topK,
			"n_candidates": len(candidates),
		},
	}
	logger := decision.NewLogger(cfg.LogURL)
	defer func() {
		rec.LatencyMS = time.Since(start).Milliseconds()
		// fail-open：写流水失败不阻断（见 logger 注释）。
		_ = logger.Log(ctx, rec)
	}()

	// 候选为空直接回退（不发生决策调用）。
	if len(candidates) == 0 {
		rec.FellBack = true
		rec.Extra["reason"] = "no_candidates"
		return nil, false
	}

	// 构造 Choice 问题：每个候选一个选项，描述用 Symptom+Tags。
	options := make(map[string]string, len(candidates))
	byID := make(map[string]*Experience, len(candidates))
	for _, e := range candidates {
		d := describeCandidate(e)
		// 选项 label 用候选 ID；描述是"现象 + 角色 + 标签"。
		options[e.ID] = fmt.Sprintf("%s [role=%s tags=%s]", d.Symptom, d.Role, d.Tags)
		byID[e.ID] = e
	}
	rec.Extra["candidates"] = func() []string {
		ids := make([]string, 0, len(candidates))
		for _, e := range candidates {
			ids = append(ids, e.ID)
		}
		return ids
	}()

	client := &decision.Client{BaseURL: cfg.DecisionURL, Timeout: 5 * time.Second}
	state := fmt.Sprintf("任务目标: %s\n角色: %s", objective, role)
	qs := []decision.Question{{
		ID:           "recall_experience",
		Type:         decision.TypeChoice,
		Instructions: "判断完成该任务最该参考哪条经验。只选真正与任务相关的；都不相关时选把握最低的。",
		Options:      options,
	}}

	resp, err := client.Decide(ctx, decision.ModelRecall, state, qs)
	if err != nil {
		rec.FellBack = true
		rec.Extra["reason"] = "decision_error"
		rec.Extra["error"] = err.Error()
		return nil, false
	}

	ans, ok := resp.Answers["recall_experience"]
	if !ok {
		rec.FellBack = true
		rec.Extra["reason"] = "no_answer"
		return nil, false
	}
	judged = true
	rec.Extra["jev_choice"] = ans.Choice
	rec.Extra["jev_confidence"] = ans.Confidence
	rec.Extra["jev_probabilities"] = ans.Probabilities

	// 按概率从高到低收集候选，截到 topK。Jev 返回的 probabilities 是全分布，
	// 我们据此排序而非只取单一 choice——这正是"判断层给完整分布"的价值。
	type pv struct {
		e *Experience
		p float64
	}
	var ranked []pv
	for id, p := range ans.Probabilities {
		if e, ok := byID[id]; ok {
			ranked = append(ranked, pv{e, p})
		}
	}
	// 简单插入排序（候选 ≤ recallCandidateCap，无需 sort 包）。
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0 && ranked[j].p > ranked[j-1].p; j-- {
			ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
		}
	}
	for i, r := range ranked {
		if i >= topK {
			break
		}
		picked = append(picked, r.e)
	}
	if len(picked) == 0 {
		// 分布里没有可映射的候选（异常）：回退。
		rec.FellBack = true
		rec.Extra["reason"] = "empty_pick"
		return nil, false
	}
	return picked, true
}
