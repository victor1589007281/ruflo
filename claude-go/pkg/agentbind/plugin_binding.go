// plugin_binding.go —— 插件包的**装配读数** (plugin 运行时消费方的报告半边)。
//
// ---------------------------------------------------------------------------
// 它与 bindPlugin 是两回事
// ---------------------------------------------------------------------------
//
// asset_binding.go 的 bindPlugin 读的是**一份 agent 资产里的一个键**
// (`runtime: plugin`, 依旧没有消费方, 照旧 BindingNoConsumer)。这里读的是
// **一整个展开后的插件包** (pkg/pluginrun.Bundle): CLI `run --plugin` /
// agent frontmatter `plugins:` 真的会把它的成员装进运行时, 所以成员资产
// 第一次有了"wired 接到哪"的答案。
//
// 报告类型与 AssetBinding 分开定型而不是复用: AssetBinding 的字段
// (ToolProfile / PermissionMode / MaxTurns / ...) 是 agent 声明的取值域,
// 插件一个都没有 —— 塞进去会长出一片永远为零的字段, 而"字段在但恒为零"
// 与"装配成功"在读数上不可区分。
package agentbind

import (
	"strconv"

	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/pluginrun"
)

// PluginBinding 一个已展开插件包的装配读数 (与 AssetBinding 同一套 Binding 词汇)。
type PluginBinding struct {
	// Bundle 被装配的插件包 (展开结果); nil 表示没展开过。
	Bundle *pluginrun.Bundle `json:"-"`
	// Name 插件名 (即使 Bundle 为 nil 也尽量填上, 供留痕用)。
	Name string `json:"name"`
	// Active 清单资产是否可正常使用 (shadow/archived 为 false)。
	Active bool `json:"active"`
	// Bindings 逐成员读数: 每个成员资产一条, 按 Facet 排序。
	Bindings []Binding `json:"bindings,omitempty"`
}

// BindPluginAsset 把展开的插件包翻译成装配读数: **每个成员资产一条 Binding**。
//
// 有消费方的成员 (agents/skills/hooks) 报 BindingWired 且**点名消费方在哪**;
// 没有消费方的 (tools/workflows) 报 BindingNoConsumer 并写明原因 —— 与本文件头
// 三条纪律的第 1 条同一条: 没有"解析成功"这个中间态可以假装。
//
// Consumer 串写的是 CLI (cmd/claude-go) 两条装配路径上**真实的**接缝名;
// 别改成"未来将接"的字样 —— 读数的价值就是它说的是现状。
func BindPluginAsset(b *pluginrun.Bundle) *PluginBinding {
	if b == nil {
		return &PluginBinding{}
	}
	out := &PluginBinding{Bundle: b, Name: b.Name}
	if b.Plugin != nil {
		out.Active = b.Plugin.IsActive()
	}
	wire := func(facet string, names []string, consumer string) {
		for _, n := range names {
			out.Bindings = append(out.Bindings, Binding{
				Facet: facet, Status: BindingWired,
				Declared: []string{n}, Resolved: []string{n},
				Consumer: consumer,
			})
		}
	}
	noConsumer := func(facet string, names []string, note string) {
		for _, n := range names {
			out.Bindings = append(out.Bindings, Binding{
				Facet: facet, Status: BindingNoConsumer,
				Declared: []string{n}, Note: note,
			})
		}
	}
	wire("agents", memberNames(b.Agents),
		"agent.SetAgentCatalog 合并副本 (cmd/claude-go run --plugin 展开; 原目录不被改污)")
	wire("skills", memberNames(b.Skills),
		`skills.Registry.LoadFromDirs(source="plugin", rank 150)`)
	wire("hooks", memberNames(b.Hooks),
		`hooks.Runner.InstallHookGroup("plugin:`+b.Name+`"); `+
			"run 装配路径同时并入 hookConfigs, 子代理的 nested runner 经 deps.hookConfigs 继承")
	noConsumer("tools", memberNames(b.Tools),
		"本版本没有插件 tools 成员的消费方 (工具池只收独立 tool 资产), 这部分成员不生效")
	noConsumer("workflows", memberNames(b.Workflows),
		"本版本没有插件 workflows 成员的消费方, 这部分成员不生效")
	sortBindings(out.Bindings)
	return out
}

// Warnings 挑出这一份读数里**需要人看一眼**的项: 没有消费方的成员 +
// 展开问题 (Bundle.Problems)。与 AssetBinding.Warnings 同一条纪律:
// 全量读数在 Bindings, 这里只挑要处置的, 否则正常路径的噪声会把真问题淹掉。
func (b *PluginBinding) Warnings() []string {
	if b == nil {
		return nil
	}
	if b.Bundle == nil {
		return []string{"插件包没展开 (Bundle 为 nil)"}
	}
	var out []string
	if !b.Active {
		status := ""
		if b.Bundle.Plugin != nil {
			status = b.Bundle.Plugin.Status
		}
		out = append(out, "插件 "+b.Name+" 的状态是 "+status+" (非 active): 其成员资产照样会被展开, 启用与否由调用方决定")
	}
	for _, bd := range b.Bindings {
		switch bd.Status {
		case BindingInvalidValue, BindingUnknownName, BindingNoConsumer:
			out = append(out, "插件 "+b.Name+" 的 "+bd.Facet+" 成员 "+joinNames(bd.Declared)+": "+bd.Note)
		}
	}
	for _, p := range b.Bundle.Problems {
		out = append(out, "插件 "+b.Name+" 的展开问题: "+p)
	}
	return out
}

// memberNames 取成员资产的名字切片 (成员在 Bundle 里已按名字排序)。
func memberNames(in []*asset.Asset) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if a != nil {
			out = append(out, a.Name)
		}
	}
	return out
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "(未命名)"
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += strconv.Quote(n)
	}
	return out
}
