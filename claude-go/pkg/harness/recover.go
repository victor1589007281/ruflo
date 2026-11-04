// recover.go — 启动期崩溃恢复扫描（Q-H11 / design/07 §5.1）。
//
// # 为什么需要它
//
// 契约层早就给 interrupted 留了位置：turnState 把 cancelled/aborted 映射成
// "interrupted"，types.go 也写明它专门表达"用户消息已提交、助手消息未提交"的
// 在途 turn。但**没有任何启动扫描**在进程重启后把这些 turn 真标出来 —— 进程崩在
// 模型调用途中，库里留下一条 running 记录，重启后它既不会变成 completed，也不会
// 变成 interrupted，只是永远悬在那里。业务层看到的是一条"聊了一半、没有下文，
// 也没有任何失败标记"的会话。
//
// # 本文件只做判定与标记
//
// 四条约束是这个功能的核心，违反任何一条都会把一个恢复问题换成另一个更糟的问题：
//
//  1. **绝不自动重放模型调用**。§5.1 原文：重启后不能假装它完成了，也不能自动
//     重放模型调用（会产生重复/不同回复）。所以本文件不 import engine、不调
//     RunTurn、不碰 provider —— 从函数签名上就让它做不到。重试是业务决策，
//     不是恢复扫描的决策。
//  2. **绝不产生重复助手消息**。标记 interrupted 时**只**调 MarkTurn，
//     **不得**调 AppendAssistant。助手消息本来就没写成功，再补写一条等于凭空
//     造出一条模型从未产出过的回复。
//  3. **幂等**。对同一个 sink 连续跑两次，第二次不得重复标记：已经是 interrupted
//     的记录进 Skipped，而不是再 MarkTurn 一次。启动扫描会随每次重启/每次
//     supervisor 拉起执行，不幂等会让每次重启都多一遍写。
//  4. **单条失败不中断扫描**。某一条 MarkTurn 失败只进 Recovered.Errors，继续扫
//     下一条。返回的 error 只在**整体不可继续**时非 nil（ctx 取消、列不出来）。
//     反过来（一条失败就整体返回）会让一条坏记录挡住后面所有可恢复的 turn。
//
// # 事件续放不在这里
//
// Q-H11 的"事件可续放"由 events.go 的 Emitter.Replay(after=Seq) 负责。恢复扫描
// **不合成**任何事件：伪造一条 terminal 事件会让消费方看到一段本不存在的历史。
// 扫描只改持久化的 turn 状态，事件流的重连仍按已落盘事件续放。
//
// # 已知局限（生产接线尚不存在）
//
// Runner 目前只在 TurnRun 收尾时 MarkTurn（terminalState），**从不写 running/
// pending**；也还没有任何生产 TurnSink 实现 InFlightLister。也就是说本文件目前
// 是完整的契约与纯函数实现，但要真正自愈，还需要宿主在发起 turn 前写下 running
// 状态、并让持久层实现 ListInFlight。缺口写在这里而不是假装已闭合 —— 没有
// 生产写入方的恢复扫描会永远扫到空集合，那是最难发现的"绿"。
package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// TurnState.State 的取值（见 types.go）。这里用常量而不是散落的字面量：
// "interrupted" 拼错一个字母，扫描不会报错，只会永远标不上。
const (
	statePending     = "pending"
	stateRunning     = "running"
	stateCompleted   = "completed"
	stateFailed      = "failed"
	stateInterrupted = "interrupted"
)

// reasonCrashRecovery 是恢复扫描写入 TurnState.Reason 的可读原因。
//
// 用固定文案而不是只写 "crash"：运维与业务层看到它就能判断"助手消息确实不存在，
// 该重试还是该放弃由你决定"，而不必再去翻恢复代码。
const reasonCrashRecovery = "crash recovery: user committed, assistant missing"

