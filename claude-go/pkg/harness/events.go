// events.go — 一次 turn 内的流式事件发射器（R-H12）。
//
// # 为什么需要它
//
// 契约层（types.go）定义了 TurnEvent 与 Seq 语义，但"谁分配 Seq、谁保证终态只有
// 一条、断线重连 after= 怎么续放、缓冲满了怎么办"这四件事没有载体。业务宿主要
// 么各自手写一遍（并各自踩坑），要么把 sessionapi 的 run 级缓冲硬搬过来 —— 后者
// 是另一层协议，harness 不该依赖它。本文件提供**进程内**的最小发射器：
//
//	em := harness.NewEmitter(harness.EmitterOptions{Sink: hostFanout})
//	em.BeginTurn(turnID)
//	_ = em.Emit(harness.TurnEvent{Kind: harness.EventDelta, Delta: "你"})
//	_ = em.Emit(harness.TurnEvent{Kind: harness.EventTerminal, Terminal: &t})
//	// 客户端断线重连：
//	res := em.Replay(lastSeq) // res.Truncated 为真表示中间有事件已被淘汰
//
// 与 sessionapi.eventLog 的关系：不是替代。那边是 HTTP/SSE 出口的 4000 条大缓冲，
// 按 run 淘汰；这边是 harness 每轮一个的环形缓冲，供宿主在 turn 内做扇出与续放。
// 两层的 Seq 各自独立，不要互相赋值。
package harness

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// defaultEmitterBufferSize 是 EmitterOptions.BufferSize 为 0（或负数）时的容量。
//
// 1024 的依据：一轮里 delta 通常几千个 token，但宿主可以在入缓冲前合并相邻 delta
// （AssembleText 对分段不敏感，见下），所以 1024 条够覆盖"delta 与工具事件交替"
// 的正常序列；真的溢出也不丢 Seq 连续性，只是最早的事件不可回放（Truncated=true）。
const defaultEmitterBufferSize = 1024

var (
	// ErrTerminalAlreadyEmitted 表示同一 turn 内第二次发射终态事件。
	//
	// 为什么必须拒绝而不是覆盖：下游 SSE 消费方见到终态就关流或渲染结束态。重复
	// 的终态会关两次（第二次是空操作或报错）或渲染出两个结束块；更糟的是如果两条
	// 终态内容不同（一次 completed、一次 error），前端看到的是自相矛盾的收尾。
	ErrTerminalAlreadyEmitted = errors.New("harness: terminal event already emitted for this turn")

	// ErrTurnMismatch 表示事件自带的 TurnID 与本轮 BeginTurn 声明的 TurnID 不一致。
	//
	// 允许这种事件进缓冲会污染回放作用域：Replay 只按 Seq 过滤，混入另一轮的事件后
	// 消费方拿到的"这一轮的历史"就是假的。空 TurnID 不算不一致 —— 那是"请帮我填"。
	ErrTurnMismatch = errors.New("harness: turn id mismatch")
)

// EmitterOptions 是 Emitter 的构造参数。
type EmitterOptions struct {
	// BufferSize 是环形缓冲容量（事件条数）；0 或负数取 1024。
	BufferSize int
	// Sink 是可选的宿主扇出回调。它在 Emitter 的锁**之外**被调用，但仍是同步调用，
	// 所以 Sink 自己必须非阻塞（典型做法：投递到宿主自己的有界 channel，满了丢弃或
	// 断开慢消费者，而不是在这里等待）。并发 Emit 时回调顺序不保证与 Seq 一致，
	// Sink 需要以 TurnEvent.Seq 为准排序。
	Sink func(TurnEvent)
	// Clock 可选，测试用。TurnEvent 是跨进程协议（types.go 里没有时间戳字段，也不能
	// 加），所以时间**不写进事件**，只通过 LastEmitAt 暴露给宿主做保活/空闲回收判定。
	Clock func() time.Time
}

