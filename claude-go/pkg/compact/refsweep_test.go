// refsweep_test.go —— H2-3 折叠引用目录 TTL/上限清理的单测。
//
// 方案 §21 H2-3 的验收原文是: **「N 次重复 run 后目录 ≤ 上限」**。
//
// 这句话里最关键的一个词是 **run**。run 在 claude-go 里是**一个进程** —— 引用名是
// 内容的纯函数 (sha256), 同一个进程里重复折同一条内容只落一份文件, 所以"重复 run"
// 在单进程里根本造不出增长, 验收会变成一个恒真的空场景。真正的增长来自**跨进程**:
// 每个 run 折的是不同的工具输出, 目录只增不减。
//
// 因此本文件的**主臂**用 N 个独立存储 (各自是一个"进程", issued 从空开始) 造规模,
// 而**辅臂**钉住另一条方向相反的规定: 同一个进程里发出过的引用**一条都不能删**
// (删了就是悬空引用 —— 占位还在上下文里指着它)。两条都要有: 只测主臂的话, 一个
// "删光目录"的实现会全绿, 而它把活引用全打断了。
package compact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// refFiles 数一个目录里的常规文件 (子目录不算 —— 清理也不碰它们)。
func refFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// runOnce 模拟**一个新进程**: 新的存储 (issued 从空开始), 折一条独有的内容。
//
// 用 t.TempDir() 下的固定路径建存储而不用 NewToolResultRefStoreForCwd, 是为了避开
// spillDir 的推导 —— 本文件测的是清理, 自指保护有它自己的测试。
func runOnce(t *testing.T, dir, body string) (*ToolResultRefStore, string) {
	t.Helper()
	s := NewToolResultRefStore(dir)
	name, err := s.Put(body)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return s, name
}

// ============================================================================
// 主臂: N 次重复 run 之后目录不超上限 (方案验收原文)
// ============================================================================

// TestH23_RepeatedRunsStayUnderCap 核心判据: 跑 20 次 run、每次落一条**新**内容,
// 上限设 5 —— 目录必须停在 5 条。
//
// 每条内容都不同 (带序号), 所以内容寻址的去重在这里帮不上忙: 这正是"去重挡不住
// 总量"的那一半, 也是本能力存在的理由。每条内容长度都超过 MicroCompactKeepBytes,
// 保证走的是真实落盘路径。
func TestH23_RepeatedRunsStayUnderCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envRefMaxFiles, "5")
	t.Setenv(envRefMaxAgeDays, "0") // 只留条数规则, 把年龄规则的影响排除掉

	var last *ToolResultRefStore
	for i := 0; i < 20; i++ {
		// 每次都是"新进程": 新的存储实例, issued 为空。
		last, _ = runOnce(t, dir, strings.Repeat("A", MicroCompactKeepBytes+100)+string(rune('a'+i)))
		// 每个进程结束时主动扫一次 —— 生产里对应"下一个 run 起来时把上一个 run
		// 的陈旧引用收掉"。不主动扫的话, 只有"再落盘一次"才会触发清理, 最后一个
		// run 的产出就没机会被收。
		last.SweepNow()
	}

	got := len(refFiles(t, dir))
	if got > 5 {
		t.Fatalf("跑了 20 次 run 后目录里有 %d 条, 上限是 5 —— 清理没有生效", got)
	}
	// 对照臂: 不能把目录删空。"≤ 上限"被一个"每次删光"的实现满足, 而那样模型
	// 永远读不回任何折叠原文 —— 比不做清理更差。
	if got == 0 {
		t.Fatal("目录被清空了 —— 清理把引用全删了, 落盘机制等于没做")
	}
}

