// cacheplan_wire.go — 把 CachePlanFromLayout 接到"每个执行回合"上（Moqi R-H2）。
//
// CachePlanFromLayout 只回答"给定布局应该把断点打在哪"。真正出站时还需要两件事：
//   - 本轮到底要不要干预（预算是 0 表示不干预，不是"零个断点"）；
//   - trace 里记的断点下标必须与出站请求用的是**同一份**，否则观测与账单说的
//     不是一回事：调参时看到的命中率变化无法归因到任何真实断点上。
//
// 本文件只做这层转发，不复制选点逻辑。任何"这里再算一遍下标"的实现都会在
// CachePlanFromLayout 演进时悄悄分叉，所以返回值直接来自它返回的 plan。
package harness

import (
	"github.com/anthropic/claude-go/pkg/api"
)

// TurnCachePlan 推出本轮出站计划，并返回要写进 trace 的实际 system 断点下标。
// maxBreakpoints <= 0 表示本轮不干预：返回 (nil, nil, nil)。
//
// 参数与 CachePlanFromLayout 同口径（stable / dynamicCount / maxBreakpoints），
// 本函数不改变它的任何语义。
//
// 错误一律原样透传：预算非法、超过 provider 上限、重复 ID、动态段计数为负，
// 都是**配置错误**。把错误吞成"没有计划"会让这些配置悄悄退化成内置启发式 ——
// 请求照常成功，只是缓存收益消失了，没有任何报错或指标能提示调用方。
//
// 为什么 maxBreakpoints <= 0 要在这里先短路：CachePlanFromLayout 把非正预算
// 定义为错误（它无法产出有意义的计划），但在回合编排的语境里"预算 0"是一个
// 合法配置，含义是"本轮不要用布局干预缓存"，应当保持 api.Client.CachePlan 为
// nil 交给内置启发式。这个"不干预"的判断属于调用约定，不能下沉去改既有函数的语义
// （它已有 20+ 测试锁住"非正预算返回错误"）。
func TurnCachePlan(stable []SegmentInput, dynamicCount, maxBreakpoints int) (*api.CachePlan, []int, error) {
	if maxBreakpoints <= 0 {
		return nil, nil, nil
	}

	plan, err := CachePlanFromLayout(stable, dynamicCount, maxBreakpoints)
	if err != nil {
		return nil, nil, err
	}
	if plan == nil {
		// 没有稳定段：没有可缓存的前缀，交回内置启发式。"不干预"不是错误。
		return nil, nil, nil
	}

	// trace 用的下标从 plan.SystemIndexes 复制，而不是另算一遍 stableEnd-1：
	// 选点规则的唯一来源是 CachePlanFromLayout，两者分头计算必然在规则演进时
	// 产生"trace 说 2、请求打 1"这种只在账单上可见的缺陷。
	// 返回副本而非同一底层数组，是为了让 trace 的消费方（可能长期持有、排序或
	// 追加）不会反过来修改出站 plan；副本内容仍逐元素来自同一来源。
	indexes := make([]int, len(plan.SystemIndexes))
	copy(indexes, plan.SystemIndexes)
	return plan, indexes, nil
}
