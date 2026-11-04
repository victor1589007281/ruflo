// toolext_wiring_test.go — R-H9 在 Runner 侧的**执行**验收。
//
// toolext_test.go 验的是纯函数（ApplyCompanionProfile 的求交规则、TruncateResult
// 的 rune 边界）。那些都是"写对了"，但 R-H9 的验收原话是"未授权工具不会被执行"
// —— 那是**执行**断言。一个把档位算对、却从没把结果交给引擎的实现，能让所有
// 纯函数测试变绿而工具照常跑。
//
// 所以这里的判据一律是"工具被调用了几次"（复用 owner_wiring_test.go 的
// ownerCaptureTool 计数），而不是"名单长什么样"。
package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// ---------------------------------------------------------------------------
// 档位归一（纯函数层）
// ---------------------------------------------------------------------------

// TestApplyToolProfile_EmptyLeavesPolicyUntouched 钉住"零值 = 旧行为"。
func TestApplyToolProfile_EmptyLeavesPolicyUntouched(t *testing.T) {
	p := TurnPolicy{}
	if err := applyToolProfile(&p); err != nil {
		t.Fatalf("空档位不应报错: %v", err)
	}
	if p.AllowedTools != nil {
		t.Fatalf("空档位不得改动白名单, 实得 %v", *p.AllowedTools)
	}
}

// TestApplyToolProfile_WhitespaceOnlyLeavesPolicyUntouched 钉住纯空白等于未设。
func TestApplyToolProfile_WhitespaceOnlyLeavesPolicyUntouched(t *testing.T) {
	p := TurnPolicy{ToolProfile: "   \t"}
	if err := applyToolProfile(&p); err != nil {
		t.Fatalf("纯空白档位应等同未设: %v", err)
	}
	if p.AllowedTools != nil {
		t.Fatalf("纯空白档位不得改动白名单, 实得 %v", p.AllowedTools)
	}
}

// TestApplyToolProfile_CompanionNarrowsNilToFullSet 是"从不受限收窄"的主路径。
func TestApplyToolProfile_CompanionNarrowsNilToFullSet(t *testing.T) {
	p := TurnPolicy{ToolProfile: ToolProfileCompanion}
	if err := applyToolProfile(&p); err != nil {
		t.Fatalf("companion 档位: %v", err)
	}
	if p.AllowedTools == nil {
		t.Fatal("companion 档位必须产出非 nil 白名单 (nil 的语义是不受限)")
	}
	got := *p.AllowedTools
	want := CompanionAllowedTools()
	if len(got) != len(want) {
		t.Fatalf("陪伴集合长度不符: got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("陪伴集合内容/顺序不符: got %v want %v", got, want)
		}
	}
	// 危险工具必须在档位外 —— 这是档位存在的全部意义。
	for _, danger := range []string{"Bash", "Write", "Edit", "exec_sql"} {
		for _, n := range got {
			if n == danger {
				t.Fatalf("危险工具 %q 出现在陪伴档位里", danger)
			}
		}
	}
}

// TestApplyToolProfile_CompanionIntersectsExisting 钉住"档位只能收窄"。
func TestApplyToolProfile_CompanionIntersectsExisting(t *testing.T) {
	allowed := []string{"Bash", "Read", "memory_search"}
	p := TurnPolicy{ToolProfile: ToolProfileCompanion, AllowedTools: &allowed}
	if err := applyToolProfile(&p); err != nil {
		t.Fatalf("companion 档位求交: %v", err)
	}
	got := *p.AllowedTools
	for _, n := range got {
		if n == "Bash" {
			t.Fatal("Bash 借档位求交被放回来了 —— 档位不能放宽")
		}
	}
	if len(got) != 2 {
		t.Fatalf("求交结果应为 Read + memory_search, 实得 %v", got)
	}
}

// TestApplyToolProfile_UnknownProfileIsError 守住"拼错必须拒绝"。
//
// 静默当成"不限档位"是这里唯一危险的失效方向: 一次拼写错误就变成安全边界消失,
// 而调用方以为自己开了档位、日志里也看不出异常。
func TestApplyToolProfile_UnknownProfileIsError(t *testing.T) {
	for _, name := range []string{"companinon", "companion2", "strict"} {
		p := TurnPolicy{ToolProfile: name}
		if err := applyToolProfile(&p); err == nil {
			t.Fatalf("未知档位 %q 必须报错, 而不是静默不限", name)
		}
	}
}

