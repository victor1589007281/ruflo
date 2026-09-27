package feishu

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// session_trace_test.go — 会话级轨迹的判据。
//
// 这一组测试要回答的是**唯一那个问题**: 会话路径的 span 到底落在哪个桶里。
// 落错桶的后果是"轨迹页一条都看不见", 而"看不见"与"没采集"在读数上完全一样 ——
// 所以不能只看"有没有 Span 写出来", 必须断言**桶文件名**。

// traceBucketFiles 列出 statestore 里 trace-* 桶对应的文件。
//
// 文件命名见 statestore/filestore.go:105 `<root>/log/<bucket>.jsonl`,
// 而 bucket = "trace-" + TraceID (tracestore.bucketName)。
func traceBucketFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "log", "trace-*"))
	if err != nil {
		t.Fatalf("glob trace 桶: %v", err)
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	return names
}

// traceIDOfBucket 从桶文件名反推 TraceID —— 即 ReadRun 的入参。
//
// ★ 这里容易写错并且**错了不报错**: Store.ReadRun(traceID) 自己会再过一遍
// bucketName, 所以必须传裸 TraceID (如 `run-abc-1`), 传桶名 (如 `trace-run-abc-1`)
// 会被拼成 `trace-run-trace-run-abc-1` —— 读回空 slice 而 err 为 nil,
// 看起来就是"这个桶里没数据"。
func traceIDOfBucket(fileName string) string {
	return strings.TrimSuffix(strings.TrimPrefix(fileName, "trace-"), ".jsonl")
}

// readRunAttrs 读出该次运行的 run Span attrs (取第一条)。
func readRunAttrs(t *testing.T, sm *SessionManager, bucketFile string) map[string]any {
	t.Helper()
	spans, err := sm.traceStore.ReadRun(traceIDOfBucket(bucketFile))
	if err != nil {
		t.Fatalf("ReadRun(%s): %v", traceIDOfBucket(bucketFile), err)
	}
	if len(spans) == 0 {
		return nil
	}
	return spans[0].Attrs
}

// TestBeginSessionRunInjectsRunIDIntoCtx 是这一组的**核心判据**, 也是唯一一条
// "拿掉修复就会红"的断言。
//
// # 为什么判据必须落在 ctx 上, 而不是落在"桶文件名对不对"上
//
// 我自己先写错过一版: 断言"出现了 trace-run-<会话>-* 的桶文件" —— 而那条断言
// **拿掉 RunID 注入照样过**。因为收尾函数写 run Span 时用的是它自己那个局部
// runID 变量, 与 ctx 无关 ⇒ 桶名怎么都是对的。那种测试是假绿。
//
// ctx 里的 RunID 真正决定的是**引擎内部那些 Span** (turn / tool_call / llm_call):
// 它们全部写 `TraceID: trace.From(ctx).RunID` (hook_trace.go:91/127、engine/runner.go:81、
// trace_llm.go:99)。ctx 里没有 RunID, 它们就全落 orphan 桶 —— 而这恰恰是原始症状
// (页面里"一次运行只有个孤零零的根节点, 中间过程全看不见")。
//
// 所以这里的判据是 trace.From(ctx) 本身。引擎内 Span 的落桶由
// TestSessionRunLandsInTraceRunBucket 覆盖桶名形状, 二者合起来才是完整证据链。
func TestBeginSessionRunInjectsRunIDIntoCtx(t *testing.T) {
	dir := t.TempDir()
	ss := statestore.NewFileStore(dir)
	sm := newTestSessionManager(t, ss, nil)
	sm.traceStore = tracestore.New(ss)

	const chatID = "sess-ctx-a"
	ctx, endRun := sm.beginSessionRun(context.Background(), &Session{ChatID: chatID})
	defer endRun(runSpanStatusOK, "")

	ids := trace.From(ctx)
	if ids.RunID == "" {
		t.Fatalf("ctx 里没有 RunID —— 引擎内的 turn/tool_call/llm_call Span 会全部落进 " +
			`trace-orphan 桶, 轨迹页只列 trace-run-*, 于是"一次运行只剩一个根节点"`)
	}
	if ids.SessionID != chatID {
		t.Errorf("SessionID 应为 %q, 得到 %q", chatID, ids.SessionID)
	}
	// RunID 的形状就是桶名的形状 (bucketName = "trace-" + TraceID): 必须
	// trace-run-<会话>-<序号> 这样, 页面才认 (traces.ts:401 startsWith('trace-run-'))。
	wantPrefix := "run-" + chatID + "-"
	if !strings.HasPrefix(ids.RunID, wantPrefix) {
		t.Errorf("RunID 应以 %q 开头 (它决定桶名落在 trace-run- 前缀下), 得到 %q", wantPrefix, ids.RunID)
	}

	// 相邻两次运行不得同 RunID —— 见 TestSessionRunSeqIsUnique 的说明。
	ctx2, endRun2 := sm.beginSessionRun(context.Background(), &Session{ChatID: chatID})
	defer endRun2(runSpanStatusOK, "")
	if trace.From(ctx2).RunID == ids.RunID {
		t.Errorf("两次运行的 RunID 相同 (%q) —— 奖励归因会把两次的账记到一起", ids.RunID)
	}
}

