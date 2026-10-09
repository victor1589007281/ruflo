// budget_gate_terminal_test.go — P0-1 / H0-5 的**接线**验收: 预算耗尽必须真的终止 run。
//
// 与同名的 internal_hook/budget_gate_test.go 分开是刻意的:
//
//	判据本身没写对  → internal_hook 那条抓 (给定消耗序列, trip/Exhausted 的值对不对);
//	判据没通电      → 这一条抓 (上限设到必然触发, run 的终态 Reason 是不是它)。
//
// 混在一条测试里, 两者都失败时无法区分"判据错了"和"判据没接上"。
//
// 验收口径按 21.5.1 的否定式写, 且判据落在**运行态**: 终态 Reason 的值、以及
// "预算耗尽后不可能再有第 N+1 次模型调用"这个断言 —— 不是"字段被赋了值"。
//
// 之所以断言"不再有新的模型调用"而不是数轮数: 轮数会因工具执行与否而变, 而
// **不再烧钱**才是这道闸门存在的唯一理由。假上游自己数请求数, 这是最直接的读数。
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// budgetUpstream 是一个只会一直要求调工具的假上游 —— 不给任何终止的借口,
// 唯一能让 run 停下来的东西就是被测的预算闸门。
//
// 必须发 SSE 而不是一整个 JSON 响应体: SubmitMessage 走的是 StreamMessage,
// 它逐行读 `data:` 帧。发一个非流式响应体的话, 客户端解析出一个空的 delta 序列,
// 于是 run 会因为"模型没调工具"而以 completed 收尾 —— 看起来像测试通过了终止断言,
// 实际上测的是"上游没被听懂"。
type budgetUpstream struct {
	perCallTokens int
	mu            sync.Mutex
	calls         int
}

// sseFrames 渲染一轮"调用 noop_tool"的 Anthropic SSE 事件序列。
//
// 分工刻意与客户端一致: input tokens 走 message_start (usage 在那里的 Message 上),
// output tokens 走 message_delta。两处都给, 因为闸门算的是四项之和, 只给一处会让
// "每次调用消耗多少"这个前置条件变得不直观。
func sseFrames(inTokens, outTokens int) string {
	write := func(v any) string {
		b, _ := json.Marshal(v)
		return "data: " + string(b) + "\n"
	}
	var b strings.Builder
	b.WriteString(write(types.StreamDelta{
		Type:    "message_start",
		Message: &types.APIResponse{ID: "msg_x", Model: "m", Usage: &types.Usage{InputTokens: inTokens}},
	}))
	b.WriteString(write(types.StreamDelta{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: &types.ContentBlock{
			Type: types.ContentBlockToolUse, ID: "tu1", Name: "noop_tool",
		},
	}))
	b.WriteString(write(types.StreamDelta{
		Type: "content_block_delta", Index: 0,
		Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: "{}"},
	}))
	b.WriteString(write(types.StreamDelta{Type: "content_block_stop", Index: 0}))
	b.WriteString(write(types.StreamDelta{
		Type:  "message_delta",
		Usage: &types.Usage{OutputTokens: outTokens},
		Delta: &types.DeltaContent{StopReason: "tool_use"},
	}))
	return b.String()
}

func (u *budgetUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		// 全部 token 计在 input 一侧: 闸门算的是四项之和, 分不分摊不影响判据,
		// 而单侧记账让"第 N 次调用后累计多少"在心算时不会错。
		_, _ = w.Write([]byte(sseFrames(u.perCallTokens, 0)))
	})
}

func (u *budgetUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// runBudgetEngine 起一个接上预算闸门的引擎跑到底, 返回 (终态, 上游请求数, 引擎)。
func runBudgetEngine(t *testing.T, perCallTokens, maxRunTokens, maxTurns int) (types.Terminal, int, *QueryEngine) {
	t.Helper()

	up := &budgetUpstream{perCallTokens: perCallTokens}
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}

	// 走生产构造路径 (NewQueryEngine), 不手工塞字段 —— 闸门是 applyFeatureFlags
	// 按 Config 装的, 手工塞等于绕开了被测的装配逻辑。
	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者" // 短路掉默认提示词装配, 让这条测试只测预算

	e := NewQueryEngine(&Config{
		Model:        "m",
		MaxTurns:     maxTurns,
		MaxRunTokens: maxRunTokens,
	}, client, reg, hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)

	ch := e.SubmitMessage(context.Background(), "干活")
	for range ch { // 消费完整个消息流, 确保循环真的跑完了
	}

	e.mu.Lock()
	terminal := e.lastTerminal
	e.mu.Unlock()

	return terminal, up.count(), e
}