// TestApplyToolProfile_CaseAndSpaceInsensitive 覆盖归一化。
//
// "  Companion " 是同一个档位: 调用方从配置文件/HTTP 头读取值时带空白与大小写
// 差异是常态, 为此拒绝请求属于误伤。**但**它仍然只能收窄。
func TestApplyToolProfile_CaseAndSpaceInsensitive(t *testing.T) {
	p := TurnPolicy{ToolProfile: "  Companion "}
	if err := applyToolProfile(&p); err != nil {
		t.Fatalf("大小写/空白差异应被归一: %v", err)
	}
	if p.AllowedTools == nil || len(*p.AllowedTools) != len(CompanionAllowedTools()) {
		t.Fatalf("归一后应收窄到陪伴集合, 实得 %v", p.AllowedTools)
	}
}

// ---------------------------------------------------------------------------
// 预算：MaxRunTokens（R-H10 第五维）
// ---------------------------------------------------------------------------

func gateForPolicy(policy TurnPolicy) *budgetGate {
	return &budgetGate{
		maxRunTokens: policy.MaxRunTokens,
		maxToolCalls: policy.MaxToolCalls,
		maxCost:      policy.MaxCost,
	}
}

// TestBudgetGate_MaxRunTokensTrips 钉住 token 维度真的会触发。
//
// 口径是输入+输出+缓存读写四类之和。只算输入的实现会让"靠缓存堆上下文"的 run
// 永远打不到上限 —— 而那种 run 恰恰是成本最不可控的一类。
func TestBudgetGate_MaxRunTokensTrips(t *testing.T) {
	g := gateForPolicy(TurnPolicy{MaxRunTokens: 100})
	if g.observeUsage(types.Usage{InputTokens: 40, OutputTokens: 20}) {
		t.Fatal("60 tokens 不应触发 100 的上限")
	}
	if got := g.spentTokens(); got != 60 {
		t.Fatalf("累计 token 应为 60, 实得 %d", got)
	}
	// 缓存读写也要计入。
	if !g.observeUsage(types.Usage{CacheReadInputTokens: 30, CacheCreationInputTokens: 20}) {
		t.Fatal("累计 110 > 100 应触发上限")
	}
	if g.exhaustedReason() != "run_tokens" {
		t.Fatalf("耗尽维度应为 run_tokens, 实得 %q", g.exhaustedReason())
	}
}

// TestBudgetGate_TokenLimitIndependentOfPrice 守住"两个配置项不互相否决"。
//
// 一个没装价目表的部署仍然应该有 token 上限。把 token 检查写在 price==nil 的
// 早退之后，会让"没配价格"顺带关掉"token 止损" —— 两个不相干的配置项互锁，
// 而表现是"配了 MaxRunTokens 但完全不生效"。
func TestBudgetGate_TokenLimitIndependentOfPrice(t *testing.T) {
	g := &budgetGate{maxRunTokens: 10} // price == nil, maxCost == 0
	if !g.observeUsage(types.Usage{InputTokens: 11}) {
		t.Fatal("没有价目表时 token 上限仍须生效")
	}
	if g.exhaustedReason() != "run_tokens" {
		t.Fatalf("耗尽维度应为 run_tokens, 实得 %q", g.exhaustedReason())
	}
}

// TestBudgetGate_ZeroMaxRunTokensCountsNothing 钉住零值零开销。
func TestBudgetGate_ZeroMaxRunTokensCountsNothing(t *testing.T) {
	g := gateForPolicy(TurnPolicy{})
	if g.observeUsage(types.Usage{InputTokens: 1 << 30}) {
		t.Fatal("未设 token 上限时不应触发")
	}
	if got := g.spentTokens(); got != 0 {
		t.Fatalf("未设上限时不应累加, 实得 %d", got)
	}
}

// TestBudgetGate_FirstTripWins 是归因稳定性。
//
// 多个维度同时超限时，终态理由必须是**第一个**触发的那个：覆盖会让同一个 bug
// 每次复现出不同理由，而理由正是排查入口。
func TestBudgetGate_FirstTripWins(t *testing.T) {
	g := gateForPolicy(TurnPolicy{MaxRunTokens: 5, MaxToolCalls: 1})
	g.observeUsage(types.Usage{InputTokens: 100}) // 先触发 run_tokens
	g.observeToolCall()
	g.observeToolCall()
	if g.exhaustedReason() != "run_tokens" {
		t.Fatalf("首个耗尽维度应被保留, 实得 %q", g.exhaustedReason())
	}
}

// ---------------------------------------------------------------------------
// RunTurn 端到端：白名单真的拦住了工具
// ---------------------------------------------------------------------------

