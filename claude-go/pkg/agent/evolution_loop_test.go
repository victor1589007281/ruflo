package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// countingSink 记录 Record 调用, 供断言指标真被上报。
type countingSink struct {
	mu sync.Mutex
	n  map[string]int
}

func newCountingSink() *countingSink { return &countingSink{n: map[string]int{}} }
func (s *countingSink) Record(module, name string, _ float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n[module+"/"+name]++
}
func (s *countingSink) get(k string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n[k]
}

func newLoopForTest(t *testing.T, cfg EvolutionLoopConfig) (*EvolutionLoop, *countingSink) {
	t.Helper()
	// 真 EvolutionEngine 但 nil LLM: LearnFromTeam/Consolidate 在无轨迹无 LLM 时
	// 是无害空转, 正好用来测调度逻辑而不牵扯 LLM。
	ee := NewEvolutionEngine(t.TempDir(), nil)
	sink := newCountingSink()
	l := NewEvolutionLoop(ee, sink, cfg)
	if l == nil {
		t.Fatal("NewEvolutionLoop 返回 nil")
	}
	return l, sink
}

// engine 为 nil 时必须返回 nil —— 调用方据此回落到直调路径（向后兼容的支点）。
func TestNewEvolutionLoop_nil引擎返回nil(t *testing.T) {
	if l := NewEvolutionLoop(nil, nil, EvolutionLoopConfig{}); l != nil {
		t.Error("engine 为 nil 时应返回 nil, 以便调用方回落直调")
	}
}

// nil 循环上的所有方法必须可空安全（submitLearn 会在未装配时拿到 nil）。
func TestEvolutionLoop_nil可空安全(t *testing.T) {
	var l *EvolutionLoop
	l.Start(context.Background())
	l.Stop()
	if l.Submit(LearnRequest{Kind: LearnTeamDone, Team: "x"}) {
		t.Error("nil 循环的 Submit 应返回 false")
	}
	if snap := l.Snapshot(); snap.Submitted != 0 {
		t.Error("nil 循环的 Snapshot 应为零值")
	}
}

