// recover_test.go — Q-H11 崩溃恢复扫描的验收。
//
// 覆盖策略（每条测试都钉住一个"改坏了会怎样"的判据，而不是"函数被调用过"）：
//
//	路径对不对    → 记录只走 MarkTurn，禁止 AppendAssistant/EnsureUser；
//	幂等          → 连跑两次，第二次 Interrupted 为空、Skipped 里有它；
//	阈值          → 假时钟推进，活记录不标、死记录才标；
//	失败隔离      → 单条 MarkTurn 失败进 Errors，后续记录照常处理；
//	不静默成功    → nil sink / 未实现 InFlightLister 都必须返回 error。
package harness

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

// recoverFakeSink 是一个"持久化在内存里的"假 sink。
//
// 它同时实现 TurnSink 与 InFlightLister，并且把 MarkTurn 的结果写回自己的
// states —— 这样第二次扫描真的会看到第一次的结果，幂等测试才有意义
// （只记录调用次数的假 sink 无法区分"没标记"和"标记了但没落盘"）。
type recoverFakeSink struct {
	states map[string]TurnState
	// calls 是方法调用序列，用于断言恢复扫描"只写状态、不碰消息"。
	calls []string
	// markErr 按 turnID 注入 MarkTurn 失败。
	markErr map[string]error
	// listerErr 注入 ListInFlight 失败。
	listerErr error
	// listedOwner 记录 lister 收到的 owner 参数（应等于 opts.OwnerID 原文）。
	listedOwner string
}

func newRecoverFakeSink(recs ...TurnState) *recoverFakeSink {
	s := &recoverFakeSink{
		states:  make(map[string]TurnState, len(recs)),
		markErr: make(map[string]error),
	}
	for _, r := range recs {
		s.states[recoverInFlightKey(r)] = r
	}
	return s
}

func (s *recoverFakeSink) EnsureUser(context.Context, UserRecord) (bool, error) {
	s.calls = append(s.calls, "ensure_user")
	return false, nil
}

func (s *recoverFakeSink) AppendAssistant(context.Context, AssistantRecord) error {
	s.calls = append(s.calls, "append_assistant")
	return nil
}

func (s *recoverFakeSink) MarkTurn(_ context.Context, st TurnState) error {
	s.calls = append(s.calls, "mark_turn:"+st.TurnID)
	if err, ok := s.markErr[st.TurnID]; ok {
		return err
	}
	s.states[recoverInFlightKey(st)] = st
	return nil
}

func (s *recoverFakeSink) ListInFlight(_ context.Context, ownerID string) ([]TurnState, error) {
	s.calls = append(s.calls, "list_in_flight")
	s.listedOwner = ownerID
	if s.listerErr != nil {
		return nil, s.listerErr
	}
	out := make([]TurnState, 0, len(s.states))
	for _, r := range s.states {
		out = append(out, r)
	}
	return out, nil
}

