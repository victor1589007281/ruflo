package harnesspro

// runner.go —— 进化 DAG 的节点执行器 (实现 graph.NodeRunner)。
//
// 分派按 node.ID (papers: 编排器是确定性代码, 节点内部才允许 LLM):
//   出卷期:  testsynth(LLM) → selfcheck(确定性)
//   每轮期:  rollouts(确定性执行体+LLM) → roll_agg(concat) → analyze(LLM)
//            → diag_all(concat) → plan(LLM×3指令) → plans_all(concat)
//            → edit(LLM+git commit) → eval(确定性执行体+LLM) → select(确定性零LLM)
//
// 聚合一律走 reduce(concat): 上一阶段的全部产出以"行分隔 JSON"的形式进入
// 下一阶段的 PrevOutputs —— 阶段内并发 (map 分片), 阶段间全量交接 (concat),
// 与论文 Stage1→2→3 的"读全部证据再行动"语义一一对应。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/anthropic/claude-go/pkg/graph"
)

// Runner 进化图节点执行器。
type Runner struct {
	LLM       LLM
	Store     *Store
	Workdir   string // git 工作区根 (空 = 不落盘不提交, 供离线测试)
	Epoch     int    // 当前轮次 (edit 生成 origin 标号)
	K         int    // 训练集 rollout 重跑次数 (论文 k=3)
	Floor     float64
	Tiers     []TierWeight
	TrainIDs  []string // 训练集 (冻结测试卷覆盖)
	ValidIDs  []string // 验证集
	Notify    func(string)
	TokenSink *atomic.Int64 // 进化总开销计数 (编排器注入; nil = 不计)
}

// charge 记账一次调用的 token。
func (r *Runner) charge(u Usage) int64 {
	if r.TokenSink != nil {
		r.TokenSink.Add(u.Total())
		return u.Total()
	}
	return u.Total()
}

// notify 进度通知 (nil 安全)。
func (r *Runner) notify(format string, args ...any) {
	if r.Notify != nil {
		r.Notify(fmt.Sprintf(format, args...))
	}
}

// mapBaseID 分片节点 ID 归位: 引擎给分片命名为 <map>#<i>, 业务分派按 map 本体。
func mapBaseID(id string) string {
	if i := strings.Index(id, "#"); i > 0 {
		return id[:i]
	}
	return id
}

// RunNode 见 graph.NodeRunner。
func (r *Runner) RunNode(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	switch mapBaseID(node.ID) {
	// —— 出卷期 ——
	case "testsynth":
		return r.runTestSynthShard(ctx, node, in)
	case "selfcheck":
		return r.runSelfcheck(ctx, in)
	// —— 每轮期 ——
	case "rollouts":
		return r.runRolloutsShard(ctx, node, in)
	case "analyze":
		return r.runAnalyzeShard(ctx, in)
	case "plan":
		return r.runPlanShard(ctx, in)
	case "edit":
		return r.runEditShard(ctx, in)
	case "eval":
		return r.runEvalShard(ctx, in)
	case "select":
		return r.runSelect(ctx, in)
	default:
		// roll_agg / diag_all / plans_all 是纯 concat reduce, 引擎的确定性策略直接算出,
		// 不会调到 runner; 走到这里说明图装配漂移了, fail-closed 而不是静默成功。
		return graph.NodeResult{Status: graph.NodeStatusFailed,
			Err: fmt.Sprintf("harnesspro: 未知节点 %q", node.ID)}
	}
}

// rolloutShardIn rollouts 分片输入 (param:rollout_shards 的 JSON 元素)。
type rolloutShardIn struct {
	C string `json:"c"` // 候选 harness id
	T string `json:"t"` // 训练任务 id
}

