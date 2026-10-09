package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

// 本文件是 21.8.12 P4 的验收测试。
//
// 三条贯穿纪律在这个文件里各有一个对应用例:
//  ① 每一条新声明都必须同时给出它的消费者 —— TestStageDeclaredOverrideCarriesEveryField
//     断言的是"搬运"这一步不漏字段 (漏了的表现是某条声明在 graph 路径上静默归零)。
//  ② 降级必须可见 —— TestValidateAllWarnsOnUnregisteredAgent /
//     TestAllowUnregisteredRoleSilencesWarning。
//  ③ 验收只认运行态读数 —— 负 4 (落盘 JSON 迁移) 断的是**解析出来的 Agent 字段非空**,
//     而不是"没报错"。这两者在本仓的历史上分得很开: 一个被静默忽略的键不会报任何错。

// ---------------------------------------------------------------------------
// 负 4: 改造前落盘的工作流 JSON —— 断言 Agent 非空, 而不是靠"没报错"
// ---------------------------------------------------------------------------

// legacyStageJSON 是 StageDef **改造前**写成的样子 (键是 "role")。
//
// 直接写字面量而不是从 git 历史里取: 这个用例要断言的正是"那种形状的输入现在
// 读得出 agent" —— 输入必须钉死在测试里, 从别处取会让它随被取的东西一起演化,
// 于是"旧格式还能读吗"这个问题在旧格式消失后就再也问不出来了。
// 名字单独提出来当常量: 注册进表里的是 JSON 里的 name 字段, **不是文件名** ——
// 下面那条落盘用例一开始就是在这里错的 (按文件名去查, 查到 nil, 而装载明明成功了)。
const legacyWorkflowName = "legacy-wf"

const legacyStageJSON = `{
  "name": "` + legacyWorkflowName + `",
  "mode": "pipeline",
  "stages": [
    {"name": "plan",  "role": "architect", "prompt": "先规划: {objective}"},
    {"name": "build", "role": "coder",     "prompt": "再实现: {prev_result}"},
    {"name": "check", "role": "reviewer",  "prompt": "最后审查"}
  ],
  "custom": true,
  "revision": 3
}`

// TestLegacyRoleKeyMigratesToAgent 是 P4 验收的**负 4**。
//
// ---------------------------------------------------------------------------
// 为什么这条必须单独测, 而编译器兜不住
// ---------------------------------------------------------------------------
//
// D2 (Role → Agent) 的全部连带改动都在 Go 源码里, 改错编译不过。唯一的例外是
// **落盘的 JSON** —— 那是运行时数据, 改名之后 encoding/json 会把 "role" 键**静默
// 忽略**, Agent 字段为空, 于是每个 stage 都退化成内联 prompt。
//
// "静默忽略"意味着: 没有报错、没有日志、装载成功、工作流在列表里, 只是跑起来用的
// 是 prompt 而不是角色的 prompt —— 与"作者本来就写的内联 prompt"逐字节同形。
//
// 所以这里断的是**解析结果**, 不是 err: `if err != nil` 在这个缺陷上永远是 false。
func TestLegacyRoleKeyMigratesToAgent(t *testing.T) {
	var def WorkflowDef
	if err := json.Unmarshal([]byte(legacyStageJSON), &def); err != nil {
		t.Fatalf("旧格式必须能解析 (解析失败会让整条工作流在启动装载时被丢掉): %v", err)
	}
	want := map[string]string{"plan": "architect", "build": "coder", "check": "reviewer"}
	for _, s := range def.Stages {
		if got := strings.TrimSpace(s.Agent); got == "" {
			t.Fatalf("stage %q 的 agent 为空 —— "+
				"旧键 \"role\" 被静默忽略了 (这正是这条用例要挡的形态: 没有报错, 只是每个 stage 都退回内联 prompt)",
				s.Name)
		}
		if got := s.Agent; got != want[s.Name] {
			t.Fatalf("stage %q 解析出的 agent = %q, 期望 %q", s.Name, got, want[s.Name])
		}
	}
	// 迁移是**单向**的 (读旧写新): 落盘时只写 agent。双向会让新文件带着一个永远
	// 为空的 legacy 键, 而"文件里有两个名字且只有一个生效"是下一个人一定会踩的坑。
	again, err := json.Marshal(&def)
	if err != nil {
		t.Fatal(err)
	}
	// 匹配 `"role":` 而不是裸的 role —— 结构体里还有 allowUnregisteredRole 这类
	// 名字里带 role 的键, 用裸串会在某天它变成非 omitempty 时开始误报。
	if strings.Contains(string(again), `"role":`) {
		t.Fatalf("序列化后不该再出现 role 键 (单向兼容): %s", again)
	}
	if !strings.Contains(string(again), `"agent":"coder"`) {
		t.Fatalf("序列化后应写 agent 键: %s", again)
	}
}

