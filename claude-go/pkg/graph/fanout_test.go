package graph

// map/reduce 行为回归 (design/01 §4.2, §5 "fanout 首次真正实现")。
// 断言的是**行为**而不是构造出来的对象: 分片真并发、分片 ID 真独立、reduce 真等齐、
// 重试真落在分片级、resume 后真不重复展开。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// —— 测试辅助 ——

func findEvents(t *testing.T, j Journal, typ string) []Event {
	t.Helper()
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var out []Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// evInt 取事件 Data 里的整数 (MemoryJournal 存 int, FileJournal 经 JSON 往返成 float64)。
func evInt(t *testing.T, data map[string]any, key string) int {
	t.Helper()
	switch v := data[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		t.Fatalf("事件 Data[%q] = %#v, 不是数字", key, data[key])
		return 0
	}
}

// mapSpec 上游 src 产出 3 行 → map 节点 m 扇出 → (可选) reduce 节点。
func mapSpec(mapNode NodeSpec, withReduce *NodeSpec) GraphSpec {
	g := GraphSpec{
		Name:  "fanout",
		Nodes: []NodeSpec{vNode("src"), mapNode},
		Edges: []EdgeSpec{{From: "src", To: mapNode.ID}},
	}
	if withReduce != nil {
		g.Nodes = append(g.Nodes, *withReduce)
		g.Edges = append(g.Edges, EdgeSpec{From: mapNode.ID, To: withReduce.ID})
	}
	return g
}

func newMapNode(id string, pol MapPolicy) NodeSpec {
	return NodeSpec{ID: id, Kind: NodeKindMap, Agent: AgentSpec{Role: "worker"}, Map: &pol}
}

// ---------------------------------------------------------------------------
// 1. 分片真并发 + 分片 ID / ShardInput 正确 + journal 记 map.expanded
// ---------------------------------------------------------------------------

func TestMapFanoutRunsShardsConcurrently(t *testing.T) {
	st := newStub()
	st.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "任务一\n任务二\n任务三"}
	}
	// 三个分片互相等待对方进场: 若引擎串行跑分片, 3s 后全部超时失败。
	var mu sync.Mutex
	entered := 0
	all := make(chan struct{})
	shardFn := func(_ int, in NodeInput) NodeResult {
		mu.Lock()
		entered++
		if entered == 3 {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
			return NodeResult{Status: NodeStatusCompleted,
				Output: fmt.Sprintf("片%d:%s", in.Shard.Index, in.Shard.Value), Score: float64(70 + in.Shard.Index)}
		case <-time.After(3 * time.Second):
			return NodeResult{Status: NodeStatusFailed, Err: "分片未并发"}
		}
	}
	for i := 0; i < 3; i++ {
		st.fn[shardNodeID("m", i)] = shardFn
	}

	j := NewMemoryJournal()
	e := fastEngine(st, j)
	spec := mapSpec(newMapNode("m", MapPolicy{MaxShards: 8}), nil)
	res, err := e.Run(context.Background(), spec, RunOpts{Objective: "目标"})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q (分片可能未并发), nodes=%+v", res.Status, res.Nodes)
	}

	// 分片各调一次, 且 ID 形如 <node>#<i>
	for i := 0; i < 3; i++ {
		id := shardNodeID("m", i)
		if id != "m#"+string(rune('0'+i)) {
			t.Fatalf("分片 ID = %q, 期望 m#%d", id, i)
		}
		if st.callCount(id) != 1 {
			t.Fatalf("分片 %s 调用次数 = %d, 期望 1", id, st.callCount(id))
		}
		in := st.input(id, 0)
		if in.Shard == nil {
			t.Fatalf("分片 %s 的 NodeInput.Shard 为 nil", id)
		}
		if in.Shard.Index != i || in.Shard.Total != 3 || in.Shard.MapID != "m" || in.Shard.NodeID != id {
			t.Fatalf("分片 %s 的 ShardInput = %+v, 期望 Index=%d Total=3 MapID=m", id, *in.Shard, i)
		}
		if want := []string{"任务一", "任务二", "任务三"}[i]; in.Shard.Value != want {
			t.Fatalf("分片 %s 的 Value = %q, 期望 %q", id, in.Shard.Value, want)
		}
		// 上游产出照样下发 (分片是同构子任务, 不是被剥光的孤儿)
		if in.PrevOutputs["src"] == "" || in.Objective != "目标" {
			t.Fatalf("分片 %s 未继承上游产出/目标: %+v", id, in)
		}
	}
	// map 节点自己**不**直接调 runner (它是容器)
	if st.callCount("m") != 0 {
		t.Fatalf("map 节点本体被当普通节点执行了 %d 次", st.callCount("m"))
	}
	// 汇总: 三片产出都在, 评分取均值
	mr := res.Nodes["m"]
	if mr.Status != NodeStatusCompleted || len(mr.Shards) != 3 {
		t.Fatalf("map 结果 = %+v, 期望 completed + 3 个分片", mr)
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(mr.Output, fmt.Sprintf("片%d", i)) {
			t.Errorf("map Output 缺分片 %d 的产出: %q", i, mr.Output)
		}
	}
	if mr.Score != 71 { // (70+71+72)/3
		t.Errorf("map Score = %v, 期望分片评分均值 71", mr.Score)
	}

	// journal: 一条 map.expanded, count=3, 且分片各有自己的 node.completed
	exp := findEvents(t, j, EvMapExpanded)
	if len(exp) != 1 {
		t.Fatalf("map.expanded 事件数 = %d, 期望 1", len(exp))
	}
	if exp[0].NodeID != "m" || evInt(t, exp[0].Data, "count") != 3 {
		t.Errorf("map.expanded = %+v, 期望 NodeID=m count=3", exp[0])
	}
	if ids, _ := exp[0].Data["ids"].(string); ids != "m#0,m#1,m#2" {
		t.Errorf("map.expanded.ids = %q, 期望 m#0,m#1,m#2", ids)
	}
	completed := map[string]bool{}
	for _, ev := range findEvents(t, j, EvNodeCompleted) {
		completed[ev.NodeID] = true
	}
	for i := 0; i < 3; i++ {
		if !completed[shardNodeID("m", i)] {
			t.Errorf("journal 缺分片 %s 的 node.completed (无法按分片归因)", shardNodeID("m", i))
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 分片并发受 MaxParallel 约束
// ---------------------------------------------------------------------------

func TestMapShardConcurrencyBoundedByMaxParallel(t *testing.T) {
	st := newStub()
	st.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "a\nb\nc\nd\ne\nf"}
	}
	var (
		mu   sync.Mutex
		cur  int
		peak int
	)
	shardFn := func(int, NodeInput) NodeResult {
		mu.Lock()
		cur++
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // 给并发一个真实的重叠窗口
		mu.Lock()
		cur--
		mu.Unlock()
		return NodeResult{Status: NodeStatusCompleted, Output: "ok"}
	}
	for i := 0; i < 6; i++ {
		st.fn[shardNodeID("m", i)] = shardFn
	}
	spec := mapSpec(newMapNode("m", MapPolicy{MaxShards: 6}), nil)
	spec.Policies.MaxParallel = 2

	res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(), spec, RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q", res.Status)
	}
	mu.Lock()
	got := peak
	mu.Unlock()
	if got > 2 {
		t.Fatalf("分片并发峰值 = %d, 超过 MaxParallel=2", got)
	}
	if got < 2 {
		t.Fatalf("分片并发峰值 = %d, 期望真的并发到 2 (否则是串行执行)", got)
	}
}

