package agent

// blackboard_watch_test.go —— 黑板 `Watch` 生产订阅方 (design/01 §六 标 🟡 的
// "建成未通电") 的验收。
//
// ---------------------------------------------------------------------------
// 这组测试在钉什么
// ---------------------------------------------------------------------------
//
//	① **默认关是真的关**: 未设 CLAUDE_GO_BOARD_WATCH 时连 `Watch` 都不调, 于是
//	   `bb.watchers` 恒空 ⇒ `notifyLocked` 第一行就返回 ⇒ 黑板写入路径上一次 select
//	   都不做。判据取 `Watch` 的**副作用**(有没有订阅者) 而不是"订阅方对象是不是 nil":
//	   后者只能证明我没造对象, 证明不了黑板写入路径没变。
//	② 开启后**真的推**: 一条 `<阶段>-status` 写入 ⇒ team.Progress 立刻更新, 不必等
//	   30 秒心跳周期。这是"通电"的直接证据。
//	③ **只认 progress 分类的 `-status` 键**: result 分类 (每个阶段的产出, 体量大且
//	   高频) 与非 `-status` 形态一条都不该进 Progress。
//	④ **fail-closed 的守护不许被改成 fail-open**: 订阅方绝不把 Coordinator 的
//	   `progress.UpdatedAt` 刷新掉 —— 那会让 L2 停滞判据 (ProgressAge) 永远不达标,
//	   即一个在同一阶段空转的团队再也不会被判停滞。这是本项最容易踩的红线。
//	⑤ 生命周期: stop 之后不再消费、且可重复调 (executeWorkflow 的 defer 会走到)。
//
// ---------------------------------------------------------------------------
// 变异反证
// ---------------------------------------------------------------------------
//
//	M10 startTeamBoardWatcher 去掉 boardWatchEnabled() 判断 (即默认开)
//	    → ① 红: "默认关时黑板不该有订阅者, 实际 1 个 —— notifyLocked 的快速返回失效了"
//
//	M11 apply 去掉 `e.Category != boardProgressCategory` 那道闸
//	    → ③ 红: "非 progress/-status 的条目不该改 Progress: Phase got \"伪造的\""
//
//	    这条变异**第一版没抓到**: 当时 ③ 只写了 `article-writing-result`(result 分类)
//	    与 `chapter-3`(progress 分类无后缀), 而这两条各自都被**后缀闸**挡住 ——
//	    摘掉分类闸完全不改变结果。补上"key 以 -status 结尾但分类是 dashboard_write"
//	    这一条 (真实入口: v13 的黑板写接口) 才把两道闸区分开。
//
//	M12 apply 改成先调 `coord.ReportProgress(stage, ...)` 再取快照 (那个"更直觉"的写法)
//	    → ④ 红: "Coordinator 已有 Phase 时订阅方不该改写它, got &{Phase:e ...}"
//	      —— 同时 Coordinator 的进展时钟也被刷了, 即 L2 停滞判据永久失效。

import (
	"testing"
	"time"
)

// bwFixture 造一个带黑板的团队 + Coordinator。
func bwFixture(t *testing.T, name string) (*ProductionTeam, *Coordinator) {
	t.Helper()
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: t.TempDir(),
		Notify:  func(_, _ string) {},
	})
	team, err := ptm.CreateTeam(name, "techblog", tbObjective, "test")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if team.Blackboard == nil {
		t.Fatal("团队应带黑板")
	}
	return team, ptm.newRunCoordinator(team, false)
}

// boardWatcherCount 当前黑板上的订阅通道总数 (跨全部前缀)。
//
// 直接读 bb.watchers 而不是造个 API: 这是**同包**测试, 而要断言的恰恰是"有没有人
// 注册进派发表" —— 那正是 notifyLocked 的唯一判据。绕一层导出方法只会多一处要维护的
// 东西, 且证明力不变。
func boardWatcherCount(bb *Blackboard) int {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	n := 0
	for _, chans := range bb.watchers {
		n += len(chans)
	}
	return n
}

