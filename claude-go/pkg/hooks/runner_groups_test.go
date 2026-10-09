// runner_groups_test.go —— 21.8.12 P3「hook 进池」运行期半边 (装的进 / 看得见 / 卸的掉)。
//
// 为什么这些断言必须存在: 池化 hook 的**全部行为**都压在 findHooks 一处 (见它的
// 注释)。装上了但 findHooks 看不见 = 池里列着它、pool_load 回"已加载"、装配读数
// wired、执行一次都没发生 —— 而这与"操作员压根没配这条 hook"在外部**长得一样**。
// 所以这里的每条断言都写成"能不能看见并执行", 而不是"InstallHookGroup 返回值对不对"。
package hooks

import (
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func hc(event types.HookEvent, cmd string) types.HookConfig {
	return types.HookConfig{Event: event, Command: cmd}
}

// TestFindHooksSeesInstalledGroup 是这一组里唯一端到端的那条。
//
// 变异法: 删掉 findHooks 里 `appendMatches(r.groups[name])` 那两行 ⇒ 本用例变红
// (findHooks 只回基线那一条)。这正是"组装了但没人看得见"的形态。
func TestFindHooksSeesInstalledGroup(t *testing.T) {
	r := NewRunner([]types.HookConfig{hc(types.HookEventPreToolUse, "baseline")}, "s1")

	if got := len(r.findHooks(types.HookEventPreToolUse, "Bash")); got != 1 {
		t.Fatalf("装组之前: PreToolUse 匹配数 = %d, 期望 1 (只有基线)", got)
	}

	n := r.InstallHookGroup("guard", []types.HookConfig{
		hc(types.HookEventPreToolUse, "grp-a"),
		hc(types.HookEventPostTurn, "grp-b"),
	})
	if n != 2 {
		t.Fatalf("InstallHookGroup 返回 %d, 期望 2", n)
	}

	got := r.findHooks(types.HookEventPreToolUse, "Bash")
	if len(got) != 2 {
		t.Fatalf("装组之后: PreToolUse 匹配数 = %d, 期望 2 (基线 + 组内 1 条)", len(got))
	}
	if got[1].Command != "grp-a" {
		t.Errorf("第二条 = %q, 期望 grp-a —— 组里的 hook 没有被 findHooks 看见", got[1].Command)
	}

	// 事件过滤必须对组**同样**生效: 组里有 PostTurn 那条, 不该出现在 PreToolUse 上。
	for _, h := range got {
		if h.Event != types.HookEventPreToolUse {
			t.Errorf("PreToolUse 的匹配列表里混进了 %s 的 hook", h.Event)
		}
	}
}

// TestFindHooksOrderIsBaselineThenLoadOrder —— 顺序是语义: 同一事件上多条 hook,
// 谁先 deny 直接改变结果 (executeHook 见 deny 即返回)。map 遍历序会让它不可复现。
func TestFindHooksOrderIsBaselineThenLoadOrder(t *testing.T) {
	r := NewRunner([]types.HookConfig{hc(types.HookEventPreToolUse, "base")}, "s1")
	// 装入序刻意与字典序**相反** (zeta 先于 alpha): 若实现回落到 map 序或排序,
	// 期望序就与字典序颠倒, 本用例变红。
	r.InstallHookGroup("zeta", []types.HookConfig{hc(types.HookEventPreToolUse, "z")})
	r.InstallHookGroup("alpha", []types.HookConfig{hc(types.HookEventPreToolUse, "a")})

	got := r.findHooks(types.HookEventPreToolUse, "Bash")
	var gotCmds []string
	for _, h := range got {
		gotCmds = append(gotCmds, h.Command)
	}
	want := []string{"base", "z", "a"}
	if len(gotCmds) != len(want) {
		t.Fatalf("匹配数 = %d, 期望 %d (%v)", len(gotCmds), len(want), gotCmds)
	}
	for i := range want {
		if gotCmds[i] != want[i] {
			t.Fatalf("生效序 = %v, 期望 %v (基线在前, 组按**装入序**)", gotCmds, want)
		}
	}
}

// TestUninstallHookGroupLeavesBaselineIntact —— 卸组只作用于组。
//
// 变异法: 让 UninstallHookGroup 顺手清 r.configs ⇒ 变红。基线是操作员配的, 模型
// 的一次 pool_release 把操作员的 hook 一起丢掉, 是一条没有报错的权限越界。
func TestUninstallHookGroupLeavesBaselineIntact(t *testing.T) {
	r := NewRunner([]types.HookConfig{hc(types.HookEventPreToolUse, "base")}, "s1")
	r.InstallHookGroup("g1", []types.HookConfig{hc(types.HookEventPreToolUse, "g")})

	if !r.UninstallHookGroup("g1") {
		t.Fatal("卸载已装入的组返回 false")
	}
	if r.UninstallHookGroup("g1") {
		t.Error("重复卸载同一个组返回 true (第二次没有任何东西被卸掉)")
	}

	got := r.findHooks(types.HookEventPreToolUse, "Bash")
	if len(got) != 1 || got[0].Command != "base" {
		t.Fatalf("卸组之后匹配 = %v, 期望只剩基线 [base]", got)
	}
	if r.BaselineCount() != 1 {
		t.Errorf("BaselineCount = %d, 期望 1 —— 卸组动了基线", r.BaselineCount())
	}

	// 卸掉之后同名再装: 必须重新出现在 GroupNames 里, 且只出现一次。
	r.InstallHookGroup("g1", []types.HookConfig{hc(types.HookEventPreToolUse, "g2")})
	names := r.GroupNames()
	if len(names) != 1 || names[0] != "g1" {
		t.Fatalf("重装后的 GroupNames = %v, 期望 [g1] (groupOrder 里留了重复项?)", names)
	}
	if got := r.findHooks(types.HookEventPreToolUse, "Bash"); len(got) != 2 || got[1].Command != "g2" {
		t.Fatalf("重装后匹配 = %v, 期望 [base g2]", got)
	}
}

// TestInstallHookGroupReplacesSameName —— 同名重装是**替换**不是追加。
//
// 追加的实现会让 pool_load 同一个资产两次装出四条 hook (而池那边 IsLoaded 已经把
// 第二次挡在"无需重复加载"上, 所以这里必须自己也是对的那个)。
func TestInstallHookGroupReplacesSameName(t *testing.T) {
	r := NewRunner(nil, "s1")
	r.InstallHookGroup("g", []types.HookConfig{hc(types.HookEventPreToolUse, "a")})
	r.InstallHookGroup("g", []types.HookConfig{hc(types.HookEventPreToolUse, "b")})

	got := r.findHooks(types.HookEventPreToolUse, "Bash")
	if len(got) != 1 || got[0].Command != "b" {
		t.Fatalf("同名重装后匹配 = %v, 期望只有 [b]", got)
	}
	if r.Count() != 1 {
		t.Errorf("Count = %d, 期望 1 (重装是替换)", r.Count())
	}
}

// TestInstallHookGroupEmptyIsNoop —— 空组不建立。
//
// 为什么要管: 一份解析不出任何 hook 的资产若被装成一个空组, GroupNames 里就有它,
// "这个资产装上了"从读数上看完全成立, 而它一条都不执行。空名同理 (== 卸不掉的组)。
func TestInstallHookGroupEmptyIsNoop(t *testing.T) {
	r := NewRunner(nil, "s1")
	if n := r.InstallHookGroup("empty", nil); n != 0 {
		t.Errorf("装入空配置返回 %d, 期望 0", n)
	}
	if n := r.InstallHookGroup("", []types.HookConfig{hc(types.HookEventPreToolUse, "x")}); n != 0 {
		t.Errorf("空组名返回 %d, 期望 0", n)
	}
	if names := r.GroupNames(); len(names) != 0 {
		t.Errorf("GroupNames = %v, 期望空 —— 空组不该进组表", names)
	}
}

// TestCountVsBaselineCount —— 两个读数回答两个不同的问题, 且都不能把对方算进去。
func TestCountVsBaselineCount(t *testing.T) {
	r := NewRunner([]types.HookConfig{
		hc(types.HookEventPreToolUse, "b1"),
		hc(types.HookEventPostTurn, "b2"),
	}, "s1")
	if r.BaselineCount() != 2 || r.Count() != 2 {
		t.Fatalf("未装组时 Baseline=%d Count=%d, 都期望 2", r.BaselineCount(), r.Count())
	}
	r.InstallHookGroup("g", []types.HookConfig{hc(types.HookEventPreToolUse, "g1")})
	if r.BaselineCount() != 2 {
		t.Errorf("装组后 BaselineCount = %d, 期望仍是 2 —— 基线读数被池化污染了", r.BaselineCount())
	}
	if r.Count() != 3 {
		t.Errorf("装组后 Count = %d, 期望 3 (基线 + 组)", r.Count())
	}
}

// TestDescribeCarriesGroupAttribution —— 装配报告必须区分"操作员配的"与"模型装的"。
//
// 变异法: 删掉 HookSummary.Group 的赋值 (或让 Describe 直接遍历 r.configs) ⇒ 变红。
// 分不出来的后果是回退时不知道该卸哪些 —— 而回退正是一次性卸掉**组**的那一步。
func TestDescribeCarriesGroupAttribution(t *testing.T) {
	r := NewRunner([]types.HookConfig{hc(types.HookEventPreToolUse, "base")}, "s1")
	r.InstallHookGroup("guard", []types.HookConfig{hc(types.HookEventPostTurn, "g")})

	d := r.Describe()
	if len(d) != 2 {
		t.Fatalf("Describe 条数 = %d, 期望 2 (组里的 hook 在报告里**必须**出现)", len(d))
	}
	if d[0].Group != "" {
		t.Errorf("基线那条的 Group = %q, 期望空", d[0].Group)
	}
	if d[1].Group != "guard" {
		t.Errorf("组里那条的 Group = %q, 期望 guard", d[1].Group)
	}
	if d[1].Event != string(types.HookEventPostTurn) {
		t.Errorf("组里那条的 Event = %q, 期望 PostTurn", d[1].Event)
	}
}

// TestFindHooksConcurrentWithInstall —— 锁是**承重**的, 不是防御性的。
//
// 工具调用是并发批次 (pkg/tool/orchestration.go runConcurrentBatch), 每个分支都调
// findHooks; pool_load 在另一个 goroutine 上装组。map 的并发读写是 fatal, 不是
// 偶发错误 —— 所以本用例要在 `go test -race` 下变红才算数 (不带 -race 时它多数
// 时候只是碰运气)。
func TestFindHooksConcurrentWithInstall(t *testing.T) {
	r := NewRunner([]types.HookConfig{hc(types.HookEventPreToolUse, "base")}, "s1")
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.findHooks(types.HookEventPreToolUse, "Bash")
				_ = r.Count()
				_ = r.Describe()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "g" + string(rune('a'+i))
			for j := 0; j < 100; j++ {
				r.InstallHookGroup(name, []types.HookConfig{hc(types.HookEventPreToolUse, "x")})
				r.UninstallHookGroup(name)
			}
		}(i)
	}
	wg.Wait()
}

// TestNilRunnerIsSafe —— nil Runner 上这三个方法都要能调 (调用方在装配失败的路径上
// 会拿它兜底)。InstallHookGroup 返回 0 而不是 panic, 这是"没生效"的如实读数。
func TestNilRunnerIsSafe(t *testing.T) {
	var r *Runner
	if n := r.InstallHookGroup("g", []types.HookConfig{hc(types.HookEventPreToolUse, "x")}); n != 0 {
		t.Errorf("nil Runner 装入返回 %d, 期望 0", n)
	}
	if r.UninstallHookGroup("g") {
		t.Error("nil Runner 卸载返回 true")
	}
	if r.Count() != 0 || r.BaselineCount() != 0 {
		t.Error("nil Runner 的读数非 0")
	}
}
