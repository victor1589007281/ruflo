// ephemeral.go — 逐轮临时上下文注入（Moqi R-H1）。
//
// 改造前检索类上下文只有一条通路: MemoryInjectHook / AgentDBSemanticInjectHook 把
// 结果写进 **SystemPrompt**。对陪伴场景这是错配, 而且错在两个不同的地方:
//
//  1. **位置**: system 是稳定前缀。检索结果每轮都可能不同, 写进 system 就等于每轮
//     让整段前缀缓存作废。实测代价见 engine.go "稳定/可变分层" 那段注释里的
//     1024 vs 4200 token 对比 —— 那还只是"任务块被记忆块挤掉"的一半问题。
//  2. **生命周期**: system 与 messages 都属于会被持久化和被压缩的状态。检索结果一旦
//     进去, 就会跟着 transcript 一起落盘、跟着压缩一起被摘要、跟着下一轮一起被重发。
//     于是"第 2 轮检索到的事实"会出现在第 3 轮的请求里, 而它已经过期了。
//
// 这里的契约只有一条, 但必须逐字节成立: **临时块只进本次模型请求**, 不进 messages、
// 不进 transcript、不参与压缩、不进下一轮。实现方式见 queryLoop 里那句
// appendEphemeralBlocks 的注释: 它作用在**转换后的 apiMessages** 上, 而 messages
// 从头到尾没被碰过 —— 所以"剥离"不是一个需要记得做的动作, 而是做不到的事。
// 但凡改成往 messages 里 append 再删, 就会有一条返回路径忘了删, 而那条路径是静默的。
//
// 身份是 **run**, 不是 engine: 走 ctx 而不是 Config 字段。同一个 *QueryEngine 会被
// 团队路径的 8 个并发 runner 共用 (见 inject.go 文件头), Config 字段会被别人覆盖;
// ctx 天然按 run 隔离, 且不需要任何锁。
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// EphemeralCriticalError 标记"业务显式要求失败即拒绝"的逐轮上下文错误（R-H1）。
//
// 引擎本身不认识 harness 的 ErrorKind，也不该反向依赖它；这里只做一件事：
// 把"这个错误不能降级"这件事从 harness 传到 queryLoop。其余 provider 错误仍然
// 按可选增强处理（空 patch + 降级），那是默认语义。
type EphemeralCriticalError struct {
	Err error
}

func (e *EphemeralCriticalError) Error() string {
	if e == nil || e.Err == nil {
		return "engine: 逐轮上下文 critical 失败"
	}
	return "engine: 逐轮上下文 critical 失败: " + e.Err.Error()
}

func (e *EphemeralCriticalError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// 两个薄封装只为让 appendEphemeralBlocks 的意图在调用点可读;
// 底层仍然用 encoding/json 的 ContentBlock 表示, 与 messagesToAPI 同一形状。
func jsonUnmarshalBlocks(raw json.RawMessage, out *[]types.ContentBlock) error {
	return json.Unmarshal(raw, out)
}

func jsonMarshalBlocks(blocks []types.ContentBlock) (json.RawMessage, error) {
	b, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// EphemeralView 是交给 provider 的只读视图。
//
// History 是**只读**的: provider 不得改写它, 也不得把自己产出的块塞回去。
type EphemeralView struct {
	// SentUpTo 是已发出消息的水位线: [0, SentUpTo) 一个字节都不能改。
	SentUpTo int
	// History 是本轮开始时的完整消息历史 (只读)。
	History []types.Message
	// UserText 是本轮用户输入的纯文本 (从最后一条 user 消息抽取), 供 provider
	// 做检索 key。理由与 HookContext.TaskInstruction 相同: 让 provider 自己去
	// system 里翻找是脆的。
	UserText string
}

// EphemeralProvider 产出本轮的临时上下文块。
//
// 第一返回值是块, 第二返回值是降级原因 (空 = 未降级), 第三是错误。
//
// 失败语义: 默认**降级不中断** —— 返回 error 时本轮照常回复, 降级原因进 trace。
// 这是刻意的: 检索是可选增强, 它挂了不该让用户这一轮收不到回复。需要"失败即拒绝"
// 的场景请把判定放在 critical hook 上 (见 internal_hook.HookPolicy), 不要在这里
// 制造第二条语义。
type EphemeralProvider func(ctx context.Context, view EphemeralView) (blocks []types.ContentBlock, degraded string, err error)

// ephemeralKey 是 ctx 里的私有键类型: 不导出, 别的包无法伪造成同一个键。
type ephemeralKey struct{}

// WithEphemeralProvider 把逐轮上下文提供器放进 ctx。
//
// 传 nil 等价于不设置 (调用方可以无条件调用, 不必先判空)。
func WithEphemeralProvider(ctx context.Context, p EphemeralProvider) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, ephemeralKey{}, p)
}