// TestH23_SweepRunsThroughPut 清理必须在**正常落盘路径**上被触发, 而不是只有显式
// 调用 SweepNow 才跑。
//
// 少了这条, 上面的主臂可以由一个"清理只在被测代码里手动调用时才发生"的实现满足 ——
// 而生产里没有任何地方会去调它, 目录照样无限涨。
func TestH23_SweepRunsThroughPut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envRefMaxFiles, "3")
	t.Setenv(envRefMaxAgeDays, "0")
	// 节流是进程级的 (lastRefSweep), 测试之间会互相影响 —— 显式重置。
	lastRefSweep.Store(0)
	t.Cleanup(func() { lastRefSweep.Store(0) })

	// 每个"进程"只落一条, 且**只**靠 Put 触发清理。
	for i := 0; i < 10; i++ {
		_, _ = runOnce(t, dir, strings.Repeat("B", MicroCompactKeepBytes+100)+string(rune('a'+i)))
		// 模拟新进程: 清掉节流戳, 让下一个 Put 真的去扫。
		lastRefSweep.Store(0)
	}

	if got := len(refFiles(t, dir)); got > 3 {
		t.Fatalf("只靠 Put 触发, 目录停在 %d 条, 上限是 3 —— Put 路径上没有接清理", got)
	}
}

// TestH23_AgeRuleRemovesStale 年龄规则独立于条数规则生效。
//
// 上限设 0 (= 不限) 把条数规则完全摘掉, 确保删掉的确实是年龄那条。
func TestH23_AgeRuleRemovesStale(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// 三条: 两条 40 天前, 一条新鲜。
	fresh := NewToolResultRefStore(dir)
	freshName, err := fresh.Put("fresh")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"ancient-one", "ancient-two"} {
		name := ToolResultRefName(body)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := now.AddDate(0, 0, -40)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	// 新进程 (issued 为空): 三条都是"上一个 run 留下的", 全部可删。
	s := NewToolResultRefStore(dir)
	st := s.sweep(0, 30, now)

	if st.RemovedAge != 2 {
		t.Fatalf("按年龄删了 %d 条, 期望 2 (两条 40 天前)", st.RemovedAge)
	}
	if st.RemovedCap != 0 {
		t.Fatalf("上限为 0 却按条数删了 %d 条", st.RemovedCap)
	}
	if _, err := os.Stat(filepath.Join(dir, freshName)); err != nil {
		t.Fatalf("新鲜引用被年龄规则删掉了: %v", err)
	}
}

