package main

// run_events.go: --event-format ndjson 的运行事件协议 (deck 调度员化 §8 P2
// 「真实过程与心跳」, 实现契约 /tmp/deck-dispatch-contract.md §NDJSON)。
//
// 开启后 stdout 只出 NDJSON 事件行 —— 一行一个 JSON, 形如:
//
//	{"run_id":"...","seq":1,"ts":"RFC3339","type":"started","data":{"label":"..."}}
//	{"run_id":"...","seq":2,"ts":"...","type":"session","data":{"session_id":"..."}}
//	{"run_id":"...","seq":3,"ts":"...","type":"heartbeat","data":{"elapsed_sec":30}}
//	{"run_id":"...","seq":4,"ts":"...","type":"progress","data":{"text":"...","phase":"tool"}}
//	{"run_id":"...","seq":5,"ts":"...","type":"result","data":{"final":"...","is_error":false,...}}
//
// 五类事件的语义契约 (双方都按此实现, 不许自改协议):
//   - seq 在单次 run 内从 1 严格递增; result 必须且只出现一次, 是最后一个事件
//     (第二次 emitResult 直接 panic —— 宁可崩, 不可双发: 双发会让消费方把终态
//     记两遍, 比进程崩掉更糟)。
//   - heartbeat 由监督 goroutine 每 20s 一发 (env CLAUDE_GO_EVENT_HEARTBEAT_SEC
//     可覆盖), 与流上有没有 token 无关。
//   - progress 只携带工具名 + ≤200 字符截断摘要, 绝不携带完整工具参数
//     (参数里可能有密钥)。P1 起 data 按 phase 扩展 (全部可选, 老消费方不受影响):
//     phase:"tool"  —— 工具调用: tool / tool_use_id / args_hash (归一化入参
//     sha256-8, 等值判据不带原文) / result_bytes (结果完整字节数, 整数);
//     phase:"hook"  —— 内置 hook 执行: hook / hook_phase / result / duration_ms
//     (/ error / panic / timed_out), 只报"做了事"的 hook (见 hookTraceBridge);
//     phase:"thinking" —— 思考块摘要: text ≤200 字符, 纪律同工具摘要。
//   - result 的 data 在 P1 起可带 usage 段 (进程内 run 级用量聚合:
//     calls/usage_reported/input/output/cache_read/cache_creation),
//     与 llm.jsonl 的进程外账本互为对账; 没有数据时整段省略。
//   - 未开启 ndjson 时本文件的协议部分完全不生效; text/json 模式行为逐字节不变
//     (唯二被 ndjson 模式改道的普通信息行走 runInfof/runInfoln)。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/engine"
)

// runEventDefaultHeartbeatSec 是心跳默认间隔 (契约常数 20s)。
const runEventDefaultHeartbeatSec = 20

// runEventLine 是一行 NDJSON 事件的线格式, 字段名与契约逐字对应。
type runEventLine struct {
	RunID string         `json:"run_id"`
	Seq   int            `json:"seq"`
	TS    string         `json:"ts"`
	Type  string         `json:"type"`
	Data  map[string]any `json:"data"`
}

// runEventEmitter 是单次 run 的事件发射器: 一个 emitter 对应一个 run_id,
// seq 从 1 起严格递增。内部一把锁 —— emit 可从流式消费循环里同步调用,
// 调用方无需再加锁。
type runEventEmitter struct {
	mu    sync.Mutex
	out   io.Writer
	runID string
	seq   int
	start time.Time

	hbInterval time.Duration
	hbStop     chan struct{}
	hbDone     chan struct{}
	stopOnce   sync.Once

	resultSent bool
}

// newRunEventEmitter 建发射器并立即发 started 事件 (seq 恒为 1, label 取
// --run-label 值, 可为空串)。心跳由调用方在发完 session 事件后调
// startHeartbeat 起 —— 契约要求 started/session 排在任何 heartbeat 之前。
func newRunEventEmitter(out io.Writer, runID, label string) *runEventEmitter {
	if out == nil {
		out = os.Stdout
	}
	e := &runEventEmitter{
		out:        out,
		runID:      runID,
		start:      time.Now(),
		hbInterval: runEventHeartbeatInterval(),
	}
	e.emit("started", map[string]any{"label": label})
	return e
}