// TestLegacyRoleKeyDoesNotOverrideAgent 断言两个键**同时**出现时以 agent 为准。
//
// 迁移期可能存在手工改过一半的文件 (先加了 agent、旧键忘了删)。让新键赢是唯一
// 能预测的规则 —— 反过来 (legacy 赢) 会让"我明明改成了 X 但它跑的是 Y"没有任何
// 可查之处, 而这是本次改名要消灭的那一类恰恰相反的一种。
func TestLegacyRoleKeyDoesNotOverrideAgent(t *testing.T) {
	var s StageDef
	raw := `{"name":"s","agent":"coder","role":"architect","prompt":"x"}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if s.Agent != "coder" {
		t.Fatalf("两个键同时出现时 agent 应赢, got %q", s.Agent)
	}
}

// TestLegacyWorkflowLoadsFromDiskEndToEnd 把负 4 推到**落盘装载**这一层。
//
// 上面两条测的是 UnmarshalJSON; 这条测的是真装载路径 (LoadWorkflowsFromDirReport →
// registerWorkflowLocked → Validate): 旧格式的文件放进目录后, 注册进表里的那一份
// 必须带着非空的 agent。中间任何一环把它抹平, 生产上就是"重启之后所有工作流都退化了",
// 而那不会报错。
func TestLegacyWorkflowLoadsFromDiskEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cleanupGraphs(t, legacyWorkflowName)
	t.Cleanup(func() { cleanupGraphs(t, legacyWorkflowName) })
	if err := os.WriteFile(filepath.Join(dir, legacyWorkflowName+".json"), []byte(legacyStageJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	roles := NewRoleRegistry(t.TempDir())
	rep := LoadWorkflowsFromDirReport(dir, roles)
	if rep.Loaded != 1 || rep.Failed != 0 {
		t.Fatalf("旧格式文件应装载成功且不产生失败项: %+v", rep)
	}
	// ★ 查的是 JSON 里的 name, 不是文件名 —— 这两者在本用例里刻意写成不一样的,
	// 因为"装载成功"与"我按文件名找得到它"是两件事, 而它们一旦被写成都一样,
	// 就再也测不出"注册用了哪个名字"。
	got := getCustomWorkflow(legacyWorkflowName)
	if got == nil {
		t.Fatalf("装载后注册表里应有 %q", legacyWorkflowName)
	}
	if got.Stages[0].Agent != "architect" {
		t.Fatalf("注册进表里的那一份必须带着 agent, got %q (空 ⇒ 生产上就是整批工作流在重启后退化)",
			got.Stages[0].Agent)
	}
	if !got.Custom {
		t.Fatal("装载进来的必须是自定义工作流 (否则它会被当成内置: 不可删、不可改写)")
	}
	if got.Revision != 3 {
		t.Fatalf("装载必须保留文件里自己的 revision (3), got %d —— "+
			"装载走新建分支 (+1) 会让每次重启都给所有工作流涨一格, 客户端手里的版本号随之失效", got.Revision)
	}
}

// TestRealPersistedWorkflowsHaveAgent 用**真机落盘的**工作流跑一遍负 4。
//
// 目录不存在时 skip: 本仓的测试必须能在任意机器上跑 (CI 上没有 /mnt/data)。
// 但它存在的意义恰恰是"在这台机器上、用真的那些文件" —— 合成 fixture 覆盖不到
// 真文件里那些边角形状 (空的 prompt / 多 stage / 宿主平台的角色名)。
//
// 环境变量 CLAUDE_GO_WORKFLOW_DIR 可覆盖路径。
func TestRealPersistedWorkflowsHaveAgent(t *testing.T) {
	dir := os.Getenv("CLAUDE_GO_WORKFLOW_DIR")
	if dir == "" {
		dir = "/mnt/data/claude-go/workflows"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("落盘工作流目录不可读 (%s): %v —— 本用例只在有真实数据的机器上有意义", dir, err)
	}
	checked, empty := 0, []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		var def WorkflowDef
		if err := json.Unmarshal(data, &def); err != nil {
			t.Fatalf("%s: 解析失败 %v", e.Name(), err)
		}
		for _, s := range def.Stages {
			checked++
			if strings.TrimSpace(s.Agent) == "" {
				empty = append(empty, e.Name()+"/"+s.Name)
			}
		}
	}
	if checked == 0 {
		t.Skipf("%s 下没有可检查的 stage", dir)
	}
	if len(empty) > 0 {
		t.Fatalf("共 %d 个 stage, 其中 %d 个的 agent 读不出来 ⇒ 它们会在运行时静默退化成内联 prompt: %v",
			checked, len(empty), empty)
	}
	t.Logf("真机落盘工作流: %d 个 stage 全部带非空 agent", checked)
}

// TestLegacyRoleKeyStillResolvesRegisteredAgent 断言迁移过来的 agent 名**真的能解析**。
//
// 上一条只断"非空"。非空与"解析得出角色"是两件事 —— 一个拼错的名字同样非空,
// 而它跑起来与空的是同一个效果 (退化成内联 prompt)。这条把两件事接上。
func TestLegacyRoleKeyStillResolvesRegisteredAgent(t *testing.T) {
	var def WorkflowDef
	if err := json.Unmarshal([]byte(legacyStageJSON), &def); err != nil {
		t.Fatal(err)
	}
	roles := NewRoleRegistry(t.TempDir())
	for _, s := range def.Stages {
		if roles.Get(s.Agent) == nil {
			t.Fatalf("stage %q 的 agent %q 在角色表里解析不出来 "+
				"(这条与上一条不同: 名字非空但解析不出, 跑起来的后果一模一样)", s.Name, s.Agent)
		}
	}
}

// ---------------------------------------------------------------------------
// 纪律 ② 在装载路径上的形态: 三类结果必须分得开
// ---------------------------------------------------------------------------

// TestLoadReportSeparatesLoadedFailedDeferred 断言装载报告把三类结果**分开报**。
//
// ---------------------------------------------------------------------------
// 为什么这三类不能合成一个数
// ---------------------------------------------------------------------------
//
// 启动装载是"这份表现在长什么样"唯一的读数, 而它的三类结果指向三个完全不同的
// 下一步动作:
//
//   - Loaded   装着 ✓, 无需动作。
//   - Failed   这份文件**没进表**: 去修那个文件。
//   - Deferred 这份文件**进了表, 但跑起来与你写的不是同一件事**: 决定是改名字
//     还是把 allowUnregisteredRole 写上。
//
// 合成一个数 (或只报 Failed) 的后果是把第三类**整个吞掉**: 一份 agent 名打错的
// 工作流会安静地装载成功, 装载汇总里全绿, 而它跑起来用的是内联 prompt。这正是
// 本轮 W3 要消灭的形态, 而装载路径是它最主要的入口 (40 条在野工作流都从这里进来)。
//
// ★ Deferred 与 Loaded **同时计数**是刻意的: "装载成功"与"有降级"不是二选一。
// 把有降级的从 Loaded 里扣掉会让人以为那几个文件没进表, 于是去修本来没坏的东西。
func TestLoadReportSeparatesLoadedFailedDeferred(t *testing.T) {
	dir := t.TempDir()
	write := func(file, body string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// ① 干净的一份: 内联 prompt + 已注册的 agent。
	write("clean.json", `{"name":"lr-clean","mode":"pipeline","stages":[{"name":"a","agent":"coder","prompt":"{objective}"}]}`)
	// ② 降级的一份: agent 不在表里, 但有内联 prompt ⇒ 装得进, 跑的是 prompt。
	write("degraded.json", `{"name":"lr-degraded","mode":"pipeline","stages":[{"name":"a","agent":"host-only-agent","prompt":"{objective}"}]}`)
	// ③ 坏掉的一份: 硬错 ⇒ 装不进。
	write("broken.json", `{"name":"lr-broken","mode":"不存在的模式","stages":[{"name":"a","agent":"coder"}]}`)
	// ④ 连 JSON 都不是。
	write("garbage.json", `{不是 JSON`)

	for _, n := range []string{"lr-clean", "lr-degraded", "lr-broken"} {
		cleanupGraphs(t, n)
		t.Cleanup(func() { cleanupGraphs(t, n) })
	}
	roles := NewRoleRegistry(t.TempDir())
	rep := LoadWorkflowsFromDirReport(dir, roles)

	if rep.Loaded != 2 {
		t.Fatalf("clean 与 degraded 都应装载成功, Loaded=%d (期望 2): %+v", rep.Loaded, rep)
	}
	if rep.Failed != 2 {
		t.Fatalf("broken 与 garbage 都应记为失败, Failed=%d (期望 2): %+v", rep.Failed, rep)
	}
	if len(rep.Deferred) != 1 {
		t.Fatalf("只有 degraded 该有降级项, 但有 %d 条: %v", len(rep.Deferred), rep.Deferred)
	}
	d := rep.Deferred[0]
	for _, want := range []string{"degraded.json", "host-only-agent"} {
		if !strings.Contains(d, want) {
			t.Fatalf("降级项必须点名文件与 agent (只说'有降级'用户不知道该去改哪一条): %q 里没有 %q", d, want)
		}
	}
	if len(rep.Details) != 2 {
		t.Fatalf("失败项必须逐条给出原因, got %v", rep.Details)
	}
	joined := strings.Join(rep.Details, "\n")
	if !strings.Contains(joined, "broken.json") || !strings.Contains(joined, "garbage.json") {
		t.Fatalf("失败明细必须点名文件: %v", rep.Details)
	}
	// 降级的那一份**确实进了表** (而不是被当成失败丢掉)。
	if wf := getCustomWorkflow("lr-degraded"); wf == nil {
		t.Fatal("有降级 ≠ 没装载: lr-degraded 必须进表")
	}
	// Summary 一行里三类都要出现 —— 它是启动日志里唯一被看到的那一行。
	sum := rep.Summary(dir)
	for _, want := range []string{"2", "降级"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("装载汇总必须报出降级条数 (%q 里没有 %q): %s", sum, want, sum)
		}
	}
}

// ---------------------------------------------------------------------------
// 纪律 ③ 的另一半: 声明必须真的到达消费点
// ---------------------------------------------------------------------------

// TestStageDeclaredOverrideCarriesEveryField 断言覆盖表搬运营不漏任何声明字段。
//
// 这是 21.8.12 P4/X2 的核心: StageDef 上的每一项都必须在 stageDeclaredOverride 里
// 被搬一次。漏一个的表现**不报错** —— 那条声明在 pipeline 路径上生效、在 graph
// 路径上静默归零, 一份声明在两条执行路径上给出两个结果, 而读数上分不出哪条路在跑。
//
// 逐字段而不是整结构体比较: 结构体相等断言在**新增字段**时会自动通过 (零值 == 零值),
// 于是"新加了一个字段但忘了搬"这条最可能发生的缺陷恰好被漏掉。这里显式列出每一项,
// 加字段的人必须同时改这个用例 —— 那时他会看到自己该在 stageDeclaredOverride 里做事。
func TestStageDeclaredOverrideCarriesEveryField(t *testing.T) {
	st := StageDef{
		Name:           "s",
		Agent:          "coder",
		Prompt:         "p",
		Model:          "kimi-k2",
		Hooks:          []string{"PreToolUse:command:./g.sh"},
		Skills:         []string{"go-patterns"},
		Kind:           "gate",
		Condition:      "x > 1",
		EdgeConditions: map[string]string{"up": "y == 2"},
		Retry:          &graph.RetryPolicy{MaxRetries: 3},
		TimeoutSec:     42,
		Loop:           &graph.LoopPolicy{MaxIterations: 5},
		MaxTurns:       7,
		Deterministic:  true,
		Placement:      &graph.PlacementSpec{Prefer: "local"},
		Tools: &StageTools{
			Profile: "coding",
			Allow:   &[]string{"Bash", "StrReplace"},
			Deny:    []string{"WebFetch"},
		},
	}
	so := stageDeclaredOverride(st)

	// —— 图能力九项 ——
	if so.Kind != st.Kind {
		t.Errorf("Kind 未搬运: %q != %q", so.Kind, st.Kind)
	}
	if so.Condition != st.Condition {
		t.Errorf("Condition 未搬运")
	}
	if so.EdgeConditions["up"] != "y == 2" {
		t.Errorf("EdgeConditions 未搬运: %v", so.EdgeConditions)
	}
	if so.Retry == nil || so.Retry.MaxRetries != 3 {
		t.Errorf("Retry 未搬运: %+v", so.Retry)
	}
	if so.TimeoutSec != 42 {
		t.Errorf("TimeoutSec 未搬运: %d", so.TimeoutSec)
	}
	if so.Loop == nil || so.Loop.MaxIterations != 5 {
		t.Errorf("Loop 未搬运: %+v", so.Loop)
	}
	if so.MaxTurns != 7 {
		t.Errorf("MaxTurns 未搬运: %d", so.MaxTurns)
	}
	if !so.Deterministic {
		t.Errorf("Deterministic 未搬运")
	}
	if so.Placement == nil || so.Placement.Prefer != "local" {
		t.Errorf("Placement 未搬运: %+v", so.Placement)
	}
	// —— 工具面三项 ——
	if so.ToolProfile != "coding" {
		t.Errorf("ToolProfile 未搬运: %q", so.ToolProfile)
	}
	if so.Tools == nil || len(*so.Tools) != 2 {
		t.Errorf("Tools (白名单) 未搬运: %v", so.Tools)
	}
	if len(so.DisallowedTools) != 1 || so.DisallowedTools[0] != "WebFetch" {
		t.Errorf("DisallowedTools (黑名单) 未搬运: %v", so.DisallowedTools)
	}
}

// TestStageDeclaredOverrideCopiesMutableFields 断言搬运**复制**了会被就地改的字段。
//
// 覆盖表的合并是就地写的 (mergeStageOverride 会把 map 里的键逐个改写)。搬运时
// 直接交出 StageDef 里的那个 map/slice 引用, 等于让"合并覆盖表"这一步回头改掉
// 工作流定义本身 —— 而定义是**跨 team 共享**的 (每次按需 new 一份只是缓解, 同一个
// team 的多轮之间共享同一个 WorkflowDef)。后果是第二次执行时读到的"原始声明"
// 已经被第一次执行改过, 表现为"同一条工作流跑两遍结果不同", 且第一遍永远是对的。
func TestStageDeclaredOverrideCopiesMutableFields(t *testing.T) {
	allow := []string{"Bash"}
	st := StageDef{
		Name:           "s",
		Agent:          "coder",
		EdgeConditions: map[string]string{"up": "a"},
		Tools:          &StageTools{Allow: &allow, Deny: []string{"X"}},
	}
	so := stageDeclaredOverride(st)
	if so.EdgeConditions == nil {
		t.Fatal("EdgeConditions 应被搬运")
	}
	so.EdgeConditions["up"] = "改过了"
	if st.EdgeConditions["up"] != "a" {
		t.Fatal("EdgeConditions 必须是副本: 就地合并覆盖表会回头改掉工作流定义本身")
	}
	*so.Tools = append(*so.Tools, "多出来的")
	if len(allow) != 1 {
		t.Fatal("Tools 白名单必须是副本 (append 会复用底层数组)")
	}
	so.DisallowedTools[0] = "改过了"
	if st.Tools.Deny[0] != "X" {
		t.Fatal("DisallowedTools 必须是副本")
	}
}

// ---------------------------------------------------------------------------
// 纪律 ②: 降级必须可见 (W3)
// ---------------------------------------------------------------------------

// TestValidateAllWarnsOnUnregisteredAgent 断言"agent 没注册但有内联 prompt"这条
// 由来已久的放宽**仍然放行**, 但**不再静默**。
//
// 这条放宽不能改成硬错: 落盘的 40 条工作流用的多是宿主平台自己的角色名
// (storyloom-agent / qa-case-designer ...), 它们永远不会进 claude-go 的角色表,
// 而每条都自带完整 prompt 且跑得对。把它升级成硬错 = 一次部署事故。
//
// 但"放行且不吭声"也不行: 打错一个字母 (`coder` → `codr`) 的症状与上面那些
// **完全一样** —— 都是内联 prompt 生效, 在看板上没有任何形态。
func TestValidateAllWarnsOnUnregisteredAgent(t *testing.T) {
	roles := NewRoleRegistry(t.TempDir())
	def := &WorkflowDef{
		Name: "warn-unreg",
		Mode: "pipeline",
		Stages: []StageDef{
			{Name: "ok", Agent: "coder", Prompt: "x {objective}"},
			{Name: "typo", Agent: "codr", Prompt: "y {objective}"},
		},
	}
	warns, err := def.ValidateAll(roles)
	if err != nil {
		t.Fatalf("未注册但有内联 prompt 必须**放行** (改成硬错会让已落盘的工作流整批装载失败): %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("应恰好一条告警 (只针对 typo 那个 stage): %v", warns)
	}
	w := warns[0]
	for _, want := range []string{"typo", "codr"} {
		if !strings.Contains(w, want) {
			t.Fatalf("告警必须点名 stage 与 agent 名 (否则用户不知道该去改哪个): %q 里没有 %q", w, want)
		}
	}
	if !strings.Contains(w, "allowUnregisteredRole") {
		t.Fatalf("告警必须给出下一步 (如何消除这条): %q", w)
	}
	if !strings.Contains(w, "40") && !strings.Contains(w, "个") {
		t.Fatalf("告警应报出角色表规模 (表是空的 vs 名字写错了, 处置方向完全不同): %q", w)
	}
}

// TestAllowUnregisteredRoleSilencesWarning 断言显式开口能让告警消失。
//
// 这是 W3 的"开口"半边: 保留降级能力, 但让"我知道我在降级"能被写下来。没有这一半,
// 那 40 条工作流的作者除了无视告警没有别的动作可做 —— 而一份被无视的告警等于没有。
func TestAllowUnregisteredRoleSilencesWarning(t *testing.T) {
	roles := NewRoleRegistry(t.TempDir())
	def := &WorkflowDef{
		Name: "allow-unreg",
		Mode: "pipeline",
		Stages: []StageDef{
			{Name: "host", Agent: "storyloom-agent", Prompt: "x {objective}", AllowUnregisteredRole: true},
		},
	}
	warns, err := def.ValidateAll(roles)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("声明了 allowUnregisteredRole 就不该再告警, got %v", warns)
	}
	// 负: 开口**只**影响告警, 不放宽硬错。没有 prompt 时仍然必须拒绝 ——
	// 那样 agent 连 system prompt 都没有, 与"降级"是两回事。
	bad := &WorkflowDef{
		Name: "allow-unreg-noprompt",
		Mode: "pipeline",
		Stages: []StageDef{
			{Name: "host", Agent: "storyloom-agent", AllowUnregisteredRole: true},
		},
	}
	if _, err := bad.ValidateAll(roles); err == nil {
		t.Fatal("既没注册又没 prompt 必须硬拒 —— allowUnregisteredRole 是告警开口, 不是合法性开口")
	}
}

// TestEmptyAgentIsNotAWarning 断言 **agent 留空不告警** —— 它与"名字拼错"是两回事。
//
// ---------------------------------------------------------------------------
// 为什么这条不是"少报一条无所谓"
// ---------------------------------------------------------------------------
//
// W3 那条告警的措辞是"本次将用内联 prompt 执行"。对 agent 为空的 stage 这句是
// **假话**: 生产上有两类这样的节点, 它们的 prompt 一个字都不会被消费 ——
//
//   - subgraph 节点: 内容来自被引用的图 (subgraph_wiring_test.go 的 fixture 里
//     那两句 prompt 带着注释"去掉它会让整个声明期校验失败" —— 它纯粹是声明期占位);
//   - map / gateway 节点同理。
//
// 假话比沉默更糟: 作者会去追查一句根本不会被执行的 prompt, 而真正该看的
// (那张被引用的图) 一个字都没提。而这条告警要抓的形态 —— "把 coder 打成 codr" ——
// 前提是**有一个名字**; 空名字不可能拼错。
//
// ★ 硬错那一档必须不动: agent 空**且** prompt 空仍然拒绝 (那时连声明都没有)。
func TestEmptyAgentIsNotAWarning(t *testing.T) {
	roles := NewRoleRegistry(t.TempDir())
	// subgraph 形态: 空 agent + 占位 prompt。
	def := &WorkflowDef{
		Name: "empty-agent", Mode: "graph",
		Stages: []StageDef{
			{Name: "produce", Agent: "coder", Prompt: "先准备 {objective}"},
			{Name: "reuse", DependsOn: []string{"produce"}, Prompt: "本阶段由被引用的子图产出"},
		},
	}
	warns, err := def.ValidateAll(roles)
	if err != nil {
		t.Fatalf("空 agent + 有占位 prompt 必须放行: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("空 agent 不该告警 (告警文案会说'将用内联 prompt 执行', 而对 subgraph/map 节点这是假话): %v", warns)
	}
	// 负: 空 agent **且** 空 prompt 仍然硬拒。
	bad := &WorkflowDef{Name: "empty-both", Mode: "pipeline", Stages: []StageDef{{Name: "a"}}}
	if _, err := bad.ValidateAll(roles); err == nil {
		t.Fatal("agent 与 prompt 都空必须硬拒 —— 那时连声明都没有")
	}
	// 负: 非空但解析不出的名字**仍然**告警 (放宽不能顺手把这档也吞掉)。
	typo := &WorkflowDef{Name: "typo", Mode: "pipeline",
		Stages: []StageDef{{Name: "a", Agent: "codr", Prompt: "x {objective}"}}}
	if w, _ := typo.ValidateAll(roles); len(w) != 1 {
		t.Fatalf("名字拼错必须仍然告警 (空名字与拼错名字是两回事), got %v", w)
	}
}

// TestValidateStillRejectsHardErrors 是上一条的负向对照: 分开两级之后, 原有的
// 硬错一条都不能漏。少一条的表现是"某种跑不起来的定义被放行", 而它跑起来失败时
// 报的是下游的错 (依赖找不到 / nil stage), 与根因隔了好几层。
func TestValidateStillRejectsHardErrors(t *testing.T) {
	roles := NewRoleRegistry(t.TempDir())
	cases := []struct {
		why string
		def *WorkflowDef
	}{
		{"空名字", &WorkflowDef{Name: "", Mode: "pipeline", Stages: []StageDef{{Name: "a", Agent: "coder"}}}},
		{"无 stage", &WorkflowDef{Name: "x", Mode: "pipeline"}},
		{"mode 不支持", &WorkflowDef{Name: "x", Mode: "不存在的模式", Stages: []StageDef{{Name: "a", Agent: "coder"}}}},
		{"stage 无名", &WorkflowDef{Name: "x", Mode: "pipeline", Stages: []StageDef{{Agent: "coder"}}}},
		{"stage 重名", &WorkflowDef{Name: "x", Mode: "pipeline", Stages: []StageDef{{Name: "a", Agent: "coder"}, {Name: "a", Agent: "coder"}}}},
		{"依赖不存在", &WorkflowDef{Name: "x", Mode: "pipeline", Stages: []StageDef{{Name: "a", Agent: "coder", DependsOn: []string{"nope"}}}}},
		{"无角色且无 prompt", &WorkflowDef{Name: "x", Mode: "pipeline", Stages: []StageDef{{Name: "a", Agent: "nobody"}}}},
		{"agent 与 prompt 都空", &WorkflowDef{Name: "x", Mode: "pipeline", Stages: []StageDef{{Name: "a"}}}},
	}
	for _, c := range cases {
		if _, err := c.def.ValidateAll(roles); err == nil {
			t.Errorf("%s: 必须被硬拒", c.why)
		}
		if err := c.def.Validate(roles); err == nil {
			t.Errorf("%s: Validate (兼容入口) 也必须硬拒", c.why)
		}
	}
}

// ---------------------------------------------------------------------------
// W1: 角色表的来源
// ---------------------------------------------------------------------------

// writeRoleFile 在 <root>/<sub>/agents/<name>.json 写一份最小角色定义。
func writeRoleFile(t *testing.T, root, sub, name, desc string) {
	t.Helper()
	dir := filepath.Join(root, sub, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"` + name + `","category":"workflow","description":"` + desc + `","systemPrompt":"p"}`
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestProductionRoleSourcesCoverAllThreeTiers 断言生产来源清单**覆盖三个档位**。
//
// ---------------------------------------------------------------------------
// 为什么这条不能只断 len(sources) == 3
// ---------------------------------------------------------------------------
//
// 三个档位里 user 依赖 UserHomeDir()、project 依赖 cwd、state 依赖 stateDir, 而
// **空值会被跳过** (CLI 的不少子命令没有 stateDir, 那不是异常)。所以在一个
// HOME 被清掉、或传了空 cwd 的环境里, "三个档位"会退化成两个 —— 而退化的表现
// 是"这个来源的角色装载不上", 与本条要挡的 W1 缺陷同形。
//
// 这里传满三个参数, 断言三个档位都在, 并断言**顺序**是 user < project < state ——
// 顺序决定重名时谁赢 (先加载者优先), 而它没有任何编译期约束。
//
// ★ user 档位只断"它排在最前"而不依赖它真的存在: 某些环境取不到 home, 那时它
// 被跳过是设计内的行为 (见 ProductionRoleSources 注释), 不该让用例变红。
func TestProductionRoleSourcesCoverAllThreeTiers(t *testing.T) {
	srcs := ProductionRoleSources("/tmp/w1-cwd", "/tmp/w1-state")
	var seen []string
	for _, s := range srcs {
		switch s.Source {
		case "user":
			if len(seen) != 0 {
				t.Fatalf("user 必须排在最前 (重名时先加载者赢): %v", srcs)
			}
		case "project":
			if s.Root != "/tmp/w1-cwd" {
				t.Fatalf("project 档位的根必须是 cwd, got %q", s.Root)
			}
		case "state":
			if s.Root != "/tmp/w1-state" {
				t.Fatalf("state 档位的根必须是 stateDir, got %q", s.Root)
			}
			if len(seen) < 2 {
				t.Fatalf("state 必须排在最后 (它是覆盖性最强的一档): %v", srcs)
			}
		default:
			t.Fatalf("未知档位 %q", s.Source)
		}
		seen = append(seen, s.Source)
	}
	for _, want := range []string{"project", "state"} {
		found := false
		for _, s := range seen {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("传了非空的 %s 根, 档位清单里却没有它: %v", want, srcs)
		}
	}
	// 空值跳过: 传空 stateDir 时不该出现一个根为空的 state 档位 (那会去扫 "" 目录)。
	for _, s := range ProductionRoleSources("/tmp/w1-cwd", "") {
		if s.Source == "state" {
			t.Fatalf("stateDir 为空时不该有 state 档位 (空根会去扫当前目录): %v", s)
		}
	}
}

// TestNewRoleRegistryForSeesStateDirRoles 是 W1 的核心用例, 走**生产入口**。
//
// ---------------------------------------------------------------------------
// 为什么必须调 NewRoleRegistryFor, 而不是 NewRoleRegistryFrom
// ---------------------------------------------------------------------------
//
// 21.8.7 记的那条功能正确性缺陷**不在装载逻辑里, 而在接线里**: "用 <stateDir>
// 校验通过的工作流, 运行时 (用 cwd) 查不到它的角色 ⇒ 静默降级到内联 prompt"。
// 装载逻辑本身从来都是对的 —— 错的是三个生产入口各传一个不同的根。
//
// 所以直接拿 NewRoleRegistryFrom 配一组来源去测, 测的是**已经正确的那个东西**:
// 上面那条 M12 变异 (把 NewRoleRegistryFor 改回只认 cwd) 在那种写法下**存活**,
// 因为用例根本没经过被改的那一行。这条走生产入口, 那个变异才会红。
func TestNewRoleRegistryForSeesStateDirRoles(t *testing.T) {
	project := t.TempDir()
	state := t.TempDir()
	writeRoleFile(t, project, ".claude-go", "w1-proj-only", "只在 project")
	writeRoleFile(t, state, ".claude-go", "w1-state-only", "只在 state")

	rr := NewRoleRegistryFor(project, state)
	if rr.Get("w1-proj-only") == nil {
		t.Fatalf("project 档位的角色应被装载 (排查方向: 这个入口的 cwd 传对了吗); 摘要: %s", rr.DescribeSources())
	}
	if rr.Get("w1-state-only") == nil {
		t.Fatalf("state 档位的角色应被装载 —— 这就是 21.8.7 那条缺陷的判据: "+
			"dashboard 用 stateDir 校验通过的 stage, 运行时按 cwd 建表就查不到它, "+
			"而表现只是那个 stage 静默改用了内联 prompt。摘要: %s", rr.DescribeSources())
	}
	// 两个档位都必须出现在摘要里 —— 面板要能回答"这份表是从哪几个根来的"。
	desc := rr.DescribeSources()
	for _, want := range []string{"project", "state", project, state} {
		if !strings.Contains(desc, want) {
			t.Fatalf("来源摘要里缺 %q: %s", want, desc)
		}
	}
	if rr.builtinCount >= rr.Count() {
		t.Fatalf("自定义角色没有计入: builtin=%d total=%d (计数把自定义角色算成内置时, '装了 0 个自定义'与'全装上了'读数相同)",
			rr.builtinCount, rr.Count())
	}
}

// TestRoleRegistryDuplicateNameFirstSourceWins 钉住重名时的**方向**。
//
// 角色表与资产索引的覆盖方向是**相反**的 (资产: 后者盖前者; 角色: 前者赢, 后者被
// 拒绝并记一条装载问题), 而这两条纪律在代码上看起来很像 —— 有人"顺手改齐"的代价是
// 一批既有工作流在定义一个字没改的情况下换行为 (stage / subagent_type / 工具档位
// 回退全按角色名取)。这条把方向钉死, 顺带断言被拒的那一份**留下了读数**。
func TestRoleRegistryDuplicateNameFirstSourceWins(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeRoleFile(t, first, ".claude-go", "w1-dup", "来自第一个来源")
	writeRoleFile(t, second, ".claude-go", "w1-dup", "来自第二个来源")

	rr := NewRoleRegistryFrom([]RoleSource{
		{Source: "first", Root: first},
		{Source: "second", Root: second},
	})
	got := rr.Get("w1-dup")
	if got == nil {
		t.Fatal("重名时应保留第一个, 而不是两份都不要")
	}
	if got.Description != "来自第一个来源" {
		t.Fatalf("重名时先加载者应赢, got 描述 %q (来源顺序决定谁赢, 反过来等于静默换实现)",
			got.Description)
	}
	issues := rr.LoadIssues()
	if len(issues) != 1 {
		t.Fatalf("被拒的那一份必须留下装载问题 (只静默丢弃的话, '两个同名文件都在'与'只有一个在'读数相同): %v", issues)
	}
	if !strings.Contains(issues[0], "w1-dup") || !strings.Contains(issues[0], "[second]") {
		t.Fatalf("问题条目应点名角色与被拒的来源档位: %v", issues)
	}
}

// TestRoleRegistryLoadIssuesAreVisible 断言装载失败**可回读**, 而不是只进日志。
//
// 注册表是只增不减的: 一张"看起来正常"的表既可能是全部装载成功, 也可能是有文件
// 因为重名被整份丢掉了 —— 两者的 Count()/Names() 只在丢掉的是自定义角色时才不同,
// 而调用方没有任何办法分辨。日志能看见, 但日志是滚动的: 面板要回答的是"现在这份
// 注册表是怎么来的"。
func TestRoleRegistryLoadIssuesAreVisible(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".claude-go", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一个坏 JSON + 一个与内置同名的文件 (coder)。
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{不是 JSON`), 0o644); err != nil {
		t.Fatal(err)
	}
	dup := `{"name":"coder","category":"workflow","description":"想覆盖内置","systemPrompt":"p"}`
	if err := os.WriteFile(filepath.Join(dir, "coder.json"), []byte(dup), 0o644); err != nil {
		t.Fatal(err)
	}
	rr := NewRoleRegistryFrom([]RoleSource{{Source: "test", Root: root}})
	issues := rr.LoadIssues()
	if len(issues) != 2 {
		t.Fatalf("两条装载问题都应被记下 (坏 JSON + 重名), got %d: %v", len(issues), issues)
	}
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "broken.json") {
		t.Fatalf("坏 JSON 必须点名文件: %v", issues)
	}
	if !strings.Contains(joined, "coder.json") || !strings.Contains(joined, "已有角色") {
		t.Fatalf("重名必须点名文件与原因: %v", issues)
	}
	// 来源名进每一条: 只报路径不报档位, 排查时要先自己判断这个路径属于哪个入口 ——
	// 而判断依据 (这次是用 cwd 还是 stateDir 建的注册表) 恰恰是看不见的那一项。
	if !strings.Contains(joined, "[test]") {
		t.Fatalf("每条问题都应带来源档位: %v", issues)
	}
	// 内置的 coder 必须还在 —— 被跳过的那个文件才该消失。
	if rr.Get("coder") == nil {
		t.Fatal("与内置重名的文件应被跳过, 内置角色必须还在")
	}
	if !strings.Contains(rr.DescribeSources(), "装载问题 2 条") {
		t.Fatalf("来源摘要应报出问题条数 (否则面板读不出这份表是带伤来的): %s", rr.DescribeSources())
	}
}

// TestRoleRegistryDescribeSourcesListsEmptyRoots 断言**没贡献角色的来源也要列出来**。
//
// "某来源一个角色都没装到"与"某来源压根没被扫"是两件事: 前者通常正常 (新机器上
// 没有 .claude/agents), 后者一定是接线漏了。只列有贡献的来源, 恰好把这个区分抹掉 ——
// 而 21.8.12 W1 要修的正是接线。
func TestRoleRegistryDescribeSourcesListsEmptyRoots(t *testing.T) {
	root := t.TempDir() // 什么都没有的根
	rr := NewRoleRegistryFrom([]RoleSource{{Source: "state", Root: root}})
	desc := rr.DescribeSources()
	if !strings.Contains(desc, "state") || !strings.Contains(desc, root) {
		t.Fatalf("空来源也必须出现在摘要里 (否则分不出'没装到'与'没扫过'): %s", desc)
	}
	if !strings.Contains(desc, "无 agents 目录") {
		t.Fatalf("摘要应说明这个来源下没有 agents 目录: %s", desc)
	}
	// 没有任何来源时也要说清楚, 而不是给一句看起来正常的空摘要。
	bare := NewRoleRegistryFrom(nil)
	if !strings.Contains(bare.DescribeSources(), "无自定义来源") {
		t.Fatalf("无来源时应说明只装了内置角色: %s", bare.DescribeSources())
	}
}

// ---------------------------------------------------------------------------
// W4: 内置工作流走同一条校验路径
// ---------------------------------------------------------------------------

// TestAuditBuiltinWorkflowsReportsEveryBuiltin 断言自检的覆盖面与**对外服务的那一份**对得上。
//
// ---------------------------------------------------------------------------
// 为什么不断言 len(issues) == len(BuiltinWorkflowNames())
// ---------------------------------------------------------------------------
//
// 那个等号是**恒真**的: AuditBuiltinWorkflows 就是用 BuiltinWorkflowNames() 建 out 的,
// 一条名字一个条目, 除非有人把循环改坏, 否则它永远相等。一条永真的断言不是验收,
// 是装饰 —— 本仓 [[empty-hard-gate-set-is-vacuous-truth]] 记的正是这一类。
//
// 真正会出错的形态是**遍历了两个不同的集合**: 自检扫的是 workflowRegistry (内置),
// 而用户看到的是 ListWorkflows() (= 内置 ∪ 自定义)。两者若分叉 —— 比如有人图省事
// 让自检去扫 ListWorkflows 再手工减掉 custom —— 一个"构造器返回 nil"的内置就会
// 从自检里消失, 而它在 ListWorkflows 里本来也不出现: **两边同时消失, 报告全绿**。
// 所以这里断的是: ListWorkflows 里的每一条内置, 自检都必须有一条对应。
func TestAuditBuiltinWorkflowsReportsEveryBuiltin(t *testing.T) {
	roles := NewRoleRegistry(t.TempDir())
	issues := AuditBuiltinWorkflows(roles)
	if len(issues) == 0 {
		t.Fatal("内置工作流一条都没有 ⇒ 注册表是空的, 与 workflowRegistry 对不上")
	}
	byName := map[string]BuiltinWorkflowIssue{}
	for _, is := range issues {
		if is.Name == "" {
			t.Fatal("自检结果必须带名字 (否则报出来的问题不知道该去改哪个定义)")
		}
		if _, dup := byName[is.Name]; dup {
			t.Fatalf("自检结果重复: %s", is.Name)
		}
		byName[is.Name] = is
	}
	// 对外服务的那一份里, 每一条内置都必须被自检覆盖到。
	listed := 0
	for _, wf := range ListWorkflows() {
		if wf.Custom {
			continue
		}
		listed++
		if _, ok := byName[wf.Name]; !ok {
			t.Fatalf("内置 %q 出现在 ListWorkflows 里, 却没有被自检覆盖 "+
				"(自检扫的集合与对外服务的集合分叉了 —— 两边同时少一条时报告是全绿的)",
				wf.Name)
		}
	}
	if listed == 0 {
		t.Fatal("ListWorkflows 里一条非自定义工作流都没有")
	}
	// 反向: 自检报出"构造器返回 nil"的那一条, GetWorkflow 必须真的取不到 ——
	// 否则这条报告是在冤枉一个好好的工作流。
	for _, is := range issues {
		if is.Hard == "" {
			continue
		}
		if !strings.Contains(is.Hard, "nil") {
			continue
		}
		if GetWorkflow(is.Name) != nil {
			t.Fatalf("自检说 %q 的构造器返回 nil, 但 GetWorkflow 取得到它", is.Name)
		}
	}
}

// TestAuditBuiltinWorkflowsConsultsTheGivenRegistry 断言自检**真的读了传进来的那张表**。
//
// ---------------------------------------------------------------------------
// 为什么这条必须靠"两张表比大小"而不是"有没有告警"
// ---------------------------------------------------------------------------
//
// "有告警"是一个可以被硬编码满足的性质 —— 而内置里确实**本来就有**一批解析不出的
// agent 名 (宿主平台自己的角色名), 所以哪怕自检把 roles 丢掉 (`ValidateAll(nil)`),
// 告警照样一大片, 看起来毫无异样。
//
// 换个说法: 断言"告警数 > 0"在**改造前就成立**, 于是它对本次改动零分辨力。要问的
// 是"交进去的那张表被用了吗", 那就得交两张不同的表看结果变不变。空的表必然多出一
// 批告警 (原本解析得出的名字现在解析不出了) —— 一模一样就说明参数没被用。
func TestAuditBuiltinWorkflowsConsultsTheGivenRegistry(t *testing.T) {
	full := NewRoleRegistry(t.TempDir())
	warnCount := func(rs []BuiltinWorkflowIssue) int {
		n := 0
		for _, is := range rs {
			n += len(is.Warnings)
		}
		return n
	}
	fullN := warnCount(AuditBuiltinWorkflows(full))

	// 一张**被掏空**的表: 内置全删掉。用 in-package 的可达性直接构造 ——
	// 生产代码没有任何一条路径能造出空表 (registerBuiltins 恒跑), 而这条用例要的
	// 恰恰是这个生产上不存在的极端, 用来把"读没读表"放大成一个可观测的差。
	empty := NewRoleRegistryFrom(nil)
	empty.roles = map[string]*RoleDef{}
	emptyN := warnCount(AuditBuiltinWorkflows(empty))

	if fullN == 0 {
		t.Fatal("拿生产角色表跑自检一条告警都没有 ⇒ 要么内置全对 (不像), 要么自检没在校验")
	}
	if emptyN <= fullN {
		t.Fatalf("把角色表掏空之后自检的告警数没有增加 (%d → %d) ⇒ "+
			"自检没有读传进去的那张表 (参数被忽略时输出与'表里全都有'完全同形)", fullN, emptyN)
	}
	t.Logf("自检对角色表的依赖: 生产表 %d 条告警, 空表 %d 条", fullN, emptyN)
}

// TestAuditBuiltinWorkflowsNamesMatchRegistry 断言内置定义里 stage 用的 agent 名
// **大部分**能在角色表里解析 —— 即量具有一定的分辨力。
//
// 不断言"全部解析得出": 内置里确实有刻意走内联 prompt 的 stage (实测 149 个 stage
// 里 98 个解析得出)。断言一个下限 (过半) 是为了挡住"审计读错了表"这一整类失效 ——
// 读错了表的表现是**全部**解析不出, 而那种报告看起来像"内置工作流全是坏的",
// 一个很唬人但完全错误的结论。
func TestAuditBuiltinWorkflowsNamesMatchRegistry(t *testing.T) {
	roles := NewRoleRegistry(t.TempDir())
	total, resolved := 0, 0
	for _, n := range BuiltinWorkflowNames() {
		def := WorkflowDefFromBuiltin(n)
		if def == nil {
			continue
		}
		for _, s := range def.Stages {
			total++
			if strings.TrimSpace(s.Agent) != "" && roles.Get(s.Agent) != nil {
				resolved++
			}
		}
	}
	if total == 0 {
		t.Fatal("内置工作流里一个 stage 都没有")
	}
	if resolved*2 < total {
		t.Fatalf("内置 %d 个 stage 里只有 %d 个的 agent 解析得出 —— 过半解析不出说明审计读的"+
			"不是生产那张角色表 (量具坏了), 而不是内置定义全错", total, resolved)
	}
	t.Logf("内置工作流自检: %d 个 stage, %d 个 agent 可解析", total, resolved)
}

// TestAuditBuiltinWorkflowsDoesNotFlagCompanionModes 是本轮修掉的那条**误报**的回归。
//
// ---------------------------------------------------------------------------
// 修之前发生了什么
// ---------------------------------------------------------------------------
//
// 自检起初直接调 ValidateAll, 于是把两条**只对 JSON 定义成立**的闸门搬到了内置上:
//
//   - `mode ∈ dynamicModes` —— 那张表的定义就是"除有伴生硬编码执行器的模式之外"。
//     而内置里恰好有 app_composite / creative_media / game_composite / novel_writing /
//     swarm_novel / trading_debate 这些**有**伴生执行器的模式。
//   - `至少一个 stage` —— graph-extract-swarm / review-panel / plot-predict /
//     plot-simulate 的 stage 活在各自的图模板里。
//
// 结果是内置表里 10 条固定报"硬错" (接近四成), 每次启动都打印, 而且**永远不会变**:
// 它们不是缺陷, 是自检用错了尺子。
//
// ---------------------------------------------------------------------------
// 为什么这条必须是"零硬错"而不是"少于 N 条"
// ---------------------------------------------------------------------------
//
// 一个容差 (≤10) 会把修复与退化成同一个读数: 把闸门改回去会多 10 条, 但只要有人
// 同时再引入 10 条别的硬错, 数就不动了。而"零"在这里是**可断言的**: 内置定义在
// 编译期固定, 没有任何一条会因为运行环境而硬错。有硬错就是自检的尺子有问题。
func TestAuditBuiltinWorkflowsDoesNotFlagCompanionModes(t *testing.T) {
	roles := NewRoleRegistry(t.TempDir())
	issues := AuditBuiltinWorkflows(roles)
	var hard []string
	for _, is := range issues {
		if is.Hard != "" {
			hard = append(hard, is.Name+": "+is.Hard)
		}
	}
	if len(hard) > 0 {
		t.Fatalf("内置定义在编译期固定, 不该有任何硬错 (%d 条)。每一条都是自检用错了尺子 ——\n"+
			"查 validateStagesAndGraph 与 ValidateAll 的分工:\n  %s",
			len(hard), strings.Join(hard, "\n  "))
	}
	// 反向: 那些有伴生执行器的模式**必须在**内置表里 (否则这条用例会因为"表里
	// 恰好没有它们"而变得永远通过 —— 那正是它要挡的那类失效)。
	want := map[string]bool{
		"app_composite": true, "creative_media": true, "game_composite": true,
		"novel_writing": true, "swarm_novel": true, "trading_debate": true,
	}
	seen := map[string]bool{}
	for _, n := range BuiltinWorkflowNames() {
		if def := WorkflowDefFromBuiltin(n); def != nil {
			if want[def.Mode] {
				seen[def.Mode] = true
			}
		}
	}
	if len(seen) != len(want) {
		var missing []string
		for k := range want {
			if !seen[k] {
				missing = append(missing, k)
			}
		}
		t.Fatalf("这条用例的前提没了: 内置表里应含有伴生执行器的模式 %v, 实际只见到 %v",
			missing, seen)
	}
}

// TestAuditBuiltinWorkflowsReportsUncheckedStages 断言"查不到 stage"这件事**被报出来**。
//
// 一部分内置的 stage 在图模板里 (WorkflowDef.Stages 为空), 自检一个 stage 都查不到。
// 它们与"查过了且全对"在输出上必须分得开 —— 否则一份全绿的自检报告既可能是
// "全都查过", 也可能是"一大半根本没查", 而后者会让人对这份报告建立错误的信心。
//
// ★ 这里不断言 unchecked 的**具体数值**: 那个数取决于有几个内置把 stage 放在图模板
// 里, 会随定义演化。断言的是这个数**不小于**"Stages 为 0 的内置个数" —— 由数据算出的
// 期望值, 而不是抄一个会过期的常数。
func TestAuditBuiltinWorkflowsReportsUncheckedStages(t *testing.T) {
	roles := NewRoleRegistry(t.TempDir())
	issues := AuditBuiltinWorkflows(roles)

	wantUnchecked := 0
	staged := 0
	for _, is := range issues {
		if is.Stages == 0 {
			wantUnchecked++
			continue
		}
		staged++
	}
	if staged == 0 {
		t.Fatal("没有一条内置带 stage ⇒ 自检根本没有可检查的对象, 这份报告是空的")
	}
	total, hard, degraded, unchecked := BuiltinAuditSummary(issues)
	if total != len(issues) {
		t.Fatalf("汇总的 total=%d 与结果条数 %d 对不上", total, len(issues))
	}
	if unchecked != wantUnchecked {
		t.Fatalf("汇总的 unchecked=%d, 按每条 Stages==0 数出来是 %d", unchecked, wantUnchecked)
	}
	if hard != 0 {
		t.Fatalf("内置不该有硬错, got %d", hard)
	}
	t.Logf("内置自检: 共 %d 条 (带 stage 的 %d 条), 静默降级 %d 条, 无从检查 %d 条",
		total, staged, degraded, unchecked)
}
