package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// writeTestAgent 在 <cwd>/.claude-go/agents 下写一份 agent 资产。
func writeTestAgent(t *testing.T, cwd, name, frontmatter, body string) {
	t.Helper()
	p := filepath.Join(cwd, ".claude-go", "agents")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("建目录 %s: %v", p, err)
	}
	content := "---\nname: " + name + "\n" + frontmatter + "---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(p, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("写资产 %s: %v", name, err)
	}
}

// TestResolveAgentRunSpec_两层都没命中必须报错 是本条最要紧的判据。
//
// 失效形态: 解析失败时静默回落成"普通 run"。那会让一个拼错的 agent 名与一个正确的
// agent 名从外面**完全不可区分** —— 两种情况都是"跑起来了, 输出了一段文本"。
//
// 变异: 把 resolveAgentRunSpec 末尾的 return err 改成
// `return &agentRunSpec{Name: name, Source: "none"}, nil` ⇒ 本测试必须 FAIL。
func TestResolveAgentRunSpec_两层都没命中必须报错(t *testing.T) {
	cwd := t.TempDir()
	loadAssetCatalog(cwd, "")
	rr := agent.NewRoleRegistryFor(cwd, "")

	_, err := resolveAgentRunSpec("definitely-not-an-agent-xyz", rr, nil)
	if err == nil {
		t.Fatal("名字两层都查不到时必须报错, 实际返回 nil —— 静默回落会让拼错的名字与正确的名字不可区分")
	}
	if !strings.Contains(err.Error(), "definitely-not-an-agent-xyz") {
		t.Fatalf("错误信息里必须含请求的名字, 实际: %v", err)
	}
	// 错误要说明**查过哪两层**, 只报"没找到"会让人以为只有一层可查。
	if !strings.Contains(err.Error(), "资产") || !strings.Contains(err.Error(), "角色") {
		t.Fatalf("错误信息要写清查过哪两层, 实际: %v", err)
	}
}

// TestResolveAgentRunSpec_角色层命中 —— 角色注册表兜底, 且人格正文真的取到。
func TestResolveAgentRunSpec_角色层命中(t *testing.T) {
	cwd := t.TempDir()
	loadAssetCatalog(cwd, "")
	rr := agent.NewRoleRegistryFor(cwd, "")

	spec, err := resolveAgentRunSpec("sentiment-analyst", rr, nil)
	if err != nil {
		t.Fatalf("sentiment-analyst 是内置角色, 应命中: %v", err)
	}
	if spec.Source != "role" {
		t.Fatalf("Source = %q, want \"role\"", spec.Source)
	}
	if !strings.Contains(spec.SystemPrompt, "金融情绪分析专家") {
		t.Fatalf("人格正文没取到, 实际前 120 字: %.120s", spec.SystemPrompt)
	}
	// 角色层不声明工具面 —— 这一点必须如实为空, 否则 applyAgentToolFace 会去动
	// 一个没人声明过的工具面。
	if spec.HasToolProfile || spec.HasTools || spec.HasDisallowedTools {
		t.Fatalf("角色层不该声明工具面, 实际 profile=%v tools=%v disallowed=%v",
			spec.HasToolProfile, spec.HasTools, spec.HasDisallowedTools)
	}
}

// TestResolveAgentRunSpec_资产层优先于角色层 —— 同名时资产赢 (查表顺序的第一条)。
func TestResolveAgentRunSpec_资产层优先于角色层(t *testing.T) {
	cwd := t.TempDir()
	// 用一个**同时也是角色名**的资产名, 才能把"顺序"测出来。
	writeTestAgent(t, cwd, "sentiment-analyst", "max-turns: 3\n", "资产层的人格句 ASSET-WINS")
	loadAssetCatalog(cwd, "")
	rr := agent.NewRoleRegistryFor(cwd, "")

	spec, err := resolveAgentRunSpec("sentiment-analyst", rr, nil)
	if err != nil {
		t.Fatalf("应命中: %v", err)
	}
	if spec.Source != "asset" {
		t.Fatalf("同名时资产层必须优先, 实际 Source=%q", spec.Source)
	}
	if !strings.Contains(spec.SystemPrompt, "ASSET-WINS") {
		t.Fatalf("取到的不是资产正文: %.120s", spec.SystemPrompt)
	}
	if spec.MaxTurns != 3 {
		t.Fatalf("资产的 max-turns 没解析出来: %d", spec.MaxTurns)
	}
}

