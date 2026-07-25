// Mailbox — 通信机制抽象的定向消息部分 (design/01 §4.11)。
//
// 现状为什么不够用: 仓内有两份"邮箱", 都是裸 slice, 都没有真正的消费方。
//
//   - agent.go:143 Team.Mailbox — TeamManager.CreateTeam 每次新建一份内存 slice,
//     SendMessage 往里 append, GetMessages 能读, 但全仓没有任何执行路径调用它,
//     进程一退整份消息就没了。装饰性未接线。
//   - teams.go:409 ProductionTeam.Mailbox — SendMailMessage 写进去并随 team.json
//     落盘, 顺带把同一条消息写进黑板 (所以 agent 实际是"经黑板"看到消息的)。
//     邮箱本身只被 dashboard 当展示数据读, 没有"投递给某个 agent 且只投一次"的语义。
//
// 抽象出接口换来三件事:
//  1. 换后端不动调用方 (内存 / statestore / 分布式);
//  2. **Take 的游标语义**: 同一条消息只投递一次。这是当前裸 slice 给不了的 ——
//     重跑/精修阶段会把整个 Mailbox 再拼一遍 prompt, 老消息重复投递;
//  3. 收编两份实现为同一接口, 为 M4 退役其中之一铺路。
//
// 谁应该消费 (本轮没接, 因为消费点在 coordinator.go/teams.go, 不在本次文件范围):
//   - 主消费方 = Coordinator 在每个阶段开跑前对本阶段角色 Take 一次, 把消息拼进
//     该阶段 prompt (与黑板快照并列)。接线点见文件尾部注释。
//   - 次消费方 = dashboard /api/teams/{name} 用 Inbox (非破坏性) 做展示。
package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// Mailbox 定向消息通信抽象 (design/01 §4.11 的 Mailbox 接口)。
type Mailbox interface {
	// Send 投递一条消息。To 为空视为广播 (所有 agent 都能收到)。
	Send(msg MailMessage) error
	// Inbox 非破坏性读取发给 agent 的消息 (含广播), 按投递顺序。
	Inbox(agent string) []MailMessage
}

// MailboxConsumer 带"取走一次"游标语义的邮箱。
//
// 为什么单独一个接口而不塞进 Mailbox: 展示型实现 (只读投影) 没有游标可推进,
// 强塞会逼它们实现一个骗人的 Take。消费方用类型断言按需取用。
type MailboxConsumer interface {
	Mailbox
	// Take 取走发给 agent 的未读消息并推进游标: 同一条消息不会被投递两次。
	Take(agent string) ([]MailMessage, error)
}

// normalizeMail 补齐消息缺省字段 (类型默认 message, 时间戳默认现在)。
// 统一在这里做, 免得每个实现各写一遍、各写各的默认值。
func normalizeMail(msg MailMessage) (MailMessage, error) {
	if strings.TrimSpace(msg.Content) == "" {
		return msg, fmt.Errorf("mailbox: 消息内容不能为空")
	}
	if msg.Type == "" {
		msg.Type = "message"
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	return msg, nil
}

// mailMatches 消息是否该投给 agent (To 为空 = 广播)。
func mailMatches(msg MailMessage, agent string) bool {
	return msg.To == "" || msg.To == agent
}

// ---------------------------------------------------------------------------
// MemMailbox: 进程内实现 (带游标)
// ---------------------------------------------------------------------------

// MemMailbox 纯内存邮箱。语义与持久实现一致, 用于测试与单进程嵌入。
type MemMailbox struct {
	mu     sync.Mutex
	msgs   []MailMessage
	cursor map[string]int // agent → 已消费到的全局下标 (非 per-agent 计数)
}

var _ MailboxConsumer = (*MemMailbox)(nil)

// NewMemMailbox 创建内存邮箱。
func NewMemMailbox() *MemMailbox {
	return &MemMailbox{cursor: make(map[string]int)}
}

func (m *MemMailbox) Send(msg MailMessage) error {
	norm, err := normalizeMail(msg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, norm)
	return nil
}

func (m *MemMailbox) Inbox(agent string) []MailMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MailMessage
	for _, msg := range m.msgs {
		if mailMatches(msg, agent) {
			out = append(out, msg)
		}
	}
	return out
}

