// runner_citation_gate_test.go —— 交付前引用核验闸的验收。
//
// 失败形态是"报告看起来很完整": 引用表里每个 URL 格式都对, 但过半只出现在
// 搜索结果里, 摘录却是凭 snippet 写的。审计日志能事后发现, 但交付物已经出去了。
// 这里要求在**结束点**再给模型一次修订机会, 且只给一次 (避免无限循环)。
package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCitationGateAsksForRevisionOnce(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 0, "草稿: https://a.example/1 https://b.example/2 https://c.example/3")
	var revised atomic.Bool
	u.onRequest = func(turn int, req seenRequest) {
		if turn != 1 {
			return
		}
		if !strings.Contains(req.Flat, "引用核验") {
			t.Errorf("第二次请求里没有引用核验指令: %q", req.Flat)
		}
		if !strings.Contains(req.Flat, "https://a.example/1") {
			t.Errorf("核验指令没有列出未抓取的 URL: %q", req.Flat)
		}
		revised.Store(true)
		u.text = "# 修订后的报告\n\n正文"
	}
	e := newStateEngine(t, u, cfg, t.TempDir())

	got, err := e.RunIsolated(context.Background(), "写一份报告", IsolatedRunOptions{
		MaxTurns:     8,
		CitationGate: true,
		Stage:        id.Stage,
	})
	if err != nil {
		t.Fatalf("核验后应正常收尾: %v", err)
	}
	if !revised.Load() {
		t.Fatal("引用核验闸没有触发, 模型草稿被直接交付")
	}
	if !strings.HasPrefix(got, "# 修订后的报告") {
		t.Fatalf("应返回修订后的文本, got %q", got)
	}
	if n := u.count(); n != 2 {
		t.Fatalf("应发出 2 次请求 (草稿 + 修订), 实际 %d 次", n)
	}
}

// TestCitationGateOnlyOnce 钉住"修订只给一次": 第二稿仍然不达标时必须交付并
// 让审计日志记录, 不能把一次证据不足变成无限重试。
func TestCitationGateOnlyOnce(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 0, "草稿: https://a.example/1 https://b.example/2 https://c.example/3")
	e := newStateEngine(t, u, cfg, t.TempDir())

	got, err := e.RunIsolated(context.Background(), "写一份报告", IsolatedRunOptions{
		MaxTurns:     8,
		CitationGate: true,
		Stage:        id.Stage,
	})
	if err != nil {
		t.Fatalf("第二次不达标也应交付: %v", err)
	}
	if !strings.Contains(got, "https://a.example/1") {
		t.Fatalf("应返回第二稿文本, got %q", got)
	}
	if n := u.count(); n != 2 {
		t.Fatalf("只应重试一次, 实际请求 %d 次", n)
	}
}

// TestCitationGateSkippedWhenTooLate 钉住"余量不足就不开闸"。
//
// 实测 23/28 轮才触发核验时, 模型没有余量补抓, 直接把 6 条引用删掉把比例
// "修"达标 —— 报告从 9 个来源缩到 3 个。修一个指标不能把内容修没。
func TestCitationGateSkippedWhenTooLate(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 0, "草稿: https://a.example/1 https://b.example/2 https://c.example/3")
	e := newStateEngine(t, u, cfg, t.TempDir())

	got, err := e.RunIsolated(context.Background(), "写一份报告", IsolatedRunOptions{
		MaxTurns:     5, // 剩余 5 轮 < 6, 不触发
		CitationGate: true,
		Stage:        id.Stage,
	})
	if err != nil {
		t.Fatalf("应直接交付: %v", err)
	}
	if !strings.Contains(got, "https://a.example/1") {
		t.Fatalf("应返回原稿, got %q", got)
	}
	if n := u.count(); n != 1 {
		t.Fatalf("余量不足时不应修订, 实际请求 %d 次", n)
	}
}

// TestCitationGateIgnoresSmallCitationSet 钉住阈值: 只有 2 条引用时不值得
// 为它多花一整轮 —— 短报告里的个别二手链接应写进「局限」, 由提示词约束。
func TestCitationGateIgnoresSmallCitationSet(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 0, "短报告: https://a.example/1 https://b.example/2")
	e := newStateEngine(t, u, cfg, t.TempDir())

	got, err := e.RunIsolated(context.Background(), "写一份报告", IsolatedRunOptions{
		MaxTurns:     8,
		CitationGate: true,
		Stage:        id.Stage,
	})
	if err != nil {
		t.Fatalf("小引用集不应触发核验: %v", err)
	}
	if !strings.Contains(got, "短报告") {
		t.Fatalf("应直接交付草稿, got %q", got)
	}
	if n := u.count(); n != 1 {
		t.Fatalf("小引用集不应多发请求, 实际 %d 次", n)
	}
}
