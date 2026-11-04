// ephemeral_test.go — Moqi R-H1 的**接线**验收: 逐轮临时上下文必须真的逐轮生效,
// 且真的不落进历史。
//
// 判据分三层, 缺一层这个改造就等于没做:
//
//	① 判据本身        → collectEphemeral / appendEphemeralBlocks 的单元断言;
//	② 判据有没有通电  → 起真引擎 + 假上游, 看**发出去了什么字节**;
//	③ 副作用有没有发生 → 跑完之后 messages 里**没有**证据块。
//
// 第三条是本次改造的全部要害。只验①和②的话, 一个"往 messages 里 append 再在每个
// 返回路径删掉"的实现照样能通过 —— 而那种实现在漏删的那条路径上静默地把检索结果
// 写进了 transcript。所以这里断言的是"跑完之后 messages 的正文里没有证据块"。
//
// ★ 这里把"轮"钉死为**用户 turn** (一次 SubmitMessage), 不是"模型请求"。
// 一个用户 turn 内部会因工具调用循环多次; 契约要求 provider 每个用户 turn 只被
// 调用一次, 且该 turn 内的每一次请求都带**逐字节相同**的块 —— 否则同一位置的内容
// 逐次漂移, 缓存断点之后每轮都变, 前缀缓存被自己打掉。
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
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// captureUpstream 记录每一次请求的原始 body, 并按需返回工具调用或文本收口。
type captureUpstream struct {
	mu        sync.Mutex
	bodies    []string
	toolTurns int // 每次 SubmitMessage 里前多少轮返回 tool_use
	calls     int // 跨多次 SubmitMessage 的累计请求数
}

func (u *captureUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				raw.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		u.mu.Lock()
		u.calls++
		call := u.calls
		u.bodies = append(u.bodies, raw.String())
		u.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if call <= u.toolTurns {
			_, _ = w.Write([]byte(sseFramesToolUse("tu" + string(rune('0'+call)))))
			return
		}
		_, _ = w.Write([]byte(textSSEFrames()))
	})
}

func (u *captureUpstream) snapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.bodies))
	copy(out, u.bodies)
	return out
}

func sseWrite(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n"
}

// sseFramesToolUse 渲染一轮"调用 noop_tool"。
func sseFramesToolUse(toolID string) string {
	var b strings.Builder
	b.WriteString(sseWrite(types.StreamDelta{
		Type:    "message_start",
		Message: &types.APIResponse{ID: "msg_x", Model: "m", Usage: &types.Usage{InputTokens: 10}},
	}))
	b.WriteString(sseWrite(types.StreamDelta{
		Type:  "content_block_start",
		Index: 0,
		ContentBlock: &types.ContentBlock{
			Type: types.ContentBlockToolUse, ID: toolID, Name: "noop_tool",
		},
	}))
	b.WriteString(sseWrite(types.StreamDelta{
		Type: "content_block_delta", Index: 0,
		Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: "{}"},
	}))
	b.WriteString(sseWrite(types.StreamDelta{Type: "content_block_stop", Index: 0}))
	b.WriteString(sseWrite(types.StreamDelta{
		Type:  "message_delta",
		Usage: &types.Usage{OutputTokens: 5},
		Delta: &types.DeltaContent{StopReason: "tool_use"},
	}))
	return b.String()
}

// textSSEFrames 渲染一轮纯文本收口。
func textSSEFrames() string {
	var b strings.Builder
	b.WriteString(sseWrite(types.StreamDelta{
		Type:    "message_start",
		Message: &types.APIResponse{ID: "msg_x", Model: "m", Usage: &types.Usage{InputTokens: 10}},
	}))
	b.WriteString(sseWrite(types.StreamDelta{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: &types.ContentBlock{Type: types.ContentBlockText},
	}))
	b.WriteString(sseWrite(types.StreamDelta{
		Type: "content_block_delta", Index: 0,
		Delta: &types.DeltaContent{Type: "text_delta", Text: "好的"},
	}))
	b.WriteString(sseWrite(types.StreamDelta{Type: "content_block_stop", Index: 0}))
	b.WriteString(sseWrite(types.StreamDelta{
		Type:  "message_delta",
		Usage: &types.Usage{OutputTokens: 5},
		Delta: &types.DeltaContent{StopReason: "end_turn"},
	}))
	return b.String()
}

// newEphemeralEngine 起一个走生产构造路径的引擎, 接假上游。
func newEphemeralEngine(t *testing.T, up *captureUpstream, maxTurns int) *QueryEngine {
	t.Helper()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}

	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"

	return NewQueryEngine(&Config{
		Model:    "m",
		MaxTurns: maxTurns,
	}, client, reg, hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeAcceptEdits), nil, promptMgr)
}