// EphemeralFromContext 取 ctx 里的逐轮上下文提供器; 没有时返回 nil。
//
// 为什么走 ctx 而不是 QueryEngine 上的字段或 Config 字段: 本引擎**不是**一次一个 run
// —— 团队路径下 agentPool 8 并发共用同一个 *QueryEngine (见 inject.go 文件头那段
// "身份是 run, 不是 engine")。装在字段上时, 两个并发 run 会互相覆盖对方的 provider,
// 表现是 A 轮的检索结果进了 B 轮的请求, 而两边都看不出异常。
func EphemeralFromContext(ctx context.Context) EphemeralProvider {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(ephemeralKey{}).(EphemeralProvider)
	return p
}

// lastUserText 取最后一条 user 消息的可读文本, 作为检索 key。
//
// 只取 text 块: tool_result 的内容是上一轮工具的输出, 把它当成本轮意图既长又无关,
// 而且会在工具密集回合里让检索 key 每一轮都变 —— 那正好是前缀缓存最怕的形状。
func lastUserText(messages []types.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Type != types.MessageTypeUser {
			continue
		}
		var b strings.Builder
		for _, c := range messages[i].Content {
			if c.Type == types.ContentBlockText && c.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	return ""
}

// collectEphemeral 调用 provider 并返回可注入的块。
//
// 四条边界都在这里收口, 调用点不必各判一遍:
//  1. provider 为 nil ⇒ 零块零降级 (改造前行为, 逐字节相同);
//  2. provider 返回普通 error ⇒ 零块 + 降级原因 + 记日志, 不向 queryLoop 传 error;
//  3. provider 返回 *EphemeralCriticalError ⇒ 原样上抛, 由 queryLoop 终止本轮
//     (R-H1: 只有业务显式标 critical 才失败);
//  4. provider 返回的块若含空文本或非法类型 ⇒ 该块被丢弃并记降级, 而不是把坏块
//     发给上游 (上游对空 text 块会 400, 一个 provider 的 bug 会变成整轮失败)。
func collectEphemeral(ctx context.Context, p EphemeralProvider, sentUpTo int, messages []types.Message) ([]types.ContentBlock, string, error) {
	if p == nil {
		return nil, "", nil
	}
	blocks, degraded, err := p(ctx, EphemeralView{
		SentUpTo: sentUpTo,
		History:  messages,
		UserText: lastUserText(messages),
	})
	if err != nil {
		var crit *EphemeralCriticalError
		if errors.As(err, &crit) {
			logging.For("engine").Warn("逐轮上下文提供器 critical 失败, 终止本轮", "err", err)
			return nil, "", err
		}
		logging.For("engine").Warn("逐轮上下文提供器失败, 本轮降级", "err", err)
		return nil, "context_degraded", nil
	}
	if degraded != "" {
		logging.For("engine").Warn("逐轮上下文提供器自报降级", "reason", degraded)
	}
	kept := blocks[:0:0]
	for _, b := range blocks {
		if b.Type == types.ContentBlockText && strings.TrimSpace(b.Text) == "" {
			degraded = "context_block_empty"
			continue
		}
		kept = append(kept, b)
	}
	return kept, degraded, nil
}

// appendEphemeralBlocks 把临时块并进本轮请求的**最后一条 user 消息**。
//
// 为什么是"最后一条 user 消息的额外块"而不是追加一条新 user 消息:
//   - 保持 user/assistant 角色交替稳定 —— 连续两条 user 消息会被部分上游网关合并
//     (见 messagesToAPI 里合并连续同角色那段), 合并后的边界与缓存断点都会漂移;
//   - 临时块天然位于当前 turn 尾部, 在缓存断点之后, 不碰稳定 system 前缀。
//
// 为什么作用在 apiMessages 而不是 messages: 见本文件头 —— messages 不被碰,
// 所以"临时块进了 transcript"这件事在结构上不可能发生。
//
// 边界: 末尾没有 user 消息 (纯 assistant 收口轮) 时不注入。此时注入无处可放, 硬塞
// 一条新 user 消息会破坏角色交替, 代价大于收益。
func appendEphemeralBlocks(apiMessages []types.APIMessage, blocks []types.ContentBlock) []types.APIMessage {
	if len(blocks) == 0 || len(apiMessages) == 0 {
		return apiMessages
	}
	last := len(apiMessages) - 1
	if apiMessages[last].Role != "user" {
		return apiMessages
	}
	var existing []types.ContentBlock
	if err := jsonUnmarshalBlocks(apiMessages[last].Content, &existing); err != nil {
		return apiMessages
	}
	merged, err := jsonMarshalBlocks(append(existing, blocks...))
	if err != nil {
		return apiMessages
	}
	out := make([]types.APIMessage, len(apiMessages))
	copy(out, apiMessages)
	out[last] = types.APIMessage{Role: apiMessages[last].Role, Content: merged}
	return out
}
