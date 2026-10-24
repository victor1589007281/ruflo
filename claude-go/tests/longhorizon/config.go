// config.go —— H4-2 的"除被测开关外无其他差异"这一半。
//
// # 这一半为什么必须由机器判, 不能由人保证
//
// 方案 §21.5.6 给 H4-2 的验收是两句话:
//
//	能给出两组成功率与置信区间; 且两组**除被测开关外无其他差异** (配置 diff 可查)
//
// 后一句是前一句成立的前提 —— 只要两组配置还有别的差异, 成功率之差就不能归给被测
// 开关。而"无其他差异"最容易在**演进中**丢掉: 有人给 A 组加了个超时, B 组忘了;
// 有人改了 A 组的模型别名, B 组还留着旧的。这类漂移在汇总数字上完全看不出来
// (成功率差了 10 个点, 看起来像"闸门有效"), 所以它必须是断言, 不是纪律。
//
// # 判据落在**路径**上, 不落在"两条命令看起来差不多"
//
// 两组配置各是一份 JSON。差异用**路径集合**表达 (如 /ai/maxTurns,
// /engine/verificationGate), 而不是文本 diff —— 文本 diff 会把缩进、键序、
// 数字写法 (1e3 vs 1000) 都算成差异, 于是"逐字节相同"这条永远过不了, 人就会把
// 它关掉。路径级比较才是"配置语义上有没有差异"的正确粒度。
package longhorizon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// KnobEnv 一个被测开关的环境变量名 + 它在配置里的**路径**。
//
// 为什么要成对给出: 环境变量是**施加**开关的手段, 路径是**核对**开关的判据。
// 只给环境变量, 就没法验证"这个变量真的改到了配置里对应的那一项" —— 设一个
// 拼错的变量, 两组配置完全相同, 而汇总数字上看起来像"这个开关没效果"。
type KnobEnv struct {
	// Name 开关名 (报告里用)。
	Name string
	// Env 施加它用的环境变量 (空 = 这个开关不走环境变量, 只能用配置文件)。
	Env string
	// ConfigPath 它在配置里的路径 (如 /engine/verificationGate)。
	ConfigPath string
	// On / Off 两个取值。
	On  string
	Off string
}

// RunSpec 一次运行的完整输入: 配置文档 + 环境变量。
//
// **两者都要**, 因为本仓的开关不都在配置里: H4-3 的档位在配置 (/ai/longHorizonMaxTurns),
// 而 H0-3 的验证闸门根本不在配置里 —— 它是 CLAUDE_GO_RUN_INTERCEPTORS 这个环境变量
// 上的一个白名单项 (pkg/agent/run_interceptors.go, "gate" 是非 core 环,
// 设成 off 即关)。只核配置的台子会对闸门那一组给出"两组配置完全相同"的假绿,
// 而那正是 A/B 最该防的形态。
type RunSpec struct {
	// Config 配置文档 (any, 由 ParseConfigDoc 得来)。
	Config any
	// Env 本次运行额外的环境变量 (不含继承的宿主环境)。
	Env map[string]string
}

// ConfigPair 两组配对的运行输入: 同一份基线, 只在被测开关上取两个值。
//
// A 与 B 的 Config 都是**完整**文档, 不是 patch —— 补丁形态在"基线演进"时
// 会静默失配 (patch 打不上就跳过, 于是两组跑的是同一份配置)。完整文档下,
// DiffPaths 能看见任何差异。
type ConfigPair struct {
	// Base 基线配置 (两组都从它派生)。
	Base any
	// A / B 两组运行输入。
	A RunSpec
	B RunSpec
	// Knob 被测开关。
	Knob KnobEnv
}

// DiffEnv 返回两组环境变量在**开关路径**上的差异。
//
// 只报 env 里显式声明的键: 宿主继承来的环境两组都一样, 报出来只是噪声。
// 键名排序保证同样输入给同样输出 (报告要可比)。
func DiffEnv(a, b map[string]string) []string {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var out []string
	for _, k := range sorted {
		x, hasX := a[k]
		y, hasY := b[k]
		if hasX == hasY && x == y {
			continue
		}
		switch {
		case hasX && !hasY:
			out = append(out, fmt.Sprintf("env:%s: %q -> (未设)", k, x))
		case !hasX && hasY:
			out = append(out, fmt.Sprintf("env:%s: (未设) -> %q", k, y))
		default:
			out = append(out, fmt.Sprintf("env:%s: %q -> %q", k, x, y))
		}
	}
	return out
}

