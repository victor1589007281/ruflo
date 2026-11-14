// plugin_runtime_attrs.go —— 成员/插件的**运行时口径**投影 (声明值 vs 缺省值)。
//
// ---------------------------------------------------------------------------
// 它补的是什么
// ---------------------------------------------------------------------------
// dashboard 的成员摘要此前只回 name/description + 少量结构化字段 (model /
// tool-profile / max-turns ...)。但 claude-go 的运行时属性远不止这些, 且**绝大
// 多数属性在资产里根本没写** —— 它们由引擎/装配器在运行期取缺省值。从页面上看不
// 到"没写时会发生什么", 于是:
//
//   - 以为没写 = 没限制, 实际工具档位回退成 chat (更窄);
//   - 以为没写 = 不限制回合, 实际引擎兜底 30 轮;
//   - 以为 workflow 的 md 只是文档, 实际 steps 是所属应用的调度契约;
//   - 以为 hook 资产写了 `hooks:` 就生效, 实际 type 留空按 command, 事件名拼错
//     则永远不触发且没有任何报错。
//
// 本文件把每一类资产的运行时键**逐条**摊开: 声明值 / 是否声明 / 缺省值 / 最终生效
// 值 / 一句话说明。键名与消费方口径一律对齐 pkg/agentbind、pkg/asset、pkg/hooks、
// pkg/pluginrun —— 这里只是投影, 不新造解释。
//
// ★ 为什么缺省值要由服务端给, 而不是前端自己拼一份:
//
//	缺省值是**运行时的行为**, 随引擎代码变化。前端手抄一份的那天就会与引擎漂移,
//	而漂移的表现是"页面写着 30 轮, 实际跑了 25 轮", 没有任何报错。
package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/asset"
)

// runtimeAttrDTO 一条运行时属性的声明/缺省对账。
//
// Declared 是**声明原文**的可读形态 (列表用 " · " 连接; 显式空列表用 "[]")。
// IsSet 保留"键写没写"这一位 —— `tools: []` 与"没写 tools"在语义上相反, 只靠
// Declared 字符串长度会把两者抹成一样 (见 asset.MetaListSet 的注释)。
type runtimeAttrDTO struct {
	Key      string `json:"key"`   // frontmatter / JSON 原始键名
	Label    string `json:"label"` // 页面标签
	Declared string `json:"declared,omitempty"`
	IsSet    bool   `json:"isSet"`
	Default  string `json:"default,omitempty"`
	// Effective 未声明时填缺省行为, 已声明时填最终生效值 —— 页面上"现在到底怎样"
	// 看这一列, "如果没写会怎样"看 Default。
	Effective string `json:"effective,omitempty"`
	Note      string `json:"note,omitempty"`
	// Level: "" 正常 / "warn" 非法值或本版本无消费方。页面据此上色。
	Level string `json:"level,omitempty"`
}

// mkAttr 组装一条属性。declared 为空即视为未声明, Effective 自动落到缺省值;
// 已声明时 Effective 默认等于声明值 (需要不同措辞时显式覆盖)。
func mkAttr(key, label, declared, def, effective, note, level string) runtimeAttrDTO {
	a := runtimeAttrDTO{
		Key: key, Label: label,
		Declared: strings.TrimSpace(declared),
		Default:  def,
		Note:     note,
		Level:    level,
	}
	if a.Declared == "" {
		a.Effective = def
	} else if effective != "" {
		a.Effective = effective
	} else {
		a.Effective = a.Declared
	}
	a.IsSet = a.Declared != ""
	return a
}

// isSetAttr 显式给 IsSet=true 的版本 (显式空列表 `[]` 用)。
func isSetAttr(key, label, declared, def, effective, note, level string) runtimeAttrDTO {
	a := mkAttr(key, label, declared, def, effective, note, level)
	a.IsSet = true
	return a
}

// joined 把列表转成可读声明值; nil/空返回 "" (调用方按"未声明"处理)。
func joined(v []string) string { return strings.Join(v, " · ") }

// runtimeAttrsFor 按成员种类摊开运行时属性。kind 取自 asset.Asset.Kind。
func runtimeAttrsFor(kind asset.Kind, a *asset.Asset) []runtimeAttrDTO {
	if a == nil {
		return nil
	}
	switch kind {
	case asset.KindAgent:
		return agentRuntimeAttrs(a)
	case asset.KindSkill:
		return skillRuntimeAttrs(a)
	case asset.KindHook:
		return hookRuntimeAttrs(a)
	case asset.KindTool:
		return toolRuntimeAttrs(a)
	case asset.KindWorkflow:
		return workflowRuntimeAttrs(a)
	default:
		return []runtimeAttrDTO{
			mkAttr("name", "名称", a.Name, "", "", "", ""),
			mkAttr("description", "描述", a.Description, "无", "", "", ""),
		}
	}
}

