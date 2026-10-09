package agent

// workflow_revision_test.go —— 21.8.12 P3 的 `U` 版本守卫回归测试。
//
// ---------------------------------------------------------------------------
// 这条守卫挡的是什么
// ---------------------------------------------------------------------------
//
// 改造前 RegisterWorkflow 是创建与改写共用的一个口: 同名再来一次 = Lock 之后直接
// `customWFs[name] = &cp`, **后到者静默覆盖先到者, 双方都不知情**。本文件每一条
// 用例都对应那个行为的一个具体后果 —— 而它们全是"没有报错"的那种:
//
//	静默覆盖      → 被覆盖的那一版不可撤销 (而进化引擎的 rollback 依赖旧版还在)
//	过期改写      → 两个人同时编辑, 后保存的把先保存的整份吃掉
//	装载也 +1     → 每次重启 revision 都涨, 客户端手里刚取的值在无人改动时失效
//	落盘不带新版  → 内存里是 r5, 重启后变回 r1, 守卫开始拒绝本该接受的提交
//
// 判据全部取自**注册表与磁盘的读数**, 不取返回值 —— 返回一个 Revision 而没真的
// 存下来, 是本仓库反复出现的那类"自述不能当判据"。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// revFixture 造一个可注册的最小动态工作流。prompt 用来区分版本。
func revFixture(name, prompt string) *WorkflowDef {
	return &WorkflowDef{
		Name: name, Mode: "pipeline",
		Stages: []StageDef{{Name: "s1", Agent: "coder", Prompt: prompt}},
	}
}

// TestWorkflowRevisionStartsAtOne 新建的工作流 GET 得到 r1, 而不是 0。
//
// 0 是"没带版本"这个取值 (守卫据此报"未携带 revision")。若新建后 GET 出 0,
// 客户端把它原样回传时服务端分不开"没带"与"带了个合法的初始版本", 于是一次
// 正确的改写会被判成漏字段。
func TestWorkflowRevisionStartsAtOne(t *testing.T) {
	const name = "rev-start"
	cleanupWF(t, name)
	roles := NewRoleRegistry(t.TempDir())
	if err := RegisterWorkflow(revFixture(name, "v1 {objective}"), roles); err != nil {
		t.Fatal(err)
	}
	if got := RevisionOf(name); got != 1 {
		t.Fatalf("新建后版本应为 1 (可回传的期望值), got %d", got)
	}
}

// TestWorkflowReregisterIsRejectedNotSilent 同名再走创建口必须**报错**, 且原样保留。
//
// 这是"静默覆盖"那个洞本身。断言两件事: ① 报的是版本冲突 (dashboard 侧映射成 409,
// 前端据此提示"刷新重试"而不是"改你的定义"); ② 注册表里的内容**一个字都没变** ——
// 只断言报错是不够的, 先写后判的实现同样会报错。
func TestWorkflowReregisterIsRejectedNotSilent(t *testing.T) {
	const name = "rev-reregister"
	cleanupWF(t, name)
	roles := NewRoleRegistry(t.TempDir())
	if err := RegisterWorkflow(revFixture(name, "第一版 {objective}"), roles); err != nil {
		t.Fatal(err)
	}
	err := RegisterWorkflow(revFixture(name, "第二版 {objective}"), roles)
	if err == nil {
		t.Fatal("同名再注册必须被拒; 静默覆盖会让被覆盖的那一版不可撤销")
	}
	if !errors.Is(err, ErrWorkflowRevisionConflict) {
		t.Fatalf("应报版本冲突哨兵错误, got %v", err)
	}
	got := getCustomWorkflow(name)
	if got == nil {
		t.Fatal("被拒的注册不得删掉原有定义")
	}
	if got.Stages[0].Prompt != "第一版 {objective}" {
		t.Fatalf("被拒的注册不得改动原有定义, got %q", got.Stages[0].Prompt)
	}
	if got.Revision != 1 {
		t.Fatalf("被拒的注册不得改动版本号, got %d", got.Revision)
	}
}