// TestSessionRunLandsInTraceRunBucket 断言 run Span 的落桶形状。
//
// 改造前: 会话路径 ctx 里没有 RunID ⇒ span.TraceID 为空 ⇒ bucketName("") 返回
// "trace-orphan" ⇒ 桶文件叫 trace-orphan.jsonl ⇒ webapp 轨迹页的
// `.filter(f => f.startsWith('trace-run-'))` 一条都列不出来。
//
// ★ 这条**不能**单独当作修复的判据: 收尾函数写 run Span 用的是局部 runID,
// 拿掉 ctx 注入它照样绿 (见上一条的说明)。它验的是"桶名形状 + attrs 齐全",
// 与上面那条合起来才是完整证据链。
func TestSessionRunLandsInTraceRunBucket(t *testing.T) {
	dir := t.TempDir()
	ss := statestore.NewFileStore(dir)
	sm := newTestSessionManager(t, ss, nil)

	// TraceStore 是 run Span 的落点; 生产上由 Bot 用同一个 StateStore 建好后注入。
	sm.traceStore = tracestore.New(ss)

	const chatID = "sess-trace-a"
	sess := &Session{ChatID: chatID, DisplayName: "巡检员甲"}

	_, endRun := sm.beginSessionRun(context.Background(), sess)
	endRun(runSpanStatusOK, "")

	files := traceBucketFiles(t, dir)
	if len(files) == 0 {
		t.Fatalf("一个 trace 桶都没有 —— 说明 Span 根本没写出去 (不是桶名问题, 是没采集)")
	}

	var hit string
	for _, f := range files {
		if strings.HasPrefix(f, "trace-run-") && strings.Contains(f, chatID) {
			hit = f
			break
		}
	}
	if hit == "" {
		t.Fatalf("没有 trace-run-*%s* 这样的桶; 实际拿到 %v\n"+
			"★ 这正是改造前的症状: 桶名落到 trace-orphan, 而 webapp 轨迹页只列 trace-run-*", chatID, files)
	}

	// 反向对照: orphan 桶必须**没有**这次运行 —— 否则上面那条会被"两处都写"蒙混过去。
	for _, f := range files {
		if f == "trace-orphan.jsonl" {
			t.Errorf("本次运行也写进了 trace-orphan —— RunID 只应落一处, 两处都写说明注入点重复")
		}
	}

	// 内容判据: run Span 必须带 team / workflow / status 三个键。
	// 页面按前两个分组、用第三个算失败率 (traces.ts:314-329) —— 少一个就会被
	// 混进「(未知团队)」桶里, 那是"看着有数据其实归不了类"。
	// 序号是全局递增的 (beginSessionRun 里 runSeq.Add(1)), 不去猜它是几 —— 直接
	// 用命中的那个桶文件名反推 TraceID。
	attrs := readRunAttrs(t, sm, hit)
	if attrs == nil {
		t.Fatalf("桶文件在 (%s) 但读回 0 条 span", hit)
	}
	for _, key := range []string{"team", "workflow", "status"} {
		if _, ok := attrs[key]; !ok {
			t.Errorf("run Span 缺 %q —— 页面靠它分组/算失败率, 缺了就归不了类。实际 attrs=%v", key, attrs)
		}
	}
	if got := attrs["team"]; got != "巡检员甲" {
		t.Errorf("team 应取 DisplayName, 得到 %v", got)
	}
	// Kind 判据: 页面按 Kind 分派节点类型 (agent / gate / turn…), KindRun 是那次
	// 运行的"根"节点 —— 写错 Kind 会让这次运行在页面上不成为一个可点开的条目。
	if spans, err := sm.traceStore.ReadRun(traceIDOfBucket(hit)); err == nil {
		if len(spans) == 0 || spans[0].Kind != tracestore.KindRun {
			t.Errorf("Span Kind 应为 %q, 得到 %v", tracestore.KindRun, spans)
		}
	}
}

