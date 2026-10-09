// mode_rank.go —— 权限档的**宽严序**, 用于回答一句很具体的话:
// "把外层的档换成声明里的那一档, 是不是一次放宽?"
//
// 为什么需要它: 21.8.12 P2 让 agent 资产能声明自己的 permission-mode, 而资产目录
// 是一个**较低权限的作者也写得进去**的地方 (state/project/user 三层, `evo` 与面板
// 都能落盘)。如果声明可以无条件生效, 那么一份 .md 里写 `permission-mode: bypass`
// 就是一个从文件到权限的提升路径 —— 而且是静默的: 子代理照跑, 只是不再问任何事。
//
// 本仓对这件事的既有口径是 "约束按构造只会收窄" (engine.Config.ApplyConstraints)。
// 这里是同一条纪律在权限档上的落实: 声明**可以**让子代理更严, **不可以**让它更宽。
//
// ---------------------------------------------------------------------------
// 与 modeDecision 是一对, 改那个必须回来看这里
// ---------------------------------------------------------------------------
//
// 序的依据不是"名字听起来多严", 而是 modeDecision (permissions.go) 的实际分支:
// 一个写入类工具在这个档下是**自动放行 / 还是问 / 还是直接拒**。排序越靠后 = 放行
// 得越多。六个档在这个维度上的位置如下 —— 抄自 modeDecision 的实际 switch:

//	 plan        只读放行, 写入**拒绝**        ← 最严
//	 dontAsk     只读放行, 写入转成拒绝
//	 default     只读放行, 写入询问
//	 auto        只读放行, 写入询问            ← 与 default **逐字节同分支**
//	 acceptEdits 只读放行, 文件编辑放行, 其余询问
//	 bypass      一律放行                       ← 最宽
//
// `default` 与 `auto` 同分不是笔误: modeDecision 里 PermissionModeAuto 与落到
// default 的那条分支产出完全相同的 PermissionResult (只读 allow / 其余 ask), 两者
// 只在**名字**上不同。给它们不同的分会让"把 default 换成 auto"看起来是一次放宽,
// 于是本函数会拒掉一个实际零变化的声明 —— 一条恒真的闸比没有闸更坏, 因为它会让
// 人以为这里有防护。
//
// 反过来, 哪天 modeDecision 把 auto 改成"写入自动放行", 这个表必须同步 —— 否则
// 那道防提升的闸会在最需要它的时候失效。两处的注释互相指名, 就是为了让改的人看到。
package permissions

import "github.com/anthropic/claude-go/pkg/types"

// modeRank 各档的宽严分 (越大越宽)。未列出的取值返回 false —— 调用方必须自己
// 先过 types.ValidPermissionMode, 拿不到一个"看起来像 0"的默认分。
var modeRank = map[types.PermissionMode]int{
	types.PermissionModePlan:        0,
	types.PermissionModeDontAsk:     1,
	types.PermissionModeDefault:     2,
	types.PermissionModeAuto:        2,
	types.PermissionModeAcceptEdits: 3,
	types.PermissionModeBypass:      4,
}

// ModeRank 返回某档的宽严分; ok 为 false 表示这不是一个已知的档。
func ModeRank(m types.PermissionMode) (int, bool) {
	r, ok := modeRank[m]
	return r, ok
}

// Widens checks 把外层档换成 inner 是不是一次放宽 (inner 比 outer 宽)。
//
// 两个档只要有一个不认识就返回 true —— fail-closed 的方向: 判不出来的时候
// 按"这是一次放宽"处理, 于是声明被拒、保持外层现状。反过来 (判不出来就放行)
// 会让一个拼错的档变成一次没人看得见的权限变更。
//
// 相等不算放宽: 相同分返回 false。
func Widens(outer, inner types.PermissionMode) bool {
	ro, ok1 := ModeRank(outer)
	ri, ok2 := ModeRank(inner)
	if !ok1 || !ok2 {
		return true
	}
	return ri > ro
}