// TestResolveAgentRunSpec_显式空白名单不能被当成没声明 是本仓最贵的一类缺陷。
//
// `tools: []` 与"根本没写 tools:"解析出来的切片**长度都是 0**。少了 HasTools 这一位,
// 一份 fail-closed 声明会在下游静默退化成完全放行。
//
// 变异: 把 `spec.HasTools = b.HasTools` 改成 `spec.HasTools = len(b.Tools) > 0`
// ⇒ 本测试必须 FAIL。
func TestResolveAgentRunSpec_显式空白名单不能被当成没声明(t *testing.T) {
	cwd := t.TempDir()
	writeTestAgent(t, cwd, "empty-tools", "tools: []\n", "显式空白名单")
	writeTestAgent(t, cwd, "no-tools", "max-turns: 1\n", "没写 tools")
	loadAssetCatalog(cwd, "")
	rr := agent.NewRoleRegistryFor(cwd, "")

	empty, err := resolveAgentRunSpec("empty-tools", rr, nil)
	if err != nil {
		t.Fatalf("empty-tools: %v", err)
	}
	none, err := resolveAgentRunSpec("no-tools", rr, nil)
	if err != nil {
		t.Fatalf("no-tools: %v", err)
	}

	if len(empty.Tools) != 0 || len(none.Tools) != 0 {
		t.Fatalf("两者的 Tools 切片都该是空的 (这正是陷阱)")
	}
	if !empty.HasTools {
		t.Fatal("`tools: []` 是**显式空白名单**, HasTools 必须为 true —— 退化成 false 就是一份 fail-closed 声明被静默翻成完全放行")
	}
	if none.HasTools {
		t.Fatal("没写 tools: 时 HasTools 必须为 false, 否则 applyAgentToolFace 会去清空一个没人声明过的白名单")
	}
}

// TestResolveAgentRunSpec_非法档位整条作废 —— 与 AssetBinding.bindToolProfile 同口径。
//
// NormalizeToolProfile 把不认识的串静默掰成 chat: `tool-profile: codign` 会让 agent
// 连 Read 都没有, 而日志上一个字都没有。所以非法取值必须表现为"没声明"。
func TestResolveAgentRunSpec_非法档位整条作废(t *testing.T) {
	cwd := t.TempDir()
	writeTestAgent(t, cwd, "typo-profile", "tool-profile: codign\n", "拼错的档位")
	loadAssetCatalog(cwd, "")
	rr := agent.NewRoleRegistryFor(cwd, "")

	spec, err := resolveAgentRunSpec("typo-profile", rr, nil)
	if err != nil {
		t.Fatalf("typo-profile: %v", err)
	}
	if spec.HasToolProfile {
		t.Fatalf("非法档位必须整条作废 (HasToolProfile=false), 实际被接受成 %q —— 静默掰成 chat 是一次没人察觉的权限收窄", spec.ToolProfile)
	}
}

// newProbeEngine 造一个带真实工具注册表的引擎, 用来对 applyAgentToolFace 做**真实**
// 调用。自己复刻一遍求交/求差的语义只能证明复刻对, 证明不了被测对象对。
func newProbeEngine(t *testing.T) *engine.QueryEngine {
	t.Helper()
	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg, nil)
	eng := engine.NewQueryEngine(&engine.Config{Cwd: t.TempDir()}, nil, reg, nil, nil, nil, nil)
	if eng == nil {
		t.Fatal("引擎没造出来")
	}
	return eng
}

func hasTool(eng *engine.QueryEngine, name string) bool {
	for _, n := range eng.VisibleTools() {
		if n == name {
			return true
		}
	}
	return false
}

// TestApplyAgentToolFace_显式白名单是 fail-closed —— 只含一个不存在的工具名时,
// 模型必须一个工具都看不见。
func TestApplyAgentToolFace_显式白名单是_failclosed(t *testing.T) {
	eng := newProbeEngine(t)
	// 负对照: 没应用任何声明时必须有工具可见, 否则下面的"全不可见"说明不了任何事。
	if len(eng.VisibleTools()) == 0 {
		t.Fatal("基线就一个工具都看不见 —— 判据恒假, 后面的断言全是空的")
	}

	spec := &agentRunSpec{Name: "t", Source: "asset", HasTools: true, Tools: []string{"NoSuchToolZZZ"}}
	if err := applyAgentToolFace(eng, spec); err != nil {
		t.Fatalf("applyAgentToolFace: %v", err)
	}
	if got := eng.VisibleTools(); len(got) != 0 {
		t.Fatalf("白名单只含不存在的工具名时仍可见 %d 个: %v —— fail-closed 退化了", len(got), firstN(got, 5))
	}
}

