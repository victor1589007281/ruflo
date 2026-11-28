// Package asset —— 资产模型 (21.8.12 P1)。
//
// 本包只做两件事, 且刻意只依赖标准库:
//
//  1. **一种 frontmatter 解析器** (frontmatter.go)。全仓此前只有 pkg/skills 有一份,
//     而且是 `*Skill` 绑定的私有函数 —— 于是每加一种带 frontmatter 的资产格式,
//     就会长出第二份解析器, 两份对 CRLF / 多行列表 / 引号的处理必然分叉。
//  2. **Asset: 一个带 kind 的资产声明** (asset.go), kind ∈ agent/workflow/plugin/
//     skill/tool/hook。此前这六种东西各有各的结构体、各有各的目录、各有各的加载器,
//     没有任何一个地方能回答"这台机器上声明了哪些资产"。
//
// 为什么把解析器放在这里而不是 pkg/skills: 依赖方向必须是 skills → asset, 反过来
// 会让 asset 拽进 pkg/tool 与 pkg/types, 而 asset 要被 dashboard/feishu/cmd 三条
// 互不相干的路径引用 —— 一个零依赖的叶子包才能被它们同时引用而不引入环。
package asset

import (
	"sort"
	"strings"
)

// Frontmatter 一次拆分的完整结果。
//
// 三态必须分清 (这是本包最容易被用错的地方):
//   - Present=false: 内容里**根本没有** frontmatter 块, 正文就是全部内容;
//   - Present=true 且 Values/Lists 为空: 有 `---` 包起来的块, 但块里没有有效键;
//   - Present=true 且有值: 正常。
//
// 把前两种混成一种 ("没读到键") 正是 pkg/skills 里那个 CRLF 陷阱的形态: 文件里
// 明明写了 `name: foo`, 解析结果却与"文件里什么都没写"完全一样, 而且没有任何报错。
type Frontmatter struct {
	// Values 单值键。键已归一 (小写, '_' → '-'), 值已去引号。
	Values map[string]string
	// Lists 多行列表键 (`key:` 后面跟 `- item` 缩进行)。
	Lists map[string][]string
	// Body frontmatter 之后的正文 (已 TrimSpace); 无 frontmatter 时为原内容。
	Body string
	// Present 是否解析到了完整的 `---` ... `---` 块。
	Present bool
}

// Get 按归一后的键取单值 (找不到返回空串)。
func (f Frontmatter) Get(key string) string {
	return f.Values[NormalizeKey(key)]
}

// List 按归一后的键取列表 (找不到返回 nil)。
func (f Frontmatter) List(key string) []string {
	return f.Lists[NormalizeKey(key)]
}

// Keys 返回全部**单值**键, 已排序。
//
// 排序而不是 map 遍历序: 消费方 (pkg/skills.parseFrontmatter) 是用它驱动一个 switch
// 逐键赋值的, 顺序不定时最后一个 `paths:` 写进谁就是随机的。实际上 switch 的每个
// 分支互不影响, 但"顺序不定"这件事在别处一定会咬人, 而在源头 (这里) 排一次序是零成本的。
func (f Frontmatter) Keys() []string {
	out := make([]string, 0, len(f.Values))
	for k := range f.Values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NormalizeKey 归一键名: 小写 + 下划线转连字符。
//
// 为什么归一而不是同时查两种写法: pkg/skills 的 switch 里每个键都要写两遍
// (`case "model-invocable", "model_invocable":`), 漏写一遍就是一个"配置写了不生效"
// 的静默失败。归一之后键的拼写差异在进入 switch 之前就被抹平了。
func NormalizeKey(key string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "_", "-")
}

// SplitFrontmatter 把内容拆成 frontmatter + 正文。
//
// 识别规则与 pkg/skills 逐字节一致 (整个 SKILL.md 生态依赖它, 不能改):
// 内容必须以**精确的** "---\n" 开头, 且后面能找到 "\n---" 作为结束行。
//
// 与旧实现的两处**有意的**差异, 都是修 bug 而不是改语义:
//
//  1. **CRLF 不再整块丢弃**。旧实现只认 "---\n", 一个用 Windows 换行写出的 SKILL.md
//     会让 frontmatter 全部消失, 且表现成"这个文件本来就没写 frontmatter" —— 名字
//     回落成目录名, description 回落成正文第一行, 没有任何报错。这里先归一 CRLF。
//  2. **多行列表键泛化**。旧实现只对 `paths` 认 `- item` 续行, 别的键写多行列表会被
//     静默丢掉。这里对所有键都认 —— 对既有 SKILL.md 是纯扩展 (旧实现只读它认识的那
//     几个键, 多收进来的键它不查)。
func SplitFrontmatter(content string) Frontmatter {
	fm := Frontmatter{
		Values: map[string]string{},
		Lists:  map[string][]string{},
		Body:   content,
	}
	// CRLF / 孤立 CR 归一: 见上差异 1。
	if strings.ContainsRune(content, '\r') {
		content = strings.ReplaceAll(content, "\r\n", "\n")
		content = strings.ReplaceAll(content, "\r", "\n")
	}
	if !strings.HasPrefix(content, "---\n") {
		fm.Body = content
		return fm
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		// 有开没有闭: 视为没有 frontmatter, 正文是全文 (与旧实现一致 —— 此时把
		// 前半段当 frontmatter 丢掉会让整篇文档少一半, 那更坏)。
		fm.Body = content
		return fm
	}
	fm.Present = true
	block := content[4 : 4+end]
	fm.Body = strings.TrimSpace(content[4+end+4:])

	// curList 当前正在累积的多行列表键; 遇到任何新键行就清空。
	curList := ""
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "-") {
			item := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "-")), `"'`)
			if item != "" && curList != "" {
				fm.Lists[curList] = append(fm.Lists[curList], item)
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue // 没有冒号的行既不是键也不是列表项, 无从归类
		}
		key = NormalizeKey(key)
		if key == "" {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		delete(fm.Lists, key) // 同名键重新出现时, 旧的多行列表作废
		if val == "" {
			curList = key // 空值 = 后面跟 `- item` 续行
			continue
		}
		// YAML 块标量 (`key: |` / `key: >`, 允许 -/+ chomping): 值在缩进续行里。
		//
		// 不认它的代价是**静默的**: description 变成字面量 "|" 或 ">"。
		// 实测 (2026-11-28) ~/.claude/skills 里 agent-reach/ima-skill 都这么写,
		// 清单里就出现 "- agent-reach: >" —— 描述正是技能路由的唯一信号,
		// 等于这两个技能对模型不可路由。
		if isBlockScalarHeader(val) {
			body, next := collectBlockScalar(lines, i+1)
			fm.Values[key] = foldBlockScalar(body, val)
			curList = ""
			i = next - 1
			continue
		}
		curList = ""
		fm.Values[key] = val
	}
	return fm
}