// TestWorkflowUpdateRejectsStaleRevision 过期版本必须被拒 —— 这是"两个人同时编辑"那条。
//
// 关键断言是**内容不变**: 版本冲突时最坏的结果不是"没保存", 而是"保存了但报冲突",
// 用户会以为自己的改动没生效, 实际已经覆盖了别人。
func TestWorkflowUpdateRejectsStaleRevision(t *testing.T) {
	const name = "rev-stale"
	cleanupWF(t, name)
	roles := NewRoleRegistry(t.TempDir())
	if err := RegisterWorkflow(revFixture(name, "v1 {objective}"), roles); err != nil {
		t.Fatal(err)
	}
	// 甲先改一步: r1 → r2。
	if _, err := UpdateWorkflow(revFixture(name, "甲改的 {objective}"), roles, 1); err != nil {
		t.Fatal(err)
	}
	// 乙手上还是 r1, 提交必须被拒。
	_, err := UpdateWorkflow(revFixture(name, "乙改的 {objective}"), roles, 1)
	if err == nil {
		t.Fatal("过期版本必须被拒; 否则后保存的会把先保存的整份吃掉")
	}
	if !errors.Is(err, ErrWorkflowRevisionConflict) {
		t.Fatalf("应报版本冲突, got %v", err)
	}
	got := getCustomWorkflow(name)
	if got.Stages[0].Prompt != "甲改的 {objective}" {
		t.Fatalf("被拒的改写不得覆盖已存在的内容, got %q", got.Stages[0].Prompt)
	}
	if got.Revision != 2 {
		t.Fatalf("被拒的改写不得推进版本号, got %d", got.Revision)
	}
}

// TestWorkflowUpdateVersionIsNotOptional 不带版本 (0 / 缺字段) 的改写必须被拒。
//
// 一个可以省略的守卫等于没有守卫: 省略时若退化成"强制覆盖", 那么任何忘了带版本的
// 调用方都会拿到改造前那个静默覆盖的行为, 而它看起来与成功一模一样。
func TestWorkflowUpdateVersionIsNotOptional(t *testing.T) {
	const name = "rev-optional"
	cleanupWF(t, name)
	roles := NewRoleRegistry(t.TempDir())
	if err := RegisterWorkflow(revFixture(name, "v1 {objective}"), roles); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []int64{0, -1} {
		if _, err := UpdateWorkflow(revFixture(name, "覆盖尝试 {objective}"), roles, rev); err == nil {
			t.Fatalf("expectRevision=%d 必须被拒 (不能退化成强制覆盖)", rev)
		} else if !errors.Is(err, ErrWorkflowRevisionConflict) {
			t.Fatalf("expectRevision=%d 应报版本冲突, got %v", rev, err)
		}
	}
	if got := getCustomWorkflow(name); got.Stages[0].Prompt != "v1 {objective}" {
		t.Fatalf("被拒的改写不得改动内容, got %q", got.Stages[0].Prompt)
	}
}

// TestWorkflowUpdateBumpsRevisionOnSuccess 成功的改写把版本推进 1, 且只推进 1。
//
// "只推进 1" 值得单独断言: 推 2 (或按内容哈希推进) 会让客户端"改一次再取一次"
// 的常规回路在任何一次成功改写后立刻过期 —— 表现是"我明明刚保存过, 再保存就冲突了"。
func TestWorkflowUpdateBumpsRevisionOnSuccess(t *testing.T) {
	const name = "rev-bump"
	cleanupWF(t, name)
	roles := NewRoleRegistry(t.TempDir())
	if err := RegisterWorkflow(revFixture(name, "v1 {objective}"), roles); err != nil {
		t.Fatal(err)
	}
	out, err := UpdateWorkflow(revFixture(name, "v2 {objective}"), roles, 1)
	if err != nil {
		t.Fatal(err)
	}
	if out.Revision != 2 {
		t.Fatalf("返回的版本应为 2, got %d", out.Revision)
	}
	if got := RevisionOf(name); got != 2 {
		t.Fatalf("注册表里的版本应为 2, got %d", got)
	}
	if got := getCustomWorkflow(name); got.Revision != 2 {
		t.Fatalf("取回的副本版本也应为 2, got %d", got.Revision)
	}
}

// TestWorkflowReloadDoesNotBumpRevision 装载**不是**改写: 重启不推进版本号。
//
// 这条是最容易写错、也最难在开发机上发现的一条 —— 本地进程通常不重启, 而线上
// 每次滚动更新都重启一次。若装载也 +1, 客户端手里那个"刚 GET 到的版本"会在一次
// 无人改动的重启之后失效, 版本号于是不再表示"内容变的次数"而表示"进程起过几次",
// 守卫退化成随机拒绝 —— 而随机拒绝会被当成"偶发网络问题"重试掉。
func TestWorkflowReloadDoesNotBumpRevision(t *testing.T) {
	const name = "rev-reload"
	cleanupWF(t, name)
	dir := t.TempDir()
	roles := NewRoleRegistry(t.TempDir())

	def := revFixture(name, "v1 {objective}")
	if err := RegisterWorkflow(def, roles); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateWorkflow(revFixture(name, "v2 {objective}"), roles, 1); err != nil {
		t.Fatal(err)
	}
	// 落盘 (以注册表那一份为准, 它带着 r2)。
	registered := getCustomWorkflow(name)
	if registered.Revision != 2 {
		t.Fatalf("改写后内存里应是 r2, got %d", registered.Revision)
	}
	if err := SaveWorkflowToDir(dir, registered); err != nil {
		t.Fatal(err)
	}

	// 模拟重启: 清空注册表, 从磁盘装载。
	cleanupWF(t, name)
	loaded, failed, details := LoadWorkflowsFromDir(dir, roles)
	if loaded != 1 || failed != 0 {
		t.Fatalf("装载应成功 1 份, got loaded=%d failed=%d %v", loaded, failed, details)
	}
	if got := RevisionOf(name); got != 2 {
		t.Fatalf("重启后版本必须仍是 2 —— 装载不是改写, got %d", got)
	}
	// 装载出来的内容也必须是改写后的那一版 (否则版本对了、内容是旧的)。
	if got := getCustomWorkflow(name); got.Stages[0].Prompt != "v2 {objective}" {
		t.Fatalf("装载应取回最新内容, got %q", got.Stages[0].Prompt)
	}
	// 装载后带 r2 的改写必须成功 (守卫认得这个值)。
	if _, err := UpdateWorkflow(revFixture(name, "v3 {objective}"), roles, 2); err != nil {
		t.Fatalf("装载后带正确版本的改写应成功: %v", err)
	}
}

