// toolext.go — 业务工具契约元数据（Moqi R-H9）。
//
// pkg/tool.Tool 只回答"怎么执行"（Name/Description/InputSchema/Call/...），
// 回答不了业务宿主真正需要判断的四件事：
//
//	这个工具会不会改世界？      → SideEffect
//	改了世界要不要用户点头？    → NeedsConfirmation
//	结果能不能跨 owner 复用？   → OwnerScoped
//	它会不会卡住 / 撑爆上下文？ → Timeout / MaxResultBytes
//
// 没有这些，宿主只能靠**工具名子串**去猜（"Bash 大概危险"），猜错的形态是静默的：
// 一个叫 `exec_sql` 的写工具会被当成只读放行。本文件把这些判断变成工具自己的
// 声明，并为没声明的工具给出 fail-closed 的默认值。
//
// 与 internal_hook.HookPolicy 同一套做法：**可选接口，零值 = 旧行为**。工具作者
// 什么都不用改；想接管的工具多实现一个 ToolMeta 即可。
//
// 关于 import：pkg/harness → pkg/tool 不构成环。pkg/tool 只依赖
// pkg/logging / pkg/textutil / pkg/types，这三者（及其传递依赖）都不 import
// pkg/harness（`go list -deps ./pkg/tool` 验证）。若将来 pkg/tool 反过来要引用
// harness，环就出现了——那时应把 ToolMeta 收窄成 `interface{ Name() string }`，
// 而不是让 tool 包依赖契约层。
package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// SideEffect 描述一次工具调用对工具之外的世界造成的影响范围。
//
// 只有三档而不是更细的谱系：宿主真正据以决策的分支只有"可以随便重放"、
// "需要确认/记账"、"出了本机就收不回来"三种，再细分只会让工具作者在填表时
// 猜，而猜错的元数据比没有元数据更危险（它会带着"已声明"的可信度被消费）。
type SideEffect int

const (
	// SideEffectRead 只读：不改任何持久状态，可安全重放与并发。
	SideEffectRead SideEffect = iota
	// SideEffectWrite 改本机持久状态（记忆、文件、库）。可重试，但需要幂等键。
	SideEffectWrite
	// SideEffectExternal 离开本机（发消息、调第三方 API、下单）。**不可撤销**，
	// 重放可能造成外部副作用，必须确认且必须有幂等键。
	SideEffectExternal
)

// String 返回可读名，进 trace 用。
func (s SideEffect) String() string {
	switch s {
	case SideEffectRead:
		return "read"
	case SideEffectWrite:
		return "write"
	case SideEffectExternal:
		return "external"
	default:
		// 未知取值按最危险的一档呈现：元数据写错时，日志与策略都应该偏向保守，
		// 而不是把未知悄悄显示成 "read"。
		return "external(unknown:" + fmt.Sprintf("%d", int(s)) + ")"
	}
}

// ToolMeta 是工具**可选实现**的元数据声明，镜像 internal_hook.HookPolicy 的模式。
//
// 不实现它的工具由 ToolMetaOf 给出 fail-closed 默认值（写、需确认、按 owner 隔离）。
// 因此"接入元数据"是纯增量动作：存量工具一个字段都不用改，而新工具可以逐项收紧。
type ToolMeta interface {
	// SideEffect 返回副作用档位。
	SideEffect() SideEffect
	// NeedsConfirmation 表示执行前是否需要用户/宿主显式确认。
	NeedsConfirmation() bool
	// OwnerScoped 表示结果是否只属于当前 owner（true = 不得跨 owner 复用/缓存）。
	OwnerScoped() bool
	// Timeout 返回单次执行上限；0 = 不限时（与 HookPolicy.Timeout 同义）。
	Timeout() time.Duration
	// MaxResultBytes 返回结果字节上限；0 = 不截断。
	MaxResultBytes() int
}

// defaultToolMeta 是未声明元数据时的兜底。三项取值的方向全部是 fail-closed：
// 写而不是读、要确认而不是放行、按 owner 隔离而不是共享。
type defaultToolMeta struct{}

// SideEffect 返回 SideEffectWrite。
//
// 为什么默认是"写"而不是"读"：一个忘了声明的工具，最坏的解读是它有副作用。
// 反过来（默认只读）会让 `exec_sql` 这类名字看不出危险的工具被自动放行、
// 并发执行、跨 owner 复用结果——三条都是静默的错，且都可以写穿数据。
func (defaultToolMeta) SideEffect() SideEffect { return SideEffectWrite }

// NeedsConfirmation 默认 true：没声明就等于没承诺无害，必须问。
func (defaultToolMeta) NeedsConfirmation() bool { return true }

// OwnerScoped 默认 true：宁可少复用，也不能把 A 的结果给 B 看。
func (defaultToolMeta) OwnerScoped() bool { return true }

