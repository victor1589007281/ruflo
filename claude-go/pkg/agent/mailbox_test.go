package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// 本文件覆盖 design/01 §4.11 通信机制抽象的两半:
// 前半 = Mailbox (三个实现), 后半 = Board 接口 + 新增的 Watch。

// ---------------------------------------------------------------------------
// Mailbox
// ---------------------------------------------------------------------------

// TestMemMailboxTakeCursor 断言 Take 的"只投递一次"语义与广播处理:
// 第一次 Take 拿到定向 + 广播消息, 第二次 Take 为空 (游标已推进), 新消息只拿到新的。
func TestMemMailboxTakeCursor(t *testing.T) {
	mb := NewMemMailbox()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	must(mb.Send(MailMessage{From: "lead", To: "coder", Content: "先写接口"}))
	must(mb.Send(MailMessage{From: "lead", To: "", Content: "全员注意"}))     // 广播
	must(mb.Send(MailMessage{From: "lead", To: "tester", Content: "别管"})) // 给别人的

	got, err := mb.Take("coder")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(got) != 2 || got[0].Content != "先写接口" || got[1].Content != "全员注意" {
		t.Fatalf("coder 应收到定向+广播两条, got %+v", got)
	}
	again, _ := mb.Take("coder")
	if len(again) != 0 {
		t.Fatalf("第二次 Take 应为空 (游标已推进), got %+v", again)
	}
	// 广播对别人也可见, 且各自游标独立
	tester, _ := mb.Take("tester")
	if len(tester) != 2 {
		t.Fatalf("tester 应收到广播+定向两条, got %+v", tester)
	}

	must(mb.Send(MailMessage{From: "lead", To: "coder", Content: "再改一版"}))
	fresh, _ := mb.Take("coder")
	if len(fresh) != 1 || fresh[0].Content != "再改一版" {
		t.Fatalf("只应拿到新消息, got %+v", fresh)
	}

	// Inbox 非破坏性: 不受游标影响
	if n := len(mb.Inbox("coder")); n != 3 {
		t.Fatalf("Inbox 应返回全部 3 条 (定向2+广播1), got %d", n)
	}
	if mb.Len() != 4 {
		t.Fatalf("总消息数应为 4, got %d", mb.Len())
	}
}

// TestMailboxRejectsEmptyContent 断言空内容被拒 (空消息拼进 prompt 是纯噪声)。
func TestMailboxRejectsEmptyContent(t *testing.T) {
	if err := NewMemMailbox().Send(MailMessage{From: "a", To: "b", Content: "   "}); err == nil {
		t.Fatal("空内容应被拒绝")
	}
}

