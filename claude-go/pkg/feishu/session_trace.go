package feishu

import (
	"context"
	"fmt"
	"time"

	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// session_trace.go — 让**会话路径**的运行也落进 `trace-run-*` 桶。
//
// # 修的是什么
//
// RunID 此前只在团队 / 图 / 工作流 / worker 路径注入 (teams.go:880、
// graph/engine.go:269、worker/worker.go:346)。会话路径 (飞书会话、`claude-go run`)
// 的 ctx 里 RunID 为空, 而 tracestore 的桶名是从 span.TraceID 派生的:
//
//	tracestore.go:188  bucketName("") -> "trace-orphan"
//	tracestore.go:58   TraceID 字段 = RunID (episode); 会话路径为 session id
//
// 于是会话的 span **一条没丢**, 只是全进了 trace-orphan 桶 —— 而 webapp 的轨迹页
// 只列 `trace-run-*` (webapp-service/backend/src/lib/traces.ts:401), 一条都看不见。
// 这不是采集失败, 是桶名对不上。
//
// # 为什么 RunID 里带一个序号, 而不是直接用 session id
//
// **一次运行一个文件**, 与团队路径同形 (团队的 RunID 是 `run-<team>-<logging.TraceID>`,
// 同样一次 run 一个文件)。两条理由:
//
//  1. pkg/trace/trace.go:34 写明 RunID "必须每次运行唯一, 否则奖励归因会把两次运行
//     的账记到一起"。桶名从 RunID 派生, 所以"每 session 一个文件"等于要求 RunID 跨次
//     稳定 —— 那与奖励归因直接冲突, 不能兼得。
//  2. SessionID 是另一个字段, 它承担"跨 run 稳定"(出站提示缓存分片, pkg/api/trace.go:158)。
//     两个键在这里是分开的两个东西, 我们不合并它们。
//
// 结果: 桶名 = `trace-run-<sessionID>-<seq>`, 会话靠前缀可辨, 页面上一行 = 一次运行。
//
// # 判据
//
//	ls <state>/statestore/log/trace-run-*   出现文件名前缀是会话 id 的新文件
//	trace-orphan.py --since 10m             不再命中这批运行 (它们已经不在 orphan 桶了)
//
// run span 的终态与 workflow 标签已上移到 pkg/evolution/tracestore —— 那里是
// 三个产生方 (团队 / 飞书会话 / CLI) 共用的契约面, 各写各的字面量会让"什么算
// completed"出现多份真相源。这里保留别名只为不动下面的引用点。
const (
	runSpanStatusOK      = tracestore.RunStatusOK
	runSpanStatusFailed  = tracestore.RunStatusFailed
	sessionWorkflowLabel = tracestore.SessionWorkflowLabel
)

// nextRunSeq 发号器。进程内全局递增即可 —— 要的只是"同一次会话的相邻两次运行不撞名",
// 全局递增天然满足, 且不必为每个会话维护一张计数器表 (会话是懒创建、会被淘汰的)。
func (sm *SessionManager) nextRunSeq() uint64 {
	return sm.runSeq.Add(1)
}

// beginSessionRun 给一次会话运行开 trace 作用域, 并返回一个**必须被调用的**收尾函数。
//
// 收尾函数写成闭包而不是让调用方自己拼 Span, 是为了让"写 run Span"这件事只有一个
// 出口: 调用方只能选择传什么 status, 不能选择"忘了写"。参照 pkg/agent/run_span.go 的
// 同一理由 —— 它挂在 defer 上, 因为那是唯一"无论从哪条路径退出都会走一遍"的位置。
//
// TraceStore 未装配时返回原 ctx 与一个空操作 —— 与 hook_trace.go:28 "store 为 nil 时
// hook 不注册"的守卫同向: 没接轨迹就不假装接了。
func (sm *SessionManager) beginSessionRun(ctx context.Context, session *Session) (context.Context, func(status string, errText string)) {
	if sm == nil || session == nil {
		// 会话为 nil 时不能返回空操作: 调用方会无条件下一次 deferred 调用,
		// 而它同时还指望这次调用释放占位 (见 session_host.go 的 Reserve)。
		if session != nil {
			defer session.releaseReserved()
		}
		return ctx, func(string, string) {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// 占位已分配的 runID 优先 —— 它必须与 HTTP 面回给调用方的轮询键**逐字节相同**,
	// 否则拿到 runId 去 ReadRun 会读空, 而"读不到"与"没采集"在读数上一模一样。
	// 没有占位 (飞书路径、cron 路径) 才现编一个。
	runID := session.reservedRunIDFor()
	if runID == "" {
		runID = fmt.Sprintf("run-%s-%d", session.ChatID, sm.nextRunSeq())
	}

	// TraceStore 未装配: ctx 照常注入 (占位还得释放), 只是不写 run Span。
	// 与 hook_trace.go:28 "store 为 nil 时 hook 不注册" 同向: 没接轨迹就不假装接了。
	if sm.traceStore == nil {
		ctx = trace.With(ctx, trace.IDs{RunID: runID, SessionID: session.ChatID})
		return ctx, func(string, string) { session.releaseReserved() }
	}
	// SessionID 一并注入: 它与 RunID 是两个不同的键 (见文件头), 出站请求头按
	// SessionID 分片, 同一会话的多轮因此共享同一份前缀缓存。
	ctx = trace.With(ctx, trace.IDs{RunID: runID, SessionID: session.ChatID})

	start := time.Now()
	return ctx, func(status string, errText string) {
		// 占位必须在**所有**退出路径上释放, 所以它排在这个闭包的第一行 ——
		// 下面任何一处提前 return 都不会漏掉它。漏掉的后果是这个会话永久 409。
		defer session.releaseReserved()

		attrs := map[string]any{
			// team 取会话的显示名 —— 页面按它分组, 每个会话一个桶, 这正是
			// "会话级轨迹"要的粒度。DisplayName 未设时回落成会话 id。
			"team":     session.displayName(),
			"workflow": sessionWorkflowLabel,
			"status":   status,
			"session":  session.ChatID,
		}
		// Engine 可能为 nil (会话已淘汰/尚未构造完), 此时不写 model 而不是写空串:
		// 空串与"模型就是空"在读数上不可分。
		if session.Engine != nil {
			attrs["model"] = session.Engine.Config.Model
		}
		if errText != "" {
			attrs["error"] = truncateResult(errText, 500)
		}
		sm.traceStore.Write(tracestore.Span{
			TraceID: runID,
			SpanID:  internal_hook.GenerateUUID(),
			Kind:    tracestore.KindRun,
			Name:    session.ChatID,
			// 刻意不设 ParentID: run 是这棵树的根 (与 run_span.go:88 同一处理)。
			InputRef:  sm.traceStore.MakeRef(""),
			OutputRef: sm.traceStore.MakeRef(errText),
			Attrs:     attrs,
			TS:        start.UnixMilli(),
			DurMS:     time.Since(start).Milliseconds(),
		})
	}
}

// errTextOf 把 error 转成写进 Span 的文本。nil 返回空串 —— 调用方据此决定
// "不写 error 字段"而不是"写一个空字符串" (两者在读 span 时不可分)。
func errTextOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// displayName 会话的展示名。spec 未设 Name 时回落成 ChatID —— 回落值是**有意义**的
// (它就是会话的身份), 不是占位符; 因此不印 "(未命名)" 之类会掩盖真实取值的东西。
func (s *Session) displayName() string {
	if s == nil {
		return ""
	}
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.ChatID
}
