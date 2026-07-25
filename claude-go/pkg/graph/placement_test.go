package graph

// 逐节点放置声明回归 (design/01 §4.9, spec.go PlacementSpec)。
//
// 引擎自己不解释 Placement (调度内核不认识 runtime), 所以这里只能断言两件事,
// 但两件都是真闸:
//   - **形状校验 fail-closed**: 非法 prefer 必须被拒而不是当成 any。写错的
//     prefer="remote" (漏冒号与名字) 若被当成 any, 症状是节点随机落在任意机器上
//     却毫无报错, 而作者以为自己钉住了它;
//   - **分片继承**: map 节点声明一次, N 个分片全都带着约束走 —— 否则"每个渲染
//     分片都要 browser"要在图里写 N 遍 (而分片数是运行期才知道的)。
//
// 声明真正生效 (硬约束挡住无能力 runtime) 的端到端断言在 pkg/worker/placement_node_test.go。

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestValidate_放置约束形状(t *testing.T) {
	mk := func(p *PlacementSpec) GraphSpec {
		n := vNode("a")
		n.Agent.Placement = p
		return GraphSpec{Name: "g", Nodes: []NodeSpec{n}}
	}
	ok := []*PlacementSpec{
		nil,
		{},
		{Require: []string{"browser"}},
		{Prefer: PlacementPreferLocal},
		{Prefer: PlacementPreferAny},
		{Prefer: "remote:render-pool"},
		{Affinity: PlacementAffinityTeam},
		{Affinity: PlacementAffinityTeam, AffinityKey: "trading-v2"},
		{Require: []string{"browser", "bash"}, Prefer: "remote:render-1", Affinity: "team"},
	}
	for i, p := range ok {
		if err := mk(p).Validate(); err != nil {
			t.Errorf("合法声明 #%d (%+v) 被拒: %v", i, p, err)
		}
	}
	bad := []struct {
		p    *PlacementSpec
		want string
	}{
		{&PlacementSpec{Prefer: "remote"}, "非法"},              // 漏冒号: 最容易写错的一档
		{&PlacementSpec{Prefer: "remote:  "}, "缺少 runtime 名"}, // 有冒号没名字
		{&PlacementSpec{Prefer: "LOCAL"}, "非法"},               // 大小写不宽容: 放置是治理侧
		{&PlacementSpec{Prefer: "任意"}, "非法"},
		{&PlacementSpec{Require: []string{"browser", " "}}, "require"}, // 空标签对 Has 恒真 = 空约束
		{&PlacementSpec{Affinity: "run"}, "affinity"},
		{&PlacementSpec{AffinityKey: "t1"}, "没声明 affinity"}, // 死配置
	}
	for _, c := range bad {
		err := mk(c.p).Validate()
		if err == nil {
			t.Errorf("非法声明 %+v 未被拒", c.p)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("声明 %+v 的报错应含 %q, 实得: %v", c.p, c.want, err)
		}
	}
}

// 组内成员与展开产物也走同一套形态校验 (validateNodeShape 是唯一入口)。
func TestValidate_组内成员的放置同样被校验(t *testing.T) {
	g := advGroup(2, "")
	g.Group.Nodes[0].Agent.Placement = &PlacementSpec{Prefer: "remote:"}
	err := GraphSpec{Name: "g", Nodes: []NodeSpec{g}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "缺少 runtime 名") {
		t.Errorf("组内成员的非法放置应被拒, 实得 %v", err)
	}
}

// map 分片继承 map 节点的放置声明: 声明一次覆盖全部分片。
// (shardNodeSpec 整份复制 Agent —— 这里把它锁死, 否则某天有人为了"分片不该继承
// Expand" 顺手清掉整个 Agent, 放置就会静默消失。)
func TestPlacement_分片继承map节点的声明(t *testing.T) {
	want := &PlacementSpec{Require: []string{"browser"}, Prefer: "remote:render-pool"}
	m := newMapNode("m", MapPolicy{MaxShards: 3})
	m.Agent.Placement = want

	stub := newStub()
	stub.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "一\n二\n三"}
	}
	capture := &placementCapture{seen: map[string]*PlacementSpec{}, inner: stub}
	if _, err := fastEngine(capture, NewMemoryJournal()).Run(context.Background(),
		mapSpec(m, nil), RunOpts{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		id := shardNodeID("m", i)
		got := capture.get(id)
		if got == nil {
			t.Fatalf("分片 %s 没拿到放置声明", id)
		}
		if len(got.Require) != 1 || got.Require[0] != "browser" || got.Prefer != want.Prefer {
			t.Errorf("分片 %s 的放置声明 = %+v, 期望与 map 节点一致", id, got)
		}
	}
}