// ---------------------------------------------------------------------------
// 3. 切分策略 / MaxShards 截断 / MinShards 下限
// ---------------------------------------------------------------------------

func TestMapSplitStrategiesAndBounds(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		pol    MapPolicy
		want   []string
		status string
	}{
		{name: "lines", src: "一\n\n二\n三", pol: MapPolicy{MaxShards: 9}, want: []string{"一", "二", "三"}, status: NodeStatusCompleted},
		{name: "paragraphs", src: "段一行1\n段一行2\n\n段二", pol: MapPolicy{Split: SplitParagraph, MaxShards: 9},
			want: []string{"段一行1\n段一行2", "段二"}, status: NodeStatusCompleted},
		{name: "json_array", src: `["甲","乙",{"k":1}]`, pol: MapPolicy{Split: SplitJSONArray, MaxShards: 9},
			want: []string{"甲", "乙", `{"k":1}`}, status: NodeStatusCompleted},
		{name: "whole", src: "一\n二", pol: MapPolicy{Split: SplitWhole, MaxShards: 9}, want: []string{"一\n二"}, status: NodeStatusCompleted},
		{name: "截到MaxShards", src: "1\n2\n3\n4\n5", pol: MapPolicy{MaxShards: 2}, want: []string{"1", "2"}, status: NodeStatusCompleted},
		// 不足 MinShards: 真失败 (声明了"至少扇出 N 路"就是硬要求)
		{name: "不足MinShards", src: "1\n2", pol: MapPolicy{MaxShards: 9, MinShards: 3}, status: NodeStatusFailed},
		// 空集合 = 未走的分支, 必须 skipped 而**不是** failed
		{name: "空集合skipped", src: "   \n\n", pol: MapPolicy{MaxShards: 9}, status: NodeStatusSkipped},
		{name: "坏JSON按空集合", src: "不是JSON", pol: MapPolicy{Split: SplitJSONArray, MaxShards: 9}, status: NodeStatusSkipped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStub()
			src := tc.src
			st.fn["src"] = func(int, NodeInput) NodeResult {
				return NodeResult{Status: NodeStatusCompleted, Output: src}
			}
			j := NewMemoryJournal()
			spec := mapSpec(newMapNode("m", tc.pol), nil)
			res, err := fastEngine(st, j).Run(context.Background(), spec, RunOpts{})
			if err != nil {
				t.Fatalf("Run 出错: %v", err)
			}
			if got := res.Nodes["m"].Status; got != tc.status {
				t.Fatalf("map 状态 = %q (err=%q), 期望 %q", got, res.Nodes["m"].Err, tc.status)
			}
			if tc.status != NodeStatusCompleted {
				if st.callCount(shardNodeID("m", 0)) != 0 {
					t.Fatalf("非 completed 情形不应派发分片")
				}
				return
			}
			for i, want := range tc.want {
				id := shardNodeID("m", i)
				if st.callCount(id) != 1 {
					t.Fatalf("分片 %s 调用次数 = %d, 期望 1", id, st.callCount(id))
				}
				if got := st.input(id, 0).Shard.Value; got != want {
					t.Fatalf("分片 %d 内容 = %q, 期望 %q", i, got, want)
				}
			}
			if st.callCount(shardNodeID("m", len(tc.want))) != 0 {
				t.Fatalf("分片数超过期望的 %d 个", len(tc.want))
			}
			if tc.name == "截到MaxShards" {
				exp := findEvents(t, j, EvMapExpanded)
				if len(exp) != 1 || evInt(t, exp[0].Data, "truncated") != 3 {
					t.Fatalf("map.expanded 应记 truncated=3, 实得 %+v", exp)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. 重试是**分片级**: 失败的分片自己重试, 已成功的分片不被连带重跑
// ---------------------------------------------------------------------------

func TestMapRetryIsPerShardNotPerGroup(t *testing.T) {
	st := newStub()
	st.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "x\ny"}
	}
	st.fn[shardNodeID("m", 1)] = func(call int, _ NodeInput) NodeResult {
		if call < 2 {
			return NodeResult{Status: NodeStatusFailed, Err: "瞬态"}
		}
		return NodeResult{Status: NodeStatusCompleted, Output: "片1终成"}
	}
	mn := newMapNode("m", MapPolicy{MaxShards: 4})
	mn.Retry = &RetryPolicy{MaxRetries: 3, BackoffSec: 1}
	j := NewMemoryJournal()
	res, err := fastEngine(st, j).Run(context.Background(), mapSpec(mn, nil), RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Nodes["m"].Status != NodeStatusCompleted {
		t.Fatalf("map 结果 = %+v, 期望重试后 completed", res.Nodes["m"])
	}
	if n := st.callCount(shardNodeID("m", 0)); n != 1 {
		t.Fatalf("成功的分片 0 被调用 %d 次, 期望 1 (组级重试会把它连带重跑)", n)
	}
	if n := st.callCount(shardNodeID("m", 1)); n != 3 {
		t.Fatalf("失败的分片 1 被调用 %d 次, 期望 3 (1+2 次重试)", n)
	}
	// node.retried 记在分片 ID 上, 归因到片而不是整组
	rt := findEvents(t, j, EvNodeRetried)
	if len(rt) != 2 || rt[0].NodeID != shardNodeID("m", 1) {
		t.Fatalf("node.retried = %+v, 期望 2 条且挂在 m#1 上", rt)
	}
}

// 分片全失败 ⇒ map failed; 分片部分失败 ⇒ map completed 但 Err 带明细。
func TestMapPartialAndTotalShardFailure(t *testing.T) {
	run := func(failIdx map[int]bool) NodeResult {
		st := newStub()
		st.fn["src"] = func(int, NodeInput) NodeResult {
			return NodeResult{Status: NodeStatusCompleted, Output: "a\nb"}
		}
		for i := range failIdx {
			st.fn[shardNodeID("m", i)] = func(int, NodeInput) NodeResult {
				return NodeResult{Status: NodeStatusFailed, Err: "坏了"}
			}
		}
		res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(),
			mapSpec(newMapNode("m", MapPolicy{MaxShards: 4}), nil), RunOpts{})
		if err != nil {
			t.Fatalf("Run 出错: %v", err)
		}
		return res.Nodes["m"]
	}
	if r := run(map[int]bool{1: true}); r.Status != NodeStatusCompleted || !strings.Contains(r.Err, "1/2") {
		t.Fatalf("部分失败 = %+v, 期望 completed 且 Err 带 1/2", r)
	}
	if r := run(map[int]bool{0: true, 1: true}); r.Status != NodeStatusFailed {
		t.Fatalf("全部失败 = %+v, 期望 failed", r)
	}
}

// ---------------------------------------------------------------------------
// 5. reduce 真等齐 + 分片结果下发 + 确定性策略零 runner 调用
// ---------------------------------------------------------------------------

func TestReduceWaitsForWholeMapGroup(t *testing.T) {
	st := newStub()
	st.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "a\nb\nc"}
	}
	var (
		mu       sync.Mutex
		finished int
		atReduce int
	)
	for i := 0; i < 3; i++ {
		idx := i
		st.fn[shardNodeID("m", idx)] = func(int, NodeInput) NodeResult {
			time.Sleep(time.Duration(idx) * 10 * time.Millisecond)
			mu.Lock()
			finished++
			mu.Unlock()
			return NodeResult{Status: NodeStatusCompleted, Output: fmt.Sprintf("out%d", idx), Score: 80}
		}
	}
	st.fn["r"] = func(_ int, in NodeInput) NodeResult {
		mu.Lock()
		atReduce = finished // reduce 进场时已完成的分片数
		mu.Unlock()
		return NodeResult{Status: NodeStatusCompleted, Output: fmt.Sprintf("聚合%d片", len(in.Shards))}
	}
	rn := NodeSpec{ID: "r", Kind: NodeKindReduce, Agent: AgentSpec{Role: "fuser"}}
	res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(),
		mapSpec(newMapNode("m", MapPolicy{MaxShards: 4}), &rn), RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Status != RunStatusCompleted {
		t.Fatalf("Status = %q, nodes=%+v", res.Status, res.Nodes)
	}
	mu.Lock()
	got := atReduce
	mu.Unlock()
	if got != 3 {
		t.Fatalf("reduce 进场时只完成了 %d 个分片, 期望 3 (reduce 必须等齐整组)", got)
	}
	in := st.input("r", 0)
	if len(in.Shards) != 3 {
		t.Fatalf("reduce 收到 %d 个分片结果, 期望 3", len(in.Shards))
	}
	for i, s := range in.Shards {
		if s.Index != i || s.NodeID != shardNodeID("m", i) || s.MapID != "m" ||
			s.Status != NodeStatusCompleted || s.Output != fmt.Sprintf("out%d", i) {
			t.Fatalf("第 %d 个分片结果 = %+v, 期望按序且带 ID/产出", i, s)
		}
		if s.Input == "" {
			t.Errorf("分片结果缺 Input (runner 无法知道这片当初处理的是什么)")
		}
	}
	if res.Nodes["r"].Output != "聚合3片" {
		t.Fatalf("reduce 产出 = %q", res.Nodes["r"].Output)
	}
}

