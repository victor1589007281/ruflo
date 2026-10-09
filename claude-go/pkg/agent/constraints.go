package agent

// constraints.go —— design/01 §4.6「规则与约束: ConstraintSet 单一真源」。
//
// 为什么需要这个文件: 约束此前散在三处且各自为政 ——
//  1. pkg/feishu/session.go profileForTeamRole: 按**角色名子串**猜工具档位
//     (world-builder 因角色名含 "build" 被判成 coding 档, 从而拿到 Shell —— 真实误判);
//  2. pkg/engine engine.Config.DisabledTools/AllowedTools: 调用点各写一份裸 map 字面量;
//  3. cmd/claude-go/main.go 的 --allowed-tools / DisableTools 标志。
//
// 三处没有共同的类型, 于是「约束只能收窄不能放宽」这条安全属性**无处表达也无处
// 校验**。本文件提供承载体与合成规则; **执行点不变** —— 工具的可见性/可执行性
// 仍然只由 engine.Config.toolExposed 一个函数裁决 (design/01 §4.6「运行期强制
// 单点」), ConstraintSet 只负责「声明一次 + 单调合成 + 编译下发」, 不做第二次裁决。
//
// 四条设计取舍, 都是安全考虑:
//
//   - **nil 与空集语义不同**: Allow == nil 表示"不设白名单"; Allow == []string{}
//     表示"全部拒绝"。与 engine.Config.AllowedTools 的既有语义严格对齐 (fail-closed)。
//     ToolConstraint.Allow 故意**不带** omitempty: 空集若在 JSON 往返中被省掉,
//     "全拒"会静默退化成"不限制" —— 那是最典型的 fail-open 回归。
//
//   - **档位之间是偏序而非全序**: 六个档位的工具集并不互相包含 (research 有
//     WebFetch/WebSearch 而 coding 没有; analysis 有网络而 team 没有), 所以不能用
//     一个 int 等级比较"谁更严"。这里用特权位集合 (Capability) 做子集判定; 两个
//     档位不可比时 Narrow 直接拒绝 (fail-closed), 而不是猜一个。
//
//   - **角色名推断保留为最后回退**: 6 个下游平台的现行行为依赖它, 一刀切删掉等于
//     替生产改行为。ResolveToolProfile 只把"显式声明优先、用了回退就留痕"这条规则
//     实现一次; 各宿主把自己的启发式函数**作为 fallback 传进来** —— 启发式的实现
//     仍只有一份 (在宿主那边), 这里绝不复制, 否则就造出了第二个真源, 正是本文件
//     要消除的问题。
//
//   - **热路径零负担**: 本文件的方法都在"建会话 / 建 agent"时调用一次, 不在每轮
//     每工具的路径上。toolExposed 一个字节都没动, 仍是无分配的两次 map 查。
//
// 未接线的字段 (Paths/ModelTier/Conflicts/Provides/Requires) 是设计给出的承载位:
// 现在有类型、有单调性规则、有校验, 但运行期还没有消费者。**先有单一真源再谈接线**,
// 顺序反了就会变成第四处各自为政的约束。

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// ---------------------------------------------------------------------------
// 工具档位 (ToolProfile) 与特权位
// ---------------------------------------------------------------------------

// 工具档位名。与 pkg/tool/builtin 的 ToolProfile 常量取值逐字一致 —— 这里只需要
// 名字 (用于偏序判定), 具体每个档位注册哪些工具仍然只有 builtin/profile.go 一份
// 真源。constraints_test.go 会拿真实注册表反查本文件的特权位表, 防止两边漂移。
const (
	ProfileChat     = "chat"
	ProfileResearch = "research"
	ProfileCoding   = "coding"
	ProfileTeam     = "team"
	ProfileAnalysis = "analysis"
	ProfileAdmin    = "admin"
)

// Capability 特权维度: 档位偏序判定的一个轴。
//
// 只对"能造成外部影响"的能力立位 (读盘/写盘/执行/联网/写索引/改权限档/管任务/
// 打断用户)。纯输出成型类工具 (StructuredOutput / ToolSearch) 不占位 —— 它们不
// 扩大 agent 的能力边界, 若也算进来会让偏序退化到几乎两两不可比, 把 Narrow 变成
// 一个只会说"不"的函数。
type Capability uint32

const (
	CapReadFS     Capability = 1 << iota // Read / Glob / Grep
	CapWriteFS                           // Write / StrReplace / worktree 切换
	CapExec                              // Shell 及其后台任务管理 (TaskOutput/TaskStop)
	CapNet                               // WebFetch / WebSearch / 行情抓取
	CapReadIndex                         // code_intel_query / _status / _branch (只读图谱)
	CapWriteIndex                        // code_intel_init / _update (落盘建索引)
	CapPlan                              // EnterPlanMode / ExitPlanMode (会话权限档自改)
	CapTask                              // TodoWrite / Task* 任务板
	CapInteract                          // AskUserQuestion (打断并索取人类输入)
	CapMedia                             // Generate* 媒体产出
	CapTeam                              // TeamCreate / TeamDelete (团队编排)
	CapAdminExtra                        // LSP / Config / Skill / Cron* / ComputerUse* 等运维面
)

// CapAll 表示"没有任何上界"。admin 档位的历史行为就是全部内置工具, 用全 1 表达,
// 这样 admin 自动成为偏序的顶元素 (任何档位都可以收窄自 admin)。
const CapAll = Capability(0xFFFFFFFF)

// profileCaps 各档位的特权位。**派生自 pkg/tool/builtin/profile.go 的注册内容**,
// 不是独立事实; constraints_test.go 用真实注册表双向校验这张表 (表说可收窄 ⟺ 真实
// 特权工具集包含), 任何人往某档位加工具而忘了改这里, 测试会失败。
var profileCaps = map[string]Capability{
	ProfileChat:     CapReadIndex | CapInteract,
	ProfileAnalysis: CapReadFS | CapReadIndex | CapNet,
	ProfileTeam:     CapReadFS | CapReadIndex | CapWriteIndex | CapTask | CapInteract,
	ProfileResearch: CapReadFS | CapReadIndex | CapWriteIndex | CapTask | CapNet | CapInteract | CapPlan,
	ProfileCoding:   CapReadFS | CapWriteFS | CapExec | CapReadIndex | CapWriteIndex | CapTask | CapInteract | CapPlan,
	ProfileAdmin:    CapAll,
}

