package worker

// 逐节点放置回归 (design/01 §4.9)。
//
// 这里断言的是**声明真的生效**, 而不是"字段传下来了":
//   - 声明了 require:["browser"] 的节点在没有 browser 能力的 runtime 上必须**跑不了**
//     (fail-closed)。回落到"随便找个能跑的"会让渲染节点在没有无头浏览器的机器上
//     产出假货, 而且看起来是成功的;
//   - 有 browser 的 runtime 在场时必须落到它上面;
//   - 节点只声明 require 时**不得丢掉进程级默认里的团队亲和** —— 丢了就换工作区,
//     症状是"渲染节点看不见前面生成的 HTML", 极难归因到那一行 require 上。

import (
	"context"
	"errors"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/trace"
)

// nodeRunnerFor 按 RuntimeFactory 的同一套规则从 ctx 捕获声明并造一个 runner。
//
// 直接构造而不走 RuntimeFactory: 本测试断言的是**放置求解**这一件事, 与工厂的
// 注入参数无关; 工厂从 ctx 捕获 hints 这一步由
// TestRuntimeFactory_节点声明与trace下传 覆盖。
func nodeRunnerFor(ctx context.Context, reg agent.RuntimeRegistry, def *agent.Placement) *runtimeRunner {
	return &runtimeRunner{
		reg: reg, role: "renderer",
		hints: agent.NodeExecHintsFromContext(ctx),
		ids:   trace.From(ctx),
		meta:  agent.RunMetadataFromContext(ctx),
		def:   def,
	}
}

// ctxWithNodePlacement 模拟 stageNodeRunner 把 graph.AgentSpec.Placement 转好放进 ctx。
func ctxWithNodePlacement(p *agent.Placement) context.Context {
	return agent.WithNodeExecHints(context.Background(), agent.NodeExecHints{
		Node: "render", Role: "renderer", Kind: "agent", Placement: p,
	})
}

// 节点声明的硬约束必须挡住没有该能力的 runtime, 且有能力的 runtime 在场时必须命中它。
func TestNodePlacement_require挡住无能力runtime(t *testing.T) {
	plain := &recordRuntime{name: "local-session", caps: agent.RuntimeCaps{Bash: true}, out: "无浏览器机器"}
	reg := agent.NewRuntimeRegistry()
	reg.Register(plain, 0)

	ctx := ctxWithNodePlacement(&agent.Placement{Require: []string{"browser"}})
	// 进程级默认只说"偏好本地", 不带任何硬约束 —— 硬约束只来自节点声明。
	runner := nodeRunnerFor(ctx, reg, &agent.Placement{Prefer: "local"})

	out, err := runner.Execute(context.Background(), "渲染这张图")
	if err == nil || !errors.Is(err, agent.ErrNoRuntime) {
		t.Fatalf("声明了 browser 却落在无 browser 的 runtime 上: out=%q err=%v", out, err)
	}

	// 有 browser 的 runtime 上线后必须落到它 (同时证明约束不是"永远拒绝")。
	browser := &recordRuntime{name: "render-pool-1",
		caps: agent.RuntimeCaps{Bash: true, Browser: true}, out: "有浏览器机器"}
	reg.Register(browser, 0)
	got, err := runner.Execute(context.Background(), "渲染这张图")
	if err != nil {
		t.Fatalf("browser runtime 在场却失败: %v", err)
	}
	if got != "有浏览器机器" {
		t.Errorf("产出来自 %q, 期望落在 render-pool-1", got)
	}
	if len(plain.tasks) != 0 {
		t.Errorf("无 browser 的 runtime 被派了 %d 个任务", len(plain.tasks))
	}
	// 求解出来的策略随任务下发: 远程 worker 侧要靠它做二次校验/路由。
	if len(browser.tasks) != 1 || browser.tasks[0].Placement == nil ||
		len(browser.tasks[0].Placement.Require) != 1 {
		t.Errorf("任务未携带求解后的放置策略: %+v", browser.tasks)
	}
}

// 节点声明**不得**整份替换进程级默认: 只写 require 的节点必须保住团队亲和,
// 否则它会被派到另一个工作区, 上一阶段写的代码在它眼里不存在。
func TestNodePlacement_节点声明不丢团队亲和(t *testing.T) {
	reg := agent.NewRuntimeRegistry()
	reg.Register(&recordRuntime{name: "w1", caps: agent.RuntimeCaps{Bash: true, Browser: true}, out: "ok"}, 0)
	def := &agent.Placement{Affinity: "team", Prefer: "local"}

	ctx := agent.WithRunMetadata(ctxWithNodePlacement(
		&agent.Placement{Require: []string{"browser"}}), agent.RunMetadata{Team: "creative-v2"})
	p := nodeRunnerFor(ctx, reg, def).Placement()

	if len(p.Require) != 1 || p.Require[0] != "browser" {
		t.Errorf("节点声明的硬约束丢了: %+v", p)
	}
	if p.Affinity != "team" || p.AffinityKey != "creative-v2" {
		t.Errorf("团队亲和被节点声明挤掉了: %+v", p)
	}
	if p.Prefer != "local" {
		t.Errorf("节点没提 prefer 就该继承默认, 实得 %q", p.Prefer)
	}
	if def.AffinityKey != "" || len(def.Require) != 0 {
		t.Errorf("进程级默认被就地修改了 (它被所有节点共享): %+v", def)
	}
}

// 节点提了的字段才覆盖: prefer / affinity 各自独立生效, 且亲和与分组键成对覆盖。
func TestNodePlacement_逐字段覆盖(t *testing.T) {
	reg := agent.NewRuntimeRegistry()
	def := &agent.Placement{Prefer: "local", Affinity: "team", AffinityKey: "默认键"}

	// 只覆盖 prefer。
	ctx := ctxWithNodePlacement(&agent.Placement{Prefer: "remote:gpu-1"})
	p := nodeRunnerFor(ctx, reg, def).Placement()
	if p.Prefer != "remote:gpu-1" || p.Affinity != "team" || p.AffinityKey != "默认键" {
		t.Errorf("只覆盖 prefer 时其余应继承: %+v", p)
	}
	// 覆盖亲和口径时分组键一起换 (拿旧键去配新口径会把不相关的节点钉到一起)。
	ctx = ctxWithNodePlacement(&agent.Placement{Affinity: "team", AffinityKey: "本节点专属"})
	p = nodeRunnerFor(ctx, reg, def).Placement()
	if p.AffinityKey != "本节点专属" {
		t.Errorf("分组键应随亲和一起被覆盖: %+v", p)
	}
	// 未声明放置的节点: 行为与改造前一字不变 (整份取进程默认)。
	p = nodeRunnerFor(context.Background(), reg, def).Placement()
	if p.Prefer != "local" || p.AffinityKey != "默认键" || len(p.Require) != 0 {
		t.Errorf("未声明放置的节点行为变了: %+v", p)
	}
	// 既无进程默认又无节点声明: 仍偏好本地 (改造前的缺省)。
	p = nodeRunnerFor(context.Background(), reg, nil).Placement()
	if p.Prefer != "local" {
		t.Errorf("双缺省应偏好本地, 实得 %+v", p)
	}
	// 有节点声明但无进程默认: 不替它塞 Prefer:"local" (那是悄悄改它的放置)。
	p = nodeRunnerFor(ctxWithNodePlacement(&agent.Placement{Require: []string{"gpu"}}), reg, nil).Placement()
	if p.Prefer != "" || len(p.Require) != 1 {
		t.Errorf("节点显式声明时不该被塞默认偏好: %+v", p)
	}
}