func TestReduceDeterministicStrategies(t *testing.T) {
	mk := func(pol ReducePolicy, outs []string, fails map[int]bool) (RunResult, *stubRunner) {
		st := newStub()
		src := strings.Repeat("x\n", len(outs))
		st.fn["src"] = func(int, NodeInput) NodeResult {
			return NodeResult{Status: NodeStatusCompleted, Output: src}
		}
		for i, o := range outs {
			out := o
			if fails[i] {
				st.fn[shardNodeID("m", i)] = func(int, NodeInput) NodeResult {
					return NodeResult{Status: NodeStatusFailed, Err: "坏了"}
				}
				continue
			}
			st.fn[shardNodeID("m", i)] = func(int, NodeInput) NodeResult {
				return NodeResult{Status: NodeStatusCompleted, Output: out}
			}
		}
		rn := NodeSpec{ID: "r", Kind: NodeKindReduce, Agent: AgentSpec{Role: "fuser"}, Reduce: &pol}
		res, err := fastEngine(st, NewMemoryJournal()).Run(context.Background(),
			mapSpec(newMapNode("m", MapPolicy{MaxShards: 9}), &rn), RunOpts{})
		if err != nil {
			t.Fatalf("Run 出错: %v", err)
		}
		return res, st
	}

	// concat: 确定性拼接, **runner 完全不被调用** (零 LLM)
	res, st := mk(ReducePolicy{Strategy: ReduceConcat, Separator: "|"}, []string{"甲", "乙"}, nil)
	if got := res.Nodes["r"].Output; got != "甲|乙" {
		t.Fatalf("concat 产出 = %q, 期望 甲|乙", got)
	}
	if st.callCount("r") != 0 {
		t.Fatalf("确定性 reduce 调用了 runner %d 次, 期望 0 (零 LLM)", st.callCount("r"))
	}
	// longest
	res, _ = mk(ReducePolicy{Strategy: ReduceLongest}, []string{"短", "长长长长"}, nil)
	if got := res.Nodes["r"].Output; got != "长长长长" {
		t.Fatalf("longest 产出 = %q", got)
	}
	// RequireAll: 一片失败即 reduce failed
	res, st = mk(ReducePolicy{Strategy: ReduceConcat, RequireAll: true}, []string{"甲", "乙"}, map[int]bool{1: true})
	if r := res.Nodes["r"]; r.Status != NodeStatusFailed || !strings.Contains(r.Err, "require_all") {
		t.Fatalf("RequireAll 下 reduce = %+v, 期望 failed", r)
	}
	// 默认 (RequireAll=false): 尽力聚合成功的分片
	res, _ = mk(ReducePolicy{Strategy: ReduceConcat}, []string{"甲", "乙"}, map[int]bool{1: true})
	if r := res.Nodes["r"]; r.Status != NodeStatusCompleted || r.Output != "甲" {
		t.Fatalf("默认 reduce = %+v, 期望只聚合成功片 甲", r)
	}
}