// runEventHeartbeatInterval 读 env CLAUDE_GO_EVENT_HEARTBEAT_SEC 覆盖,
// 缺省或非法值回落契约常数 20s。
func runEventHeartbeatInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_EVENT_HEARTBEAT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return runEventDefaultHeartbeatSec * time.Second
}

// writeLocked 在持有 e.mu 的前提下写一行事件。
func (e *runEventEmitter) writeLocked(typ string, data map[string]any) {
	e.seq++
	line := runEventLine{
		RunID: e.runID,
		Seq:   e.seq,
		TS:    time.Now().Format(time.RFC3339),
		Type:  typ,
		Data:  data,
	}
	b, err := json.Marshal(line)
	if err != nil {
		// data 只由本文件构造 (string/bool/int/map), marshal 失败属于编程错误。
		panic("run event: marshal failed: " + err.Error())
	}
	e.out.Write(append(b, '\n'))
}

// emit 发一条非 result 事件。必须轻: 持锁 + 一次 Marshal + 一次写,
// 在流式消费循环内同步调用不会阻塞消费。
func (e *runEventEmitter) emit(typ string, data map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.writeLocked(typ, data)
}

// emitResult 发 result 事件 —— 一次 run 只许一次, 且必须是最后一个事件。
// 第二次调用直接 panic (宁可崩, 不可双发)。
func (e *runEventEmitter) emitResult(data map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resultSent {
		panic("run event: result emitted twice (run_id=" + e.runID + ")")
	}
	e.resultSent = true
	e.writeLocked("result", data)
}

// resultWasSent 报告 result 是否已发 (RunE 收尾守卫判重入用)。
func (e *runEventEmitter) resultWasSent() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resultSent
}

// startHeartbeat 起心跳监督 goroutine, 每 hbInterval 发一条
// {"elapsed_sec": <自 started 起的整数秒>}。与流上有没有 token 无关。
func (e *runEventEmitter) startHeartbeat() {
	e.hbStop = make(chan struct{})
	e.hbDone = make(chan struct{})
	go func() {
		defer close(e.hbDone)
		t := time.NewTicker(e.hbInterval)
		defer t.Stop()
		for {
			select {
			case <-e.hbStop:
				return
			case <-t.C:
				e.emit("heartbeat", map[string]any{
					"elapsed_sec": int64(time.Since(e.start).Seconds()),
				})
			}
		}
	}()
}

// stopHeartbeat 停心跳并等 goroutine 完全退出, 保证此后绝不再有 heartbeat
// 事件 —— 调用方随后发 result, result 才是真正的最后一个事件。可重复调用。
func (e *runEventEmitter) stopHeartbeat() {
	e.stopOnce.Do(func() {
		if e.hbStop == nil {
			return
		}
		close(e.hbStop)
		<-e.hbDone
	})
}

// runResultEventData 构造 result 事件的 data。final/errKind 与
// --output-format json 的信封完全同源 (同一个变量、同一个 sanitize、
// is_error = errKind != "")。usage/cost_cny/artifacts 在 run 路径上没有现成
// 真实数据源, 按契约「有真实数据才给, 没有就省略」整段缺席, 而不是填空值冒充。
func runResultEventData(final, sessionID, errKind string) map[string]any {
	data := map[string]any{
		"final":    final,
		"is_error": errKind != "",
	}
	if errKind != "" {
		data["error_kind"] = errKind
	}
	if sessionID != "" {
		data["session_id"] = sessionID
	}
	return data
}

// runProgressText 拼 progress 事件的 text: 工具名 + ≤200 字符截断摘要。
// 只给截断摘要, 绝不携带完整工具参数/结果 —— 那里面可能有密钥。
func runProgressText(name, summary string) string {
	name = strings.TrimSpace(name)
	summary = strings.TrimSpace(summary)
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}
	if summary == "" {
		return name
	}
	if name == "" {
		return summary
	}
	return name + " " + summary
}

