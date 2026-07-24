// Package eventbus 实现 design/02 §3.4.2 的通信系统抽象 (R0 接口抽取)。
//
// EventBus 是分层架构的事件总线: 黑板 Watch、dashboard SSE、飞书进度播报、
// 轨迹采集全部是 subject 订阅者; 任务分发走队列组语义。
// 本包提供进程内 channel 后端 ChanBus (默认); 分布式后端 (NATS JetStream)
// 只需实现同一接口。
//
// subject 命名约定 (点分段): task.dispatch.<caps-hash> / run.events.<runID>
// / user.msg.<chatID> / heartbeat.<kind> / cron.fire / notify.feishu 等。
package eventbus

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event 总线事件。
type Event struct {
	Subject string         // 事件主题 (Publish 时由总线钉为实际发布 subject)
	TS      int64          // Unix 毫秒时间戳 (Publish 时若为 0 由总线补齐)
	Data    map[string]any // 事件载荷
}

// EventBus 事件总线抽象。
type EventBus interface {
	// Publish 向 subject 发布一条事件。绝不阻塞发布方。
	Publish(subject string, ev Event) error
	// Subscribe 订阅 pattern:
	//   - group == ""  → 广播语义: 每个订阅者都收到;
	//   - group != ""  → 队列组语义: 同 (pattern, group) 组内轮询单投递 (任务分发)。
	// pattern 支持精确匹配与末段后缀通配 "a.b.*" (匹配 "a.b." 前缀下的任意后续段)。
	// 返回只读事件通道与退订函数; 退订后通道关闭、组内成员移除。
	Subscribe(pattern, group string) (<-chan Event, func(), error)
}

// DefaultBuf 订阅者通道默认缓冲大小。
const DefaultBuf = 256

// ChanBus 进程内 channel 后端的 EventBus 实现。
//
// 投递纪律: Publish 非阻塞 —— 订阅者通道满则丢弃该订阅者的本条事件并累加
// 其 drop 计数 (Drops() 观测), 绝不阻塞发布方。队列组轮询用原子计数取模。
// 并发安全。
type ChanBus struct {
	buf int

	mu     sync.RWMutex
	nextID int64
	subs   map[int64]*subscriber
	groups map[string]*groupState // key: pattern + "\x00" + group

	dropMu sync.Mutex
	drops  map[string]int64 // 订阅者标识 -> 丢弃条数
}

var _ EventBus = (*ChanBus)(nil)

// subscriber 单个订阅者。
type subscriber struct {
	id      int64
	pattern string
	group   string
	ch      chan Event
}

// key 订阅者观测标识: "<pattern>|<group>|#<id>" (广播时 group 为空)。
func (s *subscriber) key() string {
	return fmt.Sprintf("%s|%s|#%d", s.pattern, s.group, s.id)
}

// groupState 队列组状态: 成员列表 + 轮询原子计数。
type groupState struct {
	members []*subscriber // 有序成员表 (按订阅先后)
	rr      atomic.Int64  // 轮询计数, 取模选成员
}

// NewChanBus 创建进程内事件总线; buf <= 0 时使用 DefaultBuf (256)。
func NewChanBus(buf int) *ChanBus {
	if buf <= 0 {
		buf = DefaultBuf
	}
	return &ChanBus{
		buf:    buf,
		subs:   make(map[int64]*subscriber),
		groups: make(map[string]*groupState),
		drops:  make(map[string]int64),
	}
}

// validatePattern 校验订阅 pattern: 非空, "*" 仅允许出现在末段且独占该段。
func validatePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("eventbus: pattern 不能为空")
	}
	segs := strings.Split(pattern, ".")
	for i, seg := range segs {
		if seg == "" {
			return fmt.Errorf("eventbus: pattern %q 含空段", pattern)
		}
		if strings.Contains(seg, "*") && (seg != "*" || i != len(segs)-1) {
			return fmt.Errorf("eventbus: pattern %q 非法, 通配 * 仅允许独占末段 (如 a.b.*)", pattern)
		}
	}
	return nil
}