// Take 游标记的是**全局下标**而不是 per-agent 已读条数: 后者在有广播消息时会算错
// (广播对每个 agent 都算一条, 但只占一个全局位置)。
func (m *MemMailbox) Take(agent string) ([]MailMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	from := m.cursor[agent]
	var out []MailMessage
	for i := from; i < len(m.msgs); i++ {
		if mailMatches(m.msgs[i], agent) {
			out = append(out, m.msgs[i])
		}
	}
	m.cursor[agent] = len(m.msgs)
	return out, nil
}

// Len 已投递消息总数 (观测用)。
func (m *MemMailbox) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

// ---------------------------------------------------------------------------
// PersistentMailbox: statestore 后端 (跨重启)
// ---------------------------------------------------------------------------

// PersistentMailbox 经 pkg/statestore 持久化的邮箱: 消息进 AppendLog, 游标进 KV。
//
// 为什么消息用 AppendLog 而不是 KV: 邮箱天然只追加, AppendLog 单次 write 提交一行,
// 崩溃最多留一个尾部半行 (ReadAll 容忍), 比"整文件 KV 读改写"更抗崩且并发写更省。
//
// 并发: 每实例一把锁。**不承诺跨进程互斥** —— statestore.FileStore 的锁是
// per-instance 的 (见其包注释), 两个进程同时 Take 同一个 agent 可能重复投递一次。
type PersistentMailbox struct {
	log      statestore.AppendLog
	kv       statestore.KVStore
	mu       sync.Mutex
	writeErr atomic.Int64
}

var _ MailboxConsumer = (*PersistentMailbox)(nil)

// NewPersistentMailbox 为某团队创建持久邮箱。
// team 名可能含 '/' 或中文, 经 flattenBucket 消毒 —— 否则 statestore.validateBucket
// 会让每次读写都退化成报错实例 (badBucketLog), 而且很难察觉。
func NewPersistentMailbox(store statestore.StateStore, team string) *PersistentMailbox {
	if store == nil {
		store = statestore.NewMemStore()
	}
	return &PersistentMailbox{
		log: store.Log(flattenBucket("mailbox", team)),
		kv:  store.KV(flattenBucket("mailbox-cursor", team)),
	}
}

func (p *PersistentMailbox) Send(msg MailMessage) error {
	norm, err := normalizeMail(msg)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.log.Append(norm); err != nil {
		p.writeErr.Add(1)
		return fmt.Errorf("mailbox: 投递失败: %w", err)
	}
	return nil
}

// readAllLocked 读出全部消息 (调用方须持锁)。
func (p *PersistentMailbox) readAllLocked() []MailMessage {
	var out []MailMessage
	err := p.log.ReadAll(func(line []byte) error {
		var msg MailMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil // 单条坏记录跳过: 一封坏信不该让整个收件箱不可读
		}
		out = append(out, msg)
		return nil
	})
	if err != nil {
		logging.For("mailbox").Warn("读取邮箱失败", "err", err)
	}
	return out
}

func (p *PersistentMailbox) Inbox(agent string) []MailMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []MailMessage
	for _, msg := range p.readAllLocked() {
		if mailMatches(msg, agent) {
			out = append(out, msg)
		}
	}
	return out
}

func (p *PersistentMailbox) Take(agent string) ([]MailMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := p.readAllLocked()
	var from int
	if _, err := p.kv.Get(agent, &from); err != nil {
		// 游标读不出来时宁可重复投递也不丢消息 (fail-open, 但要留痕)。
		logging.For("mailbox").Warn("读取邮箱游标失败, 从头投递", "agent", agent, "err", err)
		from = 0
	}
	if from > len(all) {
		from = len(all) // 日志被截断/轮转过, 游标越界则退回末尾
	}
	var out []MailMessage
	for i := from; i < len(all); i++ {
		if mailMatches(all[i], agent) {
			out = append(out, all[i])
		}
	}
	if err := p.kv.Put(agent, len(all)); err != nil {
		p.writeErr.Add(1)
		return out, fmt.Errorf("mailbox: 游标推进失败 (消息已返回, 可能重复投递): %w", err)
	}
	return out, nil
}

