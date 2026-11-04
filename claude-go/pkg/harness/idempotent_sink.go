// idempotent_sink.go — TurnSink 的幂等包装（Moqi R-H4 验收第 3 条）。
//
// # 为什么需要这一层
//
// TurnSink 契约写明"同一 TurnID 重放两次只产生一条用户消息与一条助手消息"，但
// 契约本身没有任何强制力：AppendAssistant 是直通 sink 的，宿主（或重试的业务
// API）写两遍库里就多两条。把去重放在 harness 而不是要求每个宿主各自实现，是
// 因为宿主的实现分散且互不共享状态 —— 只要有一家漏了，重复消息就会出现在用户
// 面前。包装层是唯一能对"所有已发布 TurnSink 实现"同时生效的地方。
//
// # 为什么键是三元组而不是 turnID
//
// turnID 只在**会话内**唯一：业务侧常按会话自增（session-1 的 turn-1 与
// session-2 的 turn-1 是两条完全不同的消息），不同 owner 更是天然可以复用同一
// 编号。只按 turnID 去重会让第二个会话/租户的合法消息被静默丢弃 —— 这类 bug
// 在单会话测试里永远看不到。owner 还必须先归一：空 owner 与 "local" 在账本和
// ctx 里是同一个身份，若包装层按原始字符串比较，就会出现"同一个人两套键、
// 重放照样重复落库"的漏洞。
//
// # 为什么失败不缓存
//
// 缓存是"已写成功"的凭据，不是"写过了"的日志。若把失败的调用也记成已完成，
// 宿主重试时会得到一个 nil（看起来成功）却永远没有数据落库，而且没有任何
// 路径能再触发真正的写入 —— 失败会被永久掩盖。所以只有 err==nil 才落标记；
// 失败路径必须让下一次调用重新转发。
package harness

import (
	"context"
	"sync"
)

// TurnReplay 是 TurnSink 的可选读能力：按 (owner,session,turnID) 取已完成的助手消息。
//
// 用可选接口而不是扩展 TurnSink：TurnSink 是冻结契约（只增不删），扩展它会逼所有
// 已发布实现立刻跟进。作为独立接口，只有需要"重放前先查"的调用方（runner/宿主）
// 才做一次类型断言，其余实现完全不受影响。
//
// 返回 ok=false 的语义是"没有可复用的完成结果"：可能从未写过，也可能写了助手
// 消息但 turn 终态是 failed/interrupted（此时重放必须重新执行，不能把半截结果
// 当成完成）。error 留给将来需要真正查询底层存储的实现；包装层自身不走 I/O，
// 因此只在参数层面可能报错（当前恒为 nil）。
type TurnReplay interface {
	CompletedAssistant(ctx context.Context, ownerID, sessionID, turnID string) (AssistantRecord, bool, error)
}

// sinkKey 是幂等键。(owner, session, turn) 三元组，owner 已归一。
type sinkKey struct {
	owner   string
	session string
	turn    string
}

// sinkEntry 是单个逻辑 turn 的包装层状态。
type sinkEntry struct {
	// userEnsured 表示用户消息已确认持久化（EnsureUser 成功过）。
	userEnsured bool

	// assistant 是首次 AppendAssistant 成功时缓存下来的记录。
	assistant AssistantRecord
	// assistantDone 表示助手消息确实写入成功。false 时 assistant 无意义 ——
	// 区分"缓存了零值"与"从未成功"必须靠这个布尔位，不能看字段是否为空。
	assistantDone bool

	// turnTerminal 是最近一次终态 MarkTurn 的状态（completed/failed/interrupted）。
	// 只有终态才写；running/pending 不覆盖，否则"重放到一半的 turn"会被
	// 误判成终态。
	turnTerminal string
}

// IdempotentSink 包装宿主 sink，保证同一逻辑 turn 重放不会重复落库。
//
// 零值不可用，必须经 NewIdempotentSink 构造。所有方法对 nil receiver 安全：
// Runner 里 Sink=nil 是合法语义（"不落库"），包装层不能把它变成 panic。
type IdempotentSink struct {
	inner TurnSink

	mu      sync.Mutex
	entries map[sinkKey]*sinkEntry
	// inflight 是 per-key 的转发占位。为什么不用"先查后写"：两个并发调用会同时
	// 查到空、同时转发，sink 就写两遍。占位必须在锁内原子地"检查并插入"，
	// 后来者等这个 channel 关闭后再决定是返回缓存还是接手重试。
	inflight map[sinkKey]chan struct{}
}

var (
	_ TurnSink   = (*IdempotentSink)(nil)
	_ TurnReplay = (*IdempotentSink)(nil)
)

// NewIdempotentSink 包装 inner。inner 为 nil 时返回 nil，保持 Runner 里
// "nil sink = 不落库"的语义；调用方不需要为包装层写特例。
func NewIdempotentSink(inner TurnSink) *IdempotentSink {
	if inner == nil {
		return nil
	}
	return &IdempotentSink{
		inner:    inner,
		entries:  make(map[sinkKey]*sinkEntry),
		inflight: make(map[sinkKey]chan struct{}),
	}
}

// keyFrom 从记录字段构造幂等键。owner 归一后比较，空串与 "local" 合并 ——
// 与账本/ctx 同口径，否则同一个人会因入口不同拿到两套键。
func keyFrom(ownerID, sessionID, turnID string) sinkKey {
	return sinkKey{owner: NormalizeOwner(ownerID), session: sessionID, turn: turnID}
}