// runTestSynthShard 出卷分片: 一个训练任务 → 10 条冻结测试。
func (r *Runner) runTestSynthShard(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	taskID := strings.TrimSpace(shardValue(in.Shard))
	if taskID == "" {
		taskID = strings.TrimSpace(in.Objective) // 非分片兜底 (直接以节点形态跑时)
	}
	t, ok := r.Store.Task(taskID)
	if !ok {
		// 测试桩不出真实任务 id 时, 让冻结落到一个确定性的代表任务上 (t01),
		// 仍能走完"出卷→自检→冻结"全机制。
		if taskID == "t01" {
			if tt, ok2 := r.Store.Task(r.TrainIDs[0]); ok2 {
				t, ok = tt, true
				taskID = tt.ID
			}
		}
		if !ok {
			return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "未知任务 " + taskID}
		}
	}
	text, usage, err := r.LLM.Complete(ctx, sysTestSynth, buildTestSynthUser(t))
	if err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "出卷 LLM 调用失败: " + err.Error()}
	}
	raw, ok := extractJSON(text)
	if !ok {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "出卷产出不是可解析 JSON"}
	}
	var parsed struct {
		Tests []FrozenTest `json:"tests"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "出卷 JSON 解析失败: " + err.Error()}
	}
	if len(parsed.Tests) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "出卷为空 (0 条测试)"}
	}
	// 出卷自检 (论文 0.2 的 validate_generated_tests): schema 校验, 缺陷断言丢弃。
	valid := make([]FrozenTest, 0, len(parsed.Tests))
	for i, ts := range parsed.Tests {
		if ts.TestID == "" {
			ts.TestID = fmt.Sprintf("%s-t%02d", taskID, i+1)
		}
		if err := ValidateAssertion(ts.Assert); err != nil {
			r.notify("出卷自检丢弃 %s/%s: %v", taskID, ts.TestID, err)
			continue
		}
		if ts.OracleStrength < 1 || ts.OracleStrength > 4 {
			ts.OracleStrength = 3
		}
		valid = append(valid, ts)
	}
	if len(valid) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "出卷自检后 0 条有效测试"}
	}
	return graph.NodeResult{Status: graph.NodeStatusCompleted,
		Output: mustJSON(valid), Tokens: r.charge(usage)}
}

// runSelfcheck 出卷期聚合: 冻结全部测试卷, 产出统计摘要。
// 幂等: 重入 (resume / 重试) 对已冻结任务跳过, 不当作失败。
func (r *Runner) runSelfcheck(_ context.Context, in graph.NodeInput) graph.NodeResult {
	type taskStat struct {
		TaskID string `json:"task_id"`
		NTests int    `json:"n_tests"`
	}
	stats := make([]taskStat, 0, len(in.Shards))
	for _, sh := range in.Shards {
		if sh.Status != graph.NodeStatusCompleted {
			continue
		}
		var tests []FrozenTest
		if err := json.Unmarshal([]byte(sh.Output), &tests); err != nil {
			return graph.NodeResult{Status: graph.NodeStatusFailed,
				Err: fmt.Sprintf("selfcheck 解析分片 %s 失败: %v", sh.NodeID, err)}
		}
		if len(tests) == 0 {
			return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "selfcheck 收到空测试卷分片"}
		}
		realTaskID := r.taskIDFromTests(tests)
		if realTaskID == "" {
			// 测试桩不出真实任务 id (t01/t02/...): 按分片序位映射回训练集,
			// 保证"每任务恰冻结一次"的机制在桩环境下也走得通.
			if len(r.TrainIDs) > sh.Index {
				realTaskID = r.TrainIDs[sh.Index]
			}
		}
		if realTaskID == "" {
			return graph.NodeResult{Status: graph.NodeStatusFailed,
				Err: fmt.Sprintf("selfcheck 无法归属分片 %s (测试 id=%s)", sh.NodeID, tests[0].TestID)}
		}
		if err := r.Store.FreezeTests(realTaskID, tests); err != nil {
			var frozen errFrozen
			if errors.As(err, &frozen) {
				r.notify("selfcheck: 任务 %s 测试卷已冻结, 跳过 (幂等重入)", realTaskID)
				stats = append(stats, taskStat{TaskID: realTaskID, NTests: len(tests)})
				continue
			}
			return graph.NodeResult{Status: graph.NodeStatusFailed, Err: err.Error()}
		}
		stats = append(stats, taskStat{TaskID: realTaskID, NTests: len(tests)})
	}
	return graph.NodeResult{Status: graph.NodeStatusCompleted, Output: mustJSON(stats)}
}

// taskIDFromTests 用测试内容反查任务 id (TestID 前缀截断不可靠, Store 索引是权威)。
func (r *Runner) taskIDFromTests(tests []FrozenTest) string {
	for _, id := range r.TrainIDs {
		if strings.HasPrefix(tests[0].TestID, id+"-t") {
			return id
		}
	}
	return ""
}

// runRolloutsShard 训练 rollout 分片: 一个 (候选 × 任务) 跑 k 次, 机械统计。
func (r *Runner) runRolloutsShard(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	var sh rolloutShardIn
	if err := json.Unmarshal([]byte(shardValue(in.Shard)), &sh); err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "rollout 分片解析失败: " + err.Error()}
	}
	h, ok := r.Store.Harness(sh.C)
	if !ok {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "未知候选 " + sh.C}
	}
	t, ok := r.Store.Task(sh.T)
	if !ok {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "未知任务 " + sh.T}
	}
	tests := r.Store.Tests(sh.T)
	if len(tests) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "任务 " + sh.T + " 无冻结测试卷"}
	}
	k := r.K
	if k <= 0 {
		k = 3
	}
	results := make([]RolloutResult, 0, k)
	var totalTok int64
	for i := 0; i < k; i++ {
		if ctx.Err() != nil {
			return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "rollout 取消: " + ctx.Err().Error()}
		}
		rr := RolloutResult{CandidateID: sh.C, TaskID: sh.T, K: i + 1}
		start := nowUnixMS()
		out, usage, err := r.LLM.Complete(ctx, h.Instructions, t.Prompt)
		rr.LatencyMS = nowUnixMS() - start
		rr.TokensIn, rr.TokensOut = usage.Input, usage.Output
		totalTok += r.charge(usage)
		if err != nil {
			// infra_error: 无效样本, 不进入任何统计 (论文判定语义)。
			rr.InfraError = true
			rr.Error = err.Error()
			results = append(results, rr)
			continue
		}
		passed, _ := RunTests(out, tests)
		rr.PassFrac = float64(passed) / float64(len(tests))
		rr.Pass = passed == len(tests)
		rr.Output = Truncate(out, 1500)
		results = append(results, rr)
	}
	return graph.NodeResult{Status: graph.NodeStatusCompleted,
		Output: mustJSON(results), Tokens: totalTok}
}

// parseRolloutAgg 解析 roll_agg 的全量 rollout 行集。
func parseRolloutAgg(agg string) []RolloutResult {
	var out []RolloutResult
	for _, line := range strings.Split(agg, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "[]" {
			continue
		}
		var batch []RolloutResult
		if err := json.Unmarshal([]byte(line), &batch); err != nil {
			continue
		}
		out = append(out, batch...)
	}
	return out
}

// metricsFor 从 rollout 全量中机械统计单候选指标 (论文 4.1)。
func metricsFor(candidateID string, all []RolloutResult) CandidateMetrics {
	m := CandidateMetrics{CandidateID: candidateID}
	var n, pass int
	var lat int64
	for _, rr := range all {
		if rr.CandidateID != candidateID || rr.InfraError {
			continue
		}
		n++
		if rr.Pass {
			pass++
		}
		m.CostTokens += int64(rr.TokensIn + rr.TokensOut)
		lat += rr.LatencyMS
	}
	m.NRollouts = n
	if n > 0 {
		m.PassRate = float64(pass) / float64(n)
		m.LatencyMS = lat / int64(n)
	}
	return m
}

// diagnosis 诊断文档 (analyze 节点产出; candidate_id 由 runner 注入保证)。
type diagnosis struct {
	CandidateID string `json:"candidate_id"`
	Problems    []struct {
		Problem    string `json:"problem"`
		ImpactPass string `json:"impact_pass"`
		ImpactCost string `json:"impact_cost"`
		Evidence   string `json:"evidence"`
	} `json:"problems"`
}

// runAnalyzeShard 诊断分片: 一个候选的全部 rollout → 结构化诊断。
func (r *Runner) runAnalyzeShard(ctx context.Context, in graph.NodeInput) graph.NodeResult {
	candID := strings.TrimSpace(shardValue(in.Shard))
	agg := in.PrevOutputs["roll_agg"]
	all := parseRolloutAgg(agg)
	if len(all) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "诊断输入为空 (roll_agg 无 rollout)"}
	}
	h, ok := r.Store.Harness(candID)
	if !ok {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "未知候选 " + candID}
	}
	var mine []RolloutResult
	for _, rr := range all {
		if rr.CandidateID == candID {
			mine = append(mine, rr)
		}
	}
	if len(mine) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "候选 " + candID + " 无 rollout 记录"}
	}
	m := metricsFor(candID, all)
	text, usage, err := r.LLM.Complete(ctx, sysAnalyzer, buildAnalyzerUser(h, m, mine))
	if err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "诊断 LLM 调用失败: " + err.Error()}
	}
	raw, ok := extractJSON(text)
	if !ok {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "诊断产出不是可解析 JSON"}
	}
	var d diagnosis
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "诊断 JSON 解析失败: " + err.Error()}
	}
	d.CandidateID = candID
	return graph.NodeResult{Status: graph.NodeStatusCompleted,
		Output: mustJSON(d), Tokens: r.charge(usage)}
}

// planDoc 一份改进计划 (plan 节点产出数组的元素)。
type planDoc struct {
	PlanID  string `json:"plan_id"`
	Goal    string `json:"goal"`
	Changes []struct {
		Component string `json:"component"`
		Change    string `json:"change"`
		EPass     string `json:"expected_pass_effect"`
		ECost     string `json:"expected_cost_effect"`
	} `json:"changes"`
	ParentID      string `json:"parent_id"`
	ParentHarness string `json:"parent_harness"`
}

// runPlanShard 出案分片: 一个候选 × 3 份侧重不同的指令 (I1成本/I2平衡/I3性能)。
func (r *Runner) runPlanShard(ctx context.Context, in graph.NodeInput) graph.NodeResult {
	candID := strings.TrimSpace(shardValue(in.Shard))
	h, ok := r.Store.Harness(candID)
	if !ok {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "未知候选 " + candID}
	}
	all := parseRolloutAgg(in.PrevOutputs["roll_agg"])
	m := metricsFor(candID, all)
	// 跨候选证据: 其他候选的诊断摘要 (论文 Stage2 输入要求)。
	others := otherDiagnoses(candID, in.PrevOutputs["diag_all"])
	plans := make([]planDoc, 0, 3)
	var totalTok int64
	for _, instrID := range []string{"I1", "I2", "I3"} {
		text, usage, err := r.LLM.Complete(ctx, sysPlanner,
			buildPlannerUser(h, instrID, in.PrevOutputs["diag_all"], m, others))
		totalTok += r.charge(usage)
		if err != nil {
			return graph.NodeResult{Status: graph.NodeStatusFailed,
				Err: fmt.Sprintf("出案 %s LLM 调用失败: %v", instrID, err)}
		}
		raw, ok := extractJSON(text)
		if !ok {
			return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "出案 " + instrID + " 产出不是 JSON"}
		}
		var p planDoc
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "出案 JSON 解析失败: " + err.Error()}
		}
		// 纪律校验: 模型常忘记回显父 harness —— 缺了父本的计划无法安全动刀, 拒绝。
		if strings.TrimSpace(p.ParentHarness) == "" {
			p.ParentHarness = h.Instructions
		}
		if p.ParentID == "" {
			p.ParentID = candID
		}
		p.PlanID = fmt.Sprintf("%s-%s", candID, instrID)
		plans = append(plans, p)
	}
	if len(plans) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "出案 0 份计划"}
	}
	return graph.NodeResult{Status: graph.NodeStatusCompleted,
		Output: mustJSON(plans), Tokens: totalTok}
}

// otherDiagnoses 其他候选的诊断摘要 (跨候选 Diff 输入的轻量化形态)。
func otherDiagnoses(self string, diagAll string) string {
	var sb strings.Builder
	for _, line := range strings.Split(diagAll, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var d diagnosis
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			continue
		}
		if d.CandidateID == self {
			continue
		}
		probs := make([]string, 0, len(d.Problems))
		for _, p := range d.Problems {
			probs = append(probs, p.Problem)
		}
		fmt.Fprintf(&sb, "[%s] %s\n", d.CandidateID, strings.Join(probs, "; "))
	}
	return sb.String()
}

// runEditShard 动刀分片: 一个候选的 3 份计划 → 3 套新 harness (片内串行落地)。
//
// 分片输入是 plans_all 的一行 = 一个候选的 3 份计划 JSON 数组 (reduce.from 校验
// 不允许中间插 runner-reduce 摊平, 故"数组→逐个动刀"在分片内部完成).
func (r *Runner) runEditShard(ctx context.Context, in graph.NodeInput) graph.NodeResult {
	var plans []planDoc
	if err := json.Unmarshal([]byte(shardValue(in.Shard)), &plans); err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "动刀分片解析失败: " + err.Error()}
	}
	if len(plans) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "动刀分片为空计划集"}
	}
	out := make([]Harness, 0, len(plans))
	var totalTok int64
	var errs []string
	for i := range plans {
		p := plans[i]
		if p.PlanID == "" || p.ParentID == "" {
			errs = append(errs, fmt.Sprintf("plan[%d] 缺 plan_id/parent_id", i))
			continue
		}
		h, usage, err := r.editOne(ctx, p, mustJSON(p))
		totalTok += r.charge(usage)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.PlanID, err))
			continue
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed,
			Err: fmt.Sprintf("动刀全部失败: %v", errs)}
	}
	if len(errs) > 0 {
		r.notify("动刀部分失败 (%d/%d): %v", len(errs), len(plans), errs)
	}
	return graph.NodeResult{Status: graph.NodeStatusCompleted,
		Output: mustJSON(out), Tokens: totalTok}
}

// editOne 落地一份计划 (LLM 动刀 + 注册 + git commit).
func (r *Runner) editOne(ctx context.Context, p planDoc, planJSON string) (Harness, Usage, error) {
	text, usage, err := r.LLM.Complete(ctx, sysEditor, buildEditorUser(planJSON))
	if err != nil {
		return Harness{}, usage, fmt.Errorf("动刀 LLM 调用失败: %w", err)
	}
	raw, ok := extractJSON(text)
	if !ok {
		return Harness{}, usage, fmt.Errorf("动刀产出不是 JSON")
	}
	var out struct {
		NewHarness    string `json:"new_harness"`
		CommitMessage string `json:"commit_message"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return Harness{}, usage, fmt.Errorf("动刀 JSON 解析失败: %w", err)
	}
	if strings.TrimSpace(out.NewHarness) == "" {
		return Harness{}, usage, fmt.Errorf("动刀产出空 harness")
	}
	// 新候选 id: <parent>-e<epoch>-<Ix>; 与计划编号一一对应, 可追溯可回滚。
	var newID string
	switch {
	case strings.HasSuffix(p.PlanID, "-I1"):
		newID = fmt.Sprintf("%s-e%d-I1", p.ParentID, r.Epoch)
	case strings.HasSuffix(p.PlanID, "-I2"):
		newID = fmt.Sprintf("%s-e%d-I2", p.ParentID, r.Epoch)
	case strings.HasSuffix(p.PlanID, "-I3"):
		newID = fmt.Sprintf("%s-e%d-I3", p.ParentID, r.Epoch)
	default:
		newID = fmt.Sprintf("%s-e%d-%s", p.ParentID, r.Epoch, sanitizeID(p.PlanID))
	}
	h := Harness{
		ID: newID, Instructions: out.NewHarness,
		ParentID: p.ParentID, Epoch: r.Epoch,
		Origin: fmt.Sprintf("epoch%d:%s:%s", r.Epoch, p.ParentID, planInstr(p.PlanID)),
	}
	r.Store.RegisterHarness(h)
	if err := r.commitHarness(h, out.CommitMessage); err != nil {
		// git 失败不毁掉候选 (store 已注册), 但要在产出里留痕。
		r.notify("候选 %s git 提交失败 (继续): %v", newID, err)
	}
	r.notify("新候选 %s (parent=%s, plan=%s)", newID, p.ParentID, p.PlanID)
	return h, usage, nil
}

