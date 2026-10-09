// pooltools_hook_test.go —— 21.8.12 P3「hook 进池」的池侧验收。
//
// 这一组与 pkg/hooks/runner_groups_test.go 是**两侧独立读数**: 那边测 Runner 能不能
// 看见装进来的组, 这边测池有没有把 hook 交到 Runner 手上。两侧都要, 因为
// "hook 进池"这条链路上有四个各自独立失联的点 (登记进池 / 进检索索引 / load 交出去 /
// release 收回来), 每一个坏掉的表现都是**同一个**: 池里列着那条 hook、加载回成功、
// 而它一次都不执行。所以每条断言都钉在"Runner 上到底有没有这组"这个可执行的读数上,
// 而不是包装三件的返回值。
//
// 池侧与 Runner 侧的接口是 tool.HookGroupSink; 这里用一个**记名假实现**代替真 Runner:
// 真 Runner 会 spawn 进程 (command 类型), 那属于 runner_groups_test.go 的范围。
package builtin

import (
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// fakeHookSink 记录装卸调用, 不做任何执行 —— "交到手上"这件事的可观测面。
type fakeHookSink struct {
	mu       sync.Mutex
	installs []string
	uninstal []string
	// refuse 命中 = 这次装入"失败"(返回 0), 用来走内部不一致分支。
	refuse map[string]bool
	// cfgs 最后一次装入的配置, 供断言"装进去的是解析出来的那几条"。
	cfgs map[string][]types.HookConfig
}

func (f *fakeHookSink) InstallHookGroup(name string, cfgs []types.HookConfig) int {
	if f.refuse[name] {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs = append(f.installs, name)
	if f.cfgs == nil {
		f.cfgs = map[string][]types.HookConfig{}
	}
	f.cfgs[name] = cfgs
	return len(cfgs)
}

func (f *fakeHookSink) UninstallHookGroup(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.installs {
		if n == name {
			f.uninstal = append(f.uninstal, name)
			return true
		}
	}
	return false
}

func (f *fakeHookSink) installedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.installs...)
}

func (f *fakeHookSink) uninstalledNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.uninstal...)
}

func (f *fakeHookSink) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs = nil
}

// buildHookAsset 用 ParseContent 真解析一份 hook 资产 (含 hooks 段)。
//
// 走生产同一条解析路径而不是手搓 *asset.Asset: 手搓会绕过 frontmatter, 而
// frontmatter 的**扁平**形态 (pkg/asset.SplitFrontmatter) 正是这套声明最容易写错的
// 地方 —— 绕过它测出来的"能解析"不能代表配置里那样写能解析。
func buildHookAsset(t *testing.T, name, hooksBlock, body, status string) *asset.Asset {
	t.Helper()
	content := "---\nname: " + name + "\n"
	if status != "" {
		content += "status: " + status + "\n"
	}
	content += hooksBlock + "---\n" + body
	a, err := asset.ParseContent(content, "/tmp/hooks/"+name+"/HOOK.md", "state", asset.KindHook)
	if err != nil {
		t.Fatalf("解析 hook 资产 %q 失败: %v\n内容:\n%s", name, err, content)
	}
	return a
}

// hookDecl 拼一段合法的 hooks frontmatter (多行列表形态)。
func hookDecl(lines ...string) string {
	var sb strings.Builder
	sb.WriteString("hooks:\n")
	for _, l := range lines {
		sb.WriteString("  - " + l + "\n")
	}
	return sb.String()
}

// setupHookPool 装配一个含 hook 资产的池, 并把假 sink 挂上去 (模拟
// engine.NewQueryEngine 的注入点)。返回 (reg, pool, sink)。
func setupHookPool(t *testing.T, assets *asset.Set) (*tool.Registry, *tool.Pool, *fakeHookSink) {
	t.Helper()
	reg := tool.NewRegistry()
	reg.Register(&fakeTool{name: "Base1"})
	reg.Register(&fakeTool{name: "evo_inspect"})
	pool := RegisterPoolTools(reg, nil, assets, nil, nil, nil)
	sink := &fakeHookSink{refuse: map[string]bool{}}
	pool.SetHookSink(sink)
	return reg, pool, sink
}