// TestH23_ZeroMeansUnlimited 显式写 0 是"不限", 不是"退回默认值"。
//
// 这是配置语义的分界: 0 与"没配"必须是两个意思, 否则运维没有任何办法关掉清理
// (配 0 会被当成没配、又回到默认的 1000)。
func TestH23_ZeroMeansUnlimited(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < 7; i++ {
		p := filepath.Join(dir, string(rune('a'+i))+".txt")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// 全部做成很旧 —— 年龄规则关掉时它们都该留下。
		ts := now.AddDate(0, 0, -400)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	s := NewToolResultRefStore(dir)
	st := s.sweep(0, 0, now) // 两条规则都 0 = 都不限
	if st.RemovedAge+st.RemovedCap != 0 {
		t.Fatalf("上限/期限都为 0 却删了 %d 条", st.RemovedAge+st.RemovedCap)
	}
	if st.FilesAfter != 7 {
		t.Fatalf("剩余 %d 条, 期望 7", st.FilesAfter)
	}
}

// TestH23_MisconfiguredFallsBackToDefault 配错的值退回默认值, **不是**当成"不限"。
//
// 一个拼错的值 (比如 "1OOO" 里的字母 O) 静默关掉清理, 是最难发现的一种配置错误:
// 后果要几周后才显形, 而那时没人会去怀疑一个变量名。
func TestH23_MisconfiguredFallsBackToDefault(t *testing.T) {
	t.Setenv(envRefMaxFiles, "not-a-number")
	if got := refLimitFromEnv(envRefMaxFiles, defaultRefMaxFiles); got != defaultRefMaxFiles {
		t.Fatalf("配错时退回 %d, 期望默认值 %d", got, defaultRefMaxFiles)
	}
	t.Setenv(envRefMaxFiles, "-3")
	if got := refLimitFromEnv(envRefMaxFiles, defaultRefMaxFiles); got != defaultRefMaxFiles {
		t.Fatalf("负数时退回 %d, 期望默认值 %d", got, defaultRefMaxFiles)
	}
	// 空值 = 没配, 同样走默认。
	t.Setenv(envRefMaxFiles, "")
	if got := refLimitFromEnv(envRefMaxFiles, defaultRefMaxFiles); got != defaultRefMaxFiles {
		t.Fatalf("空值时退回 %d, 期望默认值", got)
	}
	// 对照臂: 合法值必须被原样采纳, 否则"配错退默认"会被一个"永远退默认"的实现满足。
	t.Setenv(envRefMaxFiles, "7")
	if got := refLimitFromEnv(envRefMaxFiles, defaultRefMaxFiles); got != 7 {
		t.Fatalf("合法值 7 被改成了 %d", got)
	}
}

// ============================================================================
// 辅臂: 本进程发出过的引用一条都不能删(方向相反的那条规矩)
// ============================================================================

// TestH23_LiveRefsSurviveAgeRule 即便是"很旧"的活引用也不能按年龄删。
//
// 构造方式是时钟退化/测试摆布下的真实形态: 文件刚写出, 但 mtime 被摆到 40 天前。
// 判据必须是"发没发出过", 不是"看起来旧不旧" —— 按 mtime 判的话, 一次 NTP 跳变就
// 能把当前会话正在用的引用全删掉。
func TestH23_LiveRefsSurviveAgeRule(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	s := NewToolResultRefStore(dir)
	name, err := s.Put("live-content")
	if err != nil {
		t.Fatal(err)
	}
	// 把它做旧 —— 一个只看 mtime 的实现会毫不犹豫地删掉它。
	p := filepath.Join(dir, name)
	ts := now.AddDate(0, 0, -40)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}

	st := s.sweep(0, 30, now)
	if st.RemovedAge != 0 {
		t.Fatalf("删掉了 %d 条活引用 —— 占位还在上下文里指着它", st.RemovedAge)
	}
	if st.Protected != 1 {
		t.Fatalf("Protected = %d, 期望 1", st.Protected)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("活引用被年龄规则删掉了: %v", err)
	}
}

// TestH23_LiveRefsSurviveCap 条数规则也不能删活引用 —— 宁可超上限。
//
// 同时钉住 CapSkipped 这个读数: "为了保活引用而放过"必须被报出来。没有它, 一次
// 没能把目录压到上限的清理在磁盘上与"清理根本没跑"完全同形, 而两者的处置方向相反
// (前者要调大上限或缩短会话, 后者要查清理为什么没生效)。
func TestH23_LiveRefsSurviveCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	s := NewToolResultRefStore(dir)
	var names []string
	for i := 0; i < 5; i++ {
		n, err := s.Put(strings.Repeat("C", MicroCompactKeepBytes+100) + string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	// 同一个存储 = 同一个进程: 5 条全是活引用。
	st := s.sweep(2, 0, now)

	if st.RemovedCap != 0 {
		t.Fatalf("上限 2 时删掉了 %d 条活引用", st.RemovedCap)
	}
	if st.CapSkipped != 3 {
		t.Fatalf("CapSkipped = %d, 期望 3 (5 条活引用 - 上限 2)", st.CapSkipped)
	}
	if st.FilesAfter != 5 {
		t.Fatalf("剩余 %d 条, 期望 5 (全部是活引用)", st.FilesAfter)
	}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("活引用 %s 被条数规则删掉了: %v", n, err)
		}
	}
}

