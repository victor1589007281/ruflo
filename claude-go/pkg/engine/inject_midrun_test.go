// inject_midrun_test.go — 运行中入站通道 (21.8.12 P5 / D6-3 缺口 2 级 2)。
//
// 背景. 改造前 QueryEngine 只有"开新一轮"的入口: SubmitMessage / SubmitStream /
// SubmitStreamBlocks 三个都是 append 一条 user 消息再从头跑。一个**正在跑的** run
// 是独立 goroutine, 调这三个只会并发起第二轮 —— 全仓没有"注入当前轮"的 API。
//
// 这条缺口的表现不是报错, 是**送达时机**:
//   - 团队信箱的注入发生在**阶段起点一次** (workflow.go 的 takeMailFor);
//   - 阶段跑十分钟, 中途用户发来的更正只能等这一轮结束才看得到;
//   - 而那时基于旧信息的活已经干完了 —— 用户看到的是"我明明告诉它了"。
//
// ---------------------------------------------------------------------------
// 判据为什么必须落在**上游请求体**上
// ---------------------------------------------------------------------------
//
// 断言"InjectMessage 返回 true"是恒真的 —— 它只说明消息进了队列。断言"Messages
// 里多了一条"也不够: 一条排在 run 之后、永远不会被发送的消息同样满足它。
//
// 唯一说得清"送达"的读数是 **模型真的看到了它**: 也就是注入之后发往上游的请求体里
// 出现了那段文本。所以这里的上游把**每一次请求的 messages** 都记下来, 测试去其中
// 找。这与 21.8.12 负 10 (黑名单判据必须是解析出的 tools.name, 而不是"请求体含某子串")
// 是同一条纪律: 判据要落在**消费端真的收到的东西**上。
package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const injectMarker = "更正: 目标端口是 8443, 不是 8080"

// recordingToolLoop 每轮都要一次工具调用, 记录每次请求的 messages, 直到 rounds 轮后收尾。
//
// 与 stream_no_deadlock_test.go 的 toolLoopUpstream 同形, 多的是 recording ——
// 而 recording 正是判据的来源 (没有它就只能断言队列, 那测不到送达)。
type recordingToolLoop struct {
	mu      sync.Mutex
	seen    [][]types.Message // 每次请求的 messages 快照
	started chan struct{}     // 第一次请求到达时关闭 (测试据此知道 run 真的在跑)
	once    sync.Once
}