// entryLocked 取或建条目。调用方必须持有 s.mu。
func (s *IdempotentSink) entryLocked(key sinkKey) *sinkEntry {
	e := s.entries[key]
	if e == nil {
		e = &sinkEntry{}
		s.entries[key] = e
	}
	return e
}

// EnsureUser 首次转发给 inner，之后同一键直接返回 already=true。
//
// 只有成功（err==nil）才落 userEnsured：失败的转发没有写进库里，若记成
// 已完成，宿主重试会拿到"已经写过"的假象，消息将永久缺失。inner 返回的
// already 原样透传 —— 那是宿主的判断（比如业务 API 已先写过），包装层
// 无权把它改写成 false。
func (s *IdempotentSink) EnsureUser(ctx context.Context, rec UserRecord) (bool, error) {
	if s == nil || s.inner == nil {
		return false, nil
	}
	key := keyFrom(rec.OwnerID, rec.SessionID, rec.TurnID)

	s.mu.Lock()
	if e := s.entries[key]; e != nil && e.userEnsured {
		s.mu.Unlock()
		return true, nil
	}
	s.mu.Unlock()

	already, err := s.inner.EnsureUser(ctx, rec)
	if err != nil {
		// 不落标记：失败必须允许重试。
		return already, err
	}

	s.mu.Lock()
	s.entryLocked(key).userEnsured = true
	s.mu.Unlock()
	return already, nil
}

// AppendAssistant 首次转发，成功后缓存记录；同一键后续调用直接返回 nil。
//
// 并发语义：同一键同时只允许一个转发在途，后来者等它结束后再看缓存。
// 第一个失败时后来者会接手重试（串行），所以"失败不缓存"与"不重复转发"
// 可以同时成立：成功最多写一次，失败不会被吞掉。
func (s *IdempotentSink) AppendAssistant(ctx context.Context, rec AssistantRecord) error {
	if s == nil || s.inner == nil {
		return nil
	}
	key := keyFrom(rec.OwnerID, rec.SessionID, rec.TurnID)

	// ctx 可能为 nil（与 context 包约定不同，但内部调用不保证），
	// 取 Done 前先兜底，避免 nil 接口调用 panic。
	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}

	for {
		s.mu.Lock()
		if e := s.entries[key]; e != nil && e.assistantDone {
			s.mu.Unlock()
			return nil
		}
		if ch, ok := s.inflight[key]; ok {
			// 已有转发在途：等它结束而不是自己再写一遍。
			s.mu.Unlock()
			select {
			case <-ch:
				continue // 回到开头重新判断：可能已成功，也可能需要接手重试
			case <-ctxDone:
				return ctx.Err()
			}
		}
		ch := make(chan struct{})
		s.inflight[key] = ch
		s.mu.Unlock()

		err := s.inner.AppendAssistant(ctx, rec)

		s.mu.Lock()
		if err == nil {
			e := s.entryLocked(key)
			e.assistant = rec
			e.assistantDone = true
		}
		delete(s.inflight, key)
		close(ch)
		s.mu.Unlock()
		return err
	}
}

// MarkTurn 总是转发：它是状态覆盖而不是追加，running→completed 的每一次
// 转换都必须真的写下去，否则崩溃恢复会读到过期的 running。
//
// 终态（completed/failed/interrupted）同时更新本地记录，供 CompletedAssistant
// 判断"这个 turn 的结果能不能复用"。即使 inner 报错也更新本地终态：本地已经
// 知道它失败了，此时把 failed 藏起来、让重放误以为可以跳过，风险更大。
// 失败不改变已缓存的助手消息 —— 消息本身写没写成功由 AppendAssistant 决定，
// 两件事不能互相污染（否则一次状态写失败会让已落盘的消息"消失"）。
func (s *IdempotentSink) MarkTurn(ctx context.Context, state TurnState) error {
	if s == nil || s.inner == nil {
		return nil
	}
	err := s.inner.MarkTurn(ctx, state)

	switch state.State {
	case "completed", "failed", "interrupted":
		key := keyFrom(state.OwnerID, state.SessionID, state.TurnID)
		s.mu.Lock()
		s.entryLocked(key).turnTerminal = state.State
		s.mu.Unlock()
	}
	return err
}

// CompletedAssistant 返回可复用的助手消息：必须**写入成功**且 turn 终态
// 不是 failed/interrupted。
//
// 为什么"只有用户消息"时要返回 ok=false：那说明模型调用/落盘还没完成，
// 假装完成会让 runner 跳过模型调用却拿不到任何回复 —— 用户看到空白。
// failed/interrupted 同理：半截结果重放时必须重新执行，而不是原样返回。
func (s *IdempotentSink) CompletedAssistant(ctx context.Context, ownerID, sessionID, turnID string) (AssistantRecord, bool, error) {
	if s == nil || s.inner == nil {
		return AssistantRecord{}, false, nil
	}
	key := keyFrom(ownerID, sessionID, turnID)

	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	if e == nil || !e.assistantDone {
		return AssistantRecord{}, false, nil
	}
	if e.turnTerminal == "failed" || e.turnTerminal == "interrupted" {
		return AssistantRecord{}, false, nil
	}
	return e.assistant, true, nil
}
