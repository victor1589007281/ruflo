package agent

// run_interceptors_equiv_test —— executeWorkflow 拆成"主体 + 运行级拦截器链"的等价性验收
// (design/01 §4.10)。
//
// ---------------------------------------------------------------------------
// 金标准是怎么来的 (这决定了这些字面量值不值得信)
// ---------------------------------------------------------------------------
//
// 下面每一个期望值都是**在 executeWorkflow 还是 282 行巨函数的时候, 用同一个桩 agent
// 工厂真跑出来的**, 不是照着源码推断的。做法: 先只写本文件 (不动 teams.go), 跑
// TestRunChain四维金标准 让它把观测值打出来, 把打出来的值固化成字面量; 再做拆分;
// 再复跑。拆分前后字面量一个都没改 —— 这就是等价性证明。
//
// 四个维度 (照 orchestrated_equiv_test.go / graph_templates_test.go 的做法):
//
//	① 阶段序列 (名字 + 角色 + 状态) —— REPORT.md / 门禁统计 / dashboard / 下游平台读的就是它
//	② agent 调用次数 —— 多跑一次就是多烧一次 token
//	③ 峰值并发 —— 桩里带真实 sleep, 让并行/串行在时间上真的可分辨
//	④ 提示词逐字 —— 拆分若碰掉 WorkflowExecutor 的任一注入字段 (roles/planCfg/evolution),
//	   提示词立刻变形, 而模型产出随之变形
//
// 外加**运行级副作用**五项 —— 它们才是本次真正搬动的东西, 前四维只是"没被顺手弄坏"的护栏:
//
//	⑤ 通知文案逐条 (Notifier 拦截器搬的就是它, 飞书用户直接看到)
//	⑥ 团队终态 + team.Stages 落盘内容 (GateEnforcer 的裁决结果)
//	⑦ 报告文件与高权重记忆写入 (Memory 拦截器)
//	⑧ 指标条目 (MetricsEmitter)
//	⑨ 阶段级黑板键 (下游 harvest 依赖)
//
// 变异反证 (证明这些断言不是许愿) 见 TestRunChain变异反证说明 的注释。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试装置
// ---------------------------------------------------------------------------

// riCall 一次 agent 执行的 (角色, 完整提示词)。
type riCall struct{ role, prompt string }

// riRec 记录调用序 / 峰值并发 / 逐条提示词的桩记录器。
//
// 桩产出**只依赖角色名**是刻意的: 下游阶段的 {prev_result} 因此完全确定, 提示词才
// 可以逐字比对。若产出带调用序号, 并发抖动会让下游提示词每次都不同, 第 ④ 维就
// 退化成"能跑"。
type riRec struct {
	mu       sync.Mutex
	calls    []riCall
	inFlight int
	peak     int
	delay    time.Duration
}

type riRunner struct {
	role string
	rec  *riRec
}

func (r *riRunner) Execute(_ context.Context, prompt string) (string, error) {
	rec := r.rec
	rec.mu.Lock()
	rec.calls = append(rec.calls, riCall{role: r.role, prompt: prompt})
	rec.inFlight++
	if rec.inFlight > rec.peak {
		rec.peak = rec.inFlight
	}
	d := rec.delay
	rec.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	rec.mu.Lock()
	rec.inFlight--
	rec.mu.Unlock()
	// 产出必须够长: 太短会被 saveTeamReport / 记忆摘要之外的下游当成空产出。
	return "OUT[" + r.role + "]" + strings.Repeat("内容充实的一段产出。", 12), nil
}

func (r *riRec) snapshot() (calls int, peak int, roles []string, prompts []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		roles = append(roles, c.role)
		prompts = append(prompts, c.prompt)
	}
	return len(r.calls), r.peak, roles, prompts
}

// riMemWriter 记录高权重记忆写入 (⑦ 维)。
type riMemWriter struct {
	mu   sync.Mutex
	rows []string
}

func (m *riMemWriter) AddTeamMemory(teamName, workflow, objective, summary string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, fmt.Sprintf("%s|%s|%s|len=%d", teamName, workflow, objective, len(summary)))
}

func (m *riMemWriter) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.rows...)
}

// riNotifySink 记录通知文案 (⑤ 维)。
type riNotifySink struct {
	mu   sync.Mutex
	msgs []string
}

func (n *riNotifySink) add(msg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.msgs = append(n.msgs, msg)
}

func (n *riNotifySink) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.msgs...)
}