// TestApplyAgentToolFace_白名单与既有白名单求交 —— 绝不放宽。
//
// 变异: 把求交那段改成直接 `eng.Config.AllowedTools = declared` ⇒ 本测试必须 FAIL
// (那样 Read 之外的东西会被**放回来**, 而不是被收掉)。
func TestApplyAgentToolFace_白名单与既有白名单求交(t *testing.T) {
	eng := newProbeEngine(t)
	eng.Config.AllowedTools = map[string]bool{"Read": true, "Write": true, "Bash": true}
	if !hasTool(eng, "Bash") || !hasTool(eng, "Write") {
		t.Fatal("前置条件不成立: 既有白名单里该有 Bash/Write")
	}

	spec := &agentRunSpec{Name: "t", Source: "asset", HasTools: true, Tools: []string{"Read"}}
	if err := applyAgentToolFace(eng, spec); err != nil {
		t.Fatalf("applyAgentToolFace: %v", err)
	}
	if !hasTool(eng, "Read") {
		t.Fatal("Read 在两个白名单里都有, 必须仍然可见")
	}
	if hasTool(eng, "Bash") || hasTool(eng, "Write") {
		t.Fatal("agent 声明了更窄的白名单时, 既有的 Bash/Write 必须被收掉 —— 替换成并集是一次静默放宽")
	}
}

// TestApplyAgentToolFace_档位收窄真的落到可见集 (三层递进, 证明判据有分辨力)。
func TestApplyAgentToolFace_档位收窄真的落到可见集(t *testing.T) {
	eng := newProbeEngine(t)
	before := len(eng.VisibleTools())
	if !hasTool(eng, "Bash") || !hasTool(eng, "Write") {
		t.Fatal("前置条件: 无档位时 Bash/Write 该可见")
	}

	spec := &agentRunSpec{Name: "t", Source: "asset",
		HasToolProfile: true, ToolProfile: builtin.ToolProfileAnalysis}
	if err := applyAgentToolFace(eng, spec); err != nil {
		t.Fatalf("applyAgentToolFace: %v", err)
	}
	after := len(eng.VisibleTools())
	if hasTool(eng, "Bash") || hasTool(eng, "Write") {
		t.Fatal("analysis 档必须收掉 Bash/Write")
	}
	if !hasTool(eng, "Read") || !hasTool(eng, "Grep") {
		t.Fatal("analysis 档必须保留只读工具 (Read/Grep) —— 收过头与没收够是两种缺陷")
	}
	// 中间点必须真的窄了, 否则"收窄"这件事没有被量到。
	if after >= before {
		t.Fatalf("档位没让可见集变窄: before=%d after=%d", before, after)
	}
	t.Logf("可见工具: 无档位=%d, analysis 档=%d", before, after)
}

// TestApplyAgentToolFace_无声明时不动工具面 —— 不声明档位不能有任何副作用。
func TestApplyAgentToolFace_无声明时不动工具面(t *testing.T) {
	eng := newProbeEngine(t)
	before := append([]string(nil), eng.VisibleTools()...)

	if err := applyAgentToolFace(eng, &agentRunSpec{Name: "t", Source: "role"}); err != nil {
		t.Fatalf("applyAgentToolFace: %v", err)
	}
	after := eng.VisibleTools()
	if len(before) != len(after) {
		t.Fatalf("没声明任何工具面时可见集不该变: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("可见集变了: %v -> %v", before, after)
		}
	}
}