// TestSessionRunSeqIsUnique 断言同名会话的相邻两次运行**不共桶**。
//
// 为什么这条是必须的: 桶名从 RunID 派生, 而 pkg/trace 明确要求 RunID
// "每次运行唯一, 否则奖励归因会把两次运行的账记到一起"。若两次运行落进同一个
// 文件, 页面上它们会被读成**一次**运行 —— 而"少了一次"没有任何提示。
func TestSessionRunSeqIsUnique(t *testing.T) {
	dir := t.TempDir()
	ss := statestore.NewFileStore(dir)
	sm := newTestSessionManager(t, ss, nil)
	sm.traceStore = tracestore.New(ss)

	sess := &Session{ChatID: "sess-trace-b"}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		_, endRun := sm.beginSessionRun(context.Background(), sess)
		endRun(runSpanStatusOK, "")
	}
	for _, f := range traceBucketFiles(t, dir) {
		if !strings.HasPrefix(f, "trace-run-") {
			continue
		}
		if seen[f] {
			t.Fatalf("同一个桶 %s 出现两次", f)
		}
		seen[f] = true
	}
	if len(seen) != 3 {
		t.Fatalf("3 次运行应产出 3 个桶, 得到 %d 个: %v", len(seen), seen)
	}
}

// TestSessionRunFailedStatusIsRecorded 断言失败运行**如实**写 failed。
//
// 反向对照的核心: 若失败也写 completed, 页面上的失败率恒为 0 —— 而 0 与
// "这段时间没跑过任何东西"长得一样, 是同一类静默。
func TestSessionRunFailedStatusIsRecorded(t *testing.T) {
	dir := t.TempDir()
	ss := statestore.NewFileStore(dir)
	sm := newTestSessionManager(t, ss, nil)
	sm.traceStore = tracestore.New(ss)

	_, endRun := sm.beginSessionRun(context.Background(), &Session{ChatID: "sess-trace-c"})
	endRun(runSpanStatusFailed, "模型返回空 final")

	var got map[string]any
	for _, f := range traceBucketFiles(t, dir) {
		if !strings.HasPrefix(f, "trace-run-") {
			continue
		}
		if attrs := readRunAttrs(t, sm, f); attrs != nil {
			got = attrs
		}
	}
	if got == nil {
		t.Fatal("读不到 run Span")
	}
	if got["status"] != runSpanStatusFailed {
		t.Errorf("status 应为 %q, 得到 %v", runSpanStatusFailed, got["status"])
	}
	if got["error"] != "模型返回空 final" {
		t.Errorf("error 文本应被记下, 得到 %v", got["error"])
	}
}

// TestBeginSessionRunNilStoreIsInert 断言没接 TraceStore 时是**恒等**而非崩溃/假写。
//
// 与 hook_trace.go:28 "store 为 nil 时 hook 不注册" 同向: 没接轨迹就不假装接了。
// 这条同时挡住一类常见退化 —— 回调里解引用 nil 指针, 让整个会话在收尾时 panic。
func TestBeginSessionRunNilStoreIsInert(t *testing.T) {
	sm := &SessionManager{} // traceStore 为 nil
	ctx, endRun := sm.beginSessionRun(context.Background(), &Session{ChatID: "x"})
	if ctx == nil {
		t.Fatal("ctx 不应为 nil")
	}
	endRun(runSpanStatusOK, "") // 不得 panic

	// 会话为 nil 时同理 (淘汰竞态下真会出现)。
	sm2 := &SessionManager{}
	if _, e := sm2.beginSessionRun(context.Background(), nil); e == nil {
		t.Fatal("endRun 不应为 nil —— 调用方会无条件下一次 deferred 调用")
	}
}
