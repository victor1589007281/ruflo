// toolgate_wiring_test.go — R-H9 的**接线**验收: pkg/tool 里的确认/幂等闸已经
// 实现并被单测覆盖，本文件回答的是另一个问题 —— 生产构造路径到底有没有把它们
// 接到 tool.ToolContext 上。
//
// 为什么必须走 NewQueryEngine + 假上游，而不是直接调 RunToolUse：
//
//	pkg/tool 的测试只能证明"若 tctx 上有闸，闸会生效"。它拿不到 engine 的构造点，
//	所以对"engine 从不设置 Confirmed/Idempotency"这种缺陷完全无感 —— 那正是本次
//	修复前的状态：闸门建成、测试全绿、生产路径惰性。只有让真实引擎跑一轮模型
//	请求 → 工具调用，才能证明"没配置"和"配置了"在**工具眼里**是不同的。
//
// 三个用例刻意互为对照：用例 1 证明默认不凭空生效，用例 2/3 证明配置真的到达
// 工具。单独任何一条都能被一个写死的实现骗过（永远 nil / 永远非 nil）。
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ===========================================================================
// 假上游: 第一次请求返回 tool_use, 之后收口成文本
// ===========================================================================

// gateUpstream 只在**第一次**模型请求返回预置的 tool_use 帧，后续一律文本收口。
//
// 用请求计数而不是"看历史里有没有 tool_result"来切换：后者在被闸拒绝的路径上
// 同样会看到 tool_result（错误结果也是结果），于是一个"工具没执行"的用例可能
// 永远收不了口。计数是这里唯一稳定的信号。
type gateUpstream struct {
	mu          sync.Mutex
	firstFrames string
	calls       int
}

func (u *gateUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls++
		n := u.calls
		u.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = w.Write([]byte(u.firstFrames))
			return
		}
		_, _ = w.Write([]byte(textSSEFrames()))
	})
}

// toolUseFrame 是一个 content block 的渲染输入。
type toolUseFrame struct {
	ID    string
	Name  string
	Input string
}

// toolUseFrames 渲染一条带 N 个 tool_use 块的助手消息。
//
// 多块是幂等用例的前提：引擎会把同一条消息里的两个相同 tool_use 都送进执行，
// 而"同参数 = 同一次调用"只有执行点的 IdempotencyGate 知道。
func toolUseFrames(frames ...toolUseFrame) string {
	var b strings.Builder
	b.WriteString(sseWrite(types.StreamDelta{
		Type:    "message_start",
		Message: &types.APIResponse{ID: "msg_gate", Model: "m", Usage: &types.Usage{InputTokens: 10}},
	}))
	for i, f := range frames {
		b.WriteString(sseWrite(types.StreamDelta{
			Type:  "content_block_start",
			Index: i,
			ContentBlock: &types.ContentBlock{
				Type: types.ContentBlockToolUse, ID: f.ID, Name: f.Name,
			},
		}))
		b.WriteString(sseWrite(types.StreamDelta{
			Type: "content_block_delta", Index: i,
			Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: f.Input},
		}))
		b.WriteString(sseWrite(types.StreamDelta{Type: "content_block_stop", Index: i}))
	}
	b.WriteString(sseWrite(types.StreamDelta{
		Type:  "message_delta",
		Usage: &types.Usage{OutputTokens: 5},
		Delta: &types.DeltaContent{StopReason: "tool_use"},
	}))
	return b.String()
}

// newGateEngine 走生产构造路径（NewQueryEngine + 真 api.Client + 假上游）。
func newGateEngine(t *testing.T, up *gateUpstream, tools ...tool.Tool) *QueryEngine {
	t.Helper()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}

	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"

	return NewQueryEngine(&Config{
		Model:    "m",
		MaxTurns: 6,
	}, client, reg, hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)
}

// ===========================================================================
// 探针工具: 在 Call 里断言 tctx 上**确实**有接线
// ===========================================================================

// probe 记录工具眼里的世界。工具批次可能并发，所有写入加锁。
type probe struct {
	mu   sync.Mutex
	call int
	seen bool   // Call 里看到过非 nil 的闸
	err  string // Call 自己返回的错误（nil 时为空）
}