// TestWorkflowBuiltinCannotBeUpdated 内置工作流不可改写。
//
// 内置由 Go 工厂产出, 覆盖掉只会在下次重启时"自己变回去" —— 中间那段时间的行为
// 差异没有任何地方记录, 而用户以为改生效了。
func TestWorkflowBuiltinCannotBeUpdated(t *testing.T) {
	cleanupWF(t, "development")
	roles := NewRoleRegistry(t.TempDir())
	if _, err := UpdateWorkflow(revFixture("development", "覆盖内置 {objective}"), roles, 1); err == nil {
		t.Fatal("内置工作流必须不可改写")
	} else if errors.Is(err, ErrWorkflowRevisionConflict) {
		// 报成"版本冲突"会把用户引向"刷新重试", 而重试多少次都不会成功。
		t.Fatalf("内置不可改写应报自己的原因, 不是版本冲突: %v", err)
	}
}

// TestWorkflowUpdateUnknownIsNotCreate 对不存在的名字改写必须报错, 不是顺手创建。
//
// 顺手创建会让一个打错名字的 PUT 静默产出一份新工作流, 而调用方以为自己改的是
// 旧的那份 —— 于是"改了没生效"与"多出来一份"同时发生。
func TestWorkflowUpdateUnknownIsNotCreate(t *testing.T) {
	const name = "rev-not-exist"
	cleanupWF(t, name)
	roles := NewRoleRegistry(t.TempDir())
	if _, err := UpdateWorkflow(revFixture(name, "x {objective}"), roles, 1); err == nil {
		t.Fatal("对不存在的名字改写必须报错, 不得顺手创建")
	}
	if getCustomWorkflow(name) != nil {
		t.Fatal("被拒的改写不得留下任何注册")
	}
}

// TestWorkflowRevisionPersistedToDiskIsTheRegisteredValue 落盘文件里的 revision
// 必须等于注册表里的值, 而不是请求体里客户端传的那个。
//
// 服务端算出的是 expect+1, 客户端传的是 expect。若落盘用请求体, 磁盘上会长期落后
// 一版 —— 进程不重启时看不出来 (内存才是权威), 重启之后版本倒退到旧值, 于是守卫
// 开始拒绝本该接受的提交。这是那种"上线当天没事、下次发版才炸"的写法。
func TestWorkflowRevisionPersistedToDiskIsTheRegisteredValue(t *testing.T) {
	const name = "rev-persist"
	cleanupWF(t, name)
	dir := t.TempDir()
	roles := NewRoleRegistry(t.TempDir())
	if err := RegisterWorkflow(revFixture(name, "v1 {objective}"), roles); err != nil {
		t.Fatal(err)
	}
	updated, err := UpdateWorkflow(revFixture(name, "v2 {objective}"), roles, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveWorkflowToDir(dir, updated); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk WorkflowDef
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Revision != 2 {
		t.Fatalf("磁盘上的 revision 应为 2 (与注册表一致), got %d", onDisk.Revision)
	}
}

// cleanupWF 从自定义注册表里摘掉一个工作流 (连同它的图)。
//
// 用 UnregisterWorkflow 而不是直接删 map: 后者会把注册的图留在 pkg/graph 里,
// 而图是**全局**的 —— 上一个用例留下的同名图会改变下一个用例里
// registerWorkflowGraph 的行为, 这类串台在本仓出现过 (见 cleanupGraphs)。
func cleanupWF(t *testing.T, name string) {
	t.Helper()
	_ = UnregisterWorkflow(name)
	t.Cleanup(func() { _ = UnregisterWorkflow(name) })
}
