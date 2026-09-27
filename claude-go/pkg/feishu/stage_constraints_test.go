package feishu

// stage_constraints_test.go —— 21.8.12 P3 / W5: 阶段工具声明在**宿主侧**的落地。
//
// ---------------------------------------------------------------------------
// 为什么这一层要单独测
// ---------------------------------------------------------------------------
//
// pkg/agent 那边测的是"声明能被读出来"(ApplyStageTools / 跨图往返 / 真图执行)。
// 这里测的是另一件事: 当**两条来源同时存在**时谁赢。两条来源是:
//
//	覆盖表  hints.ToolProfile      (图节点/灰度期, 运维写的)
//	定义    StageDef.Tools         (工作流作者写的, 经 ctx 下发)
//
// 三条优先级规则各自有一个"写错了也不报错"的失效形态, 而它们在**一次真实 run 的
// 读数上完全看不出来** —— 三种失效都只表现为"这个 stage 拿到了一组可见工具",
// 没有报错、没有日志差异、产物也正常:
//
//	规则 1 覆盖表的档位压得住定义      ⇒ 失效时: 灰度期改不动线上行为, 且看起来"改了"
//	规则 2 名单/黑名单**无条件**应用   ⇒ 失效时: 一个只想改档位的覆盖表顺手把定义里
//	                                     那份白名单整份丢掉 —— 而丢掉白名单 = 放宽
//	规则 3 两边都没声明时一个字节不动  ⇒ 失效时: 6 个下游平台 + 40 条在野工作流被静默改行为
//
// 抽成 stageConstraints 纯函数正是为了这三条能被直接喂输入断言输出 (与
// stageMaxTurns 抽函数是同一条理由, 见 stage_maxturns_test.go)。