// TestRunTurn_AllowedToolsBlocksToolExecution 是 R-H9"未授权工具不会被执行"的
// 端到端断言。上游照常返回 noop_tool 的 tool_use，但本轮白名单是空集 ——
// 工具必须一次都没跑。
func TestRunTurn_AllowedToolsBlocksToolExecution(t *testing.T) {
	up := &runnerUpstream{toolTurns: 1}
	r := newTestRunner(t, up, 2, nil)
	cap := swapTool(r)

	empty := []string{}
	req := textTurn("s1", "t1", "hi")
	req.Policy.AllowedTools = &empty

	if _, err := r.RunTurn(context.Background(), req, nil); err != nil {
		t.Fatalf("RunTurn 不应因白名单拒绝而报错 (拒绝是 tool_result 而不是 turn 失败): %v", err)
	}
	if got := len(cap.snapshot()); got != 0 {
		t.Fatalf("白名单为空集时工具不应执行, 实得 %d 次调用", got)
	}
	// 协议完整性: 被拒的 tool_use 必须有一条 error tool_result 回注，
	// 否则这段历史之后再也发不出去（tool_use 没有配对的 tool_result）。
	// 判据是"第二次请求体里带着拒绝文案" —— 那证明拒绝结果进了历史。
	body := up.lastBody()
	if !strings.Contains(body, "不在本轮允许的工具清单内") {
		t.Fatalf("拒绝结果未回注到后续请求 (最后请求体 %d 字节, 上游被调用 %d 次)",
			len(body), up.count())
	}
}

// TestRunTurn_AllowedToolsPermitsListedTool 是负对照。
//
// 没有它，"永远拒绝"的实现会让上面那条通过 —— 那时所有工具都不工作，测试全绿。
func TestRunTurn_AllowedToolsPermitsListedTool(t *testing.T) {
	up := &runnerUpstream{toolTurns: 1}
	r := newTestRunner(t, up, 2, nil)
	cap := swapTool(r)

	list := []string{"noop_tool"}
	req := textTurn("s1", "t1", "hi")
	req.Policy.AllowedTools = &list

	if _, err := r.RunTurn(context.Background(), req, nil); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Fatalf("白名单内的工具应执行一次, 实得 %d 次", got)
	}
	if body := up.lastBody(); strings.Contains(body, "不在本轮允许的工具清单内") {
		t.Fatalf("白名单内的工具不应被拒, 但请求体里有拒绝文案")
	}
}

// TestRunTurn_CompanionProfileBlocksDangerousTool 端到端钉住档位。
//
// noop_tool 不是陪伴档位里的工具，所以陪伴档位下它必须一次都不执行。
// 这条与 TestApplyToolProfile_* 不重复：那几条只看名单，这条看执行。
func TestRunTurn_CompanionProfileBlocksDangerousTool(t *testing.T) {
	up := &runnerUpstream{toolTurns: 1}
	r := newTestRunner(t, up, 2, nil)
	cap := swapTool(r)

	req := textTurn("s1", "t1", "hi")
	req.Policy.ToolProfile = ToolProfileCompanion

	if _, err := r.RunTurn(context.Background(), req, nil); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got := len(cap.snapshot()); got != 0 {
		t.Fatalf("陪伴档位下 noop_tool 不应执行, 实得 %d 次", got)
	}
}

// TestRunTurn_UnknownProfileFailsClosed 端到端钉住拼错拒绝。
//
// 判据三条一起看：返回 error、工具零执行、终态带错误。只看"返回了 error"不够
// —— 一个在跑完工具之后才报错的实现也会满足它。
func TestRunTurn_UnknownProfileFailsClosed(t *testing.T) {
	up := &runnerUpstream{toolTurns: 1}
	r := newTestRunner(t, up, 2, nil)
	cap := swapTool(r)

	req := textTurn("s1", "t1", "hi")
	req.Policy.ToolProfile = "compainion" // 拼错
	res, err := r.RunTurn(context.Background(), req, nil)
	if err == nil {
		t.Fatal("未知档位必须返回 error")
	}
	if got := len(cap.snapshot()); got != 0 {
		t.Fatalf("未知档位下不应执行任何工具, 实得 %d 次", got)
	}
	if res == nil || res.Terminal.Error == nil {
		t.Fatalf("未知档位的结果应带终态错误: %+v", res)
	}
	if up.count() != 0 {
		t.Fatalf("未知档位必须在发请求之前就拒绝, 上游却被调用了 %d 次", up.count())
	}
}