// map 空集合 (skipped) ⇒ 下游 reduce 级联 skipped, 且**都不算失败阶段**。
func TestMapSkippedCascadesToReduceWithoutFailure(t *testing.T) {
	st := newStub()
	st.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "  "}
	}
	rn := NodeSpec{ID: "r", Kind: NodeKindReduce, Agent: AgentSpec{Role: "fuser"}}
	j := NewMemoryJournal()
	res, err := fastEngine(st, j).Run(context.Background(),
		mapSpec(newMapNode("m", MapPolicy{MaxShards: 4}), &rn), RunOpts{})
	if err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	if res.Nodes["m"].Status != NodeStatusSkipped {
		t.Fatalf("空集合 map = %+v, 期望 skipped (不是 failed)", res.Nodes["m"])
	}
	if res.Nodes["r"].Status != NodeStatusSkipped {
		t.Fatalf("下游 reduce = %+v, 期望级联 skipped", res.Nodes["r"])
	}
	if len(findEvents(t, j, EvNodeFailed)) != 0 {
		t.Fatalf("空集合扇出不应产生任何 node.failed 事件")
	}
	if st.callCount("r") != 0 {
		t.Fatalf("skipped 的 reduce 被执行了")
	}
}

// ---------------------------------------------------------------------------
// 6. resume: 不重复展开, 分片集与分片产出从 journal 恢复
// ---------------------------------------------------------------------------