// callCount 数某个前缀被调用了几次。
func (s *recoverFakeSink) callCount(prefix string) int {
	n := 0
	for _, c := range s.calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// plainSink 只实现 TurnSink，**不**实现 InFlightLister。
// 用来钉住"恢复没执行不能看起来像成功"。
type plainSink struct{}

func (plainSink) EnsureUser(context.Context, UserRecord) (bool, error)   { return false, nil }
func (plainSink) AppendAssistant(context.Context, AssistantRecord) error { return nil }
func (plainSink) MarkTurn(context.Context, TurnState) error              { return nil }

// ---- 基础路径 ----------------------------------------------------------------

// TestRecoverInFlight_MarksCrashedTurn 是 Q-H11 的主路径：
// 用户在途 turn 重启后被标 interrupted，且**只**写状态、不写助手消息。
func TestRecoverInFlight_MarksCrashedTurn(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	rec := TurnState{
		OwnerID: "alice", SessionID: "s1", TurnID: "t1",
		State: stateRunning, UpdatedAt: at.Add(-time.Minute),
	}
	sink := newRecoverFakeSink(rec)

	got, err := RecoverInFlight(context.Background(), sink, RecoverOptions{
		Now:        func() time.Time { return at },
		StaleAfter: 30 * time.Second,
		OwnerID:    "alice",
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got.Interrupted) != 1 || got.Interrupted[0] != "t1" {
		t.Fatalf("Interrupted=%v，want [t1]", got.Interrupted)
	}
	if len(got.Errors) != 0 {
		t.Fatalf("不应有错误: %v", got.Errors)
	}

	st, ok := sink.states[recoverInFlightKey(rec)]
	if !ok {
		t.Fatal("MarkTurn 没写回假存储")
	}
	if st.State != stateInterrupted {
		t.Errorf("State=%q，want interrupted", st.State)
	}
	if st.Persisted {
		t.Error("Persisted 必须为 false：助手消息确实没有")
	}
	if st.Reason != reasonCrashRecovery {
		t.Errorf("Reason=%q，want %q", st.Reason, reasonCrashRecovery)
	}
	if !st.UpdatedAt.Equal(at) {
		t.Errorf("UpdatedAt=%v，want 扫描时钟 %v", st.UpdatedAt, at)
	}

	// 约束 1/2：绝不自动重放、绝不产生重复助手消息。
	if n := sink.callCount("append_assistant"); n != 0 {
		t.Fatalf("恢复扫描调了 %d 次 AppendAssistant —— 会产生重复助手消息", n)
	}
	if n := sink.callCount("ensure_user"); n != 0 {
		t.Fatalf("恢复扫描调了 %d 次 EnsureUser —— 它不是恢复的一部分", n)
	}
	if n := sink.callCount("mark_turn:"); n != 1 {
		t.Fatalf("MarkTurn 调了 %d 次，want 1", n)
	}
	if sink.listedOwner != "alice" {
		t.Errorf("lister 收到的 owner=%q，want alice", sink.listedOwner)
	}
}

// TestRecoverInFlight_Idempotent 钉住约束 3：连跑两次，第二次不得重复标记。
//
// 判据是"第二次 Interrupted 为空且 Skipped 含 t1"，而不是"MarkTurn 只被调一次"
// —— 后者会让"第二次根本没看到记录"也通过（那是 lister 的 bug，不是幂等）。
func TestRecoverInFlight_Idempotent(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sink := newRecoverFakeSink(TurnState{
		SessionID: "s1", TurnID: "t1",
		State: stateRunning, UpdatedAt: at.Add(-time.Hour),
	})
	opts := RecoverOptions{Now: func() time.Time { return at }}

	first, err := RecoverInFlight(context.Background(), sink, opts)
	if err != nil {
		t.Fatalf("第一次扫描失败: %v", err)
	}
	if len(first.Interrupted) != 1 {
		t.Fatalf("第一次应标记 1 条，got %v", first.Interrupted)
	}

	second, err := RecoverInFlight(context.Background(), sink, opts)
	if err != nil {
		t.Fatalf("第二次扫描失败: %v", err)
	}
	if len(second.Interrupted) != 0 {
		t.Fatalf("第二次不得重复标记，got Interrupted=%v", second.Interrupted)
	}
	if !containsStr(second.Skipped, "t1") {
		t.Fatalf("第二次应把 t1 放进 Skipped，got %v", second.Skipped)
	}
	if n := sink.callCount("mark_turn:"); n != 1 {
		t.Fatalf("两次扫描总共 MarkTurn %d 次，want 1（幂等）", n)
	}
}

// TestRecoverInFlight_StaleThreshold 钉住 StaleAfter 的判据方向。
//
// 两条记录同一 owner/session，只有 UpdatedAt 不同：新鲜的不标、过期的才标。
// 阈值反向（用 < 而不是 >=）时，恰好只有"新鲜"那条会被标 —— 断言会红。
func TestRecoverInFlight_StaleThreshold(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sink := newRecoverFakeSink(
		TurnState{SessionID: "s1", TurnID: "fresh", State: stateRunning, UpdatedAt: at.Add(-time.Minute)},
		TurnState{SessionID: "s1", TurnID: "stale", State: stateRunning, UpdatedAt: at.Add(-10 * time.Minute)},
	)

	got, err := RecoverInFlight(context.Background(), sink, RecoverOptions{
		Now:        func() time.Time { return at },
		StaleAfter: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got.Interrupted) != 1 || got.Interrupted[0] != "stale" {
		t.Fatalf("Interrupted=%v，want [stale]", got.Interrupted)
	}
	if !containsStr(got.Skipped, "fresh") {
		t.Fatalf("新鲜记录应在 Skipped，got %v", got.Skipped)
	}
}

// TestRecoverInFlight_StaleAfterZeroMarksAll 钉住"0 = 不按时间判，一律视为在途"。
// 零阈值被误当成"永不过期"时，这条会红。
func TestRecoverInFlight_StaleAfterZeroMarksAll(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sink := newRecoverFakeSink(TurnState{
		SessionID: "s1", TurnID: "just-now", State: stateRunning,
		// 比"现在"还晚：只有完全不看时间才会被标。
		UpdatedAt: at.Add(time.Hour),
	})
	got, err := RecoverInFlight(context.Background(), sink, RecoverOptions{
		Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got.Interrupted) != 1 || got.Interrupted[0] != "just-now" {
		t.Fatalf("StaleAfter=0 应标记全部在途，got %v", got.Interrupted)
	}
}

// TestRecoverInFlight_ZeroUpdatedAtIsStale 钉住零值时间按死处理。
func TestRecoverInFlight_ZeroUpdatedAtIsStale(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sink := newRecoverFakeSink(TurnState{SessionID: "s1", TurnID: "t0", State: stateRunning})
	got, err := RecoverInFlight(context.Background(), sink, RecoverOptions{
		Now:        func() time.Time { return at },
		StaleAfter: time.Minute,
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if !containsStr(got.Interrupted, "t0") {
		t.Fatalf("零 UpdatedAt 应按死处理，got %v", got.Interrupted)
	}
}

// ---- 分拣 --------------------------------------------------------------------

// TestRecoverInFlight_SkipsTerminalAndAmbiguous 钉住"只处理 §5.1 定义的在途"。
func TestRecoverInFlight_SkipsTerminalAndAmbiguous(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	old := at.Add(-time.Hour)
	sink := newRecoverFakeSink(
		TurnState{SessionID: "s1", TurnID: "done", State: stateCompleted, Persisted: true, UpdatedAt: old},
		TurnState{SessionID: "s1", TurnID: "failed", State: stateFailed, UpdatedAt: old},
		TurnState{SessionID: "s1", TurnID: "already", State: stateInterrupted, UpdatedAt: old},
		// 助手消息已在库里却没有终态：自相矛盾，不能按"在途"标 Persisted=false。
		TurnState{SessionID: "s1", TurnID: "persisted-running", State: stateRunning, Persisted: true, UpdatedAt: old},
		// 未知状态：不能覆盖宿主的自定义状态机。
		TurnState{SessionID: "s1", TurnID: "custom", State: "paused", UpdatedAt: old},
	)

	got, err := RecoverInFlight(context.Background(), sink, RecoverOptions{
		Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got.Interrupted) != 0 {
		t.Fatalf("不应标记任何终态/歧义记录，got %v", got.Interrupted)
	}
	want := []string{"already", "custom", "done", "failed", "persisted-running"}
	if !equalStrs(got.Skipped, want) {
		t.Fatalf("Skipped=%v，want %v", got.Skipped, want)
	}
	if n := sink.callCount("mark_turn:"); n != 0 {
		t.Fatalf("MarkTurn 被调 %d 次，want 0", n)
	}
}

// TestRecoverInFlight_PendingIsInFlight 钉住 pending 也算在途。
// 只认 running 的实现在"写下 pending 之后崩溃"的窗口里会漏判。
func TestRecoverInFlight_PendingIsInFlight(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sink := newRecoverFakeSink(TurnState{
		SessionID: "s1", TurnID: "p1", State: statePending, UpdatedAt: at.Add(-time.Hour),
	})
	got, err := RecoverInFlight(context.Background(), sink, RecoverOptions{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if !containsStr(got.Interrupted, "p1") {
		t.Fatalf("pending 也应恢复，got Interrupted=%v", got.Interrupted)
	}
}

// ---- owner 作用域 ------------------------------------------------------------

// TestRecoverInFlight_OwnerScope 钉住 OwnerID 过滤：
//   - 空 = 全部 owner（旧行为）；
//   - 非空 = 只标该 owner，且 lister 收到原文（空白由扫描归一化兜底）；
//   - 空白 owner 与默认 owner 互认，不能因空格漏标。
func TestRecoverInFlight_OwnerScope(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	seeded := func() *recoverFakeSink {
		return newRecoverFakeSink(
			TurnState{OwnerID: "alice", SessionID: "s1", TurnID: "a1", State: stateRunning, UpdatedAt: at.Add(-time.Hour)},
			TurnState{OwnerID: "bob", SessionID: "s1", TurnID: "b1", State: stateRunning, UpdatedAt: at.Add(-time.Hour)},
		)
	}

	all := seeded()
	got, err := RecoverInFlight(context.Background(), all, RecoverOptions{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if !equalStrs(got.Interrupted, []string{"a1", "b1"}) {
		t.Fatalf("空 OwnerID 应扫全部，got %v", got.Interrupted)
	}

	scoped := seeded()
	got, err = RecoverInFlight(context.Background(), scoped, RecoverOptions{
		Now: func() time.Time { return at }, OwnerID: "alice",
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if !equalStrs(got.Interrupted, []string{"a1"}) {
		t.Fatalf("OwnerID=alice 只应标 a1，got %v", got.Interrupted)
	}
	if !containsStr(got.Skipped, "b1") {
		t.Fatalf("bob 的记录应进 Skipped，got %v", got.Skipped)
	}
	if scoped.listedOwner != "alice" {
		t.Errorf("lister 应收到原文 owner，got %q", scoped.listedOwner)
	}

	// lister 不守约（返回了别人的记录）时，扫描自身的过滤仍要挡住。
	leaky := newRecoverFakeSink(
		TurnState{OwnerID: "bob", SessionID: "s1", TurnID: "leak", State: stateRunning, UpdatedAt: at.Add(-time.Hour)},
	)
	got, err = RecoverInFlight(context.Background(), leaky, RecoverOptions{
		Now: func() time.Time { return at }, OwnerID: "alice",
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got.Interrupted) != 0 {
		t.Fatalf("跨 owner 记录被标了: %v", got.Interrupted)
	}
	if n := leaky.callCount("mark_turn:"); n != 0 {
		t.Fatalf("跨 owner 记录触发了 %d 次 MarkTurn", n)
	}

	// 空白 owner 归一化。
	blank := newRecoverFakeSink(TurnState{
		OwnerID: " local ", SessionID: "s1", TurnID: "l1", State: stateRunning, UpdatedAt: at.Add(-time.Hour),
	})
	got, err = RecoverInFlight(context.Background(), blank, RecoverOptions{
		Now: func() time.Time { return at }, OwnerID: "local",
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if !containsStr(got.Interrupted, "l1") {
		t.Fatalf("空白归一后应匹配，got %v", got.Interrupted)
	}
}

// ---- 失败与不可执行的边界 ----------------------------------------------------

// TestRecoverInFlight_MarkFailureContinues 钉住约束 4：
// 单条 MarkTurn 失败进 Errors，扫描继续，返回的 error 为 nil。
func TestRecoverInFlight_MarkFailureContinues(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sink := newRecoverFakeSink(
		TurnState{SessionID: "s1", TurnID: "bad", State: stateRunning, UpdatedAt: at.Add(-time.Hour)},
		TurnState{SessionID: "s1", TurnID: "good", State: stateRunning, UpdatedAt: at.Add(-time.Hour)},
	)
	boom := errors.New("db down")
	sink.markErr["bad"] = boom

	got, err := RecoverInFlight(context.Background(), sink, RecoverOptions{
		Now: func() time.Time { return at }, StaleAfter: 0,
	})
	if err != nil {
		t.Fatalf("单条失败不应让整体失败: %v", err)
	}
	if len(got.Errors) != 1 {
		t.Fatalf("Errors=%v，want 1 条", got.Errors)
	}
	if !errors.Is(got.Errors[0], boom) {
		t.Fatalf("Errors[0] 未包装原始错误: %v", got.Errors[0])
	}
	if !strings.Contains(got.Errors[0].Error(), "bad") {
		t.Errorf("Errors[0] 没带 turnID，无法定位: %v", got.Errors[0])
	}
	if !equalStrs(got.Interrupted, []string{"good"}) {
		t.Fatalf("后续记录应继续处理，got %v", got.Interrupted)
	}
}

// TestRecoverInFlight_ListerFailureReturnsError 钉住"列不出来 = 整体失败"。
func TestRecoverInFlight_ListerFailureReturnsError(t *testing.T) {
	boom := errors.New("index unavailable")
	sink := newRecoverFakeSink()
	sink.listerErr = boom

	if _, err := RecoverInFlight(context.Background(), sink, RecoverOptions{}); !errors.Is(err, boom) {
		t.Fatalf("lister 失败应返回包装错误，got %v", err)
	}
}

// TestRecoverInFlight_NilSinkIsError 钉住 nil sink 不是"没有在途 turn"。
func TestRecoverInFlight_NilSinkIsError(t *testing.T) {
	if _, err := RecoverInFlight(context.Background(), nil, RecoverOptions{}); !errors.Is(err, ErrRecoverNilSink) {
		t.Fatalf("nil sink 应返回 ErrRecoverNilSink，got %v", err)
	}
}

// TestRecoverInFlight_WithoutListerIsError 是本文件最重要的失败面：
// sink 没实现 InFlightLister 时**不得**返回成功。
//
// 判据同时要求 (a) error 非 nil、(b) 能用 errors.Is 认出哨兵、(c) 结果为空。
// 只断言 (a) 的话，未来有人改成"返回一个带警告的 error 但结果非空"仍会通过，
// 而 (c) 保证调用方不会误把半截结果当成扫描结论。
func TestRecoverInFlight_WithoutListerIsError(t *testing.T) {
	got, err := RecoverInFlight(context.Background(), plainSink{}, RecoverOptions{})
	if err == nil {
		t.Fatal("未实现 InFlightLister 时必须返回 error —— 静默成功会让「恢复没跑」看起来像「没有在途 turn」")
	}
	if !errors.Is(err, ErrInFlightListerUnsupported) {
		t.Fatalf("应返回 ErrInFlightListerUnsupported，got %v", err)
	}
	if len(got.Interrupted) != 0 || len(got.Skipped) != 0 || len(got.Errors) != 0 {
		t.Fatalf("失败时结果必须为空，got %+v", got)
	}
}

// TestRecoverInFlight_CanceledContext 钉住入口取消直接返回、不写任何状态。
func TestRecoverInFlight_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := newRecoverFakeSink()
	if _, err := RecoverInFlight(ctx, sink, RecoverOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应返回 context.Canceled，got %v", err)
	}
	if n := sink.callCount("mark_turn:"); n != 0 {
		t.Fatalf("取消后仍写了 %d 条状态", n)
	}
}

// ---- 确定性 ------------------------------------------------------------------

// TestRecoverInFlight_DeterministicAndDeduped 钉住：
//   - 重复条目只标记一次；
//   - 输出按 TurnID 排序（两次启动日志可 diff）；
//   - 不就地修改 lister 返回的切片。
func TestRecoverInFlight_DeterministicAndDeduped(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	dup := TurnState{SessionID: "s1", TurnID: "b", State: stateRunning, UpdatedAt: at.Add(-time.Hour)}
	records := []TurnState{
		{SessionID: "s1", TurnID: "c", State: stateRunning, UpdatedAt: at.Add(-time.Hour)},
		dup,
		{SessionID: "s1", TurnID: "a", State: stateRunning, UpdatedAt: at.Add(-time.Hour)},
		dup,
	}
	// 用一个固定返回这组的 lister（不受 MarkTurn 回写影响，便于断言重复条目）。
	sink := &fixedListerSink{recs: records, markCalls: map[string]int{}}

	got, err := RecoverInFlight(context.Background(), sink, RecoverOptions{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if !equalStrs(got.Interrupted, []string{"a", "b", "c"}) {
		t.Fatalf("Interrupted=%v，want [a b c]（排序且去重）", got.Interrupted)
	}
	if !sort.StringsAreSorted(got.Skipped) {
		t.Fatalf("Skipped 未排序: %v", got.Skipped)
	}
	// 重复的那条只标记一次：一次真正标记 + 一次 Skipped。
	if sink.markCalls["b"] != 1 {
		t.Fatalf("重复条目被 MarkTurn %d 次，want 1", sink.markCalls["b"])
	}
	if !containsStr(got.Skipped, "b") {
		t.Fatalf("重复条目应进 Skipped，got %v", got.Skipped)
	}
	// 调用方传入的切片顺序不得被就地改写。
	if records[0].TurnID != "c" || records[1].TurnID != "b" {
		t.Fatalf("扫描就地排序了 lister 的切片: %s,%s", records[0].TurnID, records[1].TurnID)
	}
}

// fixedListerSink 返回固定记录集，记录每条被标记的次数。
type fixedListerSink struct {
	recs      []TurnState
	markCalls map[string]int
}

func (s *fixedListerSink) EnsureUser(context.Context, UserRecord) (bool, error) { return false, nil }
func (s *fixedListerSink) AppendAssistant(context.Context, AssistantRecord) error {
	return nil
}
func (s *fixedListerSink) MarkTurn(_ context.Context, st TurnState) error {
	s.markCalls[st.TurnID]++
	return nil
}
func (s *fixedListerSink) ListInFlight(context.Context, string) ([]TurnState, error) {
	return s.recs, nil
}

// ---- 小助手 ----------------------------------------------------------------

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func equalStrs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// 编译期断言：测试替身真的满足它声称的接口。缺一个方法时应在编译期失败，
// 而不是让类型断言在运行期悄悄走进 "unsupported" 分支 —— 那会把一条本该
// 覆盖主路径的测试变成重复覆盖错误路径。
var (
	_ TurnSink       = (*recoverFakeSink)(nil)
	_ InFlightLister = (*recoverFakeSink)(nil)
	_ TurnSink       = (*fixedListerSink)(nil)
	_ InFlightLister = (*fixedListerSink)(nil)
	_ TurnSink       = plainSink{}
)
