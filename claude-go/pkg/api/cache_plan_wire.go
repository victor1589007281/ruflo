package api

// WithCachePlan 返回带缓存计划的浅副本；plan == nil 时返回原 client（零开销、不改原对象）。
//
// 为什么要克隆而不是原地赋值：Client 会被同一个进程里的多个 run 共享（engine 的
// 相位路由、并发的会话轮次都持有同一份装配好的 Client）。原地写 CachePlan 会让
// A 轮算出的稳定段断点滞留在对象上，随后被 B 轮的请求带走 —— 断点位置与 B 轮
// 的 prompt 布局毫无关系。这种错误不会报错，只会让缓存命中率无声地漂移，正是
// R-H2 要消灭的那类缺陷。
//
// 为什么负数/零值计划不做特殊处理：本函数的职责只是"设置"，语义校验在
// harness.CachePlanFromLayout 里完成。这里若悄悄把非法计划当成 nil，配置错误
// 就会被伪装成"没启用缓存"，调用方永远看不到。
//
// 用结构体值拷贝（clone := *c）而非逐字段罗列：Client 字段一直在长，逐字段
// 复制必然漂移（WithModel 就漏过 CachePlan，见其注释）。值拷贝天然把 Http client、
// 限流器 Guard、拦截器切片等全部原样共享 —— 克隆只覆盖计划这一个字段。
//
// nil 时直接返回 c 而非 &(clone)：保证"未启用缓存计划"这条路径上不产生新对象，
// 调用方拿到的就是原来那个指针，出站行为逐字节不变。返回副本也能做到行为不变，
// 但那会让"是否启用"在指针身份上不可观测，调用方无法用 nil 判断来避免无谓的分配。
func (c *Client) WithCachePlan(plan *CachePlan) *Client {
	if plan == nil {
		return c
	}
	clone := *c
	clone.CachePlan = plan
	return &clone
}
