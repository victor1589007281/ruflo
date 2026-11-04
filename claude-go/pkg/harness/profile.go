// profile.go — 工具档位（Moqi R-H9 的 `companion` 档位）在 harness 侧的落地。
//
// # 为什么档位要在 harness 归一，而不是引擎
//
// 引擎只认识"白名单"这一种形状（map / nil）。档位是**策略名**，它背后是一份
// 由 harness 维护的常量集合（见 toolext.go 的 companionAllowed）。让引擎认识
// "companion" 这个词，等于把业务策略下沉到引擎 —— 而引擎是给所有调用方共用的，
// 它不该知道陪伴场景存在。
//
// 所以分工是：harness 把"档位"翻译成"白名单"，引擎只执行白名单。这样档位怎么
// 演化（加一个 memory_correct、去掉一个 Read）都不需要动引擎。
//
// # 为什么未实现的档位必须报错
//
// 空串 = 不限档位（旧行为），未知名字则**报错**。理由：把 "companion "（多一个
// 空格）或 "companinon"（拼错）当成"没有档位"静默放行，会让一次配置错误变成
// 一次安全边界的消失，而调用方以为自己开了档位。这类拼写错误的失效方向必须是
// 拒绝，不是放开。
package harness

import (
	"fmt"
	"strings"
)

// ToolProfileCompanion 是陪伴档位的名字（TurnPolicy.ToolProfile 取值）。
//
// 与 toolext.go 的 companionAllowed 同一份真相：档位名在这里，集合在那里。
// 分成两处是因为前者是**请求侧词汇**、后者是**策略内容**，但它们必须一起改
// （有测试钉住"companion 档位 = CompanionAllowedTools()"）。
const ToolProfileCompanion = "companion"

// applyToolProfile 按 policy.ToolProfile 收窄 policy.AllowedTools。
//
// 归一化规则：
//
//	ToolProfile 为空（含纯空白）→ 不动 AllowedTools（旧行为，逐字节相同）
//	ToolProfile = "companion"  → AllowedTools 收窄到陪伴集合（见 ApplyCompanionProfile）
//	其它取值                   → 返回错误，不执行本轮
func applyToolProfile(policy *TurnPolicy) error {
	if policy == nil {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(policy.ToolProfile))
	if name == "" {
		return nil
	}
	switch name {
	case ToolProfileCompanion:
		// AllowedTools 是三态指针：nil 表示"不限"，那正是陪伴档位要收窄的起点。
		// ApplyCompanionProfile 要求一个非 nil 的落点，所以先把 nil 变成
		// "指向 nil 的变量"，收窄后再写回 —— 不能直接把 nil 交给它（它会以为
		// 调用方写错了而报错，而"从 nil 收窄"恰恰是档位最常见的用法）。
		var allowed []string
		if policy.AllowedTools != nil {
			allowed = *policy.AllowedTools
		}
		if err := ApplyCompanionProfile(&allowed); err != nil {
			return err
		}
		// 非 nil（可能为空集）：空集是"全部拒绝"，不是"不限"，必须保留这个区别。
		policy.AllowedTools = &allowed
		return nil
	default:
		// 列出已知取值而不是只说"未知": 调用方看到 allowed 的集合就能自己判断
		// 是拼错还是漏了实现。新档位加进来时这里必须跟着改，改漏了会当场报错，
		// 而不是静默按"不限档位"跑。
		return fmt.Errorf("harness: 未知的工具档位 %q（已实现: %q；空串 = 不限档位）",
			policy.ToolProfile, ToolProfileCompanion)
	}
}
