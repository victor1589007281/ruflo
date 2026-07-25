package agent

import (
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/graph"
)

// gi 造一个 n 节点的线性图。
func giSpec(n int) graph.GraphSpec {
	s := graph.GraphSpec{Name: "gi"}
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		s.Nodes = append(s.Nodes, graph.NodeSpec{ID: id, Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "worker"}})
	}
	return s
}

// 生产链必须真装上东西 —— 空链就是"建成未通电", 本仓反复吃过这个亏。
func TestGraphInterceptors_生产链非空(t *testing.T) {
	t.Setenv("CLAUDE_GO_GRAPH_INTERCEPTORS", "")
	ics := graphInterceptors(nil, nil, giSpec(3))
	if len(ics) == 0 {
		t.Fatal("生产装配返回空链 —— 拦截器等于没通电")
	}
	if ics[0].Name() != "budget" {
		t.Errorf("链首 = %q, 期望 budget 在最外层 (被预算拒掉的节点不该被别的拦截器记账)", ics[0].Name())
	}
}

// 开关可关: 出问题时要能一键退回改造前行为。
func TestGraphInterceptors_开关(t *testing.T) {
	for _, v := range []string{"off", "none", "OFF"} {
		t.Setenv("CLAUDE_GO_GRAPH_INTERCEPTORS", v)
		if ics := graphInterceptors(nil, nil, giSpec(3)); len(ics) != 0 {
			t.Errorf("%s 应关闭全部拦截器, 实得 %d 个", v, len(ics))
		}
	}
	t.Setenv("CLAUDE_GO_GRAPH_INTERCEPTORS", "metrics") // 只开一个不存在的
	if ics := graphInterceptors(nil, nil, giSpec(3)); len(ics) != 0 {
		t.Errorf("未点名 budget 时不该装它, 实得 %d 个", len(ics))
	}
}

// 关键安全性质: 算出来的上界必须**高于任何合法运行**的实际执行次数,
// 否则默认打开预算就会掐死正常工作流 —— 那是生产事故。
func TestNodeRunCeiling_合法运行撞不到(t *testing.T) {
	cases := []struct {
		name string
		spec graph.GraphSpec
		// legit 一次合法运行最多的真实执行次数
		legit int
	}{
		{"纯线性3节点", giSpec(3), 3},
		{
			"图级默认重试6",
			func() graph.GraphSpec {
				s := giSpec(4)
				s.Policies.DefaultRetry = &graph.RetryPolicy{MaxRetries: 6}
				return s
			}(),
			4 * 7,
		},
		{
			"节点级循环5轮",
			func() graph.GraphSpec {
				s := giSpec(2)
				s.Nodes[1].Loop = &graph.LoopPolicy{MaxIterations: 5}
				return s
			}(),
			1 + 5,
		},
		{
			"重试与循环叠加",
			func() graph.GraphSpec {
				s := giSpec(3)
				s.Policies.DefaultRetry = &graph.RetryPolicy{MaxRetries: 2}
				s.Nodes[2].Loop = &graph.LoopPolicy{MaxIterations: 4}
				return s
			}(),
			3 * 3 * 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeRunCeiling(tc.spec)
			if got < tc.legit {
				t.Fatalf("上界 %d < 合法运行上限 %d —— 默认预算会掐死正常工作流", got, tc.legit)
			}
			// 也不该宽到失去意义 (失控运行撞不上就等于没有闸)
			if got > tc.legit*8 {
				t.Errorf("上界 %d 比合法上限 %d 宽出 8 倍以上, 闸形同虚设", got, tc.legit)
			}
		})
	}
}

// 声明了 map/展开/组循环的图, 运行期会长大, 上界必须按总量闸算。
func TestNodeRunCeiling_动态长大按总量闸(t *testing.T) {
	s := giSpec(2)
	s.Nodes[1].Map = &graph.MapPolicy{Source: "prev", Split: graph.SplitLines, MaxShards: 50}
	got := nodeRunCeiling(s)
	if got < 50 {
		t.Errorf("声明 map 扇出 50 片, 上界 = %d, 必须 ≥ 分片数否则扇出中途被掐", got)
	}

	s2 := giSpec(2)
	s2.Policies.MaxTotalNodes = 30
	s2.Nodes[1].Expand = &graph.ExpandSpec{MaxNodes: 5}
	if got := nodeRunCeiling(s2); got < 30 {
		t.Errorf("声明动态展开且总量闸 30, 上界 = %d, 必须 ≥ 30", got)
	}
}

