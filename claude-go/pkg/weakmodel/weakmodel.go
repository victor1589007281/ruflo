// Package weakmodel 弱模型 harness 增强配置 (规划手册 13.3 方案三 第一批)。
//
// 背景: 本地小模型 (LFM2.5-2.6B / Gemma4-12B, 经 Ollama 接入) 在 agent 循环中的
// 主要失败模式是格式错、选错工具、上下文腐烂——这些失败不靠权重也能消掉大半。
// 本包定义一个 Profile 统一控制四件确定性机制 (全部冻结权重):
//
//	SchemaRetry       L2  tool_use 入参 JSON Schema 校验 + 错误回注重试
//	ResultPostprocess L3  tool_result 确定性后处理 (单行截断 + 保头尾)
//	ToolMasking       L4  静态工具掩码 (对本地模型屏蔽媒体/金融/编排等长尾工具)
//	PlanningHints     L6b system prompt 注入 checklist 规划指引
//
// 开关解析优先级: 显式 env CLAUDE_GO_WEAK_MODEL=1|0 > 自动 (provider=="ollama" 时开)。
// 设计纪律 (13.3.1): 零额外调用优先、确定性 verifier 优先、能离线不在线。
package weakmodel

import (
	"strings"
)

// Profile 弱模型增强开关集。零值 = 全关 (不改变任何既有行为)。
type Profile struct {
	Enabled           bool     // 总开关; false 时以下全部不生效
	SchemaRetry       bool     // L2: 执行前 schema 校验, 失败回注错误重填 (≤2 次后终止该调用)
	ResultPostprocess bool     // L3: tool_result 入库前确定性后处理
	ToolMasking       bool     // L4: 按 DefaultMaskedTools 掩码长尾工具
	PlanningHints     bool     // L6b: system prompt 追加 planning checklist 段
	MaskedExtra       []string // 额外掩码工具名 (env CLAUDE_GO_WML_MASK_EXTRA, 逗号分隔)
	// ToolChoiceAny L1 裁定项 (D2): 执行回合 (尾部是 tool_result) 强制 tool_choice=any。
	// 仅 env CLAUDE_GO_WML_TOOL_CHOICE_ANY=1 显式开启——全局强制会令模型无法
	// end_turn 自然收尾, 故默认关, 且引擎只在执行回合临时启用。
	ToolChoiceAny bool
}

// Enabled 返回一个全开的 Profile。
func Enabled() Profile {
	return Profile{
		Enabled:           true,
		SchemaRetry:       true,
		ResultPostprocess: true,
		ToolMasking:       true,
		PlanningHints:     true,
	}
}

// EnvOverride 总开关环境变量名。
const EnvOverride = "CLAUDE_GO_WEAK_MODEL"

// EnvMaskExtra 额外掩码工具列表环境变量名 (逗号分隔)。
const EnvMaskExtra = "CLAUDE_GO_WML_MASK_EXTRA"

// EnvToolChoiceAny tool_choice=any 场景化开关 (L1 裁定项, 默认关)。
const EnvToolChoiceAny = "CLAUDE_GO_WML_TOOL_CHOICE_ANY"

// Resolve 解析弱模型 Profile。
//
// provider 为模型别名的 provider 段 (如 "ollama:gemma4:..." → "ollama")。
// getenv 通常为 os.Getenv (测试可注入)。规则:
//   - CLAUDE_GO_WEAK_MODEL=1/true/on  → 全开 (显式强制, 与 provider 无关)
//   - CLAUDE_GO_WEAK_MODEL=0/false/off → 全关 (显式禁用, 基线 A/B 用)
//   - 未设置 → 自动: provider=="ollama" 视为本地弱模型 → 全开; 否则全关
func Resolve(provider string, getenv func(string) string) Profile {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	sw := strings.ToLower(strings.TrimSpace(getenv(EnvOverride)))
	var p Profile
	switch sw {
	case "1", "true", "on", "yes":
		p = Enabled()
	case "0", "false", "off", "no":
		p = Profile{}
	default:
		if strings.EqualFold(strings.TrimSpace(provider), "ollama") {
			p = Enabled()
		}
	}
	if p.Enabled {
		switch strings.ToLower(strings.TrimSpace(getenv(EnvToolChoiceAny))) {
		case "1", "true", "on", "yes":
			p.ToolChoiceAny = true
		}
	}
	if p.ToolMasking {
		if extra := strings.TrimSpace(getenv(EnvMaskExtra)); extra != "" {
			for _, name := range strings.Split(extra, ",") {
				if name = strings.TrimSpace(name); name != "" {
					p.MaskedExtra = append(p.MaskedExtra, name)
				}
			}
		}
	}
	return p
}

// DefaultMaskedTools L4 默认掩码清单 (13.3.3 L4 的静态保守版)。
//
// 入选标准: 本地 2-12B 模型几乎不可能用对的工具 —— 媒体生成六件套、金融行情、
// 团队/定时编排、进化实验八件套、codeintel 五件套、advisor 等。保留编码核心
// (Shell/Read/Write/StrReplace/Glob/Grep/TodoWrite/PlanMode/Task*/Web*/agent/
// delegate_task/LSP/ToolSearch/Skill)。动态阶段感知掩码为后续迭代 (见手册 13.3.6)。
// CoreVisibleTools L4 动态掩码的核心可见集 (使用感知渐进披露)。
//
// 动态掩码规则 (engine queryLoop 每轮重算): 可见 = 本核心集 ∪ 本次会话已实际
// 调用过的工具 (粘性) ∪ ToolSearch (发现通道) − DisabledTools (硬门)。
// 与静态掩码的分工: 静态掩码砍"本地模型永远用不对"的长尾 (进 DisabledTools,
// 执行面也拦); 动态掩码只收窄 API 面 (执行面不拦, ToolSearch 找到的仍可调用,
// 这正是"渐进披露"与"硬掩码"的安全差异)。
var CoreVisibleTools = []string{
	// 编码核心
	"Bash", "Shell", "Read", "Write", "StrReplace", "Glob", "Grep",
	// 规划与任务跟踪
	"TodoWrite", "EnterPlanMode", "ExitPlanMode",
	"TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "TaskOutput", "TaskStop",
	// 检索与发现 (ToolSearch 是动态掩码的逃生通道, 绝不可掩)
	"WebFetch", "WebSearch", "ToolSearch",
}

var DefaultMaskedTools = []string{
	// 媒体生成 (本地模型写不对参数, 且 32K 上下文塞不下其 schema 开销)
	"GenerateImage", "GenerateGIF", "GenerateVideo",
	"GeneratePPTX", "GenerateChart", "GenerateSpeech",
	// 金融行情 (垂直场景)
	"FetchKLine", "FetchQuote",
	// 团队/定时编排 (headless 单跑场景无意义)
	"TeamCreate", "TeamDelete", "TeamMailbox",
	"CronCreate", "CronDelete", "CronList",
	// 进化实验八件套 (治理面, 非执行任务)
	"evo_inspect", "evo_list_envs", "evo_promote", "evo_propose",
	"evo_rollback", "evo_run_experiment", "evo_smoke", "evo_status",
	// codeintel 五件套 (索引管理面)
	"code_intel_branch", "code_intel_init", "code_intel_query",
	"code_intel_status", "code_intel_update",
	// 其他治理/平台面
	"advisor", "assembly", "Config",
	// Worktree 编排 (弱模型易误用, 需要时经 MaskedExtra 反向放开)
	"EnterWorktree", "ExitWorktree",
}