// ReplayResult 是 Replay 的返回。
//
// 选择"一个结构体同时给数据和缺口语义"，而不是另开 Gap(after) bool：两个函数是
// 两次读锁，两次读之间可能又发生 Emit 与淘汰，Gap 的答案在 Replay 执行时已经过期。
// 一次加锁把"给你这些事件"与"这些事件是否完整"绑在同一个快照上，调用方不会看到
// 自相矛盾的一对答案。
type ReplayResult struct {
	// Events 是 Seq > afterSeq 且仍在缓冲里的事件，按 Seq 升序。
	//
	// 事件里的指针字段（Citation/Tool/Usage/Terminal）与发射方共享，调用方应视为
	// 只读；切片本身是新分配的，追加/截断不会影响 Emitter。
	Events []TurnEvent
	// Truncated 为真表示 afterSeq 之后有事件已经因环形缓冲淘汰而不可回放。
	// 调用方**必须**据此让客户端重新拉全量（或提示"历史不完整"）—— 静默返回短列表
	// 会让客户端以为自己掌握了完整历史，这比直接报错更难排查。
	Truncated bool
	// TurnID 是当前缓冲所属的 turn。跨 turn 的续放必须同时比对 (TurnID, Seq)：
	// BeginTurn 会重置 Seq，旧 turn 的 seq=7 与新 turn 的 seq=7 不是同一条事件。
	TurnID string
}

// Emitter 是单 turn 的流式事件发射器，并发安全。
type Emitter struct {
	mu sync.Mutex

	buf   []TurnEvent // 环形缓冲，长度即容量
	start int         // 最旧事件的下标
	count int         // 当前条数
	// dropped 记录因溢出被淘汰的条数，仅用于判断 Truncated。Seq 本身不因淘汰回绕
	// 或重置 —— 一旦 Seq 重排，after= 续放的锚点就失去意义。
	dropped int64

	turnID   string
	nextSeq  int64
	terminal bool // 本轮终态是否已发射
	sink     func(TurnEvent)
	clock    func() time.Time
	lastEmit time.Time
}

// NewEmitter 构造发射器。未 BeginTurn 时也可以 Emit（Seq 从 1 开始，TurnID 为空），
// 但宿主应当成对使用 BeginTurn/Emit，否则事件无法按轮归属。
func NewEmitter(opts EmitterOptions) *Emitter {
	size := opts.BufferSize
	if size <= 0 {
		size = defaultEmitterBufferSize
	}
	return &Emitter{
		buf:   make([]TurnEvent, size),
		sink:  opts.Sink,
		clock: opts.Clock,
	}
}

// BeginTurn 开始新一轮：清空缓冲、Seq 归 1、解除终态封锁、记录 turnID。
//
// 缓冲必须一起清空而不是保留跨轮事件：Seq 每轮都从 1 开始，若两轮事件混在一个
// 缓冲里，Replay 的输出会出现重复 Seq，单调唯一的前提当场失效。跨轮续放因此
// 只能靠 (TurnID, Seq) 组合，见 ReplayResult.TurnID。
func (e *Emitter) BeginTurn(turnID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turnID = turnID
	e.nextSeq = 0
	e.start = 0
	e.count = 0
	e.dropped = 0
	e.terminal = false
	clear(e.buf) // 释放上一轮事件里的指针引用，长会话下避免无谓的常驻内存
}

// Emit 追加一条事件并同步扇出到 Sink。
//
// 调用方**不要**自己填 Seq（填了也会被覆盖）：Seq 的唯一分配点是这里，手填等于
// 绕过单调唯一保证。TurnID 留空则填本轮 ID；填了但与本轮不一致返回 ErrTurnMismatch。
// 终态事件每轮只接受第一条，第二条返回 ErrTerminalAlreadyEmitted 且不进缓冲、
// 不进 Sink。
//
// 锁只保护缓冲与计数；Sink 在解锁后调用，因此 Sink 里回调 Replay 不会自锁。
func (e *Emitter) Emit(ev TurnEvent) error {
	now := e.now()

	e.mu.Lock()
	if ev.Kind == EventTerminal && e.terminal {
		e.mu.Unlock()
		return ErrTerminalAlreadyEmitted
	}
	if ev.TurnID != "" && ev.TurnID != e.turnID {
		e.mu.Unlock()
		return fmt.Errorf("%w: event=%q emitter=%q", ErrTurnMismatch, ev.TurnID, e.turnID)
	}

	e.nextSeq++
	ev.Seq = e.nextSeq
	if ev.TurnID == "" {
		ev.TurnID = e.turnID
	}
	if ev.Kind == EventTerminal {
		e.terminal = true
	}
	e.appendLocked(ev)
	e.lastEmit = now
	sink := e.sink
	e.mu.Unlock()

	if sink != nil {
		sink(ev)
	}
	return nil
}

