// Package assembly 描述"这次运行实际装配成了什么", 并把它与"声明要装配什么"
// 逐项对账。
//
// 为什么需要这个包 —— 一个具体的、已经发生过的缺陷:
//
//	EnableFrontierOptimizations() 只在飞书路径上被调用。纯 CLI 形态下
//	error_classifier / json_repair / loop_detector / prompt_cache /
//	budget_degrade / stop_signal 六个组件的字段是 nil, 于是对应的内置 hook
//	从未注册。而下游平台走的正是 CLI headless —— 看板上"配置已下发", 实际
//	装配里那一半是空的, 两边都没报错。
//
// 这类缺陷的共性是: **它只存在于"配置里写了"与"实际装上了"之间的缝隙里**,
// 而这条缝隙此前在运行时不可观测 (装配是内存态, 进程起来后没人答得出当时装了
// 什么)。所以本包做两件事:
//
//  1. Report —— 让 harness 主动**自报**实际装配 (实际注册的内置 hook、实际
//     装载的外部 hook、实际可见的工具名、实际生效的配置来源)。
//  2. Reconcile —— 把描述符里的**声明**与自报的**实际**逐项比对, 产出三类差异。
//
// 顺序是刻意的: 先自报, 再对账, 最后才轮到面板。跳过前两步直接做页面, 得到的
// 是一个只能显示"配置里写了什么"的面板 —— 而那恰恰是最不可信的那一份。
//
// 本包**只读**: 不装配任何组件, 不改任何开关, 不产生副作用。它是对装配结果的
// 观察, 不是装配的参与者。
package assembly

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SchemaVersion 报告结构版本。对账双方 (harness 与平台) 靠它判断字段含义是否
// 一致; 结构变更时递增, 平台可以对不认识的版本直接拒绝出结论而不是误读。
const SchemaVersion = 1

// Report 是一次运行的装配自报。
//
// 所有 "实际" 字段都取自运行期的活对象 (HookChain / Runner / Registry), 而不是
// 配置文件 —— 这是本类型的全部意义所在。配置原文另在 ConfigSource 里给出来源
// 路径, 两者刻意分开存: 混在一起就又会退化成"报告配置里写了什么"。
type Report struct {
	SchemaVersion int `json:"schema_version"`

	// RunID / SessionID 把这份报告与 trace、会话对上。
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`

	// TicketID / InstanceID 运维场景的索引维度 (题库 Q15): 出问题时人的检索习惯
	// 是"这个实例上周发生了什么"或"这张工单当时怎么判的", 而不是会话 ID。
	// 空表示本次运行不属任何工单 (交互式使用)。
	TicketID   string `json:"ticket_id,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`

	// ConfigSource 实际生效的配置来自查找链的哪一环。
	//
	// 查找链有六环 (--config > $CLAUDE_GO_CONFIG > ./claude-go.json >
	// ./config/claude-go.json > ~/.claude-go/config/config.json > ~/.claude-go/config.json),
	// 出问题时"你改的不是它读的那一份"是最常见的浪费。这里同时给路径与来源,
	// 面板上直接标出来。
	ConfigSource *ConfigSource `json:"config_source,omitempty"`

	// DeclarationVersion 本次运行**实际读到的**那份装配描述符的版本 (空 = 没读到)。
	//
	// 为什么自报要带上"声明"的版本号: 平台侧的对账是拿**当前**描述符与**这一次**
	// 的自报比。若档案在这之后改过, 平台签名发的已是新版本, 而这份自报来自旧版本 ——
	// 两个不同源的输入比出来的"差异"是幻觉, 人照着修只会越修越乱。带上版本号,
	// 消费侧才判得出"我手上的声明是不是这次跑的那一份"。
	//
	// 这仍是**观测**: 记录的是从配置里读到了什么, 不是"我同意它"。
	DeclarationVersion string `json:"declaration_version,omitempty"`

	// Model 实际生效的主模型与相位模型。
	Model *ModelInfo `json:"model,omitempty"`

	// Budget 实际生效的预算。零值字段表示该维度未设限 (不是"设成了 0")。
	Budget *BudgetInfo `json:"budget,omitempty"`

	// Tools 工具面的实际状态。
	Tools *ToolsInfo `json:"tools,omitempty"`

	// InternalHooks 内置 Hook 链的实际注册情况, 键是相位名。
	// **包含空相位** —— 见 pkg/engine/internal_hook.AllPhases 的说明。
	InternalHooks map[string][]string `json:"internal_hooks,omitempty"`

	// ExternalHooks 外部 hook 的实际装载情况 (来自配置, 由 Runner 装载)。
	ExternalHooks []ExternalHook `json:"external_hooks,omitempty"`

	// Prompt 系统提示词各段的实际字符数。
	Prompt *PromptInfo `json:"prompt,omitempty"`

	// Policy 权限与约束的实际状态。
	Policy *PolicyInfo `json:"policy,omitempty"`

	// Workspace 工作目录与 statestore 根 (会话隔离靠它)。
	Workspace *WorkspaceInfo `json:"workspace,omitempty"`

	// Components 前端优化组件的装配情况。
	//
	// 单独列出来是因为上面那个真实缺陷正好落在这里: 六个组件的 nil 与否决定了
	// 对应内置 hook 注册不注册, 而它取决于**走的哪条入口** (CLI vs 飞书),
	// 不取决于配置文件。把它做成显式字段, 对账才点得出名。
	Components map[string]bool `json:"components,omitempty"`

	// Warnings 装配期的可读提醒 (如"白名单为空, 一切工具将被拒绝")。
	// 这是给人看的, 不参与对账。
	Warnings []string `json:"warnings,omitempty"`
}

