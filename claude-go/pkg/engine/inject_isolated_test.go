// inject_isolated_test.go —— 运行中入站通道在 **RunIsolated** 上的那半 (21.8.12 P5)。
//
// ---------------------------------------------------------------------------
// 为什么这半必须单独有用例: 它和主循环那条是两套
// ---------------------------------------------------------------------------
//
// 主循环 (queryLoop) 的队列挂在引擎上, 由 QueryEngine.InjectMessage 收 ——
// inject_midrun_test.go 测的就是那一条。而**团队 stage 走的不是主循环**:
// CLI 的 cliAgentRunner.Execute → engine.RunIsolated, 而 RunIsolated 有自己的循环
// (runner.go), 引擎级队列在那条路上永远不会被 drain。
//
// 于是"改了主循环"这件事对团队路径**一点用都没有**, 而两条路在验收读数上完全同形:
// 都是"注入返回 true 而模型看不到"。改哪一条都不会让另一条的用例变红 —— 这正是本
// 文件存在的理由。
//
// ---------------------------------------------------------------------------
// 判据落在请求体上, 不落在队列上 (与 inject_midrun_test.go 同一条纪律)
// ---------------------------------------------------------------------------
//
// 断言"Inject 返回 true"只说明消息进了队列; 断言"Drain 取到了"只说明循环读了一次。
// 唯一说得清"送达"的读数是**模型真的看到了它** —— 也就是注入之后发往上游的请求体里
// 出现了那段文本。所以这里复用 runner_state_test.go 的假上游 (它记下每一次请求)。
//
// ---------------------------------------------------------------------------
// 第三条: 队列的身份是 run, 不是 engine
// ---------------------------------------------------------------------------
//
// CLI 团队路径下 agentPool 是 8 并发, 8 个 runner **共用同一个引擎**。队列挂在引擎上
// 时, "这条消息是给哪一次 run 的"没有答案 —— 表现是 A 阶段的更正被 B 阶段收走, 而两边
// 都看不出异常。TestInjectQueueIsPerRunNotPerEngine 钉的就是这一条: 同一个引擎上两个
// 并发的 run, 往其中一个注入, 只有那一个能看到。
package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// TestRunIsolatedDeliversInjectedMessage 是这半的核心判据。
//
// 用一个"永不收尾"的假上游 (toolTurns<0) 把循环拉住, 注入之后从**第 1 次请求起**
// 找那段文本; 第 0 次请求里不该有它 (注入发生在注入动作之后)。
//
// ★ 上游在**第 1 轮**被闸住再注入, 不能靠 waitFor 碰时机: 本地假上游一轮往返是
// 微秒级, 12 轮 MaxTurns 常常在测试还停在轮询间隔里就跑完了 —— 那时注入落在 run
// 之后, 用例会以"注入被拒"或"没送到"**假红**, 而且时快时慢。
func TestRunIsolatedDeliversInjectedMessage(t *testing.T) {
	const gateAt = 1
	u := newStateUpstream(t, -1, "收工") // 负数 = 永远回 tool_use, 只能靠 MaxTurns 结束
	id, cfg := stateIdentity()
	e := newStateEngine(t, u, cfg, t.TempDir())

	g := newUpstreamGate()
	defer g.release()
	// onRequest 在响应**之前**回调, 此刻循环正阻塞在 SendMessage 里 —— 这是唯一
	// 既能说"run 在跑"又能让它停下来的位置。
	u.onRequest = func(turn int, _ seenRequest) {
		if turn == gateAt {
			g.wait()
		}
	}

	q := NewInjectQueue()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.RunIsolated(context.Background(), "去干活", IsolatedRunOptions{
			MaxTurns: 12, Stage: id.Stage, Inject: q,
		})
	}()

	waitFor(t, func() bool { return u.count() > gateAt }, "run 没有走到闸门那一轮")
	if !q.Active() {
		t.Fatal("RunIsolated 跑起来了而队列认为没人接 —— Enter/Leave 没配上")
	}
	if !q.Inject(types.Message{Type: types.MessageTypeUser, UUID: "inj-1",
		CreatedAt: time.Now(),
		Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: injectMarker}}}) {
		t.Fatal("run 在跑, 注入必须被接受")
	}
	g.release()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("run 没有收尾")
	}

	reqs := u.requests()
	if len(reqs) < 3 {
		t.Fatalf("只发出了 %d 次请求, 太短 —— 这条用例会退化成假绿", len(reqs))
	}
	found := false
	for i := 1; i < len(reqs); i++ {
		if strings.Contains(reqs[i].Flat, injectMarker) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("注入的消息没有出现在任何一次后续请求里 (%d 次) —— "+
			"消息进了队列却没人取, 与没送达完全一样 (团队 stage 走的正是这条路径)", len(reqs))
	}
	if strings.Contains(reqs[0].Flat, injectMarker) {
		t.Error("第 0 次请求里就有注入内容 —— 注入必须发生在注入动作之后")
	}
	// ★ 注入必须**只出现一次**。
	//
	// 上面那个 Contains 判据对"投递两遍"毫无分辨力: 而把 Drain 的 `q.msgs = nil`
	// 删掉就正好是那样 —— 队列永远不清空, 每一轮的轮次边界都把**此前所有**注入
	// 重新追加一遍。表现是模型反复看到同一句话、请求体逐轮膨胀, 而文本本身完好,
	// 任何断言"有没有送到"的用例都不会红。(变异 M44 就是这么活下来的。)
	if got := strings.Count(reqs[len(reqs)-1].Flat, injectMarker); got != 1 {
		t.Fatalf("注入重复了 %d 次 —— 队列没被清空, 每一轮都把旧注入又追加一遍 "+
			"(请求体逐轮膨胀, 而'送到了'这件事照样成立)", got)
	}
}

