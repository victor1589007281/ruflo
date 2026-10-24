// mail_relay.go — 信箱的**运行中**投递 (21.8.12 P5 / D6-3 缺口 2 级 2)。
//
// 改造前, 一个成员的收件只在**阶段起点**发生一次 (workflow.go 的 takeMailFor)。
// 阶段跑十分钟时, 中途发来的消息要等这一轮结束才看得到 —— 而那时基于旧信息的活
// 已经干完了。用户看到的是"我明明告诉它了", agent 侧则完全无感 (它确实没收到)。
//
// 本文件补的就是那一段: 阶段**运行中**轮询信箱, 有新的就注入正在跑的 loop,
// 由引擎在**轮次边界** (一次工具调用一轮, 通常几秒) 交给模型。
//
// ---------------------------------------------------------------------------
// 三条必须同时成立的设计约束 (缺一条就会变成"补了反而更糟")
// ---------------------------------------------------------------------------
//
//  1. **先问再取**。Take 是**破坏性**的 (推进游标), 取到手却投不出去 = 一条用户
//     消息被静默吞掉。而它本来会在下一次读取时正常投递 —— 于是"加了一条运行中
//     通道"反而让某些消息永远收不到。这类回归最难查: 表现是"消息有时收得到有时
//     收不到", 取决于阶段跑得比发消息快还是慢。
//  2. **退回**。上面那个检查与注入之间存在一个极窄的窗口 (引擎恰好在这一刻收尾)。
//     注入落空的消息**退回信箱**, 下一次读取照常投递。整条通道因此是无损的:
//     要么运行中送到, 要么留给下一次读 —— 没有第三种下场。
//  3. **渲染只有一份**。注入正文由 FormatMailboxForPrompt 产出, 与阶段起点那条
//     路径**同源同预算**。各写一份的话, 预算上限 / 来源标记 / 不越权这三条边界
//     (见 mailbox.go 文件尾) 会在其中一条上悄悄失效, 而失效方向不报错。
package agent

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/statestore"
)

// MidRunMailInjector 由 AgentRunner **选择性**实现, 表示它背后的执行体支持往
// 正在运行的 loop 里注入内容。
//
// 为什么是**可选**接口而不是给 AgentRunner 加两个方法: 实现 AgentRunner 的还有
// worker/本地 runtime 那条路 (pkg/worker), 它们的"执行体"在别的机器上, 没有可注入
// 的 loop。强塞两个方法会逼那些实现编一个骗人的返回值 —— 而一个恒真或恒假的
// 实现比不实现更糟 (读代码的人会以为那条路也支持)。
//
// 为什么是**两个**方法而不是一个: 取信是破坏性的, 所以必须先问"现在能不能接",
// 再取。合成一个方法 (取完再问) 会让引擎空闲期间每一轮都"取出来又退回去",
// 信箱日志随轮询次数增长 —— 而且每条都带着"退回"的告警, 噪声盖住真信号。
type MidRunMailInjector interface {
	// CanInjectMidRun 报告当前是否有一次运行能接手注入。
	// 它**不消费**任何东西, 可以被反复调用。
	CanInjectMidRun() bool
	// InjectMidRun 把正文注入正在跑的 loop。返回 false = 没送出去 (引擎已收尾),
	// 此时调用方**必须**把已取出的消息退回信箱 (见 returnMail)。
	InjectMidRun(text string) bool
}

// mailRelayInterval 运行中轮询信箱的间隔。
//
// 取 2s 的理由: 注入只在**轮次边界**生效 (一次工具调用一轮, 通常几秒), 比这更密的
// 轮询换不来更早的送达, 只换来更多次 Take (而 Take 每次都要 ReadAll 整个信箱日志)。
const mailRelayInterval = 2 * time.Second

