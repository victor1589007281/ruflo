// taskclass.go —— 任务时程分层与 maxTurns 档位 (H4-3)。
//
// # 缺口在哪
//
// 回合上限此前是**一个数管所有任务**: config.json 的 `.ai.maxTurns = 25` 是全局兜底,
// 团队路径再用 clampTurns 兜一个 40 的硬下限 (pkg/feishu/session.go)。于是
// "写一个 hello world" 与 "实现 6 个模块并让它编译通过" 拿到同一个 25/40。
//
// 那个数是按**冒烟任务**定的, 对长程任务是紧的: 实测 Phase1+Phase2 全量实现
// 6 分多钟才跑 30 turns 还没接近完成 (cmd/claude-go/main.go 里那条注释就是为此把
// CLI 路径硬编码成 150 的)。CLI 路径已经承认了这个事实, 却只在**一条路径**上承认:
// 同一个团队跑在飞书侧仍然是 40。两处不一致, 而且都不是配置项 —— 想调只能改代码。
//
// # 分层而不是调大
//
// 直接把 25 改成 150 是错的: 那个数同时是"跑飞的止损线"。冒烟任务本该 3 轮收尾,
// 给它 150 轮意味着一个卡住的 run 要烧 50 倍的 token 才停。所以按**任务时程**分档,
// 每一档各自有上限, 长程档的上限由配置给 (见 GlobalConfig.LongHorizonMaxTurns)。
//
// # 分层判据用声明, 不用推断
//
// 判据全部来自调用方**已经知道**的事实 (角色名 / 工作流是否产码 / 显式声明),
// 不引入任何新的探测: 一个"猜这个任务有多长"的分类器一旦猜错, 错的那一侧是
// "长程任务拿到短上限" (跑不完) 或"短任务拿到长上限" (跑飞没人喊停) ——
// 两者都比"没分层"更糟。声明式判据错了是可见的 (改配置), 推断式判据错了是
// 沉默的。
//
// # 顺序约束 (方案 §21.5.6 的警告)
//
// 放开 maxTurns 会放大终止判据的必要性: 从"25 轮止损"变成"跑到人喊停"。
// 方案原文要求 H0-2 (终止判据通电) 先行 —— 本项落地时它已经完成
// (pkg/engine/internal_hook/hook_error.go 的 CircuitBreakerHook 两条判据),
// 所以顺序满足。**若将来有人回退那一项, 这里必须一起回退。**
package modelconfig

import "strings"

// TaskClass 任务时程档。
//
// 三档, 刻意不多: 每一档都要在配置里占一个键, 而每一档的实际使用率决定了
// 有没有人会去调它。加第四档的收益远小于它带来的"配错档"的概率。
type TaskClass string

const (
	// TaskClassShort 冒烟/一次性任务: 单轮出结论, 不开工具循环。
	// (architect / researcher / planner 这类思考型角色。)
	TaskClassShort TaskClass = "short"
	// TaskClassStandard 常规任务: 默认档, 取 .ai.maxTurns。
	TaskClassStandard TaskClass = "standard"
	// TaskClassLongHorizon 长程任务: 需要上百轮工具调用才能收敛
	// (产码 / 测试 / 修复循环)。上限取 .ai.longHorizonMaxTurns。
	TaskClassLongHorizon TaskClass = "long-horizon"
)

// 编译期默认档位值。
//
// Standard 取 25 与现状逐字相同 —— 这是**刻意的**: 分层的默认行为必须等于
// 改造前的行为, 否则这次改动会在没人开开关的时候改变每一个任务的止损线。
// LongHorizon 取 150 对齐 CLI 路径已有的硬编码值 (那是实测调出来的), 于是
// "把飞书侧也放开"与"CLI 侧不变"是同一件事的两种说法。
const (
	DefaultStandardMaxTurns    = 25
	DefaultLongHorizonMaxTurns = 150
)

