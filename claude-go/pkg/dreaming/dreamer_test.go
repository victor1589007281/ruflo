package dreaming

// dreamer_test.go —— 这个包此前**一个测试都没有**, 而 `WaitBackground` 这个"可 join"的
// 承诺刚被 -race 证明是**假的**: ForceDream 起的外层 goroutine 没登记进 bg, 于是
//   ① bg.Wait() 在计数器为 0 时立刻返回（等待承诺形同虚设）;
//   ② 随后 executeDream 内部的 bg.Add(1) 与那个已开始的 Wait 并发 = WaitGroup 误用,
//      race 检测器如实报警。
//
// 两个后果里 ① 更隐蔽: 它让一个"已修复非密闭测试"的修复本身是空的 —— 测试照旧靠运气。
// 所以这里断言的是**契约**而不是"跑得过": WaitBackground 返回后, 整理必须已经发生。

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWaitBackground_返回后整理必已完成 —— 契约断言, 不靠睡眠。
//
// 变异反证: 把 startDream 改回 `go d.executeDream(ctx)`（不登记 bg）后, 本测试
// **确定性变红**（WaitBackground 立刻返回, 回调还没跑）—— 而原来那种 `time.Sleep(200ms)`
// 写法在同样的缺陷下多半是绿的, 这正是它抓不到问题的原因。
func TestWaitBackground_返回后整理必已完成(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultDreamConfig()
	cfg.MemoryDir = filepath.Join(dir, "memory")
	d := NewDreamer(cfg, dir)

	var mu sync.Mutex
	consolidated := false
	d.SetConsolidateFn(func(ctx context.Context, sessions []SessionRecord, memoryDir string) error {
		// ⚠️ 标志位必须在 sleep **之后**置位。
		//
		// 第一版写成"先置位再 sleep", 结果变异反证**没红**(0.003s 就 ok): WaitBackground
		// 早退后主 goroutine 去读标志, 而回调恰好已经把它置成了 true —— 断言退化成
		// "看谁先跑到", 一个**无牙**的测试。放到 sleep 之后, 早退路径必然读到 false,
		// 于是缺陷被确定性地抓住。
		time.Sleep(250 * time.Millisecond)
		mu.Lock()
		consolidated = true
		mu.Unlock()
		return nil
	})

	d.RecordSession(SessionRecord{
		ChatID: "c1", StartTime: time.Now().Add(-time.Hour), EndTime: time.Now(),
		Turns: 3, Summary: "测试会话", Importance: 0.9,
	})

	if err := d.ForceDream(context.Background()); err != nil {
		t.Fatalf("ForceDream: %v", err)
	}
	d.WaitBackground()

	mu.Lock()
	defer mu.Unlock()
	if !consolidated {
		t.Fatal("WaitBackground 返回了但整理回调还没跑完 —— 外层 goroutine 没登记进 bg")
	}
}

// TestWaitBackground_可重复调用且无并发写入者时安全。
//
// 钉住使用契约: 它只在"已无并发 ForceDream"时可用（sync.WaitGroup 规定计数器为 0 时
// 开始的 Add 必须发生在 Wait 之前）。这条测试同时保证第二次调用不会 panic —— 一个
// 只能调一次的 Wait 在 defer 里很容易被调两次。
func TestWaitBackground_可重复调用(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultDreamConfig()
	cfg.MemoryDir = filepath.Join(dir, "memory")
	d := NewDreamer(cfg, dir)
	d.SetConsolidateFn(func(context.Context, []SessionRecord, string) error { return nil })
	d.RecordSession(SessionRecord{ChatID: "c1", EndTime: time.Now(), Turns: 1, Summary: "s", Importance: 0.9})

	_ = d.ForceDream(context.Background())
	d.WaitBackground()
	d.WaitBackground() // 第二次必须无害
}

// TestForceDream_并发只有一个能进 —— dreaming 标志位的 CAS 语义。
//
// 与上面两条一起构成"可 join"的完整前提: 若 ForceDream 允许并发进入, 那么
// WaitBackground 等到的只是其中一次。
func TestForceDream_并发只有一个能进(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultDreamConfig()
	cfg.MemoryDir = filepath.Join(dir, "memory")
	d := NewDreamer(cfg, dir)

	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	d.SetConsolidateFn(func(context.Context, []SessionRecord, string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release // 卡住第一次整理, 让第二次 ForceDream 必然撞上"已在整理中"
		return nil
	})
	d.RecordSession(SessionRecord{ChatID: "c1", EndTime: time.Now(), Turns: 1, Summary: "s", Importance: 0.9})

	if err := d.ForceDream(context.Background()); err != nil {
		t.Fatalf("第一次 ForceDream 应成功: %v", err)
	}
	// 等第一次真的进了回调再发第二次, 否则这条测试会退化成"看运气"。
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("第一次整理 3 秒内没进回调")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := d.ForceDream(context.Background()); err == nil {
		close(release)
		t.Fatal("整理进行中时第二次 ForceDream 应被拒绝")
	}
	close(release)
	d.WaitBackground()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("整理回调应只被调用 1 次, 实际 %d", calls)
	}
}
