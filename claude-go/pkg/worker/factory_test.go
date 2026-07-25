package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/trace"
)

// recordRuntime 记录收到的任务与 ctx, 用于断言"节点声明有没有跨层传下去"。
type recordRuntime struct {
	name  string
	caps  agent.RuntimeCaps
	out   string
	err   error
	tasks []agent.RuntimeNodeTask
	hints []agent.NodeExecHints
	ids   []trace.IDs
}

func (r *recordRuntime) Name() string                    { return r.name }
func (r *recordRuntime) Capabilities() agent.RuntimeCaps { return r.caps }
func (r *recordRuntime) Cancel(string, string) error     { return nil }
func (r *recordRuntime) Execute(ctx context.Context, task agent.RuntimeNodeTask) (<-chan agent.NodeEvent, error) {
	r.tasks = append(r.tasks, task)
	r.hints = append(r.hints, agent.NodeExecHintsFromContext(ctx))
	r.ids = append(r.ids, trace.From(ctx))
	ch := make(chan agent.NodeEvent, 2)
	if r.err != nil {
		ch <- agent.NodeEvent{Kind: agent.NodeEventFailed, Err: r.err.Error()}
	} else {
		ch <- agent.NodeEvent{Kind: agent.NodeEventDone, Output: r.out}
	}
	close(ch)
	return ch, nil
}