// TestBuildAgentRunner_声明的回合上限接进既有判定链。
func TestBuildAgentRunner_声明的回合上限接进既有判定链(t *testing.T) {
	spec := &agentRunSpec{Name: "a", Source: "asset", MaxTurns: 7, SystemPrompt: "p"}
	r := buildAgentRunner(nil, spec, modelconfig.ResolvedConfig{}, 150)
	if r.modelCfg.MaxTurns != 7 {
		t.Fatalf("modelCfg.MaxTurns = %d, want 7 —— 声明没接进 cliStageMaxTurns 的 modelTurns 档", r.modelCfg.MaxTurns)
	}
	if got := cliStageMaxTurns("a", r.modelCfg.MaxTurns, 150); got != 7 {
		t.Fatalf("cliStageMaxTurns = %d, want 7", got)
	}
	// 思考型角色的 1 轮短路仍必须在最前面 (既有语义不能被声明顶掉)。
	if got := cliStageMaxTurns("architect", r.modelCfg.MaxTurns, 150); got != 1 {
		t.Fatalf("思考型角色应短路成 1 轮, 实际 %d", got)
	}
}

// TestBuildAgentRunner_收口与Bash合并声明被搬运 钉住两个新开关不会只停在
// agentRunSpec 上: 少搬一层, 角色声明看起来生效、运行时仍是旧行为.
func TestBuildAgentRunner_收口与Bash合并声明被搬运(t *testing.T) {
	spec := &agentRunSpec{Name: "a", Source: "role", FinalizeTurns: 2, MergeBashTargets: true, RestrictWritesToCwd: true, CitationGate: true, NormalizeEvidence: true}
	r := buildAgentRunner(nil, spec, modelconfig.ResolvedConfig{}, 0)
	if r.finalizeTurns != 2 {
		t.Fatalf("finalizeTurns = %d, want 2", r.finalizeTurns)
	}
	if !r.mergeBashTargets {
		t.Fatal("mergeBashTargets = false, want true")
	}
	if !r.restrictWritesToCwd {
		t.Fatal("restrictWritesToCwd = false, want true")
	}
	if !r.citationGate {
		t.Fatal("citationGate = false, want true")
	}
	if !r.normalizeEvidence {
		t.Fatal("normalizeEvidence = false, want true")
	}
}

func TestBuildAgentRunner_显式收口旗标覆盖角色声明(t *testing.T) {
	oldTurns, oldInstruction := flagFinalizeTurns, flagFinalizeInstruction
	t.Cleanup(func() {
		flagFinalizeTurns, flagFinalizeInstruction = oldTurns, oldInstruction
	})
	flagFinalizeTurns = 1
	flagFinalizeInstruction = "  只输出 STRICT_JSON  "
	r := buildAgentRunner(nil, &agentRunSpec{Name: "a", Source: "asset", FinalizeTurns: 4}, modelconfig.ResolvedConfig{}, 0)
	if r.finalizeTurns != 1 || r.finalizeInstruction != "只输出 STRICT_JSON" {
		t.Fatalf("显式收口配置没有进入 runner: turns=%d instruction=%q", r.finalizeTurns, r.finalizeInstruction)
	}
}

// TestBuildAgentRunner_角色层技能不重复并入 —— 角色层的 MergedPrompt 已经拼过正文。
func TestBuildAgentRunner_角色层技能不重复并入(t *testing.T) {
	roleSpec := &agentRunSpec{Name: "r", Source: "role", Skills: []string{"s1"}}
	r := buildAgentRunner(nil, roleSpec, modelconfig.ResolvedConfig{}, 0)
	if len(r.stageDecl.Skills) != 0 {
		t.Fatalf("角色层技能不该再设给 stageDecl (MergedPrompt 已拼过): %v", r.stageDecl.Skills)
	}
	assetSpec := &agentRunSpec{Name: "a", Source: "asset", Skills: []string{"s1"}}
	r2 := buildAgentRunner(nil, assetSpec, modelconfig.ResolvedConfig{}, 0)
	if len(r2.stageDecl.Skills) != 1 {
		t.Fatalf("资产层技能要交给既有渲染路径: %v", r2.stageDecl.Skills)
	}
}

// TestBuildAgentRunner_档位常量有效性 保证 spec.ToolProfile 的取值域与 builtin 一致。
func TestBuildAgentRunner_档位常量有效性(t *testing.T) {
	for _, p := range []builtin.ToolProfile{
		builtin.ToolProfileChat, builtin.ToolProfileAnalysis, builtin.ToolProfileCoding,
	} {
		if !builtin.ValidToolProfile(p) {
			t.Fatalf("%q 应当是合法档位", p)
		}
	}
	if builtin.ValidToolProfile("codign") {
		t.Fatal("拼错的档位必须判为非法 —— 它由 ValidToolProfile 判定后才敢用")
	}
}