// agentRuntimeAttrs 一份 agent 资产的装配面 (与 pkg/agentbind 的 bind* 逐项对应)。
func agentRuntimeAttrs(a *asset.Asset) []runtimeAttrDTO {
	attrs := make([]runtimeAttrDTO, 0, 12)

	status := strings.TrimSpace(a.Status)
	attrs = append(attrs, mkAttr("status", "治理状态", status,
		"active (缺省即启用)", "", "shadow / archived 一律不装配 —— 影子资产被模型直接调用等于绕过灰度机制", ""))

	attrs = append(attrs, mkAttr("model", "模型", strings.TrimSpace(a.Meta("model")),
		"跟随外层会话 / provider 解析", "",
		"同名模型必须存在于已解析的 provider 表, 否则出站请求 404; dashboard 不校验 provider", ""))

	profile := strings.TrimSpace(a.Meta("tool-profile"))
	profileEff := ""
	profileNote := "合法取值: chat | research | coding | team | analysis | admin"
	profileLevel := ""
	if profile != "" {
		switch profile {
		case "chat", "research", "coding", "team", "analysis", "admin":
			profileEff = profile
		default:
			profileEff = "整条作废, 回退调用方基线"
			profileLevel = "warn"
			profileNote = "非法档位被整条作废, **不会**按 chat 处理 —— NormalizeToolProfile 对不认识的串静默返回 chat, 那是一次没人察觉得到的权限收窄"
		}
	}
	attrs = append(attrs, mkAttr("tool-profile", "工具档位", profile,
		"按调用方启发式推断 (角色名 / 消息文本), 最终回退 chat", profileEff, profileNote, profileLevel))

	perm := strings.TrimSpace(a.Meta("permission-mode"))
	permEff, permLevel, permNote := "", "", "合法取值: default | auto | plan | bypass | dontAsk | acceptEdits"
	if perm != "" {
		switch perm {
		case "default", "auto", "plan", "bypass", "dontAsk", "acceptEdits":
			permEff = perm
		default:
			permEff = "整条作废, 回退调用方基线"
			permLevel = "warn"
			permNote = "非法权限档会被整条作废; 不认识的档喂给权限检查器会落到「每次询问」, 在非交互子代理里约等于全部拒绝"
		}
	}
	attrs = append(attrs, mkAttr("permission-mode", "权限档", perm,
		"default (每次询问); 只读派生强制 plan", permEff, permNote, permLevel))

	maxTurns := strings.TrimSpace(a.Meta("max-turns"))
	maxEff, maxLevel, maxNote := "", "", "正整数; 0 / 负数会被引擎读成「不限制」, 与作者意图相差一个数量级"
	if maxTurns != "" {
		if n, err := strconv.Atoi(maxTurns); err == nil && n > 0 {
			maxEff = strconv.Itoa(n)
		} else {
			maxEff = "整条作废, 跟随外层"
			maxLevel = "warn"
			maxNote = "非正整数已整条作废 (跟随外层), 不会被当成 0=不限制"
		}
	}
	attrs = append(attrs, mkAttr("max-turns", "回合上限", maxTurns,
		"30 (engine 兜底); 团队成员阶段另有下限", maxEff, maxNote, maxLevel))

	// tools 的三态: 没写 / 显式 [] / 白名单。三者在页面上必须长得不一样。
	if tools, ok := a.MetaListSet("tools"); ok {
		if len(tools) == 0 {
			attrs = append(attrs, isSetAttr("tools", "工具白名单", "[]",
				"跟随工具档位基线 (不限制)", "拒绝一切工具 (fail-closed)",
				"显式空名单 = 一个工具都不给, 与「没写 tools」语义相反", "warn"))
		} else {
			attrs = append(attrs, isSetAttr("tools", "工具白名单", joined(tools),
				"跟随工具档位基线 (不限制)", joined(tools),
				"只给名单内的工具; 白名单写错是太窄 (黑名单写错才是太宽)", ""))
		}
	} else {
		attrs = append(attrs, mkAttr("tools", "工具白名单", "",
			"跟随工具档位基线 (不限制)", "", "没写这个键 ⇒ 不限制白名单", ""))
	}

	if deny, ok := a.MetaListSet("disallowed-tools"); ok {
		if len(deny) == 0 {
			attrs = append(attrs, isSetAttr("disallowed-tools", "工具黑名单", "[]",
				"无", "不挡任何工具", "显式空名单与不写等价, 但保留了作者的声明意图", ""))
		} else {
			attrs = append(attrs, isSetAttr("disallowed-tools", "工具黑名单", joined(deny),
				"无", joined(deny),
				"黑名单减的是注册表; 名字大小写写错等于什么都没挡", ""))
		}
	} else {
		attrs = append(attrs, mkAttr("disallowed-tools", "工具黑名单", "", "无", "", "", ""))
	}

	attrs = append(attrs, mkAttr("skills", "技能", joined(a.MetaList("skills")),
		"不注入技能清单", "",
		"声明的技能必须已在技能注册表里; 查不到只会静默少给, 不阻断装配", ""))

	attrs = append(attrs, mkAttr("hooks", "外部 Hook", joined(a.MetaList("hooks")),
		"无外部 hook", "",
		"扁平格式 <event>:<type>:<target>[:<if>]; type 留空即 command, 事件名拼错则永远不触发", ""))

	if plugins := a.MetaList("plugins"); len(plugins) > 0 {
		attrs = append(attrs, isSetAttr("plugins", "插件包", joined(plugins),
			"不加载插件包", joined(plugins),
			"由 CLI run --plugin 的装配路径消费; 飞书 Agent 派生路径不读这个键", ""))
	} else {
		attrs = append(attrs, mkAttr("plugins", "插件包", "", "不加载插件包", "", "", ""))
	}

	// runtime: agent 资产里的 `runtime: plugin` 与插件包是两回事, 本仓没有消费方。
	if rt := strings.TrimSpace(a.Meta("runtime")); rt != "" {
		lvl, note := "", ""
		if rt == "plugin" {
			lvl, note = "warn", "本仓没有 plugin 运行时: 这条声明不会生效。要跑外部逻辑请用 hooks, 要加工具请用工具池"
		}
		attrs = append(attrs, mkAttr("runtime", "运行时形态", rt,
			"builtin (未声明)", rt, note, lvl))
	}
	return attrs
}