// startMidRunMailRelay 启动运行中信箱中继, 返回停止函数 (可重复调用)。
//
// 返回 noop 的全部情形都是"这条通道不适用", **不是错误**: 没有团队 (单 agent 路径)、
// 运行器不支持注入 (worker/远端 runtime)、团队没有信箱后端、或运维关掉了它。
// 四者各自有别的投递路径 (阶段起点那一处仍在), 在这里报错只会变成噪声。
//
// stage 可能为空串: withStage 只在 executeStage 那一处标注 (见 model.go 的论证),
// 其余调用方 (adversarial / novel) 不带 stage。takeMailFor 会跳过空地址,
// 于是收件人退化成 {role} —— 比编一个阶段名更保守。
func startMidRunMailRelay(ctx context.Context, runner AgentRunner, team *ProductionTeam, role, stage string) func() {
	noop := func() {}
	if team == nil || runner == nil || !midRunMailEnabled() {
		return noop
	}
	inj, ok := runner.(MidRunMailInjector)
	if !ok {
		return noop
	}
	store := team.mailStore()
	if store == nil {
		return noop
	}

	done := make(chan struct{})
	go func() {
		t := time.NewTicker(mailRelayInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				// 阶段被判超时/取消: 引擎那一侧也会退出, 中继没有留下的理由。
				return
			case <-t.C:
			}
			relayMailOnce(inj, store, team.Name, role, stage)
		}
	}()

	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// relayMailOnce 做一轮"取信 → 注入", 是无损的: 取出的消息只有"送达"与"退回"两种去向。
func relayMailOnce(inj MidRunMailInjector, store statestore.StateStore, teamName, role, stage string) {
	// 1. 先问再取 (约束 ①)。引擎没在跑时**一个字节都不取** —— 这条早退是整条通道
	//    无损的前提, 不是性能优化。
	if !inj.CanInjectMidRun() {
		return
	}
	msgs, err := takeMailFor(store, teamName, role, stage)
	if err != nil {
		// 与阶段起点同口径: 不阻断执行 (信箱是增量通道), 但必须留痕 ——
		// 静默失联看起来就是"没人给他发消息"。
		logging.For("mailbox").Warn("运行中取定向消息失败 (不阻断当前阶段)",
			"team", teamName, "role", role, "stage", stage, "err", err)
	}
	if len(msgs) == 0 {
		return
	}
	block := FormatMailboxForPrompt(msgs)
	if block == "" {
		// 全部是空正文 (normalizeMail 已经拦过, 这里兜的是直接构造的结构体)。
		// 与"注入失败"同一处置: 退回, 不吞。
		retLog(teamName, role, stage, returnMail(store, teamName, msgs))
		return
	}
	if inj.InjectMidRun(block) {
		logging.For("mailbox").Info("运行中注入定向消息",
			"team", teamName, "role", role, "stage", stage, "count", len(msgs))
		return
	}
	// 2. 引擎在这一刻收尾了 (窗口极窄)。退回, 下一次读取照常投递 (约束 ②)。
	retLog(teamName, role, stage, returnMail(store, teamName, msgs))
}

// retLog 记录一次"运行中注入落空、消息已退回信箱"的告警 (下一次读取时会照常投递)。
func retLog(teamName, role, stage string, err error) {
	logging.For("mailbox").Warn(
		"运行中注入落空, 消息已退回信箱 (将在下一次读取时投递)",
		"team", teamName, "role", role, "stage", stage, "err", err)
}

// midRunMailEnabled 运行中信箱投递的开关。**默认开**, 写 "0"/"false"/"off"/"no" 关掉。
//
// 为什么默认开: "中途发来的更正能立刻被看到"正是这条通道存在的全部理由, 默认关的
// 话等于没做 —— 一个没人打开的开关与一段死代码在验收上完全一样。
//
// 为什么仍然留一个开关: 它改的是**投递时机** (阶段起点一次 → 运行中即可达)。时机
// 变化在生产上有后果: 一条本意是"先别动手"的消息, 改造前必然在阶段开始前被读到,
// 改造后可能落在阶段中途。默认开是有意的取舍, 而"退回旧时机"是运维出问题时唯一
// 能立刻做的事 (改 env 重启 vs 回滚整份镜像)。
//
// 只在 startMidRunMailRelay **一处**读, 不散在各个消费点上: 散着读会让"关掉了但
// 某一处没读到"变成静默的半开状态 —— 而半开比全开更难解释。
func midRunMailEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GO_MIDRUN_MAIL"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}