// 放置求解失败必须失败, 不许回落到"随便找个能跑的"——声明了 gpu/browser 的节点
// 落在没有该能力的机器上会产出假货。
func TestRuntimeFactory_放置无解必须失败(t *testing.T) {
	reg := agent.NewRuntimeRegistry()
	reg.Register(&recordRuntime{name: "local-a", caps: agent.RuntimeCaps{}}, 0)
	f := RuntimeFactory(reg, &agent.Placement{Require: []string{"gpu"}}, nil)
	runner, err := f(context.Background(), "coder", "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Execute(context.Background(), "写代码")
	if err == nil || !errors.Is(err, agent.ErrNoRuntime) {
		t.Fatalf("期望 ErrNoRuntime, 实得 out=%q err=%v", out, err)
	}
	if !strings.Contains(err.Error(), "gpu") {
		t.Errorf("错误信息应带上未满足的约束: %v", err)
	}
	// 未注入注册表时也必须报错而不是空跑。
	if _, err := RuntimeFactory(nil, nil, nil)(context.Background(), "coder", ""); err == nil {
		t.Error("未注入 RuntimeRegistry 应报错")
	}
}

// 节点声明 (tool_profile/max_turns) 与 trace 四元组必须一路传到 RuntimeNodeTask:
// 远程 worker 只能靠任务字段知道这些, 丢了就等于工具画像静默失效。
func TestRuntimeFactory_节点声明与trace下传(t *testing.T) {
	rec := &recordRuntime{name: "remote-w1", caps: agent.RuntimeCaps{Bash: true}, out: "ok"}
	reg := agent.NewRuntimeRegistry()
	reg.Register(rec, 0)

	// 创建时的 ctx 带 hints/trace (生产里由 stageNodeRunner + 引擎注入)。
	ctx := agent.WithNodeExecHints(context.Background(), agent.NodeExecHints{
		Node: "impl", Role: "coder", Kind: "agent", ToolProfile: "coding", MaxTurns: 42,
	})
	ctx = trace.With(ctx, trace.IDs{RunID: "run-x", NodeID: "impl"})

	f := RuntimeFactory(reg, &agent.Placement{Prefer: "remote:remote-w1"}, nil)
	runner, err := f(ctx, "coder", "系统提示词")
	if err != nil {
		t.Fatal(err)
	}
	// 执行时的 ctx 故意是干净的 —— 生产里 runAgent 换过 ctx, 声明只能来自创建时捕获。
	if out, err := runner.Execute(context.Background(), "干活"); err != nil || out != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(rec.tasks) != 1 {
		t.Fatalf("runtime 被调用 %d 次", len(rec.tasks))
	}
	got := rec.tasks[0]
	if got.Role != "coder" || got.UserPrompt != "干活" || got.SystemPrompt != "系统提示词" {
		t.Errorf("任务基本字段错: %+v", got)
	}
	if got.ToolProfile != "coding" || got.MaxTurns != 42 {
		t.Errorf("工具画像/轮数未下传: %+v", got)
	}
	if got.RunID != "run-x" || got.NodeID != "impl" {
		t.Errorf("trace 未下传: %+v", got)
	}
	// 本地 runtime 是从 **ctx** 读声明的 (宿主 factory 的既有契约), 所以 ctx 也必须回填。
	if rec.hints[0].ToolProfile != "coding" || rec.hints[0].MaxTurns != 42 {
		t.Errorf("hints 未回填 ctx: %+v", rec.hints[0])
	}
	if rec.ids[0].RunID != "run-x" || rec.ids[0].NodeID != "impl" {
		t.Errorf("trace 未回填 ctx: %+v", rec.ids[0])
	}
}

// 团队亲和: AffinityKey 缺省时用团队名补齐 (产码工作流靠它把同团队节点钉在同一
// 工作区)。默认策略结构体不得被就地修改 —— 它被所有节点共享。
func TestRuntimeFactory_团队亲和补全分组键(t *testing.T) {
	rec := &recordRuntime{name: "w1", caps: agent.RuntimeCaps{Bash: true}, out: "ok"}
	reg := agent.NewRuntimeRegistry()
	reg.Register(rec, 0)
	def := &agent.Placement{Affinity: "team", Prefer: "any"}

	ctx := agent.WithRunMetadata(context.Background(), agent.RunMetadata{Team: "trading-v2"})
	runner, err := RuntimeFactory(reg, def, nil)(ctx, "coder", "")
	if err != nil {
		t.Fatal(err)
	}
	rr, ok := runner.(*runtimeRunner)
	if !ok {
		t.Fatalf("runner 类型 = %T", runner)
	}
	if p := rr.Placement(); p.AffinityKey != "trading-v2" {
		t.Errorf("AffinityKey = %q, 期望团队名", p.AffinityKey)
	}
	if def.AffinityKey != "" {
		t.Errorf("默认放置策略被就地修改了: %+v", def)
	}
	// 没有任何分组键时必须关掉亲和 —— 否则所有团队共用一条亲和记录, 反而互相钉住。
	runner2, _ := RuntimeFactory(reg, def, nil)(context.Background(), "coder", "")
	if p := runner2.(*runtimeRunner).Placement(); p.Affinity != "" {
		t.Errorf("无分组键时应关掉亲和, 实得 %+v", p)
	}
	if _, err := runner.Execute(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

// 默认 (未注入放置策略) 必须偏好本地: 接上 runtime 层不该改变既有部署的行为。
func TestRuntimeFactory_默认偏好本地(t *testing.T) {
	reg := agent.NewRuntimeRegistry()
	local := &recordRuntime{name: "local-session", caps: agent.RuntimeCaps{Bash: true}, out: "本地"}
	remote := &recordRuntime{name: "w1", caps: agent.RuntimeCaps{Bash: true}, out: "远程"}
	reg.Register(local, 0)
	reg.Register(remote, 0)

	runner, err := RuntimeFactory(reg, nil, nil)(context.Background(), "coder", "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Execute(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if out != "本地" {
		t.Errorf("默认应落本地 runtime, 实得产出 %q", out)
	}
	// 本地 runtime 全部下线后, 软偏好必须能落到远程 (Prefer 不是硬约束)。
	reg.Unregister("local-session")
	out2, err := runner.Execute(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if out2 != "远程" {
		t.Errorf("本地缺席时应回落远程, 实得 %q", out2)
	}
}

// 执行失败必须原样归约成 error (CollectRuntimeOutput 的语义) 而不是空产出+nil。
func TestRuntimeFactory_失败归约为error(t *testing.T) {
	rec := &recordRuntime{name: "w1", caps: agent.RuntimeCaps{}, err: errors.New("worker 掉线")}
	reg := agent.NewRuntimeRegistry()
	reg.Register(rec, 0)
	runner, err := RuntimeFactory(reg, &agent.Placement{}, nil)(context.Background(), "coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Execute(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "worker 掉线") {
		t.Fatalf("期望归约为 error, 实得 %v", err)
	}
}