// ---------------------------------------------------------------------------
// 1. 全链: pool_search 命中 → pool_load 真的装进 sink → pool_release 真的卸下来
// ---------------------------------------------------------------------------

func TestHookPoolLoadInstallsIntoHookRunner(t *testing.T) {
	assets := asset.NewSet()
	assets.Add(buildHookAsset(t, "tool-guard",
		hookDecl("PreToolUse:command:./scripts/guard.sh:Bash*"), "拦截危险命令。", ""))

	reg, _, sink := setupHookPool(t, assets)

	// search 必须命中 —— 只登记不检索 = 模型永远看不到这条 hook。
	got := poolCall(t, reg, PoolSearchName, `{"query":"guard","limit":10}`)
	if !strings.Contains(got.Content, "tool-guard") {
		t.Fatalf("pool_search 未命中 hook 资产: %s", got.Content)
	}

	// load 必须真的把配置**原样**交给 sink (不是只标记"已加载")。
	res := poolCall(t, reg, PoolLoadName, `{"name":"tool-guard"}`)
	if res.IsError {
		t.Fatalf("pool_load 返回错误: %s", res.Content)
	}
	if names := sink.installedNames(); len(names) != 1 || names[0] != "tool-guard" {
		t.Fatalf("hook 运行器上装到的组 = %v, 期望 [tool-guard] —— pool_load 只改了加载标记", names)
	}
	cfgs := sink.cfgs["tool-guard"]
	if len(cfgs) != 1 {
		t.Fatalf("装进去的配置条数 = %d, 期望 1", len(cfgs))
	}
	if cfgs[0].Event != types.HookEventPreToolUse || cfgs[0].If != "Bash*" || cfgs[0].Command != "./scripts/guard.sh" {
		t.Fatalf("装进去的配置与声明不符: %+v", cfgs[0])
	}

	// release 必须**真的卸下来** —— 只收标记而留着组, 是一条静默继续运行的 hook。
	rres := poolCall(t, reg, PoolReleaseName, `{"name":"tool-guard"}`)
	if rres.IsError {
		t.Fatalf("pool_release 返回错误: %s", rres.Content)
	}
	if u := sink.uninstalledNames(); len(u) != 1 || u[0] != "tool-guard" {
		t.Fatalf("卸载记录 = %v, 期望 [tool-guard]", u)
	}
}

// TestHookPoolLoadWithoutSinkReportsNotWired —— sink 未注入时必须**如实报不支持**。
//
// 变异法: 把那句 errOnly 换成返回"已加载" ⇒ 变红。这是本批最贵的缺陷形态: 读数说
// 装了, 而执行一次都没发生 (池装配早于 Runner 构造, 漏注入的路径上必然如此)。
func TestHookPoolLoadWithoutSinkReportsNotWired(t *testing.T) {
	assets := asset.NewSet()
	assets.Add(buildHookAsset(t, "tool-guard", hookDecl("PreToolUse:command:./g.sh"), "拦截。", ""))

	reg := tool.NewRegistry()
	reg.Register(&fakeTool{name: "Base1"})
	pool := RegisterPoolTools(reg, nil, assets, nil, nil, nil)
	// 刻意不注入 sink。
	if pool.HookSink() != nil {
		t.Fatal("测试前提不成立: 池已经有了 sink")
	}

	res := poolCall(t, reg, PoolLoadName, `{"name":"tool-guard"}`)
	if !res.IsError {
		t.Fatalf("sink 未注入时 pool_load 未报错 (内容: %s) —— 这会被读成'已加载'", res.Content)
	}
	if !strings.Contains(res.Content, "没有生效") {
		t.Errorf("错误文本未说明'没有生效': %s", res.Content)
	}
}