// Timeout 默认 0（不限时）。这里**不**发明一个默认超时：工具的合理耗时相差
// 几个数量级（读文件 vs 跑训练），拍一个值只会把慢工具在高峰期变成随机失败，
// 而"随机失败"比"可能卡住"更难定位。超时由声明者给出，或由宿主的 run 级
// 预算（TurnPolicy.MaxWall）统一收口。
func (defaultToolMeta) Timeout() time.Duration { return 0 }

// MaxResultBytes 默认 0（不截断）。同理：harness 里写一个默认上限，等于给
// 所有未声明工具加了一次**行为变更式**的静默截断——上线后表现为"某工具偶尔
// 少半截输出"，而工具本身一行没改。
func (defaultToolMeta) MaxResultBytes() int { return 0 }

// ToolMetaOf 返回工具的元数据实现；未实现 ToolMeta 时返回 fail-closed 默认值。
//
// nil 工具（接口为 nil）同样得到默认值而不是 panic：装配期拿一个可能为 nil 的
// 工具来问元数据是常见写法，为这个 panic 不值得。
func ToolMetaOf(t tool.Tool) ToolMeta {
	if t == nil {
		return defaultToolMeta{}
	}
	if m, ok := t.(ToolMeta); ok {
		return m
	}
	return defaultToolMeta{}
}

// ============================================================================
// 陪伴（companion）工具档位
// ============================================================================

// 陪伴档位允许的工具名。
//
// 为什么是"白名单常量"而不是"在 chat 档上做差集"：差集依赖被减集合的当前
// 内容，而那个集合会随内置工具增长——今天减掉 Bash/Write，明天有人注册
// `code_exec`，差集就会自动把它放进来。白名单的失效方向相反：新工具默认
// **不可达**，要显式加进来才会出现在陪伴场景。
const (
	// CompanionMemorySearch 记忆检索（只回索引，不拖正文）。
	CompanionMemorySearch = "memory_search"
	// CompanionMemoryRead 按 id 取记忆正文。
	CompanionMemoryRead = "memory_read"
	// CompanionMemoryCorrect 记忆更正（用户说"记错了"时的写入路径）。
	//
	// 这个名字目前在 pkg/tool/builtin 里还没有实现——白名单里出现一个尚不
	// 存在的工具是**零成本恒真约束**（永远为真、也不放过任何东西），而不是漏洞；
	// 反过来，等它实现了再补进白名单，中间窗口里陪伴场景就真的改不了记忆。
	CompanionMemoryCorrect = "memory_correct"
	// CompanionFileRead 文件读取。刻意只放 Read，不放 Glob/Grep/Write。
	CompanionFileRead = "Read"
)

// companionAllowed 是陪伴档位的唯一真相源（名字 → 说明），顺序由
// CompanionAllowedTools 排序后给出。
var companionAllowed = []string{
	CompanionMemorySearch,
	CompanionMemoryRead,
	CompanionMemoryCorrect,
	CompanionFileRead,
}

// CompanionAllowedTools 返回陪伴档位允许的工具名（排序后的副本）。
//
// 这个集合就是"陪伴对话没有任意代码执行面"的全部依据：不在集合里的工具
// （Bash、Write、任意 code-exec）在这个档位下**不可见也不可执行**。
func CompanionAllowedTools() []string {
	out := append([]string(nil), companionAllowed...)
	sort.Strings(out)
	return out
}

