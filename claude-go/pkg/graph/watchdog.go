package graph

// watchdog.go —— **图级**停滞检测 (design/01 §4.3 双层 watchdog 里缺的那一层)。
//
// ---------------------------------------------------------------------------
// 设计原文与现状
// ---------------------------------------------------------------------------
//
// §4.3: "watchdog: **图级**(进展检测: Journal 尾部 N 分钟无事件即停滞) +
// 节点级 (activity 心跳) 两层, 参数沿用 coordinator.go:155-161; 停滞动作 =
// 发 hook 事件 + 按策略 retry/fail/notify"。
//
// 节点级那层早就有 (`teamGraphHooks.touch` → `activityCallback`/`progressCallback`
// → `Coordinator.TouchActivity`/`ReportProgress`, 由 `Coordinator.teamWatchdog`
// 的 L1/L2 消费)。**图级那层缺**: 图引擎自己不知道"我这一整张图已经 N 分钟没有
// 任何事件了"。
//
// 两层不是冗余。节点级心跳的盲区是**没有节点在跑的时候**: 一次 Emit 都不发、
// 一条 journal 都不写, 而 activity 时间戳停在最后一次 touch —— 比如
//   - 全部就绪节点都卡在 `acquireNested` 等一张永远不还的并发票;
//   - 唯一在跑的节点在 runner 内部无限期阻塞 (网络挂死, 没有 deadline);
//   - AIMD 背压把并发收到 0 之后没人再放行。
// 这三种形态下"节点级"探针一切正常 (它只知道最后一次心跳的时间, 而没有新心跳
// 这件事本身没人看), 而图层能看见的唯一事实是 **journal 尾部不再增长**。
//
// ---------------------------------------------------------------------------
// ⚠️ 默认只观测, 不干预 —— 这是硬约束不是保守
// ---------------------------------------------------------------------------
//
// `Watchdog` 为 nil (缺省) 时**连 goroutine 都不起**: 零 ticker、零分配、零事件,
// 现存全部图的行为逐字节不变。声明了才生效。
//
// 声明后的缺省动作也是 `notify`(= 只发 hook 事件 + 记 journal, 不碰执行)。
// **一个会自动把团队判失败的新 watchdog 是生产事故**: 本仓 8+ 下游平台在用
// :18080, 而"多久没有事件算停滞"这件事在长阶段 (整本小说起草、大仓索引) 上极易
// 误判 —— 一个 40 分钟不产出中间事件的合法节点会被一个"看起来很合理"的 10 分钟
// 阈值杀掉。要干预必须显式写 `action: "fail"`。
//
// ---------------------------------------------------------------------------
// 为什么不用更直觉的三种做法
// ---------------------------------------------------------------------------
//
//  1. **不去 `ReadAll()` 重读 journal 尾部**(设计原文的字面读法)。生产 journal 是
//     per-team append-only 文件, 一个跑过几十轮的团队 journal 有上万行, 每 60 秒
//     全量读盘 + 逐行 JSON 解码只为拿最后一条的时间戳 —— 而这个量在进程内是免费的:
//     `appendEv` 是**全引擎唯一**的 journal 写入口 (engine.go 里那个闭包), 在它里面
//     记一个时间戳即可。等价性: 只要没有别人绕过 appendEv 写 journal, 两者读到的
//     "尾部时间"就是同一个量; 而绕过 appendEv 写 journal 的路径一条都没有
//     (`InvalidateFrom` 是**运行之外**由平台调的, 那时本 watchdog 早已停了)。
//
//  2. **停滞不改变 RunStatus 的取值集合**。`action: "fail"` 走的是引擎**既有的
//     ctx 取消路径** (语义 8: 未跑节点不再调度、在跑节点等待收尾、Status=failed),
//     而不是新造一个 `stalled` 终态。新造终态要让 8+ 下游平台的 status 分支各长
//     一个 case, 漏一个就是静默丢状态 —— 而"停滞后放弃"与"被取消"对下游是同一件事
//     (都要人来看), 区别写在 journal 事件里 (`graph.stalled` 带 action)。
//
//  3. **不做 `retry`**(设计原文列了 retry/fail/notify 三档)。图级重试的语义是
//     "把卡住的节点重新派发", 而卡住的节点此刻**正在 runner 里跑**(goroutine 还
//     活着、可能还持着并发票)。要真重试就得先能安全终止它, 那需要节点级可中断
//     执行 —— 现在只有 TimeoutSec 那条 deadline 能做到, 而它已经是节点自己的事。
//     硬做的结果是同一个节点两份 goroutine 同时写同一个 NodeResult。所以这里
//     **只提供 notify|fail 两档**, 且 Validate 对未知 action 显式报错 —— 静默接受
//     一个引擎不解释的 action 等于让声明它的人以为策略生效了 (本仓修过的一类缺陷)。

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// 图级 watchdog 的缺省参数。
//
// ⚠️ 这两个数字**不是新立的**, 逐一对应 `pkg/agent/coordinator.go` 既有 watchdog:
//
//	DefaultWatchdogStallSec  = 600 ⇔ coordinator.go 的 watchdogProgressStaleThreshold
//	                                 (L2 进展检测: 10min 进展不变即疑似卡住)
//	DefaultWatchdogCheckSec  =  60 ⇔ coordinator.go 的 watchdogCheckInterval (巡检间隔)
//
// 之所以在这里也写一份而不是引用: `pkg/graph` 只依赖 `pkg/trace`, 引用 `pkg/agent`
// 是倒挂。生产接线处 (pkg/agent/graph_watchdog.go) **显式把 coordinator 的常量传进来**,
// 于是真正生效的值只有一个来源; 这里的缺省只在别人 (测试/第三方) 不传时兜底。
const (
	DefaultWatchdogStallSec = 600
	DefaultWatchdogCheckSec = 60
)