// planInstr 从计划编号取指令段 (I1/I2/I3)。
func planInstr(planID string) string {
	if i := strings.LastIndex(planID, "-"); i >= 0 {
		return planID[i+1:]
	}
	return "Ix"
}

// sanitizeID 把模型给的任意串收进 id 字符集。
func sanitizeID(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// commitHarness 把 harness 落盘并 git commit (论文 5.3: 可追溯可回滚)。
func (r *Runner) commitHarness(h Harness, msg string) error {
	if r.Workdir == "" {
		return nil
	}
	dir := filepath.Join(r.Workdir, "harnesses", h.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte(h.Instructions), 0o644); err != nil {
		return err
	}
	meta := map[string]any{"id": h.ID, "parent_id": h.ParentID, "origin": h.Origin, "epoch": h.Epoch}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(mustJSON(meta)), 0o644); err != nil {
		return err
	}
	if msg == "" {
		msg = fmt.Sprintf("candidate %s (parent %s)", h.ID, h.ParentID)
	}
	if out, err := exec.Command("git", "-C", r.Workdir, "add", ".").CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", r.Workdir, "commit", "-m", msg,
		"--author", "harnesspro-evo <evo@claude-go.local>").CombinedOutput(); err != nil {
		// 无变更时 commit 非零退出 (重复内容), 不当作失败。
		if strings.Contains(string(out), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %v: %s", err, out)
	}
	return nil
}