// TestHookPoolLoadSameNameTwiceIsIdempotent —— 重复加载不该装出两份, 也不该报错;
// 但**必须重装一次**。
//
// 幂等分支重装的理由: 加载标记与 Runner 上的组是两处状态, 会话重建 Runner 后标记
// 还在而组没了 —— 那时回"已在加载面, 无需重复加载"而不重装, 会留下一条永远不生效
// 的 hook。所以这里的断言是"第二次调用**也**碰了 sink", 不是"没碰 sink"。
func TestHookPoolLoadSameNameTwiceIsIdempotent(t *testing.T) {
	assets := asset.NewSet()
	assets.Add(buildHookAsset(t, "tool-guard", hookDecl("PreToolUse:command:./g.sh"), "拦截。", ""))
	reg, _, sink := setupHookPool(t, assets)

	if r := poolCall(t, reg, PoolLoadName, `{"name":"tool-guard"}`); r.IsError {
		t.Fatalf("首次加载失败: %s", r.Content)
	}
	// 模拟"Runner 被重建": 清掉 sink 的装机记录, 加载标记仍在池上。
	sink.forget()

	r := poolCall(t, reg, PoolLoadName, `{"name":"tool-guard"}`)
	if r.IsError {
		t.Fatalf("重复加载报错: %s", r.Content)
	}
	if names := sink.installedNames(); len(names) != 1 {
		t.Fatalf("重复加载后 sink 上的装机次数 = %d, 期望 1 (幂等分支必须重装)", len(names))
	}
}

// TestHookPoolLoadInstallFailureIsNotReportedAsSuccess —— sink 说装入 0 条时必须报错。
func TestHookPoolLoadInstallFailureIsNotReportedAsSuccess(t *testing.T) {
	assets := asset.NewSet()
	assets.Add(buildHookAsset(t, "tool-guard", hookDecl("PreToolUse:command:./g.sh"), "拦截。", ""))
	reg, _, sink := setupHookPool(t, assets)
	sink.refuse["tool-guard"] = true

	res := poolCall(t, reg, PoolLoadName, `{"name":"tool-guard"}`)
	if !res.IsError {
		t.Fatalf("sink 拒装时 pool_load 未报错: %s", res.Content)
	}
	if !strings.Contains(res.Content, "内部状态不一致") {
		t.Errorf("错误文本未点明内部不一致: %s", res.Content)
	}
}

// ---------------------------------------------------------------------------
// 2. 入池门禁: 坏声明 / 影子态 —— 两种都**不进池**
// ---------------------------------------------------------------------------

func TestHookAssetWithBrokenDeclNeverEntersPool(t *testing.T) {
	assets := asset.NewSet()
	// 事件名拼错: 解析器会**成功返回 0 条 + 一条 problem**。
	assets.Add(buildHookAsset(t, "typo-event", hookDecl("PretoolUse:command:./g.sh"), "事件名拼错了。", ""))
	// 只有正文, 没有 hooks 段。
	assets.Add(buildHookAsset(t, "no-decl", "", "只有说明, 没有 hooks 段。", ""))

	reg, pool, sink := setupHookPool(t, assets)

	for _, name := range []string{"typo-event", "no-decl"} {
		if pool.IsMember(name) {
			t.Errorf("%s 被登记成了池成员 —— 成员表与索引表分叉的起点", name)
		}
		if res := poolCall(t, reg, PoolLoadName, `{"name":"`+name+`"}`); !res.IsError {
			t.Errorf("%s 能加载 (内容: %s) —— 装不出任何 hook 的资产不该进池", name, res.Content)
		}
	}
	// 检索侧: 用**两份资产正文里的词**查, 命中即说明它进了索引。
	// (不能拿资产名当查询词: 未命中回退提示会把查询词原样回显, 于是"名字出现在
	// 返回里"对任何查询都恒真 —— 那是一条没有分辨力的断言。)
	res := poolCall(t, reg, PoolSearchName, `{"query":"事件名拼错了","limit":10}`)
	if strings.Contains(res.Content, "typo-event") {
		t.Errorf("坏声明的 hook 进了检索索引: %s", res.Content)
	}
	if n := len(sink.installedNames()); n != 0 {
		t.Errorf("sink 上被装了 %d 次, 期望 0", n)
	}
}

