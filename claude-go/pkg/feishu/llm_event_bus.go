package feishu

// llm_event_bus.go —— design/02 §3.4.2 L4 通信系统 (EventBus) 的第一条**生产**链路。
//
// 链路形状:
//
//	pkg/api  fireEvent (重试/熔断/降级/致命失败, 14 处调用点, 覆盖进程内所有 Client)
//	   └─ api.SetGlobalLLMEventSink ──▶ eventbus.Default().Publish("llm.event.<type>")
//	                                        └─▶ Subscribe("llm.event.*") ──▶ 本文件的消费循环
//	                                              ├─ 落 [LLM事件] 日志 (全部来源)
//	                                              └─ 播报到在跑团队的飞书会话 (口径见下)
//
// 为什么这条链路值得从"直接回调"改成过总线 —— 三个具体理由, 都是踩过的:
//
//  1. **单回调字段漏得静默**。改造前播报挂在 `aiClient.OnLLMEvent` 这一个字段上,
//     而生产里有 7 处 `api.NewClient`: 主模型 (bot.go:309)、advisor (bot.go:462
//     与 :3265 的 /model 切换)、dashboard 诊断 (dashboard/llm_client.go:299),
//     以及 CLI/wechat 那几个别的进程。只有主模型那个被赋了值 —— 另外几个的 429
//     重试、熔断开合、"N 次全部失败"从来没有任何人收到过, 连日志都没有。少赋一个
//     结构体字段不会编译报错, 这类漏只能靠通读装配代码发现。改成进程级 sink 后,
//     "哪个 Client 实例"不再决定"事件能不能被看见"。
//
//  2. **观测把背压传染给了执行**。fireEvent 是在 SendMessage/StreamMessage 的
//     **重试循环内部**同步调的; 旧回调在里面直接 `for 每个在跑的团队 { 发飞书 }`,
//     于是一次 429 退避要额外等 N 次飞书 HTTP 往返才继续。现在 sink 只做一次
//     非阻塞 Publish (ChanBus 满则丢并计数), 真正的发送在本文件的消费 goroutine 里。
//
//  3. 这是 §3.4.2 落地的**最小可通电切片**: 换 NATS 后端时只动
//     `eventbus.Default()` 一处, 产生方与订阅方一行不改。
//
// 刻意保持不变的地方 (免得"顺手改进"改掉线上口径):
//
//   - **飞书消息文案逐字不变**: `%s **LLM 事件 [%s]**\n%s` + 同一张图标表。
//   - **播报范围逐字不变**: 只播报主模型客户端及其克隆 (Client.Tag 以 "feishu"
//     打头; WithModel 克隆成 "feishu:<model>", ConfiguredCloneFull 原样继承)。
//     advisor / dashboard 这些**此前根本发不出事件**的来源现在能到总线了, 但它们
//     只落日志、不进飞书 —— 否则 :18080 上的消息量会凭空变多, 那是下游 8+ 平台
//     能感知到的默认行为变化。要放开只需删掉 broadcastable 这一个判断。
//   - **只在有团队在跑且有 ChatID 时播报**, 同改造前。

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/eventbus"
)

const (
	// llmEventSubjectPrefix LLM 运行事件的 subject 前缀 (点分段, 见 eventbus 包注释)。
	llmEventSubjectPrefix = "llm.event."
	// llmEventPattern 订阅模式: 末段通配, 命中 llm.event.<type> 全部类型。
	llmEventPattern = "llm.event.*"
	// llmEventBroadcastTag 允许播报到飞书的来源前缀 (= 主模型客户端及其克隆的 Tag)。
	llmEventBroadcastTag = "feishu"
)

// llmEventIcon 事件类型 → 图标 (与改造前的 switch 逐字一致; default 为 ℹ️)。
func llmEventIcon(eventType string) string {
	switch eventType {
	case "retry":
		return "🔄"
	case "circuit_open":
		return "🔴"
	case "circuit_close":
		return "🟢"
	case "fatal":
		return "🚨"
	default:
		return "ℹ️"
	}
}

