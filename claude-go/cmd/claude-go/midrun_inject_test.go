// midrun_inject_test.go —— CLI 团队路径的运行中入站通道 (21.8.12 P5)。
//
// ---------------------------------------------------------------------------
// 为什么这条通道在 CLI 上要单独验一次
// ---------------------------------------------------------------------------
//
// 团队 stage 在 CLI 上走的是 cliAgentRunner.Execute → engine.RunIsolated, 而
// **不是** QueryEngine 的主循环。两条路的入站实现完全不同:
//
//	主循环   —— 队列挂在引擎上, 由 QueryEngine.InjectMessage 收 (pkg/engine);
//	隔离运行 —— 队列每次 Execute 新起一个, 由 IsolatedRunOptions.Inject 传进去。
//
// 于是"引擎那条改好了"对 CLI 团队路径一点用都没有, 而两条路在读数上完全同形:
// 都是"注入看起来成功了, 模型看不到"。pkg/engine 那批用例一条都不会因为这里写错
// 而变红 —— 它们不经过 cliAgentRunner。
//
// ---------------------------------------------------------------------------
// 判据落在**发往上游的请求体**上, 且必须真跑 Execute
// ---------------------------------------------------------------------------
//
// 断言 `InjectMidRun` 返回 true 太弱: 一条 role≠user 的消息会被队列拒掉, 而"没包对
// role"在返回值上与"引擎没在跑"完全同形 (都是 false), 排查方向完全在别处。
//
// 更要紧的是**不能把装配抄一遍来测**: 队列的建立、传给 RunIsolated、收尾摘除这三步
// 分散在 Execute 的首尾, 测试里手写一遍只会证明"我抄的这份是对的"。变异测试证实了
// 这一点 —— 抄一份的版本对"收尾不摘队列"(M50)、"RunIsolated 不传队列"(M51) 都全绿。
// 所以下面第三条**真调 Execute**, 只把上游换成本地假服务。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestCliRunnerMidRunInjectNeedsLiveQueue 断言没有正在跑的 run 时**如实拒绝**。
//
// "收下但不投递"是最坏的一种: 调用方以为送到了, 而队列永远不会被读。所以要求返回
// false —— 中继据此把消息退回信箱 (见 pkg/agent/mail_relay.go)。
func TestCliRunnerMidRunInjectNeedsLiveQueue(t *testing.T) {
	r := &cliAgentRunner{}
	if r.CanInjectMidRun() {
		t.Fatal("没装配队列时 CanInjectMidRun 必须为假")
	}
	if r.InjectMidRun("x") {
		t.Fatal("没装配队列时注入必须被拒")
	}
	// 装配了队列但没人接 (Enter 没调) —— 同样必须拒。
	r.inject = engine.NewInjectQueue()
	if r.CanInjectMidRun() {
		t.Fatal("队列存在但没有 run 在跑时 CanInjectMidRun 必须为假")
	}
	if r.InjectMidRun("x") {
		t.Fatal("没有 run 在跑时注入必须被拒")
	}
}

// TestCliRunnerMidRunInjectWrapsAsUserMessage 断言注入物是**一条 user 消息**。
//
// role 写错不会有任何编译错误, 而且现形方式只有"注入静默失效"一种 (队列按 role 拒)。
// 所以这里读回队列里那条, 断言 Type 与正文。
func TestCliRunnerMidRunInjectWrapsAsUserMessage(t *testing.T) {
	r := &cliAgentRunner{}
	q := engine.NewInjectQueue()
	q.Enter() // 假装 run 在跑
	r.inject = q

	if !r.InjectMidRun("更正: 端口是 8443") {
		t.Fatal("run 在跑时注入必须被接受")
	}
	got := q.Drain()
	if len(got) != 1 {
		t.Fatalf("队列里应有 1 条, 实到 %d", len(got))
	}
	if got[0].Type != types.MessageTypeUser {
		t.Fatalf("注入物的 role 是 %v —— 非 user 的消息会被队列拒掉, "+
			"而它现形的方式只有'注入静默失效'一种", got[0].Type)
	}
	var text strings.Builder
	for _, b := range got[0].Content {
		if b.Type == types.ContentBlockText {
			text.WriteString(b.Text)
		}
	}
	if !strings.Contains(text.String(), "更正: 端口是 8443") {
		t.Fatalf("注入正文不对: %q", text.String())
	}
}

// ---- 真跑 Execute --------------------------------------------------------

const cliInjectMarker = "更正: 目标端口是 8443, 不是 8080"

// cliToolTurns 前几轮回 tool_use 驱动工具循环 —— 必须有**多于一轮**的后续请求,
// 否则注入发生时循环已经走到收尾, 它永远没有可观察的落点 (用例会以"太短"假红)。
const cliToolTurns = 3