// waitProgress 等 Progress.Phase 变成期望值 (推送是异步的)。
// 轮询而不是 sleep 固定时长: 固定 sleep 要么慢要么在负载高时假红。
func waitProgress(t *testing.T, team *ProductionTeam, want string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		team.mu.Lock()
		p := team.Progress
		team.mu.Unlock()
		if p != nil && p.Phase == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// Test黑板订阅默认关时黑板写入路径不变 —— ① 段。
func Test黑板订阅默认关时黑板写入路径不变(t *testing.T) {
	t.Setenv("CLAUDE_GO_BOARD_WATCH", "") // 显式清掉, 防继承外部环境
	team, coord := bwFixture(t, "bwoff")

	w := startTeamBoardWatcher(team, coord)
	defer w.stop() // nil 安全: 这一行本身也是 ⑤ 的一部分

	if w != nil {
		t.Error("默认关时不该造订阅方")
	}
	if got := boardWatcherCount(team.Blackboard); got != 0 {
		t.Errorf("默认关时黑板不该有订阅者, 实际 %d 个 —— notifyLocked 的快速返回失效了", got)
	}
	// 写一条 progress 条目: 既不该派发, 也不该改 Progress。
	team.Blackboard.Write("article-writing-status", "completed", "system", boardProgressCategory)
	team.mu.Lock()
	p := team.Progress
	team.mu.Unlock()
	if p != nil {
		t.Errorf("默认关时不该有任何进度推送, got %+v", *p)
	}
	if got := team.Blackboard.WatchDrops(); got != 0 {
		t.Errorf("无订阅者时不该有丢弃计数, got %d", got)
	}
}

// Test黑板订阅开启后进度立即推送 —— ② 段 (通电证据)。
func Test黑板订阅开启后进度立即推送(t *testing.T) {
	t.Setenv("CLAUDE_GO_BOARD_WATCH", "1")
	team, coord := bwFixture(t, "bwon")

	w := startTeamBoardWatcher(team, coord)
	if w == nil {
		t.Fatal("开启后应造出订阅方")
	}
	defer w.stop()

	if got := boardWatcherCount(team.Blackboard); got != 1 {
		t.Fatalf("应注册 1 个订阅通道, 实际 %d", got)
	}
	team.Blackboard.Write("article-writing-status", "completed", "system", boardProgressCategory)
	if !waitProgress(t, team, "article-writing") {
		team.mu.Lock()
		p := team.Progress
		team.mu.Unlock()
		t.Fatalf("进度应被推送到 team.Progress, got %+v", p)
	}
}

// Test黑板订阅只认progress分类的status键 —— ③ 段。
func Test黑板订阅只认progress分类的status键(t *testing.T) {
	t.Setenv("CLAUDE_GO_BOARD_WATCH", "1")
	team, coord := bwFixture(t, "bwfilter")
	w := startTeamBoardWatcher(team, coord)
	if w == nil {
		t.Fatal("应造出订阅方")
	}
	defer w.stop()

	// result 分类: 每阶段一条且体量大, 绝不该进 Progress。
	team.Blackboard.Write("article-writing-result", "很长的产出……", "tech-writer", "result")
	// progress 分类但非 -status 形态 (novel-v3 的章节键): 不猜, 忽略。
	team.Blackboard.Write("chapter-3", "done", "novelist", boardProgressCategory)
	// **key 恰好以 -status 结尾但分类不是 progress** —— 这一条才真正区分"分类过滤"
	// 与"后缀过滤"两道闸。上面两条只被后缀闸挡住, 所以单摘分类闸时它们照样绿
	// (第一版变异反证就是这么漏掉 M11 的)。
	// 形态取自真实入口: v13 的 POST /api/teams/:name/blackboard 允许写任意 key,
	// 分类恒为 dashboard_write —— 少了分类闸, 外部一 POST 就能伪造一个阶段名。
	team.Blackboard.Write("伪造的-status", "completed", "dashboard", "dashboard_write")

	// 给订阅 goroutine 充分的机会犯错。
	time.Sleep(80 * time.Millisecond)
	team.mu.Lock()
	p := team.Progress
	team.mu.Unlock()
	if p != nil {
		t.Errorf("非 progress/-status 的条目不该改 Progress: Phase got %q", p.Phase)
	}

	// 反面对照: 正确形态必须生效 —— 否则上面的"没生效"可能只是订阅根本没通。
	team.Blackboard.Write("formatting-status", "completed", "system", boardProgressCategory)
	if !waitProgress(t, team, "formatting") {
		t.Error("正确形态的条目应生效")
	}
}

// Test黑板订阅不刷新Coordinator进展时钟 —— ④ 段 (fail-closed 红线)。
//
// 为什么这是红线: Coordinator 的 L2 停滞判据是 `ProgressAge() > 10min && Phase != ""`。
// 若订阅方去调 `coord.ReportProgress(...)`, 每写一条黑板就把 progress.UpdatedAt
// 刷成"刚才" —— 而黑板在一个卡住的阶段里仍可能被 agent 反复写。结果是一个真卡死的
// 团队**永远不会被判停滞**, 即把一个 fail-closed 的守护改成了 fail-open。
func Test黑板订阅不刷新Coordinator进展时钟(t *testing.T) {
	t.Setenv("CLAUDE_GO_BOARD_WATCH", "1")
	team, coord := bwFixture(t, "bwclock")
	w := startTeamBoardWatcher(team, coord)
	if w == nil {
		t.Fatal("应造出订阅方")
	}
	defer w.stop()

	// Coordinator 报一次进展, 然后记下它的时钟。
	coord.ReportProgress("LLM生成", 1, 0, "")
	before := coord.CurrentProgress().UpdatedAt
	if before.IsZero() {
		t.Fatal("ReportProgress 应设置 UpdatedAt")
	}
	time.Sleep(20 * time.Millisecond)

	// 黑板连写 5 条进度条目 —— 模拟"阶段在写东西但整体没前进"。
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		team.Blackboard.Write(s+"-status", "completed", "system", boardProgressCategory)
	}
	if !waitProgress(t, team, "LLM生成") {
		// Coordinator 已有 Phase, 订阅方不该覆盖它 (它带执行相位, 更权威)
		team.mu.Lock()
		p := team.Progress
		team.mu.Unlock()
		t.Fatalf("Coordinator 已有 Phase 时订阅方不该改写它, got %+v", p)
	}

	if after := coord.CurrentProgress().UpdatedAt; !after.Equal(before) {
		t.Errorf("订阅方绝不能刷新 Coordinator 的进展时钟 (L2 停滞判据会永久失效): before=%s after=%s",
			before, after)
	}
}