// match 判断 subject 是否命中 pattern。
// 精确匹配, 或 pattern 以 ".*" 结尾时匹配 "前缀." 之后至少一段的任意 subject
// (如 "run.events.*" 命中 "run.events.abc" 与 "run.events.abc.def", 不命中 "run.other" 与 "run.events")。
func match(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := pattern[:len(pattern)-1] // 保留尾部 ".", 如 "run.events."
		return len(subject) > len(prefix) && strings.HasPrefix(subject, prefix)
	}
	return false
}

// groupKey 队列组索引键。
func groupKey(pattern, group string) string {
	return pattern + "\x00" + group
}

// Subscribe 订阅 pattern (见 EventBus 接口说明)。返回的退订函数幂等。
func (b *ChanBus) Subscribe(pattern, group string) (<-chan Event, func(), error) {
	if err := validatePattern(pattern); err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	b.nextID++
	sub := &subscriber{
		id:      b.nextID,
		pattern: pattern,
		group:   group,
		ch:      make(chan Event, b.buf),
	}
	b.subs[sub.id] = sub
	if group != "" {
		gk := groupKey(pattern, group)
		gs, ok := b.groups[gk]
		if !ok {
			gs = &groupState{}
			b.groups[gk] = gs
		}
		gs.members = append(gs.members, sub)
	}
	b.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.subs, sub.id)
			if sub.group != "" {
				gk := groupKey(sub.pattern, sub.group)
				if gs, ok := b.groups[gk]; ok {
					kept := gs.members[:0]
					for _, m := range gs.members {
						if m.id != sub.id {
							kept = append(kept, m)
						}
					}
					gs.members = kept
					if len(gs.members) == 0 {
						delete(b.groups, gk)
					}
				}
			}
			// 在写锁内关闭: Publish 的投递在读锁内进行, 与此互斥, 不会向已关闭通道发送。
			close(sub.ch)
		})
	}
	return sub.ch, unsub, nil
}

// Publish 向 subject 发布事件。非阻塞: 满通道的订阅者丢本条并计数。
// ev.Subject 被钉为实际发布 subject; ev.TS 为 0 时补当前 Unix 毫秒。
func (b *ChanBus) Publish(subject string, ev Event) error {
	if subject == "" {
		return fmt.Errorf("eventbus: subject 不能为空")
	}
	if strings.Contains(subject, "*") {
		return fmt.Errorf("eventbus: subject %q 不允许含通配符", subject)
	}
	ev.Subject = subject
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	// 广播订阅者: 逐个非阻塞投递
	for _, sub := range b.subs {
		if sub.group != "" || !match(sub.pattern, subject) {
			continue
		}
		b.trySend(sub, ev)
	}
	// 队列组: 每个命中的 (pattern, group) 组只投一名成员 (原子计数轮询)
	for gk, gs := range b.groups {
		pattern := gk[:strings.IndexByte(gk, '\x00')]
		if !match(pattern, subject) || len(gs.members) == 0 {
			continue
		}
		n := gs.rr.Add(1) - 1
		target := gs.members[int(n%int64(len(gs.members)))]
		b.trySend(target, ev)
	}
	return nil
}

// trySend 非阻塞投递: 通道满则丢弃并累加该订阅者的 drop 计数。
// 须在持有 b.mu (读锁即可) 时调用, 保证与退订时的 close 互斥。
func (b *ChanBus) trySend(sub *subscriber, ev Event) {
	select {
	case sub.ch <- ev:
	default:
		b.dropMu.Lock()
		b.drops[sub.key()]++
		b.dropMu.Unlock()
	}
}

// Drops 返回各订阅者的丢弃计数快照, key 格式 "<pattern>|<group>|#<id>"。
// 退订后计数保留 (便于事后观测)。
func (b *ChanBus) Drops() map[string]int64 {
	b.dropMu.Lock()
	defer b.dropMu.Unlock()
	out := make(map[string]int64, len(b.drops))
	for k, v := range b.drops {
		out[k] = v
	}
	return out
}