// DiffPaths 返回两份 JSON 文档在**语义路径**上的差异, 形如
// "/ai/maxTurns: 25 -> 150"。数组按下标展开 (a[0].x)。
//
// 返回空切片表示两份文档语义相同 (键序/缩进/数字写法不同不算差异)。
func DiffPaths(a, b any) []string {
	var out []string
	diffInto("", a, b, &out)
	sort.Strings(out)
	return out
}

// diffInto 递归比对两份 JSON 文档, 把差异按语义路径追加到 out。
//
// 算法解读：
//
//	原理：按 Go 的 any 动态类型分派 —— map 走键并集 (逐键递归或记增删), 数组按下标
//	      展开 (先记长度差再逐位递归), 标量用 jsonEqual 判等。
//	关键不变量：**键的遍历顺序必须确定** (键并集先 sort 再遍历) —— Go 的 map 迭代
//	      顺序随机, 不排序会让两次同样的比对产出不同顺序的差异列表, 下游按行断言就成
//	      了随机失败。
//	代价与边界：路径拼接用 "/" 且数组下标写成 "[i]"; 标量判等走 json.Marshal 比较
//	      字符串, 所以 1 与 1.0 是否相等取决于 json.Number 保真 (见 ParseConfigDoc)。
//	设计权衡：用路径字符串而不是返回结构化 diff —— 报告要给人读, 且断言只需比对
//	      文本; 代价是下游想程序化消费差异得自己再解析这行文本 (见 pathOf)。
func diffInto(path string, a, b any, out *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: %s -> %s", pathOrRoot(path), typeName(a), typeName(b)))
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			x, hasX := av[k]
			y, hasY := bv[k]
			child := path + "/" + k
			switch {
			case hasX && !hasY:
				*out = append(*out, fmt.Sprintf("%s: %s -> (缺失)", child, compact(x)))
			case !hasX && hasY:
				*out = append(*out, fmt.Sprintf("%s: (缺失) -> %s", child, compact(y)))
			default:
				diffInto(child, x, y, out)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: %s -> %s", pathOrRoot(path), typeName(a), typeName(b)))
			return
		}
		if len(av) != len(bv) {
			*out = append(*out, fmt.Sprintf("%s: 长度 %d -> %d", pathOrRoot(path), len(av), len(bv)))
		}
		n := len(av)
		if len(bv) < n {
			n = len(bv)
		}
		for i := 0; i < n; i++ {
			diffInto(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], out)
		}
	default:
		if !jsonEqual(a, b) {
			*out = append(*out, fmt.Sprintf("%s: %s -> %s", pathOrRoot(path), compact(a), compact(b)))
		}
	}
}

// pathOrRoot 空路径显示成 "(根)", 免得差异行以 ": " 开头看不出层级。
func pathOrRoot(p string) string {
	if p == "" {
		return "(根)"
	}
	return p
}

// jsonEqual 用 JSON 序列化结果判等 (任一侧 Marshal 失败即判不等)。
func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

// compact 把值压成单行短串 (超过 60 字节截断为 "..."), 供差异行内联显示。
func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// typeName 给出值的类型名 (容器与 null 用中文, 其余用 %T)。
func typeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "对象"
	case []any:
		return "数组"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// AssertOnlyKnobDiffers 核对一对运行输入**只**在被测开关上不同。返回违规说明