// Test黑板订阅停止后不再消费且可重复停止 —— ⑤ 段。
func Test黑板订阅停止后不再消费且可重复停止(t *testing.T) {
	t.Setenv("CLAUDE_GO_BOARD_WATCH", "1")
	team, coord := bwFixture(t, "bwstop")
	w := startTeamBoardWatcher(team, coord)
	if w == nil {
		t.Fatal("应造出订阅方")
	}
	team.Blackboard.Write("s1-status", "completed", "system", boardProgressCategory)
	if !waitProgress(t, team, "s1") {
		t.Fatal("停止前应正常推送")
	}
	w.stop()
	if got := boardWatcherCount(team.Blackboard); got != 0 {
		t.Errorf("stop 后应归还订阅通道, 实际 %d 个", got)
	}
	// 重复 stop 不得 panic (executeWorkflow 的 defer 与显式 stop 可能都走到)。
	w.stop()
	// 停止后的写入不再被消费: Progress 停留在 s1。
	team.Blackboard.Write("s2-status", "completed", "system", boardProgressCategory)
	time.Sleep(50 * time.Millisecond)
	team.mu.Lock()
	p := team.Progress
	team.mu.Unlock()
	if p == nil || p.Phase != "s1" {
		t.Errorf("stop 后不该再消费, Progress 应停在 s1, got %+v", p)
	}
}

// Test黑板写入经ProductionTeamManager可达内存实例 —— v13 那个绕过内存黑板的缺陷的验收。
//
// 缺陷形态: dashboard 只改磁盘 blackboard.json, 而内存黑板下一次 debounce 刷盘会把它
// **整份覆盖掉** (调用方却拿到 200 OK)。修法是让队列动作走到内存实例, 即本方法。
func Test黑板写入经ProductionTeamManager可达内存实例(t *testing.T) {
	t.Setenv("CLAUDE_GO_BOARD_WATCH", "1")
	ptm := NewProductionTeamManager(TeamManagerConfig{BaseDir: t.TempDir(), Notify: func(_, _ string) {}})
	team, err := ptm.CreateTeam("bwapi", "techblog", tbObjective, "test")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	w := startTeamBoardWatcher(team, ptm.newRunCoordinator(team, false))
	if w == nil {
		t.Fatal("应造出订阅方")
	}
	defer w.stop()

	if err := ptm.WriteTeamBlackboard("bwapi", "review-status", "completed", "", boardProgressCategory); err != nil {
		t.Fatalf("WriteTeamBlackboard: %v", err)
	}
	// ① 内存黑板真的看到了 (改造前 dashboard 那条路径做不到这一点)。
	if got, ok := team.Blackboard.Read("review-status"); !ok || got != "completed" {
		t.Errorf("内存黑板应可读到写入值, got %q ok=%v", got, ok)
	}
	// ② 且它同时派发给了订阅方 —— 外部注入的条目对进度可见。
	if !waitProgress(t, team, "review") {
		t.Error("经 API 写入的进度条目也应派发给订阅方")
	}
	// ③ 团队不在本进程内时**报错**而不是退回改磁盘 (退回磁盘正是静默丢数据的形态)。
	if err := ptm.WriteTeamBlackboard("不存在的团队", "k", "v", "", ""); err == nil {
		t.Error("团队不存在时应报错")
	}
	// ④ 空 key 拒 (空 key 在覆盖查找里会匹配到任意未命名条目)。
	if err := ptm.WriteTeamBlackboard("bwapi", "  ", "v", "", ""); err == nil {
		t.Error("空 key 应被拒绝")
	}
}
