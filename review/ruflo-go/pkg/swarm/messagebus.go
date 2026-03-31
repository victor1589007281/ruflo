package swarm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

type queuedMsg struct {
	msg       api.Message
	enqueued  time.Time
	priorityN int // lower = higher priority for sorting
}

// MessageBus delivers prioritized per-agent queues with ACK/retry/TTL.
type MessageBus struct {
	mu sync.RWMutex

	cfg MessageBusConfig

	queues   map[string][]queuedMsg // agentId -> priority queue (slice sorted by priority)
	capacity int
	subs     map[string][]subscription
	subMu    sync.RWMutex

	stats   MessageBusStats
	statsMu sync.Mutex

	processCtx    context.Context
	processCancel context.CancelFunc
	processWg     sync.WaitGroup

	ackMu    sync.Mutex
	pending  map[string]chan struct{} // ACKID -> done
	maxRetry int
}

type subscription struct {
	cb     func(api.Message)
	filter func(api.Message) bool
}

// NewMessageBus constructs a bus with defaults.
func NewMessageBus(cfg MessageBusConfig) *MessageBus {
	if cfg.QueueCapacityPerAgent <= 0 {
		cfg.QueueCapacityPerAgent = 1024
	}
	if cfg.ProcessBatchMax <= 0 {
		cfg.ProcessBatchMax = 10
	}
	if cfg.ProcessInterval <= 0 {
		cfg.ProcessInterval = 10 * time.Millisecond
	}
	if cfg.SlidingWindowSecs <= 0 {
		cfg.SlidingWindowSecs = 5
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &MessageBus{
		cfg:           cfg,
		queues:        make(map[string][]queuedMsg),
		capacity:      cfg.QueueCapacityPerAgent,
		subs:          make(map[string][]subscription),
		processCtx:    ctx,
		processCancel: cancel,
		pending:       make(map[string]chan struct{}),
		maxRetry:      5,
	}
	b.statsMu.Lock()
	b.stats.LastWindowStart = time.Now()
	b.statsMu.Unlock()
	b.processWg.Add(1)
	go b.processLoop()
	return b
}

func priorityOrder(p api.MessagePriority) int {
	return -int(p)
}

// Send enqueues a message for the recipient agent.
func (b *MessageBus) Send(msg api.Message) error {
	if msg.To == "" {
		return errors.New("messagebus: empty To")
	}
	if msg.ID == "" {
		msg.ID = randomMsgID()
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	if msg.TTL > 0 {
		msg.ExpiresAt = msg.Timestamp.Add(msg.TTL)
	}
	if msg.Payload == nil {
		msg.Payload = make(map[string]any)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if !msg.ExpiresAt.IsZero() && time.Now().After(msg.ExpiresAt) {
		b.statsMu.Lock()
		b.stats.MessagesDropped++
		b.statsMu.Unlock()
		return errors.New("messagebus: TTL expired before enqueue")
	}

	qm := queuedMsg{
		msg:       msg,
		enqueued:  time.Now(),
		priorityN: priorityOrder(msg.Priority),
	}

	q := b.queues[msg.To]
	if len(q) >= b.capacity {
		// Evict lowest priority (highest priorityN).
		sort.SliceStable(q, func(i, j int) bool {
			if q[i].priorityN != q[j].priorityN {
				return q[i].priorityN < q[j].priorityN
			}
			return q[i].enqueued.Before(q[j].enqueued)
		})
		worst := len(q) - 1
		for i := len(q) - 1; i >= 0; i-- {
			if q[i].priorityN > q[worst].priorityN {
				worst = i
			}
		}
		q = append(q[:worst], q[worst+1:]...)
		b.statsMu.Lock()
		b.stats.MessagesDropped++
		b.statsMu.Unlock()
	}
	q = append(q, qm)
	sort.SliceStable(q, func(i, j int) bool {
		if q[i].priorityN != q[j].priorityN {
			return q[i].priorityN < q[j].priorityN
		}
		return q[i].enqueued.Before(q[j].enqueued)
	})
	b.queues[msg.To] = q

	b.statsMu.Lock()
	b.stats.MessagesSent++
	b.statsMu.Unlock()
	return nil
}

// Broadcast sends a copy to every known queue key except optional sender.
func (b *MessageBus) Broadcast(msg api.Message, excludeSender string) {
	b.mu.RLock()
	keys := make([]string, 0, len(b.queues))
	for k := range b.queues {
		keys = append(keys, k)
	}
	b.mu.RUnlock()
	if len(keys) == 0 {
		return
	}
	for _, k := range keys {
		if k == excludeSender {
			continue
		}
		cp := msg
		cp.To = k
		cp.ID = randomMsgID()
		_ = b.Send(cp)
	}
}

// Subscribe registers a callback for an agent's incoming messages.
func (b *MessageBus) Subscribe(agentID string, callback func(api.Message), filter func(api.Message) bool) {
	b.subMu.Lock()
	defer b.subMu.Unlock()
	b.subs[agentID] = append(b.subs[agentID], subscription{cb: callback, filter: filter})
}

// Unsubscribe removes all callbacks for an agent.
func (b *MessageBus) Unsubscribe(agentID string) {
	b.subMu.Lock()
	defer b.subMu.Unlock()
	delete(b.subs, agentID)
}

// GetQueueDepth returns queued (not yet delivered) messages for an agent.
func (b *MessageBus) GetQueueDepth(agentID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.queues[agentID])
}

// HasPendingMessages reports whether the agent has queued messages.
func (b *MessageBus) HasPendingMessages(agentID string) bool {
	return b.GetQueueDepth(agentID) > 0
}

// ACK acknowledges delivery for RequiresACK messages.
func (b *MessageBus) ACK(ackID string) {
	b.ackMu.Lock()
	ch, ok := b.pending[ackID]
	if ok {
		delete(b.pending, ackID)
	}
	b.ackMu.Unlock()
	if ok {
		close(ch)
	}
	b.statsMu.Lock()
	b.stats.MessagesACKed++
	b.statsMu.Unlock()
}

func (b *MessageBus) processLoop() {
	defer b.processWg.Done()
	ticker := time.NewTicker(b.cfg.ProcessInterval)
	defer ticker.Stop()
	window := time.Duration(b.cfg.SlidingWindowSecs) * time.Second

	for {
		select {
		case <-b.processCtx.Done():
			return
		case t := <-ticker.C:
			b.tickStats(window, t)
			b.drainOnce()
		}
	}
}

func (b *MessageBus) tickStats(window time.Duration, now time.Time) {
	b.statsMu.Lock()
	if now.Sub(b.stats.LastWindowStart) >= window {
		sec := now.Sub(b.stats.LastWindowStart).Seconds()
		if sec > 0 {
			b.stats.MessagesPerSec = float64(b.stats.WindowCount) / sec
		}
		b.stats.WindowCount = 0
		b.stats.LastWindowStart = now
	}
	b.stats.UpdatedAt = now
	b.statsMu.Unlock()
}

func (b *MessageBus) drainOnce() {
	b.mu.Lock()
	agents := make([]string, 0, len(b.queues))
	for a := range b.queues {
		agents = append(agents, a)
	}
	b.mu.Unlock()

	processed := int64(0)
	for _, agentID := range agents {
		for n := 0; n < b.cfg.ProcessBatchMax; n++ {
			b.mu.Lock()
			q, ok := b.queues[agentID]
			if !ok || len(q) == 0 {
				b.mu.Unlock()
				break
			}
			qm := q[0]
			q = q[1:]
			if len(q) == 0 {
				delete(b.queues, agentID)
			} else {
				b.queues[agentID] = q
			}
			b.mu.Unlock()

			if !qm.msg.ExpiresAt.IsZero() && time.Now().After(qm.msg.ExpiresAt) {
				b.statsMu.Lock()
				b.stats.MessagesDropped++
				b.statsMu.Unlock()
				continue
			}

			delivered := b.deliver(agentID, qm.msg)
			if !delivered && qm.msg.RetryCount < b.maxRetry {
				qm.msg.RetryCount++
				_ = b.Send(qm.msg)
			}
			processed++
		}
	}

	b.statsMu.Lock()
	b.stats.WindowCount += processed
	b.stats.MessagesProcessed += processed
	b.statsMu.Unlock()
}

func (b *MessageBus) deliver(agentID string, msg api.Message) bool {
	b.subMu.RLock()
	subs := append([]subscription(nil), b.subs[agentID]...)
	b.subMu.RUnlock()

	if len(subs) == 0 {
		return false
	}
	ok := true
	for _, s := range subs {
		if s.filter != nil && !s.filter(msg) {
			continue
		}
		if msg.RequiresACK && msg.ACKID == "" {
			msg.ACKID = randomMsgID()
		}
		if msg.RequiresACK {
			ch := make(chan struct{})
			b.ackMu.Lock()
			b.pending[msg.ACKID] = ch
			b.ackMu.Unlock()
			s.cb(msg)
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
				ok = false
			}
			b.ackMu.Lock()
			delete(b.pending, msg.ACKID)
			b.ackMu.Unlock()
		} else {
			s.cb(msg)
		}
	}
	return ok
}

// Stats snapshot.
func (b *MessageBus) Stats() MessageBusStats {
	b.statsMu.Lock()
	defer b.statsMu.Unlock()
	return MessageBusStats{
		MessagesSent:      b.stats.MessagesSent,
		MessagesDropped:   b.stats.MessagesDropped,
		MessagesACKed:     b.stats.MessagesACKed,
		MessagesProcessed: b.stats.MessagesProcessed,
		MessagesPerSec:    b.stats.MessagesPerSec,
		LastWindowStart:   b.stats.LastWindowStart,
		WindowCount:       b.stats.WindowCount,
		UpdatedAt:         b.stats.UpdatedAt,
	}
}

// Shutdown stops the process loop.
func (b *MessageBus) Shutdown() {
	b.processCancel()
	b.processWg.Wait()
}

func randomMsgID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
