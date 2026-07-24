package graph

// condition.go —— 极简确定性条件语言 (design/01 §4.2 条件边的 v1 实现)。
//
// 这是 v1 有意的极简集: 全确定性、零依赖、可在 Validate 编译期完整检查。
// design/01 §4.2 提到的 CEL 表达式 (output.score >= 75) 是开放问题 (design/01 §九),
// 引入 CEL 意味着外部依赖与不可枚举的表达式空间, v1 先用可穷举的三类原子条件覆盖
// 现有 mode 的全部分支语义 (评分门禁/失败路由/关键词判定):
//
//	ok / fail                        —— 节点最终状态 (completed / failed)
//	score >= 75  (op: >= > <= < == !=) —— 门禁评分 float 比较
//	output contains "xxx"            —— 产出包含子串 (带引号, 大小写敏感)
//	output not_contains "xxx"        —— 产出不包含子串
//
// 空串解析为恒真条件 (无条件边)。

import (
	"fmt"
	"strconv"
	"strings"
)

// condKind 条件类别。零值 condTrue 使 Condition 零值即恒真。
type condKind int

const (
	condTrue   condKind = iota // 空条件: 恒真
	condOK                     // ok: 状态 == completed
	condFail                   // fail: 状态 == failed
	condScore                  // score <op> <num>
	condOutput                 // output contains / not_contains "..."
)

// Condition 已解析的条件。零值为恒真, 可直接 Eval。
type Condition struct {
	kind   condKind
	op     string  // score 比较符
	num    float64 // score 阈值
	substr string  // output 匹配子串
	negate bool    // true = not_contains
	raw    string  // 原始文本 (调试用)
}

// String 返回原始条件文本 (空条件返回 "<恒真>")。
func (c Condition) String() string {
	if c.raw == "" {
		return "<恒真>"
	}
	return c.raw
}

// ParseCondition 解析条件文本。空串 (或纯空白) 解析为恒真条件。
// 语法错误返回中文错误信息, Validate 在编译期调用以拦截坏条件。
func ParseCondition(s string) (Condition, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Condition{kind: condTrue}, nil
	}
	fields := strings.Fields(raw)
	switch fields[0] {
	case "ok":
		if len(fields) != 1 {
			return Condition{}, fmt.Errorf("条件 %q: ok 之后不允许附加内容", raw)
		}
		return Condition{kind: condOK, raw: raw}, nil
	case "fail":
		if len(fields) != 1 {
			return Condition{}, fmt.Errorf("条件 %q: fail 之后不允许附加内容", raw)
		}
		return Condition{kind: condFail, raw: raw}, nil
	case "score":
		if len(fields) != 3 {
			return Condition{}, fmt.Errorf("条件 %q: score 比较必须是 \"score <op> <数字>\" 三段式", raw)
		}
		op := fields[1]
		switch op {
		case ">=", ">", "<=", "<", "==", "!=":
		default:
			return Condition{}, fmt.Errorf("条件 %q: 不支持的比较符 %q (支持 >= > <= < == !=)", raw, op)
		}
		n, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return Condition{}, fmt.Errorf("条件 %q: 阈值 %q 不是数字", raw, fields[2])
		}
		return Condition{kind: condScore, op: op, num: n, raw: raw}, nil
	case "output":
		// 匹配串带引号且可含空格, 不能用 Fields 粗切, 按关键字剥离后取原始子串。
		rest := strings.TrimSpace(strings.TrimPrefix(raw, "output"))
		negate := false
		var lit string
		switch {
		case strings.HasPrefix(rest, "not_contains"):
			negate = true
			lit = rest[len("not_contains"):]
		case strings.HasPrefix(rest, "contains"):
			lit = rest[len("contains"):]
		default:
			return Condition{}, fmt.Errorf("条件 %q: output 之后必须跟 contains 或 not_contains", raw)
		}
		if lit == "" || (lit[0] != ' ' && lit[0] != '\t') {
			return Condition{}, fmt.Errorf("条件 %q: contains/not_contains 之后缺少空白分隔的匹配串", raw)
		}
		lit = strings.TrimSpace(lit)
		if len(lit) < 2 || !strings.HasPrefix(lit, `"`) || !strings.HasSuffix(lit, `"`) {
			return Condition{}, fmt.Errorf("条件 %q: 匹配串必须用双引号包裹 (大小写敏感, 不支持转义)", raw)
		}
		return Condition{kind: condOutput, substr: lit[1 : len(lit)-1], negate: negate, raw: raw}, nil
	default:
		return Condition{}, fmt.Errorf("条件 %q: 无法识别 (支持: ok | fail | score <op> <数字> | output contains \"...\" | output not_contains \"...\")", raw)
	}
}

// Eval 对节点结果求值。恒真条件恒返回 true; 未知类别防御性返回 false。
func (c Condition) Eval(r NodeResult) bool {
	switch c.kind {
	case condTrue:
		return true
	case condOK:
		return r.Status == NodeStatusCompleted
	case condFail:
		return r.Status == NodeStatusFailed
	case condScore:
		switch c.op {
		case ">=":
			return r.Score >= c.num
		case ">":
			return r.Score > c.num
		case "<=":
			return r.Score <= c.num
		case "<":
			return r.Score < c.num
		case "==":
			return r.Score == c.num
		case "!=":
			return r.Score != c.num
		}
		return false
	case condOutput:
		has := strings.Contains(r.Output, c.substr)
		if c.negate {
			return !has
		}
		return has
	}
	return false
}
