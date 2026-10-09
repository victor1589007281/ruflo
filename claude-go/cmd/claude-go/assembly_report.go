// **声明侧**的翻译: 把平台的装配描述符翻成 pkg/assembly.Declaration。
//
// 采集侧 (从活引擎读实际装配) 在 21.8.12 P2 搬去了 pkg/engine/assembly_report.go ——
// 理由见那个文件头: 子代理的派生入口有两条 (飞书/团队 与 CLI), 两份采集端迟早分家。
// 本文件剩下的这一半是**平台特有的**: 描述符是平台签发的概念, 引擎不认识它,
// 只有入口才拿得到, 所以它留在入口这一侧。
package main

import (
	"github.com/anthropic/claude-go/pkg/assembly"
	"github.com/anthropic/claude-go/pkg/feishu"
)

// declarationVersionOf 取本次实际读到的那一版描述符的版本号 (没有描述符 → 空串)。
//
// 单独一个函数是为了让调用点读起来是"带上了版本", 而不是"又读了配置里的一个字段":
// 自报里的版本号回答的是"我照哪张图纸装的", 与"装备装成了什么"是两件事, 采集端
// (pkg/engine) 明确不碰它 —— 所以这唯一的取值点必须在入口这一侧。
func declarationVersionOf(cfg *feishu.JSONConfig) string {
	if cfg == nil || cfg.AssemblyProfile == nil {
		return ""
	}
	return cfg.AssemblyProfile.Version
}

// declarationFromProfile 把平台的**装配描述符**翻成声明侧。
//
// 一个字都不许补: 描述符里没写的段就是"未声明", 不参与对账。特别地, 不能拿
// cfg.AllowedTools / cfg.PermissionMode / Enable* 开关去"推断"一份声明 ——
// 那些是**生效路径的输入**, 不是**平台的声明**。拿它们当声明, 对账就变成了
// "配置与它自己比", 恒等; 而真正要照的那条缝 (平台要的 vs 实际装上的) 恰好
// 被这个恒等盖住了。
//
// 返回 nil 表示"没有声明可对" —— 对账把它如实报成"未对账", 而不是假装一致。
// 一份空的描述符 (字段全空) 与没有描述符也是两回事, 前者是一份**什么都没要求**
// 的声明, 后者是平台根本没签发。
func declarationFromProfile(cfg *feishu.JSONConfig) *assembly.Declaration {
	if cfg == nil || cfg.AssemblyProfile == nil {
		return nil
	}
	p := cfg.AssemblyProfile
	d := &assembly.Declaration{
		// nil / 空表之分原样保留: 描述符里 tools 是 null 还是 [], 正是
		// "没要求工具面"与"要求拒绝一切工具"的分界, 压平就分不出来了。
		Tools:          p.Tools,
		Components:     p.Components,
		Model:          p.Model,
		PermissionMode: p.PermissionMode,
	}
	// Index 由数组下标决定, 与采集侧从 Runner 拿到的编号同源 (都是配置数组的下标)。
	// 若哪天描述符允许手填 index, 这两个口径就会分家, 对账会整片报"未装载" ——
	// 那是幻觉, 不是发现。
	for i, h := range p.Hooks {
		d.ExternalHooks = append(d.ExternalHooks, assembly.ExternalHook{
			Event: h.Event, Type: h.Type, Index: i,
		})
	}
	return d
}