// TestRunIsolatedWithoutQueueUnchanged 断言**不传队列**时逐字节走改造前的行为。
//
// 这条是"纯增量"的判据: RunIsolated 有大量既有调用方 (Orchestrator / 各种 bench),
// 它们不传 Inject。若进入通道的实现会顺手改掉别的东西 (多一条消息、少一轮、
// 动一下 messages 的顺序), 那些调用方会**静默**跟着变 —— 而它们的用例大多只断言
// 最终产出文本, 完全看不见。
func TestRunIsolatedWithoutQueueUnchanged(t *testing.T) {
	u := newStateUpstream(t, 2, "收工")
	id, cfg := stateIdentity()
	e := newStateEngine(t, u, cfg, t.TempDir())

	out, err := e.RunIsolated(context.Background(), "去干活",
		IsolatedRunOptions{MaxTurns: 5, Stage: id.Stage}) // 不传 Inject
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if out != "收工" {
		t.Fatalf("产出不对: %q", out)
	}
	// 请求数应当正好是 toolTurns+1 —— 一条注入都没发生。
	if n := u.count(); n != 3 {
		t.Fatalf("不传队列时不该有任何额外轮次 (请求 %d 次, 期望 3)", n)
	}
}

// TestRunIsolatedLeavesQueueOnExit 断言 run 结束后队列不再认为有人接。
//
// 漏掉 Leave 的后果不是崩溃: 队列会**永久认为有人接**, 之后每一条注入都被收下、
// 永远不被消费, 而没有任何信号 —— 调用方看到的是"送达了"。
func TestRunIsolatedLeavesQueueOnExit(t *testing.T) {
	u := newStateUpstream(t, 1, "收工")
	id, cfg := stateIdentity()
	e := newStateEngine(t, u, cfg, t.TempDir())

	q := NewInjectQueue()
	if _, err := e.RunIsolated(context.Background(), "去干活",
		IsolatedRunOptions{MaxTurns: 5, Stage: id.Stage, Inject: q}); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if q.Active() {
		t.Fatal("run 已结束而队列仍认为有人接 —— 之后所有注入都被收下却永不投递")
	}
	// 附带: 一条迟到的注入必须被**如实拒绝** (而不是收下)。
	if q.Inject(types.Message{Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: "太晚了"}}}) {
		t.Fatal("run 结束后注入必须被拒")
	}
}

// TestInjectQueueIsPerRunNotPerEngine 在 zz_inject_perrun_test.go —— 它要起两个并发
// run 并把上游闸住, 与这里这批单 run 用例的夹具不同, 所以分开放 (那条用例的注释里
// 记着为什么"靠 waitFor 碰时机"会变成假红)。

// upstreamGate 一个"无论如何都会被放行"的闸门, 用于把假上游的某一轮挡住,
// 让"此刻 run 一定在跑、且后面一定还有轮次"成为构造出来的事实 (见各用例的注释)。
//
// 它单独抽出来的理由是**失败形态**, 而不是复用:
// t.Fatal 之后 t.Cleanup 会调 srv.Close(), 而 httptest 的 Close 要等所有在途请求
// 结束。handler 正卡在闸门上时, 用例不失败、只是**不动**, 直到包级 -timeout 用墙钟
// 把它砸掉 —— 那时拿到的是"超时", 而"超时"与"断言抓住了它"是两回事: 变异测试里
// 前者会被记成 KILLED, 于是**一个纯粹由测试夹具造成的挂起会被误读成防线有效**。
//
// defer g.release() 是有效的: t.Fatal 走 runtime.Goexit, 它会跑完当前 goroutine 上
// 已注册的 defer 才终止。所以成功、失败两条路径上闸门都会开。
type upstreamGate struct {
	ch   chan struct{}
	once sync.Once
}

func newUpstreamGate() *upstreamGate { return &upstreamGate{ch: make(chan struct{})} }

func (g *upstreamGate) wait()    { <-g.ch }
func (g *upstreamGate) release() { g.once.Do(func() { close(g.ch) }) }

// waitFor 轮询一个条件 (超时即 fatal)。抽出来是因为这几条用例的等待条件都是
// "循环真的跑起来了", 各写一遍容易漏掉超时 —— 而漏掉的表现是**用例永久挂起**,
// 比失败难查得多。
//
// 超时定得比包级 -timeout 短得多是刻意的: 前置条件不成立时要**尽快如实失败**,
// 而不是把整个包拖到墙钟闸门那里 (那时报的是"包超时", 看不出是哪一条)。
func waitFor(t *testing.T, cond func() bool, why string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s (前置条件不成立)", why)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