// ProfileCapabilities 返回档位的特权位; 第二个返回值为 false 表示档位名不认识。
func ProfileCapabilities(profile string) (Capability, bool) {
	c, ok := profileCaps[profile]
	return c, ok
}

// KnownProfiles 返回全部已识别的档位名 (无序)。
// 供外部测试核对"档位表与 pkg/tool/builtin 的常量没漂移" —— 那个核对必须在外部
// 测试包里做 (它要 import pkg/tool/builtin, 而 builtin 经 evotools → evolution/govern
// 反向依赖本包, 同包测试会构成导入环)。
func KnownProfiles() []string {
	out := make([]string, 0, len(profileCaps))
	for k := range profileCaps {
		out = append(out, k)
	}
	return out
}

// ProfileNarrows 报告 child 档位是否是 parent 档位的收窄 (相等也算收窄)。
// 任一档位名不认识时返回 false —— 认不出来就不放行 (fail-closed)。
func ProfileNarrows(child, parent string) bool {
	cc, ok1 := profileCaps[child]
	pc, ok2 := profileCaps[parent]
	if !ok1 || !ok2 {
		return false
	}
	return cc&^pc == 0 // child 的特权位没有一位落在 parent 之外
}

// NarrowerProfile 返回 a、b 中更窄的那个档位。两者不可比 (互不包含) 时返回
// ("", false) —— 调用方必须自己决定退到哪个已知安全的值, 而不是拿一个猜的。
func NarrowerProfile(a, b string) (string, bool) {
	if a == b {
		if _, ok := profileCaps[a]; ok {
			return a, true
		}
		return "", false
	}
	if ProfileNarrows(a, b) {
		return a, true
	}
	if ProfileNarrows(b, a) {
		return b, true
	}
	return "", false
}

// ProfileSource 档位是怎么定下来的 —— 审计与"退役角色名推断"的依据。
type ProfileSource string

const (
	ProfileSourceUnset ProfileSource = ""
	// ProfileSourceExplicit 显式声明 (图节点 AgentSpec.tool_profile / RoleDef 等)。
	ProfileSourceExplicit ProfileSource = "explicit"
	// ProfileSourceInherited 从父约束继承而来。
	ProfileSourceInherited ProfileSource = "inherited"
	// ProfileSourceRoleNameFallback ⚠️ 按角色名子串推断 —— design/01 §4.6 要求
	// 它只做缺省回退并留 deprecation 痕迹, 将来退役。
	ProfileSourceRoleNameFallback ProfileSource = "role_name_fallback"
	// ProfileSourceTextHeuristic 按用户输入文本启发式判定 (飞书主会话)。不是角色名
	// 推断, 不属于待退役对象, 但同样要可审计。
	ProfileSourceTextHeuristic ProfileSource = "text_heuristic"
)

// ---------------------------------------------------------------------------
// 约束子结构
// ---------------------------------------------------------------------------

// ToolConstraint 工具面约束。
type ToolConstraint struct {
	// Profile 工具档位; "" = 未声明, 由宿主回退推断 (见 ConstraintSet.ResolveToolProfile)。
	Profile string `json:"profile,omitempty"`
	// ProfileSource 档位来源 (审计)。
	ProfileSource ProfileSource `json:"profile_source,omitempty"`
	// Allow 工具白名单。**nil 与空集语义不同**, 与 engine.Config.AllowedTools 对齐:
	//   nil    = 不设白名单 (仅受 Deny 约束);
	//   非 nil = 仅名单内放行, **空集 = 全部拒绝**。
	// 不加 omitempty: 见文件头注释 (空集被省掉 = 全拒静默变不限制 = fail-open 回归)。
	Allow []string `json:"allow"`
	// Deny 工具黑名单; 与 Allow 冲突时黑名单胜 (沿用 toolExposed 的既有次序)。
	Deny []string `json:"deny,omitempty"`
}

// PathConstraint 路径沙箱。Roots 为 nil 表示不限制; 非 nil 表示只允许这些根之下。
type PathConstraint struct {
	ReadRoots  []string `json:"read_roots,omitempty"`
	WriteRoots []string `json:"write_roots,omitempty"`
	DenyGlobs  []string `json:"deny_globs,omitempty"`
}

// ModelBound 模型档位上下限 (design/01 §4.6 ModelTier)。
// 档位是抽象阶梯 small < standard < large, 具体模型名到档位的映射由宿主负责
// (尚未接线, 见文件头"未接线的字段")。
type ModelBound struct {
	MinTier string `json:"min_tier,omitempty"`
	MaxTier string `json:"max_tier,omitempty"`
}

// 模型档位阶梯。
const (
	ModelTierSmall    = "small"
	ModelTierStandard = "standard"
	ModelTierLarge    = "large"
)

var modelTierRank = map[string]int{ModelTierSmall: 0, ModelTierStandard: 1, ModelTierLarge: 2}

// Blocking 语义 (收编 WBS TaskNode.BlockingPolicy, 取值与 orchestrator.go 逐字一致)。
const (
	// BlockingFailOpen 节点失败不阻塞下游 —— "fail-open 的交付"。
	BlockingFailOpen = "fail_open"
	// BlockingFailBlocksDependents 节点失败阻塞下游 —— "fail-closed 的治理"。
	BlockingFailBlocksDependents = "fail_blocks_dependents"
)

// normalizeBlocking 归一 Blocking 取值。接受 "fail_closed" 作为
// fail_blocks_dependents 的别名: 设计文字里用 fail-open/fail-closed 对举, 而 WBS
// 线上值是 fail_blocks_dependents, 两种写法都会有人写。
func normalizeBlocking(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return ""
	case BlockingFailOpen, "fail-open":
		return BlockingFailOpen
	case BlockingFailBlocksDependents, "fail-blocks-dependents", "fail_closed", "fail-closed":
		return BlockingFailBlocksDependents
	default:
		return s // 原样返回, 由 Validate 报错; 不静默当成某一档
	}
}

