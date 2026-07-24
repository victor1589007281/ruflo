package eventbus

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// recv 带超时收一条事件, 超时视为测试失败。
func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("通道已关闭")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("等待事件超时")
		return Event{}
	}
}

func TestBroadcast(t *testing.T) {
	bus := NewChanBus(0)
	ch1, un1, err := bus.Subscribe("run.events.r1", "")
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	defer un1()
	ch2, un2, err := bus.Subscribe("run.events.r1", "")
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	defer un2()

	if err := bus.Publish("run.events.r1", Event{Data: map[string]any{"n": 1}}); err != nil {
		t.Fatalf("Publish 失败: %v", err)
	}
	for _, ch := range []<-chan Event{ch1, ch2} {
		ev := recv(t, ch)
		if ev.Subject != "run.events.r1" {
			t.Fatalf("Subject 应被钉为发布主题, got=%q", ev.Subject)
		}
		if ev.TS == 0 {
			t.Fatalf("TS 应被补齐")
		}
		if ev.Data["n"] != 1 {
			t.Fatalf("Data 不一致: %v", ev.Data)
		}
	}
}

// TestQueueGroupRoundRobin 队列组: 100 条均匀轮询到 2 个成员, 每条只投一次。
func TestQueueGroupRoundRobin(t *testing.T) {
	bus := NewChanBus(256)
	ch1, un1, err := bus.Subscribe("task.dispatch.*", "workers")
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	defer un1()
	ch2, un2, err := bus.Subscribe("task.dispatch.*", "workers")
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	defer un2()

	const total = 100
	for i := 0; i < total; i++ {
		if err := bus.Publish("task.dispatch.h1", Event{Data: map[string]any{"seq": i}}); err != nil {
			t.Fatalf("Publish 失败: %v", err)
		}
	}

	drain := func(ch <-chan Event) map[int]bool {
		seen := map[int]bool{}
		for {
			select {
			case ev := <-ch:
				seen[ev.Data["seq"].(int)] = true
			default:
				return seen
			}
		}
	}
	got1, got2 := drain(ch1), drain(ch2)
	if len(got1) != 50 || len(got2) != 50 {
		t.Fatalf("轮询应各得 50 条, got=%d/%d", len(got1), len(got2))
	}
	// 每条只投一次: 两成员收到的序号不相交且并集为 0..99
	for seq := range got1 {
		if got2[seq] {
			t.Fatalf("序号 %d 被重复投递", seq)
		}
	}
	if len(got1)+len(got2) != total {
		t.Fatalf("总投递数应为 %d", total)
	}
	if d := bus.Drops(); len(d) != 0 {
		t.Fatalf("缓冲充足不应有丢弃: %v", d)
	}
}

func TestWildcardMatch(t *testing.T) {
	bus := NewChanBus(8)
	ch, un, err := bus.Subscribe("run.events.*", "")
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	defer un()

	if err := bus.Publish("run.events.abc", Event{}); err != nil {
		t.Fatalf("Publish 失败: %v", err)
	}
	if err := bus.Publish("run.other", Event{}); err != nil {
		t.Fatalf("Publish 失败: %v", err)
	}
	if err := bus.Publish("run.events", Event{}); err != nil { // 无后续段, 不命中
		t.Fatalf("Publish 失败: %v", err)
	}
	ev := recv(t, ch)
	if ev.Subject != "run.events.abc" {
		t.Fatalf("应只收到 run.events.abc, got=%q", ev.Subject)
	}
	select {
	case ev := <-ch:
		t.Fatalf("不应再收到事件, got=%q", ev.Subject)
	case <-time.After(50 * time.Millisecond):
	}

	// 非法 pattern
	for _, bad := range []string{"", "a.*.b", "a.b*", "a..b"} {
		if _, _, err := bus.Subscribe(bad, ""); err == nil {
			t.Errorf("pattern %q 应报错", bad)
		}
	}
	// 非法 subject
	if err := bus.Publish("", Event{}); err == nil {
		t.Errorf("空 subject 应报错")
	}
	if err := bus.Publish("a.*", Event{}); err == nil {
		t.Errorf("含通配 subject 应报错")
	}
}

// TestFullBufferDrop 满缓冲: 丢弃该订阅者本条并计数, 发布方不阻塞。
func TestFullBufferDrop(t *testing.T) {
	bus := NewChanBus(1)
	ch, un, err := bus.Subscribe("notify.feishu", "")
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	defer un()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			_ = bus.Publish("notify.feishu", Event{Data: map[string]any{"seq": i}})
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Publish 阻塞了发布方")
	}

	// 缓冲 1: 第 0 条被缓冲, 第 1/2 条被丢弃
	ev := recv(t, ch)
	if ev.Data["seq"] != 0 {
		t.Fatalf("应收到第 0 条, got=%v", ev.Data)
	}
	var totalDrops int64
	for _, n := range bus.Drops() {
		totalDrops += n
	}
	if totalDrops != 2 {
		t.Fatalf("应丢弃 2 条, got=%d (%v)", totalDrops, bus.Drops())
	}
}

// TestUnsubscribe 退订后: 通道关闭, 不再投递, 组内成员移除。
func TestUnsubscribe(t *testing.T) {
	bus := NewChanBus(8)
	ch, un, err := bus.Subscribe("cron.fire", "")
	if err != nil {
		t.Fatalf("Subscribe 失败: %v", err)
	}
	un()
	un() // 幂等
	if _, ok := <-ch; ok {
		t.Fatalf("退订后通道应关闭")
	}
	if err := bus.Publish("cron.fire", Event{}); err != nil {
		t.Fatalf("退订后 Publish 不应报错: %v", err)
	}

	// 队列组: 一成员退订后, 全部事件落到剩余成员
	g1, gun1, _ := bus.Subscribe("task.q", "g")
	g2, gun2, _ := bus.Subscribe("task.q", "g")
	gun1()
	defer gun2()
	for i := 0; i < 4; i++ {
		if err := bus.Publish("task.q", Event{Data: map[string]any{"seq": i}}); err != nil {
			t.Fatalf("Publish 失败: %v", err)
		}
	}
	for i := 0; i < 4; i++ {
		recv(t, g2)
	}
	select {
	case _, ok := <-g1:
		if ok {
			t.Fatalf("退订成员不应再收到事件")
		}
	default:
	}
}

// TestConcurrency 并发发布 + 订阅/退订, -race 洁净。
func TestConcurrency(t *testing.T) {
	bus := NewChanBus(64)
	var wg sync.WaitGroup

	// 4 个发布者
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = bus.Publish(fmt.Sprintf("run.events.p%d", p), Event{Data: map[string]any{"i": i}})
			}
		}(p)
	}
	// 4 个订阅者反复订阅/消费/退订
	for s := 0; s < 4; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				ch, un, err := bus.Subscribe("run.events.*", "grp")
				if err != nil {
					t.Errorf("Subscribe 失败: %v", err)
					return
				}
				select {
				case <-ch:
				case <-time.After(time.Millisecond):
				}
				un()
			}
		}()
	}
	wg.Wait()
	bus.Drops() // 快照读也参与竞态检查
}