var (
	// ErrRecoverNilSink 表示调用方没给 sink。
	//
	// nil sink 在 Runner 里是合法的（"只想知道结果、不想落库"），但在恢复扫描里
	// 没有意义：没有 sinks 就没有可读写的状态。必须报错而不是返回空结果 ——
	// 空结果长得和"没有在途 turn"一模一样。
	ErrRecoverNilSink = errors.New("harness: RecoverInFlight 需要非 nil 的 TurnSink")

	// ErrInFlightListerUnsupported 表示 sink 没实现 InFlightLister。
	//
	// 这是本文件最重要的一个取舍：TurnSink 是冻结契约（只增不删），而"列出在途
	// turn"需要一个新的读方法。我们**不**扩展 TurnSink —— 那会强逼所有已发布的
	// 宿主实现补一个它们可能根本做不到的方法。改成可选接口 + 类型断言。
	//
	// 但没实现时**必须报错，不能静默成功**。本仓反复踩过同一类坑（memory 里的
	// local-missing-is-not-a-delete-signal 等）：把"读不到/没检查"当成"没有"，
	// 于是恢复没跑，而调用方看到的是"零个在途 turn，一切正常"。启动日志里
	// 一条都没有，事故复盘时才发现自愈从未接线。
	ErrInFlightListerUnsupported = errors.New("harness: sink 未实现 InFlightLister，无法列出在途 turn（恢复未执行）")
)

// InFlightLister 是可选的扩展：宿主若能列出在途 turn，RecoverInFlight 就用它。
//
// 单独定义而不是并进 TurnSink：写契约（EnsureUser/AppendAssistant/MarkTurn）与
// 读契约的生命周期不同 —— 只读副本、审计工具可以只实现读；旧宿主的写实现也不该
// 因为多了一个恢复入口就编译不过。
//
// 契约：
//   - ownerID 为空表示"全部 owner"（单租户旧行为）；非空时返回结果应限定在该
//     owner。RecoverInFlight 不会只依赖这个承诺，见 recoverInFlightOwner。
//   - 返回的 TurnState 可以是终态记录（completed/failed/interrupted）—— 扫描
//     自己会分拣；调用方不必先过滤。这样"列在途"与"对账"可以共用一次全量读。
//   - 返回的顺序无所谓：RecoverInFlight 会按 (owner, session, turnID) 排序后处理。
type InFlightLister interface {
	ListInFlight(ctx context.Context, ownerID string) ([]TurnState, error)
}

// RecoverOptions 控制启动扫描。
type RecoverOptions struct {
	// Now 可注入时钟（测试用假时钟）。零值取 time.Now。
	//
	// 注入点必须在扫描开始时只取**一次**：逐条取时间会让同一次扫描里的记录
	// 用不同的"现在"做阈值比较，长扫描下先扫到的记录更容易被判新鲜。
	Now func() time.Time
	// StaleAfter 是判定「这条 running 记录已经死了」的阈值。
	// 0 = 不按时间判，一律视为在途（最保守）。
	//
	// 保守的含义：宁可把一条其实还活着的记录标成 interrupted，也不要把崩溃遗留
	// 当成"还在跑"而永远不收敛。多标一次 interrupted 只是多一条可读的状态；
	// 漏标则是一条永远悬空的会话。负数按 0 处理（"负数阈值"没有独立语义）。
	StaleAfter time.Duration
	// OwnerID 限定扫描范围；空 = 全部（单租户旧行为）。
	//
	// 同一份状态库里可能装着多个租户。启动扫描是**写**操作，scope 写错就是跨租户
	// 篡改数据，所以除了把它透传给 lister，扫描自己还会再按归一化 owner 过滤一遍
	// （见 recoverInFlightOwner）。lister 不守约时这层过滤仍然兜得住。
	OwnerID string
}

// Recovered 是一次扫描的结果。
type Recovered struct {
	// Interrupted 是被标记为 interrupted 的 turn。
	//
	// 元素是 TurnID（不是复合键）：业务层拿它去查会话历史。同一次扫描内按字典序
	// 排序，让两次启动的日志可以直接 diff。
	Interrupted []string
	// Skipped 是状态已终态、无需处理的 turn（含完成与已失败）。
	//
	// 也包括三类"扫描没有动它"的记录：已经是 interrupted 的（幂等）、
	// pending 之外的未知 State（语义不明，标了可能覆盖宿主的自定义状态）、
	// 以及 Persisted=true 的 running（助手消息已经在库里，它不是 §5.1 的在途）。
	// 把它们放进 Skipped 而不是静默丢弃，是为了让"我看到了但没动"可观测。
	Skipped []string
	// Errors 是标记失败但扫描继续的条目。
	//
	// 元素是带 turn 信息的包装错误；errors.Is 仍可穿透到宿主返回的原始错误。
	Errors []error
}