// thinkingRoles 思考型角色集: 一次性产出设计稿/报告, 不该开工具循环。
//
// 收口到这里是刻意的: 此前这份名单**只**存在于 cmd/claude-go/main.go 的
// cliAgentRunner.Execute 里 (一个 switch), 于是同一条判据在飞书路径上根本不存在 ——
// 一个 architect 在 CLI 侧单轮收尾, 在团队侧却拿到 40 轮工具循环。名单有两份
// 就等于没有名单。
var thinkingRoles = map[string]bool{
	"architect": true, "researcher": true, "planner": true,
	"outline-architect": true, "evo-architect": true, "game-architect": true,
	// 市场/情报类: 产出是一份报告, 不需要读改编译循环。
	"analyst": true, "writer": true, "editor": true,
}

// IsThinkingRole 判定角色名是否属于思考型 (大小写与首尾空白不敏感)。
//
// 导出是刻意的: CLI 路径的 disableTools 判据也要用它。判据与名单必须同源 ——
// 两处各列一遍的失效形态是"同一个角色在两条路径上跑不同的轮数", 而那种偏差
// 没有任何读数会指出。
func IsThinkingRole(role string) bool {
	return thinkingRoles[strings.ToLower(strings.TrimSpace(role))]
}

// TaskSignals 分档用的可观测信号。
//
// 全部是**声明** (调用方已经知道的字段), 没有一项需要探测:
// 每引入一个探测就多一条"猜错了也不报错"的路径。
type TaskSignals struct {
	// Explicit 调用方显式指定的档位 (最高优先级)。空 = 未指定。
	Explicit TaskClass
	// Role 本次运行的角色名。
	Role string
	// ProducesCode 所属工作流是否产出代码 (GraphMeta.ProducesCode)。
	// 产码任务天然是长程的: 写→编译→改 的循环没有下界。
	ProducesCode bool
	// DisableTools 调用方已决定关闭工具循环 —— 那就不可能长程。
	DisableTools bool
}

// ClassifyTask 判定任务时程档。
//
// 判据按**代价**排序, 不是按直觉排序:
//
//  1. Explicit —— 显式声明永远赢。出了问题改配置就能修, 不用改代码。
//  2. DisableTools —— 没有工具循环 ⇒ 至多一轮, 长程档在这里没有任何意义,
//     而误判成短程的代价是"上限比实际需要大", 无副作用。
//  3. Role 是思考型 ⇒ Short。
//  4. ProducesCode ⇒ LongHorizon。放在角色之前: 一个产码工作流里的
//     "reviewer" 也要读几十个文件才能给出结论, 它与 coder 同档。
//  5. 其余 ⇒ Standard。
//
// 第 4/5 条之间**没有**"角色是做事型就长程"这一条: 那会把几乎每个团队 stage
// 都推向长程档, 等于没有分层。长程要有依据 —— 产码是一个可判的依据, 角色名不是。
func ClassifyTask(sig TaskSignals) TaskClass {
	if sig.Explicit != "" {
		return sig.Explicit
	}
	if sig.DisableTools {
		return TaskClassShort
	}
	if IsThinkingRole(sig.Role) {
		return TaskClassShort
	}
	if sig.ProducesCode {
		return TaskClassLongHorizon
	}
	return TaskClassStandard
}

// MaxTurnsFor 按档位取回合上限。<=0 表示不限。
//
// longHorizon 为 0 时**回落到 standard**, 不回落到一个更大的硬编码值:
// 配置没写就是没开分层, 而"没开"必须等于改造前的行为 (见 DefaultStandardMaxTurns
// 的说明)。回落成 150 会让这次改动在没人配置的情况下改掉每一个产码任务的止损线。
func (g GlobalConfig) MaxTurnsFor(class TaskClass) int {
	standard := g.DefaultMaxTurns
	if standard <= 0 {
		standard = DefaultStandardMaxTurns
	}
	if class != TaskClassLongHorizon {
		return standard
	}
	if g.LongHorizonMaxTurns > 0 {
		return g.LongHorizonMaxTurns
	}
	return standard
}

// MaxTurns 按档位取回合上限 (读当前生效的全局配置)。
//
// 与 Resolve 系列分开而不是加一个参数: Resolve 的签名被二十多处调用, 加参数
// 会逼着每个调用点回答一个它答不上来的问题 ("这个 run 属于哪一档")。档位是
// **执行侧**才知道的信息, 所以由执行侧在拿到 ResolvedConfig 之后单独问一次。
func (r *ConfigResolver) MaxTurns(class TaskClass) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.global.MaxTurnsFor(class)
}
