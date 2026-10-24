// Mailbox — 通信机制抽象的定向消息部分 (design/01 §4.11)。
//
// 它解决的是黑板**结构上做不到**的那件事。黑板是广播通道: BoardEntry 只有
// {Key, Value, Author, Category, Timestamp}, **没有 To**。所以"这条只给 coder"在
// 黑板里无从表达 —— 写进去就是全员可见。定向消息必须有它自己的一条通道。
//
// 抽象出接口换来三件事:
//  1. 换后端不动调用方 (内存 / statestore / 分布式);
//  2. **Take 的游标语义**: 同一条消息只投递一次。这是裸 slice 给不了的 ——
//     重跑/精修阶段会把整个 Mailbox 再拼一遍 prompt, 老消息重复投递;
//  3. 收编多份实现为同一接口 (2026-09-25 已完成: 四份 → 一份, 见下方收敛记录)。
//
// 谁应该消费 (P3 接线, 见文件尾部注释): 三条执行路径 (workflow/swarm/graph)
// **必须一次接全**。只接其中一条, 另外两条上的 agent 永远收不到信, 而那不会有
// 任何报错 —— 收不到消息看起来和"没人给他发消息"完全一样。
package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

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
// PersistentMailbox: 唯一实现 (statestore 后端, 跨重启)
// ---------------------------------------------------------------------------
//
// 收敛记录 (2026-09-25, 21.8.9-D6): 本文件曾经有四份"邮箱"并存 ——
// MemMailbox / PersistentMailbox / ProductionTeamMailbox, 外加
// pkg/tool/builtin/teamtools.go 里一份由 LLM 工具写入、零读者的 Mailboxes map。
// 现在只留 PersistentMailbox 一份:
//
//   - MemMailbox 是**纯冗余**: NewPersistentMailbox(statestore.NewMemStore(), …)
//     逐字节等价 (MemStore 实现了 StateStore, 同时提供 Log 与 KV)。要内存语义就传
//     一个 MemStore, 不必再维护第二份实现。
//   - ProductionTeamMailbox 是**适配器**, 被适配的 ProductionTeam.Mailbox 裸 slice
//     已删, 适配器随之消失。
//   - teamtools 那份不是冗余而是**空承诺** (工具回"已投递"而无人读), 已删。
//
// 收敛后 Mailbox 接口的原始目的 (换后端不动调用方) 才第一次真正可达:
// 今天"换后端"不必先在图里选四份语义还不完全一致的实现中的一份。

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

// Send 归一化后把消息追加到持久日志; 追加失败时累加 writeErr 计数并返回错误。
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

// Inbox 返回收件人匹配的全部消息, 不推进游标(可重复读取, 供展示用)。
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

