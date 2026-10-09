package internal_hook

// memory_telemetry_test.go —— 记忆检索遥测。
//
// 这套断言针对的是**账本上的一个空洞**: 在此之前 memory.jsonl 里只有 L2 的库存量
// (fact_*), 而 L1 才是注入的大头 (实测 top-7 合计 ~19K 字符), mem_entry_count 与
// mem_retrieval_count 声明了却全仓零发射方。于是"记忆到底注入了多少、命中几条"
// 这个问题在账本上根本没有落点 —— 只有 llm.jsonl 的 memory_chars 总量,
// 分不出层, 也分不出命中数。
//
// 所以这里断言的不是"记了没有", 而是**几个数各自的含义不能串**: 检索次数是
// "打了几次", 命中条数是"捞回几条", 注入字符数是"吃掉多少预算"。三者混在一起
// 时看起来都对, 只有分开断言才能发现串了。

import (
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/memory"
	"github.com/anthropic/claude-go/pkg/types"
)

// 生产形态的 ctx: 最后一条 user 消息是占位符, 真正的任务在 TaskInstruction 里
// (见 retrievalQuery 的注释)。构造它而不是直接塞一条普通 user 消息, 是为了让测试
// 走的是**线上那条分支** —— 直接给 user 消息会让 retrievalQuery 走另一个早退分支。
func telemetryCtx(task string) *HookContext {
	return &HookContext{
		TaskInstruction: task,
		Messages: []types.Message{{
			Type: types.MessageTypeUser,
			Content: []types.ContentBlock{
				{Type: types.ContentBlockText, Text: taskInstructionPointerText},
			},
		}},
	}
}

// fakeRecorder 收集 (name → 值序列), 不碰 Prometheus 注册表。
type fakeRecorder struct {
	mu   sync.Mutex
	rows []struct {
		name  string
		value float64
	}
}

func (f *fakeRecorder) Record(module, name string, value float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, struct {
		name  string
		value float64
	}{name, value})
}

// last 返回某个指标的最后一次取值; ok=false 表示一次都没记过。
func (f *fakeRecorder) last(name string) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.rows) - 1; i >= 0; i-- {
		if f.rows[i].name == name {
			return f.rows[i].value, true
		}
	}
	return 0, false
}

func (f *fakeRecorder) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r.name)
	}
	return out
}

// 有命中的正常路径: 三个数各自对得上, 且注入字符数与实际生成的块等长。
func Test记忆遥测_命中时三项都对得上(t *testing.T) {
	store := memory.NewTieredStore()
	store.Add(&memory.MemoryEntry{
		Content:    "团队 if-jd-analysis 执行完成, 产出面试备战包",
		Topics:     []string{"if-jd-analysis", "team_result"},
		Source:     "team_result",
		Importance: 0.9,
	})

	rec := &fakeRecorder{}
	h := NewMemoryInjectHook(store, nil)
	h.SetMetrics(rec)
	h.ResetRun("run-1")

	// search + render 是生产里的一对: 检索记"打了几次/捞回几条", 渲染记"吃掉多少
	// 预算"。两个口径各有各的落点, 所以这里也分开调, 而不是合成一个入口。
	h.search(telemetryCtx("分析岗位 JD, 产出面试备战包"))
	got := h.render()
	if got == "" {
		t.Fatal("应检索到刚写入的记忆")
	}

	if v, ok := rec.last("mem_entry_count"); !ok || v != 1 {
		t.Errorf("mem_entry_count = %v (ok=%v), 期望 1", v, ok)
	}
	if v, ok := rec.last("mem_retrieval_count"); !ok || v != 1 {
		t.Errorf("mem_retrieval_count = %v (ok=%v), 期望 1 (打了一次检索)", v, ok)
	}
	l1, ok := rec.last("mem_retrieval_count_l1")
	if !ok || l1 != 1 {
		t.Errorf("mem_retrieval_count_l1 = %v (ok=%v), 期望 1 (命中一条)", l1, ok)
	}
	if v, ok := rec.last("mem_retrieval_count_l2"); !ok || v != 0 {
		// factStore 为 nil: L2 没检索, 命中数为 0。仍然要记 —— 记 0 与"压根没这条
		// 指标"在面板上是两件事。
		t.Errorf("mem_retrieval_count_l2 = %v (ok=%v), 期望 0", v, ok)
	}
	// 注入字符数必须与**真正拼出来的块**逐字节等长 —— 否则这个数就不能用来
	// 回答"记忆吃掉多少预算"。
	if v, ok := rec.last("mem_injection_chars"); !ok || int(v) != len(got) {
		t.Errorf("mem_injection_chars = %v (ok=%v), 期望 %d", v, ok, len(got))
	}
}