// blockingStrictness Blocking 的严格度: 未声明 < fail_open < fail_blocks_dependents。
func blockingStrictness(s string) (int, bool) {
	switch normalizeBlocking(s) {
	case "":
		return 0, true
	case BlockingFailOpen:
		return 1, true
	case BlockingFailBlocksDependents:
		return 2, true
	default:
		return 0, false
	}
}

// permissionStrictness 权限档位的严格度阶梯 —— **全仓唯一一份**。
// engine 侧不再复制这张表 (见 engine.ToolConstraintSource.NarrowedPermissionMode
// 的注释), 否则又是一个第二真源。
//
// 排序理由 (由松到紧):
//
//	bypass      全部允许, 最松;
//	acceptEdits 文件编辑自动通过, 其余照问;
//	auto        只读放行, 写入需确认;
//	default     每次询问;
//	dontAsk     会触发询问的调用直接拒绝;
//	plan        只读, 写类工具一律拒绝, 最紧。
//
// "" (未设置) 视为 default 一档: 它落到 permissions 的"每次询问"基线上, 若当成
// "无约束"就会让 bypass 这种放宽声明被当作收窄接受 —— 那是 fail-open。
func permissionStrictness(mode string) (int, bool) {
	switch types.PermissionMode(mode) {
	case types.PermissionModeBypass:
		return 0, true
	case types.PermissionModeAcceptEdits:
		return 1, true
	case types.PermissionModeAuto:
		return 2, true
	case types.PermissionModeDefault, "":
		return 3, true
	case types.PermissionModeDontAsk:
		return 4, true
	case types.PermissionModePlan:
		return 5, true
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// ConstraintSet
// ---------------------------------------------------------------------------

// ConstraintSet 工具白名单/路径/权限/契约/Blocking 语义的单一声明处
// (design/01 §4.6)。字段布局与设计文档一致。
//
// 所有方法都对 nil 接收者安全: 上游"没有父约束"的情形非常普遍 (今天生产上就是
// 全部如此), 让调用方不必到处判空。
type ConstraintSet struct {
	Tools      *ToolConstraint `json:"tools,omitempty"`
	Paths      *PathConstraint `json:"paths,omitempty"`
	Permission string          `json:"permission,omitempty"`
	Conflicts  []string        `json:"conflict_keys,omitempty"`
	Provides   []string        `json:"provides,omitempty"`
	Requires   []string        `json:"requires,omitempty"`
	Blocking   string          `json:"blocking,omitempty"`
	ModelTier  *ModelBound     `json:"model_tier,omitempty"`

	// Origin 本层声明来自哪 —— 审计用, 也是 deprecation 日志的定位信息。
	// 例: "cli-flags" / "feishu-session" / "team-role:world-builder"。
	Origin string `json:"origin,omitempty"`
	// Lineage 继承链: 从最外层父到直接父的 Origin 序列, Narrow 时累加。
	// 这就是"能表达从哪继承而来"。
	Lineage []string `json:"lineage,omitempty"`
}

// NewConstraintSet 建一个空约束集 (什么都不限制), 只带来源标记。
// **空 = 不限制**是刻意的: 现有工作流/角色/下游平台不声明任何约束, 默认必须与
// 改造前逐项等价, 不能因为引入了约束类型就凭空变严。
func NewConstraintSet(origin string) *ConstraintSet {
	return &ConstraintSet{Origin: origin}
}

// WithToolProfile 声明显式工具档位。profile 为空时是 no-op (保持"未声明"),
// 这样调用方可以无脑把可能为空的声明值传进来。
func (cs *ConstraintSet) WithToolProfile(profile string) *ConstraintSet {
	if cs == nil || profile == "" {
		return cs
	}
	cs.ensureTools()
	cs.Tools.Profile = profile
	cs.Tools.ProfileSource = ProfileSourceExplicit
	return cs
}

// WithDeniedTools 追加工具黑名单。
func (cs *ConstraintSet) WithDeniedTools(names ...string) *ConstraintSet {
	if cs == nil || len(names) == 0 {
		return cs
	}
	cs.ensureTools()
	cs.Tools.Deny = appendUniqueStrings(cs.Tools.Deny, names...)
	return cs
}

// WithToolAllowList 设置工具白名单。
// 参数是 slice 而不是可变参数, 因为 nil 与空集必须能被分别表达:
// nil = 不设白名单; []string{} = 全部拒绝。可变参数写法下 f() 到底是哪一个,
// 调用方无从表达, 而这两者的差别正是 fail-closed 的关键。
func (cs *ConstraintSet) WithToolAllowList(names []string) *ConstraintSet {
	if cs == nil {
		return cs
	}
	cs.ensureTools()
	if names == nil {
		cs.Tools.Allow = nil
		return cs
	}
	cs.Tools.Allow = appendUniqueStrings([]string{}, names...)
	return cs
}

// WithPermission 声明权限档位。
func (cs *ConstraintSet) WithPermission(mode string) *ConstraintSet {
	if cs == nil {
		return cs
	}
	cs.Permission = mode
	return cs
}

// WithBlocking 声明 Blocking 语义 (fail_open / fail_blocks_dependents)。
func (cs *ConstraintSet) WithBlocking(b string) *ConstraintSet {
	if cs == nil {
		return cs
	}
	cs.Blocking = normalizeBlocking(b)
	return cs
}

func (cs *ConstraintSet) ensureTools() {
	if cs.Tools == nil {
		cs.Tools = &ToolConstraint{}
	}
}

// ResolveToolProfile 决定生效档位: **显式声明优先, 没有声明才回退**。
//
// fallback 是宿主自己的启发式 (profileForTeamRole / profileForRunOptions /
// inferFeishuToolProfile) —— 本函数只负责优先级与留痕, 绝不复制它们的匹配逻辑。
// fallbackSource 说明这次回退属于哪一类 (角色名子串推断 / 文本启发式), 调用方据此
// 决定是否打 deprecation 日志。
//
// 副作用是刻意的: 回退结果会被写回本约束集, 于是"决策之后"整个进程里只有一个
// 地方持有生效档位与它的来源, 后续 Narrow / 审计都基于同一份事实。
func (cs *ConstraintSet) ResolveToolProfile(fallbackSource ProfileSource, fallback func() string) (string, ProfileSource) {
	if cs == nil {
		if fallback == nil {
			return "", ProfileSourceUnset
		}
		return fallback(), fallbackSource
	}
	if cs.Tools != nil && cs.Tools.Profile != "" {
		src := cs.Tools.ProfileSource
		if src == ProfileSourceUnset {
			src = ProfileSourceExplicit
			cs.Tools.ProfileSource = src
		}
		return cs.Tools.Profile, src
	}
	if fallback == nil {
		return "", ProfileSourceUnset
	}
	v := fallback()
	cs.ensureTools()
	cs.Tools.Profile = v
	cs.Tools.ProfileSource = fallbackSource
	return v, fallbackSource
}

// ToolProfile 返回当前生效档位 ("" = 未决定)。
func (cs *ConstraintSet) ToolProfile() string {
	if cs == nil || cs.Tools == nil {
		return ""
	}
	return cs.Tools.Profile
}

// ToolProfileSource 返回档位来源。
func (cs *ConstraintSet) ToolProfileSource() ProfileSource {
	if cs == nil || cs.Tools == nil {
		return ProfileSourceUnset
	}
	return cs.Tools.ProfileSource
}

// ---------------------------------------------------------------------------
// 编译下发: engine.ToolConstraintSource 的结构性实现
// ---------------------------------------------------------------------------
//
// 为什么用"结构性满足接口"而不是互相 import: pkg/engine 是每轮每工具都要过的
// 热路径底层包, 让它反向依赖 pkg/agent 那棵重依赖树 (orchestrator/swarm_intel/
// media/...) 是架构上的倒挂, 而且一旦将来 pkg/agent 需要 import pkg/engine 就是
// 依赖环。ConstraintSet 只提供扁平的读方法, engine 定义最小接口, 两边零 import。

// CompiledToolAllow 编译后的白名单。nil = 不设白名单; 非 nil (含空 map) = 仅名单内
// 放行, 空 map 即全部拒绝 —— 语义与 engine.Config.AllowedTools 严格一致。
//
// 每次调用分配一个 map: 只在建会话/建 agent 时调用一次, 不在热路径上。
func (cs *ConstraintSet) CompiledToolAllow() map[string]bool {
	if cs == nil || cs.Tools == nil || cs.Tools.Allow == nil {
		return nil
	}
	out := make(map[string]bool, len(cs.Tools.Allow))
	for _, n := range cs.Tools.Allow {
		out[n] = true
	}
	return out
}

// CompiledToolDeny 编译后的黑名单 (nil = 不禁任何工具)。
func (cs *ConstraintSet) CompiledToolDeny() map[string]bool {
	if cs == nil || cs.Tools == nil || len(cs.Tools.Deny) == 0 {
		return nil
	}
	out := make(map[string]bool, len(cs.Tools.Deny))
	for _, n := range cs.Tools.Deny {
		out[n] = true
	}
	return out
}

// NarrowedPermissionMode 给定引擎当前权限档, 返回应下发的档位; "" = 不覆盖。
//
// 契约 (engine 依赖它, 因为严格度阶梯只有本文件一份): 返回值只可能是 current
// 或**比 current 更严**的一档。约束声明的档位若比 current 松, 或压根不认识,
// 一律返回 "" —— 不覆盖 = 保留更严的现状 (fail-closed)。
func (cs *ConstraintSet) NarrowedPermissionMode(current string) string {
	if cs == nil || cs.Permission == "" {
		return ""
	}
	want, okWant := permissionStrictness(cs.Permission)
	cur, okCur := permissionStrictness(current)
	if !okWant || !okCur {
		return ""
	}
	if want <= cur {
		return "" // 相等无需下发; 更松则拒绝
	}
	return cs.Permission
}

// ConstraintOrigin 审计: 本约束的来源链, 形如 "cli-flags > team-role:coder"。
func (cs *ConstraintSet) ConstraintOrigin() string {
	if cs == nil {
		return ""
	}
	if len(cs.Lineage) == 0 {
		return cs.Origin
	}
	parts := append(append([]string{}, cs.Lineage...), cs.Origin)
	return strings.Join(parts, " > ")
}

// AuditTrail 人可读的一行审计串: 来源链 + 生效档位及其来源。
func (cs *ConstraintSet) AuditTrail() string {
	if cs == nil {
		return "<nil constraints>"
	}
	s := cs.ConstraintOrigin()
	if p := cs.ToolProfile(); p != "" {
		s += fmt.Sprintf(" [profile=%s via %s]", p, cs.ToolProfileSource())
	}
	if cs.Permission != "" {
		s += fmt.Sprintf(" [perm=%s]", cs.Permission)
	}
	if cs.Tools != nil && cs.Tools.Allow != nil {
		s += fmt.Sprintf(" [allow=%d]", len(cs.Tools.Allow))
	}
	if cs.Tools != nil && len(cs.Tools.Deny) > 0 {
		s += fmt.Sprintf(" [deny=%d]", len(cs.Tools.Deny))
	}
	return s
}

// ---------------------------------------------------------------------------
// 单调收窄
// ---------------------------------------------------------------------------

// Narrow 把 child 叠加到 cs (父) 上, 返回合成后的新集合; cs 与 child 都不被修改。
//
// **只允许收窄**: child 任何试图放宽的声明都会被拒绝并返回错误 —— 这是安全属性,
// 不是尽力而为。放宽包括:
//   - 工具档位的特权位超出父档位, 或与父档位不可比 (认不出关系就不放行);
//   - 白名单里出现父白名单之外的工具;
//   - 请求比父更松的权限档;
//   - 路径根跳出父允许的根;
//   - 把 fail_blocks_dependents 降级为 fail_open;
//   - 抬高模型档位上限 / 压低下限。
//
// 调用方必须把 error 当作**拒绝**处理 (保留父约束或直接失败), 而不是"退而用
// child" —— 后者等于把单调性变成一句注释。为此错误时第一个返回值是 nil。
//
// 契约类字段 (Conflicts/Provides/Requires) 与黑名单/DenyGlobs 取并集: 它们只会
// 让约束更多, 不构成放宽。
func (cs *ConstraintSet) Narrow(child *ConstraintSet) (*ConstraintSet, error) {
	if child == nil {
		return cs.Clone(), nil
	}
	if cs == nil {
		// 无父约束: child 原样成立 (今天生产上的常态), 但仍要自校验。
		out := child.Clone()
		if err := out.Validate(); err != nil {
			return nil, err
		}
		return out, nil
	}
	if err := cs.Validate(); err != nil {
		return nil, fmt.Errorf("父约束不合法: %w", err)
	}
	if err := child.Validate(); err != nil {
		return nil, fmt.Errorf("子约束不合法: %w", err)
	}

	var widen []string
	out := cs.Clone()
	// 继承链: 父的链 + 父自身, 当前层来源换成 child 的。
	out.Lineage = appendUniqueStrings(append([]string{}, cs.Lineage...), cs.Origin)
	out.Origin = child.Origin

	// --- 工具 ---
	if child.Tools != nil {
		out.ensureTools()
		// 档位: 子档位必须是父档位的收窄。父未声明档位 = 无上界, 子自由。
		if child.Tools.Profile != "" {
			parentProfile := ""
			if cs.Tools != nil {
				parentProfile = cs.Tools.Profile
			}
			switch {
			case parentProfile == "":
				out.Tools.Profile = child.Tools.Profile
				out.Tools.ProfileSource = child.Tools.ProfileSource
			case ProfileNarrows(child.Tools.Profile, parentProfile):
				out.Tools.Profile = child.Tools.Profile
				out.Tools.ProfileSource = child.Tools.ProfileSource
			default:
				widen = append(widen, fmt.Sprintf("工具档位 %s 不是 %s 的收窄", child.Tools.Profile, parentProfile))
			}
		} else if out.Tools.Profile != "" {
			// 子未声明: 继承父档位, 来源标记为 inherited (审计上能看出不是自己声明的)。
			out.Tools.ProfileSource = ProfileSourceInherited
		}

		// 白名单: 父 nil = 无名单, 子的名单直接生效 (设名单本身就是收窄);
		// 父非 nil = 子只能取子集, 越界项即放宽。
		if child.Tools.Allow != nil {
			if cs.Tools == nil || cs.Tools.Allow == nil {
				out.Tools.Allow = appendUniqueStrings([]string{}, child.Tools.Allow...)
			} else {
				parentAllow := make(map[string]bool, len(cs.Tools.Allow))
				for _, n := range cs.Tools.Allow {
					parentAllow[n] = true
				}
				kept := []string{}
				var escaped []string
				for _, n := range child.Tools.Allow {
					if parentAllow[n] {
						kept = appendUniqueStrings(kept, n)
					} else {
						escaped = append(escaped, n)
					}
				}
				if len(escaped) > 0 {
					sort.Strings(escaped)
					widen = append(widen, "白名单越界工具: "+strings.Join(escaped, ","))
				}
				out.Tools.Allow = kept
			}
		}
		// 黑名单: 并集, 只增不减。
		out.Tools.Deny = appendUniqueStrings(out.Tools.Deny, child.Tools.Deny...)
	} else if out.Tools != nil && out.Tools.Profile != "" {
		// 子完全没声明工具约束: 整块继承父的, 来源标 inherited 以便审计区分
		// "自己声明的" 与 "继承来的"。
		out.Tools.ProfileSource = ProfileSourceInherited
	}

	// --- 权限档 ---
	if child.Permission != "" {
		cur, okCur := permissionStrictness(cs.Permission)
		want, okWant := permissionStrictness(child.Permission)
		switch {
		case !okWant:
			widen = append(widen, "权限档不可识别: "+child.Permission)
		case !okCur:
			widen = append(widen, "父权限档不可识别: "+cs.Permission)
		case want >= cur:
			out.Permission = child.Permission
		default:
			widen = append(widen, fmt.Sprintf("权限档 %s 比父 %s 更松", child.Permission, cs.Permission))
		}
	}

	// --- 路径沙箱 ---
	if child.Paths != nil {
		if out.Paths == nil {
			out.Paths = &PathConstraint{}
		}
		var parentRead, parentWrite []string
		if cs.Paths != nil {
			parentRead, parentWrite = cs.Paths.ReadRoots, cs.Paths.WriteRoots
		}
		if roots, esc := narrowRoots(parentRead, child.Paths.ReadRoots); len(esc) > 0 {
			widen = append(widen, "读路径跳出父沙箱: "+strings.Join(esc, ","))
		} else {
			out.Paths.ReadRoots = roots
		}
		if roots, esc := narrowRoots(parentWrite, child.Paths.WriteRoots); len(esc) > 0 {
			widen = append(widen, "写路径跳出父沙箱: "+strings.Join(esc, ","))
		} else {
			out.Paths.WriteRoots = roots
		}
		out.Paths.DenyGlobs = appendUniqueStrings(out.Paths.DenyGlobs, child.Paths.DenyGlobs...)
	}

	// --- Blocking ---
	if child.Blocking != "" {
		cur, okCur := blockingStrictness(cs.Blocking)
		want, okWant := blockingStrictness(child.Blocking)
		switch {
		case !okWant:
			widen = append(widen, "Blocking 取值不可识别: "+child.Blocking)
		case !okCur:
			widen = append(widen, "父 Blocking 取值不可识别: "+cs.Blocking)
		case want >= cur:
			out.Blocking = normalizeBlocking(child.Blocking)
		default:
			widen = append(widen, fmt.Sprintf("Blocking %s 比父 %s 更松", child.Blocking, cs.Blocking))
		}
	}

	// --- 模型档位 ---
	if child.ModelTier != nil {
		merged, err := narrowModelBound(cs.ModelTier, child.ModelTier)
		if err != nil {
			widen = append(widen, err.Error())
		} else {
			out.ModelTier = merged
		}
	}

	// --- 契约类: 并集 ---
	out.Conflicts = appendUniqueStrings(out.Conflicts, child.Conflicts...)
	out.Provides = appendUniqueStrings(out.Provides, child.Provides...)
	out.Requires = appendUniqueStrings(out.Requires, child.Requires...)

	if len(widen) > 0 {
		return nil, fmt.Errorf("约束单调性违规 (%s → %s): %s",
			cs.ConstraintOrigin(), child.Origin, strings.Join(widen, "; "))
	}
	return out, nil
}

// narrowRoots 计算子根集合相对父根集合的收窄; 返回越界的子根 (非空即放宽)。
// 父为 nil = 不限制, 子根全部成立。子为 nil = 继承父。
func narrowRoots(parent, child []string) (roots []string, escaped []string) {
	if child == nil {
		return parent, nil
	}
	if parent == nil {
		return appendUniqueStrings([]string{}, child...), nil
	}
	kept := []string{}
	for _, c := range child {
		if pathWithinAny(c, parent) {
			kept = appendUniqueStrings(kept, c)
		} else {
			escaped = append(escaped, c)
		}
	}
	sort.Strings(escaped)
	return kept, escaped
}

// pathWithinAny 报告 p 是否落在 roots 中某个根之下 (含相等)。
// 用 filepath.Clean 后按分隔符边界比对, 避免 "/srv/app" 被 "/srv/appx" 误判为父。
func pathWithinAny(p string, roots []string) bool {
	cp := filepath.Clean(p)
	for _, r := range roots {
		cr := filepath.Clean(r)
		if cp == cr {
			return true
		}
		if strings.HasPrefix(cp, strings.TrimSuffix(cr, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// narrowModelBound 模型档位区间求交; 子区间跳出父区间即放宽。
func narrowModelBound(parent, child *ModelBound) (*ModelBound, error) {
	if child == nil {
		return parent, nil
	}
	rank := func(t string) (int, bool) {
		if t == "" {
			return 0, true
		}
		r, ok := modelTierRank[t]
		return r, ok
	}
	cMin, okCMin := rank(child.MinTier)
	cMax, okCMax := rank(child.MaxTier)
	if !okCMin || !okCMax {
		return nil, fmt.Errorf("模型档位不可识别: min=%q max=%q", child.MinTier, child.MaxTier)
	}
	out := &ModelBound{MinTier: child.MinTier, MaxTier: child.MaxTier}
	if parent == nil {
		return out, nil
	}
	pMin, okPMin := rank(parent.MinTier)
	pMax, okPMax := rank(parent.MaxTier)
	if !okPMin || !okPMax {
		return nil, fmt.Errorf("父模型档位不可识别: min=%q max=%q", parent.MinTier, parent.MaxTier)
	}
	if parent.MaxTier != "" && (child.MaxTier == "" || cMax > pMax) {
		return nil, fmt.Errorf("模型档位上限 %q 高于父 %q", child.MaxTier, parent.MaxTier)
	}
	if parent.MinTier != "" && child.MinTier != "" && cMin < pMin {
		return nil, fmt.Errorf("模型档位下限 %q 低于父 %q", child.MinTier, parent.MinTier)
	}
	if out.MinTier == "" {
		out.MinTier = parent.MinTier
	}
	return out, nil
}

// Validate 编译期校验 (design/01 §4.6「编译期校验」的约束冲突部分)。
// 只校验本层声明的自相矛盾与不可识别取值; 跨节点的死锁/占位符校验属于
// GraphSpec.Validate 的职责, 不在这里。
func (cs *ConstraintSet) Validate() error {
	if cs == nil {
		return nil
	}
	var errs []string
	if cs.Tools != nil {
		if cs.Tools.Profile != "" {
			if _, ok := profileCaps[cs.Tools.Profile]; !ok {
				errs = append(errs, "工具档位不可识别: "+cs.Tools.Profile)
			}
		}
		if cs.Tools.Allow != nil && len(cs.Tools.Deny) > 0 {
			deny := make(map[string]bool, len(cs.Tools.Deny))
			for _, n := range cs.Tools.Deny {
				deny[n] = true
			}
			var both []string
			for _, n := range cs.Tools.Allow {
				if deny[n] {
					both = append(both, n)
				}
			}
			if len(both) > 0 {
				sort.Strings(both)
				// 不是致命错误 (toolExposed 里黑名单本就胜过白名单), 但一定是写错了:
				// 声明者以为放开了, 实际被拒。宁可让它编译期就炸出来。
				errs = append(errs, "工具同时在白名单与黑名单: "+strings.Join(both, ","))
			}
		}
	}
	if cs.Permission != "" {
		if _, ok := permissionStrictness(cs.Permission); !ok {
			errs = append(errs, "权限档不可识别: "+cs.Permission)
		}
	}
	if cs.Blocking != "" {
		if _, ok := blockingStrictness(cs.Blocking); !ok {
			errs = append(errs, "Blocking 取值不可识别: "+cs.Blocking)
		}
	}
	if cs.ModelTier != nil {
		minR, okMin := modelTierRank[cs.ModelTier.MinTier]
		maxR, okMax := modelTierRank[cs.ModelTier.MaxTier]
		if cs.ModelTier.MinTier != "" && !okMin {
			errs = append(errs, "模型档位下限不可识别: "+cs.ModelTier.MinTier)
		}
		if cs.ModelTier.MaxTier != "" && !okMax {
			errs = append(errs, "模型档位上限不可识别: "+cs.ModelTier.MaxTier)
		}
		if okMin && okMax && cs.ModelTier.MinTier != "" && cs.ModelTier.MaxTier != "" && minR > maxR {
			errs = append(errs, "模型档位下限高于上限")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("约束不合法 (%s): %s", cs.ConstraintOrigin(), strings.Join(errs, "; "))
	}
	return nil
}

// Clone 深拷贝 (Narrow 不修改任何入参, 靠它保证)。
func (cs *ConstraintSet) Clone() *ConstraintSet {
	if cs == nil {
		return nil
	}
	out := *cs
	if cs.Tools != nil {
		t := *cs.Tools
		// Allow 必须保住 nil / 空集的区别, 不能一律 append 成非 nil。
		if cs.Tools.Allow != nil {
			t.Allow = append([]string{}, cs.Tools.Allow...)
		}
		if len(cs.Tools.Deny) > 0 {
			t.Deny = append([]string{}, cs.Tools.Deny...)
		}
		out.Tools = &t
	}
	if cs.Paths != nil {
		p := *cs.Paths
		if cs.Paths.ReadRoots != nil {
			p.ReadRoots = append([]string{}, cs.Paths.ReadRoots...)
		}
		if cs.Paths.WriteRoots != nil {
			p.WriteRoots = append([]string{}, cs.Paths.WriteRoots...)
		}
		if len(cs.Paths.DenyGlobs) > 0 {
			p.DenyGlobs = append([]string{}, cs.Paths.DenyGlobs...)
		}
		out.Paths = &p
	}
	if cs.ModelTier != nil {
		m := *cs.ModelTier
		out.ModelTier = &m
	}
	if len(cs.Conflicts) > 0 {
		out.Conflicts = append([]string{}, cs.Conflicts...)
	}
	if len(cs.Provides) > 0 {
		out.Provides = append([]string{}, cs.Provides...)
	}
	if len(cs.Requires) > 0 {
		out.Requires = append([]string{}, cs.Requires...)
	}
	if len(cs.Lineage) > 0 {
		out.Lineage = append([]string{}, cs.Lineage...)
	}
	return &out
}

// appendUniqueStrings 追加去重 (保持首次出现顺序, 便于审计串稳定)。
func appendUniqueStrings(dst []string, add ...string) []string {
	if len(add) == 0 {
		return dst
	}
	seen := make(map[string]bool, len(dst)+len(add))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range add {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		dst = append(dst, v)
	}
	return dst
}

// ---------------------------------------------------------------------------
// StageTools —— stage 级的工具声明 (21.8.12 P3 / W5)
// ---------------------------------------------------------------------------
//
// W5 的原文: "把 profileForTeamRole 的名字猜测改成 StageDef.Tools 显式声明 ——
// 让'能力从哪来'可读, 而不是靠字符串包含"。
//
// 那套名字猜测有两处真实误判, 都不是理论问题:
//
//	world-builder  含 "build" ⇒ 被判成 coding 档 ⇒ 拿到 Shell。一个写世界观设定的
//	               角色因此能执行任意命令。
//	tech-investigator 含 "investigat" 但不含 "research"/"review" ⇒ 落到 team 档,
//	               而那是一个**只读调研**角色。
//
// 改成显式声明之后, "这个阶段能拿什么工具"写在定义里, 读得到、能审、不随名字变。
//
// ---------------------------------------------------------------------------
// 为什么是"三态"而不是两个字段
// ---------------------------------------------------------------------------
//
// 需要能分开表达三件事, 而它们不能合并:
//
//	没写            ⇒ 与改造前逐项等价 (仍走名字猜测), 6 个下游平台行为一字节不变
//	写了档位        ⇒ 明确的能力画像, 取代猜测
//	写了 tools 名单 ⇒ 精确到工具的边界
//
// 用一个 `Tools []string` 加一个 `ToolProfile string` 是做不到的 ——
// Go 的零值会把"没写"和"写了空"压成同一个 (len==0 / "")。这正是
// pkg/agentbind 里 HasTools/HasDisallowedTools 那对存在性位的同一个理由, 也是
// `tools: []` 在 P2′ 差一点退化成"完全放行"的原因。所以这里显式带存在性:
// 用指针表达"键写没写", 与 pkg/sessionapi.AllowedTools 是同一手法。
//
// nil 与空切片的差别在这里有**实际后果**, 不是风格问题:
//
//	Tools == nil  ⇒ 没声明白名单 (不限制)
//	Tools == &[]{} ⇒ 显式空白名单 ⇒ fail-closed, 拒绝一切工具
//
// 后者在 ApplyStageTools 里经 WithToolAllowList 走 CompiledToolAllow, 而那里
// `Allow != nil` 即"仅名单内放行, 空 map 即全部拒绝" —— 语义与
// engine.Config.AllowedTools 严格一致, 不在本层二次解释。
type StageTools struct {
	// Profile 显式工具档位 (chat|research|coding|team|analysis|admin)。
	// 空 = 未声明 ⇒ 回退到宿主的角色名推断, 并留痕 (ProfileSourceRoleNameFallback)。
	Profile string `json:"profile,omitempty"`
	// Allow 工具白名单。**指针**: nil = 没写这一栏; &[] = 显式空名单 (拒绝一切)。
	// 与 Profile 是两件事, 各自独立生效 —— 同时写时先定档位再取交集,
	// 按构造只会更窄 (见 ApplyStageTools)。
	Allow *[]string `json:"tools,omitempty"`
	// Deny 工具黑名单 (并集, 只会让可见面更小)。
	Deny []string `json:"disallowedTools,omitempty"`
}

// Declares 报告这份声明是否真的写了东西。
//
// 存在的理由: 调用方要能区分"这个 stage 声明了工具面"与"它的 StageTools 是零值"。
// 判据必须是**逐键**(Allow 看指针非 nil), 不能是 `!= StageTools{}` —— 后者在
// 有人往结构体里加了切片字段之后就不可比较, 编译期才发现, 而那时的修法往往是
// 随手改成 len() 判断, 于是 `tools: []` 又被吞掉一次。
func (st *StageTools) Declares() bool {
	if st == nil {
		return false
	}
	return st.Profile != "" || st.Allow != nil || len(st.Deny) > 0
}

// ApplyStageTools 把 stage 的显式工具声明接进约束集 —— **唯一**的应用点。
//
// 只有一处是刻意的。同一条声明有两条送达路径时, 拆掉任意一条都不改变行为: 一份
// "看起来接了、实际可以整条删掉"的接线比明摆着没接更难发现。这个坑在本仓已经踩过
// 一次 (assetProfile 同时经 WithToolProfile 与 fallback 闭包送达, 删掉任一条测试
// 依然全绿 —— 见 agent_assets.go 文件头), 所以这里不再重演:
// 工作流路径 (pkg/feishu/session.go 的 teamRole 分支) 与图路径 (stageNodeRunner)
// 都调**这一个**函数, 没有第二个写者。
//
// 返回值是"哪些字段真的应用了", 供调用方记审计 —— 静默应用与静默不应用都是本仓
// 明令要避免的形态。
func ApplyStageTools(cs *ConstraintSet, st *StageTools) []string {
	if cs == nil || st == nil || !st.Declares() {
		return nil
	}
	var applied []string
	if st.Profile != "" {
		// 经 WithToolProfile ⇒ ProfileSourceExplicit。于是 ResolveToolProfile 会
		// 走"显式优先"分支, 名字猜测那条 fallback 闭包根本不会被调用 —— 这不是
		// 巧合, 是 W5 的实现方式: 旧路径留着 (下游 platform 不声明时仍用它), 而
		// 声明一出现, 它就自动退居其次。
		cs.WithToolProfile(st.Profile)
		applied = append(applied, "profile="+st.Profile)
	}
	if st.Allow != nil {
		// 复制而不是传引用: 约束集会被 Narrow/审计读很多次, 而 StageDef 的那份
		// 声明属于注册表, 让两者共享底层数组意味着任何一侧的原地修改都会串台。
		// 空切片要显式构造成非 nil —— WithToolAllowList(nil) 是 no-op。
		if len(*st.Allow) == 0 {
			cs.WithToolAllowList([]string{})
			applied = append(applied, "tools=<empty/deny-all>")
		} else {
			cs.WithToolAllowList(append([]string{}, (*st.Allow)...))
			applied = append(applied, "tools="+strings.Join(*st.Allow, ","))
		}
	}
	if len(st.Deny) > 0 {
		cs.WithDeniedTools(st.Deny...)
		applied = append(applied, "disallowedTools="+strings.Join(st.Deny, ","))
	}
	return applied
}

// ---------------------------------------------------------------------------
// stage 声明的下发通道
// ---------------------------------------------------------------------------
//
// 走 context 而不是改 CreateAgentFunc 的签名 ("(ctx, role, systemPrompt)"), 理由与
// RunMetadata.Cwd / NodeExecHints 完全相同: 那个签名被 6 个下游平台的工厂实现共用,
// 加参数等于要求它们全部改一遍, 而这次要下发的只是一个"可以没有"的声明。
//
// ★ 为什么不是复用 NodeExecHints: 那个通道上已经挂了**图覆盖表**的声明, 而两者的
// 优先级不同 —— 覆盖表是运维/灰度期的最后一跳, 必须压得住工作流定义里的声明
// (见 mergeStageOverride)。两条来源挤进同一个字段, "谁赢"就取决于哪个先写,
// 而这个顺序在以后任何一次重构里都可能被无声地改掉。分成两个键之后, 优先级由
// 消费点 (pkg/feishu/session.go) 显式地按顺序调用表达。
//
// 注入点是 executeStage —— pipeline / fanout / adversarial / graph **四条路径全过它**
// (图路径经 stageNodeRunner → ExecuteSingleStage → executeStage), 所以这里是
// "接一次, 四条都通"的那个点。只接一条路径的后果在本仓已经写过一次:
// 信箱只接 workflow.go 一处, 图路径上的 agent 就永远收不到信。
type stageToolsKey struct{}

// WithStageTools 把当前阶段声明的工具面写入 context (nil 是合法的 no-op)。
func WithStageTools(ctx context.Context, st *StageTools) context.Context {
	if ctx == nil || st == nil || !st.Declares() {
		return ctx
	}
	return context.WithValue(ctx, stageToolsKey{}, st)
}

// StageToolsFromContext 读回当前阶段声明的工具面 (未声明返回 nil)。
func StageToolsFromContext(ctx context.Context) *StageTools {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(stageToolsKey{}).(*StageTools)
	return st
}

// StageDecl 本阶段在**工具面之外**的三项声明 (21.8.12 P4 / X1):
// Model (模型覆盖) / Hooks (阶段专属 hook) / Skills (阶段专属技能)。
//
// 为什么与 StageTools 分成两个键而不是并成一个 "StageDecl":
//  1. 工具面那三项有**图覆盖表**这个上游 (NodeExecHints.ToolProfile), 于是消费点
//     必须显式表达"覆盖表 > 声明"的先后 (见 stageConstraints); 这三项没有覆盖表
//     对应物, 消费点就是一句直读。把两者塞进同一个键, 会让"哪些字段需要让位给
//     覆盖表"这件事从类型上消失。
//  2. StageTools 的存在性有语义 (nil vs &StageTools{}) —— 它承载 "tools: [] 是显式
//     空名单" 这条 fail-closed 声明。这三项都是空值即未声明, 混在一起会让那个
//     区分多一个无关的邻居。
type StageDecl struct {
	// Model 声明本阶段用哪个模型别名。空 = 未声明, 按 plan/agent 层级解析。
	Model string
	// Hooks 本阶段的 hook 声明原文 (<event>:<type>:<target>[:<if>])。**原文**而不是
	// 解析结果: 解析器在 pkg/hooks, 而这个通道的消费者 (宿主的 runner 装配点) 本来
	// 就要 import 它 —— 在这里先解析一遍, 等于把同一份解析实现引到 pkg/agent。
	Hooks []string
	// Skills 本阶段额外声明的技能名 (须已在技能注册表里)。
	Skills []string
}

// Declares 三项全空时返回 false —— 与 StageTools.Declares 同一语义:
// "没写这一栏"不该在消费点上与"写了但都是空"分岔。
func (d StageDecl) Declares() bool {
	return strings.TrimSpace(d.Model) != "" || len(d.Hooks) > 0 || len(d.Skills) > 0
}

type stageDeclKey struct{}

// WithStageDecl 把本阶段的 Model/Hooks/Skills 声明写入 context (全空是 no-op)。
//
// 与 WithStageTools 同一个注入点 (executeStage, 四条路径的交汇), 同一套理由:
// 接一次四条路径都通, 只接一条的结果是"某个 mode 下这三项静默不生效"。
func WithStageDecl(ctx context.Context, d StageDecl) context.Context {
	if ctx == nil || !d.Declares() {
		return ctx
	}
	return context.WithValue(ctx, stageDeclKey{}, d)
}

// StageDeclFromContext 读回本阶段的 Model/Hooks/Skills 声明 (未声明返回零值)。
func StageDeclFromContext(ctx context.Context) StageDecl {
	if ctx == nil {
		return StageDecl{}
	}
	d, _ := ctx.Value(stageDeclKey{}).(StageDecl)
	return d
}