func TestShadowHookAssetNeverEntersPool(t *testing.T) {
	assets := asset.NewSet()
	assets.Add(buildHookAsset(t, "shadowed", hookDecl("PreToolUse:command:./g.sh"), "影子态。", "shadow"))
	assets.Add(buildHookAsset(t, "active-one", hookDecl("PreToolUse:command:./a.sh"), "生效态。", ""))

	reg, _, _ := setupHookPool(t, assets)

	if res := poolCall(t, reg, PoolLoadName, `{"name":"shadowed"}`); !res.IsError {
		t.Errorf("影子态 hook 能加载: %s", res.Content)
	}
	if res := poolCall(t, reg, PoolLoadName, `{"name":"active-one"}`); res.IsError {
		t.Errorf("active hook 加载失败: %s", res.Content)
	}
}

// TestNilAssetSetIsByteIdenticalToBefore —— assets 为 nil 时池里一条 hook 都没有。
//
// 这条是"与改造前逐字节一致"的落点: 六种资产在绝大多数部署上还是空的, 空目录
// 不该改变任何既有行为 —— 包括**不改变**池的成员表与检索结果。
func TestNilAssetSetIsByteIdenticalToBefore(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&fakeTool{name: "Base1"})
	reg.Register(&fakeTool{name: "evo_inspect"})
	pool := RegisterPoolTools(reg, nil, nil, nil, nil, nil)

	for _, m := range pool.MemberNames() {
		if m != "evo_inspect" {
			t.Errorf("池成员里出现了 %q (assets=nil 时只应有下沉集)", m)
		}
	}
	res := poolCall(t, reg, PoolSearchName, `{"query":"hook","limit":10}`)
	if res.IsError {
		t.Fatalf("pool_search 报错: %s", res.Content)
	}
}

// ---------------------------------------------------------------------------
// 3. 同一个索引: search 命中的名字 load 必须认
// ---------------------------------------------------------------------------

// TestHookIndexSharedBetweenSearchAndLoad —— 三个工具必须看**同一张表**。
//
// ★ 这条断言的是**结构事实** (指针同一), 不是行为:
// 今天索引是一次性建好的, 三个工具各建一份**碰巧**得到同样的内容 —— 所以只测
// "search 命中 → load 认得", 变异 (让 search 自建索引) 测不出来。而分叉是真实的
// 风险: 任何一处未来的增量登记 (常驻面聚合、运行期新增资产) 都会让两份表漂开,
// 表现是"search 命中、load 说不在池内"这种没有任何报错的怪相。
//
// 所以这里直接钉住同一性 (变异成 NewPoolSearchTool 自建 ⇒ 指针不同 ⇒ 变红),
// 后面再补一条 search→load 的端到端读数 —— 两者缺一: 前者防漂移, 后者防"同表但内容错"。
func TestHookIndexSharedBetweenSearchAndLoad(t *testing.T) {
	assets := asset.NewSet()
	assets.Add(buildHookAsset(t, "audit-log", hookDecl("PostTurn:command:./audit.sh"), "记录每轮。", ""))

	reg, _, sink := setupHookPool(t, assets)

	st, ok := mustTool[*PoolSearchTool](t, reg, PoolSearchName)
	if !ok {
		t.Fatalf("%s 未注册成 *PoolSearchTool", PoolSearchName)
	}
	lt, ok := mustTool[*PoolLoadTool](t, reg, PoolLoadName)
	if !ok {
		t.Fatalf("%s 未注册成 *PoolLoadTool", PoolLoadName)
	}
	rt, ok := mustTool[*PoolReleaseTool](t, reg, PoolReleaseName)
	if !ok {
		t.Fatalf("%s 未注册成 *PoolReleaseTool", PoolReleaseName)
	}
	if st.idx == nil || st.idx != lt.idx || st.idx != rt.idx {
		t.Fatalf("三件不共享同一份索引 (search=%p load=%p release=%p) —— hookCfgs 会有多份快照",
			st.idx, lt.idx, rt.idx)
	}
	if _, isHook := lt.idx.hookCfgs["audit-log"]; !isHook {
		t.Fatalf("共享索引里没有 hook 配置 —— 同表但表是空的")
	}

	// 端到端: search 报出的名字 load 必须认, 且真的落到 sink 上。
	res := poolCall(t, reg, PoolSearchName, `{"query":"audit","limit":10}`)
	if !strings.Contains(res.Content, "audit-log") {
		t.Fatalf("search 未命中 audit-log: %s", res.Content)
	}
	lres := poolCall(t, reg, PoolLoadName, `{"name":"audit-log"}`)
	if lres.IsError {
		t.Fatalf("search 报出的名字 load 不认: %s", lres.Content)
	}
	if n := len(sink.installedNames()); n != 1 {
		t.Fatalf("sink 装机次数 = %d, 期望 1", n)
	}
}

