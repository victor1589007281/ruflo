// zz_inject_perrun_test.go —— 入站队列的身份是 **run**, 不是 engine (21.8.12 P5)。
//
// （文件名沿用本包的 zz_ 前缀约定: 它要起两个并发 run, 与 inject_isolated_test.go
// 里那批单 run 用例分开, 免得混读。）
//
// ---------------------------------------------------------------------------
// 为什么这条必须单独有用例
// ---------------------------------------------------------------------------
//
// 团队 stage 在 CLI 路径上的真实形状是: agentPool 8 并发, 而 8 个 runner
// **共用同一个 *QueryEngine** (见 cmd/claude-go 的 agentFactory 闭包与其注释)。
// 入站队列若挂在引擎上, "这条消息是给哪一次 run 的"就没有答案 —— 表现是 A 阶段的
// 更正被 B 阶段收走, 而**两边都看不出异常**: 收到的那个当真去做, 没收到的那个
// 以为没人给它发信 (与"发错地址静默丢弃"是同一形态, 见 takeMailFor 的注释)。
//
// ---------------------------------------------------------------------------
// 判据为什么落在"最后一次请求体"上
// ---------------------------------------------------------------------------
//
// 只断言"markerA 到了、markerB 也到了"是不够的: 一个把消息**广播给所有 run** 的
// 实现照样满足它, 而那正是本用例要排除的失效。所以每一条都要同时断言两件事 ——
// 自己的到得了, 别人的到不了。
//
// 取**最后一次**请求而不是"任意一次": 注入在轮次边界生效, 此后每一轮都该带着它,
// "只在头几轮有、后面被折叠掉了"这种失效也只有末次请求能抓住。
//
// ---------------------------------------------------------------------------
// 注入时机必须被钉住, 不能靠调度
// ---------------------------------------------------------------------------
//
// 假上游是本地 httptest, 一轮往返是**微秒级**, 12 轮 MaxTurns 常常在测试还停在
// waitFor 的轮询间隔里就跑完了。于是"注入"会落在 run 结束之后 —— 此时 Inject 要么
// 被拒 (用例以"注入被拒"失败), 要么在最后一轮前挤进去、永远不被 drain (用例以
// "没送到"失败)。两种都是**假红**, 而且时快时慢。
//
// 所以这里把上游**闸住**: 第 1 轮请求在 handler 里阻塞, 直到测试放行。这样一来
// "注入的这一刻 run 一定在跑、且后面一定还有轮次"就是构造出来的事实, 不是靠调度
// 碰运气。
package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestInjectQueueIsPerRunNotPerEngine(t *testing.T) {
	const (
		markerA = "更正A: 端口是 8443"
		markerB = "更正B: 换个目录"
		gateAt  = 1 // 在第 1 轮请求上闸住 (第 0 轮已回, 循环确定还在跑)
	)

	type runOut struct {
		u    *stateUpstreamRun
		done chan struct{}
		gate *upstreamGate
	}
	// start 起一个 run, 等在闸门口 (run 确定在跑), 注入, 再放行。
	start := func(marker string) runOut {
		u := newStateUpstream(t, -1, "收工") // 负数 = 永远回 tool_use, 只能靠 MaxTurns 收尾
		id, cfg := stateIdentity()
		e := newStateEngine(t, u, cfg, t.TempDir())
		q := NewInjectQueue()

		// 闸门用 upstreamGate 而不是裸 chan: 上面的 t.Fatal 走 Goexit 之后 defer 仍会
		// 跑, 闸门因此一定被放行 —— 裸 chan 的 close 在"已经 close 过"时会 panic,
		// 而"卡住不放行"会让 httptest 的 Close 干等, 把失败伪装成超时 (见该类型的注释)。
		g := newUpstreamGate()
		// 下面正常路径已显式放过一次; release 幂等, 这一句只为 t.Fatal 那条路径 ——
		// 否则闸门永不打开, httptest 的 Close 干等, 失败会伪装成包级超时。
		defer g.release()
		// onRequest 在响应**之前**回调, 此刻循环正阻塞在 SendMessage 里 —— 这里是
		// 唯一能既让 run "在跑"又让它"停下来"的位置。
		u.onRequest = func(turn int, _ seenRequest) {
			if turn == gateAt {
				g.wait()
			}
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = e.RunIsolated(context.Background(), "去干活",
				IsolatedRunOptions{MaxTurns: 12, Stage: id.Stage, Inject: q})
		}()

		// 等第 gateAt 轮请求**已经发出并在闸门里**。这一刻队列必然 Active:
		// Enter 在循环顶部、Leave 在 defer, 中间这一段没有出口。
		waitFor(t, func() bool { return u.count() > gateAt }, "run 没有走到闸门那一轮")
		if !q.Active() {
			t.Fatal("run 正卡在请求里而队列认为没人接 —— Enter/Leave 没配上")
		}
		if !q.Inject(types.Message{Type: types.MessageTypeUser, UUID: "uuid-" + marker,
			CreatedAt: time.Now(),
			Content:   []types.ContentBlock{{Type: types.ContentBlockText, Text: marker}}}) {
			t.Fatalf("run 在跑, 注入 %q 必须被接受", marker)
		}
		g.release()
		return runOut{u: u, done: done, gate: g}
	}

	a := start(markerA)
	b := start(markerB)
	// 两个闸门都由 start 内的 defer 兜着, 这里只是让失败路径更早收手。
	defer a.gate.release()
	defer b.gate.release()
	for _, r := range []runOut{a, b} {
		select {
		case <-r.done:
		case <-time.After(30 * time.Second):
			t.Fatal("run 没有收尾")
		}
	}

	lastFlat := func(u *stateUpstreamRun) string {
		reqs := u.requests()
		if len(reqs) == 0 {
			t.Fatal("这个 run 一次请求都没发出")
		}
		return reqs[len(reqs)-1].Flat
	}
	fa, fb := lastFlat(a.u), lastFlat(b.u)

	if !strings.Contains(fa, markerA) {
		t.Error("A 的注入没到 A 的 run")
	}
	if strings.Contains(fa, markerB) {
		t.Error("B 的注入串到了 A 的 run —— 队列挂在了引擎上, 而不是每次 run 自己一份")
	}
	if !strings.Contains(fb, markerB) {
		t.Error("B 的注入没到 B 的 run")
	}
	if strings.Contains(fb, markerA) {
		t.Error("A 的注入串到了 B 的 run —— 队列挂在了引擎上, 而不是每次 run 自己一份")
	}
}
