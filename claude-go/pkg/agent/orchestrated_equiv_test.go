package agent

// orchestrated_equiv_test —— orchestrated 模式迁到图引擎 (design/01 M4) 的等价性验收。
//
// ---------------------------------------------------------------------------
// 金标准是怎么来的 (这决定了这些字面量值不值得信)
// ---------------------------------------------------------------------------
//
// 下面每一个期望值都是**在 pkg/orchestrator 还活着的时候, 用同一个桩 LLM 跑旧路径
// 真跑出来的**, 不是照着源码推断的。做法: 临时把旧 executeOrchestrated 原样复制成
// executeOrchestratedLegacy (仍调 pkg/orchestrator.Engine), 与新路径逐项比对四个维度,
// 全绿后把新路径这一侧的数值固化成本文件的字面量, 再删掉旧路径与那份临时比对文件。
//
// 四个维度 (照 graph_templates_test.go 的做法):
//
//	① 阶段序列 (名字 + 角色 + 状态) —— REPORT.md / 门禁统计 / dashboard / 下游平台读的就是它
//	② LLM 调用次数 —— 多跑一次就是多烧一次 token
//	③ 峰值并发 —— 桩里带真实 sleep, 让并行/串行在时间上真的可分辨
//	④ 提示词逐字 —— orchestrated 的内核是裸 completion, 提示词变了就是模型产出变了
//
// 外加两组旧路径专有语义的验收: **AND-join + 级联取消** (失败路径) 与
// **对抗内层轮数** (QualityTermination 三条终止条件)。
//
// 变异反证 (证明这些断言不是许愿): 逐个改坏后本文件必红 ——
//   - 并行阶段串成链 (Policies.MaxParallel=1) → ③ 报 "峰值并发不等价 旧=4 新=1"
//   - 拆掉级联闸 → 失败路径 ② 报 "旧=3 新=15" (下游全跑了) 且终态集合不同
//   - 提示词丢掉团队名前缀 → ④ 报 43 条不等价
//   - 对抗轮数 3→2 → ② 报 "旧=15 新=13"

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试装置
// ---------------------------------------------------------------------------

// orchCall 一次 LLM 调用的 (系统提示词, 用户提示词)。
type orchCall struct{ sys, user string }

// orchStubLLM 可记录调用次数/峰值并发/逐条提示词的桩 LLMClient。
//
// 桩产出**只依赖 system prompt** (= 阶段身份) 是刻意的: 下游阶段的 {prev_result}
// 因此完全确定, 提示词才可以逐字比对。若产出带调用序号, 并发抖动会让下游提示词
// 每次都不同, 第 ④ 维就退化成"能跑"。
type orchStubLLM struct {
	delay time.Duration           // 模拟耗时: 让"并行"在时间上可观测
	out   func(sys string) string // 产出函数 (nil = orchStubTag)
	// failSys/failMsg: 命中该 system prompt 的调用返回错误 (失败路径用)。
	failSys string
	failMsg string

	mu       sync.Mutex
	calls    []orchCall
	inFlight int
	peak     int
}

func (s *orchStubLLM) SimpleComplete(_ context.Context, sys, user string) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, orchCall{sys: sys, user: user})
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	if s.failSys != "" && sys == s.failSys {
		return "", fmt.Errorf("%s", s.failMsg)
	}
	if s.out != nil {
		return s.out(sys), nil
	}
	return orchStubTag(sys), nil
}

func (s *orchStubLLM) snapshot() (calls, peak int, recorded []orchCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls), s.peak, append([]orchCall(nil), s.calls...)
}

// orchStubTag 无阶段名映射时的兜底产出。
func orchStubTag(sys string) string {
	if len(sys) > 12 {
		sys = sys[:12]
	}
	return "OUT<" + sys + ">"
}

// stageOutputStub 让桩产出成为 "OUT[<阶段名>]" —— 可读且可写进金标准字面量。
func stageOutputStub(wf *WorkflowDef) func(string) string {
	byPrompt := make(map[string]string, len(wf.Stages))
	for _, st := range wf.Stages {
		byPrompt[st.Prompt] = st.Name
	}
	return func(sys string) string {
		if name, ok := byPrompt[sys]; ok {
			return "OUT[" + name + "]"
		}
		return orchStubTag(sys)
	}
}