// mustTool 取注册表里的工具并按 T 断言类型。
func mustTool[T any](t *testing.T, reg *tool.Registry, name string) (T, bool) {
	t.Helper()
	var zero T
	raw, ok := reg.Get(name)
	if !ok {
		return zero, false
	}
	v, ok := raw.(T)
	return v, ok
}

// TestHookDeclNamesRenderedInLoadResult —— 加载回注里要写出装了什么。
//
// 与 pool_load 的技能分支回注正文同一条理由: 模型要能看见"这次加载得到了什么"。
// hook 没有正文可回注, 声明清单就是它的全部读数。
func TestHookDeclNamesRenderedInLoadResult(t *testing.T) {
	assets := asset.NewSet()
	assets.Add(buildHookAsset(t, "both",
		hookDecl("PreToolUse:command:./a.sh", "PostTurn:http:https://h.internal/x"), "两条。", ""))
	reg, _, _ := setupHookPool(t, assets)

	res := poolCall(t, reg, PoolLoadName, `{"name":"both"}`)
	if res.IsError {
		t.Fatalf("加载失败: %s", res.Content)
	}
	for _, want := range []string{"PreToolUse:command", "PostTurn:http"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("加载回注里没有 %q: %s", want, res.Content)
		}
	}
}

// TestRegisterPoolToolsOnlyRegistersParseableHooks —— 登记与检索条件必须一致。
//
// 变异法: 把 RegisterPoolTools 里那句 `len(cfgs) > 0` 去掉 (改成无条件 AddMember) ⇒
// 破声明的那份会成为池成员, 但 buildPoolIndex 会跳过它 → 成员表与索引表分叉。
// 表现: pool_load 报"不在池内"而池里明明有它。
func TestRegisterPoolToolsOnlyRegistersParseableHooks(t *testing.T) {
	assets := asset.NewSet()
	assets.Add(buildHookAsset(t, "good", hookDecl("PreToolUse:command:./g.sh"), "好的。", ""))
	assets.Add(buildHookAsset(t, "broken", hookDecl("Nope:command:./b.sh"), "坏的。", ""))

	reg := tool.NewRegistry()
	reg.Register(&fakeTool{name: "Base1"})
	pool := RegisterPoolTools(reg, nil, assets, nil, nil, nil)

	if !pool.IsMember("good") {
		t.Error("可解析的 hook 未登记进池 —— 池里列不出它, search 永远命不中")
	}
	if pool.IsMember("broken") {
		t.Error("装不出 hook 的资产被登记进池了 —— 成员表与索引表分叉")
	}
}
