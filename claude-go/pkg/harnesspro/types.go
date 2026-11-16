// Package harnesspro 复现 HarnessPro 论文 (arXiv:2603.xxxx, GLM-5.2 × Claude Code 组合)
// 的 harness 自动进化逻辑: 确定性编排器 + 固定 DAG 工作流, LLM 只出现在
// "出卷 / 诊断 / 出案 / 动刀" 四个节点内部。
//
// 与 pkg/agent/evolution.go (EvolveR/ERL 经验学习) 与 pkg/evolution (账本) 不同,
// 本包的对象是 **harness 本身** (任务指令 + 运行时策略), 手段是
// "冻结合成测试卷 → 训练集多轮 rollout → 诊断 → 三向出案 → 独立工作区动刀 →
//
//	双重过滤 (地板+帕累托) → 三档选点 (Lite/Medium/Heavy)" 的固定流水线。
//
// 论文复现的边界 (诚实声明, 见 docs):
//   - harness 的执行体 = 本包的一次单发补全 (system=harness 指令, user=任务),
//     不含论文中的 Codex/Claude Code 工具循环执行器适配 (src/rho/ 角色);
//     被进化的"指令层"正是论文产物 schema 里的 instructions.md, 机制等价。
//   - 合成测试的断言是可机械执行的声明式断言 (见 Assertion), 不是任意代码 —
//     这对应论文"可执行、可自动判定 pass/fail"的要求, 同时保证零副作用。
//   - DPP 选题用任务元数据特征 (类别 one-hot + 难度先验 + 文本长度) 做贪心 MAP,
//     不经执行器烧一轮真跑 (论文 0.1 节用 1 次执行拿特征; 那是为了难度真值,
//     内置任务池的难度先验已离线标定, 机制保留、开销省略)。
package harnesspro

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Task 进化任务池里的一道题。
type Task struct {
	ID          string   `json:"id"`
	Prompt      string   `json:"prompt"`       // 给执行体的任务描述 (契约的唯一来源)
	Category    string   `json:"category"`     // 任务类别 (json/text/structured/compute/format/extract)
	Difficulty  float64  `json:"difficulty"`   // 难度先验 0..1 (离线标定; DPP 质量项)
	EstFeatures []string `json:"est_features"` // DPP 多样性特征 (除类别外的标注)
}

// Assertion 一条可机械执行的冻结测试断言。
// TestSynth 只产出这个数据结构, 判定零 LLM (论文 0.2: 不用 LLM 当裁判)。
//
// Param/Param2/Param3 用 json.RawMessage 承接: 真实 LLM 常把数字界/次数写成
// JSON number (5) 而非字符串 ("5"), 严格 string 反序列化会让整张出卷批次
// 报废 —— 出卷是"LLM 产出 → 机械结构"的边界, 容忍标量类型漂移是这一层的职责.
type Assertion struct {
	Kind   string          `json:"kind"`             // 见 Assert* 常量
	Param  string          `json:"param"`            // contains/regex/field: 模式; field/eq: 字段名
	Param2 json.RawMessage `json:"param2,omitempty"` // eq: 期望值; range: 下界; count: 出现次数
	Param3 json.RawMessage `json:"param3,omitempty"` // range: 上界
	Negate bool            `json:"negate,omitempty"` // true = 断言取反 (not_contains 的通用形态)
}

// Param2S/Param3S 字符串化取值 (LLM 给 number 5 → "5"; 给 string "5" → "5")。
func (a Assertion) Param2S() string { return rawScalarString(a.Param2) }
func (a Assertion) Param3S() string { return rawScalarString(a.Param3) }

