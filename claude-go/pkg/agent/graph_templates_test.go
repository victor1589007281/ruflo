package agent

// graph_templates_test —— 15 种 mode → 图模板映射的等价性验收 (design/01 §五)。
//
// 等价性怎么证: 用同一个桩 agent 工厂各跑一遍**旧执行器**与**图模板路径**, 逐项比对
//
//	① 阶段序列 (名字 + 角色 + 状态, 按返回顺序 —— 这正是 REPORT.md / 门禁统计 /
//	   dashboard / 下游平台读到的东西);
//	② agent 调用次数 (多跑一次就是多烧一次 token);
//	③ 峰值并发 (桩里带真实 sleep, 让并行/串行在时间上真的可分辨);
//	④ 提示词里的依赖块顺序 (证明 {prev_result} 真被注入且与 pipeline 同序)。
//
// 只断言"能跑"是不够的 —— 那连"阶段被静默少跑了一半"都发现不了。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
)

// ---------------------------------------------------------------------------
// 测试装置: 可记录序列/并发/提示词的桩 agent
// ---------------------------------------------------------------------------

// tplRecorder 记录桩 agent 的调用: 次数、峰值并发、每次的 (角色, 提示词)。
type tplRecorder struct {
	delay time.Duration // 每次调用的模拟耗时: 让"并行"在时间上真的可观测

	mu       sync.Mutex
	calls    int
	inFlight int
	peak     int
	prompts  []tplCall
}

type tplCall struct {
	role   string
	prompt string
}

func (r *tplRecorder) enter(role, prompt string) {
	r.mu.Lock()
	r.calls++
	r.inFlight++
	if r.inFlight > r.peak {
		r.peak = r.inFlight
	}
	r.prompts = append(r.prompts, tplCall{role: role, prompt: prompt})
	r.mu.Unlock()
}

func (r *tplRecorder) leave() {
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
}

func (r *tplRecorder) snapshot() (calls, peak int, prompts []tplCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.peak, append([]tplCall(nil), r.prompts...)
}

// promptFor 返回某角色第 idx 次调用的提示词 (无则空串)。
func (r *tplRecorder) promptFor(role string, idx int) string {
	_, _, calls := r.snapshot()
	seen := 0
	for _, c := range calls {
		if c.role != role {
			continue
		}
		if seen == idx {
			return c.prompt
		}
		seen++
	}
	return ""
}

type tplStubRunner struct {
	role string
	rec  *tplRecorder
}

func (s *tplStubRunner) Execute(_ context.Context, userPrompt string) (string, error) {
	s.rec.enter(s.role, userPrompt)
	if s.rec.delay > 0 {
		time.Sleep(s.rec.delay)
	}
	defer s.rec.leave()
	// 产出须过 validateStageOutputForRetry: 结构化标记 + 足够长度。
	return fmt.Sprintf("[%s] done\n\n## 分析\n- 结论: 桩 agent 模拟产出\n"+
		"- 方案: 该文本仅用于满足阶段产出结构化校验, 含标记与足量正文以模拟真实 agent 的交付形态。", s.role), nil
}

func newRecordingExecutor(rec *tplRecorder) *WorkflowExecutor {
	return &WorkflowExecutor{
		factory: func(_ context.Context, role, _ string) (AgentRunner, error) {
			return &tplStubRunner{role: role, rec: rec}, nil
		},
		notify: func(_, _ string) {},
	}
}

// tplStageSeq 把阶段结果压成可直接比对的序列 (名字/角色/状态)。
func tplStageSeq(rs []StageResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%s|%s|%s", r.Name, r.Role, r.Status))
	}
	return out
}

// assertSameSeq 逐项比对两条阶段序列, 差异处直接打出上下文。
func assertSameSeq(t *testing.T, what string, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s: 阶段数不等价 旧=%d 新=%d\n旧: %v\n新: %v", what, len(want), len(got), want, got)
		return
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s: 第 %d 个阶段不等价 旧=%q 新=%q\n旧全序列: %v\n新全序列: %v",
				what, i+1, want[i], got[i], want, got)
		}
	}
}

