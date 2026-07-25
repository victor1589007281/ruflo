package engine

// constraints_apply_test.go —— Config.ApplyConstraints 的单调性与 fail-closed 断言
// (design/01 §4.6)。这里不 import pkg/agent (会是架构倒挂, 见 ToolConstraintSource
// 注释), 用一个满足接口的 fake 来源, 反倒能构造出真实来源不该产生的"放宽"输入,
// 证明 engine 侧自己也不会被放宽。

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

type fakeConstraints struct {
	allow      map[string]bool
	deny       map[string]bool
	narrowPerm string // NarrowedPermissionMode 的返回值 (直接给, 不做阶梯判定)
	origin     string
}

func (f *fakeConstraints) CompiledToolAllow() map[string]bool           { return f.allow }
func (f *fakeConstraints) CompiledToolDeny() map[string]bool            { return f.deny }
func (f *fakeConstraints) NarrowedPermissionMode(current string) string { return f.narrowPerm }
func (f *fakeConstraints) ConstraintOrigin() string                     { return f.origin }

func TestApplyConstraints_黑名单只增不减(t *testing.T) {
	c := &Config{DisabledTools: map[string]bool{"TeamCreate": true}}
	if err := c.ApplyConstraints(&fakeConstraints{deny: map[string]bool{"Shell": true}, origin: "node"}); err != nil {
		t.Fatalf("黑名单并集不是放宽: %v", err)
	}
	if !c.DisabledTools["TeamCreate"] || !c.DisabledTools["Shell"] {
		t.Errorf("期望并集 {TeamCreate, Shell}, 实际 %v", c.DisabledTools)
	}
	if c.toolExposed("Shell") || c.toolExposed("TeamCreate") {
		t.Error("新并入的黑名单必须被 toolExposed 拒绝 —— 执行点只有它一个")
	}
}

func TestApplyConstraints_首次设白名单即收窄(t *testing.T) {
	c := &Config{}
	if err := c.ApplyConstraints(&fakeConstraints{allow: map[string]bool{"Read": true}, origin: "cli"}); err != nil {
		t.Fatalf("从'不设名单'到'只放行 Read'是收窄: %v", err)
	}
	if !c.toolExposed("Read") || c.toolExposed("Shell") {
		t.Errorf("白名单未生效: Read=%v Shell=%v", c.toolExposed("Read"), c.toolExposed("Shell"))
	}
}

func TestApplyConstraints_白名单取交集且越界项被丢弃(t *testing.T) {
	// 关键的放宽场景: 会话已锁定 {Read, Grep}, 来源却声称还允许 Shell。
	c := &Config{AllowedTools: map[string]bool{"Read": true, "Grep": true}}
	err := c.ApplyConstraints(&fakeConstraints{
		allow:  map[string]bool{"Read": true, "Shell": true},
		origin: "malicious-node",
	})
	if err == nil {
		t.Fatal("白名单越界必须报错 (放宽意图被丢弃需要可观测)")
	}
	if c.toolExposed("Shell") {
		t.Fatal("越界工具绝不能因为来源'也允许'就被放进来 —— 这是 fail-open 回归")
	}
	if !c.toolExposed("Read") {
		t.Error("交集内的 Read 应仍可见")
	}
	if c.toolExposed("Grep") {
		t.Error("交集语义: 来源没列的 Grep 应被收掉")
	}
}

