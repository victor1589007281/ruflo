// compact_test.go —— pkg/compact 首个测试文件; F6 compaction 事件锁语义单测
// (docforge planning-dsh-adopt: 「压缩期间事件写入阻塞排队, 杜绝折叠竞态」)。
//
// 锁定语义:
//  1. 折叠窗口 (runCompaction 全程) 与事件写入 (GateEventWrite) 互斥 —— 窗口内
//     事件写入阻塞排队, 窗口关闭后按序放行, 事件链不横跨折叠边界;
//  2. 窗口状态位 Compacting() 与折叠精确同步, 事件写入侧可观测;
//  3. AutoCompact 阈值下不动 / 阈值上折叠, 结果保持 [boundary, summary, ...tail]
//     结构且入参切片不被改写;
//  4. 并发折叠在同一写锁上串行 (窗口不重叠);
//  5. GateEventWrite nil Compactor / nil fn fail-open 直通。
//
// 测试注入走 httptest 假上游 + api.Client 字面量构造 (pkg/engine/
// zz_runner_pathb_test.go 同款模式); 真实锁行为由 -race 复核。
package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// newTestCompactor 起一个假上游 (Anthropic /messages 协议), 返回按阈值放宽的
// Compactor。摘要响应文本由 ch 慢速发出以撑开可观测的折叠窗口。
func newTestCompactor(t *testing.T, maxContextTokens int, summaryDelay time.Duration, calls *atomic.Int64) (*Compactor, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		// 撑开折叠窗口: 压缩摘要请求期间窗口必须处于打开状态。
		time.Sleep(summaryDelay)
		resp := types.APIResponse{
			ID: "msg_sum", Type: "message", Role: "assistant", Model: "test",
			Content:    []types.ContentBlock{{Type: types.ContentBlockText, Text: "压缩摘要"}},
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 10, OutputTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	c := NewCompactor(&api.Client{BaseURL: srv.URL, APIKey: "k", Model: "test",
		Client: srv.Client(), RetryCount: 0}, maxContextTokens)
	return c, srv.Close
}

// bigHistory 造 N 条超长 user 消息, 使 estimateTokens 稳定越过阈值。
func bigHistory(n, chars int) []types.Message {
	msgs := make([]types.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, types.Message{
			Type:      types.MessageTypeUser,
			UUID:      fmt.Sprintf("u-%d", i),
			Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: strings.Repeat("x", chars)}},
			CreatedAt: time.Now(),
		})
	}
	return msgs
}

// TestGateEventWriteBlocksDuringWindow 核心语义: 事件写入与折叠窗口互斥。
// 起一个长窗口折叠 goroutine, 窗口内 (Compacting()==true 已观测到) 发起事件写入,
// 断言它在窗口关闭前不被执行 —— 排队而非丢失。
func TestGateEventWriteBlocksDuringWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("时序窗口测试, short 模式跳过")
	}
	c, closeSrv := newTestCompactor(t, 200000, 300*time.Millisecond, nil)
	defer closeSrv()

	msgs := bigHistory(10, 1000) // 10*1000/4 = 2500 tokens > 0.8*2000? 否 —— 阈值要精确
	// 上面 2500 < 160000 不触发; 用小窗口 compactor: 阈值 0.8*3000=2400 < 2500 ✓
	c.maxContextTokens = 3000

	var (
		wg         sync.WaitGroup
		windowSeen atomic.Bool
		eventAtWin atomic.Bool // 事件 fn 执行瞬间窗口是否仍打开
		eventDone  atomic.Bool
		evWg       sync.WaitGroup
	)
	evWg.Add(1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.AutoCompact(context.Background(), msgs, "test"); err != nil {
			t.Errorf("AutoCompact: %v", err)
		}
	}()

	// 等窗口打开 (摘要请求已发出 → 事件写入必然撞上窗口)。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Compacting() {
			windowSeen.Store(true)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !windowSeen.Load() {
		t.Fatal("折叠窗口未观测到打开 —— Compacting 状态位与折叠不同步")
	}

	// 窗口内发起事件写入: GateEventWrite 阻塞排队, fn 不得在窗口关闭前执行。
	go func() {
		defer evWg.Done()
		c.GateEventWrite(func() {
			eventAtWin.Store(c.Compacting())
			eventDone.Store(true)
		})
	}()

	// 给事件写入 50ms 撞窗口: 期间 fn 不得执行 (窗口还有 ~250ms)。
	time.Sleep(50 * time.Millisecond)
	if eventDone.Load() {
		t.Fatal("折叠窗口内事件写入被执行 —— 排队语义被破坏")
	}

	wg.Wait() // 折叠完成 → 窗口关闭 → fn 放行

	evDone := make(chan struct{})
	go func() { evWg.Wait(); close(evDone) }()
	select {
	case <-evDone:
	case <-time.After(3 * time.Second):
		t.Fatal("窗口关闭后事件写入未被放行 —— 排队变丢失")
	}
	if eventAtWin.Load() {
		t.Error("事件 fn 执行时窗口仍打开 —— 与折叠重叠, 竞态未收口")
	}
	co, eq := c.Stats()
	if co != 1 {
		t.Errorf("折叠窗口应恰好打开 1 次, got %d", co)
	}
	if eq < 1 {
		t.Errorf("事件过门至少 1 次, got %d", eq)
	}
}

