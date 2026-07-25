package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// rtRunner 本测试专用的 AgentRunner 桩（不复用 graph_adapter_test 的
// stubStageRunner —— 它的字段是 role/calls，与这里需要的 out/err 不同）。
type rtRunner struct {
	out string
	err error
}

func (r *rtRunner) Execute(context.Context, string) (string, error) { return r.out, r.err }

// fakeRuntime 可编程 AgentRuntime，用于测放置求解。
type fakeRuntime struct {
	name string
	caps RuntimeCaps
}

func (f *fakeRuntime) Name() string              { return f.name }
func (f *fakeRuntime) Capabilities() RuntimeCaps { return f.caps }
func (f *fakeRuntime) Execute(context.Context, RuntimeNodeTask) (<-chan NodeEvent, error) {
	ch := make(chan NodeEvent, 1)
	ch <- NodeEvent{Kind: NodeEventDone, Output: f.name}
	close(ch)
	return ch, nil
}
func (f *fakeRuntime) Cancel(string, string) error { return nil }

func TestRuntimeCaps_Has(t *testing.T) {
	c := RuntimeCaps{Bash: true, Browser: true, Extra: []string{"gpu-a100"}}
	for _, cap := range []string{"bash", "BASH", " browser ", "gpu-a100", "GPU-A100", ""} {
		if !c.Has(cap) {
			t.Errorf("Has(%q) 应为 true", cap)
		}
	}
	for _, cap := range []string{"gpu", "k8s-sandbox", "unknown"} {
		if c.Has(cap) {
			t.Errorf("Has(%q) 应为 false", cap)
		}
	}
	// k8s 与 k8s-sandbox 是同义词
	k := RuntimeCaps{K8sSandbox: true}
	if !k.Has("k8s") || !k.Has("k8s-sandbox") {
		t.Error("k8s / k8s-sandbox 应互为同义")
	}
}

// 硬约束：不满足 Require 的 runtime 必须被过滤掉，无候选时返回 ErrNoRuntime。
func TestRuntimeRegistry_硬约束过滤(t *testing.T) {
	r := NewRuntimeRegistry()
	r.Register(&fakeRuntime{name: "local-analysis", caps: RuntimeCaps{}}, 0)
	r.Register(&fakeRuntime{name: "local-coding", caps: RuntimeCaps{Bash: true}}, 0)

	got, err := r.Pick(&Placement{Require: []string{"bash"}})
	if err != nil {
		t.Fatalf("应选中具备 bash 的 runtime: %v", err)
	}
	if got.Name() != "local-coding" {
		t.Errorf("选中 %q, 期望 local-coding", got.Name())
	}

	if _, err := r.Pick(&Placement{Require: []string{"gpu"}}); !errors.Is(err, ErrNoRuntime) {
		t.Errorf("无满足约束的 runtime 时应返回 ErrNoRuntime, 实得 %v", err)
	}
	// 错误信息要带上没被满足的约束，否则排查时无从下手
	_, err = r.Pick(&Placement{Require: []string{"gpu"}})
	if err != nil && !strings.Contains(err.Error(), "gpu") {
		t.Errorf("错误信息应包含未满足的约束: %v", err)
	}
}

// 团队亲和：同 AffinityKey 的后续 Pick 必须落回同一 runtime。
// 产码工作流依赖它——编译门禁在 <cwd>/go.mod 上跑，节点散落会让上一阶段写的代码消失。
func TestRuntimeRegistry_团队亲和黏住同一runtime(t *testing.T) {
	r := NewRuntimeRegistry()
	r.Register(&fakeRuntime{name: "local-a", caps: RuntimeCaps{Bash: true}}, 0)
	r.Register(&fakeRuntime{name: "local-b", caps: RuntimeCaps{Bash: true}}, 0)

	p := &Placement{Require: []string{"bash"}, Affinity: "team", AffinityKey: "team-x"}
	first, err := r.Pick(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		got, err := r.Pick(p)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name() != first.Name() {
			t.Fatalf("第 %d 次选中 %q, 期望黏住 %q", i+1, got.Name(), first.Name())
		}
	}
	// 不同团队各自独立，不受上一个团队的亲和影响
	p2 := &Placement{Require: []string{"bash"}, Affinity: "team", AffinityKey: "team-y"}
	if _, err := r.Pick(p2); err != nil {
		t.Fatal(err)
	}
}

// 注销 runtime 必须一并清掉亲和记录，否则任务会被钉在已注销的 runtime 上。
func TestRuntimeRegistry_注销清理亲和(t *testing.T) {
	r := NewRuntimeRegistry()
	r.Register(&fakeRuntime{name: "local-a", caps: RuntimeCaps{Bash: true}}, 0)
	r.Register(&fakeRuntime{name: "local-b", caps: RuntimeCaps{Bash: true}}, 0)
	p := &Placement{Require: []string{"bash"}, Affinity: "team", AffinityKey: "k"}
	first, _ := r.Pick(p)
	r.Unregister(first.Name())

	got, err := r.Pick(p)
	if err != nil {
		t.Fatalf("注销后应改选另一个: %v", err)
	}
	if got.Name() == first.Name() {
		t.Error("仍选中已注销的 runtime")
	}
}