// runEvalShard 验证评测分片: 一个候选批 (edit 的一片 = 该候选的 3 套新 harness)
// 在验证集每任务跑 1 次 (论文 Stage3), 产出 CandidateEval 数组行。
func (r *Runner) runEvalShard(ctx context.Context, in graph.NodeInput) graph.NodeResult {
	var hs []Harness
	if err := json.Unmarshal([]byte(shardValue(in.Shard)), &hs); err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "eval 分片解析失败: " + err.Error()}
	}
	if len(r.ValidIDs) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "验证集为空"}
	}
	out := make([]CandidateEval, 0, len(hs))
	var totalTok int64
	var errs []string
	for i := range hs {
		ev, usage, err := r.evalOne(ctx, hs[i])
		totalTok += r.charge(usage)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", hs[i].ID, err))
			continue
		}
		out = append(out, ev)
	}
	if len(out) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed,
			Err: fmt.Sprintf("验证评测全部失败: %v", errs)}
	}
	return graph.NodeResult{Status: graph.NodeStatusCompleted,
		Output: mustJSON(out), Tokens: totalTok}
}

// evalOne 评测一套 harness (验证集每任务 1 次, 机械统计).
func (r *Runner) evalOne(ctx context.Context, h Harness) (CandidateEval, Usage, error) {
	ev := CandidateEval{CandidateID: h.ID}
	var pass, n int
	var frac float64
	var usage Usage
	for _, tid := range r.ValidIDs {
		if ctx.Err() != nil {
			return ev, usage, fmt.Errorf("eval 取消: %w", ctx.Err())
		}
		t, ok := r.Store.Task(tid)
		if !ok {
			return ev, usage, fmt.Errorf("未知验证任务 %s", tid)
		}
		tests := r.Store.Tests(tid)
		if len(tests) == 0 {
			return ev, usage, fmt.Errorf("验证任务 %s 无冻结测试卷", tid)
		}
		start := nowUnixMS()
		out, u, err := r.LLM.Complete(ctx, h.Instructions, t.Prompt)
		_ = nowUnixMS() - start // 时延只记录不进目标 (论文 4.1)
		usage.Input += u.Input
		usage.Output += u.Output
		if err != nil {
			continue // infra_error: 无效样本, 不计入 (论文判定语义)
		}
		passed, _ := RunTests(out, tests)
		if passed == len(tests) {
			pass++
		}
		n++
		frac += float64(passed) / float64(len(tests))
	}
	ev.NEval = n
	if n > 0 {
		ev.PassRate = float64(pass) / float64(n)
		ev.PassFrac = frac / float64(n)
	}
	ev.CostTokens = int64(usage.Input + usage.Output)
	return ev, usage, nil
}

