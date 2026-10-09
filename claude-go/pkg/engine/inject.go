// inject.go — **运行中入站通道** (21.8.12 P5 / D6-3 缺口 2 级 2)。
//
// 改造前 QueryEngine 只有"开新一轮"的入口: SubmitMessage / SubmitStream /
// SubmitStreamBlocks 三个全是 append 一条 user 消息再从头跑。一个**正在跑的** run
// 是独立 goroutine (或独立循环), 调这三个只会并发起第二轮 —— 全仓没有"往当前这
// 一轮里塞一句话"的 API。
//
// 这条缺口的表现不是报错, 是**送达时机**: 团队信箱的注入发生在阶段起点一次
// (workflow.go 的 takeMailFor), 而一个阶段可能跑十分钟。中途发来的更正只能等这一轮
// 结束才看得到, 而那时基于旧信息的活已经干完了 —— 用户看到的是"我明明告诉它了"。
//
// ---------------------------------------------------------------------------
// 为什么是**独立的队列对象**而不是引擎上的一个字段
// ---------------------------------------------------------------------------
//
// RunIsolated 是可能并发的: CLI 团队路径下 agentPool 是 8 并发, 而 8 个 runner
// **共用同一个 *QueryEngine** (见 cmd/claude-go 的 agentFactory 闭包与它注释里那句
// "agentPool 8 并发共享同一个 eng")。注入如果落在引擎上, "这条消息是给哪一次 run 的"
// 就没有答案 —— 表现是消息被**另一个阶段**收走, 而两边都看不出异常 (收到的那个会
// 当真, 没收到的那个以为没人给它发信)。
//
// 所以入站通道的身份是 **run**, 不是 engine: 每次 RunIsolated 自己拿一个队列
// (IsolatedRunOptions.Inject), 主循环那条 (queryLoop) 用引擎自己那一个 —— 它天然
// 一次只有一个 run (SubmitMessage 是串行的)。同一个类型, 两种生命期。
package engine

import (
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// InjectQueue 一次运行的入站队列。
//
// 零值可用 (msgs 为 nil 切片, running 为 0 = 没在跑)。所有方法都持锁: 生产方
// (信箱中继) 与消费方 (queryLoop / RunIsolated) 在不同的 goroutine 上。
type InjectQueue struct {
	mu      sync.Mutex
	msgs    []types.Message
	running int // 正在跑的循环数; >0 表示队列有人接
}

// NewInjectQueue 建一个入站队列。
func NewInjectQueue() *InjectQueue { return &InjectQueue{} }

// Enter 标记"有一个循环开始跑了"。与 Leave 成对, 必须用 defer 配平 ——
// 漏掉 Leave 的后果不是崩溃: 队列会**永久认为有人接**, 之后每一条注入都被收下、
// 永远不被消费, 而没有任何信号 (见 TestRunningFlagClearedAfterRun)。
func (q *InjectQueue) Enter() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.running++
	q.mu.Unlock()
}

// Leave 标记"一个循环结束了"。
func (q *InjectQueue) Leave() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.running--
	q.mu.Unlock()
}

// Active 报告当前是否有循环能接手注入。**不消费**任何东西, 可反复调用。
//
// 这个方法存在的理由是取信是破坏性的 (见 pkg/agent/mail_relay.go 的约束 ①):
// 调用方必须先问"现在能不能接"再取, 否则引擎空闲期间会"取出来又退回去"。
func (q *InjectQueue) Active() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running > 0
}

// Inject 往**正在运行的**循环注入一条 user 消息。
//
// 返回值:
//   - true  —— 已入队, 下一个轮次边界会被取走并投递给模型;
//   - false —— 没有循环在跑。此时消息**不会**被保存: "注入一个空闲引擎"与"开一轮
//     新的"是两件不同的事, 而后者有自己的入口 (SubmitMessage), 它会做完整的会话
//     落盘与快照。静默把注入降级成新 run 会让调用方以为消息在**当前**上下文里,
//     实际却起了一个新会话 —— 那是一次无声的语义替换。
//
// 只接受 role=user, 且正文非空:
//   - assistant 注入 = **伪造模型自己的发言**, 它落进 Messages 后参与后续所有轮;
//   - system 注入会破坏 system/user 的分界, 并与 TaskInstruction 那条"把指令放进
//     system prompt"的正规路径分岔成两条; 两者都不是"调用方粗心", 是语义上不该
//     存在的东西, 所以在入口按 role 拒, 而不是相信调用方。
//   - 空正文会白白改变前缀缓存 (与 injectHookContext 里"空内容直接不注入"同一条理由)。
func (q *InjectQueue) Inject(msg types.Message) bool {
	if q == nil {
		return false
	}
	if msg.Type != types.MessageTypeUser {
		logging.For("engine").Warn(
			"运行中注入只接受 user 消息, 已丢弃 (伪造 assistant 发言或改 system 分界都不该走这条通道)",
			"role", msg.Type)
		return false
	}
	if strings.TrimSpace(textOfMessage(msg)) == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running == 0 {
		// 没有在跑的循环: 丢弃并**如实回报** —— 调用方据此决定是改走 SubmitMessage
		// 还是重试。这里不能吞掉后返回 true, 那会让"消息没送到"看起来像送到了。
		return false
	}
	q.msgs = append(q.msgs, msg)
	return true
}

// Drain 取走并清空待注入消息。只在循环的轮次边界调用。
//
// 没有 `len(q.msgs)==0` 的早退分支: 空队列取到的就是 nil, 加那个分支是**等价代码**
// (变异测试里删掉它一条用例都不变红, 因为它真的没做事)。一个不做事的守卫比没有
// 守卫更糟 —— 读的人会以为它在挡什么。
func (q *InjectQueue) Drain() []types.Message {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.msgs
	q.msgs = nil
	return out
}

// Pending 报告队列里还没被取走的条数。
//
// 为什么需要它: 注入是**轮次边界**的 (每条工具调用就是一轮, 所以几秒一条), 而 run
// 是有终点的一次性过程。最后一条消息在 run 结束前几百毫秒到达时, 它不会被投递 ——
// 而"没投递"与"没发送"在调用方看来完全一样。这条读数是那个差额的唯一出口。
//
// 只在 run **结束时**取 (此时队列里剩下的就是被丢弃的), 不是每轮取。
func (q *InjectQueue) Pending() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.msgs)
}

// textOfMessage 拼出一条消息的全部 text block 正文。
//
// 与 QueryEngine 里那处逐字同形 —— 只认 ContentBlockText, 不认 tool_use/tool_result:
// 一条只带 tool_use 的 user 消息没有任何"话"要对模型说, 放行它等于注入一条空轮次。
func textOfMessage(msg types.Message) string {
	var b strings.Builder
	for _, blk := range msg.Content {
		if blk.Type == types.ContentBlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