// Prefer 是软偏好：只影响打分，不影响可行性。
func TestRuntimeRegistry_Prefer打分(t *testing.T) {
	r := NewRuntimeRegistry()
	r.Register(&fakeRuntime{name: "remote-w1", caps: RuntimeCaps{Bash: true}}, 0)
	r.Register(&fakeRuntime{name: "local-cli", caps: RuntimeCaps{Bash: true}}, 0)

	got, _ := r.Pick(&Placement{Require: []string{"bash"}, Prefer: "local"})
	if got.Name() != "local-cli" {
		t.Errorf("Prefer=local 应选中 local-cli, 实得 %q", got.Name())
	}
	got2, _ := r.Pick(&Placement{Require: []string{"bash"}, Prefer: "remote:remote-w1"})
	if got2.Name() != "remote-w1" {
		t.Errorf("Prefer=remote:remote-w1 应选中它, 实得 %q", got2.Name())
	}
	// Prefer 指向不存在的 runtime 时不应变成硬约束（仍要能选出一个）
	if _, err := r.Pick(&Placement{Require: []string{"bash"}, Prefer: "remote:ghost"}); err != nil {
		t.Errorf("Prefer 不可满足时不应报错(它是软偏好): %v", err)
	}
}

// 同分必须按名字升序，保证放置求解确定性——否则同团队节点会在 runtime 间抖动。
func TestRuntimeRegistry_同分确定性(t *testing.T) {
	r := NewRuntimeRegistry()
	for _, n := range []string{"local-z", "local-a", "local-m"} {
		r.Register(&fakeRuntime{name: n, caps: RuntimeCaps{Bash: true}}, 0)
	}
	first, _ := r.Pick(&Placement{Require: []string{"bash"}})
	for i := 0; i < 10; i++ {
		got, _ := r.Pick(&Placement{Require: []string{"bash"}})
		if got.Name() != first.Name() {
			t.Fatalf("同分求解不确定: %q vs %q", got.Name(), first.Name())
		}
	}
	if first.Name() != "local-a" {
		t.Errorf("同分应取名字最小者, 实得 %q", first.Name())
	}
}

// 租约过期的 runtime 必须被剔除：远程 worker 掉线不会主动注销，
// 不按心跳过期会一直把任务派给死掉的 worker。
func TestRuntimeRegistry_租约过期剔除(t *testing.T) {
	r := NewRuntimeRegistry()
	r.Register(&fakeRuntime{name: "remote-dead", caps: RuntimeCaps{Bash: true}}, 30*time.Millisecond)
	if got := r.List(); len(got) != 1 {
		t.Fatalf("刚注册应可见: %v", got)
	}
	time.Sleep(60 * time.Millisecond)
	if got := r.List(); len(got) != 0 {
		t.Errorf("租约过期应被剔除, 实得 %v", got)
	}
	if _, err := r.Pick(&Placement{Require: []string{"bash"}}); !errors.Is(err, ErrNoRuntime) {
		t.Error("过期 runtime 不应被 Pick 选中")
	}
	// 心跳可续租
	r.Register(&fakeRuntime{name: "remote-alive", caps: RuntimeCaps{Bash: true}}, 80*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	r.Heartbeat("remote-alive")
	time.Sleep(50 * time.Millisecond)
	if got := r.List(); len(got) != 1 {
		t.Errorf("心跳续租后应仍可见, 实得 %v", got)
	}
	// lease<=0 = 本地 runtime 永不过期
	r2 := NewRuntimeRegistry()
	r2.Register(&fakeRuntime{name: "local-forever", caps: RuntimeCaps{}}, 0)
	time.Sleep(30 * time.Millisecond)
	if got := r2.List(); len(got) != 1 {
		t.Errorf("lease<=0 应永不过期, 实得 %v", got)
	}
}

// localRuntime 把既有 CreateAgentFunc 收编为 AgentRuntime，且事件通道必须关闭
// （否则调用方 range 不结束 → goroutine 泄漏）。
func TestLocalRuntime_收编AgentRunner(t *testing.T) {
	factory := func(_ context.Context, role, _ string) (AgentRunner, error) {
		return &rtRunner{out: "done-by-" + role}, nil
	}
	rt := NewLocalRuntime("local-test", RuntimeCaps{Bash: true}, factory)
	if rt == nil {
		t.Fatal("NewLocalRuntime 返回 nil")
	}
	ch, err := rt.Execute(context.Background(), RuntimeNodeTask{
		RunID: "r1", NodeID: "n1", Role: "coder", UserPrompt: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := CollectRuntimeOutput(ch) // 会把通道读到关闭
	if err != nil {
		t.Fatalf("执行出错: %v", err)
	}
	if out != "done-by-coder" {
		t.Errorf("产出 = %q", out)
	}
	// 通道已关闭：再读应立即返回零值
	if _, ok := <-ch; ok {
		t.Error("事件通道未关闭 —— 会泄漏生产侧 goroutine")
	}
}

// factory 为 nil 时返回 nil，调用方据此回落。
func TestNewLocalRuntime_nil工厂(t *testing.T) {
	if NewLocalRuntime("x", RuntimeCaps{}, nil) != nil {
		t.Error("factory 为 nil 时应返回 nil")
	}
}

// runner 报错要转成 NodeEventFailed 并由 CollectRuntimeOutput 归约为 error。
func TestLocalRuntime_错误转事件(t *testing.T) {
	factory := func(_ context.Context, _, _ string) (AgentRunner, error) {
		return &rtRunner{err: errors.New("boom")}, nil
	}
	rt := NewLocalRuntime("local-err", RuntimeCaps{}, factory)
	ch, err := rt.Execute(context.Background(), RuntimeNodeTask{RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CollectRuntimeOutput(ch); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("应归约为 error, 实得 %v", err)
	}
}

// Cancel 对不存在的节点要报错而不是静默成功（否则调用方以为取消了）。
func TestLocalRuntime_Cancel未知节点报错(t *testing.T) {
	rt := NewLocalRuntime("local-c", RuntimeCaps{}, func(context.Context, string, string) (AgentRunner, error) {
		return &rtRunner{out: "x"}, nil
	})
	if err := rt.Cancel("nope", "nope"); err == nil {
		t.Error("取消不存在的节点应报错")
	}
}