// 停滞动作 (WatchdogSpec.Action)。
const (
	// WatchdogNotify 只发 hook 事件 + 记 journal, **不碰执行**。缺省。
	WatchdogNotify = "notify"
	// WatchdogFail 放弃本次运行: 走引擎既有的 ctx 取消路径 (Status=failed)。
	// 显式声明才生效, 理由见文件头"默认只观测"。
	WatchdogFail = "fail"
)

// EvGraphStalled 图级 watchdog 判定停滞 (design/01 §4.3)。
// Data: stall_ms(实际静默时长) / threshold_ms / action / repeat(第几次判定)。
//
// **Replay 刻意不解释它**: 它记的是"当时没有进展"这件事, 不是进度本身。让 Replay
// 认识它就得决定"停滞过的节点要不要重跑", 而那与 node.invalidated 是同一件事的
// 两种表达 —— 两套失效语义并存必然漂移。
const EvGraphStalled = "graph.stalled"

// WatchdogSpec 图级停滞检测声明 (GraphPolicies.Watchdog)。
//
// **nil = 完全关闭** (连 goroutine 都不起)。零值不可用: StallSec/CheckSec 取 0 时
// 各自退到上面的 Default* —— 但 Action 为空退到 notify 而不是报错, 因为"只声明了
// 阈值"是最常见的用法, 强制写 action 只会让所有人抄同一个字符串。
type WatchdogSpec struct {
	// StallSec journal 尾部静默多久算停滞 (秒)。0 → DefaultWatchdogStallSec。
	StallSec int `json:"stall_sec,omitempty"`
	// CheckSec 巡检间隔 (秒)。0 → DefaultWatchdogCheckSec。
	CheckSec int `json:"check_sec,omitempty"`
	// Action 停滞动作: notify (缺省, 只观测) | fail (放弃本次运行)。
	Action string `json:"action,omitempty"`
	// MaxNotices 最多判定几次后停止巡检 (0 = 不限)。
	//
	// 为什么需要它: notify 档下停滞会**每个巡检周期重复判定一次** (静默时长只增),
	// 一个真卡死的图会往 journal 里写几百条 graph.stalled。设 1 表示"只告一次"。
	// 不给它一个默认非零值是刻意的: 默认关的东西不该在开启后又自己少报,
	// "报多了"可以调, "该报没报"排障时没人猜得到。
	MaxNotices int `json:"max_notices,omitempty"`

	// stallOvr/checkOvr **亚秒级测试注入点** (同 Engine.sleepFn 的角色)。
	// 未导出 ⇒ 不进 JSON ⇒ 运维声明面上根本不存在这两个旋钮。
	//
	// 为什么需要它们: 对外的单位是秒 (运维旋钮的正确粒度), 但真正会出错的地方是
	// **巡检 goroutine 与调度 goroutine 的交错** (谁先刷新时钟 / 取消到达时调度在
	// 哪一步), 而验证交错必须跑真的 goroutine。等 10 分钟不现实, 而换成"注入假时钟"
	// 会把这层交错整个抹掉 —— 那样测的是"我的假时钟前进了"。
	// 这两个字段只改**单位换算**这一处, 判定循环一字不变。
	stallOvr time.Duration
	checkOvr time.Duration
}

// stallAfter 停滞阈值。
func (w *WatchdogSpec) stallAfter() time.Duration {
	if w == nil {
		return DefaultWatchdogStallSec * time.Second
	}
	if w.stallOvr > 0 {
		return w.stallOvr
	}
	if w.StallSec <= 0 {
		return DefaultWatchdogStallSec * time.Second
	}
	return time.Duration(w.StallSec) * time.Second
}

// checkEvery 巡检间隔。
//
// 夹到 ≤ 阈值: 巡检比阈值还稀疏时, 一次 10 分钟的停滞可能要等 30 分钟才被发现,
// 表现为"阈值写了 10min 但告警总是迟到" —— 那种偏差没人会往巡检间隔上想。
func (w *WatchdogSpec) checkEvery() time.Duration {
	d := time.Duration(DefaultWatchdogCheckSec) * time.Second
	switch {
	case w == nil:
	case w.checkOvr > 0:
		d = w.checkOvr
	case w.CheckSec > 0:
		d = time.Duration(w.CheckSec) * time.Second
	}
	if stall := w.stallAfter(); d > stall {
		d = stall
	}
	return d
}

