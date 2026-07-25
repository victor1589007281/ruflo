package worker

// journal_test.go —— 远程执行的节点必须与本地执行走**同一套** journal 记账。
//
// 为什么这件事值得单独一组测试: journal 是事件溯源的唯一真源 (design/01 §4.3),
// resume 只认它。如果远程节点的产出没进 journal, 表现是"重跑时远程节点的状态是
// 空的"——上游产出丢失, 下游节点拿着空输入重跑, 而且**没有任何报错**。
//
// 本组测试用真的 graph.Engine + 真的 FileJournal + 真的独立 worker (HTTP 往返),
// 中间那个 NodeRunner 只做一件事: 调用 factory 拿 runner 并执行 —— 与生产的
// stageNodeRunner → WorkflowExecutor.runAgent → we.factory 那一行同构
// (生产链路上还有门禁/分片/回灌等逻辑, 但它们与"产出如何进 journal"无关,
// 且需要一个活的 WorkflowExecutor + ProductionTeam + LLM 客户端才能起来)。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/graph"
)

// factoryNodeRunner 把 CreateAgentFunc 接成 graph.NodeRunner (生产里这一步由
// pkg/agent 的 stageNodeRunner + WorkflowExecutor.runAgent 完成)。
type factoryNodeRunner struct {
	f agent.CreateAgentFunc
}

func (r *factoryNodeRunner) RunNode(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	// 与 stageNodeRunner 同款: 节点声明经 ctx 下传, 供工厂在**创建时**捕获。
	ctx = agent.WithNodeExecHints(ctx, agent.NodeExecHints{
		Node: node.ID, Role: node.Agent.Role, Kind: string(node.Kind),
		ToolProfile: node.Agent.ToolProfile, MaxTurns: node.Agent.MaxTurns,
		Iteration: in.Iteration,
	})
	runner, err := r.f(ctx, node.Agent.Role, "")
	if err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: err.Error()}
	}
	out, err := runner.Execute(ctx, in.Objective)
	if err != nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Output: out, Err: err.Error()}
	}
	return graph.NodeResult{Status: graph.NodeStatusCompleted, Output: out}
}

func twoNodeSpec() graph.GraphSpec {
	return graph.GraphSpec{
		Name: "remote-journal",
		Nodes: []graph.NodeSpec{
			{ID: "design", Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "architect"}},
			{ID: "impl", Kind: graph.NodeKindAgent, Agent: graph.AgentSpec{Role: "coder"}},
		},
		Edges: []graph.EdgeSpec{{From: "design", To: "impl"}},
	}
}