func (p *probe) run(kind string, wired bool, result string) (*tool.ToolResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.call++
	p.seen = wired
	if !wired {
		p.err = "未接线: " + kind + " 为 nil"
		return nil, fmt.Errorf("%s", p.err)
	}
	return &tool.ToolResult{Content: result}, nil
}

func (p *probe) snapshot() (calls int, seen bool, err string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call, p.seen, p.err
}

// confirmTool 刻意**不**声明 NeedsConfirmation()。
//
// 如果它声明了，orchestration 会在 Confirmed == nil 时直接拒绝，Call 根本不跑 ——
// 那么"未接线"与"已接线"在探针上又变得不可分辨了。这里要钉的是构造点是否把
// 检查器交给工具，所以让 Call 成为唯一的判据。
type confirmTool struct{ p *probe }

func (t *confirmTool) Name() string        { return "confirm_tool" }
func (t *confirmTool) Description() string { return "确认接线探针" }
func (t *confirmTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *confirmTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *confirmTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (t *confirmTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *confirmTool) Call(_ context.Context, _ json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	wired := tctx != nil && tctx.Confirmed != nil
	return t.p.run("Confirmed", wired, "confirm wired ok")
}

// idemTool 声明 Idempotent()，否则执行点的去重分支（orchestration.go 的
// `IdempotentOf(t)` 判据）根本不会读 tctx.Idempotency —— 那样"注入成功"与
// "注入了但永远不生效"就无法区分。
type idemTool struct{ p *probe }

func (t *idemTool) Name() string        { return "idem_tool" }
func (t *idemTool) Description() string { return "幂等接线探针" }
func (t *idemTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *idemTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *idemTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (t *idemTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (t *idemTool) Idempotent() bool                  { return true }
func (t *idemTool) Call(_ context.Context, _ json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	wired := tctx != nil && tctx.Idempotency != nil
	return t.p.run("Idempotency", wired, "idem wired ok")
}

// fixedChecker 是宿主确认的最小实现。true = 一律已确认。
type fixedChecker struct{ ok bool }

func (c fixedChecker) IsConfirmed(_ string, _ json.RawMessage) bool { return c.ok }

// ===========================================================================
// 用例
// ===========================================================================

// TestToolGateWiring_默认不凭空生效 是负向对照。
//
// 不设 RunOverrides 时，工具必须看到 nil —— 证明默认路径没有被某个全局单例
// 悄悄接上，也证明本测试能分辨"接了"和"没接"（否则用例 2/3 的通过毫无意义）。
func TestToolGateWiring_默认不凭空生效(t *testing.T) {
	p := &probe{}
	up := &gateUpstream{firstFrames: toolUseFrames(toolUseFrame{
		ID: "tu-1", Name: "confirm_tool", Input: `{}`,
	})}
	e := newGateEngine(t, up, &confirmTool{p: p})

	for range e.SubmitMessage(context.Background(), "跑一下") {
	}

	calls, seen, errMsg := p.snapshot()
	if calls != 1 {
		t.Fatalf("工具应被调用一次, 实际 %d", calls)
	}
	if seen {
		t.Fatal("未配置 RunOverrides 时工具不应看到 Confirmed（接线凭空生效了）")
	}
	if !strings.Contains(errMsg, "未接线") {
		t.Fatalf("工具 Call 应报未接线错误, 实际 %q", errMsg)
	}
}

// TestToolGateWiring_确认检查器到达工具 是正向判据。
func TestToolGateWiring_确认检查器到达工具(t *testing.T) {
	p := &probe{}
	up := &gateUpstream{firstFrames: toolUseFrames(toolUseFrame{
		ID: "tu-2", Name: "confirm_tool", Input: `{}`,
	})}
	e := newGateEngine(t, up, &confirmTool{p: p})

	ctx := WithRunOverrides(context.Background(), RunOverrides{Confirmation: fixedChecker{ok: true}})
	for range e.SubmitMessage(ctx, "跑一下") {
	}

	calls, seen, errMsg := p.snapshot()
	if calls != 1 {
		t.Fatalf("工具应被执行一次, 实际 %d", calls)
	}
	if !seen {
		t.Fatal("配置了 Confirmation, 但工具 Call 里看到的是 nil —— 构造点没接线")
	}
	if errMsg != "" {
		t.Fatalf("已接线时不应报错, 实际 %q", errMsg)
	}
}

// TestToolGateWiring_幂等闸到达工具 钉住第二个构造点。
func TestToolGateWiring_幂等闸到达工具(t *testing.T) {
	p := &probe{}
	up := &gateUpstream{firstFrames: toolUseFrames(toolUseFrame{
		ID: "tu-3", Name: "idem_tool", Input: `{}`,
	})}
	e := newGateEngine(t, up, &idemTool{p: p})

	gate := tool.NewIdempotencyGate(true, 0)
	ctx := WithRunOverrides(context.Background(), RunOverrides{Idempotency: gate})
	for range e.SubmitMessage(ctx, "跑一下") {
	}

	calls, seen, errMsg := p.snapshot()
	if calls != 1 {
		t.Fatalf("工具应被执行一次, 实际 %d", calls)
	}
	if !seen {
		t.Fatal("配置了 Idempotency, 但工具 Call 里看到的是 nil —— 构造点没接线")
	}
	if errMsg != "" {
		t.Fatalf("已接线时不应报错, 实际 %q", errMsg)
	}
}

// TestToolGateWiring_同参数两次命中幂等闸 是端到端的"去重真的发生"。
//
// 只断言"闸门非 nil"还不够：注入一个永不命中的闸同样能让上一个用例绿。
// 这里让假上游在同一条助手消息里发出两个**同名同参**的 tool_use，判据是
// 工具实际执行次数 == 1 且闸自己记到 hits == 1。
func TestToolGateWiring_同参数两次命中幂等闸(t *testing.T) {
	p := &probe{}
	up := &gateUpstream{firstFrames: toolUseFrames(
		toolUseFrame{ID: "tu-4a", Name: "idem_tool", Input: `{}`},
		toolUseFrame{ID: "tu-4b", Name: "idem_tool", Input: `{}`},
	)}
	e := newGateEngine(t, up, &idemTool{p: p})

	gate := tool.NewIdempotencyGate(true, 0)
	ctx := WithRunOverrides(context.Background(), RunOverrides{Idempotency: gate})
	for range e.SubmitMessage(ctx, "跑两次") {
	}

	calls, seen, errMsg := p.snapshot()
	if !seen || errMsg != "" {
		t.Fatalf("闸门应已接线且不报错: seen=%v err=%q", seen, errMsg)
	}
	if calls != 1 {
		t.Fatalf("同参数两次调用应只执行一次, 实际 %d 次", calls)
	}
	hits, misses := gate.Stats()
	if hits != 1 {
		t.Fatalf("第二次调用应命中幂等闸, hits=%d misses=%d", hits, misses)
	}
}

// TestRunOverrides_闸门字段参与零值判据 防"加字段时漏改 IsZero"。
//
// 漏改的后果不是编译错，而是 WithRunOverrides 静默丢弃整个覆盖（见
// RunOverridesFromContext 的零值分支）—— 闸门建成未通电的典型形态。
func TestRunOverrides_闸门字段参与零值判据(t *testing.T) {
	if !(RunOverrides{}).IsZero() {
		t.Fatal("零值 RunOverrides 必须判为零")
	}
	if (RunOverrides{Confirmation: fixedChecker{ok: true}}).IsZero() {
		t.Fatal("只带 Confirmation 的覆盖被当成零值 —— WithRunOverrides 会静默丢弃它")
	}
	if (RunOverrides{Idempotency: tool.NewIdempotencyGate(true, 0)}).IsZero() {
		t.Fatal("只带 Idempotency 的覆盖被当成零值 —— WithRunOverrides 会静默丢弃它")
	}

	// 端到端确认 WithRunOverrides 真的把只带闸门的覆盖写进了 ctx（而不是靠
	// RunOverridesFromContext 之外的其他路径生效）。
	ctx := WithRunOverrides(context.Background(), RunOverrides{Confirmation: fixedChecker{ok: true}})
	got, ok := RunOverridesFromContext(ctx)
	if !ok || got.Confirmation == nil {
		t.Fatalf("只带 Confirmation 的覆盖没有进入 ctx: ok=%v", ok)
	}
}