// Take 取出该收件人在上次游标之后的新消息, 并把游标推进到日志末尾。
//
// 算法解读：
//
//	原理：游标记的是「已消费到日志的第几条」, 与收件人无关; 取出时只对游标之后的消息做收件人匹配, 最后无条件把游标推到当前日志长度。
//	关键不变量：游标必须以「整条日志的位置」而非「该收件人的第几条」为单位——按后者记, 其他收件人的消息会让下标错位, 造成漏投或重复投。
//	代价与边界：游标读失败时 fail-open 退回 0, 宁可重复投递也不丢消息; 日志被轮转/截断导致游标越界时退回末尾(丢弃历史而非重放)。
//	设计权衡：每次 Take 都全量读日志再过滤, 换取「不必按收件人建索引」; 消息量大时应改成按收件人分片。
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
// 消费方 (21.8.9-D6-3): 两个注入点, 不是三条路径各一处
// ---------------------------------------------------------------------------
//
// 生产方: ProductionTeamManager.SendMailMessage → PersistentMailbox.Send。
//
// 消费方只有**两个**注入点:
//
//	① workflow.go 的 executeStage —— pipeline / fanout / adversarial / graph
//	   **四条路径全过这里** (graph 经 stageNodeRunner → ExecuteSingleStage →
//	   executeStage)。所以这一处就覆盖四条。
//	② swarm.go 的 runTask —— 蜂群自己拼 prompt 并直调 agent.Runner.Execute,
//	   不经过 executeStage, 所以它必须单独接一处。
//
// 早先的计划写的是"workflow / swarm / graph 各接一处", 那是按**执行器**数的;
// executeStage 才是四条路径的共同必经点 (与 W5 的 StageTools 同一条理由)。按执行器
// 接的代价是接了三处却漏掉 fanout/adversarial —— 而漏掉的表现是"那些 agent 收不到信",
// 与"根本没人给他发消息"**完全一样**, 没有任何报错。同理, 图路径**不需要**单独接:
// graph_adapter.go 那段只有黑板回写 (写), 从来没有读 —— 给它加写入不是接线, 是加噪音。
//
// 三条边界 (21.8.9-D6-3, 缺一不可, 全部落在 FormatMailboxForPrompt 里):
//   - **预算上限**: 不能像 HandoffContext 的 decision 段那样全量零预算列出;
//   - **来源标记**: 每条拼进 prompt 的消息写明 From 与写入时刻;
//   - **不越权**: 信箱内容不得携带工具/权限/角色声明 —— 否则模型可以靠"发消息给
//     自己"扩大自己的动作空间 (治理四律第四条, pkg/evolution/govern/authority.go)。
//     信箱是数据通道, 不是配置通道。

// 注入预算。信箱是**增量**通道: 一个失控的生产者不该把阶段 prompt 顶爆。
const (
	// MailboxPromptBudget 单次注入的正文总预算 (字符)。
	MailboxPromptBudget = 2000
	// MailboxMaxMessages 单次注入的消息条数上限。
	MailboxMaxMessages = 10
)