import (
	"context"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// TestStageConstraints覆盖表的档位压得住定义 规则 1。
func TestStageConstraints覆盖表的档位压得住定义(t *testing.T) {
	allow := []string{"Read", "Grep"}
	ctx := agent.WithStageTools(context.Background(), &agent.StageTools{
		Profile: string(builtin.ToolProfileAnalysis), Allow: &allow, Deny: []string{"Bash"},
	})

	cs := stageConstraints(ctx, agent.NodeExecHints{ToolProfile: "coding"}, "impl", "coder")

	if got := cs.ToolProfile(); got != string(builtin.ToolProfileCoding) {
		t.Fatalf("覆盖表的档位必须压住定义里声明的档位, got %q (定义声明的是 analysis)", got)
	}
	if got := cs.ToolProfileSource(); got != agent.ProfileSourceExplicit {
		t.Fatalf("两条来源都是显式的, 来源应为 explicit, got %q", got)
	}
}

// TestStageConstraints覆盖表只改档位不丢名单 规则 2 —— 本文件里最要紧的一条。
//
// 三个维度里只有**档位**会冲突 (图路径下覆盖表与定义描述的是同一个 stage);
// 名单与黑名单各自只会收窄, 没有冲突可言, 所以必须无条件应用。
//
// 把"覆盖表优先"一并套到名单上, 结果是: 一次只想把 coding 改成 analysis 的灰度,
// 会顺手把定义里那份 `tools: [Read, Grep]` 白名单**整份丢掉** —— 于是一个原本只能读
// 两个工具的 stage 变成了"analysis 档下所有工具"。这是**放宽**, 而且发生在一个
// 明确写着"收窄"的操作上。
func TestStageConstraints覆盖表只改档位不丢名单(t *testing.T) {
	allow := []string{"Read", "Grep"}
	ctx := agent.WithStageTools(context.Background(), &agent.StageTools{
		Profile: string(builtin.ToolProfileAnalysis), Allow: &allow, Deny: []string{"Bash"},
	})

	cs := stageConstraints(ctx, agent.NodeExecHints{ToolProfile: "coding"}, "impl", "coder")

	m := cs.CompiledToolAllow()
	if m == nil {
		t.Fatal("覆盖表只声明了档位, 定义里的白名单被整份丢掉了 —— 这是放宽而不是收窄")
	}
	if len(m) != 2 || !m["Read"] || !m["Grep"] {
		t.Fatalf("定义里的白名单应原样生效, got %v", m)
	}
	if d := cs.CompiledToolDeny(); !d["Bash"] {
		t.Fatalf("定义里的黑名单也应原样生效, got %v", d)
	}
}

// TestStageConstraints只有覆盖表时不凭空造名单 覆盖表存在而定义没声明。
//
// 这条挡的是相反方向的错法: 为了让"名单照常应用"成立而写成一个恒真的赋值, 例如
// `toApply = &StageTools{Allow: stageTools.Allow}` 在 stageTools 为 nil 时 panic,
// 或者更隐蔽地 —— 用一个默认非 nil 的空切片兜底, 于是"没声明"被翻译成"拒绝一切"。
func TestStageConstraints只有覆盖表时不凭空造名单(t *testing.T) {
	cs := stageConstraints(context.Background(), agent.NodeExecHints{ToolProfile: "coding"}, "impl", "coder")

	if got := cs.ToolProfile(); got != string(builtin.ToolProfileCoding) {
		t.Fatalf("覆盖表的档位应生效, got %q", got)
	}
	if m := cs.CompiledToolAllow(); m != nil {
		t.Fatalf("没有任何来源声明白名单, 它必须保持 nil (= 不限制), got %v", m)
	}
	if d := cs.CompiledToolDeny(); len(d) != 0 {
		t.Fatalf("没有任何来源声明黑名单, got %v", d)
	}
}

// TestStageConstraints不声明时一个字节不动 规则 3 —— "6 个下游平台行为不变"的回归面。
//
// 断言的是 ConstraintSet 的**初始态**而不是某个取值: 档位必须留空 (而不是留一个
// 猜出来的默认值), 因为留空正是"交给 resolveToolProfile 去回退"的信号。
// 一个"体贴"的默认值会把回退闭包整个旁路掉, 而回退闭包里还挂着 deprecation 留痕 ——
// 于是角色名推断被静默执行却不再有人知道。
func TestStageConstraints不声明时一个字节不动(t *testing.T) {
	cs := stageConstraints(context.Background(), agent.NodeExecHints{}, "impl", "tech-investigator")

	if got := cs.ToolProfile(); got != "" {
		t.Fatalf("没有任何声明时档位必须留空以便回退, got %q", got)
	}
	if got := cs.ToolProfileSource(); got != agent.ProfileSourceUnset {
		t.Fatalf("来源必须是 unset, got %q —— 非 unset 会让回退闭包不再被调用", got)
	}
	if m := cs.CompiledToolAllow(); m != nil {
		t.Fatalf("不该凭空引入白名单, got %v", m)
	}
	if d := cs.CompiledToolDeny(); len(d) != 0 {
		t.Fatalf("不该凭空引入黑名单, got %v", d)
	}
	if got := cs.ConstraintOrigin(); got != "team-role:tech-investigator" {
		t.Fatalf("来源链应仍是 team-role:<角色> (审计用), got %q", got)
	}
}

// TestStageConstraints拼错的覆盖表不静默降权 覆盖表这一维的既有约定在新增路径上延续。
//
// mapExplicitToolProfile 对未知值返回 "", 于是走回退 —— 一个拼错的 tool_profile
// 不应静默剥掉 agent 的工具。这条在新路径上要重新钉一次, 因为 stageConstraints 是
// 新写的调用点: 它如果改成"非空即 WithToolProfile", 同一条拼错的值就会带着
// ProfileSourceExplicit 落地成空档, 回退闭包再也不被调用。
func TestStageConstraints拼错的覆盖表不静默降权(t *testing.T) {
	cs := stageConstraints(context.Background(), agent.NodeExecHints{ToolProfile: "codingg"}, "impl", "coder")

	// 覆盖表这条来源没能定下档位 ⇒ 应留给回退。
	if got := cs.ToolProfile(); got != "" {
		t.Fatalf("拼错的覆盖表不该定下档位 (应回落推断), got %q", got)
	}
	prof := resolveToolProfile(cs, agent.ProfileSourceRoleNameFallback, func() builtin.ToolProfile {
		return profileForTeamRole("coder", "")
	})
	if want := profileForTeamRole("coder", ""); prof != want {
		t.Fatalf("拼错的覆盖表应回落到角色名推断 %q, got %q", want, prof)
	}
}

// TestStageConstraints阶段只声明名单时档位仍走回退 声明是按维度独立的, 不是整体替换。
//
// 这是 W5 的设计要点: 一个只写 `tools: [...]` 的阶段并没有对**档位**表态, 所以档位
// 仍应由宿主按既有规则决定。把它当成"声明了工具面"从而一并定下档位, 会让一个只想
// 收窄名单的作者顺手改掉了档位 —— 而档位决定的是 schema 体积与系统提示, 两回事。
func TestStageConstraints阶段只声明名单时档位仍走回退(t *testing.T) {
	allow := []string{"Read"}
	ctx := agent.WithStageTools(context.Background(), &agent.StageTools{Allow: &allow})

	cs := stageConstraints(ctx, agent.NodeExecHints{}, "dig", "world-builder")

	if got := cs.ToolProfile(); got != "" {
		t.Fatalf("只声明了名单不等于对档位表态, 档位应留空, got %q", got)
	}
	m := cs.CompiledToolAllow()
	if len(m) != 1 || !m["Read"] {
		t.Fatalf("名单应已生效, got %v", m)
	}
	prof := resolveToolProfile(cs, agent.ProfileSourceRoleNameFallback, func() builtin.ToolProfile {
		return profileForTeamRole("world-builder", "")
	})
	if prof == "" {
		t.Fatal("档位仍应由回退定下来")
	}
}

// TestStageConstraints两边并存且定义没声明白名单时不凭空造一个 —— 三态在**重建分支**上的复核。
//
// 上面那条"只有覆盖表时"走的是 `stageTools == nil` 的短路, 压根到不了重建那一行;
// 而重建那一行 (`overrideDeclaredProfile` 为真时重造一个 StageTools) 恰恰是三态最容易
// 被压平的地方: 顺手写一个 `allow := []string{}` 兜底来避免解引用 nil, 就把
// "没声明白名单"翻译成了"拒绝一切"。
//
// 这条是变异测试补出来的: 把重建里的 Allow 换成"nil 即给空切片", 上面所有用例
// **全绿** —— 因为能走到重建分支的用例一个都没有。缺的正是这一格 (两条来源都在,
// 且定义只声明了黑名单)。
func TestStageConstraints两边并存且定义没声明白名单时不凭空造一个(t *testing.T) {
	ctx := agent.WithStageTools(context.Background(), &agent.StageTools{Deny: []string{"Bash"}})

	cs := stageConstraints(ctx, agent.NodeExecHints{ToolProfile: "analysis"}, "impl", "coder")

	if m := cs.CompiledToolAllow(); m != nil {
		t.Fatalf("定义没声明白名单 (Allow 为 nil), 重建后必须仍是 nil (= 不限制); "+
			"非 nil 的空名单语义是「拒绝一切」, got %v", m)
	}
	if d := cs.CompiledToolDeny(); !d["Bash"] {
		t.Fatalf("定义声明的黑名单在黑名单维度上应照常生效, got %v", d)
	}
	if got := cs.ToolProfile(); got != string(builtin.ToolProfileAnalysis) {
		t.Fatalf("覆盖表的档位应生效, got %q", got)
	}
}

// TestStageConstraints展开式空白名单拒绝一切 三态在宿主侧的端到端确认。
//
// ApplyStageTools 那一侧已有单测; 这里再走一遍 stageConstraints, 是因为中间多了
// "覆盖表并存时重建 StageTools"那一步 —— `&agent.StageTools{Allow: stageTools.Allow, ...}`
// 这一行如果把 Allow 写成 `*stageTools.Allow` 或漏掉, 空白名单就被翻译成 nil (不限制),
// 一份 fail-closed 声明静默变成完全放行。
func TestStageConstraints展开式空白名单拒绝一切(t *testing.T) {
	empty := []string{}
	ctx := agent.WithStageTools(context.Background(), &agent.StageTools{Allow: &empty})

	// 同时给覆盖表档位, 强制走"重建 StageTools"那条分支。
	cs := stageConstraints(ctx, agent.NodeExecHints{ToolProfile: "analysis"}, "impl", "coder")

	m := cs.CompiledToolAllow()
	if m == nil {
		t.Fatal("tools: [] 必须落成**非 nil 的空白名单**; nil 的语义是「不限制」, 正好相反")
	}
	if len(m) != 0 {
		t.Fatalf("空名单不该含任何工具, got %v", m)
	}
}