func newOrchStubExecutor(llm *orchStubLLM) *WorkflowExecutor {
	return &WorkflowExecutor{llm: llm, notify: func(_, _ string) {}}
}

func orchSeq(rs []StageResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%s|%s|%s", r.Name, r.Role, r.Status))
	}
	return out
}

// orchFailSummary 阶段终态 + 错误文案 (排序后比对, 与执行序无关)。
func orchFailSummary(rs []StageResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%s|%s|%s", r.Name, r.Status, r.Error))
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 一、四维金标准 (逐个生产 orchestrated 工作流)
// ---------------------------------------------------------------------------

// orchGolden 一个工作流的金标准。Seq 写全是刻意的: 它是一份**独立**的期望,
// 只断言"新旧相等"发现不了两边同时被改坏。
type orchGolden struct {
	wf    func() *WorkflowDef
	calls int      // LLM 调用总次数 (含对抗内层的 generator + reviewer)
	peak  int      // 峰值并发
	seq   []string // 阶段序列 name|role|status, **按节点声明序**
}

var orchGoldens = map[string]orchGolden{
	// code-review: 3 个入口 → 4 路专家评审 → 对抗质疑 (内层 3 轮 ×(1 生成+1 评审)=6 次)
	// → 测试验证 → 报告。9 个普通阶段 + 6 = 15 次调用。
	"code-review": {
		wf: codeReviewWorkflow, calls: 15, peak: 4,
		seq: []string{
			"ingest|ingester|completed",
			"static-analysis|static-analyzer|completed",
			"context-retrieve|context-retriever|completed",
			"logic-review|logic-reviewer|completed",
			"security-review|security-reviewer|completed",
			"performance-review|performance-reviewer|completed",
			"style-review|style-reviewer|completed",
			"adversarial-challenge|skeptical-reviewer|completed",
			"test-verify|test-verifier|completed",
			"report|report-writer|completed",
		},
	},
	// testing: 12 个阶段无对抗 ⇒ 12 次调用。注意 quality-gate 是**普通 LLM 阶段**,
	// 不是 gate 节点 —— 这正是 orchestratedGraphSpec 不复用 TranslateWorkflow 的理由之一。
	"testing": {
		wf: testingWorkflow, calls: 12, peak: 4,
		seq: []string{
			"code-analysis|code-analyst|completed",
			"risk-model|risk-assessor|completed",
			"test-plan|test-planner|completed",
			"gen-unit-tests|unit-generator|completed",
			"gen-integration-tests|integration-generator|completed",
			"gen-property-tests|property-generator|completed",
			"gen-chaos-tests|chaos-engineer|completed",
			"run-tests|test-runner|completed",
			"compile-check|compile-validator|completed",
			"mutation-analysis|mutation-analyst|completed",
			"quality-gate|gate-keeper|completed",
			"test-report|test-reporter|completed",
		},
	},
	"hiring": {
		wf: hiringWorkflow, calls: 10, peak: 3,
		seq: []string{
			"parse-jd|jd-analyst|completed",
			"parse-resume|resume-parser|completed",
			"gap-analysis|gap-analyzer|completed",
			"resume-optimize|resume-coach|completed",
			"story-bank|story-coach|completed",
			"study-plan|study-planner|completed",
			"mock-behavioral|behavioral-interviewer|completed",
			"mock-technical|technical-interviewer|completed",
			"score-feedback|interview-evaluator|completed",
			"improvement-report|career-coach|completed",
		},
	},
	"parenting": {
		wf: parentingWorkflow, calls: 8, peak: 4,
		seq: []string{
			"intake|intake-counselor|completed",
			"safety-screen|safety-screener|completed",
			"academic-tutor|academic-tutor|completed",
			"psychology-coach|child-psychologist|completed",
			"parenting-advisor|parent-coach|completed",
			"development-assessor|dev-assessor|completed",
			"action-plan|action-planner|completed",
			"consultation-report|report-writer|completed",
		},
	},
	// 纯线性链 ⇒ 峰值并发 1 (这条能抓住"把串行阶段并行化"这种反向改坏)。
	"manager-lab-simulation-v2": {
		wf: managerLabSimulationV2Workflow, calls: 6, peak: 1,
		seq: []string{
			"intake|researcher|completed",
			"situate|researcher|completed",
			"simulator|architect|completed",
			"reflect|reviewer|completed",
			"evaluator|critic|completed",
			"coach|coach|completed",
		},
	},
}

// TestOrchestrated四维金标准 阶段序列 / 调用次数 / 峰值并发三维对齐旧路径实测值。
func TestOrchestrated四维金标准(t *testing.T) {
	names := make([]string, 0, len(orchGoldens))
	for n := range orchGoldens {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		g := orchGoldens[name]
		wf := g.wf()
		llm := &orchStubLLM{delay: 20 * time.Millisecond, out: stageOutputStub(wf)}
		res, err := newOrchStubExecutor(llm).executeOrchestrated(
			context.Background(), wf, "目标: "+name, newStubTeam(t, "eq-"+name))
		if err != nil {
			t.Fatalf("%s 执行失败: %v", name, err)
		}
		// ① 阶段序列 (含顺序): 旧路径这里是 map 随机序, 新路径固定为节点声明序。
		assertSameSeq(t, "orchestrated/"+name, g.seq, orchSeq(res))
		calls, peak, _ := llm.snapshot()
		// ② 调用次数
		if calls != g.calls {
			t.Errorf("%s LLM 调用次数 = %d, 金标准 %d", name, calls, g.calls)
		}
		// ③ 峰值并发
		if peak != g.peak {
			t.Errorf("%s 峰值并发 = %d, 金标准 %d (串行链被并行化 / 并行组被串行化都会命中)", name, peak, g.peak)
		}
	}
}

// TestOrchestrated阶段序列确定 旧 convertResults 遍历 map ⇒ 返回序随机。
// 跑两遍必须逐字相同 —— 这条钉住"顺带修掉的缺陷 1"不会退化回去。
func TestOrchestrated阶段序列确定(t *testing.T) {
	wf := codeReviewWorkflow()
	var first []string
	for i := 0; i < 3; i++ {
		llm := &orchStubLLM{out: stageOutputStub(wf)}
		res, err := newOrchStubExecutor(llm).executeOrchestrated(
			context.Background(), wf, "目标", newStubTeam(t, fmt.Sprintf("det-%d", i)))
		if err != nil {
			t.Fatalf("第 %d 次执行失败: %v", i+1, err)
		}
		got := orchSeq(res)
		if first == nil {
			first = got
			continue
		}
		assertSameSeq(t, fmt.Sprintf("第 %d 次与第 1 次", i+1), first, got)
	}
}

// ---------------------------------------------------------------------------
// 二、提示词逐字 (第 ④ 维)
// ---------------------------------------------------------------------------

// TestOrchestrated提示词逐字等价 三件事一次钉住:
//
//	a) **system prompt 原样下发, 占位符不替换** —— 旧 LLMRunner 只 resolve user prompt,
//	   所以 "审查任务: {objective}" 这种字面量真的进了模型上下文。这是刻意保留的旧行为
//	   (改它 = 改每个阶段的提示词), 不是遗漏;
//	b) user prompt = "{prev_result}\n\n任务目标: {objective}" 模板的替换结果;
//	c) 依赖块标签是 **<团队名>/<阶段名>** 且按 DependsOn 声明序 —— 旧路径的任务 ID
//	   就是这个形态。这一条是真跑旧路径比出来的: 少了团队名前缀, 43 条提示词全不等价。
func TestOrchestrated提示词逐字等价(t *testing.T) {
	wf := codeReviewWorkflow()
	llm := &orchStubLLM{out: stageOutputStub(wf)}
	const team = "cr-eq"
	const objective = "审查目标"
	if _, err := newOrchStubExecutor(llm).executeOrchestrated(
		context.Background(), wf, objective, newStubTeam(t, team)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	_, _, calls := llm.snapshot()

	byStage := map[string][]string{} // 阶段名 → 该阶段收到的 user prompt 列表
	promptToStage := map[string]string{}
	for _, st := range wf.Stages {
		promptToStage[st.Prompt] = st.Name
	}
	for _, c := range calls {
		name, ok := promptToStage[c.sys]
		if !ok {
			t.Fatalf("出现了不属于任何阶段的 system prompt (前 40 字: %q)", head40(c.sys))
		}
		byStage[name] = append(byStage[name], c.user)
	}

	// a) 入口阶段: 无上游 ⇒ {prev_result} 替换成空串。
	wantIngest := "\n\n任务目标: " + objective
	if got := byStage["ingest"]; len(got) != 1 || got[0] != wantIngest {
		t.Errorf("ingest 的 user prompt = %q, 期望 %q", got, wantIngest)
	}
	// a) system prompt 必须与 StageDef.Prompt **逐字相同** (含未替换的 {objective})。
	for _, st := range wf.Stages {
		if !strings.Contains(st.Prompt, "{objective}") {
			continue
		}
		found := false
		for _, c := range calls {
			if c.sys == st.Prompt {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("阶段 %q 的 system prompt 没有原样下发 (占位符被替换了?)", st.Name)
		}
	}

	// b+c) 三上游的 join 阶段: 标签带团队名前缀, 顺序 = DependsOn 声明序
	// (ingest → static-analysis → context-retrieve, **不是**字典序)。
	wantJoin := strings.Join([]string{
		"[" + team + "/ingest]\nOUT[ingest]",
		"[" + team + "/static-analysis]\nOUT[static-analysis]",
		"[" + team + "/context-retrieve]\nOUT[context-retrieve]",
	}, "\n\n") + "\n\n任务目标: " + objective
	for _, stage := range []string{"logic-review", "security-review", "performance-review", "style-review"} {
		got := byStage[stage]
		if len(got) != 1 {
			t.Errorf("%s 应恰好被调用 1 次, 实际 %d 次", stage, len(got))
			continue
		}
		if got[0] != wantJoin {
			t.Errorf("%s 的 user prompt 不等价:\n实际:\n%s\n期望:\n%s", stage, got[0], wantJoin)
		}
	}

	// 对抗阶段: 3 轮 × (generator + 1 reviewer) = 6 次, 且**每次提示词都一样**
	// (反馈从未进入提示词, 见 orchestrated_runner.go 文件头第 2 条)。
	adv := byStage["adversarial-challenge"]
	if len(adv) != 6 {
		t.Fatalf("adversarial-challenge 应被调用 6 次 (3 轮 × 2), 实际 %d 次", len(adv))
	}
	for i := 1; i < len(adv); i++ {
		if adv[i] != adv[0] {
			t.Errorf("对抗第 %d 次调用的提示词与第 1 次不同 —— 说明凭空发明了反馈回灌", i+1)
		}
	}
}

func head40(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

// ---------------------------------------------------------------------------
// 三、AND-join + 级联取消 (失败路径)
// ---------------------------------------------------------------------------

// orchCascadeGolden 上游 ingest 失败时的阶段终态集合 (排序后), 团队名占位 %s。
// 逐条来自旧路径实测: 旧引擎 unblockDownstream 要求全部上游 Completed,
// cascadeFailure 递归把下游标 Cancelled 并写 "上游任务 <任务ID> 失败, 级联取消"。
func orchCascadeGolden(team, ingestErr string) []string {
	g := []string{
		"ingest|failed|" + ingestErr,
		"static-analysis|completed|",
		"context-retrieve|completed|",
		"logic-review|failed|上游任务 " + team + "/ingest 失败, 级联取消",
		"security-review|failed|上游任务 " + team + "/ingest 失败, 级联取消",
		"performance-review|failed|上游任务 " + team + "/ingest 失败, 级联取消",
		"style-review|failed|上游任务 " + team + "/ingest 失败, 级联取消",
		"adversarial-challenge|failed|上游任务 " + team + "/logic-review 失败, 级联取消",
		"test-verify|failed|上游任务 " + team + "/adversarial-challenge 失败, 级联取消",
		"report|failed|上游任务 " + team + "/test-verify 失败, 级联取消",
	}
	sort.Strings(g)
	return g
}

// TestOrchestrated级联取消等价 致命/永久两种错误下, 终态集合、错误文案、LLM 调用
// 次数三项与旧路径实测值相同。
//
// 调用次数是这条最容易被改坏的地方: 级联闸没了的话, 下游会带着**缺一份上游产出**
// 照跑 (pkg/graph 的 OR-join 语义), 于是从 3 次变 15 次 —— 白烧 12 次 token 产出一堆
// 缺输入的废报告。
func TestOrchestrated级联取消等价(t *testing.T) {
	restore := orchSleepFn
	orchSleepFn = func(context.Context, time.Duration) bool { return true } // 退避不真睡
	defer func() { orchSleepFn = restore }()

	cases := []struct {
		name      string
		errMsg    string
		wantCalls int // ingest 的尝试次数 + 另外两个入口各 1 次
	}{
		// 致命: 永不重试 ⇒ ingest 只调 1 次, 加 2 个入口 = 3
		{"fatal", "invalid api key", 3},
		// 永久: MaxRetries=2 ⇒ ingest 调 3 次, 加 2 个入口 = 5
		{"permanent", "输出校验不通过: 结构缺失", 5},
	}
	for _, tc := range cases {
		wf := codeReviewWorkflow()
		team := "casc-" + tc.name
		llm := &orchStubLLM{out: stageOutputStub(wf), failSys: wf.Stages[0].Prompt, failMsg: tc.errMsg}
		res, _ := newOrchStubExecutor(llm).executeOrchestrated(
			context.Background(), wf, "目标", newStubTeam(t, team))

		calls, _, _ := llm.snapshot()
		if calls != tc.wantCalls {
			t.Errorf("%s LLM 调用次数 = %d, 金标准 %d", tc.name, calls, tc.wantCalls)
		}
		assertSameSeq(t, "级联/"+tc.name, orchCascadeGolden(team, tc.errMsg), orchFailSummary(res))
	}
}

// TestOrchestrated瞬态重试额度 瞬态错误的额度是 MaxRetries+MaxTransient=7 次重试
// (共 8 次尝试), 与旧 orchestrator.DefaultRetryPolicy 逐值相同。
//
// 与旧路径的**已知差异**在这里说清: 旧路径 8 次尝试之间不退避 (退避是死代码:
// handleFailure 的 sleep goroutine 空转 + CooldownFilter 未注册), 8 次在一秒内烧完、
// 全部撞同一个 429, 然后置 Suspended → 停滞恢复 3 次也救不回来 → 引擎主循环永久空转。
// 新路径真退避且末态 failed。次数 (= token 账) 等价, 时序与末态刻意不等价。
func TestOrchestrated瞬态重试额度(t *testing.T) {
	var slept []time.Duration
	restore := orchSleepFn
	orchSleepFn = func(_ context.Context, d time.Duration) bool {
		slept = append(slept, d)
		return true
	}
	defer func() { orchSleepFn = restore }()

	wf := &WorkflowDef{Name: "orch-transient", Mode: "orchestrated", Stages: []StageDef{
		{Name: "only", Role: "worker", Prompt: "干活"},
	}}
	llm := &orchStubLLM{failSys: "干活", failMsg: "429 rate limit exceeded"}
	res, _ := newOrchStubExecutor(llm).executeOrchestrated(
		context.Background(), wf, "目标", newStubTeam(t, "transient"))

	calls, _, _ := llm.snapshot()
	if calls != 8 {
		t.Errorf("瞬态错误应尝试 8 次 (1 + MaxRetries 2 + MaxTransient 5), 实际 %d 次", calls)
	}
	if len(slept) != 7 {
		t.Errorf("应退避 7 次, 实际 %d 次", len(slept))
	}
	if len(res) != 1 || res[0].Status != TaskFailed {
		t.Errorf("额度耗尽应判 failed (旧路径是 Suspended → 引擎空转), 实际 %v", orchFailSummary(res))
	}
}

// TestOrch错误分类与重试额度 分类表与决策树的单元边界 (照抄 orchestrator/errors.go)。
func TestOrch错误分类与重试额度(t *testing.T) {
	kinds := map[string]orchErrorKind{
		"":                          orchErrPermanent, // 空消息按永久
		"invalid api key":           orchErrFatal,
		"Quota Exceeded for org":    orchErrFatal,
		"unauthorized":              orchErrFatal,
		"429 too many requests":     orchErrTransient,
		"context deadline exceeded": orchErrTransient,
		"上游返回 503":                  orchErrTransient,
		"限流, 请稍后":                   orchErrTransient,
		"输出校验不通过":                   orchErrPermanent,
		// 致命优先于瞬态: 同时命中两张表时必须判 fatal (否则会对着一个坏 key 重试 8 次)
		"429 unauthorized": orchErrFatal,
	}
	for msg, want := range kinds {
		if got := classifyOrchError(msg); got != want {
			t.Errorf("classifyOrchError(%q) = %v, 期望 %v", msg, got, want)
		}
	}

	p := orchDefaultRetryPolicy()
	if p.orchMaxAttempts() != 8 {
		t.Errorf("最大尝试次数 = %d, 期望 8", p.orchMaxAttempts())
	}
	// 致命: 一次都不重试
	if d := p.ShouldRetry("invalid api key", 0); d.ShouldRetry {
		t.Error("致命错误不该重试")
	}
	// 永久: 额度 2
	for attempt, want := range map[int]bool{0: true, 1: true, 2: false} {
		if d := p.ShouldRetry("校验失败", attempt); d.ShouldRetry != want {
			t.Errorf("永久错误第 %d 次尝试后 ShouldRetry = %v, 期望 %v", attempt, d.ShouldRetry, want)
		}
	}
	// 瞬态: 额度 7
	for attempt, want := range map[int]bool{0: true, 6: true, 7: false} {
		if d := p.ShouldRetry("429", attempt); d.ShouldRetry != want {
			t.Errorf("瞬态错误第 %d 次尝试后 ShouldRetry = %v, 期望 %v", attempt, d.ShouldRetry, want)
		}
	}
	// 退避有上限 (全抖动 ⇒ 只能断言不超过 MaxDelay)
	if d := p.ShouldRetry("429", 20); d.Delay > p.MaxDelay {
		t.Errorf("退避 %v 超过上限 %v", d.Delay, p.MaxDelay)
	}
}

// ---------------------------------------------------------------------------
// 四、对抗内层轮数 (QualityTermination 三条终止条件)
// ---------------------------------------------------------------------------

// TestOrchestrated对抗轮数金标准 code-review 有 1 个对抗阶段, 内层每轮 =
// 1 次 generator + 1 次 reviewer。轮数由 QualityTermination(7.0, 0.5) 决定,
// 打分对象是 **generator 产出**。三个用例的总调用次数逐条来自旧路径实测。
func TestOrchestrated对抗轮数金标准(t *testing.T) {
	wf := codeReviewWorkflow()
	base := stageOutputStub(wf)
	cases := []struct {
		name      string
		suffix    string
		wantCalls int
	}{
		// 无关键词 ⇒ 启发式给 Overall=6.0 < 7.0 ⇒ 不达标; 前两轮样本不足 3 个无法判收敛,
		// 第 3 轮已是 maxRounds ⇒ 跑满 3 轮。9 + 3×2 = 15。
		{"无关键词跑满3轮", "", 15},
		// 含"合格" ⇒ Overall=8.0 且 Pass ⇒ 第 1 轮即终止。9 + 1×2 = 11。
		{"含合格一轮即止", " 合格", 11},
		// 含 "failed" ⇒ Pass=false Overall=4.0 ⇒ 不终止 ⇒ 跑满 3 轮。
		{"含failed跑满3轮", " failed", 15},
	}
	for _, tc := range cases {
		suffix := tc.suffix
		llm := &orchStubLLM{out: func(sys string) string { return base(sys) + suffix }}
		if _, err := newOrchStubExecutor(llm).executeOrchestrated(
			context.Background(), wf, "目标", newStubTeam(t, "adv-"+tc.name)); err != nil {
			t.Fatalf("%s 执行失败: %v", tc.name, err)
		}
		calls, _, _ := llm.snapshot()
		if calls != tc.wantCalls {
			t.Errorf("%s LLM 调用次数 = %d, 金标准 %d", tc.name, calls, tc.wantCalls)
		}
	}
}

// TestOrch质量评分三条终止条件 直接打 orchQualityTermination 的三条边界,
// 免得只能靠端到端轮数间接推断。
func TestOrch质量评分三条终止条件(t *testing.T) {
	// 条件 1: 达标 (JSON 形态)
	tm := newOrchQualityTermination(7.0, 0.5)
	if !tm.ShouldTerminate(`{"pass":true,"overall":8.5}`) {
		t.Error("overall 8.5 且 pass 应终止")
	}
	// 条件 2: 收敛 (连续 3 轮变化 < delta)
	tm = newOrchQualityTermination(7.0, 0.5)
	for i, in := range []string{`{"pass":false,"overall":5.0}`, `{"pass":false,"overall":5.1}`} {
		if tm.ShouldTerminate(in) {
			t.Errorf("第 %d 轮不该终止 (样本不足)", i+1)
		}
	}
	if !tm.ShouldTerminate(`{"pass":false,"overall":5.2}`) {
		t.Error("连续 3 轮变化均 < 0.5 应判收敛终止")
	}
	// 条件 3: 退化 (连续 2 轮下降)
	tm = newOrchQualityTermination(7.0, 0.5)
	_ = tm.ShouldTerminate(`{"pass":false,"overall":6.5}`)
	_ = tm.ShouldTerminate(`{"pass":false,"overall":5.0}`)
	if !tm.ShouldTerminate(`{"pass":false,"overall":3.0}`) {
		t.Error("连续 2 轮下降应判退化终止")
	}
	// 关键词分支的判定顺序: 同时含"不通过"与其子串"通过"时, 后扫的通过词胜出。
	// 这不是笔误 —— 照抄旧实现, 改顺序会让这类产出从"一轮即止"变成"跑满 3 轮"。
	s := scoreOrchOutput("评审结论: 不通过")
	if !s.Pass || s.Overall != 8.0 {
		t.Errorf(`"不通过" 应因含子串"通过"被判 pass/8.0 (旧实现的判定顺序), 实际 pass=%v overall=%v`, s.Pass, s.Overall)
	}
	// 非 JSON 且无关键词 ⇒ 中性 6.0
	if s := scoreOrchOutput("一段普通文本"); s.Overall != 6.0 || !s.Pass {
		t.Errorf("无关键词应给中性 6.0/pass, 实际 %v/%v", s.Overall, s.Pass)
	}
}

// ---------------------------------------------------------------------------
// 五、构图边界
// ---------------------------------------------------------------------------

// TestOrchestratedGraphSpec构图边界 三件必须成立的事:
// 全 agent 节点 (不许被推断成 gate)、图层重试恒 0、节点总预算包住全部重试。
func TestOrchestratedGraphSpec构图边界(t *testing.T) {
	wf := testingWorkflow() // 它有个叫 quality-gate 的普通阶段
	spec, err := orchestratedGraphSpec(wf, 4)
	if err != nil {
		t.Fatalf("构图失败: %v", err)
	}
	if len(spec.Nodes) != len(wf.Stages) {
		t.Fatalf("节点数 %d != 阶段数 %d", len(spec.Nodes), len(wf.Stages))
	}
	if spec.Policies.MaxParallel != 4 {
		t.Errorf("图级并发 = %d, 期望 4", spec.Policies.MaxParallel)
	}
	if spec.Policies.DefaultRetry == nil || spec.Policies.DefaultRetry.MaxRetries != 0 {
		t.Error("图级默认重试必须为 0 (重试全在 runner 内按错误类型分档), 否则会两层相乘")
	}
	for i, n := range spec.Nodes {
		if n.ID != wf.Stages[i].Name {
			t.Errorf("第 %d 个节点 ID = %q, 期望阶段名 %q (声明序必须保持)", i+1, n.ID, wf.Stages[i].Name)
		}
		if n.Kind != "agent" {
			t.Errorf("节点 %q 的 Kind = %q, orchestrated 一律 agent (gate 化会把产出换成 JSON 评分)", n.ID, n.Kind)
		}
		if n.Retry == nil || n.Retry.MaxRetries != 0 {
			t.Errorf("节点 %q 的图层重试必须为 0", n.ID)
		}
		if n.Agent.Prompt != wf.Stages[i].Prompt {
			t.Errorf("节点 %q 的 Prompt 被改写了 (必须原样透传)", n.ID)
		}
		// 预算必须 ≥ 单次尝试超时 × 最大尝试次数, 否则合法的重试序列会撞 deadline。
		minBudget := int(taskTimeout(wf.Stages[i]).Seconds()) * orchDefaultRetryPolicy().orchMaxAttempts()
		if n.TimeoutSec < minBudget {
			t.Errorf("节点 %q 预算 %ds < 最坏重试耗时 %ds (合法重试会被掐死)", n.ID, n.TimeoutSec, minBudget)
		}
	}
	if _, err := orchestratedGraphSpec(&WorkflowDef{Name: "空"}, 4); err == nil {
		t.Error("零阶段工作流应报错, 而不是产出一张空图 (空图会被引擎当成跑完了)")
	}
}

// TestOrchestrated灰度开关两侧都走同一内核 orchestrated 已无条件在图引擎上跑, 但入口
// 必须是 executeOrchestrated —— 它带着 LLMClient 缺失时的降级与飞书通知。
//
// 具体要挡住的退化: 若有人把 "orchestrated" 加进 modeGraphTemplates, 那么
// CLAUDE_GO_GRAPH_ENGINE=1 时 workflow.go 的灰度分发口会直接跳到 executeGraph +
// stageNodeRunner —— 阶段从裸 completion 变成带工具的 ExecuteSingleStage, 且降级失效。
// 判据: 两侧的 LLM 调用次数都等于阶段数, 且 factory 一次都没被用到。
func TestOrchestrated灰度开关两侧都走同一内核(t *testing.T) {
	wf := parentingWorkflow()
	for _, flag := range []string{"", "1"} {
		t.Setenv("CLAUDE_GO_GRAPH_ENGINE", flag)
		llm := &orchStubLLM{out: stageOutputStub(wf)}
		rec := &tplRecorder{} // 若被 stageNodeRunner 接管, factory 会被调用
		we := newRecordingExecutor(rec)
		we.llm = llm
		res, err := we.Execute(context.Background(), wf, "目标", newStubTeam(t, "flag"+flag))
		if err != nil {
			t.Fatalf("flag=%q 执行失败: %v", flag, err)
		}
		calls, _, _ := llm.snapshot()
		if calls != len(wf.Stages) {
			t.Errorf("flag=%q 裸 completion 调用 %d 次, 期望 %d 次", flag, calls, len(wf.Stages))
		}
		if fc, _, _ := rec.snapshot(); fc != 0 {
			t.Errorf("flag=%q 被 stageNodeRunner 接管了 (factory 造的 agent 跑了 %d 次) —— 阶段悄悄拿到了工具权限", flag, fc)
		}
		if len(res) != len(wf.Stages) {
			t.Errorf("flag=%q 阶段数 = %d, 期望 %d", flag, len(res), len(wf.Stages))
		}
	}
	// 反向: 灰度分发的判据里不得出现 orchestrated。
	if ModeHasGraphTemplate("orchestrated") {
		t.Error("orchestrated 不该进 modeGraphTemplates (会被灰度分发口劫到 stageNodeRunner 上)")
	}
}

// TestOrchestrated无LLM降级pipeline LLMClient 缺失时必须降级 pipeline 而不是整体失败。
// 裸 completion 没有 LLM 无从下手, 但 pipeline 走 factory 造 agent, 不依赖 we.llm。
func TestOrchestrated无LLM降级pipeline(t *testing.T) {
	rec := &tplRecorder{}
	we := newRecordingExecutor(rec) // 有 factory, 无 llm
	wf := parentingWorkflow()
	res, err := we.executeOrchestrated(context.Background(), wf, "目标", newStubTeam(t, "no-llm"))
	if err != nil {
		t.Fatalf("降级路径失败: %v", err)
	}
	if len(res) != len(wf.Stages) {
		t.Errorf("降级后阶段数 = %d, 期望 %d", len(res), len(wf.Stages))
	}
	calls, _, _ := rec.snapshot()
	if calls != len(wf.Stages) {
		t.Errorf("降级应走 factory 造的 agent (调用 %d 次), 实际 %d 次", len(wf.Stages), calls)
	}
}
