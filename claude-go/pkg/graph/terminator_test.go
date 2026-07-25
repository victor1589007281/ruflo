package graph

// 可插拔终止器回归 (design/01 §4.4, terminator.go)。
//
// 为什么这组测试值得写死: 五路信号里有三路 (收敛 / 退化 / best-of-N 回滚) 从产出
// 本身看不出任何痕迹 —— 判据坏掉的症状是"图跑满了轮次"或"交付了劣化产出",
// 两者都会被当成模型质量问题, 而不是编排 bug。所以每一路都必须有断言,
// 而且必须断言它**进了 journal** (事后归因的唯一凭据)。

import (
	"context"
	"strings"
	"testing"
)

// adaptiveSpec 一份完整的 adaptive 声明 (四个判据阈值缺一不可, 见 newAdaptiveTerminator)。
// 数值取 AdaptiveTerminator 的原值 (0-10 量纲): pass 6.0 / ε 0.5 / δ 0.3 / 连续 2 轮。
func adaptiveSpec() *TerminatorSpec {
	return &TerminatorSpec{
		Name: TerminatorAdaptive, PassScore: 6.0, ConvergeEpsilon: 0.5,
		DegradeDelta: 0.3, DegradeRounds: 2, MaxStrategyShifts: 2, MinRounds: 1,
	}
}