func TestMapReduceResumeDoesNotReExpand(t *testing.T) {
	dir := t.TempDir()
	rn := NodeSpec{ID: "r", Kind: NodeKindReduce, Agent: AgentSpec{Role: "fuser"}}
	spec := mapSpec(newMapNode("m", MapPolicy{MaxShards: 9}), &rn)

	// —— 首跑: 分片成功, reduce 失败 → partial ——
	j1, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	st1 := newStub()
	st1.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "甲\n乙\n丙"}
	}
	st1.fn["r"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusFailed, Err: "聚合第一趟失败"}
	}
	res1, err := fastEngine(st1, j1).Run(context.Background(), spec, RunOpts{RunID: "run-map"})
	if err != nil {
		t.Fatalf("首跑出错: %v", err)
	}
	if res1.Status != RunStatusPartial {
		t.Fatalf("首跑 Status = %q, 期望 partial", res1.Status)
	}
	j1.Close()

	// —— 续跑: 同一 journal, Resume ——
	j2, err := NewFileJournal(dir)
	if err != nil {
		t.Fatalf("重开 journal: %v", err)
	}
	defer j2.Close()
	st2 := newStub()
	// src 若被重跑会给出**不同的**集合 —— 用它反证"分片集来自 journal 而非重新切分"。
	st2.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "完全不同\n的两行"}
	}
	var gotShards []ShardResult
	st2.fn["r"] = func(_ int, in NodeInput) NodeResult {
		gotShards = append([]ShardResult(nil), in.Shards...)
		return NodeResult{Status: NodeStatusCompleted, Output: "聚合成功"}
	}
	res2, err := fastEngine(st2, j2).Run(context.Background(), spec, RunOpts{RunID: "run-map", Resume: true})
	if err != nil {
		t.Fatalf("续跑出错: %v", err)
	}
	if res2.Status != RunStatusCompleted {
		t.Fatalf("续跑 Status = %q, 期望 completed", res2.Status)
	}
	// src / map / 分片 全部走缓存, 一次 runner 都不该调
	for _, id := range []string{"src", "m#0", "m#1", "m#2"} {
		if st2.callCount(id) != 0 {
			t.Fatalf("缓存命中的 %s 被重跑 %d 次", id, st2.callCount(id))
		}
	}
	// 只有首跑那一条 map.expanded —— resume 不重复展开
	if n := len(findEvents(t, j2, EvMapExpanded)); n != 1 {
		t.Fatalf("map.expanded 事件数 = %d, 期望 1 (resume 不得重复展开)", n)
	}
	// 分片结果经 journal 恢复并原样喂给 reduce
	if len(gotShards) != 3 {
		t.Fatalf("resume 后 reduce 收到 %d 个分片, 期望 3", len(gotShards))
	}
	for i, want := range []string{"甲", "乙", "丙"} {
		if gotShards[i].Input != want {
			t.Fatalf("第 %d 片输入 = %q, 期望 journal 里首跑的 %q (说明分片集被重新切分了)",
				i, gotShards[i].Input, want)
		}
		if gotShards[i].Status != NodeStatusCompleted || gotShards[i].Output == "" {
			t.Fatalf("第 %d 片结果未从 journal 恢复: %+v", i, gotShards[i])
		}
	}
}