// TestEphemeral_OneCallPerUserTurnStableWithinTurn 钉住两条契约:
//   - provider 每个用户 turn 只调一次 (turn 内有 N 次模型请求, 但 provider 只问一次);
//   - 该 turn 内每一次请求都带**逐字节相同**的块 (前缀缓存不被自己打掉)。
func TestEphemeral_OneCallPerUserTurnStableWithinTurn(t *testing.T) {
	up := &captureUpstream{toolTurns: 2} // 第一个用户 turn 里 3 次模型请求
	e := newEphemeralEngine(t, up, 8)

	const marker = "证据块-MARKER-7f3a"
	var provided int
	ctx := WithEphemeralProvider(context.Background(), func(_ context.Context, _ EphemeralView) ([]types.ContentBlock, string, error) {
		provided++
		return []types.ContentBlock{{Type: types.ContentBlockText, Text: marker}}, "", nil
	})

	for range e.SubmitMessage(ctx, "第一句") {
	}
	bodies := up.snapshot()
	if len(bodies) != 3 {
		t.Fatalf("期望 3 次模型请求 (2 次工具 + 1 次收口), 实际 %d", len(bodies))
	}
	if provided != 1 {
		t.Fatalf("一个用户 turn 内 provider 应只被调 1 次, 实际 %d 次", provided)
	}
	for i, body := range bodies {
		if n := strings.Count(body, marker); n != 1 {
			t.Errorf("第 %d 次请求里证据块出现 %d 次, 期望恰好 1 次", i+1, n)
		}
	}

	// ③ 跑完之后 messages 里没有证据块正文 —— 没进 transcript。
	for _, m := range e.GetMessages() {
		for _, b := range m.Content {
			if strings.Contains(b.Text, marker) {
				t.Fatalf("证据块进了 messages (没被剥离): %q", b.Text)
			}
		}
	}
}

// TestEphemeral_SecondTurnSeesNewEvidenceHistoryHoldsNone 是契约的主验收:
//   - 第 2 个用户 turn 的 provider 能拿到前一轮的历史 (可以检索到新事实);
//   - 但第 2 个 turn 的请求里**只有**本轮的块, 第 1 轮的块不在;
//   - 跑完之后 messages 里两个块都没有。
func TestEphemeral_SecondTurnSeesNewEvidenceHistoryHoldsNone(t *testing.T) {
	up := &captureUpstream{toolTurns: 0}
	e := newEphemeralEngine(t, up, 4)

	var turn int
	ctx := WithEphemeralProvider(context.Background(), func(_ context.Context, view EphemeralView) ([]types.ContentBlock, string, error) {
		turn++
		// 第二轮能看到第一轮已经落进历史的那句话 —— 这就是"能检索到新事实"
		// 在引擎侧的等价物: provider 拿到的是**本轮开始时的完整历史**。
		if turn == 2 {
			found := false
			for _, m := range view.History {
				for _, b := range m.Content {
					if strings.Contains(b.Text, "我昨天搬家了") {
						found = true
					}
				}
			}
			if !found {
				t.Error("第 2 轮的 provider 没看到第 1 轮的用户消息, 历史视图是空的")
			}
		}
		return []types.ContentBlock{{Type: types.ContentBlockText, Text: "EV" + string(rune('0'+turn))}}, "", nil
	})

	for range e.SubmitMessage(ctx, "我昨天搬家了") {
	}
	for range e.SubmitMessage(ctx, "那我新家附近有什么") {
	}

	if turn != 2 {
		t.Fatalf("两个用户 turn 应各调 provider 一次, 实际 %d 次", turn)
	}
	bodies := up.snapshot()
	last := bodies[len(bodies)-1]
	if !strings.Contains(last, "EV2") {
		t.Error("末次请求没有本轮证据块")
	}
	if strings.Contains(last, "EV1") {
		t.Error("末次请求里仍带着上一轮的证据块 —— 临时块被累积进了历史")
	}
	for _, m := range e.GetMessages() {
		for _, b := range m.Content {
			if strings.Contains(b.Text, "EV") && strings.HasPrefix(strings.TrimSpace(b.Text), "EV") {
				t.Fatalf("证据块进了 messages: %q", b.Text)
			}
		}
	}
}

// TestEphemeral_ProviderErrorDegradesNotFails 钉住失败语义: provider 挂了本轮照常回复。
//
// 这是与"critical hook 失败即终止"的**对照**: 同一个引擎、同一条链路, 只是把失败
// 放在 advisory 位置上, 结果必须是 completed 而不是 failed。只测"降级了"不够 ——
// 必须证明 run 没有因此终止。
func TestEphemeral_ProviderErrorDegradesNotFails(t *testing.T) {
	up := &captureUpstream{toolTurns: 0}
	e := newEphemeralEngine(t, up, 4)

	ctx := WithEphemeralProvider(context.Background(), func(_ context.Context, _ EphemeralView) ([]types.ContentBlock, string, error) {
		return nil, "", context.DeadlineExceeded
	})
	for range e.SubmitMessage(ctx, "hi") {
	}

	e.mu.Lock()
	terminal := e.lastTerminal
	e.mu.Unlock()
	if terminal.Reason != "completed" {
		t.Fatalf("provider 失败后终态应为 completed (降级), 实际 %q (%v)", terminal.Reason, terminal.Error)
	}
	if len(up.snapshot()) == 0 {
		t.Fatal("降级后仍应调用模型")
	}
}