func TestApplyConstraints_空白名单是全拒且不被绕过(t *testing.T) {
	// 空非 nil 白名单 = 全部拒绝 (engine.Config.AllowedTools 的既有 fail-closed 语义)。
	// 两条路径都必须拒: 主循环的 toolExposed, 以及 RunIsolated 的 gateToolUses。
	c := &Config{}
	if err := c.ApplyConstraints(&fakeConstraints{allow: map[string]bool{}, origin: "locked"}); err != nil {
		t.Fatalf("空白名单是最严的收窄, 不该报错: %v", err)
	}
	if c.AllowedTools == nil {
		t.Fatal("空白名单必须落成非 nil 空 map, 否则退化成'不限制'")
	}
	for _, name := range []string{"Read", "Shell", "AnythingElse"} {
		if c.toolExposed(name) {
			t.Errorf("空白名单下 %s 仍可见", name)
		}
	}
	allowed, denied := gateToolUses(c, []types.ContentBlock{
		{Type: types.ContentBlockToolUse, ID: "t1", Name: "Read"},
		{Type: types.ContentBlockToolUse, ID: "t2", Name: "Shell"},
	})
	if len(allowed) != 0 {
		t.Errorf("gateToolUses 应全拒, 实际放过 %d 个", len(allowed))
	}
	if len(denied) != 2 {
		t.Fatalf("每个被拒的 tool_use 都必须有对应的错误 tool_result (协议一致), 实际 %d", len(denied))
	}
	for _, m := range denied {
		if len(m.Content) != 1 || !m.Content[0].IsError {
			t.Errorf("拒绝结果必须是 IsError 的 tool_result, 实际 %+v", m.Content)
		}
	}
}

func TestApplyConstraints_权限档由来源裁决(t *testing.T) {
	// 严格度阶梯只有 pkg/agent 一份, engine 只负责落地来源给出的收窄结果;
	// 来源返回 "" 就是"不覆盖", 现状必须原样保留。
	c := &Config{PermissionMode: types.PermissionModeDefault}
	if err := c.ApplyConstraints(&fakeConstraints{narrowPerm: string(types.PermissionModePlan)}); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if c.PermissionMode != types.PermissionModePlan {
		t.Errorf("期望收窄到 plan, 实际 %q", c.PermissionMode)
	}
	if err := c.ApplyConstraints(&fakeConstraints{narrowPerm: ""}); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if c.PermissionMode != types.PermissionModePlan {
		t.Errorf("来源不覆盖时必须保留更严的现状, 实际 %q", c.PermissionMode)
	}
}

func TestApplyConstraints_来源链累加(t *testing.T) {
	c := &Config{}
	_ = c.ApplyConstraints(&fakeConstraints{origin: "cli-flags"})
	_ = c.ApplyConstraints(&fakeConstraints{origin: "feishu-session"})
	if want := "cli-flags > feishu-session"; c.ConstraintOrigin != want {
		t.Errorf("期望来源链 %q, 实际 %q", want, c.ConstraintOrigin)
	}
}

func TestApplyConstraints_nil来源与nil配置无副作用(t *testing.T) {
	c := &Config{DisabledTools: map[string]bool{"X": true}}
	if err := c.ApplyConstraints(nil); err != nil {
		t.Fatalf("nil 来源应是 no-op: %v", err)
	}
	if len(c.DisabledTools) != 1 || c.AllowedTools != nil {
		t.Errorf("nil 来源改动了配置: %+v / %v", c.DisabledTools, c.AllowedTools)
	}
	var nilCfg *Config
	if err := nilCfg.ApplyConstraints(&fakeConstraints{origin: "x"}); err != nil {
		t.Errorf("nil 配置不应 panic 或报错: %v", err)
	}
}

func TestApplyConstraints_不设名单时与改造前逐项等价(t *testing.T) {
	// 现有工作流/角色/下游平台不声明任何约束 —— 装配一个空约束后, 配置必须与
	// 完全没调用过 ApplyConstraints 时一模一样, 一个工具都不能凭空变严。
	base := &Config{}
	got := &Config{}
	if err := got.ApplyConstraints(&fakeConstraints{}); err != nil {
		t.Fatalf("空约束应是 no-op: %v", err)
	}
	if got.DisabledTools != nil || got.AllowedTools != nil || got.PermissionMode != base.PermissionMode {
		t.Errorf("空约束改变了配置: disabled=%v allowed=%v perm=%q", got.DisabledTools, got.AllowedTools, got.PermissionMode)
	}
	for _, name := range []string{"Read", "Shell", "TeamCreate"} {
		if got.toolExposed(name) != base.toolExposed(name) {
			t.Errorf("工具 %s 的可见性变了", name)
		}
	}
}