// 崩溃恰好发生在扇出中途 (map 节点还没写 node.completed, 但部分分片已经写了):
// 已 completed 的分片走缓存, 其余分片重跑, 分片集仍取 journal 记下的那份。
// journal 手工构造 —— 这正是进程被 kill 时留在盘上的形态, 跑不出来只能手写。
func TestMapResumeReusesCompletedShards(t *testing.T) {
	spec := mapSpec(newMapNode("m", MapPolicy{MaxShards: 9}), nil)
	j := NewMemoryJournal()
	for _, ev := range []Event{
		{Type: EvRunCreated, RunID: "run-shard"},
		{Type: EvNodeCompleted, RunID: "run-shard", NodeID: "src", Data: map[string]any{"output": "甲\n乙"}},
		{Type: EvMapExpanded, RunID: "run-shard", NodeID: "m",
			Data: map[string]any{"count": 2, "shards": `["甲","乙"]`}},
		{Type: EvNodeCompleted, RunID: "run-shard", NodeID: "m#0", Data: map[string]any{"output": "片0首跑产出"}},
		// 没有 m 的 node.completed, 也没有 run.finished —— 进程在这里被 kill
	} {
		if err := j.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	st := newStub()
	st.fn["src"] = func(int, NodeInput) NodeResult {
		t.Error("src 已在 journal 里 completed, 不应重跑")
		return NodeResult{Status: NodeStatusCompleted}
	}
	res, err := fastEngine(st, j).Run(context.Background(), spec, RunOpts{RunID: "run-shard", Resume: true})
	if err != nil {
		t.Fatalf("续跑出错: %v", err)
	}
	if res.Nodes["m"].Status != NodeStatusCompleted {
		t.Fatalf("续跑后 map = %+v, 期望 completed", res.Nodes["m"])
	}
	if n := st.callCount(shardNodeID("m", 0)); n != 0 {
		t.Fatalf("已成功的分片 0 被重跑 %d 次, 期望 0 (崩溃前的产出应复用)", n)
	}
	if n := st.callCount(shardNodeID("m", 1)); n != 1 {
		t.Fatalf("未完成的分片 1 执行 %d 次, 期望 1", n)
	}
	if got := res.Nodes["m"].Shards[0].Output; got != "片0首跑产出" {
		t.Fatalf("缓存分片产出 = %q, 期望 journal 里的 片0首跑产出", got)
	}
	if n := len(findEvents(t, j, EvMapExpanded)); n != 1 {
		t.Fatalf("map.expanded 事件数 = %d, 期望仍是 1 (分片集来自 journal, 不重新切分)", n)
	}
	// 分片集取 journal 的那份: 第 1 片的输入必须是"乙", 不是重新切分的结果
	if got := st.input(shardNodeID("m", 1), 0).Shard.Value; got != "乙" {
		t.Fatalf("分片 1 的输入 = %q, 期望 journal 里的 乙", got)
	}
}

// ---------------------------------------------------------------------------
// 7. hook 载荷契约: 分片带 shard_of/shard_index; map 节点带分片计数
// ---------------------------------------------------------------------------

func TestMapHookPayloads(t *testing.T) {
	st := newStub()
	st.fn["src"] = func(int, NodeInput) NodeResult {
		return NodeResult{Status: NodeStatusCompleted, Output: "a\nb"}
	}
	bus := &recordBus{}
	e := fastEngine(st, NewMemoryJournal())
	e.Hooks = bus
	if _, err := e.Run(context.Background(), mapSpec(newMapNode("m", MapPolicy{MaxShards: 4}), nil), RunOpts{}); err != nil {
		t.Fatalf("Run 出错: %v", err)
	}
	pre, ok := bus.find(ScopeNode, "pre", "m#1")
	if !ok {
		t.Fatal("缺分片 m#1 的 node pre 事件 (dashboard 无法逐片显示进度)")
	}
	if pre.Payload["shard_of"] != "m" {
		t.Errorf("分片 pre 载荷缺 shard_of=m: %v", pre.Payload)
	}
	post, ok := bus.find(ScopeNode, "post", "m")
	if !ok {
		t.Fatal("缺 map 节点的 node post 事件")
	}
	if post.Payload["shards"] != 2 || post.Payload["shards_ok"] != 2 {
		t.Errorf("map post 载荷 = %v, 期望 shards=2 shards_ok=2", post.Payload)
	}
	if post.Payload["kind"] != string(NodeKindMap) {
		t.Errorf("map post 载荷 kind = %v, 期望 map", post.Payload["kind"])
	}
}