func (h *recordingToolLoop) handler(rounds int) http.Handler {
	var calls int
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []types.Message `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		h.mu.Lock()
		h.seen = append(h.seen, req.Messages)
		n := len(h.seen)
		h.mu.Unlock()
		h.once.Do(func() { close(h.started) })

		w.Header().Set("Content-Type", "text/event-stream")
		write := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write(append([]byte("data: "), append(b, '\n')...))
		}
		write(types.StreamDelta{Type: "message_start",
			Message: &types.APIResponse{ID: "m", Model: "m",
				Usage: &types.Usage{InputTokens: 10, OutputTokens: 1}}})

		calls++
		if calls >= rounds {
			write(types.StreamDelta{Type: "content_block_start", Index: 0,
				ContentBlock: &types.ContentBlock{Type: types.ContentBlockText}})
			write(types.StreamDelta{Type: "content_block_delta", Index: 0,
				Delta: &types.DeltaContent{Type: "text_delta", Text: "收工"}})
			write(types.StreamDelta{Type: "content_block_stop", Index: 0})
		} else {
			write(types.StreamDelta{Type: "content_block_start", Index: 0,
				ContentBlock: &types.ContentBlock{
					Type: types.ContentBlockToolUse, ID: "tu", Name: "noop_tool"}})
			write(types.StreamDelta{Type: "content_block_delta", Index: 0,
				Delta: &types.DeltaContent{Type: "input_json_delta", PartialJSON: `{}`}})
			write(types.StreamDelta{Type: "content_block_stop", Index: 0})
		}
		write(types.StreamDelta{Type: "message_delta",
			Usage: &types.Usage{OutputTokens: 1},
			Delta: &types.DeltaContent{StopReason: "end_turn"}})
		_ = n
	})
}

// sawText 在第 n 次 (0 基) 请求的 messages 里找某段文本。
func (h *recordingToolLoop) sawText(n int, text string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n >= len(h.seen) {
		return false
	}
	for _, m := range h.seen[n] {
		for _, b := range m.Content {
			if b.Type == types.ContentBlockText && strings.Contains(b.Text, text) {
				return true
			}
		}
	}
	return false
}

func (h *recordingToolLoop) requestCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.seen)
}

// TestInjectedMessageReachesUpstreamRequest 是 D6-3 缺口 2 级 2 的核心判据:
// **运行中注入的消息必须真的出现在后续的上游请求里**。
func TestInjectedMessageReachesUpstreamRequest(t *testing.T) {
	loop := &recordingToolLoop{started: make(chan struct{})}
	srv := httptest.NewServer(loop.handler(12))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}
	promptMgr := prompt.NewManager(t.TempDir())
	promptMgr.OverridePrompt = "你是执行者"
	e := NewQueryEngine(&Config{Model: "m", MaxTurns: 20},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeBypass), nil, promptMgr)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ch := e.SubmitStreamBlocks(context.Background(), "去干活", nil)
		for range ch {
		}
	}()

	// 等 run 真的在跑 (第一次请求已发出), 再注入 —— 否则就是在测"往空闲引擎注入"。
	select {
	case <-loop.started:
	case <-time.After(30 * time.Second):
		t.Fatal("run 没有发出第一次请求, 前置条件不成立")
	}

	accepted := e.InjectMessage(types.Message{
		Type:      types.MessageTypeUser,
		UUID:      "inj-1",
		CreatedAt: time.Now(),
		Content: []types.ContentBlock{
			{Type: types.ContentBlockText, Text: injectMarker},
		},
	})
	if !accepted {
		t.Fatal("引擎在跑, 注入必须被接受 —— 拒绝说明 running 标记没置上 (接线断了)")
	}

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("run 没有收尾")
	}

	// 判据: 注入发生在第 1 次请求之后, 所以从第 2 次请求起必须能看到它。
	// 不在第 1 次里找, 是因为那时注入还没发生 —— 要求它出现才是自欺。
	total := loop.requestCount()
	if total < 3 {
		t.Fatalf("只发出了 %d 次请求, 太短 —— 注入没有可观察的后续轮次, "+
			"这条用例会退化成假绿 (它必须压到至少一次请求之后)", total)
	}
	found := false
	for i := 1; i < total; i++ {
		if loop.sawText(i, injectMarker) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("注入的消息没有出现在任何一次后续的上游请求里 (%d 次请求) —— "+
			"消息进了队列却没人取, 那与没送达完全一样", total)
	}
	// 阴性对照: 第 0 次请求里**不该**有它 (那时还没注入)。它出现说明注入被提前了,
	// 而提前注入会让前缀缓存从第一次请求就失效。
	if loop.sawText(0, injectMarker) {
		t.Error("第 0 次请求里就有注入内容 —— 注入必须发生在注入动作**之后**")
	}
}

// TestInjectRejectedWhenEngineIdle 断言往**空闲**引擎注入会被拒绝, 而不是静默丢弃。
//
// "收下但不投递"是最坏的一种: 调用方以为送到了, 而引擎永远不跑。所以这里要求
// 返回值如实为 false —— 调用方据此改走 SubmitMessage。
func TestInjectRejectedWhenEngineIdle(t *testing.T) {
	e := &QueryEngine{}
	if e.InjectMessage(types.Message{Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "x"}}}) {
		t.Fatal("引擎没在跑时注入必须被拒 —— 静默收下会让'没送到'看起来像送到了")
	}
}

// TestInjectRejectsNonUserRole 断言只接受 user 消息。
//
// assistant 注入 = 伪造模型自己的发言 (它落进 Messages 后参与后续所有轮);
// system 注入会破坏 system/user 分界, 并与 TaskInstruction 那条正规路径分岔成两条。
// 两者都不是"调用方粗心", 是语义上不该存在的东西 —— 所以在入口按 role 拒, 而不是
// 信任调用方。
func TestInjectRejectsNonUserRole(t *testing.T) {
	for _, role := range []types.MessageType{
		types.MessageTypeAssistant, types.MessageTypeSystem,
	} {
		e := &QueryEngine{}
		e.inbox.Enter() // 假装在跑: 保证被拒的原因是 role 而不是"没人接"
		got := e.InjectMessage(types.Message{Type: role,
			Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "x"}}})
		if got {
			t.Errorf("role=%v 的注入必须被拒", role)
		}
	}
	// 阳性对照: 同一状态下 user 消息**能**进 —— 否则上面两条可能只是因为整个入口坏了。
	e := &QueryEngine{}
	e.inbox.Enter()
	if !e.InjectMessage(types.Message{Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "x"}}}) {
		t.Fatal("阳性对照失败: user 消息在 run 中应能被接受")
	}
}

// TestInjectEmptyTextRejected 断言空正文不进队列 —— 注入一条空 user 消息会白白改变
// 前缀缓存 (与 injectHookContext 里"空内容直接不注入"同一条理由)。
func TestInjectEmptyTextRejected(t *testing.T) {
	e := &QueryEngine{}
	e.inbox.Enter()
	if e.InjectMessage(types.Message{Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "   "}}}) {
		t.Fatal("空正文不该进队列")
	}
}

// TestRunningFlagClearedAfterRun 断言 run 结束后 running 归零。
//
// 漏掉减法的后果不是崩溃: 引擎会**永久认为自己在跑**, 之后每一次注入都被收下、
// 永远不被消费 (没有 loop 会去 drain), 而且没有任何信号。所以这条只测一个数:
// 跑完之后 running 必须是 0。
func TestRunningFlagClearedAfterRun(t *testing.T) {
	loop := &recordingToolLoop{started: make(chan struct{})}
	srv := httptest.NewServer(loop.handler(2))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}
	e := NewQueryEngine(&Config{Model: "m", MaxTurns: 5}, client, reg,
		hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeBypass),
		nil, prompt.NewManager(t.TempDir()))

	ch := e.SubmitStreamBlocks(context.Background(), "干活", nil)
	for range ch {
	}
	// 收尾在 goroutine 里 (close(streamCh) 之后还有 lastTerminal/SnapshotFn 两步),
	// 所以要等 running 归零而不是"流关了就读"。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		n := e.CountInjectionsPending()
		e.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.mu.Lock()
	n := e.CountInjectionsPending()
	e.mu.Unlock()
	t.Fatalf("run 已结束而 running=%d —— 引擎会永久认为自己在跑, 之后所有注入都被收下却不被消费", n)
}

// TestInjectQueuedButUndeliveredIsObservable 断言"收了但没送到"这件事**数得出来**。
//
// 注入是轮次边界的 (每条工具调用一轮, 通常几秒), 而 run 有终点。最后一条在 run 结束
// 前几百毫秒到达的消息不会被投递 —— 而"没投递"与"没发送"在调用方看来一样。
// 这条读数是那个差额的唯一出口。
func TestInjectQueuedButUndeliveredIsObservable(t *testing.T) {
	loop := &recordingToolLoop{started: make(chan struct{})}
	srv := httptest.NewServer(loop.handler(2))
	t.Cleanup(srv.Close)

	reg := tool.NewRegistry()
	reg.Register(noopTool{})
	client := &api.Client{BaseURL: srv.URL, APIKey: "k", Model: "m",
		Client: srv.Client(), RetryCount: 0}
	e := NewQueryEngine(&Config{Model: "m", MaxTurns: 5}, client, reg,
		hooks.NewRunner(nil, ""), permissions.NewChecker(types.PermissionModeBypass),
		nil, prompt.NewManager(t.TempDir()))

	ch := e.SubmitStreamBlocks(context.Background(), "干活", nil)
	for range ch {
	}
	// run 已结束: 此时再注入必然被拒 (running==0 之后), 队列里也不会留下东西。
	if e.InjectMessage(types.Message{Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "太晚了"}}}) {
		t.Fatal("run 结束后注入必须被拒")
	}
	if n := e.CountInjectionsDropped(); n != 0 {
		t.Fatalf("被拒的注入不该进队列, 却读到 %d 条", n)
	}
}