// riVolatile 把通知文案里的易变量 (耗时/时间戳/临时目录) 归一, 否则金标准每次都不同。
// 归一的都是"与本次拆分无关的量"; 文案结构与措辞一个字都不归一。
var riVolatile = []*regexp.Regexp{
	regexp.MustCompile(`耗时 [0-9hmsµn.]+`),
	regexp.MustCompile(`/tmp/[^\s\x60]+`),
	regexp.MustCompile(`/var/folders/[^\s\x60]+`),
}

func riNormalize(msgs []string) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		for _, re := range riVolatile {
			m = re.ReplaceAllString(m, "<X>")
		}
		out = append(out, m)
	}
	return out
}

// riWorkflowName 等价性专用动态工作流: 提示词刻意短小, 金标准才能读得懂。
const riWorkflowName = "ri-equiv-flow"

// riWorkflow s0 → (s1 ∥ s2) → s3。
// 角色名刻意避开代码类关键词 (isCodeAnalysisRole 会额外注入 codeIntelDirective) 与
// 阶段名 plan/design (会触发目标分解器的产出校验), 让提示词与调用次数完全确定。
func riWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        riWorkflowName,
		Description: "运行级拦截器等价性夹具",
		Mode:        "pipeline",
		Stages: []StageDef{
			{Name: "s0", Role: "ri-lead", Prompt: "S0 目标: {objective}"},
			{Name: "s1", Role: "ri-one", Prompt: "S1 依据: {prev_result}", DependsOn: []string{"s0"}, Parallel: true},
			{Name: "s2", Role: "ri-two", Prompt: "S2 依据: {prev_result}", DependsOn: []string{"s0"}, Parallel: true},
			{Name: "s3", Role: "ri-sum", Prompt: "S3 汇总: {prev_result}", DependsOn: []string{"s1", "s2"}},
		},
	}
}

// riHarness 一次完整 executeWorkflow 运行的观测面。
type riHarness struct {
	ptm    *ProductionTeamManager
	team   *ProductionTeam
	rec    *riRec
	notify *riNotifySink
	mem    *riMemWriter
	tmp    string
}

func newRIHarness(t *testing.T, delay time.Duration) *riHarness {
	t.Helper()
	if err := RegisterWorkflow(riWorkflow(), nil); err != nil {
		t.Fatalf("注册夹具工作流: %v", err)
	}
	t.Cleanup(func() { _ = UnregisterWorkflow(riWorkflowName) })

	tmp := t.TempDir()
	rec := &riRec{delay: delay}
	sink := &riNotifySink{}
	mem := &riMemWriter{}
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(tmp, "state", "teams"),
		Factory: func(_ context.Context, role, _ string) (AgentRunner, error) { return &riRunner{role: role, rec: rec}, nil },
		Notify:  func(_, msg string) { sink.add(msg) },
	})
	// 记忆写入器只能经 setter 注入: TeamManagerConfig.MemWriter 是**死字段** ——
	// NewProductionTeamManager 从不读它 (生产也走 SetMemoryWriter, 见 feishu/bot.go)。
	// 这里照生产的接法来, 否则 ⑦ 维测的是一条永不通电的路。
	ptm.SetMemoryWriter(mem)
	team, err := ptm.CreateTeam("ri-team", riWorkflowName, "把 X 做成 Y", "chat-1")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	return &riHarness{ptm: ptm, team: team, rec: rec, notify: sink, mem: mem, tmp: tmp}
}

// run 同步跑完一次 executeWorkflow (不经 RunTeam 的 goroutine, 断言才好写)。
func (h *riHarness) run(t *testing.T) {
	t.Helper()
	h.team.mu.Lock()
	h.team.Status = TeamStatusRunning
	h.team.StartedAt = time.Now()
	h.team.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ptm.executeWorkflow(context.Background(), h.team, false)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("executeWorkflow 超时未返回")
	}
}

// stageSeq 阶段序列 (名字/角色/状态), ① 维。
func riStageSeq(team *ProductionTeam) []string {
	team.mu.Lock()
	defer team.mu.Unlock()
	out := make([]string, 0, len(team.Stages))
	for _, s := range team.Stages {
		out = append(out, fmt.Sprintf("%s/%s/%s", s.Name, s.Role, s.Status))
	}
	return out
}