// TestMailboxDefaultsFilled 断言默认字段补齐 (Type=message, Timestamp 非零)。
func TestMailboxDefaultsFilled(t *testing.T) {
	mb := NewMemMailbox()
	if err := mb.Send(MailMessage{From: "a", To: "b", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	got := mb.Inbox("b")
	if len(got) != 1 || got[0].Type != "message" || got[0].Timestamp.IsZero() {
		t.Fatalf("默认字段未补齐: %+v", got)
	}
}

// TestPersistentMailboxSurvivesRestart 断言持久邮箱跨实例可读, 且游标也持久
// (换一个实例继续 Take 不会重复投递同一条)。
func TestPersistentMailboxSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	mb := NewPersistentMailbox(statestore.NewFileStore(dir), "团队/一号") // 故意含 '/' 与中文
	if err := mb.Send(MailMessage{From: "lead", To: "coder", Content: "第一封"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got, err := mb.Take("coder")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应取到 1 封, got %d (bucket 名可能非法导致读写全废)", len(got))
	}

	// 新实例 (模拟重启): 历史消息还在, 游标也还在
	mb2 := NewPersistentMailbox(statestore.NewFileStore(dir), "团队/一号")
	if n := len(mb2.Inbox("coder")); n != 1 {
		t.Fatalf("重启后 Inbox 应仍有 1 封, got %d", n)
	}
	again, err := mb2.Take("coder")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("游标应已持久化, 不该重复投递, got %+v", again)
	}
	if err := mb2.Send(MailMessage{From: "lead", To: "coder", Content: "第二封"}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := mb2.Take("coder")
	if len(fresh) != 1 || fresh[0].Content != "第二封" {
		t.Fatalf("只应取到新消息, got %+v", fresh)
	}
	if mb2.WriteErrors() != 0 {
		t.Fatalf("不应有落盘失败, got %d", mb2.WriteErrors())
	}
}

// TestTeamMailboxWritesTeamAndBoard 断言 ProductionTeam 适配器与 SendMailMessage 行为一致:
// 消息进 team.Mailbox、同时进黑板 (agent 今天是经黑板看到消息的)、并落盘 team.json。
func TestTeamMailboxWritesTeamAndBoard(t *testing.T) {
	tmp := t.TempDir()
	team := &ProductionTeam{
		Name:       "mbteam",
		dataDir:    tmp,
		Blackboard: NewBlackboard("mbteam", tmp),
	}
	mb := TeamMailbox(team)
	if err := mb.Send(MailMessage{From: "lead", To: "coder", Content: "按接口写"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if n := len(team.Mailbox); n != 1 {
		t.Fatalf("消息应进 team.Mailbox, got %d", n)
	}
	found := false
	for _, e := range team.Blackboard.ReadByCategory("context") {
		if e.Value == "From lead: 按接口写" {
			found = true
		}
	}
	if !found {
		t.Fatal("消息应同步写入黑板 (否则 agent 看不到)")
	}
	// 落盘: team.json 里应能读回这条消息
	b, err := os.ReadFile(filepath.Join(tmp, "team.json"))
	if err != nil {
		t.Fatalf("team.json 未落盘: %v", err)
	}
	var persisted struct {
		Mailbox []MailMessage `json:"mailbox"`
	}
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Mailbox) != 1 || persisted.Mailbox[0].Content != "按接口写" {
		t.Fatalf("team.json 中的邮箱不对: %+v", persisted.Mailbox)
	}

	got, err := mb.Take("coder")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Take 应取到 1 条, got %d", len(got))
	}
	if again, _ := mb.Take("coder"); len(again) != 0 {
		t.Fatalf("同一条不该被投递两次, got %+v", again)
	}
}

// TestMailboxInterfaceCompliance 断言三个实现都满足接口 (编译期 + 运行期都过一遍)。
func TestMailboxInterfaceCompliance(t *testing.T) {
	var impls = []MailboxConsumer{
		NewMemMailbox(),
		NewPersistentMailbox(statestore.NewMemStore(), "x"),
		TeamMailbox(&ProductionTeam{Name: "y", Blackboard: NewBlackboard("y", "")}),
	}
	for i, mb := range impls {
		if err := mb.Send(MailMessage{From: "a", To: "b", Content: "ping"}); err != nil {
			t.Fatalf("实现 %d Send: %v", i, err)
		}
		got, err := mb.Take("b")
		if err != nil {
			t.Fatalf("实现 %d Take: %v", i, err)
		}
		if len(got) != 1 {
			t.Fatalf("实现 %d 应取到 1 条, got %d", i, len(got))
		}
		var _ Mailbox = mb
	}
}

// ---------------------------------------------------------------------------
// Board 接口 + Watch
// ---------------------------------------------------------------------------

// TestBoardInterfaceOnBlackboard 断言 *Blackboard 满足 Board 接口, 且 Put/Read/
// SnapshotForRole/Handoff 经接口调用行为正确。
func TestBoardInterfaceOnBlackboard(t *testing.T) {
	var b Board = NewBlackboard("bt", t.TempDir())
	if err := b.Put(BoardEntry{Key: "objective", Value: "做一个 X", Author: "system", Category: "context"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Put(BoardEntry{Key: "draft-result", Value: "第一版产出", Author: "writer", Category: "result"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if v, ok := b.Read("objective"); !ok || v != "做一个 X" {
		t.Fatalf("Read 不对: %q %v", v, ok)
	}
	if err := b.Put(BoardEntry{Key: "  ", Value: "v"}); err == nil {
		t.Fatal("空 key 应被拒绝 (否则覆盖查找会误命中)")
	}
	snap := b.SnapshotForRole("writer", 2000)
	if snap == "" || !contains(snap, "做一个 X") {
		t.Fatalf("SnapshotForRole 应含 context 条目: %q", snap)
	}
	ho := b.Handoff([]string{"draft"}, "reviewer")
	if !contains(ho, "第一版产出") || !contains(ho, "reviewer") {
		t.Fatalf("Handoff 应含已完成阶段产出与下一角色: %q", ho)
	}
}

// TestBoardPutPreservesTimestamp 断言 Put 尊重调用方给的时间戳 (跨黑板同步/回放需要)。
func TestBoardPutPreservesTimestamp(t *testing.T) {
	bb := NewBlackboard("ts", "")
	ts := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	if err := bb.Put(BoardEntry{Key: "k", Value: "v", Category: "context", Timestamp: ts}); err != nil {
		t.Fatal(err)
	}
	got := bb.ReadByCategory("context")
	if len(got) != 1 || !got[0].Timestamp.Equal(ts) {
		t.Fatalf("Put 应保留给定时间戳, got %+v", got)
	}
}

// TestBoardWatchReceivesChanges 断言 Watch 真收到变更: 新增与覆盖都派发,
// 前缀过滤生效, 不匹配前缀的订阅者收不到。
func TestBoardWatchReceivesChanges(t *testing.T) {
	bb := NewBlackboard("wt", t.TempDir())
	stage := bb.Watch("stage-")
	other := bb.Watch("nope-")
	defer bb.StopWatch("stage-", stage)
	defer bb.StopWatch("nope-", other)

	bb.Write("stage-1-result", "产出A", "coder", "result")
	select {
	case e := <-stage:
		if e.Key != "stage-1-result" || e.Value != "产出A" || e.Author != "coder" {
			t.Fatalf("事件内容不对: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("新增条目未派发事件")
	}

	// 覆盖同 key 也应派发 (下游要知道产出被改了)
	bb.Write("stage-1-result", "产出B", "fixer", "result")
	select {
	case e := <-stage:
		if e.Value != "产出B" || e.Author != "fixer" {
			t.Fatalf("覆盖事件不对: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("覆盖写入未派发事件")
	}

	// 经 Put 写入也派发
	if err := bb.Put(BoardEntry{Key: "stage-2-result", Value: "产出C", Category: "result"}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-stage:
		if e.Key != "stage-2-result" {
			t.Fatalf("Put 事件不对: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("Put 未派发事件")
	}

	// 前缀不匹配的订阅者应始终为空
	select {
	case e := <-other:
		t.Fatalf("不匹配前缀的订阅者不该收到事件: %+v", e)
	default:
	}

	// 空前缀 = 订阅全部
	all := bb.Watch("")
	defer bb.StopWatch("", all)
	bb.Write("random-key", "任意", "sys", "context")
	select {
	case e := <-all:
		if e.Key != "random-key" {
			t.Fatalf("空前缀应收到任意 key, got %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("空前缀订阅未收到事件")
	}
}

// TestBoardWatchNeverBlocksWriter 断言无消费者时写入不被阻塞: 订阅通道 (缓冲 64) 灌满后
// 继续写 500 条必须迅速完成, 数据全部写入, 且丢弃事件被计数 (fail-open 且可观测)。
func TestBoardWatchNeverBlocksWriter(t *testing.T) {
	bb := NewBlackboard("nb", t.TempDir())
	ch := bb.Watch("") // 拿到就不读, 模拟卡死的消费者
	defer bb.StopWatch("", ch)

	const n = 500
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			bb.Write("k"+itoa(i), "v", "author", "result")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("无消费者时写入被阻塞了 (必须 fail-open)")
	}

	if got := bb.WatchDrops(); got < int64(n-watchBuffer) {
		t.Fatalf("丢弃事件数应被计数 (>= %d), got %d", n-watchBuffer, got)
	}
	if v, ok := bb.Read("k499"); !ok || v != "v" {
		t.Fatal("写入方数据必须全部落到黑板, 与订阅者是否消费无关")
	}
	if len(ch) != watchBuffer {
		t.Fatalf("通道应正好被灌满 %d 条, got %d", watchBuffer, len(ch))
	}
}

// TestBoardStopWatchClosesChannel 断言 StopWatch 关闭通道且之后的写入不会 panic
// (先摘除再关闭, 派发侧绝不向已关闭通道发送)。
func TestBoardStopWatchClosesChannel(t *testing.T) {
	bb := NewBlackboard("sw", t.TempDir())
	ch := bb.Watch("p-")
	bb.Write("p-1", "v1", "a", "result")
	bb.StopWatch("p-", ch)

	// 通道应已关闭: 先读出缓冲里的事件, 再读到零值 + ok=false
	if e, ok := <-ch; !ok || e.Key != "p-1" {
		t.Fatalf("停止订阅前的事件应还能读到, got %+v ok=%v", e, ok)
	}
	if _, ok := <-ch; ok {
		t.Fatal("StopWatch 后通道应已关闭")
	}
	// 关键: 之后继续写不能 panic (send on closed channel)
	bb.Write("p-2", "v2", "a", "result")
	if v, ok := bb.Read("p-2"); !ok || v != "v2" {
		t.Fatal("StopWatch 之后写入应正常")
	}
}

// TestBoardFuncsAdapter 断言 BoardFuncs 能把"别的黑板实现"适配进 Board
// (当初为 M4 退役 pkg/orchestrator 那份铺路; 那份已删, 现由 design/02 分布式后端承接),
// 且 nil 字段 fail-open 不 panic。
func TestBoardFuncsAdapter(t *testing.T) {
	inner := NewBlackboard("fa", "")
	var b Board = BoardFuncs{
		PutFn:      inner.Put,
		ReadFn:     inner.Read,
		SnapshotFn: inner.SnapshotForRole,
		HandoffFn:  inner.Handoff,
		WatchFn:    inner.Watch,
	}
	if err := b.Put(BoardEntry{Key: "k", Value: "v", Category: "context"}); err != nil {
		t.Fatal(err)
	}
	if v, ok := b.Read("k"); !ok || v != "v" {
		t.Fatalf("适配器读写不通: %q %v", v, ok)
	}

	var empty Board = BoardFuncs{}
	if err := empty.Put(BoardEntry{Key: "k"}); err != nil {
		t.Fatalf("空适配器 Put 应 fail-open, got %v", err)
	}
	if _, ok := empty.Read("k"); ok {
		t.Fatal("空适配器 Read 应返回未命中")
	}
	if empty.SnapshotForRole("r", 10) != "" || empty.Handoff(nil, "r") != "" {
		t.Fatal("空适配器应返回空串")
	}
	if empty.Watch("p") == nil {
		t.Fatal("空适配器 Watch 应返回非 nil 通道 (调用方 range 不该 panic)")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}