// ApplyCompanionProfile 把一份工具白名单收窄到陪伴档位允许的集合。
//
// 用法对的是 harness.TurnPolicy.AllowedTools 的三态语义（nil = 不受白名单限制，
// 非 nil = 白名单内可见可执行，空集 = 全部拒绝）：
//
//	*allowed == nil  → 置为完整的陪伴集合（这是"只想开陪伴档位"的常见路径）；
//	*allowed != nil  → 与陪伴集合**求交**（白名单只能收窄，不能借 profile 放宽）。
//
// 为什么是求交而不是直接覆盖：调用方可能已经带上了本部署特有的工具集（MCP 工具、
// 动态注册的检索器）。直接覆盖会把它们**悄悄**弄丢，表现为"配了 MCP 但模型说
// 没有工具"。求交同时满足两件事：保留调用方想要的，且保证危险工具怎么传都进不来。
//
// 错误只用于两种"调用方写错了"的情形：allowed 为 nil 指针（没给落点），以及
// 求交结果为空且原集合非空（两边没有一个交集，继续跑只会得到一个什么都不能做的
// 哑巴会话，而不报错的话没人会想到是档位配置对不上）。
func ApplyCompanionProfile(allowed *[]string) error {
	if allowed == nil {
		return errors.New("harness.ApplyCompanionProfile: allowed 为 nil 指针；需要传入白名单字段的地址")
	}
	companion := make(map[string]struct{}, len(companionAllowed))
	for _, n := range companionAllowed {
		companion[n] = struct{}{}
	}

	if *allowed == nil {
		// 原本不受限 —— 收窄成陪伴集合。这是本函数存在的**主要**场景：
		// 把"默认什么都能用"变成"只有一个只读+记忆写的面"。
		*allowed = CompanionAllowedTools()
		return nil
	}

	original := append([]string(nil), (*allowed)...)
	narrowed := make([]string, 0, len(original))
	seen := make(map[string]struct{}, len(original))
	for _, n := range original {
		if _, ok := companion[n]; !ok {
			continue // 不在陪伴集合内：静默丢弃（安全方向），见下方注释
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		narrowed = append(narrowed, n)
	}
	if len(original) > 0 && len(narrowed) == 0 {
		return fmt.Errorf("harness.ApplyCompanionProfile: 请求的工具集 %v 与陪伴档位 %v 无交集",
			original, CompanionAllowedTools())
	}
	// 保持确定顺序：同一个输入两次调用必须产出逐字节相同的白名单，否则它在
	// trace / 缓存前缀里会变成一个无意义的抖动源。
	sort.Strings(narrowed)
	*allowed = narrowed
	return nil
}

// ============================================================================
// 幂等键
// ============================================================================

// IdempotencyKey 返回工具名 + 规范化输入参数的稳定哈希。
//
// 实现**委托**给 types.IdempotencyKey，本函数只是给业务层留的稳定入口。
//
// # 为什么要委托而不是在这里另写一份
//
// 幂等键有两个消费方：业务契约层（本函数，判"这一轮重试是不是同一件事"）与执行层
// （`pkg/tool` 的去重闸，判"同一 (工具, 参数) 的第二次调用直接返回首次结果"）。
// `pkg/tool` 不能 import `pkg/harness`（依赖方向是 harness → engine → tool），
// 所以它只能引 types。若两边各写一份哈希，失效形态不是报错而是"两套键都算得出来、
// 但彼此不等"——执行层按自己的键去重，业务层按自己的键判重放，同一件事被记成两件，
// 且没有任何日志异常。这与 owner 载体（types/owner.go 文件头）是同一个陷阱。
//
// 保留这个名字而不是让业务层直接调 types：`harness.IdempotencyKey` 是契约面的
// 词汇，业务代码不该为了一个哈希去 import 引擎的叶子包。委托关系保证签名与语义
// 只有一处真相，改 types 那边会立刻反映到这里（有黄金值测试钉住）。
func IdempotencyKey(toolName string, input json.RawMessage) string {
	return types.IdempotencyKey(toolName, input)
}

// ============================================================================
// 结果截断
// ============================================================================

// TruncateResult 把 s 截到不超过 maxBytes 字节，且**不切断多字节 UTF-8 字符**。
//
// 三条性质：
//
//  1. out 永远是 s 的前缀（不追加 "…"、不加长度标记）。调用方要加什么提示由它
//     自己决定，因为拼接位置（对话、日志、tool_result）各不相同。
//  2. maxBytes <= 0 表示不截断（与 ToolMeta.MaxResultBytes 的 0 语义一致）。
//  3. 只在 rune 起点切：`s[:maxBytes]` 可能落在一个 3 字节汉字的中间两个字节上，
//     得到的是非法 UTF-8。它不会立刻报错，而是随着 transcript 一路走下去——
//     存进库、发给模型、渲染到前端，最后表现为一个乱码方块。切在 rune 边界上
//     的代价是少一两个字节，收益是"结果永远是合法的 UTF-8"这个不变量。
func TruncateResult(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	// s[cut] 若本身是 rune 的**起首字节**，说明 s[:cut] 恰好落在边界上。
	// 否则向前退到最近的一个起首字节。cut 恒 < len(s)，下标安全。
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	// 输入本身可能是非法 UTF-8（源头就坏了）。此时向前退到的位置虽然"看起来"
	// 是边界，切出来的前缀仍可能不是合法 UTF-8；这是输入的问题，不在这里修，
	// 否则就成了"截断函数偷偷改内容"。
	return s[:cut], true
}

// IsCompanionTool 报告某工具名是否属于陪伴档位允许集合（大小写敏感，与
// engine.AllowedTools 的精确匹配语义一致）。
//
// 存在的理由：调用方在**装配期**需要一条廉价的断言（"我这个部署确认没有
// 把 Bash 露给陪伴会话"），而不是每次自己构造 slice 再线性比较。
func IsCompanionTool(name string) bool {
	name = strings.TrimSpace(name)
	for _, n := range companionAllowed {
		if n == name {
			return true
		}
	}
	return false
}