// placementCapture 记录每个节点执行时看到的 Placement 声明。
// 分片是并发跑的, 记录必须加锁 (-race 会抓)。
type placementCapture struct {
	mu    sync.Mutex
	seen  map[string]*PlacementSpec
	inner NodeRunner
}

func (c *placementCapture) RunNode(ctx context.Context, node NodeSpec, in NodeInput) NodeResult {
	c.mu.Lock()
	c.seen[node.ID] = node.Agent.Placement
	c.mu.Unlock()
	return c.inner.RunNode(ctx, node, in)
}

func (c *placementCapture) get(id string) *PlacementSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[id]
}

// 单调收窄 (§4.2): 展开产物的放置约束只能更严, 不能更松。
//
// 为什么这条必须锁死: 展开内容来自 LLM 产出。父节点声明 require:["browser"] 的
// 意思是"这一支任务必须在有浏览器的机器上跑", 若模型给出的子节点漏写 require,
// 子节点就会落到没有浏览器的机器上产出假货 —— 而图上"约束只收窄"这条不变量
// 看起来还成立 (子节点确实没有比父节点更宽的**声明**)。
func TestPlacement_展开产物只能收窄(t *testing.T) {
	parent := &PlacementSpec{Require: []string{"browser"}, Prefer: "remote:render-pool", Affinity: "team"}
	cases := []struct {
		name  string
		child *PlacementSpec
		want  []string
	}{
		{name: "子节点未声明: 整份继承父约束", child: nil, want: []string{"browser"}},
		{name: "子节点漏写硬约束: 补回父的", child: &PlacementSpec{}, want: []string{"browser"}},
		{name: "子节点另加一条: 取并集", child: &PlacementSpec{Require: []string{"gpu"}},
			want: []string{"browser", "gpu"}},
		{name: "子节点重复声明: 去重", child: &PlacementSpec{Require: []string{"browser", "gpu"}},
			want: []string{"browser", "gpu"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NodeSpec{ID: "p", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w", Placement: parent}}
			ch := NodeSpec{ID: "c", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w", Placement: c.child}}
			got := narrowToParent(p, ch, 1, 1).Agent.Placement
			if got == nil {
				t.Fatal("父节点声明了放置, 子节点不该是 nil")
			}
			if len(got.Require) != len(c.want) {
				t.Fatalf("Require = %v, 期望 %v", got.Require, c.want)
			}
			for i, w := range c.want {
				if got.Require[i] != w {
					t.Errorf("Require[%d] = %q, 期望 %q (顺序须确定性: 父的先入)", i, got.Require[i], w)
				}
			}
			// 偏好与亲和被父节点强制继承: 换机器池 = 换能力集, 不能由模型决定。
			if got.Prefer != parent.Prefer || got.Affinity != parent.Affinity {
				t.Errorf("prefer/affinity 未强制继承父节点: %+v", got)
			}
		})
	}
	// 子节点想换机器池: 被压回父节点的声明。
	ch := NodeSpec{ID: "c", Kind: NodeKindAgent,
		Agent: AgentSpec{Role: "w", Placement: &PlacementSpec{Prefer: "local"}}}
	p := NodeSpec{ID: "p", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w", Placement: parent}}
	if got := narrowToParent(p, ch, 1, 1).Agent.Placement; got.Prefer != parent.Prefer {
		t.Errorf("子节点改 prefer 应被压回父值, 实得 %q", got.Prefer)
	}
	// 父节点未声明放置时不干预子节点 (父无约束就没有"收窄"可言)。
	p2 := NodeSpec{ID: "p", Kind: NodeKindAgent, Agent: AgentSpec{Role: "w"}}
	if got := narrowToParent(p2, ch, 1, 1).Agent.Placement; got == nil || got.Prefer != "local" {
		t.Errorf("父未声明时子声明应原样保留, 实得 %+v", got)
	}
	// 不得就地改父/子的声明 (图规格被多个 goroutine 共享读)。
	if len(parent.Require) != 1 || parent.Require[0] != "browser" {
		t.Errorf("父节点声明被就地修改了: %+v", parent)
	}
}
