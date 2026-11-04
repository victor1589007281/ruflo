// critical_hook_terminal_test.go — Moqi R-H7 的生产路径验收。
//
// hookpolicy_test.go 已经证明"链会返回 typed *HookError"，但那只是判据本身。
// 本次补的是**判据有没有通电**：引擎的调用点是否真的把 Critical 失败翻译成
// 可区分的终态并停止本轮。没有这条测试时，八个调用点即使全部只记日志继续，
// 链层测试仍然全绿 —— 那正是改造前的真实状态。
package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
)

// failingPreRequestHook 不实现 HookPolicy ⇒ 默认 Critical、无超时。
// 它模拟业务侧的安全 gate（例如危机拦截）失败。
type failingPreRequestHook struct {
	internal_hook.BaseInternalHook
}

func (h *failingPreRequestHook) Execute(*internal_hook.HookContext) (*internal_hook.HookResult, error) {
	return nil, errors.New("safety gate unavailable")
}

func TestQueryEngine_CriticalHookFailureTerminatesTurn(t *testing.T) {
	up := &captureUpstream{toolTurns: 0}
	e := newEphemeralEngine(t, up, 4)
	e.HookChain.Register(&failingPreRequestHook{
		BaseInternalHook: internal_hook.BaseInternalHook{
			Name_:     "failing-gate",
			Priority_: 1,
			Phases_:   []internal_hook.InternalHookPhase{internal_hook.PhasePreRequest},
		},
	})

	for range e.SubmitStreamBlocks(context.Background(), "hi", nil) {
	}

	term := e.LastTerminal()
	if term.Reason != "hook_failed" {
		t.Fatalf("Critical hook 失败应产生 hook_failed 终态, 实际 %q (err=%v)", term.Reason, term.Error)
	}
	if term.Error == nil {
		t.Fatal("hook_failed 终态必须带错误, 否则业务层无法区分失败原因")
	}
	if got := len(up.snapshot()); got != 0 {
		t.Fatalf("PreRequest Critical 失败后不得再发模型请求, 实际 %d 次", got)
	}
}