// rounds 构造历史轮次 (分数序列, 全部 completed)。
func rounds(scores ...float64) []LoopRound {
	out := make([]LoopRound, 0, len(scores))
	for i, s := range scores {
		out = append(out, LoopRound{Iteration: i, Result: NodeResult{
			Status: NodeStatusCompleted, Score: s, Output: "r" + string(rune('0'+i))}})
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. 五路信号逐条可表达 (纯判据层, 不过引擎)
// ---------------------------------------------------------------------------

// 断言: 达标/最小轮数/轮次耗尽(带回滚)/退化(带回滚)/策略转换(不停)/收敛/改进中
// 七种决策各自的 signal 与 Stop/Rollback 取值。这是"Until 表达不了"的直接证明:
// 其中 converged / degradation / max_rounds 三路都依赖**跨轮**的分数序列。
func TestAdaptive终止器_五路信号逐条可表达(t *testing.T) {
	cases := []struct {
		name     string
		scores   []float64
		maxIter  int
		tune     func(*TerminatorSpec)
		signal   string
		stop     bool
		rollback bool
		best     int
	}{
		{name: "达标立即停", scores: []float64{6.2}, maxIter: 5, signal: "quality_pass", stop: true},
		{name: "未达最小轮数不停", scores: []float64{3.0}, maxIter: 5,
			tune: func(s *TerminatorSpec) { s.MinRounds = 3 }, signal: "min_rounds"},
		{name: "轮次耗尽停并回滚到最高分轮", scores: []float64{5.0, 4.0, 3.0}, maxIter: 3,
			signal: "max_rounds", stop: true, rollback: true, best: 0},
		{name: "连续两轮退化停并回滚", scores: []float64{5.0, 4.0, 3.0}, maxIter: 9,
			signal: "degradation", stop: true, rollback: true, best: 0},
		{name: "收敛且还有额度则策略转换不停", scores: []float64{4.0, 4.2}, maxIter: 9,
			signal: "strategy_shift"},
		{name: "收敛且额度为零则停", scores: []float64{4.0, 4.2}, maxIter: 9,
			tune: func(s *TerminatorSpec) { s.MaxStrategyShifts = 0 }, signal: "converged", stop: true},
		{name: "仍在明显改进则继续", scores: []float64{2.0, 4.0}, maxIter: 9, signal: "improving"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := adaptiveSpec()
			if c.tune != nil {
				c.tune(spec)
			}
			term, err := resolveTerminator(spec)
			if err != nil {
				t.Fatalf("构造终止器: %v", err)
			}
			// 逐轮喂进去: 退化计数与策略转换次数是跨轮状态, 只喂最后一轮拿不到。
			all := rounds(c.scores...)
			var d TerminateDecision
			for i := range all {
				d = term.Decide(LoopContext{NodeID: "n", Scope: TerminatorScopeNode,
					MaxIterations: c.maxIter, Rounds: all[:i+1]})
			}
			if d.Signal != c.signal {
				t.Errorf("signal = %q, 期望 %q (detail=%s)", d.Signal, c.signal, d.Detail)
			}
			if d.Stop != c.stop {
				t.Errorf("Stop = %v, 期望 %v", d.Stop, c.stop)
			}
			if d.Rollback != c.rollback {
				t.Errorf("Rollback = %v, 期望 %v", d.Rollback, c.rollback)
			}
			if c.rollback && d.BestRound != c.best {
				t.Errorf("BestRound = %d, 期望 %d", d.BestRound, c.best)
			}
		})
	}
}

// 回滚候选只取 completed 轮: 回滚到失败轮会把一个已经成功的节点变成失败。
func TestAdaptive终止器_回滚不选失败轮(t *testing.T) {
	term, err := resolveTerminator(adaptiveSpec())
	if err != nil {
		t.Fatal(err)
	}
	// 第 0 轮分最高但 failed; 第 1 轮 completed 分较低; 第 2 轮更低 (触发轮次耗尽)。
	hist := []LoopRound{
		{Iteration: 0, Result: NodeResult{Status: NodeStatusFailed, Score: 9}},
		{Iteration: 1, Result: NodeResult{Status: NodeStatusCompleted, Score: 4}},
		{Iteration: 2, Result: NodeResult{Status: NodeStatusCompleted, Score: 3}},
	}
	var d TerminateDecision
	for i := range hist {
		d = term.Decide(LoopContext{NodeID: "n", MaxIterations: 3, Rounds: hist[:i+1]})
	}
	if !d.Rollback || d.BestRound != 1 {
		t.Fatalf("应回滚到第 1 轮 (唯一 completed 且非本轮), 实得 rollback=%v best=%d", d.Rollback, d.BestRound)
	}
}

// ---------------------------------------------------------------------------
// 2. 参数缺省一律拒 (量纲不可推断)
// ---------------------------------------------------------------------------

// 断言: 四个判据阈值任缺一个都在**开图时**报错, 而不是跑到第二轮才发现判据是死的。
// 理由见 TerminatorSpec 字段注释: ε=0.5 在 0-100 量纲上几乎永不满足, 给缺省
// 等于让收敛早停静默失效 —— 那正是"硬用 score >= 6 顶替"的病。
func TestAdaptive终止器_判据阈值必须显式(t *testing.T) {
	cases := map[string]func(*TerminatorSpec){
		"pass_score":       func(s *TerminatorSpec) { s.PassScore = 0 },
		"converge_epsilon": func(s *TerminatorSpec) { s.ConvergeEpsilon = 0 },
		"degrade_delta":    func(s *TerminatorSpec) { s.DegradeDelta = 0 },
		"degrade_rounds":   func(s *TerminatorSpec) { s.DegradeRounds = 0 },
	}
	for field, break_ := range cases {
		spec := adaptiveSpec()
		break_(spec)
		if _, err := resolveTerminator(spec); err == nil {
			t.Errorf("%s 缺省应被拒", field)
		} else if !strings.Contains(err.Error(), field) {
			t.Errorf("%s 的报错应点名该字段: %v", field, err)
		}
	}
	// 负数同样拒 (0=不做策略转换是合法的, -1 不是)。
	spec := adaptiveSpec()
	spec.MaxStrategyShifts = -1
	if _, err := resolveTerminator(spec); err == nil {
		t.Error("max_strategy_shifts 为负应被拒")
	}
}

// 注册表: 重名一律拒绝 (静默覆盖 adaptive 等于换掉全部引用它的图的退出条件)。
func TestRegisterTerminator_重名拒绝(t *testing.T) {
	if err := RegisterTerminator(TerminatorAdaptive, newAdaptiveTerminator); err == nil {
		t.Error("重名注册应报错")
	}
	if err := RegisterTerminator("  ", newAdaptiveTerminator); err == nil {
		t.Error("空名应报错")
	}
	if err := RegisterTerminator("nil-factory", nil); err == nil {
		t.Error("nil 工厂应报错")
	}
}

// ---------------------------------------------------------------------------
// 3. 开图期校验: 与 Until 互斥 / 未注册 / 参数非法
// ---------------------------------------------------------------------------

func TestValidate_终止器声明(t *testing.T) {
	loopNode := func(l *LoopPolicy) GraphSpec {
		n := vNode("a")
		n.Loop = l
		return GraphSpec{Name: "g", Nodes: []NodeSpec{n}}
	}
	// 合法: 只声明终止器。
	if err := loopNode(&LoopPolicy{MaxIterations: 3, Terminator: adaptiveSpec()}).Validate(); err != nil {
		t.Fatalf("合法声明被拒: %v", err)
	}
	// 与 Until 并存 → 拒 (两套退出判定, 谁先谁后是纯实现细节)。
	err := loopNode(&LoopPolicy{MaxIterations: 3, Until: "score >= 75", Terminator: adaptiveSpec()}).Validate()
	if err == nil || !strings.Contains(err.Error(), "二选一") {
		t.Errorf("until 与 terminator 并存应被拒, 实得 %v", err)
	}
	// 未注册的名字 → 拒 (而不是跑起来才发现没人管终止)。
	err = loopNode(&LoopPolicy{MaxIterations: 3, Terminator: &TerminatorSpec{Name: "不存在的"}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "未注册") {
		t.Errorf("未注册终止器应被拒, 实得 %v", err)
	}
	// 参数非法 → 拒。
	bad := adaptiveSpec()
	bad.PassScore = 0
	err = loopNode(&LoopPolicy{MaxIterations: 3, Terminator: bad}).Validate()
	if err == nil || !strings.Contains(err.Error(), "pass_score") {
		t.Errorf("非法参数应被拒, 实得 %v", err)
	}
	// 组级 group.loop 走同一条校验 (validateLoopPolicy 共用)。
	g := advGroup(3, "")
	g.Group.Loop = LoopPolicy{MaxIterations: 3, Until: "ok", Terminator: adaptiveSpec()}
	err = GraphSpec{Name: "g", Nodes: []NodeSpec{g}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "二选一") {
		t.Errorf("组级并存应被拒, 实得 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4. 节点级 Loop: 收敛早停 / 退化回滚 / 回滚开关 / 策略转换改回灌
// ---------------------------------------------------------------------------

// scoreLoopNode 一个声明了终止器的自迭代节点。
func scoreLoopNode(maxIter int, spec *TerminatorSpec, feedback string) GraphSpec {
	n := vNode("w")
	n.Kind = NodeKindGate // gate 输出 score, 终止器的判据就吃它
	n.Loop = &LoopPolicy{MaxIterations: maxIter, Terminator: spec, Feedback: feedback}
	return GraphSpec{Name: "loop-term", Nodes: []NodeSpec{n}}
}

// 收敛早停: 分数 4.0 → 4.2 (差 0.2 < ε=0.5), 关掉策略转换后第 2 轮就该停,
// 而不是跑满 6 轮。这是"硬用 score >= 6 顶替会永远跑满轮次"的反证。
func TestLoop终止器_收敛早停不跑满轮次(t *testing.T) {
	spec := adaptiveSpec()
	spec.MaxStrategyShifts = 0
	stub := newStub()
	scores := []float64{4.0, 4.2, 4.3, 4.4, 4.5, 4.6}
	stub.fn["w"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "v", Score: scores[call]}
	}
	j := NewMemoryJournal()
	if _, err := fastEngine(stub, j).Run(context.Background(), scoreLoopNode(6, spec, ""), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if n := stub.callCount("w"); n != 2 {
		t.Errorf("应在第 2 轮收敛退出, 实跑 %d 轮", n)
	}
	evs := findEvents(t, j, EvLoopTerminated)
	if len(evs) != 1 {
		t.Fatalf("loop.terminated 事件 %d 条, 期望 1", len(evs))
	}
	if got, _ := evs[0].Data["signal"].(string); got != "converged" {
		t.Errorf("journal 里的终止信号 = %q, 期望 converged", got)
	}
	if got, _ := evs[0].Data["terminator"].(string); got != TerminatorAdaptive {
		t.Errorf("journal 应记下用的哪个终止器, 实得 %q", got)
	}
}

// 退化终止 + best-of-N 回滚: 分数 5.0 → 4.0 → 3.0, 连续两轮跌超 0.3。
// 断言节点最终产出是**第 0 轮**的 (不是最后一轮), 且回滚结局进了 journal。
func TestLoop终止器_退化终止并回滚到最高分轮(t *testing.T) {
	spec := adaptiveSpec()
	spec.AllowRollback = true
	stub := newStub()
	outs := []string{"最好的一版", "退化一版", "更差一版"}
	scores := []float64{5.0, 4.0, 3.0}
	stub.fn["w"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: outs[call], Score: scores[call]}
	}
	j := NewMemoryJournal()
	res, err := fastEngine(stub, j).Run(context.Background(), scoreLoopNode(9, spec, ""), RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if n := stub.callCount("w"); n != 3 {
		t.Fatalf("应在连续两轮退化后停 (第 3 轮), 实跑 %d 轮", n)
	}
	if got := res.Nodes["w"].Output; got != outs[0] {
		t.Errorf("产出 = %q, 期望回滚到第 0 轮的 %q", got, outs[0])
	}
	if got := res.Nodes["w"].Score; got != 5.0 {
		t.Errorf("评分应随回滚一起回到 5.0, 实得 %v", got)
	}
	ev := findEvents(t, j, EvLoopTerminated)[0]
	if got, _ := ev.Data["signal"].(string); got != "degradation" {
		t.Errorf("终止信号 = %q, 期望 degradation", got)
	}
	if got, _ := ev.Data["rollback"].(string); got != "applied" {
		t.Errorf("回滚结局 = %q, 期望 applied", got)
	}
	if got := evInt(t, ev.Data, "best_round"); got != 0 {
		t.Errorf("best_round = %d, 期望 0", got)
	}
	// node.completed 落的必须是回滚后的产出, 否则 resume 会拿回被回滚掉的那份。
	for _, c := range findEvents(t, j, EvNodeCompleted) {
		if c.NodeID == "w" {
			if got, _ := c.Data["output"].(string); got != outs[0] {
				t.Errorf("node.completed 里的产出 = %q, 期望回滚后的 %q", got, outs[0])
			}
		}
	}
}

// 回滚开关默认关: 终止器给了建议也不改产出, **但必须记账**。
// 这是"行为变更默认关"的落点 —— 回滚会改变"产出是哪一轮的", 属治理侧。
func TestLoop终止器_未开回滚开关只记账不改产出(t *testing.T) {
	spec := adaptiveSpec() // AllowRollback 默认 false
	stub := newStub()
	outs := []string{"最好的一版", "退化一版", "更差一版"}
	scores := []float64{5.0, 4.0, 3.0}
	stub.fn["w"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: outs[call], Score: scores[call]}
	}
	j := NewMemoryJournal()
	res, err := fastEngine(stub, j).Run(context.Background(), scoreLoopNode(9, spec, ""), RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Nodes["w"].Output; got != outs[2] {
		t.Errorf("未开回滚时产出应是最后一轮 %q, 实得 %q", outs[2], got)
	}
	ev := findEvents(t, j, EvLoopTerminated)[0]
	if got, _ := ev.Data["rollback"].(string); got != "suppressed" {
		t.Errorf("回滚被抑制也必须留痕, rollback = %q", got)
	}
	if got, _ := ev.Data["rollback_reason"].(string); !strings.Contains(got, "allow_rollback") {
		t.Errorf("抑制原因应点名开关: %q", got)
	}
}

// 策略转换: 不终止, 但把声明的文案**追加**到下一轮回灌末尾 (原 Feedback 模板保留)。
// 信号同时落到下一轮的 loop.iteration 事件上 —— 否则"第 2 轮提示词为什么变了"无解。
func TestLoop终止器_策略转换改写下一轮回灌(t *testing.T) {
	spec := adaptiveSpec()
	spec.StrategyShiftFeedback = "\n[策略转换] 换个方向"
	stub := newStub()
	scores := []float64{4.0, 4.2, 4.3}
	stub.fn["w"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "v", Score: scores[call]}
	}
	j := NewMemoryJournal()
	if _, err := fastEngine(stub, j).Run(context.Background(),
		scoreLoopNode(3, spec, "上一轮: {prev_output}"), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if n := stub.callCount("w"); n != 3 {
		t.Fatalf("策略转换不该终止, 期望跑满 3 轮, 实跑 %d", n)
	}
	// 第 0→1 轮是 improving (无文案), 第 1 轮才收敛 → 文案落在**第 2 轮**的输入上。
	if fb := stub.input("w", 1).Feedback; strings.Contains(fb, "[策略转换]") {
		t.Errorf("第 1 轮 (improving) 不该带策略转换文案: %q", fb)
	}
	fb := stub.input("w", 2).Feedback
	if !strings.Contains(fb, "上一轮: v") {
		t.Errorf("原 Feedback 模板必须保留, 实得 %q", fb)
	}
	if !strings.Contains(fb, "[策略转换]") {
		t.Errorf("策略转换文案未追加, 实得 %q", fb)
	}
	var shifted bool
	for _, ev := range findEvents(t, j, EvLoopIteration) {
		if s, _ := ev.Data["signal"].(string); s == "strategy_shift" {
			shifted = true
		}
	}
	if !shifted {
		t.Error("loop.iteration 事件应带上 strategy_shift 信号")
	}
}

// Until 路径**一字未变**: 同一张图只写 Until 时, 轮数与 journal 事件与改造前一致
// (loop.iteration 不带 signal 键, 且不产生 loop.terminated)。
func TestLoop终止器_未声明时Until路径不受影响(t *testing.T) {
	stub := newStub()
	stub.fn["w"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "v", Score: float64(70 + call*10)}
	}
	n := vNode("w")
	n.Kind = NodeKindGate
	n.Loop = &LoopPolicy{MaxIterations: 5, Until: "score >= 80"}
	j := NewMemoryJournal()
	if _, err := fastEngine(stub, j).Run(context.Background(),
		GraphSpec{Name: "until", Nodes: []NodeSpec{n}}, RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := stub.callCount("w"); got != 2 {
		t.Errorf("Until 路径轮数 = %d, 期望 2", got)
	}
	if n := len(findEvents(t, j, EvLoopTerminated)); n != 0 {
		t.Errorf("未声明终止器不该出现 loop.terminated, 实得 %d 条", n)
	}
	for _, ev := range findEvents(t, j, EvLoopIteration) {
		if _, has := ev.Data["signal"]; has {
			t.Error("Until 路径的 loop.iteration 不该多出 signal 键")
		}
	}
}

// ---------------------------------------------------------------------------
// 5. 组级 loop-group: 终止 + resume 不再重跑
// ---------------------------------------------------------------------------

// termGroup 一个用终止器控制轮数的对抗组 (gen→critic)。
func termGroup(maxIter int, spec *TerminatorSpec) NodeSpec {
	g := advGroup(maxIter, "上一轮: {prev_output}")
	g.Group.Loop = LoopPolicy{MaxIterations: maxIter, Terminator: spec,
		Feedback: "上一轮: {prev_output}"}
	return g
}

// 组级收敛早停: critic 分数 40 → 41 (差 1 < ε=2), 关策略转换后第 2 轮停,
// 且 loop.group.terminated 记下信号与最终产出。
func TestLoopGroup终止器_收敛早停并记账(t *testing.T) {
	spec := &TerminatorSpec{Name: TerminatorAdaptive, PassScore: 80, ConvergeEpsilon: 2,
		DegradeDelta: 3, DegradeRounds: 2, MaxStrategyShifts: 0}
	stub := newStub()
	scores := []float64{40, 41, 42, 43, 44}
	stub.fn["critic"] = func(call int, _ NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "评审", Score: scores[call]}
	}
	j := NewMemoryJournal()
	if _, err := fastEngine(stub, j).Run(context.Background(),
		GraphSpec{Name: "adv", Nodes: []NodeSpec{termGroup(5, spec)}}, RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if n := len(findEvents(t, j, EvGroupIteration)); n != 2 {
		t.Errorf("组轮次 = %d, 期望 2 (第 2 轮收敛)", n)
	}
	evs := findEvents(t, j, EvGroupTerminated)
	if len(evs) != 1 {
		t.Fatalf("loop.group.terminated %d 条, 期望 1", len(evs))
	}
	if got, _ := evs[0].Data["signal"].(string); got != "converged" {
		t.Errorf("组终止信号 = %q, 期望 converged", got)
	}
	if got, _ := evs[0].Data["result_from"].(string); got != "critic" {
		t.Errorf("终止事件应记下组产出节点, 实得 %q", got)
	}
}

// resume 不得推翻早停。
//
// 触发窗口很窄但真实: runLoopGroup 先记 loop.group.terminated 再由 execNode 记
// node.completed, 崩在这两条事件之间, 恢复时组节点没有缓存产出 → 会重新进
// runLoopGroup。此时新进程里的终止器**没有历史** (收敛/退化都是跨轮判断),
// 若不认那条终止事件, 它会把剩下的 3 轮全跑完, 早停判定作废。
//
// 手工构造这段 journal (与 TestLoopGroupResumeSkipsFinishedIterations 同法):
// 两轮迭代 + 一条终止事件, 无 node.completed。
func TestLoopGroup终止器_resume认终止事件不再重跑(t *testing.T) {
	spec := &TerminatorSpec{Name: TerminatorAdaptive, PassScore: 80, ConvergeEpsilon: 2,
		DegradeDelta: 3, DegradeRounds: 2, MaxStrategyShifts: 0}
	j := NewMemoryJournal()
	for _, ev := range []Event{
		{Type: EvRunCreated, RunID: "run-term"},
		{Type: EvGroupIteration, RunID: "run-term", NodeID: "adv",
			Data: map[string]any{"iteration": 0, "status": NodeStatusCompleted, "output": "一轮稿", "score": 40.0}},
		{Type: EvGroupIteration, RunID: "run-term", NodeID: "adv",
			Data: map[string]any{"iteration": 1, "status": NodeStatusCompleted, "output": "二轮稿", "score": 41.0}},
		// 终止事件带的是**回滚后真正交付的那份** (与最后一轮的 iteration 事件不同):
		// 只看轮次事件会拿到被回滚掉的劣化产出。
		{Type: EvGroupTerminated, RunID: "run-term", NodeID: "adv",
			Data: map[string]any{"signal": "converged", "status": NodeStatusCompleted,
				"output": "回滚后交付的一轮稿", "score": 40.0, "result_from": "critic"}},
	} {
		if err := j.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	stub := newStub()
	g := GraphSpec{Name: "adv", Nodes: []NodeSpec{termGroup(5, spec)}}
	res, err := fastEngine(stub, j).Run(context.Background(), g, RunOpts{RunID: "run-term", Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if stub.callCount("gen") != 0 || stub.callCount("critic") != 0 {
		t.Errorf("终止过的组在 resume 后又跑了: gen=%d critic=%d",
			stub.callCount("gen"), stub.callCount("critic"))
	}
	if r := res.Nodes["adv"]; r.Output != "回滚后交付的一轮稿" || r.Score != 40 {
		t.Errorf("resume 应拿终止事件里的最终产出收尾, 实得 %+v", r)
	}
}

// 终止器在运行中变得不可用时 fail-closed: 组直接失败, 不按"无判据跑满轮次"继续。
// (Validate 已在开图时拦下不存在的名字, 这里用直接调用模拟注册表被改过。)
func TestLoopGroup终止器_不可用时fail_closed(t *testing.T) {
	stub := newStub()
	j := NewMemoryJournal()
	e := fastEngine(stub, j)
	node := termGroup(5, &TerminatorSpec{Name: "运行中被摘掉的终止器"})
	rc := &runCtx{runID: "r", hooks: NopBus{}, appendEv: func(typ, id string, d map[string]any) {
		_ = j.Append(Event{Type: typ, RunID: "r", NodeID: id, Data: d})
	}, maxPar: 1}
	rc.nodeExec = chainNode(nil, stub.RunNode)
	res, iters, extra := e.runLoopGroup(context.Background(), rc, execScope{}, node, NodeInput{})
	if res.Status != NodeStatusFailed {
		t.Errorf("终止器不可用应 failed, 实得 %s", res.Status)
	}
	if iters != 0 || stub.callCount("gen") != 0 {
		t.Errorf("不该跑任何一轮, iters=%d gen=%d", iters, stub.callCount("gen"))
	}
	if extra["term_signal"] != "terminator_unavailable" {
		t.Errorf("hook 载荷应带上原因, 实得 %v", extra["term_signal"])
	}
	if n := len(findEvents(t, j, EvGroupTerminated)); n != 1 {
		t.Errorf("必须留痕, loop.group.terminated %d 条", n)
	}
}
