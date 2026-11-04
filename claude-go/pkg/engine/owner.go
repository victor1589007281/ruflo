// owner.go — owner（租户/用户）在引擎侧的落地（Moqi R-H6）。
//
// # 为什么读 ctx 而不是只读 Config
//
// 引擎是**会话级复用**的：同一个 *QueryEngine 会被团队路径的多个并发 runner 共用
// （见 inject.go 文件头）。把 owner 写进 Config 字段，等于让并发 run 互相覆盖 ——
// 表现是"A 用户的检索结果进了 B 用户的请求"，而两边都看不出异常。
// ctx 天然按 run 隔离、不需要任何锁，所以优先级是 ctx > Config：
//
//  1. ctx 里显式写过 owner（types.HasOwner）→ 用它。这是鉴权结果透传的正路。
//  2. ctx 没写过但 Config.OwnerID 非空 → 用它。给"一个引擎服务一个固定租户"
//     的部署形态留口，也兼容装配方只填 Config 的历史写法。
//  3. 两者都空 → types.DefaultOwnerID（单用户旧行为，不是"无主"）。
//
// 第 3 条选具体值而不是空串的理由见 types/owner.go 文件头：空串会让跨 owner 隔离
// 的断言在两个空 owner 之间恒真。
package engine

import (
	"context"

	"github.com/anthropic/claude-go/pkg/types"
)

// ownerFrom 解析本次 run 的 owner。
//
// 三个分支的顺序就是契约：ctx 优先于 Config。反过来会让"ENGINE 配了租户 A、
// 但这次请求是租户 B"的场景静默串号 —— 那不是配置错误，是越权。
func (e *QueryEngine) ownerFrom(ctx context.Context) string {
	if types.HasOwner(ctx) {
		return types.OwnerFromContext(ctx)
	}
	if e != nil && e.Config.OwnerID != "" {
		return types.NormalizeOwner(e.Config.OwnerID)
	}
	return types.DefaultOwnerID
}