// (为空表示合规)。
//
// 判定分三步, 缺一不可:
//
//  1. 两组配置在 Knob 的配置路径之外的键**必须完全相同**。
//  2. 两组环境变量在 Knob 的环境变量之外的键**必须完全相同**。
//  3. 开关本身**必须真的被改到了** (配置路径或环境变量至少一处不同) ——
//     否则这不是一次 A/B, 是同一组跑了两遍。第 3 条尤其重要: 环境变量设错
//     (拼写/取值非法) 时两组的**全部输入逐字节相同**, 而报告只能说
//     "这个开关没有效果" —— 那是最难识破的一种假结论。
func (p ConfigPair) AssertOnlyKnobDiffers() error {
	cfgDiffs := DiffPaths(p.A.Config, p.B.Config)
	envDiffs := DiffEnv(p.A.Env, p.B.Env)

	knobPath := normalizePath(p.Knob.ConfigPath)
	knobEnvKey := "env:" + strings.TrimSpace(p.Knob.Env)

	var onKnob, offKnob []string
	for _, d := range cfgDiffs {
		if pathOf(d) == knobPath {
			onKnob = append(onKnob, d)
			continue
		}
		offKnob = append(offKnob, d)
	}
	if knobEnvKey != "env:" {
		for _, d := range envDiffs {
			if pathOf(d) == knobEnvKey {
				onKnob = append(onKnob, d)
				continue
			}
			offKnob = append(offKnob, d)
		}
	} else {
		offKnob = append(offKnob, envDiffs...)
	}

	if len(offKnob) > 0 {
		return fmt.Errorf("两组在被测开关 (%s) 之外还有 %d 处差异 —— "+
			"成功率之差不能再归给这个开关: %s",
			knobPath, len(offKnob), strings.Join(offKnob, "; "))
	}
	if len(onKnob) == 0 {
		return fmt.Errorf("两组在被测开关 %s 上**完全相同** (配置与环境变量都没变) —— "+
			"这不是一次 A/B, 是同一组跑了两遍 (开关名拼错/取值非法时就是这个形态, "+
			"而报告只会说「这个开关没有效果」)", knobPath)
	}
	return nil
}

// DiffSummary 返回全部差异的字符串 (报告用, 供"配置 diff 可查"这一条落地)。
func (p ConfigPair) DiffSummary() string {
	cfg := DiffPaths(p.A.Config, p.B.Config)
	env := DiffEnv(p.A.Env, p.B.Env)
	if len(cfg) == 0 && len(env) == 0 {
		return "(两组输入完全相同 —— 不是一次 A/B)"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "被测开关: %s (配置路径 %s, 环境变量 %s)\n",
		p.Knob.Name, p.Knob.ConfigPath, orNone(p.Knob.Env))
	fmt.Fprintf(&sb, "配置差异 (%d 处):\n", len(cfg))
	for _, d := range cfg {
		fmt.Fprintf(&sb, "  %s\n", d)
	}
	fmt.Fprintf(&sb, "环境差异 (%d 处):\n", len(env))
	for _, d := range env {
		fmt.Fprintf(&sb, "  %s\n", d)
	}
	return sb.String()
}

// orNone 空串显示成 "(无)" (开关没配环境变量时的占位)。
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(无)"
	}
	return s
}

// pathOf 从 "a/b/c: x -> y" 里取出 "a/b/c"。
func pathOf(diffLine string) string {
	if i := strings.Index(diffLine, ": "); i >= 0 {
		return diffLine[:i]
	}
	return diffLine
}

// normalizePath 统一路径写法: 去掉首尾空白与结尾的 /, 补上前导 /。
//
// **刻意不做点号转换** (`/ai.maxTurns` -> `/ai/maxTurns`): 配置键里本来就可能含点
// (模型别名如 `qwen2.5-coder`、provider 名 `ollama:gemma4:26b`), 一起转换会让
// 一个含点的键在比较时被悄悄改写 —— 于是"路径相等"判错, 而这正是本文件要防的
// 那类沉默偏差。路径一律用斜杠写。
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// SetPath 在 JSON 文档里按路径写一个值, 返回新文档 (不改原文档)。
//
// 用途: 由 Base + Knob 生成 A / B。**不是**用来在跑之前临时改配置 ——
// 那样两组的差异就只存在于内存里, 事后 diff 无从查起, 而"配置 diff 可查"
// 恰恰是方案对 H4-2 的验收口径之一。
func SetPath(doc any, path string, value any) (any, error) {
	norm := normalizePath(path)
	if norm == "/" {
		return value, nil
	}
	segs := strings.Split(strings.TrimPrefix(norm, "/"), "/")
	root, ok := deepCopy(doc).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("SetPath 需要根是对象, 实得 %T", doc)
	}
	cur := root
	for i, seg := range segs {
		if seg == "" {
			return nil, fmt.Errorf("路径 %q 里有空段", path)
		}
		if i == len(segs)-1 {
			cur[seg] = value
			return root, nil
		}
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	return root, nil
}

// deepCopy 深拷贝 JSON 文档 (只认 map/slice, 标量原样返回); 供 SetPath 不原地改原文档。
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = deepCopy(x)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = deepCopy(x)
		}
		return s
	}
	return v
}

// ParseConfigDoc 把一份 JSON 配置字节解析成文档 (数字按 json.Number 保真)。
func ParseConfigDoc(b []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("配置不是合法 JSON: %w", err)
	}
	return v, nil
}