// tplAdversarialWorkflow 一个 adversarial 模式的工作流 (无内置工作流用此 mode)。
// 刻意设 Rounds=3: 用来证明两条路径都**不读** Rounds (阶段数恒等于声明的阶段数)。
func tplAdversarialWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:   "tpl-adversarial",
		Mode:   "adversarial",
		Rounds: 3,
		Stages: []StageDef{
			{Name: "propose", Role: "proposer", Prompt: "提出方案: {objective}"},
			{Name: "oppose", Role: "opponent", Prompt: "反驳: {prev_result}", DependsOn: []string{"propose"}},
			{Name: "judge", Role: "critic", Prompt: "裁决: {prev_result}", DependsOn: []string{"propose", "oppose"}},
		},
	}
}

// tplPipelineWorkflow 借 research 的形状 (1 → 3 并行 → 1 → 1) 做 pipeline 模板验收:
// 它同时覆盖"单节点波次"与"三路并行波次", 比线性链能证的东西多。
func tplPipelineWorkflow() *WorkflowDef {
	wf := researchWorkflow()
	wf.Name = "tpl-pipeline"
	wf.Mode = "pipeline"
	return wf
}

// ---------------------------------------------------------------------------
// 一、模板库自身的完备性
// ---------------------------------------------------------------------------

// tplAllModes design/01 §1.3 列出的 15 种 mode (与 dedicatedExecutorModes + pipeline/fanout 对齐)。
var tplAllModes = []string{
	"pipeline", "fanout", "adversarial", "adversarial_dev", "trading_debate",
	"creative_media", "novel_writing", "swarm_novel", "plot_simulate", "plot_predict",
	"ensemble_extract", "review_panel", "orchestrated", "app_composite", "game_composite",
}

// TestModeGraph模板表覆盖全部15种mode 15 个 mode 每个都必须**恰好**表态一次:
// 要么有等价模板 (灰度可切), 要么有专属节点内核 (已无条件在图引擎上跑),
// 要么在"已核实不可等价图化"表里写明缺什么能力。
//
// 这条守护的是**沉默**: 将来有人加了第 16 个 mode 或改了某个执行器却不更新三张表,
// 结果会是该 mode 悄悄既没有模板也没有记账 —— 报告上看不出来, 灰度时才发现。
func TestModeGraph模板表覆盖全部15种mode(t *testing.T) {
	tables := map[string]map[string]string{
		"模板表":   {},
		"专属内核表": modeGraphNativeKernel,
		"未做表":   modeGraphNotTemplated,
	}
	for m, tpl := range modeGraphTemplates {
		tables["模板表"][m] = tpl.Equivalence
	}
	total := len(tables["模板表"]) + len(tables["专属内核表"]) + len(tables["未做表"])
	if total != len(tplAllModes) {
		t.Errorf("模板表(%d) + 专属内核表(%d) + 未做表(%d) = %d != 15; 若有意增删 mode 请同步更新本测试与 design/01 §五",
			len(tables["模板表"]), len(tables["专属内核表"]), len(tables["未做表"]), total)
	}
	for _, mode := range tplAllModes {
		var hit []string
		for name, tbl := range tables {
			doc, ok := tbl[mode]
			if !ok {
				continue
			}
			hit = append(hit, name)
			if strings.TrimSpace(doc) == "" {
				t.Errorf("mode %q 在%s里的说明为空", mode, name)
			}
		}
		switch len(hit) {
		case 1: // 正常
		case 0:
			t.Errorf("mode %q 三张表都没记账 (design/01 §五 要求逐个交代)", mode)
		default:
			sort.Strings(hit)
			t.Errorf("mode %q 同时出现在 %v, 语义矛盾", mode, hit)
		}
	}
	// 反向: 三张表里不得出现 15 个之外的 mode (打错字会让守护形同虚设)。
	known := map[string]bool{}
	for _, m := range tplAllModes {
		known[m] = true
	}
	for name, tbl := range tables {
		for m := range tbl {
			if !known[m] {
				t.Errorf("%s里的 %q 不在 15 种 mode 之列", name, m)
			}
		}
	}
	// 专属内核 mode 必须**不在**灰度分发判据里: ModeHasGraphTemplate 命中会让
	// workflow.go:408 把它劫到 stageNodeRunner 路径上 (=悄悄换内核 + 绕过降级)。
	for m := range modeGraphNativeKernel {
		if ModeHasGraphTemplate(m) {
			t.Errorf("专属内核 mode %q 不该被 ModeHasGraphTemplate 命中 (会被灰度分发口劫走)", m)
		}
	}
}