// 打了一次检索但一条没捞着: 检索次数 +1, 命中数 0。两个数必须分得开 ——
// 合成一个"命中数"就永远看不出"检索打空了"这件事。
func Test记忆遥测_打空时次数与命中数分得开(t *testing.T) {
	store := memory.NewTieredStore()
	store.Add(&memory.MemoryEntry{
		Content:    "完全无关的一条记忆",
		Source:     "manual",
		Importance: 0.5,
	})

	rec := &fakeRecorder{}
	h := NewMemoryInjectHook(store, nil)
	h.SetMetrics(rec)
	h.ResetRun("run-1")

	// 查询词与记忆零重叠, BM25 不应命中。
	h.search(telemetryCtx("zzzzqqqq xxxxwwww"))
	h.render()

	if v, ok := rec.last("mem_retrieval_count"); !ok || v != 1 {
		t.Errorf("mem_retrieval_count = %v (ok=%v), 期望 1 (检索确实打了一次)", v, ok)
	}
	if v, ok := rec.last("mem_retrieval_count_l1"); !ok || v != 0 {
		t.Errorf("mem_retrieval_count_l1 = %v (ok=%v), 期望 0 (没捞着)", v, ok)
	}
}

// 未装配落点 (探针/单测路径) 时**不能 panic** —— SetMetrics 是可选的。
func Test记忆遥测_没装配落点也不崩(t *testing.T) {
	store := memory.NewTieredStore()
	store.Add(&memory.MemoryEntry{Content: "一条记忆", Importance: 0.9, Source: "manual"})
	h := NewMemoryInjectHook(store, nil)
	h.ResetRun("run-1")
	h.search(telemetryCtx("一条记忆")) // 不应 panic
	h.render()
}

// 遥测是**观测**, 不得改变注入结果: 装了落点与不装, 拿到的块必须逐字节相同。
// 这条是防"为了取数顺手改了点东西"的护栏。
func Test记忆遥测_不改变注入内容(t *testing.T) {
	store := memory.NewTieredStore()
	store.Add(&memory.MemoryEntry{
		Content:    "团队 if-jd-analysis 执行完成, 产出面试备战包",
		Topics:     []string{"if-jd-analysis"},
		Source:     "team_result",
		Importance: 0.9,
	})
	ctx := telemetryCtx("分析岗位 JD")

	plain := NewMemoryInjectHook(store, nil)
	plain.ResetRun("run-1")
	plain.search(ctx)
	want := plain.render()

	withMetrics := NewMemoryInjectHook(store, nil)
	withMetrics.SetMetrics(&fakeRecorder{})
	withMetrics.ResetRun("run-1")
	withMetrics.search(ctx)
	got := withMetrics.render()

	if got != want {
		t.Fatalf("装遥测后注入内容变了:\n有: %q\n无: %q", got, want)
	}
}

// ============================================================================
// 形态热改 (配置项 engine.memoryIndexMode → SetMemoryIndexMode)
// ============================================================================

// previewForTest 截断块文本用于报错信息 (块可能有上千字符, 全打出来没法看)。
func previewForTest(s string) string {
	r := []rune(s)
	if len(r) <= 200 {
		return s
	}
	return string(r[:200]) + "…"
}