// TestH23_CapKeepsNewestStaleRemoved 上限生效时, 删的必须是**最旧**的那些。
//
// 少了这条, "目录 ≤ 上限"会被一个"随机删够数"的实现满足 —— 而随机删会把最近的
// 上下文 (最可能还在被引用的) 优先删掉, 恰好删反。
func TestH23_CapKeepsNewestStaleRemoved(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// 五条陈旧文件 (上一个 run 留下的), 越靠后越新。
	var stale []string
	for i := 0; i < 5; i++ {
		body := strings.Repeat("D", MicroCompactKeepBytes+100) + string(rune('a'+i))
		name := ToolResultRefName(body)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(-time.Duration(5-i) * time.Hour) // i=0 最旧
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		stale = append(stale, name)
	}

	s := NewToolResultRefStore(dir) // 新进程: issued 为空, 五条都可删
	st := s.sweep(2, 0, now)

	if st.RemovedCap != 3 {
		t.Fatalf("删了 %d 条, 期望 3", st.RemovedCap)
	}
	// 最新的两条 (i=3,4) 必须留下; 最旧的三条必须没了。
	for i, n := range stale {
		_, err := os.Stat(filepath.Join(dir, n))
		switch {
		case i <= 2 && err == nil:
			t.Errorf("第 %d 条 (最旧的之一) 该删却还在", i)
		case i >= 3 && err != nil:
			t.Errorf("第 %d 条 (最新的之一) 被删了: %v", i, err)
		}
	}
}

// TestH23_SweepStatsReportEvenWhenNothingRemoved "扫了但一条没删"必须与"没扫"分得开。
//
// 只报"删了几个"的组件, 成功与没运行逐字节相同 —— 于是"清理到底通电没有"在生产上
// 永远无法回答, 直到磁盘满。
func TestH23_SweepStatsReportEvenWhenNothingRemoved(t *testing.T) {
	// 目录根本不存在 —— 每个新工作区第一次落盘前的形态。取 t.TempDir() 下的**子**
	// 路径而不是它本身: t.TempDir() 会先把目录建出来, 拿它当"不存在"是自欺。
	dir := filepath.Join(t.TempDir(), "never-used")
	s := NewToolResultRefStore(dir)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("前置条件不成立: 目录已存在")
	}
	st := s.sweep(100, 30, time.Now())
	if !st.Ran {
		t.Fatal("扫过目录却报 Ran=false —— 读数分不出'扫了没删'与'没扫'")
	}
	if st.FilesBefore != 0 || st.RemovedAge != 0 || st.RemovedCap != 0 {
		t.Fatalf("空目录上的读数不对: %+v", st)
	}

	// 最新读数必须能被问出来 (日志只在真删了东西时才打)。
	if got := s.LastSweep(); !got.Ran {
		t.Fatal("LastSweep 没留下刚才那次清理的读数")
	}
	// nil 存储上的同名方法必须安全 (fail-open 与既有纪律一致)。
	var nilStore *ToolResultRefStore
	if nilStore.SweepNow().Ran {
		t.Fatal("nil 存储报告扫过目录")
	}
	if nilStore.LastSweep().Ran {
		t.Fatal("nil 存储留下了读数")
	}
}

// TestH23_SweepDoesNotTouchSubdirectories 清理只删本目录下的常规文件。
//
// 子目录不是本机制建的, 语义不明 —— 删它等于替别的组件做决定。
func TestH23_SweepDoesNotTouchSubdirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "somebody-elses-dir")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewToolResultRefStore(dir)
	st := s.sweep(0, 1, time.Now()) // 年龄 1 天 + 不限条数: 最激进的删法

	if st.Scanned != 0 {
		t.Fatalf("Scanned = %d, 期望 0 —— 子目录被当成了常规文件", st.Scanned)
	}
	if _, err := os.Stat(filepath.Join(sub, "keep.txt")); err != nil {
		t.Fatalf("子目录里的文件被删了: %v", err)
	}
}