// RecoverInFlight 扫描并标记崩溃遗留的在途 turn。
//
// 算法：
//
//	① sink 必须非 nil 且实现 InFlightLister（否则整体失败，绝不静默成功）；
//	② 列出记录 → 按 (owner, session, turnID) 排序（先克隆，不污染 lister 的切片）；
//	③ 逐条判定：终态/未知/persisted/owner 不符 → Skipped；
//	   非终态且超过 StaleAfter → 只调 MarkTurn 标 interrupted；
//	④ 单条 MarkTurn 失败进 Errors 继续；ctx 取消则返回部分结果 + error。
//
// 幂等性来自 ③ 的"已经是 interrupted 就跳过"：第二次扫描时第一条已经不再是
// 候选，只会进 Skipped。注意幂等**不**依赖 lister 去重，也不依赖 MarkTurn 的
// 覆盖语义 —— 即使宿主把 MarkTurn 实现成追加写，我们也不会重复调用它。
//
// 不重放模型、不写助手消息：本函数除了 MarkTurn 之外不调用 sink 的任何方法，
// 也不持有 engine 引用（见文件头约束 1/2）。
func RecoverInFlight(ctx context.Context, sink TurnSink, opts RecoverOptions) (Recovered, error) {
	var out Recovered

	if sink == nil {
		return out, ErrRecoverNilSink
	}
	// 类型断言失败是**错误**而不是空结果，理由见 ErrInFlightListerUnsupported。
	lister, ok := sink.(InFlightLister)
	if !ok {
		return out, fmt.Errorf("%w（sink 类型 %T）", ErrInFlightListerUnsupported, sink)
	}
	if err := ctxErr(ctx); err != nil {
		return out, err
	}

	// owner 过滤在扫描侧独立算一次。opts.OwnerID 原样传给 lister（空 = 全部），
	// 但过滤用归一化值 —— " local " 与 "local" 是同一个 owner，别让空白差异
	// 把本该恢复的 turn 漏掉。
	ownerFilter := recoverInFlightOwner(opts.OwnerID)

	records, err := lister.ListInFlight(ctx, opts.OwnerID)
	if err != nil {
		// 列不出来 = 整体不可继续。返回包装错误而不是 Recovered.Errors：
		// Errors 的语义是"记录了，但这一条失败"，而这里一条都没处理。
		return out, fmt.Errorf("harness: 列出在途 turn 失败: %w", err)
	}
	// 克隆后再排序：lister 可能返回它内部切片的引用，就地排序等于改宿主数据。
	records = append([]TurnState(nil), records...)
	sort.SliceStable(records, func(i, j int) bool {
		return recoverInFlightKey(records[i]) < recoverInFlightKey(records[j])
	})

	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	nowT := now() // 整次扫描共用同一个"现在"，理由见 RecoverOptions.Now。

	// seen 去重：lister 返回重复条目时同一个 turn 只标记一次。重复通常是持久层
	// 索引问题，重试第二遍并不能让它变好，反而会掩盖"列出来的是两份"这个事实。
	seen := make(map[string]bool, len(records))
	for _, rec := range records {
		// 每条之间检查取消：扫描可能遍历整个状态库，不能在调用方已经放弃后继续写。
		if err := ctxErr(ctx); err != nil {
			return out, err
		}
		key := recoverInFlightKey(rec)
		if seen[key] {
			out.Skipped = append(out.Skipped, rec.TurnID)
			continue
		}
		seen[key] = true

		if !recoverInFlightCandidate(rec, ownerFilter) {
			out.Skipped = append(out.Skipped, rec.TurnID)
			continue
		}
		if !recoverInFlightStale(rec, nowT, opts.StaleAfter) {
			// 还没到 StaleAfter：可能真的有别的进程在跑这一轮（多副本部署时
			// 启动扫描会看到对端的活记录）。不标，也不报错。
			out.Skipped = append(out.Skipped, rec.TurnID)
			continue
		}

		st := TurnState{
			OwnerID:   rec.OwnerID,
			SessionID: rec.SessionID,
			TurnID:    rec.TurnID,
			State:     stateInterrupted,
			// 助手消息确实没有 —— 这正是 §5.1 的"在途"。写 true 会让业务层以为
			// 消息已落盘，从而不去补写/不提示用户。
			Persisted: false,
			Reason:    reasonCrashRecovery,
			UpdatedAt: nowT,
		}
		// 唯一的写调用。**不要**在这里补 AppendAssistant：见文件头约束 2。
		if err := sink.MarkTurn(ctx, st); err != nil {
			// 单条失败继续扫（约束 4）。带上 turn 信息，否则 Errors 里只有
			// "数据库超时"，业务层无法知道是哪一轮没恢复。
			out.Errors = append(out.Errors, fmt.Errorf("harness: 标记 turn %s 为 interrupted 失败: %w", rec.TurnID, err))
			continue
		}
		out.Interrupted = append(out.Interrupted, rec.TurnID)
	}

	// 输出也排序：Interrupted/Skipped 的可读性依赖确定性，而 records 的排序键
	// 含 owner/session，仅按 TurnID 看可能与处理顺序不同。
	sort.Strings(out.Interrupted)
	sort.Strings(out.Skipped)
	return out, nil
}