// 判据 1 (主判据): 预算设到必然触发 ⇒ run 的终态原因**必须是** budget_exhausted。
//
// 对照是刻意设计进同一条测试里的: 同样的假上游、同样的轮数上限, 只是把预算放开,
// 终态就必须**不是** budget_exhausted。只测"会终止"是不够的 —— 原来也能终止
// (靠 maxTurns), 必须证明是新的判据在终止它 (与 21.5.1 对 H0-2 的"对照"要求同源)。
func TestH05_BudgetExhaustionTerminatesRun(t *testing.T) {
	const perCall, limit, maxTurns = 100, 250, 50

	terminal, calls, _ := runBudgetEngine(t, perCall, limit, maxTurns)
	if terminal.Reason != internal_hook.RunBudgetExhaustedReason {
		t.Fatalf("终态原因 = %q, 期望 %q —— 预算耗尽没有终止 run",
			terminal.Reason, internal_hook.RunBudgetExhaustedReason)
	}
	if terminal.Error == nil {
		t.Fatal("预算终止必须带 error (台账与告警都要读它), got nil")
	}

	// 3 次调用 = 300 > 250; 第 4 次调用绝不能发生。
	if calls > 4 {
		t.Fatalf("预算耗尽后仍在调用模型: %d 次 (上限 250 / 每次 100, 第 4 次就该被拦)", calls)
	}
	if calls < 3 {
		t.Fatalf("过早终止: 只调用了 %d 次, 上限 250 至少要花到第 3 次", calls)
	}

	// 对照臂: 不设预算 ⇒ 同一假上游下终态必须由 maxTurns 决定, 而不是预算。
	terminal2, calls2, _ := runBudgetEngine(t, perCall, 0, maxTurns)
	if terminal2.Reason == internal_hook.RunBudgetExhaustedReason {
		t.Fatal("未设预算却报 budget_exhausted —— 判据在没有上限时也在触发")
	}
	if calls2 <= calls {
		t.Fatalf("对照臂请求数 (%d) 应显著多于预算臂 (%d) —— 否则'终止'不是预算造成的", calls2, calls)
	}
}

// 判据 2: 预算远大于消耗 ⇒ 不可能触发, 行为与未设预算逐字相同。
//
// 这条守的是"加了闸门却改变了正常路径"—— 默认配置下不该有任何 run 被误杀。
func TestH05_GenerousBudgetIsInvisible(t *testing.T) {
	const perCall, maxTurns = 100, 3

	withGate, callsWith, _ := runBudgetEngine(t, perCall, 100_000, maxTurns)
	without, callsWithout, _ := runBudgetEngine(t, perCall, 0, maxTurns)

	if withGate.Reason == internal_hook.RunBudgetExhaustedReason {
		t.Fatalf("预算 100000 而实际只花几百, 却报了耗尽: %+v", withGate)
	}
	if withGate.Reason != without.Reason {
		t.Fatalf("宽松预算改变了终态: 有闸门=%q 无闸门=%q", withGate.Reason, without.Reason)
	}
	if callsWith != callsWithout {
		t.Fatalf("宽松预算改变了模型调用次数: %d vs %d —— 闸门不该影响未超支的 run",
			callsWith, callsWithout)
	}
}

// 判据 3: 终止后 transcript 不得以"悬空的 tool_use"收尾。
//
// 这是本实现选"tool 执行之后才检查"的全部理由: 若在 usage 累加处就返回, 最后一条
// assistant 消息里的 tool_use 永远等不到 tool_result, 那段历史此后任何 turn 都发不
// 出去 (Anthropic 会 400)。H1/H3 要的正是"停下之后还能接着干", 所以这里必须钉住。
func TestH05_TerminatedTranscriptHasNoDanglingToolUse(t *testing.T) {
	_, _, e := runBudgetEngine(t, 100, 250, 50)

	e.mu.Lock()
	msgs := append([]types.Message(nil), e.Messages...)
	e.mu.Unlock()

	// 收集所有已声明的 tool_use id 与已返回的 tool_result id。
	declared := map[string]bool{}
	returned := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case types.ContentBlockToolUse:
				declared[b.ID] = true
			case types.ContentBlockToolResult:
				returned[b.ToolUseID] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("前置条件失败: 这个假上游应当每轮都产出一个 tool_use")
	}
	for id := range declared {
		if !returned[id] {
			t.Fatalf("tool_use %q 没有对应的 tool_result —— 预算终止留下了一段送不出去的历史", id)
		}
	}
}