func TestEvolutionLoop_提交后真执行(t *testing.T) {
	l, sink := newLoopForTest(t, EvolutionLoopConfig{IdleAfter: -1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Start(ctx)

	if !l.Submit(LearnRequest{Kind: LearnTeamDone, Team: "t1"}) {
		t.Fatal("首次提交应入队")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && l.Snapshot().Executed == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := l.Snapshot().Executed; got != 1 {
		t.Fatalf("Executed = %d, 期望 1", got)
	}
	// 指标必须真被上报（学习成本占比的前置数据）
	if sink.get("evolution/learn_round_count") == 0 {
		t.Error("learn_round_count 未上报")
	}
	if sink.get("evolution/learn_round_duration_sec") == 0 {
		t.Error("learn_round_duration_sec 未上报")
	}
}

// 去重：团队 refine/重跑会多次走到完成路径，重复蒸馏同一批轨迹会污染经验库
// （同样的经验被记多次、UCB 计数虚高），所以窗口内必须合并。
func TestEvolutionLoop_同team窗口内去重(t *testing.T) {
	l, _ := newLoopForTest(t, EvolutionLoopConfig{DedupWindow: time.Hour, IdleAfter: -1})
	// 不 Start：只测 Submit 的去重判定，避免消费协程把队列抽空影响计数
	if !l.Submit(LearnRequest{Kind: LearnTeamDone, Team: "same"}) {
		t.Fatal("首次应入队")
	}
	for i := 0; i < 5; i++ {
		if l.Submit(LearnRequest{Kind: LearnTeamDone, Team: "same"}) {
			t.Fatal("窗口内重复提交应被去重")
		}
	}
	snap := l.Snapshot()
	if snap.Deduped != 5 {
		t.Errorf("Deduped = %d, 期望 5", snap.Deduped)
	}
	if snap.QueueLen != 1 {
		t.Errorf("队列长度 = %d, 期望 1 (只入队一次)", snap.QueueLen)
	}
	// 不同 team 不受影响
	if !l.Submit(LearnRequest{Kind: LearnTeamDone, Team: "other"}) {
		t.Error("不同 team 不应被去重")
	}
}

// 队列满必须丢弃而不是阻塞：学习的背压绝不能传导回交付路径。
func TestEvolutionLoop_队列满丢弃不阻塞(t *testing.T) {
	l, _ := newLoopForTest(t, EvolutionLoopConfig{QueueSize: 2, DedupWindow: time.Nanosecond, IdleAfter: -1})
	// 不 Start，让队列填满
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			l.Submit(LearnRequest{Kind: LearnIdle}) // Idle 不走去重分支
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit 阻塞了 —— 队列满时必须丢弃")
	}
	snap := l.Snapshot()
	if snap.Dropped == 0 {
		t.Error("队列满应有丢弃计数")
	}
	if snap.QueueLen > 2 {
		t.Errorf("队列长度 %d 超过容量 2", snap.QueueLen)
	}
}

// 预算闸：每小时轮次上限，防止学习把 LLM 预算吃光。
func TestEvolutionLoop_预算闸限制轮次(t *testing.T) {
	l, _ := newLoopForTest(t, EvolutionLoopConfig{MaxRoundsPerHour: 3, IdleAfter: -1})
	for i := 0; i < 3; i++ {
		if !l.allowRound() {
			t.Fatalf("第 %d 轮应被放行", i+1)
		}
	}
	if l.allowRound() {
		t.Error("第 4 轮应被预算闸拦住")
	}
	// MaxRoundsPerHour<=0 表示不限
	l2, _ := newLoopForTest(t, EvolutionLoopConfig{MaxRoundsPerHour: -1, IdleAfter: -1})
	for i := 0; i < 100; i++ {
		if !l2.allowRound() {
			t.Fatal("MaxRoundsPerHour<=0 应不限轮次")
		}
	}
}

// 空闲期深度整理：队列静默后由循环自己发起，散点触发做不到这件事。
func TestEvolutionLoop_空闲期自发整理(t *testing.T) {
	l, _ := newLoopForTest(t, EvolutionLoopConfig{IdleAfter: 40 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && l.Snapshot().IdleRuns == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if l.Snapshot().IdleRuns == 0 {
		t.Error("静默后应触发空闲期整理")
	}
}

// IdleAfter<=0 关闭空闲整理。
func TestEvolutionLoop_可关闭空闲整理(t *testing.T) {
	l, _ := newLoopForTest(t, EvolutionLoopConfig{IdleAfter: -1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	if got := l.Snapshot().IdleRuns; got != 0 {
		t.Errorf("IdleAfter<=0 时不应有空闲整理, 实得 %d", got)
	}
}

// Start/Stop 幂等；ctx 取消能收掉协程。
func TestEvolutionLoop_StartStop幂等(t *testing.T) {
	l, _ := newLoopForTest(t, EvolutionLoopConfig{IdleAfter: -1})
	ctx, cancel := context.WithCancel(context.Background())
	l.Start(ctx)
	l.Start(ctx) // 第二次应无效果
	l.Stop()
	l.Stop() // 重复 Stop 不应 panic（close 已关的 channel 会 panic）
	cancel()
}

// 默认值必须齐全，零值配置可用。
func TestEvolutionLoopConfig_默认值(t *testing.T) {
	c := EvolutionLoopConfig{}.withDefaults()
	if c.QueueSize <= 0 || c.DedupWindow <= 0 || c.IdleAfter <= 0 || c.MaxRoundsPerHour <= 0 {
		t.Errorf("默认值不完整: %+v", c)
	}
	// 显式负值表示关闭，不应被默认值覆盖
	c2 := EvolutionLoopConfig{IdleAfter: -1, MaxRoundsPerHour: -1}.withDefaults()
	if c2.IdleAfter != -1 || c2.MaxRoundsPerHour != -1 {
		t.Errorf("显式负值被默认值覆盖了: %+v", c2)
	}
}

// submitLearn 的回退路径：未装配循环时不得静默丢失学习。
// 这是 design/03 §1.2 开环 1 的原始形态，必须有测试守住。
func TestSubmitLearn_未装配循环时回落直调(t *testing.T) {
	ptm := &ProductionTeamManager{}
	ptm.submitLearn("no-engine") // evolution 为 nil：直接返回，不 panic

	ee := NewEvolutionEngine(t.TempDir(), nil)
	ptm2 := &ProductionTeamManager{evolution: ee} // evoLoop 为 nil
	ptm2.submitLearn("fallback")                  // 应走 go func 直调，不 panic
	time.Sleep(80 * time.Millisecond)

	// 装配循环后应走循环
	l := NewEvolutionLoop(ee, nil, EvolutionLoopConfig{IdleAfter: -1})
	ptm3 := &ProductionTeamManager{evolution: ee, evoLoop: l}
	ptm3.submitLearn("via-loop")
	if got := l.Snapshot().Submitted; got != 1 {
		t.Errorf("装配循环后 Submitted = %d, 期望 1", got)
	}
}