// WriteErrors 落盘失败计数 (可观测的降级证据)。
func (p *PersistentMailbox) WriteErrors() int64 { return p.writeErr.Load() }

// ---------------------------------------------------------------------------
// TeamMailbox: 既有 ProductionTeam.Mailbox 裸 slice 的接口化适配
// ---------------------------------------------------------------------------

// ProductionTeamMailbox 把 ProductionTeam.Mailbox 这份裸 slice 适配成 Mailbox 接口。
//
// 一行没动 teams.go: Send 的写法与 SendMailMessage (teams.go:1806) 保持一致 ——
// 同时写邮箱与黑板, 因为今天 agent 实际是"经黑板"看到消息的, 只写邮箱等于消息不可见。
// 游标存在适配器内 (不进 team.json): 跨重启会重投一次, 但绝不改动 team.json 的
// 磁盘格式 —— 老版本读新 team.json 仍然完好。要跨重启不重投就用 PersistentMailbox。
type ProductionTeamMailbox struct {
	team   *ProductionTeam
	mu     sync.Mutex
	cursor map[string]int
}

var _ MailboxConsumer = (*ProductionTeamMailbox)(nil)

// TeamMailbox 为团队创建邮箱视图。
func TeamMailbox(t *ProductionTeam) *ProductionTeamMailbox {
	return &ProductionTeamMailbox{team: t, cursor: make(map[string]int)}
}

func (m *ProductionTeamMailbox) Send(msg MailMessage) error {
	if m.team == nil {
		return fmt.Errorf("mailbox: 团队为空")
	}
	norm, err := normalizeMail(msg)
	if err != nil {
		return err
	}
	m.team.mu.Lock()
	m.team.Mailbox = append(m.team.Mailbox, norm)
	m.team.mu.Unlock()

	// 与 SendMailMessage 一致: 同步写黑板, 让消息对所有 agent 可见。
	if m.team.Blackboard != nil {
		m.team.Blackboard.Write(
			fmt.Sprintf("msg-%s-%d", norm.To, norm.Timestamp.UnixMilli()),
			fmt.Sprintf("From %s: %s", norm.From, norm.Content),
			norm.From, "context",
		)
	}
	m.team.persist() // persist 内部自己取 team.mu, 必须在解锁之后调用
	return nil
}

func (m *ProductionTeamMailbox) Inbox(agent string) []MailMessage {
	if m.team == nil {
		return nil
	}
	m.team.mu.Lock()
	defer m.team.mu.Unlock()
	var out []MailMessage
	for _, msg := range m.team.Mailbox {
		if mailMatches(msg, agent) {
			out = append(out, msg)
		}
	}
	return out
}

func (m *ProductionTeamMailbox) Take(agent string) ([]MailMessage, error) {
	if m.team == nil {
		return nil, fmt.Errorf("mailbox: 团队为空")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.team.mu.Lock()
	all := append([]MailMessage(nil), m.team.Mailbox...)
	m.team.mu.Unlock()

	from := m.cursor[agent]
	if from > len(all) {
		from = len(all)
	}
	var out []MailMessage
	for i := from; i < len(all); i++ {
		if mailMatches(all[i], agent) {
			out = append(out, all[i])
		}
	}
	m.cursor[agent] = len(all)
	return out, nil
}

// 接线点 (本次范围外, 需要改 pkg/agent/coordinator.go 或 teams.go):
//
//	① 消费方: coordinator.go 组装阶段 prompt 处 (与 SnapshotForRole 并列),
//	   mb := TeamMailbox(team); if msgs, _ := mb.Take(stage.Role); len(msgs) > 0 { 拼进 prompt }
//	② 生产方: teams.go:1806 SendMailMessage 的函数体可整体替换为
//	   return TeamMailbox(team).Send(MailMessage{From: from, To: to, Content: content})
//	   —— 行为等价 (同样写邮箱 + 黑板 + persist), 但从此走统一接口。
//	③ 若要跨重启不重复投递, 把 ①② 换成 NewPersistentMailbox(store, team.Name)。