// shouldBroadcastLLMEvent 判定一条事件是否进飞书播报。
//
// 口径 = 改造前的等价条件"这个 Client 被赋过 OnLLMEvent", 即主模型客户端及其克隆:
// WithModel 克隆的 Tag 是 "feishu:<model>", ConfiguredCloneFull 原样继承 "feishu",
// 故用前缀判定。advisor / dashboard 客户端返回 false —— 它们此前**根本发不出**
// 事件, 现在能进总线与日志, 但不该凭空给飞书加消息量。
func shouldBroadcastLLMEvent(source string) bool {
	return strings.HasPrefix(source, llmEventBroadcastTag)
}

// llmEventSubject 由事件类型拼出 subject。
//
// 空类型要兜住: Publish 不校验段是否为空, 而 "llm.event." 恰好**不**被
// "llm.event.*" 命中 (通配要求至少一段) —— 直接拼就成了一条永远没人收到的事件。
func llmEventSubject(eventType string) string {
	t := strings.TrimSpace(eventType)
	if t == "" {
		t = "unknown"
	}
	return llmEventSubjectPrefix + t
}

// startLLMEventBridge 接通 "全部 api.Client → EventBus → 飞书播报"。
//
// 订阅在前、注册 sink 在后: 反过来会有一个窗口期, 事件发到没有订阅者的总线上
// 被直接丢掉 (ChanBus 对无订阅者的 subject 是静默的)。
func (b *Bot) startLLMEventBridge() {
	bus := eventbus.Default()
	ch, unsub, err := bus.Subscribe(llmEventPattern, "")
	if err != nil {
		// 订阅失败只可能是 pattern 非法 (编译期常量, 实际不会发生)。不静默:
		// 不接总线就等于没有 LLM 事件播报, 必须在日志里说清楚。
		log.Printf("[LLM事件] 订阅 %s 失败, LLM 事件播报未启用: %v", llmEventPattern, err)
		return
	}
	b.llmEventUnsub = unsub

	api.SetGlobalLLMEventSink(func(rec api.LLMEventRecord) {
		// 非阻塞: 这里仍在 LLM 重试循环内, 绝不能做任何 I/O。
		_ = bus.Publish(llmEventSubject(rec.Type), eventbus.Event{Data: map[string]any{
			"type":   rec.Type,
			"detail": rec.Detail,
			"source": rec.Source,
			"model":  rec.Model,
		}})
	})

	go b.consumeLLMEvents(ch)
}

// consumeLLMEvents 消费循环; 通道由退订关闭, 循环随之退出。
func (b *Bot) consumeLLMEvents(ch <-chan eventbus.Event) {
	for ev := range ch {
		eventType := strEventField(ev, "type")
		detail := strEventField(ev, "detail")
		source := strEventField(ev, "source")
		model := strEventField(ev, "model")

		// 日志覆盖**全部**来源 —— advisor/dashboard 客户端的事件此前连日志都没有。
		log.Printf("[LLM事件] %s (source=%s model=%s): %s", eventType, source, model, detail)

		if !shouldBroadcastLLMEvent(source) {
			continue
		}
		msg := fmt.Sprintf("%s **LLM 事件 [%s]**\n%s", llmEventIcon(eventType), eventType, detail)
		if b.teamMgr == nil {
			continue
		}
		for _, t := range b.teamMgr.ListAllTeams() {
			if t.Status == agent.TeamStatusRunning && t.ChatID != "" {
				b.sendLongMessage(context.Background(), t.ChatID, msg)
			}
		}
	}
}

// stopLLMEventBridge 退订并摘掉全局 sink (Shutdown 调用)。
//
// 顺序与 start 相反: 先摘 sink 再退订, 否则退订后仍有事件被 Publish 到无人订阅的
// 总线上 —— 无害但会让 Drops 统计变得难解释。
func (b *Bot) stopLLMEventBridge() {
	if b.llmEventUnsub == nil {
		return
	}
	api.SetGlobalLLMEventSink(nil)
	b.llmEventUnsub()
	b.llmEventUnsub = nil
}

// strEventField 从事件载荷里取字符串字段 (缺失/类型不符 → 空串)。
func strEventField(ev eventbus.Event, key string) string {
	if ev.Data == nil {
		return ""
	}
	s, _ := ev.Data[key].(string)
	return s
}