// skillRuntimeAttrs 一份技能的运行时面 (与 pkg/skills.parseFrontmatter 对应)。
func skillRuntimeAttrs(a *asset.Asset) []runtimeAttrDTO {
	attrs := []runtimeAttrDTO{
		mkAttr("status", "治理状态", a.Status, "active (缺省即启用)", "",
			"shadow / archived 不进模型清单, Skill 工具也拒载", ""),
		mkAttr("when-to-use", "适用时机", a.Meta("when-to-use"), "不设置 (仅 description 进清单)", "",
			"进提示词的技能清单描述; 不改变工具面", ""),
		mkAttr("model", "模型", a.Meta("model"), "跟随会话", "",
			"技能声明的模型仅作元数据, 运行时模型仍由会话/阶段解析", ""),
		mkAttr("version", "版本", a.Version, "未声明", "", "", ""),
	}
	if paths := a.MetaList("paths"); len(paths) > 0 {
		attrs = append(attrs, isSetAttr("paths", "适用路径", joined(paths),
			"恒可见 (不做路径过滤)", joined(paths),
			"只在目录里存在命中 glob 的文件时才进注入清单", ""))
	} else {
		attrs = append(attrs, mkAttr("paths", "适用路径", "",
			"恒可见 (不做路径过滤)", "", "", ""))
	}
	if allowed, ok := a.MetaListSet("allowed-tools"); ok {
		attrs = append(attrs, isSetAttr("allowed-tools", "声明式工具面", joined(allowed),
			"不限制 (仅治理层可见)", joined(allowed),
			"运行期工具可见性只由 engine.Config.toolExposed 裁决; 本字段供进化治理闸判断越权, 不改变执行", ""))
	} else {
		attrs = append(attrs, mkAttr("allowed-tools", "声明式工具面", "",
			"不限制 (仅治理层可见)", "", "运行期不参与工具可见性裁决", ""))
	}
	for _, pair := range []struct{ key, label, note string }{
		{"model-invocable", "模型可调用", "false ⇒ 只作为用户命令存在, 不进模型清单、Skill 工具拒载"},
		{"user-invocable", "用户可调用", "false ⇒ 只作为模型可选项, 不进用户侧技能展示"},
	} {
		raw := strings.TrimSpace(a.Meta(pair.key))
		if raw == "" {
			attrs = append(attrs, mkAttr(pair.key, pair.label, "",
				"true (双向可用, 缺省)", "", pair.note, ""))
			continue
		}
		b := asset.ParseBool(raw)
		eff := "true (双向可用)"
		if !b {
			eff = "false (" + pair.label + "关闭)"
		}
		attrs = append(attrs, mkAttr(pair.key, pair.label, raw, "true (双向可用)", eff, pair.note, ""))
	}
	return attrs
}

