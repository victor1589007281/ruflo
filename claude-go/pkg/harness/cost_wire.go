package harness

// cost_wire.go — 把 api 层的回退观测与成本账本接起来（Moqi R-H17）。
//
// 两个函数各自只做"形状转换"，不做策略：
//
//   - TurnCostEntry 把 TurnRequest 的四维身份 + 一次调用的 usage 组装成账本条目。
//     它**不**猜 PriceUnknown —— 价格是否可用是账本（有没有价目函数、模型在不在
//     表里、usage 是否采到）的判断，调用方在这里越俎代庖，就会把"漏配价目表"
//     伪装成"调用方声明免费"。
//   - RecordTurnFallbacks 把 api.FallbackInfo 转成账本事件并记录，返回可写入
//     TurnTrace.FallbackChain 的链。账本负责去重/归一，这里只保证**顺序与输入
//     一致** —— trace 里的回退链是 A→B→C 的因果顺序，排序会毁掉它。
//
// 为什么转换层放在 harness 而不是让 api 直接写账本：api 是通用客户端，不该知道
// "turn / session / owner" 这套业务归集键；账本也不该 import api 的传输层类型。
// 这个文件是两者之间唯一的依赖方向 (harness → api)，与 cacheplan.go 一致。

import (
	"fmt"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// TurnCostEntry 组装一轮的账本条目。role 空按 dialogue 归一由账本负责。
//
// 刻意不设置 PriceUnknown：这里没有足够信息判断"这个模型的价格可信吗"。把
// PriceUnknown 留给真正知道例外的人（别名、促销、供应商改口径），其余交回
// 账本的价目函数 —— 未知模型会被记成 CostPricingUnknownModel，而不是一个
// 看起来合理的 0。
func TurnCostEntry(req TurnRequest, role, model string, usage types.Usage, usageKnown bool) CostEntry {
	return CostEntry{
		CostKey: CostKey{
			Role:      role,
			TurnID:    req.TurnID,
			SessionID: req.SessionID,
			OwnerID:   req.OwnerID,
		},
		Model:      model,
		Usage:      usage,
		UsageKnown: usageKnown,
	}
}

// RecordTurnFallbacks 把 api 层的回退观测转成账本事件并记录，返回可写入 trace 的链。
//
// l 为 nil 或 infos 为空时返回 nil：没有账本/没有回退就是"什么都不该记"，返回空
// 切片会让调用方误以为"有一个空的回退链"（序列化后可能变成 [] 而不是省略）。
func RecordTurnFallbacks(l *CostLedger, key CostKey, infos []api.FallbackInfo) []FallbackEvent {
	if l == nil || len(infos) == 0 {
		return nil
	}
	out := make([]FallbackEvent, 0, len(infos))
	for _, info := range infos {
		ev := FallbackEvent{
			CostKey:   key,
			FromModel: info.FromModel,
			ToModel:   info.ToModel,
			Reason:    fallbackEventReason(info),
		}
		l.RecordFallback(ev)
		out = append(out, ev)
	}
	return out
}

// fallbackEventReason 决定账本里保存的回退理由。
//
// 优先级: 调用方给的可读原因 > 状态码派生的稳定名 > 兜底 "fallback"。
// 状态码为 0 表示非 HTTP 原因（连接失败等），此时没有可用的具体信息；
// 写空串会让审计侧无法区分"没原因"与"字段没填"，所以用固定词兜底。
func fallbackEventReason(info api.FallbackInfo) string {
	if info.Reason != "" {
		return info.Reason
	}
	if info.StatusCode > 0 {
		return fmt.Sprintf("upstream_%d", info.StatusCode)
	}
	return "fallback"
}
