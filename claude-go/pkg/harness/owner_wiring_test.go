// owner_wiring_test.go — R-H6 的**端到端**接线验收。
//
// owner_test.go 验的是归一化与错误类型（纯函数），pkg/tool 的 owner_gate_test.go
// 验的是闸门本身。两条都绿，仍然可能整条链路不通：runner 不把 req.OwnerID 写进
// ctx，engine 就退回 Config/默认值，闸门两边都是 "local" —— 一致、放行、
// 而用户的身份从未离开 TurnRequest。这个文件补的就是那一段。
//
// 判据只有一条：**工具看到的 owner 必须等于请求里的 owner**。工具是整条链路上
// 最后一个消费方，它看到的不是外部传下来的假值时，链路才算通。
package harness

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ownerCaptureTool 记录工具上下文里的 owner。
//
// 同时记 ctx 里的 owner：两者不等说明"写进 ctx 的"与"透传给工具的"来自不同
// 来源 —— 那正是 R-H6 最容易出的错法（两处各读一处字段）。RunToolUse 的闸门
// 会在不一致时拒绝执行，所以正常情况下两个值恒等；记录两者是为了让不一致时
// 测试能指出是哪一边跑偏，而不是只看到"调用次数为 0"。
type ownerCaptureTool struct {
	mu     sync.Mutex
	owners []string
}

func (o *ownerCaptureTool) Name() string                 { return "noop_tool" } // 与 runnerUpstream 的 tool_use 同名
func (o *ownerCaptureTool) Description() string          { return "owner capture" }
func (o *ownerCaptureTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (o *ownerCaptureTool) Call(ctx context.Context, _ json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if tctx == nil {
		o.owners = append(o.owners, "<nil-tctx>")
		return &tool.ToolResult{Content: "ok"}, nil
	}
	// 用 NUL 分隔两个字段：owner 可能含任意字符（邮箱、UUID、带前缀的租户名），
	// 用 "|" 之类会让 "a|b"+"c" 与 "a"+"b|c" 撞成同一条记录。
	o.owners = append(o.owners, tctx.SessionID+"\x00"+tctx.OwnerID+"\x00"+types.OwnerFromContext(ctx))
	return &tool.ToolResult{Content: "ok"}, nil
}

func (o *ownerCaptureTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (o *ownerCaptureTool) CheckPermissions(json.RawMessage, *tool.ToolContext) *types.PermissionResult {
	return &types.PermissionResult{Behavior: types.PermissionAllow}
}
func (o *ownerCaptureTool) IsReadOnly(json.RawMessage) bool { return true }

// swapTool 把 runner 引擎注册表里的 noop_tool 换成 capture 版本。
//
// 注册表覆盖同名工具是 Register 的既定语义（见 pkg/tool.Registry.Register），
// 所以这里不需要改动 newTestRunner 这个共享夹具 —— 共享夹具被改一次，本文件
// 之外的用例都要跟着受影响。
func swapTool(r *Runner) *ownerCaptureTool {
	cap := &ownerCaptureTool{}
	r.opts.Engine.Tools.Register(cap)
	return cap
}

func (o *ownerCaptureTool) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.owners...)
}

// splitOwner 把 "session\x00tctxOwner\x00ctxOwner" 拆回三个字段。
func splitOwner(t *testing.T, rec string) (session, tctxOwner, ctxOwner string) {
	t.Helper()
	parts := strings.Split(rec, "\x00")
	if len(parts) != 3 {
		t.Fatalf("记录格式错误 (期望 3 段, 实得 %d): %q", len(parts), rec)
	}
	return parts[0], parts[1], parts[2]
}