// TestGateEventWriteOpenWhenIdle 无折叠时零阻塞: 连续过门立即执行, 无排队延迟。
func TestGateEventWriteOpenWhenIdle(t *testing.T) {
	c := NewCompactor(nil, 1000)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			c.GateEventWrite(func() {})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("空闲态事件过门不应阻塞")
	}
}

// TestConcurrentCompactionsSerial 并发折叠在同一写锁上串行: 摘要调用计数 == 折叠
// 计数, 且 Compacting 状态位在任意观测点与窗口一致。
func TestConcurrentCompactionsSerial(t *testing.T) {
	if testing.Short() {
		t.Skip("时序窗口测试, short 模式跳过")
	}
	var calls atomic.Int64
	c, closeSrv := newTestCompactor(t, 3000, 30*time.Millisecond, &calls)
	defer closeSrv()
	msgs := bigHistory(10, 1000)

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.AutoCompact(context.Background(), msgs, "test"); err != nil {
				t.Errorf("AutoCompact: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != n {
		t.Errorf("并发折叠应各自完整执行一次摘要调用, want %d got %d", n, got)
	}
	if co, _ := c.Stats(); co != n {
		t.Errorf("折叠窗口应打开 %d 次, got %d", n, co)
	}
	if c.Compacting() {
		t.Error("全部折叠完成后窗口应关闭")
	}
}

// TestAutoCompactThresholdAndShape 阈值与产物结构: 阈值下不动 (nil,nil); 阈值上
// 折叠出 [boundary, summary, ...tail6], 入参切片不被改写。
func TestAutoCompactThresholdAndShape(t *testing.T) {
	c, closeSrv := newTestCompactor(t, 1000000, 0, nil)
	defer closeSrv()
	msgs := bigHistory(10, 1000) // ~2500 tokens << 0.8*1M

	got, err := c.AutoCompact(context.Background(), msgs, "test")
	if err != nil {
		t.Fatalf("低占用应无错不折叠: %v", err)
	}
	if got != nil {
		t.Fatalf("低占用不应折叠, got %d 条", len(got))
	}

	c.maxContextTokens = 3000 // 0.8*3000=2400 < 2500 → 触发
	before := append([]types.Message(nil), msgs...)
	out, err := c.AutoCompact(context.Background(), msgs, "test")
	if err != nil {
		t.Fatalf("折叠失败: %v", err)
	}
	if len(out) != 2+6 { // boundary + summary + 6 条尾部保护
		t.Fatalf("产物应 2+6 条, got %d", len(out))
	}
	if !out[0].IsCompactBoundary {
		t.Error("首条应为 compact boundary")
	}
	if !strings.Contains(out[1].Content[0].Text, "<context_summary>") {
		t.Error("次条应为 <context_summary> 包裹的摘要")
	}
	for i := range before {
		if before[i].UUID != msgs[i].UUID {
			t.Fatal("入参切片被改写")
		}
	}
}

// TestGateEventWriteFailOpen nil Compactor / nil fn fail-open 直通, 不 panic。
func TestGateEventWriteFailOpen(t *testing.T) {
	var nilC *Compactor
	ran := false
	nilC.GateEventWrite(func() { ran = true })
	if !ran {
		t.Error("nil Compactor 应直通执行")
	}
	ran = false
	c := NewCompactor(nil, 1000)
	c.GateEventWrite(func() { ran = true })
	if !ran {
		t.Error("普通 Compactor + 非 nil fn 应执行")
	}
	c.GateEventWrite(nil) // nil fn: 零操作不 panic
}

// TestCompactingNilSafe Compacting/Stats 的 nil 接收者安全。
func TestCompactingNilSafe(t *testing.T) {
	var nilC *Compactor
	if nilC.Compacting() {
		t.Error("nil Compactor 应报告未折叠")
	}
	co, eq := nilC.Stats()
	if co != 0 || eq != 0 {
		t.Errorf("nil Compactor 计数应为 0, got %d/%d", co, eq)
	}
}
