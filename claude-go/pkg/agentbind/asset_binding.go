// asset_binding.go —— **组装器**: 把一份 agent 资产声明装配成一个可运行的子代理
// (21.8.12 P2)。
//
// ---------------------------------------------------------------------------
// 它解决的是什么
// ---------------------------------------------------------------------------
//
// P1 让六种资产能被解析 (asset.Asset + frontmatter), P5 让 `subagent_type` 能查到
// 那份资产。但"查得到"与"装得上"之间还差一层: 资产 .md 里写的 tools / hooks /
// skills / model / permission-mode, **没有任何东西把它们变成运行时**。
//
// 这就是 21.8.7 记的那个组装缺口, 也是本仓反复出现的那一类缺陷的形态 —— 声明与
// 生效之间的缝只存在于运行期, 进程起来后没人答得出"那份声明到底装上了没有"。
// 所以本文件做两件事, 缺一不可:
//
//	A. BindAgentAsset —— 按声明从**池**里装配出一个 AssetBinding (四类绑定 + 档位);
//	B. AssemblyDeclaration —— 把同一份声明翻成 assembly.Declaration, 交给
//	   pkg/assembly.Reconcile 与运行期实际对账。
//
// **只做 A 不做 B = 多了一条静默失败的路径**; 只做 B 不做 A = 一份对不上的账。
//
// ---------------------------------------------------------------------------
// 三条纪律 (P2 是本批唯一从零建的组件, 这三条是它的全部要点)
// ---------------------------------------------------------------------------
//
//  1. **每一条绑定都必须点名它的消费方在哪。** 声明被解析了而没人读, 与"写错了
//     键名"在下游完全一样 —— 都是静默无效。所以 BindingStatus 里没有"解析成功"
//     这一档: 要么 wired (接上了, 且指出接到哪), 要么 no_consumer (本仓根本没有
//     读它的地方, 如实说出来), 没有中间态可以假装。
//  2. **降级必须可见。** 档位拼错、权限档不认识、声明了池里没有的技能 —— 一律
//     BindingInvalidValue / BindingUnknownName 留痕, **不许静默回落到某个默认值**。
//     builtin.NormalizeToolProfile 对不认识的串恒返回 chat, 那是本仓最接近"静默
//     权限变更"的一处, 组装器必须比它更早地把这件事拦住。
//  3. **不猜。** 名字匹配一律 exact; 池里查不到就是查不到。
//
// ---------------------------------------------------------------------------
// 关于 plugin: 运行时消费方已补上 (pkg/pluginrun + CLI run --plugin), 边界如下
// ---------------------------------------------------------------------------
//
// 方案把 plugin 与 hook/skill/tool 并列为四类绑定之一。KindPlugin 此前只有解析
// 没有消费 (P1 登记进 asset.Set, 没有任何地方按它装配运行时), 这里曾如实记为
// "本仓没有 plugin 运行时"。现在有了: pkg/pluginrun.Expand 把插件资产展开成
// bundle 目录里的成员资产 (agents/skills/hooks/tools/workflows), CLI 的
// `run --plugin <name>` 与 agent 资产 frontmatter 的 `plugins:` 把它们装进运行时 ——
// agents 合并进目录副本 (agent.SetAgentCatalog), skills 进 skills.Registry
// (source="plugin", rank 150), hooks 进 hooks.Runner 的可装卸组 ("plugin:<name>")。
// 这条链路的装配读数是 plugin_binding.go 的 BindPluginAsset。
//
// 仍然没有消费方的部分, 照旧如实报 BindingNoConsumer, 不假装:
//   - 插件的 tools / workflows 成员 (本版本没有把它们装进运行时的地方);
//   - PLUGIN.md 声明的 commands: / mcp: 键 (CommandTypePrompt 无运行时;
//     dynmcp 在 CLI 从未初始化) —— 由 pluginrun.Bundle.Problems 留痕;
//   - agent 资产里的 `runtime: plugin` 键 (下方 bindPlugin): 那是"这个 agent
//     想当插件跑"的声明, 与插件包是两回事, 依旧没有读它的地方。
//
// ---------------------------------------------------------------------------
// 为什么是**独立包**, 而不是住在 pkg/agent 里
// ---------------------------------------------------------------------------
//
// 组装器必须能用 builtin.ValidToolProfile / NormalizeToolProfile —— 档位名与它的
// 合法取值域只有那一处定义, 在这里另抄一张表就是第二个真相源 (而档位判错会静默
// 改变一个子代理的工具面, 那正是本文件要防的事)。但 pkg/agent **不能** import
// pkg/tool/builtin: 依赖链 builtin → pkg/evolution/govern → pkg/agent 已经存在,
// 反向走一步就是 import cycle。
//
// 所以本文件住在一个只依赖 (asset, assembly, builtin, types, agent) 的叶子里。
// 它是**纯装配**: 读声明、查池、产出读数, 不注册任何东西进运行时 —— 真正把工具/
// hook/skill 装上的是调用方 (两条派生路径的装配方式本就不同)。这让本包对 pkg/agent
// 的依赖是单向的、纯粹的"查一下有没有这个名字"。
package agentbind