// TestRunTurn_OwnerReachesToolContext 是 R-H6 的接线断言。
//
// 失效形态（本测试正是为了抓它）：runner 只在 UserRecord / TurnResult / trace 里
// 带上 OwnerID，而 ctx 里什么都没有。那时工具看到 "local"，所有 owner 隔离的
// 数据都落进同一个命名空间，而没有任何错误。
func TestRunTurn_OwnerReachesToolContext(t *testing.T) {
	up := &runnerUpstream{toolTurns: 1}
	r := newTestRunner(t, up, 4, &recordingSink{})
	cap := swapTool(r)

	req := textTurn("s1", "t1", "查一下我的记忆")
	req.OwnerID = "moqi-user-alice"
	res, err := r.RunTurn(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	if res.OwnerID != "moqi-user-alice" {
		t.Fatalf("TurnResult.OwnerID 丢了: %q", res.OwnerID)
	}

	owners := cap.snapshot()
	if len(owners) == 0 {
		t.Fatal("工具从未被调用 —— 上游返回了 tool_use, 说明装配断了")
	}
	for i, rec := range owners {
		session, tctxOwner, ctxOwner := splitOwner(t, rec)
		if tctxOwner != "moqi-user-alice" || ctxOwner != "moqi-user-alice" {
			t.Fatalf("第 %d 次调用: tctx.OwnerID=%q, ctx owner=%q, 期望都是 %q —— runner 没把 OwnerID 写进 ctx",
				i, tctxOwner, ctxOwner, "moqi-user-alice")
		}
		if session != "s1" {
			t.Fatalf("第 %d 次调用: tctx.SessionID=%q, 期望 %q —— runner 没把 SessionID 写进 ctx",
				i, session, "s1")
		}
	}
}

// TestRunTurn_EmptyOwnerNormalizesToDefault 是负对照。
//
// 若实现把"没填 owner"写成空串，工具侧看到的会是 ""，而 types.OwnerFromContext
// 的缺失分支给的是 DefaultOwnerID —— 两者在闸门里都归一成同一个值，所以这条
// 测试的价值不在闸门，而在**工具自己**：工具若直接读 tctx.OwnerID 去拼存储键，
// 空串会拼出 ":" 开头的一类键，与 "local" 的那批数据永久分裂。
func TestRunTurn_EmptyOwnerNormalizesToDefault(t *testing.T) {
	up := &runnerUpstream{toolTurns: 1}
	r := newTestRunner(t, up, 4, nil)
	cap := swapTool(r)

	if _, err := r.RunTurn(context.Background(), textTurn("s1", "t1", "hi"), nil); err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	owners := cap.snapshot()
	if len(owners) == 0 {
		t.Fatal("工具从未被调用")
	}
	_, tctxOwner, ctxOwner := splitOwner(t, owners[0])
	if tctxOwner != DefaultOwnerID || ctxOwner != DefaultOwnerID {
		t.Fatalf("空 owner 应归一为 %q, 工具看到 tctx=%q ctx=%q", DefaultOwnerID, tctxOwner, ctxOwner)
	}
}

// TestRunTurn_EachOwnerSeesOwnIdentity 钉住"并发 run 不串号"。
//
// runner 是共享的：同一个 *Runner 同时服务多个会话。若 owner 写在 Runner 字段
// 上（而不是 ctx），后一次调用会覆盖前一次的 owner，工具看到的是"最后设置的那个"
// —— 而且两个 run 都不会报错，只是 A 的数据进了 B 的会话。
//
// 判据按 session 分组而不是按调用序号：每个 session 的**每一次**工具调用都必须
// 看到该 session 自己的 owner。这样即使实现改了每轮调用次数（toolTurns 拉满，
// 每轮都返 tool_use），断言也不会因为数量变化而失效或被削弱。
func TestRunTurn_EachOwnerSeesOwnIdentity(t *testing.T) {
	// toolTurns 拉满 + maxTurns=2：每个 run 恰好跑满两轮、每轮一次工具调用。
	// 这不是为了多调几个工具，而是为了让"第二次 run 覆盖第一次的状态"这类缺陷
	// 有第二个观测点。
	up := &runnerUpstream{toolTurns: 1000}
	r := newTestRunner(t, up, 2, nil)
	cap := swapTool(r)

	want := map[string]string{"s-alice": "alice", "s-bob": "bob"}
	for session, owner := range want {
		req := textTurn(session, "t-"+owner, "hi")
		req.OwnerID = owner
		// 触顶 maxTurns 会让 RunTurn 返回 error —— 那是预期内的终态，不是接线失败。
		// 断言只看"工具看到了什么", 不看这一轮有没有优雅收尾。
		_, _ = r.RunTurn(context.Background(), req, nil)
	}

	records := cap.snapshot()
	if len(records) < 4 {
		t.Fatalf("两个 run × 两轮应各调一次工具 (>=4), 实得 %d: %q", len(records), records)
	}
	for _, rec := range records {
		session, tctxOwner, ctxOwner := splitOwner(t, rec)
		wantOwner, ok := want[session]
		if !ok {
			t.Fatalf("工具看到的 session=%q 不是本次两个 run 之一: %q", session, rec)
		}
		if tctxOwner != wantOwner || ctxOwner != wantOwner {
			t.Fatalf("owner 串号: session=%q 看到 tctx=%q ctx=%q, 期望 %q (全部记录: %q)",
				session, tctxOwner, ctxOwner, wantOwner, records)
		}
	}
}