// FormatMailboxForPrompt 把定向消息渲染成一段可注入 prompt 的文本。
//
// 返回 "" 表示没有可注入的内容 —— 调用方据此保持 prompt 逐字节不变 (与
// HandoffContext 为空时同向: 没有人给这个 agent 发过消息的运行, 提示词不该有任何差别)。
//
// 三条边界逐条落实:
//
//  1. **预算上限**。逐条按剩余预算截断; 预算耗尽后**如实写明还有几条没注入**, 不静默
//     丢弃 —— "没人给我发消息"与"消息太多被丢了"如果长得一样, 排查时根本没有下手处。
//  2. **来源标记**。每条都带 From 与写入时刻: 信箱里装的是**别人说的话**, 不标来源就
//     没有任何依据判断该不该采信, 也无从分辨"这是用户直接指示"还是"某个同僚 agent 的
//     推测"。时刻同理 —— "两小时前那条"与"刚刚那条"的处理方式不同。
//  3. **不越权**。**逐字段白名单**渲染, 而不是 json.Marshal 整条。MailMessage 今天只有
//     From/To/Content/Type/Timestamp, 但整条 marshal 意味着**以后谁给它加一个 `Tools`
//     或 `Permission` 字段, 那个字段就会自动进 prompt** —— 于是模型可以靠"给自己发一条
//     消息"来扩大自己的动作空间, 而这是一条数据通道不该有的能力。白名单让新增字段默认
//     **不进** prompt, 要进得显式改这里。
//
// truncateBytes 按**字节**上限截断, 且不切断 UTF-8 字符。
//
// 不能直接用 truncateResult: 那个按 rune 截断, 与按字节算的预算不同单位, 中文会
// 超预算三倍 (见 FormatMailboxForPrompt 里的注释)。
func truncateBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	// 回退到最近一个 rune 边界。
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// FormatMailboxForPrompt 把邮箱消息渲染成注入 prompt 的一段文本, 受条数与字节双预算约束。
//
// 算法解读：
//
//	原理：逐条拼接, 每轮先看「条数是否达上限」与「已用字节是否达预算」, 再用扣除表头与截断提示后的剩余字节截断本条正文。
//	关键不变量：预算以**字节**为单位, 且截断必须回退到 UTF-8 字符边界——按 rune 截断会让中文超预算三倍(见 truncateBytes 的注释)。
//	代价与边界：单条超长消息会被截成半个句子并标注「本条被预算截断」; 未注入的条数只汇总成一行提示, 不再逐条列举。
//	设计权衡：表头只拼一次并复用于预算计算——同一字面量出现两处就会漂移, 而「超预算」这个失效方向不报错, 只是提示词悄悄变大。
func FormatMailboxForPrompt(msgs []MailMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	shown := 0
	for _, msg := range msgs {
		if shown >= MailboxMaxMessages || b.Len() >= MailboxPromptBudget {
			break
		}
		remain := MailboxPromptBudget - b.Len()
		if remain <= 0 {
			break
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue // normalizeMail 已拦空内容; 这里兜的是直接构造的结构体
		}
		from := strings.TrimSpace(msg.From)
		if from == "" {
			from = "(未署名)"
		}
		when := "(时刻未记录)"
		if !msg.Timestamp.IsZero() {
			when = msg.Timestamp.Format("2006-01-02 15:04:05")
		}
		// 表头**只在这里拼一次**。
		//
		// 曾经把表头算了一遍 (给下面算剩余空间), 又在 Fprintf 里把同一个字面量
		// **又写了一遍** —— 两处一旦漂移 (比如这里加了来源, 那里没加), 预算算的
		// 就不再是真正写出去的那一行, 而超预算这个方向不报错、只是提示词悄悄变大。
		// 与 W5 那对"覆盖表 / 定义"挤进同一个键的毛病同形: 两个真相源。
		header := fmt.Sprintf("- 来自 %s (%s): ", from, when)
		// 正文按**字节**截断, 且要给省略号与后续行留位。
		//
		// 这里曾经用 truncateResult(content, remain): 它按 **rune** 截断, 而预算
		// (b.Len()) 是**字节**。中文一个字 3 字节 ⇒ 一条 2000 字的中文消息能被放成
		// 6000 字节, 预算形同虚设 —— 而"超预算"这个失效方向恰恰不报错, 只是提示词
		// 悄悄变大。凡是拿 len() 当尺子的地方, 两个长度必须同单位。
		room := remain - len(header) - len("\n[本条被预算截断]\n")
		if room <= 0 {
			break
		}
		if len(content) > room {
			content = truncateBytes(content, room) + "\n[本条被预算截断]"
		}
		b.WriteString(header)
		b.WriteString(content)
		b.WriteString("\n")
		shown++
	}
	if shown == 0 {
		return ""
	}
	if dropped := len(msgs) - shown; dropped > 0 {
		fmt.Fprintf(&b, "- [另有 %d 条定向消息因预算未注入]\n", dropped)
	}
	return "## 定向消息 (发给你的, 不是广播)\n" +
		"说明: 以下是团队里其他成员或用户**单独发给你**的消息, 与共享黑板不同。\n" +
		b.String()
}