// ConfigSource 配置文件来源。
type ConfigSource struct {
	// Path 实际读到的配置文件路径; 空 = 六环都没命中, 走的是纯默认值。
	Path string `json:"path,omitempty"`
	// Origin 命中的是查找链的哪一环 (如 "flag --config" / "env CLAUDE_GO_CONFIG" /
	// "cwd ./claude-go.json" / "home ~/.claude-go/config.json" / "default")。
	Origin string `json:"origin,omitempty"`
	// Found 是否读到了文件。
	Found bool `json:"found"`
}

// ModelInfo 模型装配。
type ModelInfo struct {
	Primary   string `json:"primary,omitempty"`
	Execution string `json:"execution,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Fallback  string `json:"fallback,omitempty"`
}

// BudgetInfo 预算装配。0 = 未设限。
type BudgetInfo struct {
	MaxTurns      int `json:"max_turns,omitempty"`
	MaxTokens     int `json:"max_tokens,omitempty"`
	ContextWindow int `json:"context_window,omitempty"`
}

// ToolsInfo 工具面装配。
//
// 三个数都要报, 且**必须能区分"注册了但不可见"与"根本没注册"**: 前者是配置
// 收紧 (预期), 后者是能力缺失 (可能不预期)。只报一个总数会把这两种完全不同的
// 情况压成同一个数字。
type ToolsInfo struct {
	// Registered 注册表里的工具总数。
	Registered int `json:"registered"`
	// Visible 经白名单/黑名单过滤后模型实际可见的工具 (已排序)。
	Visible []string `json:"visible,omitempty"`
	// Hidden 被约束挡住的工具 (已排序)。
	Hidden []string `json:"hidden,omitempty"`
	// Whitelist 生效的白名单; nil = 未设白名单。
	// 与 WhitelistSet 配对才能表达 fail-closed 的空集语义。
	Whitelist []string `json:"whitelist,omitempty"`
	// WhitelistSet 是否设了白名单。true 且 Whitelist 为空 = 显式空名单 = 拒绝一切。
	WhitelistSet bool `json:"whitelist_set"`
	// Blacklist 生效的黑名单 (已排序)。
	Blacklist []string `json:"blacklist,omitempty"`
}

// ExternalHook 一条已装载的外部 hook。
type ExternalHook struct {
	Event        string `json:"event"`
	Type         string `json:"type"`
	Index        int    `json:"index"`
	HasCondition bool   `json:"has_condition,omitempty"`
	Timeout      int    `json:"timeout,omitempty"`
}

// PromptInfo 提示词各段的实际字符数。
//
// 字符数而不是 token 数: 这是**装配自报**不是计费报告, 字符数足够回答"那一段
// 到底有没有进去", 且不需要跑 tokenizer。缓存边界的可见性才是重点 —— 逐工单
// 变化的段必须排在最后, 否则它之后的前缀全部失效。
type PromptInfo struct {
	// Total 渲染后的系统提示词总字符数; 0 表示本次运行未采集到。
	Total int `json:"total,omitempty"`
	// Segments 各段名 → 字符数。
	Segments map[string]int `json:"segments,omitempty"`
	// VolatileTail 逐次变化的段名 (排在最末的那些)。空 = 未标注。
	VolatileTail []string `json:"volatile_tail,omitempty"`
}

// PolicyInfo 权限与约束。
type PolicyInfo struct {
	PermissionMode string `json:"permission_mode,omitempty"`
	// ConstraintOrigin 约束来源链 (如 "team:xxx/role:yyy"); 空 = 无外部约束。
	ConstraintOrigin string `json:"constraint_origin,omitempty"`
}

// WorkspaceInfo 工作目录。
type WorkspaceInfo struct {
	Cwd      string `json:"cwd,omitempty"`
	StateDir string `json:"state_dir,omitempty"`
}

// Fingerprint 返回报告的稳定短摘要 (取参与对账的字段, 不含时间与 run id)。
//
// 用途是"同一份装配两次跑出来是不是同一份": 面板上把 fingerprint 并排显示,
// 不一样就说明两次运行的装配有实质差异。刻意排除了 RunID/TicketID 这类每次都
// 变的字段 —— 否则 fingerprint 恒变, 一眼看去像"每次装配都不一样"而实际相同,
// 这种噪声会让整个指标失效。
func (r *Report) Fingerprint() string {
	if r == nil {
		return ""
	}
	// 只取装配实质: 模型、预算、工具面、两套 hook、策略、组件。
	//
	// 刻意**不含** DeclarationVersion: 那份描述符是别人签发给我的, 换一版描述符
	// 而装配结果一字未变时, 说"装配变了"是错的 —— fingerprint 回答的是"装成了
	// 什么", 不是"照谁的图纸装的"。
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(struct {
		Model         *ModelInfo          `json:"m"`
		Budget        *BudgetInfo         `json:"b"`
		Tools         *ToolsInfo          `json:"t"`
		InternalHooks map[string][]string `json:"ih"`
		ExternalHooks []ExternalHook      `json:"eh"`
		Policy        *PolicyInfo         `json:"p"`
		Components    map[string]bool     `json:"c"`
	}{r.Model, r.Budget, r.Tools, r.InternalHooks, r.ExternalHooks, r.Policy, r.Components})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ============================================================================
// 对账
// ============================================================================

// DiffKind 一处差异的种类。
type DiffKind string

const (
	// DiffDeclaredNotActual 声明了却没生效 —— 最危险的一类, 因为它看起来是装好的。
	DiffDeclaredNotActual DiffKind = "declared_not_actual"
	// DiffActualNotDeclared 生效了却没声明 —— 同样要报, 它意味着装配路径上有
	// 声明之外的来源 (默认值、另一条入口、残留配置)。
	DiffActualNotDeclared DiffKind = "actual_not_declared"
	// DiffMismatch 两边都在但取值不同 (如同一工具一 enable 一 deny)。
	DiffMismatch DiffKind = "mismatch"
)

// Diff 一处对账差异。
type Diff struct {
	Kind DiffKind `json:"kind"`
	// Facet 差异所在的能力面 (tools / external_hooks / internal_hooks / model /
	// budget / policy / components)。
	Facet string `json:"facet"`
	// Item 具体项名 (工具名 / "Event#Index" / 组件名)。
	Item string `json:"item"`
	// Declared / Actual 两侧的取值描述 (人可读)。
	Declared string `json:"declared,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// Declaration 是**声明**侧的输入: 平台认为它下发了一份怎样的装配。
//
// 刻意用独立的类型而不是复用 Report: 声明与自报是两份不同的东西, 让它们共用一个
// 结构, 就迟早会有人把声明当自报用 —— 而那正是本包要消灭的错误。
type Declaration struct {
	// Tools 岗位模板声明的工具白名单。nil = 未声明 (不参与对账),
	// 非 nil (含空切片) = 声明了白名单, 空切片即"应当拒绝一切"。
	Tools []string `json:"tools,omitempty"`
	// DisallowedTools 岗位模板声明的工具黑名单 (`disallowed-tools:`)。
	//
	// 与 Tools 分开成一段而不是合成一张"生效名单", 是因为两者能查的**不是同一件事**:
	//
	//	Tools (白名单) —— 声明侧是"应当可见的集合", 实际侧 (Visible) 是逐工具的
	//	                 真实可见性, 两个方向都能比。
	//	DisallowedTools  —— 声明侧是"应当不可见的集合", 实际侧有两份读数可用:
	//	                 ① Visible/Hidden (真的被挡住了没有); ② Tools.Blacklist
	//	                 (声明有没有走到 Config.DisabledTools)。两者都必要, 见
	//	                 reconcileDisallowed 里的两道关。
	//
	// 注意 Tools.Blacklist 与 DisallowedTools 是**两个来源**: 前者取自运行期的
	// engine.Config.DisabledTools (生效值), 后者取自资产 frontmatter (声明值)。
	// 一条经过团队/岗位约束的路径上, 生效值还会汇入调用方的约束链 —— 所以它们
	// 不一定相等, 相等也不是靠"同一份数据抄了两遍"保证的。
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
	// ExternalHooks 声明的外部 hook (按 "event#index" 标识)。
	ExternalHooks []ExternalHook `json:"external_hooks,omitempty"`
	// Components 声明应当装上的前端优化组件。
	Components []string `json:"components,omitempty"`
	// Model 声明的主模型。
	Model string `json:"model,omitempty"`
	// PermissionMode 声明的权限档。
	PermissionMode string `json:"permission_mode,omitempty"`
}

// ReconcileResult 一次对账的完整结论 (声明 + 差异 + 一句话结论)。
//
// 为什么三者打包而不是只回差异: 差异是**相对的** —— 同一份实际装配, 对着不同的
// 声明会得出不同的差异。只给差异, 读的人无法分辨"零差异"是真的对上了, 还是因为
// 声明本身就是空的 (一份什么都没声明的模板对谁都是零差异)。故声明必须同行。
type ReconcileResult struct {
	// Declared 本次对账用的声明侧; nil = 根本没有声明可对 (不等于"声明为空")。
	Declared *Declaration `json:"declared,omitempty"`
	// Diffs 逐项差异。**空数组与整段缺席是两回事**: 前者是"对过账, 一致",
	// 后者是"没对账"。故此处不加 omitempty, 由 NewReconcileResult 保证非 nil。
	Diffs []Diff `json:"diffs"`
	// Summary 一句话结论, 供面板与日志直接引用, 免得每处各写一套措辞。
	Summary string `json:"summary"`
}

// NewReconcileResult 跑一次对账并打包成可直接落盘/上报的结论。
func NewReconcileResult(decl *Declaration, actual *Report) *ReconcileResult {
	diffs := Reconcile(decl, actual)
	if diffs == nil {
		// 显式给空数组: nil 会序列化成 null, 消费侧难以与"没有这个语义"区分,
		// 而"对过账且一致"是一个确定的结论, 必须能被确定地读出来。
		diffs = []Diff{}
	}
	return &ReconcileResult{Declared: decl, Diffs: diffs, Summary: Summary(diffs)}
}

// Reconcile 把声明与实际逐项比对。
//
// 返回的差异**已排序** (种类 → 能力面 → 项名), 让两次对账的结果可比 —— 未排序
// 的话 map 遍历顺序会让同一份装配每次报出不同的顺序, 人看两次会以为发生了变化。
func Reconcile(decl *Declaration, actual *Report) []Diff {
	var diffs []Diff
	if actual == nil {
		return diffs
	}

	if decl != nil {
		diffs = append(diffs, reconcileTools(decl, actual)...)
		diffs = append(diffs, reconcileDisallowed(decl, actual)...)
		diffs = append(diffs, reconcileExternalHooks(decl, actual)...)
		diffs = append(diffs, reconcileComponents(decl, actual)...)
		diffs = append(diffs, reconcileScalars(decl, actual)...)
	}

	sort.SliceStable(diffs, func(i, j int) bool {
		if diffs[i].Kind != diffs[j].Kind {
			return diffs[i].Kind < diffs[j].Kind
		}
		if diffs[i].Facet != diffs[j].Facet {
			return diffs[i].Facet < diffs[j].Facet
		}
		return diffs[i].Item < diffs[j].Item
	})
	return diffs
}

// reconcileTools 对账工具白名单: 声明了却不可见 → DeclaredNotActual, 可见却未声明 → ActualNotDeclared。
// 声明为 nil 表示"未声明该面", 整个面不参与对账; 声明为空切片才是"空白名单 (拒绝一切)"。
func reconcileTools(decl *Declaration, actual *Report) []Diff {
	if decl.Tools == nil {
		return nil // 未声明白名单 ⇒ 不参与对账 (不是"声明了空名单")
	}
	if actual.Tools == nil {
		return []Diff{{
			Kind: DiffDeclaredNotActual, Facet: "tools", Item: "*",
			Declared: describeTools(decl.Tools), Actual: "本次运行未采集工具面",
		}}
	}

	var diffs []Diff
	declared := map[string]bool{}
	for _, t := range decl.Tools {
		declared[t] = true
	}
	visible := map[string]bool{}
	for _, t := range actual.Tools.Visible {
		visible[t] = true
	}

	for _, t := range decl.Tools {
		if !visible[t] {
			diffs = append(diffs, Diff{
				Kind: DiffDeclaredNotActual, Facet: "tools", Item: t,
				Declared: "白名单内", Actual: hiddenReason(actual.Tools, t),
			})
		}
	}
	for _, t := range actual.Tools.Visible {
		if !declared[t] {
			diffs = append(diffs, Diff{
				Kind: DiffActualNotDeclared, Facet: "tools", Item: t,
				Declared: "不在白名单", Actual: "对模型可见",
			})
		}
	}
	return diffs
}

// reconcileDisallowed 对账**黑名单** (`disallowed-tools:`)。
//
// ---------------------------------------------------------------------------
// 为什么黑名单需要自己的一段对账, 不能被 reconcileTools 捎带
// ---------------------------------------------------------------------------
//
// 白名单与黑名单是**两个方向**的声明, 能查的东西不一样:
//
//	白名单的问题形态是"少给了" —— 声明要给的没给。实际侧有逐工具的可见集 (Visible),
//	逐项一比就出来了。
//	黑名单的问题形态是"给了但你以为没给" —— 这才是危险的: 一份没接上的黑名单,
//	在装配读数上与"接上了且恰好没有那个工具"完全一样, 而作者以为自己关掉了它。
//
// 更要紧的是: 黑名单**没有**"声明之后就必然生效"的保证。白名单那边如果注册表里
// 没有那个工具, 报出来的是 `hiddenReason` 里那句"在生效白名单内却仍不可见" ——
// 至少两边都对同一条接线路说话。黑名单的声明 (frontmatter 解析出来的) 与实际
// (engine.Config.DisabledTools) 是**两个独立来源**, 中间那条线断了就是
// "声明了但没人读" —— 本仓最贵的那类缺陷。下面两道关就是为它设的。
//
// ---------------------------------------------------------------------------
// 两道关是**先后**的, 不是并排的两格
// ---------------------------------------------------------------------------
//
//	第一关 (真的没被看见?): 名字出现在 Visible 里 ⇒ 声明要挡却对模型可见 ⇒ 失败。
//	第二关 (声明真的到了吗?): 名字没进生效黑名单 ⇒ 接线断了 ⇒ 失败 (仅当第一关过)。
//
// 顺序不能倒。先查第二关的话, "接线到位、名字也进了黑名单、但过滤环节没生效"
// 这种情形会停在第一关之前被放过 —— 而它正是"写了但没用"最典型的形态。
// 反过来先查第一关, 任何"还看得见"的情况无论成因都会被拦下, 第二关只负责把
// 剩下的"看不见但也不知道为什么看不见"再筛一遍。
//
// 两关都过的情形里包含一种**恒真的空约束**: 声明的名字在本档位压根没注册
// (`disallowed-tools: [Bash]` + `tool-profile: analysis`)。它是作者在防御性地
// 关掉一个本就不该出现的工具 —— 正确、冗余、零成本。它进了 Config.DisabledTools
// (接线照走, 与注册与否无关), 所以两关都过、不产生差异。这是刻意的: 把它报成
// 问题会让人去改一个本来就对的地方。
//
// ---------------------------------------------------------------------------
// 刻意**不**报的反方向
// ---------------------------------------------------------------------------
//
// 生效黑名单里有、而本声明里没有的项 (reconcileTools 里那个 ActualNotDeclared
// 方向) 在这里不报。理由是: 生效值是**汇总值** —— 团队约束、岗位约束、CLI 旗标
// 都会往里汇 (见 ConstraintSet.CompiledToolDeny 的合并规则), 而 Declaration 只
// 带得动其中"资产自己声明"的那一份。照汇总值报差异, 得到的是"调用方还有别的
// 约束"这句话 —— 每个嵌套子代理都会报一遍, 而它不是缺陷。
//
// 这不是把反方向的检查省掉了 (那会得到一个半边空的对账), 而是认账: 这一面的
// 反方向没有可信的声明侧可对。白名单那边的反方向可信, 是因为 Visible 也是这次
// 运行的实际可见集、来源单一。
func reconcileDisallowed(decl *Declaration, actual *Report) []Diff {
	if decl.DisallowedTools == nil {
		return nil // 未声明黑名单 ⇒ 不参与对账 (不是"声明了空黑名单")
	}
	if actual.Tools == nil {
		return []Diff{{
			Kind: DiffDeclaredNotActual, Facet: "disallowed_tools", Item: "*",
			Declared: describeTools(decl.DisallowedTools), Actual: "本次运行未采集工具面",
		}}
	}

	visible := map[string]bool{}
	for _, t := range actual.Tools.Visible {
		visible[t] = true
	}
	blacklisted := map[string]bool{}
	for _, t := range actual.Tools.Blacklist {
		blacklisted[t] = true
	}

	var diffs []Diff
	for _, t := range decl.DisallowedTools {
		// 第一关: 声明要挡的, 真的看不见吗?
		if visible[t] {
			// 可见 ⇒ toolExposed 的黑名单分支没命中它 (那个分支排在白名单之前,
			// 见 pkg/engine/engine.go 的 toolExposed) ⇒ 一定是生效黑名单里没有它。
			// 白名单在这里能起的作用**只有"拦下"**: 同一个名字若既在白名单又在生效
			// 黑名单里, 结局是被挡住而不是可见, 所以"仍可见"不可能归因于两份名单打架。
			diffs = append(diffs, Diff{
				Kind: DiffDeclaredNotActual, Facet: "disallowed_tools", Item: t,
				Declared: "声明禁用",
				Actual:   "对模型可见 —— 未进入生效黑名单, 这条禁用没有生效",
			})
			continue
		}
		// 第二关: 看不见了, 但这是声明起的作用吗?
		if !blacklisted[t] {
			diffs = append(diffs, Diff{
				Kind: DiffDeclaredNotActual, Facet: "disallowed_tools", Item: t,
				Declared: "声明禁用",
				Actual:   "未进入生效黑名单 ⇒ 声明没有接到 Config.DisabledTools (注册表里也没有它, 所以看不出异常)",
			})
		}
	}
	return diffs
}

// hiddenReason 给出一个工具为什么不可见。
//
// 这两种成因必须分开说: 被黑名单拉黑是"这个工具不该用", 被白名单挡掉是"不在本
// 岗位职责内"。对账报一句笼统的"不可见", 读的人就得回去翻两份名单才知道该改哪个。
func hiddenReason(t *ToolsInfo, name string) string {
	for _, b := range t.Blacklist {
		if b == name {
			return "被黑名单拉黑"
		}
	}
	if t.WhitelistSet {
		// 名单里有没有它, 是两种完全不同的缺陷, 不能都说成"不在白名单内":
		//   - 在名单里却不可见 ⇒ 注册表里没有这个名字 (平台把工具名写错了 /
		//     该工具没注册到这条路径上)。修的是名字或注册, 不是名单。
		//   - 不在名单里       ⇒ 这个工具本就不该给这个岗位。修的是名单。
		// 报错文案指错方向时, 看的人会去改一个本来就对的地方 —— 而"改完了还是
		// 红的"会让整个对账面板失去可信度。
		for _, w := range t.Whitelist {
			if w == name {
				return "在生效白名单内却仍不可见 (注册表里没有同名工具?)"
			}
		}
		return "不在生效白名单内"
	}
	return "未注册 (注册表中不存在)"
}

// reconcileExternalHooks 对账外部 hook: 以 "事件#序号" 为键, 声明未装载/未声明被装载/类型不符各记一条。
func reconcileExternalHooks(decl *Declaration, actual *Report) []Diff {
	if decl.ExternalHooks == nil {
		return nil
	}
	key := func(h ExternalHook) string { return fmt.Sprintf("%s#%d", h.Event, h.Index) }

	actualSet := map[string]ExternalHook{}
	for _, h := range actual.ExternalHooks {
		actualSet[key(h)] = h
	}
	declSet := map[string]ExternalHook{}
	for _, h := range decl.ExternalHooks {
		declSet[key(h)] = h
	}

	var diffs []Diff
	for k, d := range declSet {
		a, ok := actualSet[k]
		if !ok {
			diffs = append(diffs, Diff{
				Kind: DiffDeclaredNotActual, Facet: "external_hooks", Item: k,
				Declared: d.Type, Actual: "未装载",
			})
			continue
		}
		if d.Type != "" && a.Type != "" && d.Type != a.Type {
			diffs = append(diffs, Diff{
				Kind: DiffMismatch, Facet: "external_hooks", Item: k,
				Declared: d.Type, Actual: a.Type,
			})
		}
	}
	for k, a := range actualSet {
		if _, ok := declSet[k]; !ok {
			diffs = append(diffs, Diff{
				Kind: DiffActualNotDeclared, Facet: "external_hooks", Item: k,
				Declared: "未声明", Actual: a.Type,
			})
		}
	}
	return diffs
}

// reconcileComponents 对账前端优化组件 —— 上面那个真实缺陷正落在这一面上。
func reconcileComponents(decl *Declaration, actual *Report) []Diff {
	if len(decl.Components) == 0 {
		return nil
	}
	var diffs []Diff
	for _, c := range decl.Components {
		if !actual.Components[c] {
			diffs = append(diffs, Diff{
				Kind: DiffDeclaredNotActual, Facet: "components", Item: c,
				Declared: "要求启用", Actual: "未装配 (nil, 对应 hook 未注册)",
			})
		}
	}
	return diffs
}

// reconcileScalars 对账单值面 (主模型 / 权限档): 双方都非空且不等才记 DiffMismatch。
func reconcileScalars(decl *Declaration, actual *Report) []Diff {
	var diffs []Diff
	if decl.Model != "" && actual.Model != nil && actual.Model.Primary != "" && decl.Model != actual.Model.Primary {
		diffs = append(diffs, Diff{
			Kind: DiffMismatch, Facet: "model", Item: "primary",
			Declared: decl.Model, Actual: actual.Model.Primary,
		})
	}
	if decl.PermissionMode != "" && actual.Policy != nil && actual.Policy.PermissionMode != "" &&
		decl.PermissionMode != actual.Policy.PermissionMode {
		diffs = append(diffs, Diff{
			Kind: DiffMismatch, Facet: "policy", Item: "permission_mode",
			Declared: decl.PermissionMode, Actual: actual.Policy.PermissionMode,
		})
	}
	return diffs
}

// describeTools 把工具名单渲染成对账报告里的一格: 空名单明说"拒绝一切", 长名单只列前三项。
func describeTools(tools []string) string {
	if len(tools) == 0 {
		return "空白名单 (拒绝一切)"
	}
	if len(tools) <= 3 {
		return strings.Join(tools, ",")
	}
	return fmt.Sprintf("%s 等 %d 项", strings.Join(tools[:3], ","), len(tools))
}

// Summary 给出对账的一句话结论 (供日志与面板标题用)。
//
// 无差异时也返回一句话而不是空串: "零差异"本身是要被看见的结论 —— 空串在面板上
// 与"还没对账"长得一样。
func Summary(diffs []Diff) string {
	if len(diffs) == 0 {
		return "装配一致: 声明与生效零差异"
	}
	var declaredNotActual, actualNotDeclared, mismatch int
	for _, d := range diffs {
		switch d.Kind {
		case DiffDeclaredNotActual:
			declaredNotActual++
		case DiffActualNotDeclared:
			actualNotDeclared++
		case DiffMismatch:
			mismatch++
		}
	}
	parts := make([]string, 0, 3)
	if declaredNotActual > 0 {
		parts = append(parts, fmt.Sprintf("声明未生效 %d", declaredNotActual))
	}
	if actualNotDeclared > 0 {
		parts = append(parts, fmt.Sprintf("未声明却生效 %d", actualNotDeclared))
	}
	if mismatch > 0 {
		parts = append(parts, fmt.Sprintf("取值不一致 %d", mismatch))
	}
	return "装配有差异: " + strings.Join(parts, " / ")
}