// hookRuntimeAttrs 一份 hook 资产的触发面。hook 资产没有独立的 event/type 键 ——
// 声明全在 `hooks:` 的扁平条目里 (<event>:<type>:<target>[:<if>])。
func hookRuntimeAttrs(a *asset.Asset) []runtimeAttrDTO {
	decls := a.MetaList("hooks")
	attrs := []runtimeAttrDTO{
		mkAttr("hooks", "Hook 声明", joined(decls),
			"无 —— 这份 hook 资产不会触发任何事件",
			"", "扁平格式 <event>:<type>:<target>[:<if>]; 嵌套 YAML 写法读不到", ""),
		mkAttr("type", "执行方式", "",
			"command (条目里 type 留空即 command)", "",
			"可选: command | prompt | http | mcp | plugin | opa | function | grpc", ""),
		mkAttr("if", "匹配器", "",
			"不限制 (条目省略第 4 段)", "",
			"仅 PreToolUse/PostToolUse 一类带 tool 名的事件有意义, 如 Bash*", ""),
		mkAttr("timeout", "超时", "",
			"10s (Runner 级缺省)", "",
			"扁平声明不支持 timeout 字段; 该成员若要细调需走 config.json 的 hooks 配置", ""),
	}
	return attrs
}

// toolRuntimeAttrs 工具资产。本版本 claude-go 只登记/索引工具资产, 没有注册进
// 运行时 —— 这是本仓的缺口, 如实报出来, 而不是让页面看起来"工具已生效"。
func toolRuntimeAttrs(a *asset.Asset) []runtimeAttrDTO {
	return []runtimeAttrDTO{
		mkAttr("name", "名称", a.Name, "", "", "", ""),
		mkAttr("description", "描述", a.Description, "无", "", "", ""),
		mkAttr("runtime", "运行时", "",
			"本版本无消费方: 只登记/索引, 不注册进工具池", "不生效",
			"pkg/pluginrun 会展开 tools/ 成员, 但 materialize 不把 tools 拷进 state 根 —— 这是已知缺口", "warn"),
	}
}

// wfJSONDecl 运行时 workflow JSON 里本页需要的字段 (只读投影, 不是完整定义)。
type wfJSONDecl struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Mode         string `json:"mode"`
	Rounds       int    `json:"rounds"`
	ProducesCode *bool  `json:"producesCode"`
	QualityGate  string `json:"qualityGate"`
	Revision     int64  `json:"revision"`
	Stages       []struct {
		Name string `json:"name"`
	} `json:"stages"`
	Graph json.RawMessage `json:"graph"`
}

// parseWFJSON 解析插件成员的 workflow JSON; 不是 {stages:[...]} 形状返回 nil。
func parseWFJSON(body string) *wfJSONDecl {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "{") {
		return nil
	}
	var d wfJSONDecl
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return nil
	}
	if len(d.Stages) == 0 {
		return nil
	}
	return &d
}

