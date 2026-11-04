// period_budget_test.go — R-H17 时段/日预算在 RunTurn 里的**强制接线**验收。
//
// cost_period_test.go 验的是账本自身的窗口算术；这里验的是"闸门真的接在模型调用
// 之前"以及"记账真的只记一次"。两件事都必须在**上游请求数**上留下证据：
//   - 超额时必须 0 次上游请求（等到请求发出后再查就已经晚了）；
//   - 正常轮结束后窗口金额必须等于 TurnResult.Cost，Entries 必须恰好 1
//     （Record + Observe 双写会让同一笔钱变成两笔，数值两边都"合理"）。
package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// periodTurn 构造一个带 owner/role 的文本 turn。
func periodTurn(session, turnID, owner, role, text string) TurnRequest {
	req := textTurn(session, turnID, text)
	req.OwnerID = owner
	req.Role = role
	return req
}

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestRunTurn_PeriodBudgetBlocksBeforeModel 钉住"超额不发起调用"：
// 窗口里已知下界已经超过 Limit 时，本轮必须在模型之前以 typed budget 终态结束。
func TestRunTurn_PeriodBudgetBlocksBeforeModel(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	l := NewCostLedger(testPrice)
	p := NewCostPeriodLedger(l, dailyWindow(10))
	p.SetNow(fixedClock(at))
	// 10 token × strong 单价 2.0 = 20，已超 10。
	p.RecordAt(CostEntry{
		CostKey:    CostKey{Role: RoleDialogue, TurnID: "seed", SessionID: "s0", OwnerID: "alice"},
		Model:      "strong",
		Usage:      types.Usage{InputTokens: 10},
		UsageKnown: true,
	}, at)

	up := &runnerUpstream{text: "不应出现"}
	r := newTestRunner(t, up, 3, nil, func(o *RunnerOptions) {
		o.Ledger = l
		o.Period = p
		o.RoleModels = map[string]string{RoleDialogue: "strong"}
		o.Now = fixedClock(at)
	})

	res, err := r.RunTurn(context.Background(), periodTurn("s1", "t-over", "alice", RoleDialogue, "在吗"), nil)
	if err == nil {
		t.Fatalf("超额时 RunTurn 必须报错, 实际 res=%+v", res)
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindBudget {
		t.Fatalf("错误分类应为 %q, 实际 %T %v", ErrKindBudget, err, err)
	}
	if up.count() != 0 {
		t.Fatalf("超额必须在模型调用之前拒绝, 实际上游请求 %d 次", up.count())
	}
	if res.Terminal.Reason != "budget_exhausted" {
		t.Fatalf("终态应为 budget_exhausted, 实际 %q", res.Terminal.Reason)
	}
	if !containsString(res.Degraded, "budget_exhausted:period:daily") {
		t.Fatalf("缺窗口归因: %v", res.Degraded)
	}
}

// TestRunTurn_PeriodBudgetRecordsExactlyOnce 钉住"一轮只入窗一次"：
// 已装 Ledger 时走 Observe，不能再走 Record（那会把底层账本与窗口都记两遍）。
func TestRunTurn_PeriodBudgetRecordsExactlyOnce(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	l := NewCostLedger(testPrice)
	p := NewCostPeriodLedger(l, dailyWindow(1000))
	p.SetNow(fixedClock(at))

	up := &runnerUpstream{text: "好的"}
	r := newTestRunner(t, up, 3, nil, func(o *RunnerOptions) {
		o.Ledger = l
		o.Period = p
		o.RoleModels = map[string]string{RoleDialogue: "strong"}
		o.Now = fixedClock(at)
	})

	res, err := r.RunTurn(context.Background(), periodTurn("s1", "t-ok", "alice", RoleDialogue, "在吗"), nil)
	if err != nil {
		t.Fatalf("未超额不应失败: %v", err)
	}
	if !res.CostKnown || res.Cost != 30 { // (10 in + 5 out) × 2.0
		t.Fatalf("本轮成本应为 30 且已知, 实际 cost=%v known=%v", res.Cost, res.CostKnown)
	}

	got := p.QueryAt(at, "alice", RoleDialogue)
	if len(got) != 1 {
		t.Fatalf("应有一个窗口, 实际 %d", len(got))
	}
	if got[0].Entries != 1 {
		t.Fatalf("同一轮被入窗 %d 次（Record+Observe 双写）", got[0].Entries)
	}
	if got[0].Spent != res.Cost {
		t.Fatalf("窗口金额 %v 与本轮成本 %v 不同源", got[0].Spent, res.Cost)
	}
	// 底层账本也只能有一条：双写会在这里变成 2。
	if tot := l.Snapshot().ByTurn["t-ok"]; tot.Entries != 1 || tot.Cost != 30 {
		t.Fatalf("底层账本条目/金额错误: entries=%d cost=%v", tot.Entries, tot.Cost)
	}
}

// TestRunTurn_PeriodBudgetUnknownCostDoesNotBlock 钉住"未知不是超限"：
// 价目表缺项时窗口 Known=false，但没有任何证据说明已超限，本轮必须放行，
// 同时把 period_cost_unknown 写进 Degraded（不能静默当成未超）。
func TestRunTurn_PeriodBudgetUnknownCostDoesNotBlock(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	l := NewCostLedger(testPrice) // 默认模型 "m" 不在价目表里 ⇒ 未知
	p := NewCostPeriodLedger(l, dailyWindow(1))
	p.SetNow(fixedClock(at))

	up := &runnerUpstream{text: "照常回答"}
	r := newTestRunner(t, up, 3, nil, func(o *RunnerOptions) {
		o.Ledger = l
		o.Period = p
		o.Now = fixedClock(at)
	})

	res, err := r.RunTurn(context.Background(), periodTurn("s1", "t-unknown", "alice", RoleDialogue, "在吗"), nil)
	if err != nil {
		t.Fatalf("未知成本不能当超限拒绝: %v", err)
	}
	if up.count() != 1 {
		t.Fatalf("未知成本时应照常调用模型一次, 实际 %d", up.count())
	}
	if res.CostKnown {
		t.Fatal("未知模型的成本不能被报告为已知")
	}
	if got := p.QueryAt(at, "alice", RoleDialogue); len(got) != 1 ||
		got[0].Known || got[0].UnknownEntries != 1 || got[0].Over {
		t.Fatalf("窗口应记录一笔未知但不判超: %+v", got)
	}
	// 第一轮尚不可能知道价目表缺项（记录是这一轮结束才产生的）；未知标记
	// 必须出现在下一轮的 Degraded 里，否则"窗口没超"会被误读成"确实没超"。
	res2, err := r.RunTurn(context.Background(), periodTurn("s2", "t-unknown-2", "alice", RoleDialogue, "还在吗"), nil)
	if err != nil {
		t.Fatalf("未知成本累计后也不应拒绝: %v", err)
	}
	if !containsString(res2.Degraded, "period_cost_unknown:daily") {
		t.Fatalf("缺未知成本标记: %v", res2.Degraded)
	}
	if up.count() != 2 {
		t.Fatalf("两轮都应放行, 实际上游 %d 次", up.count())
	}
}

// TestRunTurn_PeriodBudgetIsPerOwnerRole 是隔离判据：alice/dialogue 超额时
// bob/dialogue 与 alice/extract 都必须照常调用 —— 否则"按角色/用户配额"只是
// 一个全局上限的别名。
func TestRunTurn_PeriodBudgetIsPerOwnerRole(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	l := NewCostLedger(testPrice)
	p := NewCostPeriodLedger(l, dailyWindow(10))
	p.SetNow(fixedClock(at))
	p.RecordAt(CostEntry{
		CostKey:    CostKey{Role: RoleDialogue, TurnID: "seed", SessionID: "s0", OwnerID: "alice"},
		Model:      "strong",
		Usage:      types.Usage{InputTokens: 10},
		UsageKnown: true,
	}, at)

	up := &runnerUpstream{text: "好的"}
	r := newTestRunner(t, up, 3, nil, func(o *RunnerOptions) {
		o.Ledger = l
		o.Period = p
		o.RoleModels = map[string]string{
			RoleDialogue: "strong",
			RoleExtract:  "strong",
		}
		o.Now = fixedClock(at)
	})

	for _, req := range []TurnRequest{
		periodTurn("s2", "t-bob", "bob", RoleDialogue, "在吗"),
		periodTurn("s3", "t-extract", "alice", RoleExtract, "抽取"),
	} {
		if _, err := r.RunTurn(context.Background(), req, nil); err != nil {
			t.Fatalf("%s/%s 不应被 alice/dialogue 的额度拖累: %v", req.OwnerID, req.Role, err)
		}
	}
	if up.count() != 2 {
		t.Fatalf("隔离失败: 期望 2 次上游请求, 实际 %d", up.count())
	}
}