// TestH23_PlaceholderPathStillReadableAfterSweep 端到端那半条: **占位文本里给出的
// 路径, 清完之后必须还能读出原文**。
//
// 上面几条测的是"文件还在"。这条测的是**模型能不能读回** —— 两者不等价: 路径拼错、
// 清理删的是同名的另一份、或者占位里写的是相对路径而清理按绝对路径比, 都会让"文件
// 还在"为真而回读失败。而回读失败正是清理**唯一**真正的危害: 模型照占位去 Read 拿到
// ENOENT, 白花一轮再重跑一遍工具 —— 落到盘机制本来要消灭的那个循环里。
func TestH23_PlaceholderPathStillReadableAfterSweep(t *testing.T) {
	dir := t.TempDir()
	store := NewToolResultRefStore(dir)

	// 走真实折叠路径生成占位 (与引擎侧 MicroCompactHook 用的是同一个函数)。
	block := bigResult("call-1", "UNIQUE-PAYLOAD-7f3a")
	// 落盘的是**整个** block.Content —— bigResult 会补一段填充凑过阈值, 所以
	// 期望值必须取自 block 本身, 不能另外拼一个"以为一样"的字符串。
	orig := block.Content
	msgs := []types.Message{userMsg(block)}
	out, counts := StubToolResultsCounted(msgs, 0, -1, store)
	if counts.Refs != 1 {
		t.Fatalf("没给出引用 (Refs=%d) —— 本测试没构造出它要测的场景", counts.Refs)
	}
	placeholder := out[0].Content[0].Content

	// 占位里必须真的写着一条 full_artifact 路径, 否则下面断言的"读得回"无从谈起。
	path := ""
	for _, line := range strings.Split(placeholder, "\n") {
		if rest, ok := strings.CutPrefix(line, "full_artifact: "); ok {
			path = strings.TrimSpace(rest)
			break
		}
	}
	if path == "" {
		t.Fatalf("占位里没有 full_artifact 路径行:\n%s", placeholder)
	}

	live := store.Path(ToolResultRefName(orig))
	now := time.Now()

	// 构造**唯一会伤到它的那个形状**: 活引用是全目录**最旧**的一条, 且目录超了上限。
	//
	// 位置与条数都不是细节 —— 这个测试前后两版都是假绿的:
	//   - 两条规则都开: 一条的保护写错了, 另一条还兜着;
	//   - 目录里只有一条文件: `len(live) > maxFiles` 为假, 条数规则压根没跑。
	// 要让它咬住, 判定权必须收进单独一条规则, 且活引用必须正好落在"该删"的那一端。
	old := now.AddDate(0, 0, -40)
	if err := os.Chtimes(live, old, old); err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"stale-newer-1", "stale-newer-2"} {
		name := ToolResultRefName(body)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := now.Add(-time.Duration(i+1) * time.Hour)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	// 只开条数规则 (年龄 0 = 不限): 3 条文件、上限 2 => 本该删掉最旧的那条,
	// 而最旧的恰好就是活引用。
	st := store.sweep(2, 0, now)

	// **先断言读得回**。它是本测试的正题 —— 上面那个 CapSkipped 是实现的读数,
	// 不是场景的性质: 保护写坏了它同样会是 0, 那时报"没构造出场景"会把真正的原因
	// (引用被删) 掩盖成一句构造问题, 指向错误的修改方向。
	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("占位里给的路径清完之后读不回来了 (%s): %v\n占位:\n%s", live, err, placeholder)
	}
	if st.CapSkipped != 1 {
		t.Fatalf("CapSkipped = %d, 期望 1 —— 3 条文件、上限 2 时最旧的那条本该被放过并计数", st.CapSkipped)
	}
	if string(got) != orig {
		t.Fatalf("读回来的不是原文: 长度 %d, 期望 %d", len(got), len(orig))
	}
}

// TestH23_PutMarksIssuedBeforeWrite 记账必须发生在落盘**之前**。
//
// 顺序反过来的话, 写盘成功与记账之间有一个窗口, 并发的清理可能把刚写出去、马上要被
// 占位引用的文件删掉。判据是**写盘失败时名字仍然被记住了** —— 那是"先记账"唯一
// 可观察的副作用 (成功路径上两种顺序的结果完全相同, 测不出差别)。
func TestH23_PutMarksIssuedBeforeWrite(t *testing.T) {
	// 用一个不可能写成功的目录: 父路径是个常规文件。
	f := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewToolResultRefStore(filepath.Join(f, "sub"))

	body := strings.Repeat("E", 100)
	if _, err := s.Put(body); err == nil {
		t.Fatal("前置条件不成立: 写入竟然成功了")
	}
	if !s.issuedNames()[ToolResultRefName(body)] {
		t.Fatal("写盘失败时名字没被记账 —— 记账排在写盘之后, 中间有并发窗口")
	}
}