// cliUpstream 假上游: 第 1 轮时**在 handler 里**注入 (此刻 Execute 正阻塞在
// SendMessage 里, 队列必然 Active), 之后正常收尾。记录每次请求体。
type cliUpstream struct {
	mu   sync.Mutex
	flat []string
}

func (u *cliUpstream) start(t *testing.T, r *cliAgentRunner) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		var flat strings.Builder
		for _, m := range body.Messages {
			flat.Write(m.Content)
		}
		u.mu.Lock()
		turn := len(u.flat)
		u.flat = append(u.flat, flat.String())
		u.mu.Unlock()

		// 第 0 轮: 此刻 run 确定在跑 (Enter 在循环顶部, Leave 在 defer, 中间没有
		// 出口)。在这里注入, 时机是构造出来的, 不靠调度 —— 本地假上游一轮是微秒级,
		// 从测试协程注入会常常落在 run 结束之后 (那种假红时快时慢)。
		if turn == 0 {
			if !r.InjectMidRun(cliInjectMarker) {
				t.Errorf("run 在跑, 注入必须被接受")
			}
		}

		resp := types.APIResponse{
			ID: "m", Type: "message", Role: "assistant", Model: req.Header.Get("x-model"),
			StopReason: "end_turn",
			Usage:      &types.Usage{InputTokens: 5, OutputTokens: 5},
		}
		if turn < cliToolTurns {
			resp.Content = []types.ContentBlock{
				{Type: types.ContentBlockToolUse, ID: "tu0", Name: "noop_tool",
					Input: json.RawMessage(`{}`)},
			}
			resp.StopReason = "tool_use"
		} else {
			resp.Content = []types.ContentBlock{
				{Type: types.ContentBlockText, Text: "收工"},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (u *cliUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.flat...)
}

// noopTestTool 一个不做事的工具 (名字与假上游回的一致), 只为把循环驱动成多轮。
type noopTestTool struct{}

func (noopTestTool) Name() string        { return "noop_tool" }
func (noopTestTool) Description() string { return "noop" }
func (noopTestTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (noopTestTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (noopTestTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}
func (noopTestTool) IsReadOnly(_ json.RawMessage) bool { return true }
func (noopTestTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	return &tool.ToolResult{Content: "ok"}, nil
}

// TestCliRunnerExecuteDeliversMidRunInjection 是这条路径的核心判据, 且**真调 Execute**。
//
// 三件事一起成立才算通过, 而它们各自会独立失效:
//   - Execute 装配了队列并把 Role 包对 (否则注入被拒 —— 上面第一条已单独验);
//   - 队列**传进了** RunIsolated (漏传的话队列 Active 但永远没人 drain);
//   - 注入真的到了模型眼前 (落在请求体上, 不是落在返回值上)。
func TestCliRunnerExecuteDeliversMidRunInjection(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(noopTestTool{})

	u := &cliUpstream{}
	r := &cliAgentRunner{role: "coder", systemPrompt: "你是 coder"}
	// eng 先建好 (只需要一个带 APIClient 的引擎) —— 假上游要在闭包里回调 r,
	// 而 r.eng 又要在建引擎时用到 srv.URL, 所以先起 srv 再建 eng (见下面 srv 的用法)。
	srv := u.start(t, r)
	r.eng = engine.NewQueryEngine(
		&engine.Config{Model: "fake", MaxTurns: 8, PermissionMode: types.PermissionModeAcceptEdits},
		&api.Client{BaseURL: srv.URL, APIKey: "k", Model: "fake", Client: srv.Client(), RetryCount: 0},
		reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, nil)

	out, err := r.Execute(context.Background(), "去干活")
	if err != nil {
		t.Fatalf("Execute 失败: %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "收工") {
		t.Fatalf("产出不对: %q", out)
	}

	reqs := u.requests()
	if len(reqs) < 3 {
		t.Fatalf("只发出了 %d 次请求, 太短 —— 注入没有可观察的后续轮次", len(reqs))
	}
	// 注入发生在第 0 轮, 所以从**第 1 轮**起必须能看到它。
	seen := false
	for i := 1; i < len(reqs); i++ {
		if strings.Contains(reqs[i], cliInjectMarker) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("运行中注入没有出现在任何后续请求里 (%d 次) —— "+
			"队列建了却没人取, 或没传进 RunIsolated", len(reqs))
	}
	if strings.Contains(reqs[0], cliInjectMarker) {
		t.Error("第 0 次请求里就有注入内容 —— 注入必须发生在注入动作之后")
	}

	// 收尾必须把队列摘干净。摘不掉的后果不是崩溃: 下一次注入会落进一个**没人读**的
	// 队列 (上一个 run 的), 而调用方看到的是"送达了"。
	if r.inject != nil {
		t.Fatal("Execute 收尾后队列没摘掉 —— 之后的注入会进一个没人读的队列")
	}
	if r.CanInjectMidRun() {
		t.Fatal("Execute 收尾后仍认为有 run 在跑")
	}
}