// action 停滞动作 (空 → notify)。
func (w *WatchdogSpec) action() string {
	if w == nil || w.Action == "" {
		return WatchdogNotify
	}
	return w.Action
}

// validate 声明合法性 (由 GraphSpec.Validate 调用)。
func (w *WatchdogSpec) validate() error {
	if w == nil {
		return nil
	}
	switch w.Action {
	case "", WatchdogNotify, WatchdogFail:
	default:
		// 显式报错而不是退到 notify: 静默降级会让一个写了 action:"retry" 的人
		// 以为运行会被干预, 而实际只发了条日志 —— fail-open 且完全不可考。
		return fmt.Errorf("graph: policies.watchdog.action=%q 不支持 (只有 %q|%q; retry 未实现, 理由见 watchdog.go 文件头)",
			w.Action, WatchdogNotify, WatchdogFail)
	}
	if w.StallSec < 0 || w.CheckSec < 0 || w.MaxNotices < 0 {
		return fmt.Errorf("graph: policies.watchdog 的 stall_sec/check_sec/max_notices 不能为负")
	}
	return nil
}

// progressClock 记录 journal 最后一次追加的时刻 (图级进展的唯一可观测量)。
//
// 单独一个类型而不是往 runCtx 塞两个字段: 它被 appendEv 闭包 (写) 与 watchdog
// goroutine (读) 并发访问, 把锁与被保护的字段放在一起才不会有人在别处漏加锁。
type progressClock struct {
	mu   sync.Mutex
	last time.Time
	// events 已记账的事件条数, 只为 hook 载荷提供"停滞前一共发生过多少事"。
	events int64
}

func newProgressClock(now time.Time) *progressClock {
	return &progressClock{last: now}
}

// touch 记一次 journal 追加。
func (p *progressClock) touch(now time.Time) {
	p.mu.Lock()
	p.last = now
	p.events++
	p.mu.Unlock()
}

// snapshot 返回 (最后一次追加时刻, 累计事件数)。
func (p *progressClock) snapshot() (time.Time, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last, p.events
}

// runGraphWatchdog 图级停滞巡检 (阻塞, 由 Engine.Run 起在独立 goroutine 里)。
//
// 参数 cancelRun 是本次运行的 ctx 取消函数; action=fail 时调用它, 于是停滞走的是
// **引擎既有的取消路径** (见文件头第 2 点), 不新增终态。
//
// 只在 ctx 结束时返回 —— Engine.Run 在 scheduleDAG 返回后取消 wdCtx, 于是本
// goroutine 必然收敛, 不会泄漏到下一次运行。
func runGraphWatchdog(ctx context.Context, spec *WatchdogSpec, clock *progressClock,
	runID, graphName string, hooks HookBus, appendEv evAppender, cancelRun func(),
	now func() time.Time) {
	if spec == nil || clock == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	stall := spec.stallAfter()
	act := spec.action()
	ticker := time.NewTicker(spec.checkEvery())
	defer ticker.Stop()

	notices := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last, events := clock.snapshot()
			silent := now().Sub(last)
			if silent < stall {
				continue
			}
			notices++
			data := map[string]any{
				"stall_ms":     silent.Milliseconds(),
				"threshold_ms": stall.Milliseconds(),
				"action":       act,
				"repeat":       notices,
				"events":       events,
			}
			// journal 先于 hook: 记账是无条件的, 而 hook 消费方可能很慢。
			//
			// ⚠️ 这条 appendEv **会自己刷新 progressClock** (它就是同一个写入口),
			// 于是下一次判定要重新等满一个 stall 周期。这是有意的: 否则 notify 档下
			// 每个巡检周期都会判定一次, 一个真卡死的图会写几百条 graph.stalled。
			// MaxNotices 是另一道闸 (给"只想告一次"的人)。
			appendEv(EvGraphStalled, "", data)
			if hooks != nil {
				// 决策**刻意丢弃**: 停滞是观测事件, 让 hook 消费方能否决"要不要算停滞"
				// 等于把一条治理决策交给一个纯观测桥 (teamGraphHooks 恒放行)。
				_ = hooks.Emit(ctx, HookEvent{
					Scope: ScopeGraph, Phase: "stalled", RunID: runID,
					Payload: map[string]any{
						"graph": graphName, "stall_ms": silent.Milliseconds(),
						"threshold_ms": stall.Milliseconds(), "action": act,
						"repeat": notices, "events": events,
					},
				})
			}
			if act == WatchdogFail && cancelRun != nil {
				cancelRun()
				return // 已放弃本次运行, 再巡检没有意义
			}
			if spec.MaxNotices > 0 && notices >= spec.MaxNotices {
				return
			}
		}
	}
}