// 远程执行的节点产出必须落进 journal 的 node.completed, 且 Replay 能读回来。
func TestJournal_远程节点产出进journal(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(_ context.Context, role, prompt string) (string, error) {
			return "远程[" + role + "]" + prompt, nil
		})
	tc.startWorker(t, Options{Name: "w1", Runtime: rt})
	tc.waitRuntime(t, "w1", time.Second)

	j, err := graph.NewFileJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	// 放置策略钉住远程 worker: 全部节点都在另一个"进程"里跑。
	runner := &factoryNodeRunner{f: RuntimeFactory(tc.rr, &agent.Placement{Prefer: "remote:w1", Require: []string{CapBash}}, nil)}
	eng := &graph.Engine{Runner: runner, Journal: j}
	res, err := eng.Run(context.Background(), twoNodeSpec(), graph.RunOpts{RunID: "run-1", Objective: "做个东西"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != graph.RunStatusCompleted {
		t.Fatalf("图状态 = %s (%+v)", res.Status, res.Nodes)
	}
	if got := res.Nodes["impl"].Output; !strings.HasPrefix(got, "远程[coder]") {
		t.Errorf("impl 产出 = %q, 期望来自远程 worker", got)
	}

	// —— journal 记账检查 ——
	evs, err := j.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{} // "<type>/<node>" → output
	for _, ev := range evs {
		out, _ := ev.Data["output"].(string)
		seen[ev.Type+"/"+ev.NodeID] = out
	}
	for _, key := range []string{
		graph.EvNodeStarted + "/design", graph.EvNodeCompleted + "/design",
		graph.EvNodeStarted + "/impl", graph.EvNodeCompleted + "/impl",
	} {
		if _, ok := seen[key]; !ok {
			t.Errorf("journal 缺事件 %s (远程节点没走同一套记账)", key)
		}
	}
	if got := seen[graph.EvNodeCompleted+"/impl"]; !strings.HasPrefix(got, "远程[coder]") {
		t.Errorf("node.completed 里的产出 = %q, 远程产出没进 journal", got)
	}

	// —— Replay: resume 能读回远程节点的状态 ——
	st := graph.Replay(evs)
	if !st.Finished || st.Status != graph.RunStatusCompleted {
		t.Fatalf("Replay 未看到 run.finished: %+v", st)
	}
}

// 强证据: 第一轮远程跑 design 成功、impl 失败 (整轮 partial) → 第二轮 Resume 时
// design 必须直接命中 journal 缓存 (不再执行), 且缓存里是**第一轮远程**的产出。
// 这正是"远程节点状态是空的"这类缺陷会被抓住的地方。
func TestJournal_resume命中远程节点缓存(t *testing.T) {
	tc := newTestControl(t, controlOpts{})
	rt := echoRuntime("local-w1", agent.RuntimeCaps{Bash: true},
		func(_ context.Context, role, prompt string) (string, error) {
			if role == "coder" { // 第一轮: impl 必失败
				return "", errors.New("远程编译失败")
			}
			return "远程[" + role + "]", nil
		})
	tc.startWorker(t, Options{Name: "w1", Runtime: rt})
	tc.waitRuntime(t, "w1", time.Second)

	dir := t.TempDir()
	j, err := graph.NewFileJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := &factoryNodeRunner{f: RuntimeFactory(tc.rr, &agent.Placement{Prefer: "remote:w1"}, nil)}
	eng := &graph.Engine{Runner: runner, Journal: j}
	res, err := eng.Run(context.Background(), twoNodeSpec(), graph.RunOpts{RunID: "run-1", Objective: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == graph.RunStatusCompleted {
		t.Fatalf("第一轮应为 partial/failed: %s", res.Status)
	}
	if res.Nodes["design"].Output != "远程[architect]" {
		t.Fatalf("design 产出 = %q", res.Nodes["design"].Output)
	}
	j.Close()

	// 第二轮: 换一个会给出**不同**产出的执行体。design 若被重跑, 产出会变;
	// 命中 journal 缓存则仍是第一轮的远程产出。
	tc2 := newTestControl(t, controlOpts{})
	rt2 := echoRuntime("local-w2", agent.RuntimeCaps{Bash: true},
		func(_ context.Context, role, _ string) (string, error) { return "第二轮[" + role + "]", nil })
	tc2.startWorker(t, Options{Name: "w2", Runtime: rt2})
	tc2.waitRuntime(t, "w2", time.Second)

	j2, err := graph.NewFileJournal(dir) // 同一目录 = 同一 journal
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	runner2 := &factoryNodeRunner{f: RuntimeFactory(tc2.rr, &agent.Placement{Prefer: "remote:w2"}, nil)}
	eng2 := &graph.Engine{Runner: runner2, Journal: j2}
	res2, err := eng2.Run(context.Background(), twoNodeSpec(),
		graph.RunOpts{RunID: "run-1", Objective: "x", Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != graph.RunStatusCompleted {
		t.Fatalf("第二轮应完成: %s (%+v)", res2.Status, res2.Nodes)
	}
	if got := res2.Nodes["design"].Output; got != "远程[architect]" {
		t.Errorf("design 未命中 journal 缓存 (远程节点状态丢失): %q", got)
	}
	if got := res2.Nodes["impl"].Output; got != "第二轮[coder]" {
		t.Errorf("impl 应由第二轮 worker 重跑: %q", got)
	}
	// 第二轮的 worker 只跑了 impl 一次 —— design 命中缓存没有再派任务出去。
	tasks, _ := tc2.q.List()
	if len(tasks) != 1 || tasks[0].NodeID != "impl" {
		t.Errorf("第二轮派出的任务 = %+v, 期望只有 impl", tasks)
	}
}