// TestModeGraph模板只用已实现的节点形态 模板产出的图必须过 Validate, 且只含
// agent|gate|map|reduce|loop-group —— router/subgraph/human 尚未实现, Validate 会报错,
// 但这里再显式断言一次: 靠"Validate 恰好会拦"是脆的, 将来放开某个 Kind 时这条会提醒。
func TestModeGraph模板只用已实现的节点形态(t *testing.T) {
	allowed := map[graph.NodeKind]bool{
		graph.NodeKindAgent: true, graph.NodeKindGate: true, graph.NodeKindMap: true,
		graph.NodeKindReduce: true, graph.NodeKindLoopGroup: true,
	}
	cases := map[string]*WorkflowDef{
		"pipeline":       tplPipelineWorkflow(),
		"fanout":         researchWorkflow(),
		"adversarial":    tplAdversarialWorkflow(),
		"trading_debate": tradingV2Workflow(),
	}
	for mode := range modeGraphTemplates {
		wf, ok := cases[mode]
		if !ok {
			t.Fatalf("mode %q 有模板但本测试没给代表性工作流, 补一个 (不给就等于没验证)", mode)
		}
		spec, err := BuildModeGraph(wf)
		if err != nil {
			t.Errorf("mode %q 模板构图失败: %v", mode, err)
			continue
		}
		if err := spec.Validate(); err != nil {
			t.Errorf("mode %q 模板产出的图非法: %v", mode, err)
		}
		var walk func(ns []graph.NodeSpec)
		walk = func(ns []graph.NodeSpec) {
			for _, n := range ns {
				if !allowed[n.Kind] {
					t.Errorf("mode %q 的节点 %q 用了未实现的 Kind %q", mode, n.ID, n.Kind)
				}
				if n.Group != nil {
					walk(n.Group.Nodes)
				}
			}
		}
		walk(spec.Nodes)
		if spec.Version != "template-"+mode {
			t.Errorf("mode %q 图版本标记应为 template-%s, got %q", mode, mode, spec.Version)
		}
	}
}

// ---------------------------------------------------------------------------
// 二、灰度: 默认必须一字不变
// ---------------------------------------------------------------------------

// TestModeGraph灰度默认关 开关未设时 Execute 必须落旧执行器 (以"没有 graph-journal
// 目录"为硬证据: 图路径一定会开 journal), 打开后才走图模板。
//
// 这条是本次改动的安全底线: :18080 上有 8+ 个下游平台在跑, 默认路径变了就是生产事故。
func TestModeGraph灰度默认关(t *testing.T) {
	journalDir := func(team *ProductionTeam) string {
		return filepath.Join(team.dataDir, "graph-journal")
	}

	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "") // 显式清空: 不受外部环境影响
	wf := tradingV2Workflow()
	offTeam := newStubTeam(t, "gray-off")
	offRec := &tplRecorder{}
	offRes, err := newRecordingExecutor(offRec).Execute(context.Background(), wf, "标的: 测试标的", offTeam)
	if err != nil {
		t.Fatalf("默认路径执行失败: %v", err)
	}
	if _, err := os.Stat(journalDir(offTeam)); !os.IsNotExist(err) {
		t.Fatalf("开关未设时不该产生 graph-journal (说明默认行为已被改成图路径): stat err=%v", err)
	}

	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")
	onTeam := newStubTeam(t, "gray-on")
	onRec := &tplRecorder{}
	onRes, err := newRecordingExecutor(onRec).Execute(context.Background(), wf, "标的: 测试标的", onTeam)
	if err != nil {
		t.Fatalf("灰度路径执行失败: %v", err)
	}
	if _, err := os.Stat(journalDir(onTeam)); err != nil {
		t.Fatalf("开关打开后应经图引擎跑 (graph-journal 应存在): %v", err)
	}
	assertSameSeq(t, "trading_debate 经 Execute 分发", tplStageSeq(offRes), tplStageSeq(onRes))

	offCalls, _, _ := offRec.snapshot()
	onCalls, _, _ := onRec.snapshot()
	if offCalls != onCalls {
		t.Errorf("agent 调用次数不等价: 默认=%d 灰度=%d", offCalls, onCalls)
	}
}