// seedProductionShaped 往库里塞一批**生产形状**的记忆: top-7, 每条几百字符。
//
// 为什么不能拿一条 30 字的记忆做形态对比: 索引块带一段 ~200 字符的**固定开销**
// (那段 "call memory_read with an id…" 的说明), 条目少且短时索引反而比正文长 ——
// 第一版断言就是这么红的。这个开关的全部理由来自生产形状: 实测 top-7 正文合计
// ~19K 字符、吃掉系统提示的 2/3, 而索引只留头信息和 120 字预览。拿一条短记忆去量,
// 量到的是固定开销, 不是这个开关的收益。
//
// 内容长度取 ~200 字符: 团队每个阶段的产出在写入前就被 truncateResult 砍到几百
// 字符 (见 MemoryEntry.Ref 的注释), 这就是库里的真实粒度。
func seedProductionShaped(n int) *memory.TieredStore {
	store := memory.NewTieredStore()
	filler := strings.Repeat("团队 if-jd-analysis 阶段产出: 面试备战包要点与岗位匹配分析。", 6)
	for i := 0; i < n; i++ {
		store.Add(&memory.MemoryEntry{
			Content:    filler,
			Topics:     []string{"if-jd-analysis", "team_result"},
			Source:     "team_result",
			Importance: 0.9,
		})
	}
	return store
}

// 关→开: 同一个 run 里, 下一个 turn 的块必须换成**索引**形态。
//
// 这条锁的是"活跃会话立即生效"这句承诺。此前不能兑现的原因是结构性的: cached
// 一旦置位就永不复位 (ResetRun 在**生产路径上零调用方**, 引擎是 per-session 建的),
// 于是注入文本在会话的第一个 turn 就被冻住 —— 原子开关翻得再勤, 也没人再读它。
func Test记忆形态_热切换到索引后下一轮就换形态(t *testing.T) {
	store := seedProductionShaped(7) // top-7 = Retrieve 的条数上限

	h := NewMemoryInjectHook(store, nil)
	h.ResetRun("run-1")
	ctx := telemetryCtx("分析岗位 JD, 产出面试备战包")

	// 第 1 轮: 默认正文形态。这是 run 的 t0, 检索在这里发生。
	before, err := h.Execute(ctx)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if before == nil {
		t.Fatal("应注入记忆块")
	}
	body := before.SystemPrompt[len(before.SystemPrompt)-1]
	if !strings.Contains(body, "<relevant_memories>") {
		t.Fatalf("切换前应是正文形态, 实际块 = %q", previewForTest(body))
	}

	// 热加载路径在另一个 goroutine 上翻开关。
	h.SetIndexMode(true)

	// 第 2 轮: 同一 run, 下一个 turn —— 必须已经是索引形态。
	after, err := h.Execute(ctx)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if after == nil {
		t.Fatal("切换形态后不应丢掉整个块")
	}
	idx := after.SystemPrompt[len(after.SystemPrompt)-1]
	if !strings.Contains(idx, "<memory_index>") {
		t.Fatalf("热切换后仍是旧形态, 实际块 = %q", previewForTest(idx))
	}
	if strings.Contains(idx, "<relevant_memories>") {
		t.Error("同一次注入里两种形态同时出现")
	}
	// 索引形态必须明显更短 —— 这是开这个开关的**全部理由**, 不短就等于没生效。
	if len(idx) >= len(body) {
		t.Errorf("索引形态没变短: 索引 %d 字符 vs 正文 %d 字符", len(idx), len(body))
	}
}

