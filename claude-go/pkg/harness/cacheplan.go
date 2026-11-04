// cacheplan.go — 把一次 prompt 分层推导成 api.CachePlan（Moqi R-H2）。
//
// 背景：pkg/harness/layout.go 已经能回答"哪些段是稳定前缀"，pkg/api 的
// CachePlan 已经能回答"断点打在哪几个 system/message 下标上"。缺的是两者之间
// 的纯函数。没有它，布局就只是**观测**：真正发出去的请求仍由 api 层按内置启发式
// 自己猜一遍，调参时看到的命中率变化无法归因到布局上。
//
// 本文件只决定断点位置，不构造请求、不改 Client 状态，也不读取 provider 返回。
// 调用方拿到计划后自行赋给 api.Client.CachePlan。
package harness

import (
	"fmt"

	"github.com/anthropic/claude-go/pkg/api"
)

// cachePlanMessageReserve 是消息侧预留给 api 内置启发式的断点数上界。
//
// 本文件固定返回 MessageIndexes == nil（消息侧交回内置启发式），而内置在流式
// 路径上最多会补 2 个断点（见 pkg/api 的 cacheBreakpointIndexes(messages, 2)）。
// 预算必须把这 2 个算进去：provider 看到的是**总数**，只数本计划里的 system
// 下标会让"总数 <= 预算"变成一句把自己控制不了的一侧排除在外的假账。
//
// 非流式路径内置消息侧是 0 个，按 2 预留只会让可用预算更紧，不会放出超限请求 ——
// 这是有意选择更严的一侧。反过来按 0 预留，流式下就会把 2 个断点漏出预算。
const cachePlanMessageReserve = 2

// CachePlanFromLayout 从一次布局推出缓存断点计划。
//
// 参数：
//   - stable：稳定前缀，按出站 system 数组的前若干块同序排列。ID 是失效归因的
//     身份，因此与 LayoutBuilder.Build 同口径地拒绝重复 ID。
//   - dynamicCount：紧随稳定段之后的动态段个数。它不改变选点（选点只看稳定段），
//     但必须是合法计数；同时它让调用方在同一处声明"稳定段之后还有多少每轮都变的
//     内容"，避免调用方把动态段误塞进 stable 而这里无从察觉。
//   - maxBreakpoints：本次请求允许的断点总数（system + messages），必须为正、
//     不超过 provider 上限，且至少够"稳定段末尾 1 个 + 消息侧内置预留"。
//
// 返回 nil, nil 表示**不干预**：调用方应保持 api.Client.CachePlan 为 nil，让内置
// 启发式工作。返回 nil, err 表示输入/预算本身有问题，调用方必须当错误处理；
// 不能把错误当成"没有计划"继续发送 —— 那会让一个配置错误伪装成正常运行。
//
// 选点只有**一个**：稳定段末尾。理由与失效方向写在函数体内。
func CachePlanFromLayout(stable []SegmentInput, dynamicCount, maxBreakpoints int) (*api.CachePlan, error) {
	if maxBreakpoints <= 0 {
		return nil, fmt.Errorf(
			"harness: cache plan 断点预算必须为正，实际 %d：零/负预算下产出的计划要么没有断点、要么无法约束总数，发出去只会得到一个语义不明的上游错误",
			maxBreakpoints)
	}
	// 复用 layout.go 的 provider 硬上限，不在这里另写一个 4。预算高于它是调用方
	// 的理解错误：本函数最多也只能合法地花到上限，静默截断会让调用方以为预算被
	// 完整使用。宁可现在报错，也不要等到上游 400 才暴露。
	if maxBreakpoints > DefaultMaxBreakpoints {
		return nil, fmt.Errorf(
			"harness: cache plan 断点预算 %d 超过 provider 上限 %d：超出部分无法合法使用，调大预算不会增加可打点数",
			maxBreakpoints, DefaultMaxBreakpoints)
	}
	if dynamicCount < 0 {
		return nil, fmt.Errorf(
			"harness: cache plan 动态段个数不能为负，实际 %d：负数说明布局计数已经算错，按它推出来的下标不可信",
			dynamicCount)
	}

	// 重复 ID 与 LayoutBuilder.Build 同口径拒绝。断点位置本身不依赖 ID，但这份
	// 布局的失效归因依赖它；让带重复 ID 的布局流到出站请求，等于先制造一份
	// "看起来正常、归因却会张冠李戴"的观测。错误必须在源头暴露。
	seen := make(map[string]struct{}, len(stable))
	for _, s := range stable {
		if _, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf(
				"harness: cache plan 稳定段 ID %q 重复：失效归因按 ID 判定，重复 ID 会让归因失真", s.ID)
		}
		seen[s.ID] = struct{}{}
	}

	// 没有稳定段 = 没有可缓存的前缀。此时返回 nil（不干预），而不是
	// &api.CachePlan{SystemIndexes: []int{}}：后者是"system 侧明确不要断点"，
	// 会把 api 内置启发式一起关掉，语义完全不同。前者让内置启发式按它自己的
	// 判断继续工作。
	if len(stable) == 0 {
		return nil, nil
	}

	// 预算要同时容纳"稳定段末尾的 1 个"和消息侧由内置补上的预留。预留不够时
	// 返回错误而不是硬塞：硬塞出来的计划总数会超出调用方给的预算，调用方却从
	// 返回值里看不出这一点。
	if maxBreakpoints < 1+cachePlanMessageReserve {
		return nil, fmt.Errorf(
			"harness: cache plan 断点预算 %d 不足：稳定段末尾需要 1 个，消息侧内置启发式需预留 %d 个，至少需要 %d 个",
			maxBreakpoints, cachePlanMessageReserve, 1+cachePlanMessageReserve)
	}

	// 选点：稳定段的最后一块。
	//
	//   - 断点缓存的是它**之前（含自身）**的全部前缀，打在最后一个稳定段上就已
	//     覆盖整个稳定前缀；再往前放（比如块 0）不会扩大覆盖范围，只会多一个
	//     更短、更容易与主缓存条目重复的条目。
	//   - 绝对不能落到动态段上：动态段每轮都变，断点落上去等于每轮换缓存键，
	//     缓存永远不会命中。这个错误不会报错、不会降级，只会在账单上体现，
	//     所以下标必须由 len(stable) 算出并严格小于动态段起点 len(stable)。
	//   - 本函数不在预算内"撒"更多断点：多个断点只有在稳定子段各自有独立失效
	//     节奏时才划算，而当前布局没有表达这种节奏的信号。把预算花满只会把
	//     选择权从"有依据的布局"换回"拍脑袋"。
	stableEnd := len(stable)
	breakpoint := stableEnd - 1

	return &api.CachePlan{
		// 这里必须是单元素的具体下标，不能是 nil：nil 会让 api 层回退到它自己的
		// 内置选点，本函数算出的稳定段边界就传不出去。
		SystemIndexes: []int{breakpoint},
		// MessageIndexes 一律 nil（不是空切片），把消息侧交回内置启发式。
		//
		// 为什么不能返回空切片：api.CachePlan 的 nil 与 []int{} 语义不同 ——
		// nil = "消息侧用内置启发式"，[]int{} = "消息侧一个断点都不要"。本函数
		// 没有消息内容（不知道条数、角色、最后一条是否带内容块），无法负责任地
		// 选点；返回空切片会静默关掉 api 内置的两级消息断点，历史增长部分重新
		// 按未命中计费，而请求依旧成功、没有任何报错。
		MessageIndexes: nil,
	}, nil
}