// isBlockScalarHeader 判定 `key:` 后面的值是不是块标量头 (`|` / `>`, 可带 chomping)。
func isBlockScalarHeader(val string) bool {
	switch strings.TrimSpace(val) {
	case "|", "|-", "|+", ">", ">-", ">+":
		return true
	}
	return false
}

// collectBlockScalar 从 lines[start:] 收集块标量的缩进续行, 返回去掉块缩进后的
// 行 (空行保留为空串) 与块的结束下标 (第一条非缩进非空行)。
//
// 缩进宽度取**第一条非空行**的前导空格数 (YAML 的自动探测规则)。
func collectBlockScalar(lines []string, start int) ([]string, int) {
	indent := -1
	end := start
	for end < len(lines) {
		raw := lines[end]
		if strings.TrimSpace(raw) == "" {
			end++ // 空行属于块内, 等看到下一条内容行再定缩进
			continue
		}
		lead := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if lead == 0 {
			break // 非缩进行: 块结束
		}
		if indent < 0 {
			indent = lead
		}
		end++
	}
	if indent < 0 {
		// 全是空行/没有内容: 返回空块, 但指针仍要越过这些行。
		return nil, end
	}
	out := make([]string, 0, end-start)
	for _, raw := range lines[start:end] {
		if strings.TrimSpace(raw) == "" {
			out = append(out, "")
			continue
		}
		// 缩进不足的行 (比首行浅但仍有缩进) 按可用宽度切, 不 panic。
		cut := indent
		if len(raw) < cut {
			cut = len(raw)
		}
		out = append(out, strings.TrimRight(raw[cut:], " \t"))
	}
	return out, end
}

// foldBlockScalar 按 YAML 语义把块内容折成字符串:
//
//	| (literal) 保留换行;  > (folded) 相邻行用空格连接, 空行表示段落分隔。
//
// 尾部换行一律去掉: 消费方是 description 这类单值字段, 尾部空白没有语义,
// 而把它带进清单会把"一行一个技能"的格式撑开。
func foldBlockScalar(lines []string, header string) string {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(header, "|") {
		return strings.TrimRight(strings.Join(lines, "\n"), "\n")
	}
	// folded
	var sb strings.Builder
	blank := false
	wrote := false
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			blank = true
			continue
		}
		if wrote {
			if blank {
				sb.WriteString("\n")
			} else {
				sb.WriteString(" ")
			}
		}
		sb.WriteString(strings.TrimSpace(ln))
		wrote = true
		blank = false
	}
	return sb.String()
}

// ParseInlineList 解析行内列表写法, 兼容三种在野形态: `A, B` / `[A, B]` / `A B`。
//
// 与 pkg/skills.parseToolList 逐字节同规则, 因为它面对的是同一批在野文件。逗号优先,
// 无逗号才按空白切 —— 反过来会把 "Read, Bash" 切成 "Read," 这种带标点的假名字,
// 而下游对"不认识的名字"通常是拒绝, 于是一个格式问题会变成一次误拒。
//
// 语义说明: 本函数不知道它切的是工具名、路径还是标签。前端把 `paths` 与
// `allowed-tools` 都送进这里, 是因为它们在文件里就是同一种写法。
func ParseInlineList(val string) []string {
	val = strings.TrimSpace(strings.Trim(strings.TrimSpace(val), "[]"))
	if val == "" {
		return nil
	}
	fields := strings.Split(val, ",")
	if !strings.Contains(val, ",") {
		fields = strings.Fields(val)
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if n := strings.Trim(strings.TrimSpace(f), `"'`); n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseBool 宽容布尔解析。1/t/true/yes/on = true, 其余一律 false。
//
// fail-safe 的方向: 配置写错时按"关闭"处理。对一个 `model-invocable` 来说, 写错时
// 取 true 会把本该只作用户命令的技能悄悄暴露给模型 —— 那是一个权限扩张, 不是一次
// 无害的格式错误。
func ParseBool(val string) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "t", "true", "yes", "on":
		return true
	default:
		return false
	}
}