// 环境变量能覆盖算出来的默认值 (真要设紧预算的出口)。
func TestBudgetFromEnv_环境变量覆盖(t *testing.T) {
	spec := giSpec(3)
	t.Setenv("CLAUDE_GO_GRAPH_BUDGET_NODE_RUNS", "7")
	t.Setenv("CLAUDE_GO_GRAPH_BUDGET_WALLCLOCK", "45m")
	t.Setenv("CLAUDE_GO_GRAPH_BUDGET_TOKENS", "120000")
	t.Setenv("CLAUDE_GO_GRAPH_BUDGET_ON_EXCEED", "skip")
	b := budgetFromEnv(spec)
	if b.MaxNodeRuns != 7 {
		t.Errorf("MaxNodeRuns = %d, 期望 7", b.MaxNodeRuns)
	}
	if b.MaxWallClock.Minutes() != 45 {
		t.Errorf("MaxWallClock = %v, 期望 45m", b.MaxWallClock)
	}
	if b.MaxTokens != 120000 {
		t.Errorf("MaxTokens = %d, 期望 120000", b.MaxTokens)
	}
	if b.OnExceed != "skip" {
		t.Errorf("OnExceed = %q, 期望 skip", b.OnExceed)
	}
}

// 非法环境变量值必须被忽略而不是变成 0 —— 0 会被当"不限制"还算好,
// 若被当成"上限 0"就是把整图拒死。
func TestBudgetFromEnv_非法值不致拒死(t *testing.T) {
	spec := giSpec(3)
	for _, bad := range []string{"abc", "-5", "0", ""} {
		t.Setenv("CLAUDE_GO_GRAPH_BUDGET_NODE_RUNS", bad)
		b := budgetFromEnv(spec)
		if b.MaxNodeRuns < len(spec.Nodes) {
			t.Errorf("NODE_RUNS=%q 时 MaxNodeRuns=%d, 低于节点数会把正常图拒死", bad, b.MaxNodeRuns)
		}
	}
	t.Setenv("CLAUDE_GO_GRAPH_BUDGET_NODE_RUNS", "")
	for _, bad := range []string{"abc", "1x", "-3s"} {
		t.Setenv("CLAUDE_GO_GRAPH_BUDGET_WALLCLOCK", bad)
		if b := budgetFromEnv(spec); b.MaxWallClock != 0 {
			t.Errorf("WALLCLOCK=%q 应被忽略, 实得 %v", bad, b.MaxWallClock)
		}
	}
}

// 空图不该算出 0 上界后被当成"限制为 0"。
func TestNodeRunCeiling_空图返回不限制(t *testing.T) {
	if got := nodeRunCeiling(graph.GraphSpec{Name: "empty"}); got != 0 {
		t.Errorf("空图上界 = %d, 期望 0(=不限制)", got)
	}
}

// 上界溢出时必须退化为"不限制", 绝不能算出负数把图拒死。
func TestNodeRunCeiling_溢出退化为不限制(t *testing.T) {
	s := giSpec(3)
	s.Policies.DefaultRetry = &graph.RetryPolicy{MaxRetries: 1 << 40}
	s.Nodes[1].Loop = &graph.LoopPolicy{MaxIterations: 1 << 40}
	if got := nodeRunCeiling(s); got < 0 {
		t.Errorf("溢出算出负数 %d —— 会把整图拒死", got)
	}
}

// 装配出的预算拦截器名字要稳定 (它是 journal 归因与开关的键)。
func TestGraphInterceptors_名字稳定(t *testing.T) {
	t.Setenv("CLAUDE_GO_GRAPH_INTERCEPTORS", "")
	ics := graphInterceptors(nil, nil, giSpec(2))
	var names []string
	for _, ic := range ics {
		names = append(names, ic.Name())
	}
	if strings.Join(names, ",") != "budget" {
		t.Errorf("链构成 = %v, 期望恰好 [budget]", names)
	}
}