// recoverInFlightOwner 归一化扫描 owner 过滤条件；空串表示不过滤。
//
// 注意不能直接 NormalizeOwner(opts.OwnerID) 后当过滤条件：NormalizeOwner("")
// 返回 DefaultOwnerID，会把"全部 owner"悄悄变成"只看 local"，跨租户扫描就漏了。
func recoverInFlightOwner(ownerID string) string {
	if ownerID == "" {
		return ""
	}
	return NormalizeOwner(ownerID)
}

// recoverInFlightCandidate 判定一条记录是否属于 §5.1 的在途 turn。
//
// 在途 = 用户消息已提交、助手消息未提交：
//   - completed/failed/interrupted 已是终态 → 不是候选（Skipped）。
//   - pending/running 是 TurnState 文档里仅有的两个非终态：lister 按契约把它们
//     返回时，助手侧一定没有提交。两者都算候选 —— 只认 running 会让崩溃在
//     "写下 pending 之后、改成 running 之前"的窗口里彻底漏判。
//   - 未知 State：语义不明。标成 interrupted 可能覆盖宿主的自定义状态机，宁可
//     不动（进 Skipped 可观测）。
//   - Persisted=true 却没有终态：自相矛盾（助手消息已在库里）。它不可能是在途，
//     照常量标 Persisted=false 会抹掉"消息其实存在"这个事实。
//   - owner 不符：见 RecoverOptions.OwnerID，扫描侧必须自己兜底。
func recoverInFlightCandidate(rec TurnState, ownerFilter string) bool {
	switch rec.State {
	case statePending, stateRunning:
		// 继续判其它条件。
	default:
		return false
	}
	if rec.Persisted {
		return false
	}
	if ownerFilter != "" && NormalizeOwner(rec.OwnerID) != ownerFilter {
		return false
	}
	return true
}

// recoverInFlightStale 判定记录是否已死。
//
//   - StaleAfter <= 0：不按时间判，一律视为在途（最保守，见字段注释）。
//   - UpdatedAt 为零：没有存活证据。按死处理 —— 零值时间在比较里会被当成
//     "很久以前"，但依赖这一点太隐晦，这里显式判掉。
//   - 恰好等于阈值算死（>=）：阈值是"活多久算死"的边界，边界值取死更安全。
func recoverInFlightStale(rec TurnState, now time.Time, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		return true
	}
	if rec.UpdatedAt.IsZero() {
		return true
	}
	return now.Sub(rec.UpdatedAt) >= staleAfter
}

// recoverInFlightKey 是 turn 的稳定去重/排序键。
//
// TurnID 只在 (session) 作用域内唯一，单用 TurnID 会把不同 session 的同名 turn
// 合并掉。用 \x00 分隔：它不可能出现在正常的 ID 里，避免 ("a","bc") 与
// ("ab","c") 拼出同一个键。
func recoverInFlightKey(rec TurnState) string {
	return rec.OwnerID + "\x00" + rec.SessionID + "\x00" + rec.TurnID
}