import (
	"sort"
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/assembly"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

// BindingStatus 一类绑定的结局。
//
// 刻意**没有** "parsed" / "ok" 这类中性档: 一份解析成功但没人读的声明与没写完全
// 等价, 给它一个中性档只会让上报看起来是绿的。四档各自对应一个确定的处置。
type BindingStatus string

const (
	// BindingUnset 资产没声明这一面。这是**正常状态** (除了正文, 每个字段都可选),
	// 与"声明了但没接上"必须分开 —— 否则每份最小资产都会报一堆假问题。
	BindingUnset BindingStatus = "unset"
	// BindingWired 声明了, 且真的接上了 (接到哪个消费方见 Binding.Consumer)。
	BindingWired BindingStatus = "wired"
	// BindingUnknownName 声明里点了名, 但池里查不到。后果是**静默少给** ——
	// 技能不列出来, hook 不触发, 而运行一切正常。
	BindingUnknownName BindingStatus = "unknown_name"
	// BindingInvalidValue 取值非法 (档位拼错 / 权限档不认识)。必须留痕:
	// 静默回落成某个合法档位是一次没人察觉得到的权限变更。
	BindingInvalidValue BindingStatus = "invalid_value"
	// BindingNoConsumer 解析出来了, 但本仓没有任何消费方读它。**这是本仓的缺口,
	// 不是资产作者的错**, 所以它跟着一句"替代路径"一起报出来。
	BindingNoConsumer BindingStatus = "no_consumer"
)

// Binding 某一面绑定的装配结果。
type Binding struct {
	// Facet 面名 (tool-profile / skills / tools / disallowed-tools / hooks / plugin /
	// model / permission-mode / max-turns)。
	Facet string `json:"facet"`
	// Status 结局。
	Status BindingStatus `json:"status"`
	// Declared 声明里写了哪些名字 (排序去重)。hooks 这类内联声明的面用
	// "Event:type" 形状的名字。
	Declared []string `json:"declared,omitempty"`
	// Resolved 其中真正在池里找到的那些 (wired 时与 Declared 等长)。
	Resolved []string `json:"resolved,omitempty"`
	// Consumer 这一面接上的**消费方在哪** (函数名/路径)。没有消费方时为空。
	//
	// 这一项是本类型的全部意义: "声明了但没人读"之所以是本仓最贵的缺陷, 是因为
	// 它在代码里看起来与"接好了"一模一样。把消费方的位置写进读数里, 才能让人
	// 一眼看出这条绑定是不是真的通电。
	Consumer string `json:"consumer,omitempty"`
	// Note 一句话说明 (为什么是这个结局 / 没有消费方时该走哪条路)。
	Note string `json:"note,omitempty"`
}

// AssetBinding 一份 agent 资产的装配结果。
//
// 它是**只读读数**: 描述这份资产按声明应该得到什么、其中哪些真的接上了。
// 真正把东西注册进运行时的是调用方 (pkg/feishu / cmd/claude-go 各自的装配代码),
// 因为两条路径的装配方式本就不同 (前者经 ConstraintSet, 后者直接注册档位) ——
// 让本文件去改运行时, 就得为两条路径各写一套, 那正是"两套装配代码迟早分家"。
type AssetBinding struct {
	// Asset 被装配的那份资产; nil 表示名字没查到 (此时其余字段全空)。
	Asset *asset.Asset `json:"-"`
	// Name 请求的名字 (即使没查到也保留, 供留痕用)。
	Name string `json:"name"`
	// Active 资产是否可正常使用 (shadow/archived 为 false)。
	//
	// 非 active 时**一律不装配**: 影子资产被模型直接调用等于绕过全部灰度机制,
	// 而 P5 的清单已经把 shadow 排除在外 —— 查表这一侧必须同样排除, 否则
	// "清单里没有它"与"它不能在运行时被激活"就成了两句互相矛盾的话。
	Active bool `json:"active"`
	// Prompt 资产正文 (进系统提示词的 append 段)。
	Prompt string `json:"-"`
	// Bindings 逐面读数 (按 Facet 名排序)。
	Bindings []Binding `json:"bindings,omitempty"`

	// ToolProfile / HasToolProfile 资产声明的工具档位。
	ToolProfile    builtin.ToolProfile `json:"-"`
	HasToolProfile bool                `json:"has_tool_profile"`
	// Model 声明的模型 (空 = 跟随外层)。
	Model string `json:"model,omitempty"`
	// MaxTurns 声明的回合上限 (0 = 不限制 / 跟随外层)。
	MaxTurns int `json:"max_turns,omitempty"`
	// PermissionMode 声明的权限档; HasPermissionMode 为 false 时本字段无意义。
	PermissionMode    types.PermissionMode `json:"-"`
	HasPermissionMode bool                 `json:"has_permission_mode"`

	// Skills 声明解析出的技能名 (已与池对过账的那个子集)。
	Skills []string `json:"-"`
	// Tools / DisallowedTools 声明的工具名。
	//
	// ★ 必须与 HasTools / HasDisallowedTools 配对读, 光看切片是不够的:
	// `tools: []` 是**显式空白名单** (fail-closed, 拒绝一切工具), 而它解析出来的
	// 切片与"根本没写 tools:"**长度一样都是 0**。少了存在性这一位, "我故意什么都没给"
	// 与"我什么都没限制"在下游是同一个分支 —— 一份 fail-closed 声明会静默退化成
	// 完全放行。这个坑在解析层 (asset.MetaList) 埋过一次, 别在读数层再埋一次。
	Tools               []string `json:"-"`
	HasTools            bool     `json:"has_tools"`
	DisallowedTools     []string `json:"-"`
	HasDisallowedTools  bool     `json:"has_disallowed_tools"`
	// Hooks 声明解析出的外部 hook (可直接喂给 hooks.NewRunner)。
	Hooks []types.HookConfig `json:"-"`
}

// AssemblyDeclaration 把这份装配结果翻成**声明侧**, 供对账使用。
//
// 返回 nil 表示"这份资产什么都没声明" —— 与"没有资产"是两回事, 对账要把它们分开:
// 前者是一份什么都不要的声明 (照跑, 零差异), 后者是根本没有声明可对 (如实报"未对账")。
//
// 刻意**只翻能翻的**:
//   - Tools 只认显式的 `tools` 白名单, DisallowedTools 只认**原文**的
//     `disallowed-tools`。两者都逐字搬, 绝不"互相换算": 把黑名单换成白名单需要拿它去
//     减**实际的**注册表, 那会让声明依赖实际 —— 正是 pkg/assembly 包注释里点名要消灭
//     的自比自。黑名单的落地效果另有可见处 (报告实际侧 Hidden), 而"声明有没有走到
//     生效黑名单"由 reconcileDisallowed 的第二关回答。
//   - Components 不翻: 前端优化组件由 config 的 enable* 开关决定, 资产声明它们
//     会是第二套真相源。
func (b *AssetBinding) AssemblyDeclaration() *assembly.Declaration {
	if b == nil || b.Asset == nil || !b.Active {
		return nil
	}
	d := &assembly.Declaration{
		Model:          b.Model,
		PermissionMode: "",
	}
	if b.HasPermissionMode {
		d.PermissionMode = string(b.PermissionMode)
	}
	// ★ 门槛是 HasTools 而不是 len(b.Tools) > 0。
	//
	// 用长度当门槛就把 `tools: []` 这一条**整个吞掉**了 —— 它是"要求拒绝一切"的声明,
	// 长度恰好是 0, 于是它既不进 Tools 也进不了别的分支, 结果是"这份资产没声明白名单",
	// 对账放行、运行时放行。一份 fail-closed 声明被静默翻译成完全放行, 是本条分支
	// 存在以来最危险的一次退化 (TestCLI显式空白名单拒绝一切 就是照它写的)。
	//
	// 两处细节别顺手"简化"掉:
	//   - 用 append 复制而不是直接赋值 —— 直接赋值让 d.Tools 与 b.Tools 共享底层数组,
	//     对账侧任何一次原地修改都会改到读数侧;
	//   - 空名单要显式构造成 `[]string{}` —— `append([]string(nil), 空切片...)` 在
	//     零个元素时返回的正是那个 nil 切片 (append 一个都不追加就原样返回 s),
	//     "显式空名单"会在这里第二次退化成 nil, 而 nil 在对账里意思是"没声明"。
	if b.HasTools {
		if len(b.Tools) == 0 {
			d.Tools = []string{} // 显式空白名单: 非 nil 空切片 = "应当拒绝一切"
		} else {
			d.Tools = append([]string(nil), b.Tools...)
		}
	}
	if b.HasDisallowedTools {
		if len(b.DisallowedTools) == 0 {
			d.DisallowedTools = []string{}
		} else {
			d.DisallowedTools = append([]string(nil), b.DisallowedTools...)
		}
	}
	for i, h := range b.Hooks {
		d.ExternalHooks = append(d.ExternalHooks, assembly.ExternalHook{
			Event: string(h.Event), Type: string(h.HookType), Index: i,
		})
	}
	if d.Tools == nil && d.DisallowedTools == nil && d.ExternalHooks == nil &&
		d.Model == "" && d.PermissionMode == "" {
		return nil
	}
	return d
}

// Warnings 产出这一份装配里**需要人看一眼**的项 (非法取值 / 查不到的名字 /
// 没有消费方的声明)。返回空表示这份资产装配干净。
//
// 与 Binding 的区别: Binding 是全量读数 (含正常项), Warnings 只挑出要处置的。
// 日志与面板上只印后者, 否则正常路径的噪声会把真问题淹掉。
func (b *AssetBinding) Warnings() []string {
	if b == nil {
		return nil
	}
	var out []string
	if b.Asset == nil {
		return []string{"agent 资产 " + b.Name + " 没查到 (声明不存在)"}
	}
	if !b.Active {
		out = append(out, "agent 资产 "+b.Name+" 的状态是 "+b.Asset.Status+
			" (非 active): 一律不装配 —— 影子/归档资产被模型直接调用等于绕过灰度机制")
		return out
	}
	for _, bd := range b.Bindings {
		switch bd.Status {
		case BindingInvalidValue, BindingUnknownName, BindingNoConsumer:
			out = append(out, "agent 资产 "+b.Name+" 的 "+bd.Facet+" 声明: "+bd.Note)
		}
	}
	return out
}

// BindAgentAsset 按资产名装配出一个 AssetBinding (名字查不到时返回 Name 已填的读数值)。
//
// skills 池是**可选**参数: 传 nil 表示调用方没有技能注册表 (CLI 的某些入口), 此时
// skills 声明一律报 unknown_name 而不是"接上了"—— 没池可查与查了没有, 在"这条
// 技能到底会不会生效"上是同一个答案, 压成两种说法反而会让人以为少传池是安全的。
func BindAgentAsset(name string, skillReg SkillPool) *AssetBinding {
	name = strings.TrimSpace(name)
	b := &AssetBinding{Name: name}
	a := agent.LookupAgentAsset(name)
	if a == nil {
		return b
	}
	b.Asset = a
	b.Active = a.IsActive()
	if !b.Active {
		return b
	}

	b.Prompt = strings.TrimSpace(a.Body)

	b.bindToolProfile(a)
	b.bindPermissionMode(a)
	b.bindMaxTurns(a)
	b.bindModel(a)
	b.bindSkills(a, skillReg)
	b.bindTools(a)
	b.bindHooks(a)
	b.bindPlugin(a)

	// 排序让两次装配的读数可比 —— 顺序不定时同一份资产每次报出不同的排列,
	// 人看两次会以为装配变了。
	sortBindings(b.Bindings)
	return b
}

// bindToolProfile 装配资产声明的 tool-profile 档位; 非法取值整条作废 (回落到调用方基线),
// 绝不静默掰成 chat。
func (b *AssetBinding) bindToolProfile(a *asset.Asset) {
	raw := strings.TrimSpace(a.Meta("tool-profile"))
	if raw == "" {
		b.Bindings = append(b.Bindings, Binding{Facet: "tool-profile", Status: BindingUnset})
		return
	}
	// NormalizeToolProfile 对**不认识的**串恒返回 chat, 且不留痕。这是本仓最接近
	// "静默权限变更"的一处: `tool-profile: codign` 会让子代理连 Read 都没有, 而
	// 日志上一个字都没有。所以这里先自己判一次合法性, 非法就整条声明作废 (回落
	// 到调用方的基线), 而不是让它变成一个看起来合法的 chat。
	if !builtin.ValidToolProfile(builtin.ToolProfile(raw)) {
		b.Bindings = append(b.Bindings, Binding{
			Facet: "tool-profile", Status: BindingInvalidValue, Declared: []string{raw},
			Note: "档位 " + raw + " 不是合法取值: 已整条作废 (回落到调用方基线), " +
				"不按 chat 处理 —— NormalizeToolProfile 会把不认识的串静默掰成 chat, 那是一次没人察觉得到的权限收窄",
		})
		return
	}
	b.ToolProfile = builtin.NormalizeToolProfile(builtin.ToolProfile(raw))
	b.HasToolProfile = true
	b.Bindings = append(b.Bindings, Binding{
		Facet: "tool-profile", Status: BindingWired, Declared: []string{raw},
		Resolved: []string{string(b.ToolProfile)},
		Consumer: "调用点经 ConstraintSet.WithToolProfile ⇒ newProfileRegistry " +
			"(pkg/feishu/session.go:runNestedAgent) / builtin.RegisterProfileToolsWithStore (cmd/claude-go)",
	})
}

// bindPermissionMode 装配资产声明的 permission-mode 权限档; 不认识的档整条作废 ——
// 喂给 permissions.NewChecker 会一路落到"每次询问", 在非交互子代理里等于全部拒绝。
func (b *AssetBinding) bindPermissionMode(a *asset.Asset) {
	raw := strings.TrimSpace(a.Meta("permission-mode"))
	if raw == "" {
		b.Bindings = append(b.Bindings, Binding{Facet: "permission-mode", Status: BindingUnset})
		return
	}
	mode := types.PermissionMode(raw)
	if !types.ValidPermissionMode(mode) {
		b.Bindings = append(b.Bindings, Binding{
			Facet: "permission-mode", Status: BindingInvalidValue, Declared: []string{raw},
			Note: "权限档 " + raw + " 不是合法取值: 已整条作废。不认识的档喂给 permissions.NewChecker 会一路落到默认行为, 而" +
				"默认行为是「每次询问」—— 在一个非交互的子代理里, 那与「全部拒绝」几乎没有区别, 且没有任何报错",
		})
		return
	}
	b.PermissionMode = mode
	b.HasPermissionMode = true
	b.Bindings = append(b.Bindings, Binding{
		Facet: "permission-mode", Status: BindingWired, Declared: []string{raw}, Resolved: []string{raw},
		Consumer: "permissions.NewChecker(permMode) (pkg/feishu/session.go:runNestedAgent 的 permMode)",
	})
}

// bindMaxTurns 装配资产声明的 max-turns 回合上限; 非正整数整条作废 (0/负数会被引擎读成
// "不限制", 与作者"限得很紧"的意图相差一个数量级的额度)。
func (b *AssetBinding) bindMaxTurns(a *asset.Asset) {
	raw := strings.TrimSpace(a.Meta("max-turns"))
	if raw == "" {
		b.Bindings = append(b.Bindings, Binding{Facet: "max-turns", Status: BindingUnset})
		return
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		b.Bindings = append(b.Bindings, Binding{
			Facet: "max-turns", Status: BindingInvalidValue, Declared: []string{raw},
			Note: "max-turns " + raw + " 不是正整数: 已整条作废 (跟随外层)。写成 0 或负数会被引擎读成「不限制」," +
				"而作者的意图多半是「限得很紧」—— 两者相差一个数量级的额度",
		})
		return
	}
	b.MaxTurns = n
	b.Bindings = append(b.Bindings, Binding{
		Facet: "max-turns", Status: BindingWired, Declared: []string{raw}, Resolved: []string{raw},
		Consumer: "engine.Config.MaxTurns (经 clampTurns) / agent.RunOptions.MaxTurns",
	})
}

// bindModel 装配资产声明的 model; 本层不校验该模型在已解析的 provider 上是否存在
// (那是调用方的池), 写错的名字会在出站请求时 404。
func (b *AssetBinding) bindModel(a *asset.Asset) {
	raw := strings.TrimSpace(a.Meta("model"))
	if raw == "" {
		b.Bindings = append(b.Bindings, Binding{Facet: "model", Status: BindingUnset})
		return
	}
	b.Model = raw
	b.Bindings = append(b.Bindings, Binding{
		Facet: "model", Status: BindingWired, Declared: []string{raw}, Resolved: []string{raw},
		Consumer: "engine.Config.Model (经 opts.Model 的现有优先级链)",
		Note:     "同名模型必须在已解析的 provider 上存在, 否则出站请求会 404 —— 本层不查 provider 表 (那是调用方的池)",
	})
}

// SkillPool 组装器需要的技能池读取面 —— 只有一句话: "这个名字在池里解析得了吗"。
//
// 收窄成一个方法而不是直接吃 *skills.Registry, 两条理由:
//
//  1. 组装器要回答的就是这一句。收全量的 Registry 之后, 包与包之间就多了一条
//     "因为实现方便"而非"因为语义需要"的依赖, 而那条依赖会随下一次改动变成环。
//  2. 测试可以给一个三条技能的假池, 不必起真的技能注册表 —— 后者要扫盘, 于是
//     "组装器对池的说法对不对"这件事会被磁盘状态绑架。
//
// *skills.Registry 通过 HasActive 直接满足本接口 (pkg/skills/skills.go)。
type SkillPool interface {
	// HasSkill 报告某个技能名在当前池里是否**可解析** (存在, 且治理状态允许使用)。
	// 存在但 shadow/archived 的必须报 false —— 组装器据此报"声明了但拿不到",
	// 而影子技能本来就不该被资产声明激活。
	HasSkill(name string) bool
}

// bindSkills 把资产声明的技能名与技能池逐名对账, 分成"接上了"与"池里没有"两组。
//
// 关键: 部分命中按 BindingUnknownName 报 (而不是 wired) —— 它看起来"生效了", 缺的那几条
// 要靠逐条比对才看得出来, 正是本文件要防的那类静默少给。池为 nil 时一律算 unknown_name,
// 因为"没池可查"与"查了没有"在"这条技能会不会生效"上是同一个答案。
//
// 算法解读：
//
//	原理：对 declared 做一次划分 —— 逐个问 pool.HasSkill, 命中的进 resolved, 其余进
//	      missing; 再用 (missing 空 / resolved 空 / 两者都非空) 把结果归成三种状态。
//	      名字先过 dedupeStrings, 保证同一名字不会被计两次。
//	关键不变量：**只有 missing 为空才报 BindingWired**。部分命中必须降级为 UnknownName,
//	      因为声明是"一组能力"而非"若干条独立条目": 缺一条就意味着模型看到的清单与资产
//	      作者写的不是同一份。nil 池与空池走同一条 unknown_name 分支 (见上)。
//	代价与边界：只做存在性判定, 不校验技能内容是否与 role 匹配 (那是技能治理侧的事);
//	      对账结果只进 Bindings 与 Warnings, **不阻断装配** —— 少给的能力靠人读报告发现。
//	设计权衡：不做"部分接上也报 wired 但附注"的折中 —— 状态位只有三档, 让它保持
//	      "wired = 声明被完整满足"这一单一含义, 否则调用方无法用状态位做判断。
func (b *AssetBinding) bindSkills(a *asset.Asset, pool SkillPool) {
	declared := dedupeStrings(a.MetaList("skills"))
	if len(declared) == 0 {
		b.Bindings = append(b.Bindings, Binding{Facet: "skills", Status: BindingUnset})
		return
	}
	if pool == nil {
		b.Bindings = append(b.Bindings, Binding{
			Facet: "skills", Status: BindingUnknownName, Declared: declared,
			Note: "这条路径上没有技能池可查 (SkillPool 为 nil): 声明无法确认, 一律不算接上 —— " +
				"「没池可查」与「查了没有」在「这条技能会不会生效」上是同一个答案",
		})
		return
	}
	var resolved, missing []string
	for _, n := range declared {
		if pool.HasSkill(n) {
			resolved = append(resolved, n)
		} else {
			missing = append(missing, n)
		}
	}
	b.Skills = resolved
	switch {
	case len(missing) == 0:
		b.Bindings = append(b.Bindings, Binding{
			Facet: "skills", Status: BindingWired, Declared: declared, Resolved: resolved,
			Consumer: "prompt.Manager.SkillListing (roleScopedSkillListing / FormatShortListingForNames)",
		})
	case len(resolved) == 0:
		b.Bindings = append(b.Bindings, Binding{
			Facet: "skills", Status: BindingUnknownName, Declared: declared,
			Note: "声明的技能一个都不在池里 (" + strings.Join(missing, ", ") + "): 模型不会被提示有这些能力, 而运行一切正常",
		})
	default:
		// 部分命中是最坏的一种: 它看起来"生效了", 缺的那几条要靠逐条比对才看得出来。
		b.Bindings = append(b.Bindings, Binding{
			Facet: "skills", Status: BindingUnknownName, Declared: declared, Resolved: resolved,
			Note: "声明的技能里有 " + strings.Join(missing, ", ") + " 不在池里: 其余 " +
				strconv.Itoa(len(resolved)) + " 条已接上, 缺的这几条不会有任何提示",
		})
	}
}

// bindTools 处理**两份**名单, 而它们的检查方式不同 —— 这里是 P2′ 的落点。
//
// ---------------------------------------------------------------------------
// 白名单与黑名单是两个不同的问题
// ---------------------------------------------------------------------------
//
//	tools:            [Read, Grep]   白名单 —— "只给这些"
//	disallowed-tools: [Bash]         黑名单 —— "别给这个"
//
// 两者都要回答"这些名字是真的吗", 但**回答完之后**能不能自动核对"它生效了没有"
// 却不一样:
//
//	白名单 —— 能。装配报告的实际侧给了逐工具的可见集 (Visible/Hidden), 声明的每一项
//	         都能拿它去比: 在名单里却不可见 ⇒ 真的没接上。声明进 AssemblyDeclaration().Tools。
//	黑名单 —— 不能*。实际侧给的是**生效的**黑名单 (Config.DisabledTools), 拿它跟声明的
//	         比是"结果与结果比": 两边都来自同一条接线路, 接线断了就两边一起空, 比出来
//	         永远是零差异。它没有独立的"实际"可以当判据。
//
// 所以本文件对两份名单做同一件事 (**逐名验名** —— 见下), 其余交给各自的机制:
// 白名单走对账, 黑名单走"接线是否真的到了 Config.DisabledTools"。
//
// ---------------------------------------------------------------------------
// 逐名验名: 这里必须做, 而且必须用**静态目录**而不是"实际注册表里有没有"
// ---------------------------------------------------------------------------
//
// `disallowed-tools: [bash]` (小写) 与 `[Bash]` 在装配读数上一模一样, 区别只在运行期:
// 前者一个工具都挡不住 —— 而作者以为自己关掉了它。这类"静默无效"必须在这里拦住。
//
// 判"这个名字存在吗"用的是 builtin.ToolNameCatalog (静态, 列出内置工具名),
// **不是**引擎的实际注册表。理由是 `disallowed-tools: [Bash]` + `tool-profile: analysis`
// 这个组合: analysis 档下 Bash 从来没被注册, 拿实际去判会把它报成"名字不存在
// (拼错了?)" —— 而它是一个**正确且冗余**的声明 (作者在防御性地关掉一个本就不该
// 出现的工具)。静态目录说"Bash 是个真工具", 于是它被正确接受。
//
// 问的是 **admin 档** —— 不是"这个资产声明的档"。这一步我第一版写错了, 被
// Test黑名单在analysis档下不误报 拦住: 拿声明档位去问, 上面那个正确且冗余的声明
// 照样被报成拼写错误 (analysis 目录里就是没有 Bash)。档位目录回答的是"这个档位
// 注册了它", 而这里要问的是"这个名字合不合法" —— 后者是一个**与档位无关**的是非
// 判断, 只有各档位的**并集** (admin) 才答得对。
//
// 代价: 目录里没有的名字一律会被报出来让人看一眼。这是刻意的 —— 宁可多问一句,
// 也不要静默放过一个拼错的名字 (拼错的黑名单什么都不挡)。
//
// ---------------------------------------------------------------------------
// 认不出的名字 **照原样放行**, 只报不改 —— 一个刻意的不作为
// ---------------------------------------------------------------------------
//
// 本函数是**纯装配**: 只产出读数 (b.Tools / b.DisallowedTools 供调用方接线),
// 不改写声明。所以"这个名字我认不出"不构成把它删掉的理由, 两个方向都一样:
//
//	剔除会改变生效行为。白名单 `tools: [mcp__foo__bar]` 若因"不在内置目录里"被整份
//	剔空, 得到的是一个**空的非 nil 白名单** = fail-closed 拒绝一切工具 —— 一份本来
//	只是窄的配置变成了一份**恒失败**的配置, 而这个变化没有任何人要求过。
//	保留则会落到"该名字实际不可见"上, 那正是它今天(未接线时)的行为, 且由对账
//	(hiddenReason 里那句"在生效白名单内却仍不可见")与下面这条 notes 两处报出来。
//
// 目录不是全知: MCP 动态工具、CLI 侧另外注册的工具都不在它里面。拿一个不全的
// 集合去**删东西**, 是在用"我没编目"冒充"它不存在"。所以它只用来**判是非**,
// 不用来下判决。
func (b *AssetBinding) bindTools(a *asset.Asset) {
	// ★ 用 MetaListSet 而不是 MetaList: `tools: []` 是**显式空名单** (fail-closed,
	// 拒绝一切工具), 与"没写 tools:" 语义相反。MetaList 把键的存在性吃掉 (它只看
	// 列表长度), 于是那条声明永远表达不出来 —— 一份故意写的"什么都不给"会静默
	// 退化成"什么都没限制"。这不是理论问题: TestCLI显式空白名单拒绝一切 就是照
	// 这条写出来、然后在旧实现上变红的。
	raw, hasTools := a.MetaListSet("tools")
	disallowed, hasDeny := a.MetaListSet("disallowed-tools")
	raw, disallowed = dedupeStrings(raw), dedupeStrings(disallowed)
	b.HasTools, b.HasDisallowedTools = hasTools, hasDeny

	if !hasTools && !hasDeny {
		b.Bindings = append(b.Bindings, Binding{Facet: "tools", Status: BindingUnset})
		return
	}
	// ★ 显式空名单要**在这里**补回非 nil 的空切片。
	//
	// dedupeStrings 对空输入返回 nil, 于是"键写了但值为空"与"键没写"在 raw/disallowed
	// 上又变得一模一样 —— 存在性刚从解析层捞回来, 不能在本层再丢一次。下游
	// (WithToolAllowList 的 nil 判据、cfg.AllowedTools 的 nil 判据) 靠的正是这个
	// 非 nil-ness; 调用点那边另用 HasTools 把关, 两条一起才谈得上"改不动也忘不掉"。
	if hasTools && len(raw) == 0 {
		raw = []string{}
	}
	if hasDeny && len(disallowed) == 0 {
		disallowed = []string{}
	}

	catalog := map[string]bool{}
	for _, n := range builtin.ToolNameCatalog(nameCatalogProfile()) {
		catalog[n] = true
	}
	badTools := unknownNames(raw, catalog)
	badDeny := unknownNames(disallowed, catalog)

	b.Tools = raw
	b.DisallowedTools = disallowed

	var notes []string
	if len(badTools) > 0 {
		notes = append(notes, "白名单里这 "+strconv.Itoa(len(badTools))+" 个名字不是该档位的内置工具: "+
			strings.Join(badTools, ", ")+
			" —— 已按原文接线; 若它也不是 MCP/调用方另外注册的工具, 那么这一项写错了"+
			" (工具名逐字节区分大小写, 如 StrReplace 而非 Edit、Bash 而非 Shell), 后果是这个工具对模型不可见")
	}
	if len(badDeny) > 0 {
		notes = append(notes, "disallowed-tools 里这 "+strconv.Itoa(len(badDeny))+" 个名字不是该档位的内置工具: "+
			strings.Join(badDeny, ", ")+
			" —— 已按原文接线; 但若它本意是挡某个内置工具, 那是把名字写错了, 而**写错的黑名单不挡任何东西**"+
			" (与白名单相反: 白名单写错是太窄, 黑名单写错是太宽)")
	}
	if len(raw) > 0 {
		notes = append(notes, "白名单逐项是否真的可见由对账回答 (声明进 AssemblyDeclaration().Tools, 与实际可见集比)")
	}
	if len(disallowed) > 0 {
		notes = append(notes, "黑名单减的是注册表, 被挡掉的工具会出现在装配报告的 Hidden 里; "+
			"声明侧另进 AssemblyDeclaration().DisallowedTools, 由对账报出\"声明要挡却仍可见 / 声明没走到生效黑名单\"")
	}

	declared := append(append([]string{}, raw...), disallowed...)
	status := BindingWired
	if len(badTools) > 0 || len(badDeny) > 0 {
		// 有认不出的名字 ⇒ 至少有一项声明不会按作者预期生效, 必须留痕。
		//
		// 这一档在**黑名单**上是唯一能看见它的地方: 名字写错的黑名单功能上完全
		// 不可观测 —— 它什么都不挡, 与不写这条声明逐字相同 (见
		// TestP2黑名单大小写写错会说话 对这条分辨力边界的如实记录)。
		status = BindingInvalidValue
	}
	b.Bindings = append(b.Bindings, Binding{
		Facet: "tools", Status: status, Declared: declared, Resolved: declared,
		Consumer: "ConstraintSet.WithToolAllowList / WithDeniedTools ⇒ engine.Config.AllowedTools / DisabledTools (toolExposed 统一过滤)",
		Note:     strings.Join(notes, "; "),
	})
}

// nameCatalogProfile 验名时用哪个档位的目录 —— 恒为 admin。
//
// 保留成一个具名函数而不是把常量内联, 是为了让"这里为什么不是 b.ToolProfile"
// 有一个能被注释解释的地方 (那正是第一版写错的那一步)。
//
// 两个问题是两回事, 不能合并:
//
//	这个名字是真工具吗?      → admin 档目录 (各档位并集, 与资产声明无关)
//	这个档位注册了它吗?      → 装配报告的实际侧 (Visible/Hidden), 或直接问注册表
//
// 用声明档位去问第一个问题, 会把"关掉一个本档位本就没有的工具"这种正确且冗余的
// 声明报成拼写错误。
func nameCatalogProfile() builtin.ToolProfile { return builtin.ToolProfileAdmin }

// unknownNames 挑出目录里没有的名字, 保持原顺序。只用于**上报**, 不带任何处置。
func unknownNames(names []string, catalog map[string]bool) []string {
	var out []string
	for _, n := range names {
		if !catalog[n] {
			out = append(out, n)
		}
	}
	return out
}

// parseAssetHooks 把资产里的 `hooks` 声明解析成可直接喂给 hooks.Runner 的配置。
//
// ---------------------------------------------------------------------------
// 为什么是**扁平三元组**, 而不是 Claude Code 的嵌套写法
// ---------------------------------------------------------------------------
//
// Claude Code 的 hooks 一定是嵌套的:
//
//	hooks:
//	  PreToolUse:
//	    - matcher: "Bash"
//	      hooks: [{type: command, command: ./x.sh}]
//
// 而本仓的 frontmatter 解析器 (pkg/asset.SplitFrontmatter) 是**扁平**的: 它只认
// 单值键与一层 `- item` 列表 (这是为了与 pkg/skills 逐字节一致, 那套解析被整个
// SKILL.md 生态依赖, 不能改)。嵌套写法喂进来会塌成 `pre-tooluse: [matcher: Bash, ...]`
// —— 键名被归一、层级被抹平, 读出来是一堆认不出的东西。
//
// 与其悄悄读不到, 不如把**本仓能读的形态**定死并校验:
//
//	hooks:
//	  - <event>:<type>:<target>[:<if 匹配器>]
//
// 例:
//
//	hooks:
//	  - PreToolUse:command:./scripts/guard.sh:Bash*
//	  - SessionStart:http:https://hooks.internal/session
//
// `type` 留空即 command (与 types.HookType 的既有约定一致)。`if` 省略即不限制。
// 行内写法 (`hooks: A, B`) 与多行写法都读得到 —— MetaList 两种都认。
//
// 返回 (解析出的 hook, 逐条问题说明)。问题与 hook **同时**返回: 一份资产里三条
// hook 两条合法一条拼错时, 合法的两条该照常生效, 拼错的那条该被点名 ——
// 整份作废会让作者以为"格式没写对", 而实际是"有一条没生效"。
func parseAssetHooks(a *asset.Asset) ([]types.HookConfig, []string) {
	items := a.MetaList("hooks")
	if len(items) == 0 {
		return nil, nil
	}
	var out []types.HookConfig
	var problems []string
	for _, raw := range items {
		// SplitN(…, 3): target 里可能有冒号 (https://…) 或 Windows 盘符, 只能切前两刀。
		parts := strings.SplitN(strings.TrimSpace(raw), ":", 3)
		if len(parts) < 3 {
			problems = append(problems, "hook 声明 "+strconv.Quote(raw)+
				" 读不出来: 需要 <event>:<type>:<target> 三段 (type 可留空即 command), "+
				"注意本仓 frontmatter 是扁平的, Claude Code 那种嵌套 hooks 写法读不到")
			continue
		}
		ev := types.HookEvent(strings.TrimSpace(parts[0]))
		ty := types.HookType(strings.TrimSpace(parts[1]))
		target := strings.TrimSpace(parts[2])
		// 第 4 段 (可选的 if 匹配器) 挂在 target 后面, 用同一个冒号切 —— 只有当
		// target 本身不含冒号时才可能切歪, 而那种 target (command 路径 / URL) 里
		// 出现冒号时本来就在第三段的余部, 所以这里再切一次是安全的。
		ifMatch := ""
		if idx := strings.LastIndex(target, ":"); idx > 0 && !strings.Contains(target[:idx], "://") {
			// 仅当冒号后面那截不含路径分隔符时, 才认为是 if 匹配器 —— 否则
			// `C:\path\to.sh` 会被切成 `C` + `\path\to.sh`。
			tail := target[idx+1:]
			if tail != "" && !strings.ContainsAny(tail, `/\`) {
				ifMatch = tail
				target = target[:idx]
			}
		}

		var prob string
		switch {
		case !types.ValidHookEvent(ev):
			prob = "hook 事件 " + strconv.Quote(string(ev)) + " 不是合法取值: 这条 hook 永远不会触发 " +
				"(装载与注册都会成功, 只是没人按这个名字问它)"
		case !types.ValidHookType(ty):
			prob = "hook 类型 " + strconv.Quote(string(ty)) + " 不是合法取值"
		case target == "":
			prob = "hook 声明 " + strconv.Quote(raw) + " 没有 target (命令 / URL / 策略路径)"
		}
		if prob != "" {
			problems = append(problems, prob)
			continue
		}
		out = append(out, types.HookConfig{
			Event:    ev,
			HookType: ty,
			If:       ifMatch,
			Command:  target,
		})
	}
	return out, problems
}

// bindHooks 装配资产声明的外部 hook: 合法的进 b.Hooks, 每条非法声明单独留一条
// BindingInvalidValue 读数 (不整份作废 —— 三条里坏一条, 好的两条照常生效)。
func (b *AssetBinding) bindHooks(a *asset.Asset) {
	hooks, problems := parseAssetHooks(a)
	for _, p := range problems {
		b.Bindings = append(b.Bindings, Binding{Facet: "hooks", Status: BindingInvalidValue, Note: p})
	}
	if len(hooks) == 0 {
		if len(problems) == 0 {
			b.Bindings = append(b.Bindings, Binding{Facet: "hooks", Status: BindingUnset})
		}
		return
	}
	b.Hooks = hooks
	names := make([]string, 0, len(hooks))
	for _, h := range hooks {
		names = append(names, string(h.Event)+":"+string(h.HookType))
	}
	b.Bindings = append(b.Bindings, Binding{
		Facet: "hooks", Status: BindingWired, Declared: names, Resolved: names,
		Consumer: "hooks.NewRunnerWithContext(ctx, cfg…) —— 调用点把资产 hook **追加**在配置 hook 之后",
	})
}

// bindPlugin 处理资产里的 `runtime: plugin` 声明 —— 本仓没有读它的地方, 如实报
// BindingNoConsumer 而不是假装接上。
func (b *AssetBinding) bindPlugin(a *asset.Asset) {
	// 见文件头: 本仓没有 plugin 运行时。资产里写了 `runtime: plugin` 也不会生效。
	raw := strings.TrimSpace(a.Meta("runtime"))
	if raw != "plugin" {
		return
	}
	b.Bindings = append(b.Bindings, Binding{
		Facet: "plugin", Status: BindingNoConsumer, Declared: []string{raw},
		Note: "本仓没有 plugin 运行时 (唯一叫 plugin 的实现是 hooks 的 HookTypePlugin, 那是 hook 的一种执行方式)。" +
			"这条声明不会生效 —— 要跑外部逻辑请用 hooks; 要加工具请用工具池",
	})
}

// sortBindings 按 Facet 排序。稳定排序 + 前面已按声明顺序 append 的同类多项
// (hooks 的非法项) 顺序也保持 —— 同一份资产两次装配要报出同一个排列。
func sortBindings(bs []Binding) {
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].Facet < bs[j].Facet })
}

// dedupeStrings 去掉首尾空白、去重并保持原顺序; 输入为空时返回 nil。
//
// 注意：返回值是 nil 还是空切片, 调用方必须自己区分 —— 本文件多处靠"显式空名单要补回
// 非 nil 空切片"来保住 fail-closed 语义 (见 bindTools)。
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