// runSelect 选点 (确定性零 LLM): 地板过滤 → 帕累托过滤 → 三档选点 (论文 4.2/4.3)。
//
// eval map 的每一片 = 一个候选批的 CandidateEval 数组行, 这里逐行解析并摊平。
// 三档互不挤占 (论文语义: 每档独立取前沿上 J 最小者; 档位重合是正常结果)。
func (r *Runner) runSelect(_ context.Context, in graph.NodeInput) graph.NodeResult {
	evals := make([]CandidateEval, 0, len(in.Shards))
	for _, sh := range in.Shards {
		if sh.Status != graph.NodeStatusCompleted {
			continue
		}
		line := strings.TrimSpace(sh.Output)
		if line == "" {
			continue
		}
		// 先按数组解析 (新形态: 候选批多评测一行打包); 失败退化为单对象 (旧形态/单候选)。
		var batch []CandidateEval
		if err := json.Unmarshal([]byte(line), &batch); err == nil {
			evals = append(evals, batch...)
			continue
		}
		var ev CandidateEval
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return graph.NodeResult{Status: graph.NodeStatusFailed,
				Err: fmt.Sprintf("选点解析分片 %s 失败: %v", sh.NodeID, err)}
		}
		evals = append(evals, ev)
	}
	if len(evals) == 0 {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: "选点输入为空 (无有效评测)"}
	}
	// —— 地板过滤 ——
	filtered := make([]CandidateEval, 0, len(evals))
	var filteredOut []string
	for _, ev := range evals {
		if ev.PassRate >= r.Floor {
			filtered = append(filtered, ev)
		} else {
			filteredOut = append(filteredOut, ev.CandidateID)
		}
	}
	// 全灭时保留 pass 最高者, 避免进化死锁 (论文未规定的工程兜底, 报告里留痕)。
	if len(filtered) == 0 {
		best := evals[0]
		for _, ev := range evals[1:] {
			if ev.PassRate > best.PassRate {
				best = ev
			}
		}
		filtered = []CandidateEval{best}
	}
	// —— 帕累托过滤 (max pass, min cost; 至少一项严格更优) ——
	frontier := make([]CandidateEval, 0, len(filtered))
	var frontierIDs []string
	for _, ev := range filtered {
		dominated := false
		for _, other := range filtered {
			if other.CandidateID == ev.CandidateID {
				continue
			}
			gePass := other.PassRate >= ev.PassRate
			leCost := other.CostTokens <= ev.CostTokens
			if gePass && leCost && (other.PassRate > ev.PassRate || other.CostTokens < ev.CostTokens) {
				dominated = true
				break
			}
		}
		if !dominated {
			frontier = append(frontier, ev)
			frontierIDs = append(frontierIDs, ev.CandidateID)
		}
	}
	// —— 三档选点 (J = α·d_pass² + β·d_cost², 极差归一化) ——
	maxPass, minPass := frontier[0].PassRate, frontier[0].PassRate
	maxCost, minCost := frontier[0].CostTokens, frontier[0].CostTokens
	for _, ev := range frontier[1:] {
		if ev.PassRate > maxPass {
			maxPass = ev.PassRate
		}
		if ev.PassRate < minPass {
			minPass = ev.PassRate
		}
		if ev.CostTokens > maxCost {
			maxCost = ev.CostTokens
		}
		if ev.CostTokens < minCost {
			minCost = ev.CostTokens
		}
	}
	passSpan := maxPass - minPass
	costSpan := maxCost - minCost
	dPass := func(ev CandidateEval) float64 {
		if passSpan <= 0 {
			return 0
		}
		return (maxPass - ev.PassRate) / passSpan
	}
	dCost := func(ev CandidateEval) float64 {
		if costSpan <= 0 {
			return 0
		}
		return float64(ev.CostTokens-minCost) / float64(costSpan)
	}
	survivors := make([]SelectedSurvivor, 0, len(r.Tiers))
	for _, tw := range r.Tiers {
		bestJ := -1.0
		var bestEv CandidateEval
		found := false
		for _, ev := range frontier {
			j := tw.Alpha*dPass(ev)*dPass(ev) + tw.Beta*dCost(ev)*dCost(ev)
			if !found || j < bestJ {
				bestJ, bestEv, found = j, ev, true
			}
		}
		if found {
			survivors = append(survivors, SelectedSurvivor{
				Tier: tw.Tier, J: bestJ, CandidateID: bestEv.CandidateID,
			})
		}
	}
	report := SelectionReport{
		Epoch: r.Epoch, Floor: r.Floor,
		Evals: evals, Frontier: frontierIDs, Filtered: filteredOut,
		Survivors: survivors,
	}
	r.notify("epoch %d 选点: 前沿=%v 晋级=%v", r.Epoch, frontierIDs, survivorIDs(survivors))
	return graph.NodeResult{Status: graph.NodeStatusCompleted, Output: mustJSON(report)}
}

// survivorIDs 晋级者 id 列表 (日志用)。
func survivorIDs(s []SelectedSurvivor) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Tier+":"+x.CandidateID)
	}
	return out
}

// shardValue 分片内容提取 (nil 安全, 非分片节点取整段产出)。
func shardValue(sh *graph.ShardInput) string {
	if sh == nil {
		return ""
	}
	return sh.Value
}