// appendLocked 把事件写入环形缓冲，满则淘汰最旧的一条。
//
// 关键不变量：淘汰只影响"能否回放"，不影响 Seq 的连续性 —— nextSeq 已经先把
// 序号发出去，这里只负责保存。
func (e *Emitter) appendLocked(ev TurnEvent) {
	if e.count < len(e.buf) {
		e.buf[(e.start+e.count)%len(e.buf)] = ev
		e.count++
		return
	}
	e.buf[e.start] = ev
	e.start = (e.start + 1) % len(e.buf)
	e.dropped++
}

// Replay 返回 Seq > afterSeq 的事件快照，供 after= 续放。
//
// afterSeq 小于等于 0 视为"从头要"。若 afterSeq 之后有事件已被淘汰，Truncated
// 为真（定义见 ReplayResult）。afterSeq 超过当前最大 Seq 时返回空且不报截断：
// 此时"Seq > afterSeq 的事件"本来就不存在，不存在缺口语义 —— 常见于宿主带上了
// 上一轮的 Seq，必须靠 TurnID 比对识别，而不是靠 Truncated。
func (e *Emitter) Replay(afterSeq int64) ReplayResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	if afterSeq < 0 {
		afterSeq = 0
	}
	res := ReplayResult{TurnID: e.turnID}
	if e.count == 0 {
		return res
	}
	oldest := e.buf[e.start].Seq
	// 有淘汰发生，且 afterSeq 落在淘汰区之内（含恰好在最旧保留事件之前）才算截断。
	// 注意 afterSeq == oldest-1 时"下一条就是 oldest"，一件不缺，所以用严格小于。
	if e.dropped > 0 && afterSeq < oldest-1 {
		res.Truncated = true
	}
	if afterSeq >= e.nextSeq {
		return res
	}
	res.Events = make([]TurnEvent, 0, e.count)
	for i := 0; i < e.count; i++ {
		ev := e.buf[(e.start+i)%len(e.buf)]
		if ev.Seq > afterSeq {
			res.Events = append(res.Events, ev)
		}
	}
	return res
}

// LastEmitAt 返回最近一次被接受的事件的时间（Clock 决定）。没有事件时为零值。
//
// 宿主用它区分"流还活着但模型在思考"与"这个 turn 已经死掉了"—— 事件协议里没有
// 时间戳字段（TurnEvent 是跨进程冻结契约），所以活性信号只能走带外接口。
func (e *Emitter) LastEmitAt() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastEmit
}

func (e *Emitter) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
}

// AssembleText 把事件序列里的文本增量按给定顺序拼成全文。
//
// 为什么单独提供：线上的 delta 可以被合并（相邻 delta 拼成一条、或按 token 再分片），
// 消费方与宿主必须能证明"合并是幂等的"。拼接对分段不敏感，所以对未合并序列与
// 合并后序列调用本函数结果相同（测试 TestEmitterAssembleTextLossless 钉住这一点）。
//
// 只累加 Kind==EventDelta 的 Delta 字段；工具/引用/终态/错误事件不贡献文本。
// 不按 BlockIndex 分组：多文本块时结果是各块按序拼接，单块场景下等于最终文本。
func AssembleText(events []TurnEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind != EventDelta {
			continue
		}
		b.WriteString(ev.Delta)
	}
	return b.String()
}