// workflowRuntimeAttrs workflow 的运行时口径。两条合法形态分开说:
//
//  1. 运行时 pipeline: workflows/*.json (mode + stages), claude-go 自己执行;
//  2. 声明式: workflows/<名>.md + frontmatter steps, 由所属应用消费 ——
//     claude-go 只登记/索引, **不执行**。
func workflowRuntimeAttrs(a *asset.Asset) []runtimeAttrDTO {
	if d := parseWFJSON(a.Body); d != nil {
		stageNames := make([]string, 0, len(d.Stages))
		for _, s := range d.Stages {
			stageNames = append(stageNames, s.Name)
		}
		attrs := []runtimeAttrDTO{
			isSetAttr("form", "形态", "pipeline (JSON)",
				"", "claude-go 运行时 pipeline",
				"mode + stages 由 claude-go 的图/工作流执行器消费", ""),
			mkAttr("mode", "模式", d.Mode,
				"必须声明 (无缺省; 未知 mode 注册被拒)", "",
				"可选: pipeline | fanout | graph | adversarial | adversarial_dev | orchestrated", ""),
			isSetAttr("stages", "阶段", joined(stageNames),
				"必须至少 1 个 stage", strconv.Itoa(len(d.Stages))+" 阶段",
				"阶段的 dependsOn 必须指向已声明的阶段且无环; 阶段级运行参数见下方 JSON / 图", ""),
		}
		rounds := ""
		if d.Rounds > 0 {
			rounds = strconv.Itoa(d.Rounds)
		}
		attrs = append(attrs,
			mkAttr("rounds", "对抗轮数", rounds,
				"0 (仅 adversarial 模式使用)", "", "非 adversarial 模式下该字段不参与执行", ""),
			mkAttr("producesCode", "产码工作流", boolDecl(d.ProducesCode),
				"按工作流名推断 (development 等为 true)", "",
				"决定是否跑编译/测试门禁; 未注册的工作流按名字判定", ""),
			mkAttr("qualityGate", "质量门禁", d.QualityGate,
				"按工作流名推断 (内容类为 content, 其余 none)", "",
				"取值: \"\" | content | none; 门禁策略在 graph.GraphMeta 里", ""),
			mkAttr("revision", "版本号", revDecl(d.Revision),
				"1 (装载时归一)", "",
				"乐观并发守卫: 改写必须携带读到的版本, 不匹配即拒", ""),
			mkAttr("graph", "图覆盖", rawJSONDecl(d.Graph),
				"无 (不注册覆盖表, 不遮蔽 env)", "",
				"工作流级图能力 (loop-group / 并发等); 空时行为与改造前逐字节一致", ""),
		)
		return attrs
	}

	// 声明式 md: steps 是调度契约, 不是文档。
	steps := a.MetaList("steps")
	attrs := []runtimeAttrDTO{
		isSetAttr("form", "形态", "declarative (md + steps)",
			"", "由所属应用消费, claude-go 只登记/索引",
			"插件包里的 workflows/*.md 不会被 claude-go 执行, 也不物化进 state/workflows", ""),
		mkAttr("steps", "步骤序列", joined(steps),
			"没有 steps ⇒ 所属应用无法按序调度", "",
			"顺序即语义; 每个 step 是所属应用的 job kind", ""),
		mkAttr("executor", "执行方", "",
			"所属应用 (如 Moqi 调度器)", "",
			"claude-go 只把这份声明注册/索引进资产目录, 不参与执行", ""),
	}
	if a.Description != "" {
		attrs = append(attrs, mkAttr("description", "描述", a.Description, "", "", "", ""))
	}
	return attrs
}

func boolDecl(p *bool) string {
	if p == nil {
		return ""
	}
	if *p {
		return "true"
	}
	return "false"
}

func revDecl(n int64) string {
	if n <= 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func rawJSONDecl(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "{}" {
		return ""
	}
	return s
}

// pluginRuntimeAttrs 插件清单自身的运行时面 (与 pkg/pluginrun/pluginrun.go 对应)。
func pluginRuntimeAttrs(a *asset.Asset, dirForm bool) []runtimeAttrDTO {
	if a == nil {
		return nil
	}
	rt := strings.TrimSpace(a.Meta("runtime"))
	rtEff, rtLevel, rtNote := "", "", "external = 成员由所属应用消费, claude-go 只注册/索引 (不物化)"
	if rt != "" {
		switch strings.ToLower(rt) {
		case "builtin":
			rtEff = "builtin"
		case "external":
			rtEff = "external"
		default:
			rtEff = "按 builtin 处理"
			rtLevel = "warn"
			rtNote = "不认识的 runtime 值已按 builtin 处理, 并会在 problems 里点名"
		}
	}
	attrs := []runtimeAttrDTO{
		mkAttr("runtime", "成员运行时", rt,
			"builtin (成员由 claude-go 安装进标准 state 根)", rtEff, rtNote, rtLevel),
		mkAttr("status", "治理状态", a.Status, "active (缺省即启用)", "", "", ""),
		mkAttr("version", "版本", a.Version, "未声明", "", "", ""),
	}
	form := "单文件 (无成员子目录)"
	if dirForm {
		form = "目录 (PLUGIN.md + agents/skills/hooks/tools/workflows)"
	}
	attrs = append(attrs, isSetAttr("dirForm", "包形态", form, "", form,
		"单文件形式没有成员子目录, 展开器报 problem", ""))

	for _, key := range []string{"commands", "mcp"} {
		if _, ok := a.MetaListSet(key); ok {
			attrs = append(attrs, isSetAttr(key, strings.ToUpper(key[:1])+key[1:], "已声明",
				"无运行时消费方", "不生效",
				"PLUGIN.md 的 "+key+" 键在本版本没有运行时消费方, 声明不会生效", "warn"))
		}
	}
	return attrs
}