// TestModeGraph未做的mode灰度也不改路 未登记模板的 mode 即使开关打开也必须走自己的
// 执行器 —— 否则"没做"就变成了"悄悄换了实现"。
func TestModeGraph未做的mode灰度也不改路(t *testing.T) {
	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")
	// plot_simulate 未登记模板, 且其执行器在 we.llm==nil 时会明确报错 —— 拿这个错误
	// 当"确实进了旧执行器"的证据 (若被图路径接管, 报错会来自图引擎/直译器)。
	wf := &WorkflowDef{Name: "plot-simulate", Mode: "plot_simulate"}
	team := newStubTeam(t, "not-templated")
	_, err := newRecordingExecutor(&tplRecorder{}).Execute(context.Background(), wf, "目标", team)
	if err == nil {
		t.Fatal("plot_simulate 无 LLMClient 时应报错")
	}
	if !strings.Contains(err.Error(), "LLMClient") {
		t.Errorf("错误应来自 plot_simulate 执行器 (含 LLMClient 字样), got %q", err)
	}
	if _, statErr := os.Stat(filepath.Join(team.dataDir, "graph-journal")); !os.IsNotExist(statErr) {
		t.Error("未登记模板的 mode 不该被图引擎接管 (却产生了 graph-journal)")
	}
}

// ---------------------------------------------------------------------------
// 三、逐 mode 等价性
// ---------------------------------------------------------------------------

// TestPipeline图模板阶段序列等价 pipeline: 1 → 3 并行 → 1 → 1 的形状,
// 旧 executePipeline 与图模板必须给出同一条阶段序列。
func TestPipeline图模板阶段序列等价(t *testing.T) {
	wf := tplPipelineWorkflow()
	oldRec, newRec := &tplRecorder{}, &tplRecorder{}
	oldRes, err := newRecordingExecutor(oldRec).executePipeline(context.Background(), wf, "调研目标", newStubTeam(t, "pipe-old"))
	if err != nil {
		t.Fatalf("旧 pipeline 失败: %v", err)
	}
	newRes, err := newRecordingExecutor(newRec).executeGraph(context.Background(), wf, "调研目标", newStubTeam(t, "pipe-new"))
	if err != nil {
		t.Fatalf("图模板失败: %v", err)
	}
	assertSameSeq(t, "pipeline", tplStageSeq(oldRes), tplStageSeq(newRes))
	oc, _, _ := oldRec.snapshot()
	nc, _, _ := newRec.snapshot()
	if oc != nc || oc != len(wf.Stages) {
		t.Errorf("agent 调用次数: 旧=%d 新=%d, 期望均为阶段数 %d", oc, nc, len(wf.Stages))
	}
}

// TestFanOut图模板阶段序列等价 fanout: 现状执行器是转调 pipeline 的空壳,
// 模板必须复现这个事实 (而不是按 design/01 §五 改成 map→reduce —— 那会换掉阶段名)。
func TestFanOut图模板阶段序列等价(t *testing.T) {
	wf := researchWorkflow()
	if wf.Mode != "fanout" {
		t.Fatalf("测试前提失效: research 工作流的 mode 已不是 fanout, 而是 %q", wf.Mode)
	}
	oldRec, newRec := &tplRecorder{}, &tplRecorder{}
	oldRes, err := newRecordingExecutor(oldRec).executeFanOut(context.Background(), wf, "调研目标", newStubTeam(t, "fan-old"))
	if err != nil {
		t.Fatalf("旧 executeFanOut 失败: %v", err)
	}
	newRes, err := newRecordingExecutor(newRec).executeGraph(context.Background(), wf, "调研目标", newStubTeam(t, "fan-new"))
	if err != nil {
		t.Fatalf("图模板失败: %v", err)
	}
	assertSameSeq(t, "fanout", tplStageSeq(oldRes), tplStageSeq(newRes))

	// 顺带钉住"模板没有偷偷 map 化": 图里不得出现 map/reduce 节点, 节点数 = 阶段数。
	spec, err := BuildModeGraph(wf)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Nodes) != len(wf.Stages) {
		t.Errorf("fanout 模板节点数 %d != 阶段数 %d (疑似被展开成 map/reduce)", len(spec.Nodes), len(wf.Stages))
	}
	for _, n := range spec.Nodes {
		if n.Kind == graph.NodeKindMap || n.Kind == graph.NodeKindReduce {
			t.Errorf("fanout 模板出现 %s 节点 %q: 与现状执行器不等价", n.Kind, n.ID)
		}
	}
}