// riBoardKeys 黑板键 (⑨ 维), 排序后比对 (写入序由并发决定, 不属等价性要求)。
func riBoardKeys(team *ProductionTeam) []string {
	if team.Blackboard == nil {
		return nil
	}
	var keys []string
	for _, cat := range []string{"context", "decision", "artifact", "result", "progress"} {
		for _, e := range team.Blackboard.ReadByCategory(cat) {
			keys = append(keys, e.Key+"/"+e.Category)
		}
	}
	sort.Strings(keys)
	return keys
}

func riEqualStrs(t *testing.T, dim string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s 不等价: 条数 got=%d want=%d\ngot =%#v\nwant=%#v", dim, len(got), len(want), got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s 第 %d 条不等价:\ngot =%q\nwant=%q", dim, i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 金标准
// ---------------------------------------------------------------------------

// riGoldenStages ① 阶段序列。
var riGoldenStages = []string{
	"s0/ri-lead/completed",
	"s1/ri-one/completed",
	"s2/ri-two/completed",
	"s3/ri-sum/completed",
}

// riGoldenCalls ② agent 调用次数 (4 个阶段各一次, 无重试)。
const riGoldenCalls = 4

// riGoldenPeak ③ 峰值并发 (s1∥s2)。
const riGoldenPeak = 2

// riGoldenBoard ⑨ 黑板键。
var riGoldenBoard = []string{
	"objective/context",
	"s0-result/result",
	"s0-status/progress",
	"s1-result/result",
	"s1-status/progress",
	"s2-result/result",
	"s2-status/progress",
	"s3-result/result",
	"s3-status/progress",
	"workflow/context",
}

// riGoldenNotify ⑤ 通知文案 (归一 + 排序后)。
//
// 为什么排序: s1∥s2 并行, 两者的"开始执行/第 1 次尝试"四条文案的**相对顺序**由
// goroutine 调度决定, 本来就不确定 (取样两次就能看到 s2 排在 s1 前面)。把不确定的
// 东西写进金标准会得到一个随机红的测试。集合与条数仍然逐字比对 —— 少一条/多一条/
// 改一个字都会红。运行级那条 (团队完成) 另外单独按前缀+序位断言, 它必须是最后一条。
var riGoldenNotify = []string{
	"__TEAM_DONE__",
	"🔄 阶段 **s0** (ri-lead) 开始执行...",
	"🔄 阶段 **s0** (ri-lead) 第 1 次尝试...",
	"🔄 阶段 **s1** (ri-one) 开始执行...",
	"🔄 阶段 **s1** (ri-one) 第 1 次尝试...",
	"🔄 阶段 **s2** (ri-two) 开始执行...",
	"🔄 阶段 **s2** (ri-two) 第 1 次尝试...",
	"🔄 阶段 **s3** (ri-sum) 开始执行...",
	"🔄 阶段 **s3** (ri-sum) 第 1 次尝试...",
}

// riTeamDonePrefix 运行级收尾通知的前缀 (Notifier 拦截器的产出)。
const riTeamDonePrefix = "✅ 团队 **ri-team** 执行完成"

// TestRunChain四维金标准 ①②③⑤⑥⑦⑨ 全维比对。
func TestRunChain四维金标准(t *testing.T) {
	h := newRIHarness(t, 120*time.Millisecond)
	h.run(t)

	calls, peak, _, prompts := h.rec.snapshot()

	// ① 阶段序列
	riEqualStrs(t, "① 阶段序列", riStageSeq(h.team), riGoldenStages)
	// ② 调用次数
	if calls != riGoldenCalls {
		t.Errorf("② agent 调用次数不等价: got=%d want=%d", calls, riGoldenCalls)
	}
	// ③ 峰值并发
	if peak != riGoldenPeak {
		t.Errorf("③ 峰值并发不等价: got=%d want=%d", peak, riGoldenPeak)
	}
	// ④ 提示词逐字: 见 TestRunChain提示词逐字
	if len(prompts) != riGoldenCalls {
		t.Fatalf("④ 提示词条数 got=%d want=%d", len(prompts), riGoldenCalls)
	}
	// ⑥ 团队终态
	h.team.mu.Lock()
	status := h.team.Status
	h.team.mu.Unlock()
	if status != TeamStatusCompleted {
		t.Errorf("⑥ 团队终态不等价: got=%s want=%s", status, TeamStatusCompleted)
	}
	// ⑦ 报告文件 (saveTeamReport 落 REPORT.md + ARTIFACTS.json) + 记忆写入
	for _, f := range []string{"REPORT.md", "ARTIFACTS.json"} {
		if _, err := os.Stat(filepath.Join(h.team.dataDir, f)); err != nil {
			t.Errorf("⑦ 缺少 %s: %v", f, err)
		}
	}
	memRows := h.mem.snapshot()
	if len(memRows) != 1 || !strings.HasPrefix(memRows[0], "ri-team|"+riWorkflowName+"|把 X 做成 Y|len=") {
		t.Errorf("⑦ 记忆写入不等价: %#v", memRows)
	}
	// ⑨ 黑板键
	riEqualStrs(t, "⑨ 黑板键", riBoardKeys(h.team), riGoldenBoard)

	// ⑤ 通知文案: 末条 (团队完成) 必须仍是最后一条 —— 报告路径要先落盘才能进文案,
	// 顺序错了用户看到的就是一条没有报告链接的完成通知。
	raw := riNormalize(h.notify.snapshot())
	if len(raw) == 0 || !strings.HasPrefix(raw[len(raw)-1], riTeamDonePrefix) {
		t.Fatalf("⑤ 末条通知应为团队完成, 实为 %#v", raw)
	}
	if !strings.Contains(raw[len(raw)-1], "📄 **完整报告**") {
		t.Errorf("⑤ 完成通知丢了报告链接 (说明报告落盘排到了通知之后): %q", raw[len(raw)-1])
	}
	raw[len(raw)-1] = "__TEAM_DONE__"
	sort.Strings(raw)
	riEqualStrs(t, "⑤ 通知文案", raw, riGoldenNotify)
}

// TestRunChain提示词逐字 ④ 维: 提示词整段固化。
//
// 为什么整段固化而不是只比长度: 拆分若丢掉 WorkflowExecutor 的某个注入字段
// (roles / planCfgResolver / evolution), 提示词长度往往还相近, 内容已经变了。
func TestRunChain提示词逐字(t *testing.T) {
	h := newRIHarness(t, 0)
	h.run(t)
	_, _, roles, prompts := h.rec.snapshot()

	byRole := map[string]string{}
	for i, r := range roles {
		byRole[r] = prompts[i]
	}
	for role, want := range riGoldenPrompts {
		got, ok := byRole[role]
		if !ok {
			t.Errorf("④ 角色 %s 未被调用", role)
			continue
		}
		if got != want {
			t.Errorf("④ 角色 %s 提示词不等价:\n--- got (%d 字节) ---\n%s\n--- want (%d 字节) ---\n%s",
				role, len(got), got, len(want), want)
		}
	}
	if len(byRole) != len(riGoldenPrompts) {
		t.Errorf("④ 被调用角色数 got=%d want=%d", len(byRole), len(riGoldenPrompts))
	}
}

// riDumpGolden 只在 RI_DUMP=1 时运行: 打印当前实现的观测值, 供固化金标准。
// 保留它是因为下一次真要改这些文案时, 重新取金标准得有个确定的办法。
func TestRunChain金标准取样(t *testing.T) {
	if os.Getenv("RI_DUMP") != "1" {
		t.Skip("设 RI_DUMP=1 打印当前观测值 (取金标准用)")
	}
	h := newRIHarness(t, 120*time.Millisecond)
	h.run(t)
	calls, peak, roles, prompts := h.rec.snapshot()
	t.Logf("① 阶段序列 = %#v", riStageSeq(h.team))
	t.Logf("② 调用次数 = %d", calls)
	t.Logf("③ 峰值并发 = %d", peak)
	t.Logf("⑨ 黑板键 = %#v", riBoardKeys(h.team))
	t.Logf("⑤ 通知文案 = %#v", riNormalize(h.notify.snapshot()))
	h.team.mu.Lock()
	t.Logf("⑥ 终态 = %s", h.team.Status)
	h.team.mu.Unlock()
	t.Logf("⑦ 记忆 = %#v", h.mem.snapshot())
	reports, _ := filepath.Glob(filepath.Join(h.team.dataDir, "report-*.md"))
	t.Logf("⑦ 报告 = %#v", reports)
	for i, r := range roles {
		t.Logf("④ 提示词[%s] = %q", r, prompts[i])
	}
}

// riOut 桩产出 (与 riRunner.Execute 同一份构造, 金标准里作为占位内容嵌入)。
func riOut(role string) string {
	return "OUT[" + role + "]" + strings.Repeat("内容充实的一段产出。", 12)
}

// riGoldenPrompts ④ 提示词逐字金标准。
//
// 结构 (Handoff 块 / Completed Work 条目 / 依赖块 / 分隔符 / 阶段模板替换结果) 全部
// 是字面量; 只有两处引用了包内的量:
//   - 阶段产出用 riOut() 嵌入 —— 它是桩自己造的, 写成字面量只是把同一段话抄两遍;
//   - 末尾的防死循环指令引用 antiLoopDirective 常量 —— 那段话本身不是本次拆分的
//     被测对象, 抄成字面量只会让"以后改指令措辞"多红一次假警报。
//     结构上"它必须出现在末尾且只出现一次"仍被本比对钉住。
// ⚠️ 依赖块的尾部换行是 **2 个**而不是 3 个。初版手写成 3 个, 三个有依赖的角色全部
// 报"1 字节不等价" —— 那不是拆分引入的偏移, 是 golden 写错了: 依赖块写入处是
// `...blackboard key \`%s-result\`.\n\n`(workflow.go buildStagePromptWithRoles),
// 恰好两个; 加上 antiLoopDirective 自带的前导换行共 3 个。
// 判据: buildStagePromptWithRoles 完全没被本次拆分改动(workflow.go 的唯一改动是限流
// 判据去重), 提示词全由它构建 ⇒ 旧路径产出与新路径逐字相同, 这条断言对旧路径同样会红。
var riGoldenPrompts = map[string]string{
	"ri-lead": "## Handoff Context\n\n" +
		"### Your Role: ri-lead\n" +
		"Use the summaries above by default. Query/read the referenced blackboard artifact only when precise details are required.\n\n" +
		"\n---\n\n" +
		"S0 目标: 把 X 做成 Y" + antiLoopDirective,

	"ri-one": "## Handoff Context\n\n" +
		"### Completed Work\n" +
		"**s0** (ri-lead):\n" + riOut("ri-lead") + "\n\n" +
		"Full artifact/ref: blackboard key `s0-result`.\n\n" +
		"### Your Role: ri-one\n" +
		"Use the summaries above by default. Query/read the referenced blackboard artifact only when precise details are required.\n\n" +
		"\n---\n\n" +
		"S1 依据: ### Dependency output from s0:\n" + riOut("ri-lead") + "\n\n" +
		"Full artifact/ref: blackboard key `s0-result`.\n\n" + antiLoopDirective,

	"ri-two": "## Handoff Context\n\n" +
		"### Completed Work\n" +
		"**s0** (ri-lead):\n" + riOut("ri-lead") + "\n\n" +
		"Full artifact/ref: blackboard key `s0-result`.\n\n" +
		"### Your Role: ri-two\n" +
		"Use the summaries above by default. Query/read the referenced blackboard artifact only when precise details are required.\n\n" +
		"\n---\n\n" +
		"S2 依据: ### Dependency output from s0:\n" + riOut("ri-lead") + "\n\n" +
		"Full artifact/ref: blackboard key `s0-result`.\n\n" + antiLoopDirective,

	// s3 的依赖块顺序 (s1 再 s2) 由 StageDef.DependsOn 声明序决定, 不是字典序偶然相同:
	// 声明序漂移会让同一阶段拿到不同提示词 (也会打散提示词前缀缓存)。
	"ri-sum": "## Handoff Context\n\n" +
		"### Completed Work\n" +
		"**s0** (ri-lead):\n" + riOut("ri-lead") + "\n\n" +
		"Full artifact/ref: blackboard key `s0-result`.\n\n" +
		"**s1** (ri-one):\n" + riOut("ri-one") + "\n\n" +
		"Full artifact/ref: blackboard key `s1-result`.\n\n" +
		"**s2** (ri-two):\n" + riOut("ri-two") + "\n\n" +
		"Full artifact/ref: blackboard key `s2-result`.\n\n" +
		"### Your Role: ri-sum\n" +
		"Use the summaries above by default. Query/read the referenced blackboard artifact only when precise details are required.\n\n" +
		"\n---\n\n" +
		"S3 汇总: ### Dependency output from s1:\n" + riOut("ri-one") + "\n\n" +
		"Full artifact/ref: blackboard key `s1-result`.\n\n" +
		"### Dependency output from s2:\n" + riOut("ri-two") + "\n\n" +
		"Full artifact/ref: blackboard key `s2-result`.\n\n" + antiLoopDirective,
}