// TestEphemeral_NoProviderIsByteIdentical 是回归判据: 没装 provider 时请求字节不变。
//
// 与"装了 provider 但返回空"分开测: 前者是 R-H16 要求的"新能力默认关闭时逐字节
// 相同", 后者是"装了但没内容" —— 两者走的是不同分支 (nil 检查 vs 空块检查)。
func TestEphemeral_NoProviderIsByteIdentical(t *testing.T) {
	runOnce := func(withProvider bool) string {
		up := &captureUpstream{toolTurns: 0}
		e := newEphemeralEngine(t, up, 3)
		ctx := context.Background()
		if withProvider {
			ctx = WithEphemeralProvider(ctx, func(_ context.Context, _ EphemeralView) ([]types.ContentBlock, string, error) {
				return nil, "", nil // 装了, 但零块零降级
			})
		}
		for range e.SubmitMessage(ctx, "同一句话") {
		}
		bodies := up.snapshot()
		if len(bodies) == 0 {
			t.Fatal("没有发出请求")
		}
		return bodies[0]
	}

	plain := runOnce(false)
	empty := runOnce(true)
	if plain != empty {
		t.Fatalf("装了空 provider 改变了请求字节:\n无 provider: %s\n空 provider: %s",
			shorten(plain), shorten(empty))
	}
}

func shorten(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

// TestCollectEphemeral_DropsEmptyBlocks 钉住"坏块不发上游":
// 空文本块会让部分上游 400, 一个 provider 的 bug 不该变成整轮失败。
func TestCollectEphemeral_DropsEmptyBlocks(t *testing.T) {
	p := func(_ context.Context, _ EphemeralView) ([]types.ContentBlock, string, error) {
		return []types.ContentBlock{
			{Type: types.ContentBlockText, Text: "  \n\t "},
			{Type: types.ContentBlockText, Text: "有内容"},
		}, "", nil
	}
	blocks, degraded, err := collectEphemeral(context.Background(), p, 0, nil)
	if err != nil {
		t.Fatalf("普通 provider 失败不应返回 error（默认降级）: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Text != "有内容" {
		t.Fatalf("空块应被丢弃, 实际 %+v", blocks)
	}
	if degraded != "context_block_empty" {
		t.Fatalf("空块应记降级, 实际 %q", degraded)
	}
}

// TestAppendEphemeralBlocks_OnlyIntoLastUserMessage 钉住位置契约:
// 临时块只能并进**最后一条 user 消息**, 不得新增一条消息 (那会破坏角色交替,
// 部分上游会合并连续 user 消息, 于是位置假设失效)。
func TestAppendEphemeralBlocks_OnlyIntoLastUserMessage(t *testing.T) {
	mk := func(role string) types.APIMessage {
		raw, _ := json.Marshal([]types.ContentBlock{{Type: types.ContentBlockText, Text: role}})
		return types.APIMessage{Role: role, Content: raw}
	}
	in := []types.APIMessage{mk("user"), mk("assistant"), mk("user")}
	out := appendEphemeralBlocks(in, []types.ContentBlock{{Type: types.ContentBlockText, Text: "证据"}})

	if len(out) != len(in) {
		t.Fatalf("不得新增消息: 前 %d 条, 后 %d 条", len(in), len(out))
	}
	var last []types.ContentBlock
	if err := json.Unmarshal(out[len(out)-1].Content, &last); err != nil {
		t.Fatal(err)
	}
	if len(last) != 2 || last[1].Text != "证据" {
		t.Fatalf("证据块应并进最后一条 user 消息, 实际 %+v", last)
	}
	// 前面几条必须逐字节没变。
	for i := 0; i < len(in)-1; i++ {
		if string(out[i].Content) != string(in[i].Content) {
			t.Errorf("第 %d 条被改动", i)
		}
	}
}

// TestAppendEphemeralBlocks_NoTrailingUserNoInjection 钉住边界:
// 末尾不是 user 时不注入 —— 硬塞一条新 user 会破坏角色交替。
func TestAppendEphemeralBlocks_NoTrailingUserNoInjection(t *testing.T) {
	raw, _ := json.Marshal([]types.ContentBlock{{Type: types.ContentBlockText, Text: "a"}})
	in := []types.APIMessage{{Role: "assistant", Content: raw}}
	out := appendEphemeralBlocks(in, []types.ContentBlock{{Type: types.ContentBlockText, Text: "证据"}})
	if len(out) != 1 || string(out[0].Content) != string(raw) {
		t.Fatalf("末尾非 user 时不应注入, 实际 %+v", out)
	}
}