// rawScalarString 把 json.RawMessage 收成一个标量字符串: 字符串去引号, 数字/布尔
// 直接字符串化, null/空 → ""。
func rawScalarString(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(r, &v); err == nil {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// 断言种类 (判定引擎的唯一入口; TestSynth 的提示词里向模型开放这个封闭集合)。
const (
	AssertContains     = "contains"       // 输出包含子串 Param
	AssertNotContains  = "not_contains"   // 输出不包含子串 Param
	AssertRegex        = "regex"          // 输出匹配正则 Param
	AssertStartsWith   = "starts_with"    // 输出以 Param 开头
	AssertEndsWith     = "ends_with"      // 输出以 Param 结尾
	AssertJSONParse    = "json_parse"     // 整个输出是可解析 JSON
	AssertJSONField    = "json_field"     // JSON 输出的字段 Param 存在
	AssertJSONFieldEq  = "json_field_eq"  // JSON 字段 Param 的字符串化值 == Param2
	AssertLenRange     = "len_range"      // 输出长度在 [Param2, Param3] (整数, 含端点)
	AssertWordRange    = "word_range"     // 词数在 [Param2, Param3]
	AssertCountOccur   = "count_occur"    // 子串 Param 出现次数 == Param2 (整数)
	AssertNoDoubleByte = "no_double_byte" // 输出不含双字节字符 (格式纪律类)
)

// FrozenTest 一道冻结测试 (TestSynth 一次生成, 全程只读)。
type FrozenTest struct {
	TestID         string    `json:"test_id"`
	OracleStrength int       `json:"oracle_strength"` // ①~④ (论文纪律二: 预言强度分级)
	Rationale      string    `json:"rationale"`       // 断言依据 (为什么这能判定正确性)
	Assert         Assertion `json:"assert"`
}

// Harness 一套被进化的 harness = 论文产物 schema 的 instructions.md。
// 运行时策略 (预算/hooks/路由) 在本复现的执行体形态下并入指令文本, 不单独成文。
type Harness struct {
	ID           string `json:"id"`
	Instructions string `json:"instructions"` // system prompt 全文
	ParentID     string `json:"parent_id,omitempty"`
	Origin       string `json:"origin"` // "h0" | "epoch{e}:{parent}:{Ix}"
	Epoch        int    `json:"epoch"`
}

// RolloutResult 一次 "harness × 任务" 执行的机械统计 (论文 4.1: 不经过 LLM)。
type RolloutResult struct {
	CandidateID string  `json:"candidate_id"`
	TaskID      string  `json:"task_id"`
	K           int     `json:"k"`         // 第几次重跑
	Pass        bool    `json:"pass"`      // 10 条冻结测试全过才算 pass (论文判据)
	PassFrac    float64 `json:"pass_frac"` // 通过比例 (地板/帕累托用)
	TokensIn    int     `json:"tokens_in"`
	TokensOut   int     `json:"tokens_out"`
	LatencyMS   int64   `json:"latency_ms"`
	Output      string  `json:"output,omitempty"` // 截断存储, 供诊断引用
	InfraError  bool    `json:"infra_error"`      // true = 无效样本 (不进入任何统计)
	Error       string  `json:"error,omitempty"`
}

// CandidateMetrics 一个候选在训练集上的汇总指标 (3 次 rollout 取均值)。
type CandidateMetrics struct {
	CandidateID string  `json:"candidate_id"`
	PassRate    float64 `json:"pass_rate"` // 全部 rollout 的 pass 比例
	CostTokens  int64   `json:"cost_tokens"`
	LatencyMS   int64   `json:"latency_ms"` // 只记录不进目标 (论文 4.1)
	NRollouts   int     `json:"n_rollouts"`
}

// EvalResult 验证集评测结果 (每候选每任务 1 次, 论文 Stage 3)。
type EvalResult struct {
	CandidateID string  `json:"candidate_id"`
	TaskID      string  `json:"task_id"`
	Pass        bool    `json:"pass"`
	PassFrac    float64 `json:"pass_frac"`
	TokensIn    int     `json:"tokens_in"`
	TokensOut   int     `json:"tokens_out"`
	LatencyMS   int64   `json:"latency_ms"`
	InfraError  bool    `json:"infra_error"`
	Error       string  `json:"error,omitempty"`
}

// CandidateEval 一个候选的验证集汇总 (选点输入)。
type CandidateEval struct {
	CandidateID string  `json:"candidate_id"`
	PassRate    float64 `json:"pass_rate"` // 验证集 pass 比例 (非 infra_error 样本)
	PassFrac    float64 `json:"pass_frac"` // 平均每题测试通过比例 (比 PassRate 更细粒度)
	CostTokens  int64   `json:"cost_tokens"`
	NEval       int     `json:"n_eval"`
}

// TierWeight 三档选点权重 (论文 4.3; (α,β)=(1,3),(1,1),(3,1) 起步做敏感性检查)。
type TierWeight struct {
	Tier  string  `json:"tier"`
	Alpha float64 `json:"alpha"` // d_pass 权重
	Beta  float64 `json:"beta"`  // d_cost 权重
}

// DefaultTiers 论文 Lite/Medium/Heavy 三档。
func DefaultTiers() []TierWeight {
	return []TierWeight{
		{Tier: "lite", Alpha: 1, Beta: 3},   // 偏成本
		{Tier: "medium", Alpha: 1, Beta: 1}, // 居中
		{Tier: "heavy", Alpha: 3, Beta: 1},  // 偏性能
	}
}

// SelectedSurvivor 一档选出的晋级者。
type SelectedSurvivor struct {
	Tier        string  `json:"tier"`
	J           float64 `json:"j"`
	CandidateID string  `json:"candidate_id"`
}

// SelectionReport 一轮选点的完整输出 (select 节点产出, orchestrator 据此组下一轮)。
type SelectionReport struct {
	Epoch     int                `json:"epoch"`
	Floor     float64            `json:"floor"`
	Evals     []CandidateEval    `json:"evals"`     // 全部候选的验证集指标 (含未晋级)
	Frontier  []string           `json:"frontier"`  // 帕累托前沿上的候选 id
	Filtered  []string           `json:"filtered"`  // 地板过滤出局的候选 id
	Survivors []SelectedSurvivor `json:"survivors"` // 三档选点 (前沿重合时 <3)
}

// evalAssert 执行一条断言。输出清洗: 去首尾空白后判定。
func evalAssert(out string, a Assertion) (bool, string) {
	t := strings.TrimSpace(out)
	fail := func(why string) (bool, string) { return false, why }
	switch a.Kind {
	case AssertContains:
		if strings.Contains(t, a.Param) {
			return true, ""
		}
		return fail(fmt.Sprintf("输出不包含 %q", a.Param))
	case AssertNotContains:
		if !strings.Contains(t, a.Param) {
			return true, ""
		}
		return fail(fmt.Sprintf("输出包含了违禁子串 %q", a.Param))
	case AssertRegex:
		re, err := regexp.Compile(a.Param)
		if err != nil {
			return fail("测试自身缺陷: 非法正则 " + a.Param)
		}
		if re.MatchString(t) {
			return true, ""
		}
		return fail(fmt.Sprintf("输出不匹配正则 %q", a.Param))
	case AssertStartsWith:
		if strings.HasPrefix(t, a.Param) {
			return true, ""
		}
		return fail(fmt.Sprintf("输出不以 %q 开头", a.Param))
	case AssertEndsWith:
		if strings.HasSuffix(t, a.Param) {
			return true, ""
		}
		return fail(fmt.Sprintf("输出不以 %q 结尾", a.Param))
	case AssertJSONParse:
		var v any
		if json.Unmarshal([]byte(t), &v) == nil {
			return true, ""
		}
		return fail("输出不是合法 JSON")
	case AssertJSONField, AssertJSONFieldEq:
		var v map[string]any
		if err := json.Unmarshal([]byte(t), &v); err != nil {
			return fail("输出不是 JSON 对象, 无法取字段 " + a.Param)
		}
		fv, ok := v[a.Param]
		if !ok {
			return fail("JSON 缺少字段 " + a.Param)
		}
		if a.Kind == AssertJSONField {
			return true, ""
		}
		if fmt.Sprintf("%v", fv) == a.Param2S() {
			return true, ""
		}
		return fail(fmt.Sprintf("字段 %s=%v 期望 %s", a.Param, fv, a.Param2S()))
	case AssertLenRange:
		lo, e1 := strconv.Atoi(a.Param2S())
		hi, e2 := strconv.Atoi(a.Param3S())
		if e1 != nil || e2 != nil {
			return fail("测试自身缺陷: len_range 界非整数")
		}
		if len(t) >= lo && len(t) <= hi {
			return true, ""
		}
		return fail(fmt.Sprintf("长度 %d 不在 [%d,%d]", len(t), lo, hi))
	case AssertWordRange:
		lo, e1 := strconv.Atoi(a.Param2S())
		hi, e2 := strconv.Atoi(a.Param3S())
		if e1 != nil || e2 != nil {
			return fail("测试自身缺陷: word_range 界非整数")
		}
		n := len(strings.Fields(t))
		if n >= lo && n <= hi {
			return true, ""
		}
		return fail(fmt.Sprintf("词数 %d 不在 [%d,%d]", n, lo, hi))
	case AssertCountOccur:
		want, err := strconv.Atoi(a.Param2S())
		if err != nil {
			return fail("测试自身缺陷: count_occur 期望非整数")
		}
		if strings.Count(t, a.Param) == want {
			return true, ""
		}
		return fail(fmt.Sprintf("子串 %q 出现 %d 次, 期望 %s", a.Param, strings.Count(t, a.Param), a.Param2S()))
	case AssertNoDoubleByte:
		for _, r := range t {
			if r > 127 {
				return fail("输出含双字节字符")
			}
		}
		return true, ""
	}
	return fail("未知断言种类 " + a.Kind)
}

// RunTests 在一套冻结测试上执行输出, 返回 (通过数, 判定明细)。
// 判定语义遵循论文纪律三: status=ok & passed=false = 产品失败;
// 测试自身缺陷 (非法正则/界) 计入失败但不记为产品失败 —— 这里统一按失败计,
// 由 TestSynth 出卷自检 (schema 校验) 把这类缺陷挡在冻结之前。
func RunTests(out string, tests []FrozenTest) (int, []string) {
	passed := 0
	var details []string
	for _, ts := range tests {
		ok, why := evalAssert(out, ts.Assert)
		if ok {
			passed++
		} else {
			details = append(details, fmt.Sprintf("%s: %s", ts.TestID, why))
		}
	}
	return passed, details
}

// ValidateAssertion schema 校验: TestSynth 自检的机械判据, 冻结前挡住"测试自身缺陷"。
func ValidateAssertion(a Assertion) error {
	switch a.Kind {
	case AssertContains, AssertNotContains, AssertStartsWith, AssertEndsWith:
		if a.Param == "" {
			return fmt.Errorf("%s: param 为空", a.Kind)
		}
	case AssertRegex:
		if _, err := regexp.Compile(a.Param); err != nil {
			return fmt.Errorf("regex 非法: %w", err)
		}
	case AssertJSONField, AssertJSONFieldEq:
		if a.Param == "" {
			return fmt.Errorf("%s: 字段名为空", a.Kind)
		}
	case AssertLenRange, AssertWordRange:
		lo, e1 := strconv.Atoi(a.Param2S())
		hi, e2 := strconv.Atoi(a.Param3S())
		if e1 != nil || e2 != nil || lo > hi {
			return fmt.Errorf("%s: 界非法", a.Kind)
		}
	case AssertCountOccur:
		if a.Param == "" {
			return fmt.Errorf("count_occur: param 为空")
		}
		if _, err := strconv.Atoi(a.Param2S()); err != nil {
			return fmt.Errorf("count_occur: 期望非整数")
		}
	case AssertJSONParse, AssertNoDoubleByte:
	default:
		return fmt.Errorf("未知断言种类 %q", a.Kind)
	}
	return nil
}

// Truncate 截断长文本 (rollout 落盘用, 诊断引用保留首尾)。
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n/2] + "\n…[truncated]…\n" + s[len(s)-n/2:]
}

// nowUnixMS 当前 Unix 毫秒 (测试可替换)。
var nowUnixMS = func() int64 { return time.Now().UnixMilli() }

// mustJSON 序列化 (panic 仅在程序员错误时)。
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