// takeMailFor 取走发给 team 里某个成员 (可能有多个地址) 的未读消息。
//
// 收敛成**一个**函数供两个注入点共用, 而不是各自 `NewPersistentMailbox(...).Take(...)`:
// 游标是**破坏性**的 (Take 推进后同一条不会再投递), 两个注入点若各自拼错参数
// (例如一个传 role、一个传 stage 名), 症状是"有时收得到有时收不到", 取决于哪条路径
// 先跑 —— 那是最难查的一类。
//
// # 为什么一个成员有多个地址
//
// 团队列表上给用户看的是 `- **<名字>** (<角色>) [状态]`, 两个名字都在眼前, 而
// `/team msg <团队> <Agent> <消息>` 的 <Agent> 参数没有任何一处告诉用户"必须是角色,
// 不能是名字"。于是**两种都可能被发出来**:
//
//	workflow/swarm 的成员  地址 = {Role, Name}   (coder | implement / researcher | t1)
//
// 只认其中一个的后果是: 发到另一个地址的消息**静默丢弃** —— 用户看到
// "📨 已发送", agent 那边什么都没有, 而"没人给我发消息"和"发错了地址"在 agent 侧
// 完全同形。所以两个地址都收, 并在下面按消息身份去重。
//
// 去重是必须的而不是保险: Take 对**广播** (To 为空) 是无条件匹配的, 收两个地址就会
// 把同一条广播取到两次 —— 一次注入两条一模一样的消息, 比收不到更像"出 bug 了"。
//
// 取信失败**不阻断阶段执行** (信箱是增量通道, 不是前置条件), 但返回的 err 由调用方
// 留痕 —— 静默失联与"没人发消息"长得一样。
func takeMailFor(store statestore.StateStore, teamName string, addresses ...string) ([]MailMessage, error) {
	if store == nil || teamName == "" {
		return nil, nil
	}
	mb := NewPersistentMailbox(store, teamName)
	seenAddr := make(map[string]bool, len(addresses))
	seenMsg := make(map[string]bool)
	var out []MailMessage
	var errs []string
	for _, addr := range addresses {
		addr = strings.TrimSpace(addr)
		if addr == "" || seenAddr[addr] {
			continue
		}
		seenAddr[addr] = true
		msgs, err := mb.Take(addr)
		if err != nil {
			errs = append(errs, fmt.Sprintf("地址 %q: %v", addr, err))
		}
		for _, m := range msgs {
			// 消息身份 = From + 写入时刻 + 正文。时刻在 Send 时由 normalizeMail 补齐,
			// 两条不同的消息撞上同一微秒且同人同文, 当成一条是可接受的。
			key := fmt.Sprintf("%s\x00%d\x00%s", m.From, m.Timestamp.UnixNano(), m.Content)
			if seenMsg[key] {
				continue
			}
			seenMsg[key] = true
			out = append(out, m)
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("mailbox: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// returnMail 把已经 Take 出来、但**没能投递**的消息退回信箱。
//
// 为什么必须有这一步: Take 推进游标是破坏性的, 一条已经取出的消息在调用方手里只有
// 两个去向 —— 要么投递, 要么退回。丢掉它不会有任何报错, 而用户看到的是"这条我发过了",
// 团队那边什么也没有 (与 takeMailFor 注释里那条"发错地址静默丢弃"是同一种失败:
// 发送方有确认, 接收方无感知)。
//
// # 退回的做法是"再追加一条", 为什么这样是对的
//
// Take 把游标推到 len(all); 而 Send 恰好落在**游标之后的下一个位置**, 于是同一个
// 地址的下一次 Take 会读到它。From/Timestamp/To 原样保留 (normalizeMail 只补零值
// 字段), 广播语义不变 (To 为空仍是广播)。
//
// 会重复吗? 不会 —— 依赖的是 takeMailFor 的去重键 (From + 写入时刻 + 正文)。
// 一条广播被退回后, 另一个**还没读过它**的成员会在同一轮里读到原件与退回件两条,
// 而两条的键逐字节相同, 在 takeMailFor 里就并成一条了。这不是巧合而是那条去重
// 存在的理由之一 (它本来是为了"一个成员的两个地址都收到广播"而写的)。
// 删掉 / 改宽那个去重键, 这里就会开始重复投递。
func returnMail(store statestore.StateStore, teamName string, msgs []MailMessage) error {
	if store == nil || teamName == "" || len(msgs) == 0 {
		return nil
	}
	mb := NewPersistentMailbox(store, teamName)
	var errs []string
	for _, m := range msgs {
		if err := mb.Send(m); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("mailbox: 退回 %d 条中 %d 条失败: %s",
			len(msgs), len(errs), strings.Join(errs, "; "))
	}
	return nil
}

// MailboxStore 供执行器取信的后端访问口。
//
// 为什么让 team / executor 从管理器拿, 而不是各自 New 一个 MemStore: 邮箱后端必须是
// **同一个 StateStore 实例**, 否则生产方写在 A 实例上、消费方从 B 实例读 —— 读到的
// 永远是空, 而"空"与"没人发消息"完全同形。这与 WithStateStore 那条注释
// (进程内只能有一个实例) 是同一条约束, 只是这里落到了信箱上。