// TestAdversarial图模板阶段序列等价 adversarial: 执行器同样是 pipeline 空壳,
// 且 wf.Rounds **在这条路径上从未被读取** —— 模板不得引入 loop-group 轮次。
func TestAdversarial图模板阶段序列等价(t *testing.T) {
	wf := tplAdversarialWorkflow()
	oldRec, newRec := &tplRecorder{}, &tplRecorder{}
	oldRes, err := newRecordingExecutor(oldRec).executeAdversarial(context.Background(), wf, "辩题", newStubTeam(t, "adv-old"))
	if err != nil {
		t.Fatalf("旧 executeAdversarial 失败: %v", err)
	}
	newRes, err := newRecordingExecutor(newRec).executeGraph(context.Background(), wf, "辩题", newStubTeam(t, "adv-new"))
	if err != nil {
		t.Fatalf("图模板失败: %v", err)
	}
	assertSameSeq(t, "adversarial", tplStageSeq(oldRes), tplStageSeq(newRes))

	spec, err := BuildModeGraph(wf)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Nodes) != 3 {
		t.Errorf("Rounds=3 不该让节点数变化 (执行器不读它), 期望 3 个节点 got %d", len(spec.Nodes))
	}
	for _, n := range spec.Nodes {
		if n.Kind == graph.NodeKindLoopGroup || n.Loop != nil {
			t.Errorf("adversarial 模板不得引入循环 (节点 %q kind=%s loop=%v)", n.ID, n.Kind, n.Loop != nil)
		}
	}
}

// tradingExpectedSeq trading_debate 的 18 个阶段 (执行器真实产出的顺序)。
// 写死在测试里是刻意的: 它是一份**独立**的期望, 若模板与执行器同时被改坏,
// 只比"两边相等"是发现不了的。
var tradingExpectedSeq = []string{
	"tech-analysis|market-analyst|completed",
	"sentiment-analysis|sentiment-analyst|completed",
	"fundamentals-analysis|fundamentals-analyst|completed",
	"news-analysis|news-analyst|completed",
	"bull-round1|bull-researcher|completed",
	"bear-round1|bear-researcher|completed",
	"bull-round2|bull-researcher|completed",
	"bear-round2|bear-researcher|completed",
	"research-manager|research-manager|completed",
	"trader|trader|completed",
	"aggressive-risk-round1|aggressive-risk|completed",
	"conservative-risk-round1|conservative-risk|completed",
	"neutral-risk-round1|neutral-risk|completed",
	"aggressive-risk-round2|aggressive-risk|completed",
	"conservative-risk-round2|conservative-risk|completed",
	"neutral-risk-round2|neutral-risk|completed",
	"portfolio-manager|portfolio-manager|completed",
	"signal-extractor|portfolio-manager|completed",
}