// attachRunUsage 把 run 级 LLM 用量聚合 (P1) 并进 result 事件的 data。
// eng 为 nil 或一次调用都没记到 (RunUsageSnapshot ok=false) 时原样返回 ——
// 按契约「有真实数据才给, 没有就省略」, 不发全零段冒充"没花钱"。
//
// 这是进程内账本, 与 llm.jsonl 的进程外账本互为冗余校验 (P1 验收: 对账
// 差异 >5% 告警)。两个账本刻意独立采集, 对账才有意义。
func attachRunUsage(data map[string]any, eng *engine.QueryEngine) map[string]any {
	u, ok := eng.RunUsageSnapshot()
	if !ok {
		return data
	}
	data["usage"] = map[string]any{
		"calls":                       u.Calls,
		"usage_reported":              u.UsageReported,
		"input_tokens":                u.InputTokens,
		"output_tokens":               u.OutputTokens,
		"cache_read_input_tokens":     u.CacheReadInputTokens,
		"cache_creation_input_tokens": u.CacheCreationInputTokens,
	}
	return data
}

// hookTraceBridge 把引擎内置 hook 链的执行 trace (engine.HookTraceObserver)
// 桥接成 ndjson 的 progress(phase:"hook") 事件 (P1: hook 可见性)。
//
// # 过滤纪律: 只报"做了事"的 hook
//
// PreRequest 段的 hook 每轮都跑 (MessageFilter/PromptCache/Metrics...), 全量上报
// 会把事件流淹成噪音。只放行三类: ① Result 非 no_op (注入了消息/改了 prompt/
// 终止了 run —— 记忆注入、循环检测、压缩都落在这类); ② 出错/panic/超时;
// ③ (Skipped 一律不报 —— 被跳过是前序 hook 的决定, 前序那条自己会报。)
//
// # 泄漏面
//
// HookTrace 本身只含类别标签不含用户正文 (见 hookpolicy.go 的 hookResultSummary
// 注释); Err/Panic 文本截断到 120 字符 —— 它们是运维错误信息, 不是用户数据,
// 但截断防的是 hook 把输入片段拼进错误串的意外。
type hookTraceBridge struct {
	ev *runEventEmitter
}

// newHookTraceBridge 返回可挂进 ctx 的观测者; ev 为 nil 时返回 nil
// (WithHookTraceObserver 对 nil 观测者原样返回 ctx)。
func newHookTraceBridge(ev *runEventEmitter) engine.HookTraceObserver {
	if ev == nil {
		return nil
	}
	return &hookTraceBridge{ev: ev}
}

// ObserveHookTrace 实现 engine.HookTraceObserver。必须轻且绝不 panic
// (调用方虽会 recover, 但 panic 会丢掉这条 trace)。
func (b *hookTraceBridge) ObserveHookTrace(_ context.Context, tr engine.HookTrace) {
	if tr.Skipped {
		return
	}
	notable := tr.Result != "" && tr.Result != "no_op"
	if !notable && tr.Err == "" && tr.Panic == "" && !tr.TimedOut {
		return
	}
	data := map[string]any{
		"phase":       "hook",
		"hook":        tr.Name,
		"hook_phase":  tr.Phase.String(),
		"duration_ms": tr.Duration.Milliseconds(),
	}
	if tr.Result != "" {
		data["result"] = tr.Result
	}
	if tr.Err != "" {
		data["error"] = truncateRunEventText(tr.Err, 120)
	}
	if tr.Panic != "" {
		data["panic"] = truncateRunEventText(tr.Panic, 120)
	}
	if tr.TimedOut {
		data["timed_out"] = true
	}
	b.ev.emit("progress", data)
}

// truncateRunEventText 按字符截断 (不是字节 —— 错误串里中文常见, 字节截断
// 会劈开多字节字符, 消费方拿到非法 UTF-8)。
func truncateRunEventText(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}

// runInfof/runInfoln 把 run 命令路径上的普通信息行写到 stdout;
// ndjson 模式下改走 stderr —— 契约规定那时 stdout 只许出 NDJSON 事件行。
// text/json 模式 (含未开 ndjson 的默认形态) 输出目标与格式逐字节不变。
func runInfof(format string, a ...any) {
	if flagEventFormat == "ndjson" {
		fmt.Fprintf(os.Stderr, format, a...)
		return
	}
	fmt.Printf(format, a...)
}

func runInfoln(a ...any) {
	if flagEventFormat == "ndjson" {
		fmt.Fprintln(os.Stderr, a...)
		return
	}
	fmt.Println(a...)
}
