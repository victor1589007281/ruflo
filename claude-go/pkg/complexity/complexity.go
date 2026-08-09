// Package complexity 提供任务复杂度判定，是引擎级 AutoMode（复杂任务 → 自动
// Plan Mode + Advisor）的能力底座。
//
// 设计目标: 零外部依赖的 leaf 包。判定器分两层:
//   - Heuristic: 纯本地中英双语启发式, 零 LLM 开销、零延迟 (默认路径);
//   - LLM 分类器: 由装配方注入 (经 api.Client.SimpleComplete / llmGW.Simple),
//     更准但每条非平凡消息多一次模型调用。
//
// 对齐参考: 飞书入口 pkg/feishu/bot.go 的 heuristicComplexity / llmDetectComplexity
// 是本包的原始雏形; 本包把启发式双语化、评分化, 并把 LLM 判定收敛成可注入函数,
// 使能力从"飞书外挂"上升为引擎内建。
package complexity

import (
	"context"
	"fmt"
	"strings"
)

// Mode 常量: Judge 的 Options.Mode 取值。
const (
	// ModeHeuristic 纯启发式, 零额外 LLM 开销 (默认)。
	ModeHeuristic = "heuristic"
	// ModeLLM 优先 LLM 判定, 失败回落启发式。
	ModeLLM = "llm"
	// ModeHybrid 极短文本快速跳过, 其余交 LLM (对齐旧飞书 llmDetectComplexity
	// "runeLen<15 直接跳过" 的行为), 失败回落启发式。
	ModeHybrid = "hybrid"
)

const (
	// MinRunesForLLM 短文本快速拒绝: 低于该 rune 数直接判简单, 不浪费 LLM 调用。
	MinRunesForLLM = 15
	// LongTextRunes 超过该 rune 数的文本直接判复杂 (隐含多步骤)。
	LongTextRunes = 300
)

// Verdict 复杂度判定结果。
type Verdict struct {
	// Complex 是否判定为复杂任务。
	Complex bool
	// Reason 判定原因 (供 system-reminder / 日志展示)。
	Reason string
	// Score 启发式得分 (0-3+), 供 hybrid 模式或观测用。
	Score int
}

// Options Judge 的判定选项。
type Options struct {
	// Mode 判定模式, 空串等价 ModeHeuristic。
	Mode string
	// LLM 可选 LLM 分类器: 返回 (是否复杂, 原因, 错误)。错误时 Judge 回落启发式。
	LLM func(ctx context.Context, text string) (complex bool, reason string, err error)
}

// Judge 按 Options.Mode 执行复杂度判定。
//
//	heuristic: 仅 Heuristic。
//	llm/hybrid: 文本长度 >= MinRunesForLLM 且 LLM 可用 → 用 LLM; 否则回落 Heuristic。
func Judge(ctx context.Context, text string, opts Options) Verdict {
	mode := opts.Mode
	if mode == "" {
		mode = ModeHeuristic
	}
	switch mode {
	case ModeLLM, ModeHybrid:
		if opts.LLM != nil && len([]rune(text)) >= MinRunesForLLM {
			if ok, reason, err := opts.LLM(ctx, text); err == nil {
				score := 0
				if ok {
					score = 2
				}
				return Verdict{Complex: ok, Reason: reason, Score: score}
			}
			// LLM 失败 → 回落启发式, 判定不可因分类器故障中断。
		}
		return Heuristic(text)
	default:
		return Heuristic(text)
	}
}

// zhConjunctions 中文连词: 出现 >=2 个说明任务有多段并行/顺序子步骤。
var zhConjunctions = []string{"并且", "然后", "同时", "另外", "还需要", "以及", "此外", "接着"}

// zhTaskKeywords / enTaskKeywords 任务关键词: 设计/重构/实现/多文件等强信号。
var zhTaskKeywords = []string{
	"设计", "重构", "实现", "方案", "架构", "多文件", "数据库", "接口",
	"影响面", "调研", "对比", "分析", "规划", "部署",
}
var enTaskKeywords = []string{
	"design", "refactor", "implement", "architecture", "plan", "multi-file",
	"multiple files", "database", "impact", "analyze", "test suite", "deploy",
}

// numberedPrefixes 编号任务项标记: "1. 2. 3." 或 "1、2、3、" 等。
var numberedPrefixes = []string{"1.", "2.", "3.", "4.", "5.", "1、", "2、", "3、"}

// Heuristic 中英双语启发式复杂度判定。返回 Verdict (Complex + Reason + Score)。
//
// 规则 (满足任一即复杂):
//   - runeLen > LongTextRunes → 复杂 (超长文本隐含多步骤);
//   - 中文连词 >=2 → 复杂;
//   - 编号任务项 >=3 → 复杂;
//   - 任务关键词 >=2 → 复杂 (1 个关键词计 1 分, 需其他信号叠加);
//   - 得分 >=2 才判复杂, 保证"一句话任务"不误伤。
func Heuristic(text string) Verdict {
	runes := []rune(text)
	n := len(runes)
	if n < MinRunesForLLM {
		return Verdict{Reason: fmt.Sprintf("消息过短(%d字)", n)}
	}
	if n > LongTextRunes {
		return Verdict{Complex: true, Reason: fmt.Sprintf("文本超长(%d字), 隐含多步骤", n), Score: 3}
	}

	score := 0
	var reasons []string
	lower := strings.ToLower(text)

	conjCount := 0
	for _, c := range zhConjunctions {
		if strings.Contains(text, c) {
			conjCount++
		}
	}
	if conjCount >= 2 {
		score += 2
		reasons = append(reasons, fmt.Sprintf("中文连词%d个", conjCount))
	}

	numbered := 0
	for _, p := range numberedPrefixes {
		if strings.Contains(text, p) {
			numbered++
		}
	}
	if numbered >= 3 {
		score += 2
		reasons = append(reasons, fmt.Sprintf("编号任务%d项", numbered))
	}

	kwHits := 0
	for _, k := range zhTaskKeywords {
		if strings.Contains(text, k) {
			kwHits++
		}
	}
	for _, k := range enTaskKeywords {
		if strings.Contains(lower, k) {
			kwHits++
		}
	}
	switch {
	case kwHits >= 2:
		score += 2
		reasons = append(reasons, fmt.Sprintf("任务关键词%d个", kwHits))
	case kwHits == 1:
		score += 1
		reasons = append(reasons, "任务关键词")
	}

	if score >= 2 {
		return Verdict{Complex: true, Reason: strings.Join(reasons, "; "), Score: score}
	}
	return Verdict{Reason: strings.Join(reasons, "; "), Score: score}
}

// ClassifierSystemPrompt LLM 分类器的 system prompt (中英双语)。
// 装配方用 api.Client.SimpleComplete(ctx, ClassifierSystemPrompt, text) 或
// llmGW.Simple 包一层, 解析 "COMPLEX"/"SIMPLE" 返回 (bool, reason, error)。
const ClassifierSystemPrompt = `You are a task complexity classifier. The user sends one message; decide whether it is a COMPLEX task or a SIMPLE task.

COMPLEX (any one suffices):
- Multiple steps or subtasks (>2 steps)
- Requires architecture design, plan evaluation
- Multi-file or multi-module changes
- Requires research, comparison, analysis
- Contains explicit numbered task lists (1. 2. 3.)
- Spans coding + testing + deployment phases
- Needs collaboration (multiple roles)

SIMPLE: single query, simple command, one-line change, translation, Q&A.

Reply with exactly one word COMPLEX or SIMPLE, then a newline with a one-sentence reason in the user's language.`