// TestTradingDebate图模板阶段序列等价 trading_debate 是 13 个专用执行器里唯一
// 阶段序列全静态的 —— 18 个阶段逐个展开为节点, 名字/角色/顺序必须逐项相同。
func TestTradingDebate图模板阶段序列等价(t *testing.T) {
	wf := tradingV2Workflow()
	oldRec, newRec := &tplRecorder{}, &tplRecorder{}
	oldRes, err := newRecordingExecutor(oldRec).executeTradingDebate(context.Background(), wf, "标的: 测试标的", newStubTeam(t, "td-old"))
	if err != nil {
		t.Fatalf("旧 executeTradingDebate 失败: %v", err)
	}
	newRes, err := newRecordingExecutor(newRec).executeGraph(context.Background(), wf, "标的: 测试标的", newStubTeam(t, "td-new"))
	if err != nil {
		t.Fatalf("图模板失败: %v", err)
	}

	assertSameSeq(t, "trading_debate 旧执行器 vs 独立期望", tradingExpectedSeq, tplStageSeq(oldRes))
	assertSameSeq(t, "trading_debate 图模板 vs 独立期望", tradingExpectedSeq, tplStageSeq(newRes))
	assertSameSeq(t, "trading_debate 旧 vs 新", tplStageSeq(oldRes), tplStageSeq(newRes))

	oc, _, _ := oldRec.snapshot()
	nc, _, _ := newRec.snapshot()
	if oc != nc || oc != len(tradingExpectedSeq) {
		t.Errorf("agent 调用次数: 旧=%d 新=%d, 期望均为 %d", oc, nc, len(tradingExpectedSeq))
	}
}

// TestTradingDebate图模板并发度等价 并发度不能只看图结构 —— 用带真实 sleep 的桩把
// "4 路分析并行、其余严格串行"变成时间上可测的事实, 两条路径的峰值必须相同。
func TestTradingDebate图模板并发度等价(t *testing.T) {
	const delay = 30 * time.Millisecond
	wf := tradingV2Workflow()
	oldRec := &tplRecorder{delay: delay}
	newRec := &tplRecorder{delay: delay}
	if _, err := newRecordingExecutor(oldRec).executeTradingDebate(context.Background(), wf, "标的: 测试标的", newStubTeam(t, "td-par-old")); err != nil {
		t.Fatalf("旧执行器失败: %v", err)
	}
	if _, err := newRecordingExecutor(newRec).executeGraph(context.Background(), wf, "标的: 测试标的", newStubTeam(t, "td-par-new")); err != nil {
		t.Fatalf("图模板失败: %v", err)
	}
	_, oldPeak, _ := oldRec.snapshot()
	_, newPeak, _ := newRec.snapshot()
	if oldPeak != 4 {
		t.Errorf("旧执行器峰值并发应为 4 (Phase 1 四路分析), got %d", oldPeak)
	}
	if newPeak != oldPeak {
		t.Errorf("峰值并发不等价: 旧=%d 新=%d (串行的辩论链被并行化 / 并行的分析被串行化都会命中这条)", oldPeak, newPeak)
	}
}

// TestTradingDebate图模板辩论链严格串行 结构层面再钉一次: 辩论/风险两条链上的每个
// 阶段都必须依赖它前面**全部**同链阶段 —— 少一条依赖就既丢了顺序也丢了辩论历史。
func TestTradingDebate图模板辩论链严格串行(t *testing.T) {
	spec, err := BuildModeGraph(tradingV2Workflow())
	if err != nil {
		t.Fatal(err)
	}
	deps := graphNodeDeps(spec)
	has := func(node, dep string) bool {
		for _, d := range deps[node] {
			if d == dep {
				return true
			}
		}
		return false
	}
	// 入度 0 的必须恰好是 4 路分析 (它们才是唯一可并行的一层)。
	var roots []string
	for _, n := range spec.Nodes {
		if len(deps[n.ID]) == 0 {
			roots = append(roots, n.ID)
		}
	}
	wantRoots := []string{"tech-analysis", "sentiment-analysis", "fundamentals-analysis", "news-analysis"}
	assertSameSeq(t, "入度 0 的入口节点", wantRoots, roots)

	// 辩论链累积依赖
	chain := []string{"bull-round1", "bear-round1", "bull-round2", "bear-round2", "research-manager"}
	for i, node := range chain {
		for j := 0; j < i; j++ {
			if !has(node, chain[j]) {
				t.Errorf("%q 缺少对同链前序 %q 的依赖: 顺序与辩论历史都会丢", node, chain[j])
			}
		}
		for _, a := range wantRoots {
			if !has(node, a) {
				t.Errorf("%q 缺少对分析阶段 %q 的依赖 (analysisReport 拿不到)", node, a)
			}
		}
	}
	// 风险链累积依赖 + 全部挂在 trader 之后
	risk := []string{
		"aggressive-risk-round1", "conservative-risk-round1", "neutral-risk-round1",
		"aggressive-risk-round2", "conservative-risk-round2", "neutral-risk-round2",
		"portfolio-manager",
	}
	for i, node := range risk {
		if !has(node, "trader") {
			t.Errorf("%q 应依赖 trader", node)
		}
		for j := 0; j < i; j++ {
			if !has(node, risk[j]) {
				t.Errorf("%q 缺少对同链前序 %q 的依赖", node, risk[j])
			}
		}
	}
	if !has("signal-extractor", "portfolio-manager") {
		t.Error("signal-extractor 应依赖 portfolio-manager")
	}
	if !has("trader", "research-manager") {
		t.Error("trader 应依赖 research-manager")
	}
}