// 重渲染不得重检索: 记忆库在每次 run 结尾被 memory 拦截器写一次, 重检索捞回的
// 是**另一批**记忆 —— 那等于把"换个呈现"变成"换批内容", 两个变量就分不开了。
func Test记忆形态_热切换不重跑检索(t *testing.T) {
	store := seedProductionShaped(7)

	rec := &fakeRecorder{}
	h := NewMemoryInjectHook(store, nil)
	h.SetMetrics(rec)
	h.ResetRun("run-1")
	ctx := telemetryCtx("分析岗位 JD, 产出面试备战包")

	if _, err := h.Execute(ctx); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if _, err := h.Execute(ctx); err != nil { // 切形态前先跑一轮, 确认正常复用不计
		t.Fatalf("Execute 失败: %v", err)
	}
	if v, _ := rec.last("mem_retrieval_count"); v != 1 {
		t.Fatalf("前提不成立: 复用的那一轮把检索计成了 %v 次, 期望 1", v)
	}

	h.SetIndexMode(true)
	if _, err := h.Execute(ctx); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	if v, _ := rec.last("mem_retrieval_count"); v != 1 {
		t.Errorf("形态热改重跑了一次检索: mem_retrieval_count = %v, 期望仍是 1", v)
	}

	// 注入字符数则**必须**多记一笔 —— 它按"每次实际发出的形态"记账, 两笔的差值
	// 就是这次切换省下的预算。
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var injections []float64
	for _, r := range rec.rows {
		if r.name == "mem_injection_chars" {
			injections = append(injections, r.value)
		}
	}
	if len(injections) != 2 {
		t.Fatalf("mem_injection_chars 记了 %d 笔, 期望 2 (正文一笔 + 索引一笔)", len(injections))
	}
	if injections[1] >= injections[0] {
		t.Errorf("索引形态没省下字符: 正文 %.0f → 索引 %.0f", injections[0], injections[1])
	}
}

// SetIndexMode 的值没变时不得推进代数 —— 热加载每 5 秒比对一次, 绝大多数时候
// 传进来的值与当前相同, 无条件推进会让下一个 turn 白白重渲染一次 (缓存白失效)。
func Test记忆形态_值没变不推进代数(t *testing.T) {
	h := NewMemoryInjectHook(memory.NewTieredStore(), nil)
	h.SetIndexMode(false) // 默认就是 false
	if gen := h.indexGen.Load(); gen != 0 {
		t.Errorf("设成与当前相同的值却推进了代数: %d", gen)
	}
	h.SetIndexMode(true)
	if gen := h.indexGen.Load(); gen != 1 {
		t.Errorf("真正改变取值时应当推进代数: %d", gen)
	}
	h.SetIndexMode(true)
	if gen := h.indexGen.Load(); gen != 1 {
		t.Errorf("重复设同一个 true 又推进了: %d", gen)
	}
	if !h.IndexMode() {
		t.Error("IndexMode() 与设置不一致")
	}
}

// 形态热改与 run 路径同时跑: 这条是 -race 的靶子。
//
// 刻意只有**一个** Execute 循环 —— 那是线上真实的并发形状: 一个 run goroutine
// (queryLoop 的 PhasePreRequest) 对上一个配置热加载 goroutine。开四个 Execute
// 去压不是更严格, 而是压一个本就不存在的场景 (cachedIn 是 run 内缓存, 从不跨 run
// 共享), 红了也只能说明这个 hook 不打算支持并发 run。
//
// 写 cachedIn / entries / seenGen 的永远只有 run 那一侧 (SetIndexMode 只原子地
// 推进代数, 不碰这些字段), 所以这里应当干净。若哪天有人图省事让 SetIndexMode
// 直接清缓存, 这个用例会红 —— 那正是它要拦的那一类"顺手简化"。
func Test记忆形态_与运行路径无竞争(t *testing.T) {
	store := seedProductionShaped(7)

	h := NewMemoryInjectHook(store, nil)
	h.ResetRun("run-1")
	ctx := telemetryCtx("分析岗位 JD, 产出面试备战包")

	// setter 侧: 模拟热加载反复应用同一份配置 (值来回翻)。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			h.SetIndexMode(i%2 == 0)
		}
	}()

	// run 侧: 同一个 ctx 反复走 Execute。
	for i := 0; i < 500; i++ {
		if _, err := h.Execute(ctx); err != nil {
			t.Fatalf("Execute 失败: %v", err)
		}
	}
	<-done
}