// ---------------------------------------------------------------------------
// 四、等价性前提: 依赖块真被注入且与 pipeline 同序
// ---------------------------------------------------------------------------

// TestGraph依赖块按声明序注入 图路径构造 StageDef 时若不填 DependsOn,
// buildStagePromptWithRoles 拼出来的依赖块恒空 ⇒ {prev_result} 被替换成空串,
// 每个阶段都在没有上游交接的情况下干活。这条同时钉住两件事:
//
//	① 上游产出真的进了提示词 (否则任何模板的"等价"都不成立);
//	② 顺序 = DependsOn 声明序, 与 pipeline 逐字相同 (字典序会让 research 的
//	   cross-verification 拿到 market/risk/tech, 而 pipeline 给的是 tech/market/risk)。
func TestGraph依赖块按声明序注入(t *testing.T) {
	wf := tplPipelineWorkflow()
	oldRec, newRec := &tplRecorder{}, &tplRecorder{}
	if _, err := newRecordingExecutor(oldRec).executePipeline(context.Background(), wf, "调研目标", newStubTeam(t, "dep-old")); err != nil {
		t.Fatalf("旧 pipeline 失败: %v", err)
	}
	if _, err := newRecordingExecutor(newRec).executeGraph(context.Background(), wf, "调研目标", newStubTeam(t, "dep-new")); err != nil {
		t.Fatalf("图模板失败: %v", err)
	}

	// cross-verification 的角色 fact-checker 在此工作流里唯一, 且它声明了三个依赖。
	const role = "fact-checker"
	oldPrompt := oldRec.promptFor(role, 0)
	newPrompt := newRec.promptFor(role, 0)
	if oldPrompt == "" || newPrompt == "" {
		t.Fatalf("未捕获到 %s 的提示词 (旧空=%v 新空=%v)", role, oldPrompt == "", newPrompt == "")
	}
	order := func(p string) []int {
		return []int{
			strings.Index(p, "Dependency output from research-tech"),
			strings.Index(p, "Dependency output from research-market"),
			strings.Index(p, "Dependency output from research-risk"),
		}
	}
	for _, tc := range []struct {
		what string
		p    string
	}{{"pipeline", oldPrompt}, {"graph", newPrompt}} {
		idx := order(tc.p)
		for i, at := range idx {
			if at < 0 {
				t.Fatalf("%s 路径: 提示词缺少第 %d 个依赖块, {prev_result} 未被注入", tc.what, i+1)
			}
		}
		if !(idx[0] < idx[1] && idx[1] < idx[2]) {
			t.Errorf("%s 路径: 依赖块顺序不是声明序 tech<market<risk, 实际位置 %v", tc.what, idx)
		}
	}
}

// TestOrderedPrevDeps 依赖顺序解析的单元边界: 声明序优先、过滤无产出的上游、
// 派生 ID (map 分片 / 组内成员 / 展开产物) 能归一回声明期节点、查不到时退化为字典序。
func TestOrderedPrevDeps(t *testing.T) {
	deps := map[string][]string{
		"c":      {"a", "b"},
		"member": {"x", "y"},
	}
	prev := map[string]string{"a": "A", "b": "B", "x": "X", "y": "Y"}

	if got := orderedPrevDeps(deps, "c", prev); !strings.EqualFold(strings.Join(got, ","), "a,b") {
		t.Errorf("声明序应为 a,b, got %v", got)
	}
	// 上游没有产出 (failed/skipped) 时不进依赖列表 —— 与 pipeline 的 prevResults 口径一致
	if got := orderedPrevDeps(deps, "c", map[string]string{"b": "B"}); len(got) != 1 || got[0] != "b" {
		t.Errorf("只应保留有产出的上游, got %v", got)
	}
	// 派生 ID 归一: 组内成员 <组>#it2/member、map 分片 member#3、展开产物 parent/member
	for _, id := range []string{"grp#it2/member", "member#3", "parent/member"} {
		got := orderedPrevDeps(deps, id, prev)
		if len(got) != 2 || got[0] != "x" || got[1] != "y" {
			t.Errorf("派生 ID %q 未归一到声明期节点 member: got %v", id, got)
		}
	}
	// 查不到声明序 → 字典序 (退化也必须确定性, 否则提示词会随 map 遍历序抖动)
	got := orderedPrevDeps(deps, "unknown", map[string]string{"z": "Z", "a": "A", "m": "M"})
	if strings.Join(got, ",") != "a,m,z" {
		t.Errorf("未知节点应退化为字典序 a,m,z, got %v", got)
	}
	if got := orderedPrevDeps(deps, "c", nil); got != nil {
		t.Errorf("无上游产出时应返回 nil, got %v", got)
	}
}

// TestGraphBaseNodeID 派生 ID 归一的顺序敏感性: 组内 ID 的 "#it<轮次>" 在 "/" 左边,
// 先截 "#" 会把整个成员名丢掉 —— 这条锁住归一顺序。
func TestGraphBaseNodeID(t *testing.T) {
	cases := map[string]string{
		"plain":                 "plain",
		"mapnode#0":             "mapnode",
		"mapnode#12":            "mapnode",
		"loop-gen#it3/critique": "critique",
		"planner/subtask-1":     "subtask-1",
	}
	for in, want := range cases {
		if got := graphBaseNodeID(in); got != want {
			t.Errorf("graphBaseNodeID(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestModeGraphTemplateEquivalence文案 每个 mode 都能查到"等价范围"或"为什么没做",
// 供 CLI/dashboard 在灰度前直接呈现边界, 而不是让人去翻源码注释。
func TestModeGraphTemplateEquivalence文案(t *testing.T) {
	for _, mode := range tplAllModes {
		doc, templated := ModeGraphTemplateEquivalence(mode)
		if strings.TrimSpace(doc) == "" {
			t.Errorf("mode %q 查不到等价性说明", mode)
		}
		_, native := modeGraphNativeKernel[mode]
		if want := ModeHasGraphTemplate(mode) || native; templated != want {
			t.Errorf("mode %q 的已验证标记 = %v, 期望 %v (模板表或专属内核表命中即为已验证)", mode, templated, want)
		}
	}
	if doc, ok := ModeGraphTemplateEquivalence("不存在的mode"); ok || doc != "" {
		t.Errorf("未知 mode 应返回 (\"\", false), got (%q, %v)", doc, ok)
	}
}

// TestBuildModeGraph拒绝无模板与空阶段 模板入口的失败路径必须显式报错,
// 而不是产出一张空图 (空图会被引擎当成"跑完了", 表现为团队秒完成且零产出)。
func TestBuildModeGraph拒绝无模板与空阶段(t *testing.T) {
	if _, err := BuildModeGraph(nil); err == nil {
		t.Error("nil 工作流应报错")
	}
	if _, err := BuildModeGraph(&WorkflowDef{Name: "x", Mode: "plot_simulate"}); err == nil {
		t.Error("无模板的 mode 应报错")
	}
	if _, err := BuildModeGraph(&WorkflowDef{Name: "x", Mode: "pipeline"}); err == nil {
		t.Error("零阶段的 pipeline 应报错")
	}
	// trading_debate 模板要求至少 4 个分析阶段 (执行器按下标取 wf.Stages[0..3])
	short := &WorkflowDef{Name: "short", Mode: "trading_debate", Stages: []StageDef{{Name: "a", Role: "r"}}}
	if _, err := BuildModeGraph(short); err == nil {
		t.Error("trading_debate 阶段不足 4 个时应报错, 而不是越界 panic")
	}
}
