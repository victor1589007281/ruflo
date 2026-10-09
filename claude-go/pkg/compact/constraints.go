// constraints.go —— 硬约束的抽取与压缩后核验 (H2-2)。
//
// # 为什么"让模型自己回答有没有丢约束"这条路走不通
//
// 方案原文写的是「压缩后加一道核验: 让模型回答'有没有丢掉当前任务的硬约束'」。
// 这句话按字面实现是**结构性不可能回答的**: 压缩之后模型看得见的只有摘要,
// 而"摘要里少了什么"是一个关于**它已经看不到的那部分**的问题 —— 拿摘要去问
// 摘要作者"你刚才漏了什么", 它只能依据摘要本身作答, 而那正是要检验的对象。
// 实测上模型几乎总会回答"没有遗漏" (它没有任何依据说遗漏了)。
//
// 所以核验必须由**仍然持有压缩前消息的那一方**来做, 也就是这里: 折叠驱动力在
// 折叠前先把硬约束抽出来, 折叠后拿这份清单去比对摘要正文。模型仍然是回答者,
// 但它回答的是**重压请求**(带着逐字列出的约束清单), 而不是"你丢了什么"。
// 这就是方案里"不合格则重压一次"能成立的前提。
//
// # 为什么只认文件路径/标识符类 key
//
// "约束保没保住"要可判定, 就不能靠语义相似度 (那正是 21.4.7 明确不上的向量)。
// 这里的判据是**判别性 token 的逐字节出现**: 摘要里出现了 `x.go`, 就说明这条
// 约束的落点还在; 没出现就说明压没了。选文件路径/标识符做 key 而不是整句,
// 是因为摘要本来就被要求"Be specific about file paths" —— 用已经被要求保留的
// 东西做判据, 不会引出"摘要写得好但没逐字复述原句"这类假阳性。
//
// 反过来, **抽不出 key 的约束一律标为不可核验, 绝不当作已保全**。一个无法判定的
// 约束如果混进"全部通过"里, 整套核验就成了一个恒真的闸门 (见验收方法论里的
// "空闸门集合上的全过恒真")。
package compact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// HardConstraint 一条从历史里抽出的硬约束。
type HardConstraint struct {
	// Text 约束原句 (截断到合理长度), 重压时逐字回灌给模型。
	Text string
	// Keys 判别性 token —— 全部出现在摘要里才算这条约束被保住。
	Keys []string
}

// Verifiable 报告这条约束能否被机械核验。
//
// false 表示抽不出任何判别性 token (例如"要小心一点"这类没有落点的约束)。
// **不可核验 ≠ 已保全**: 两者在统计里必须分开计, 否则核验结果无法证伪。
func (c HardConstraint) Verifiable() bool { return len(c.Keys) > 0 }

// ConstraintReport 一次核验的结果。
type ConstraintReport struct {
	// Total 抽出的约束条数 (含不可核验的)。
	Total int
	// Verifiable 其中可机械核验的条数。
	Verifiable int
	// Preserved 已确认保住的条数。
	Preserved int
	// Missing 判定为丢失的约束 (只可能是可核验的)。
	Missing []HardConstraint
}

// Unverifiable 抽不出判别 token 的条数。
func (r ConstraintReport) Unverifiable() int {
	n := r.Total - r.Verifiable
	if n < 0 {
		return 0
	}
	return n
}

// OK 报告是否一条都没丢。
//
// **空集合上恒为真**: 没有任何约束时 Preserved=0 且 Missing 为空 ⇒ true。调用方
// 要区分"全部保住"与"压根没约束"必须看 Total, 不能用 OK 代替 —— 这正是本包
// 坚持把两者的数都报出来的原因。
func (r ConstraintReport) OK() bool { return len(r.Missing) == 0 }

// 约束句的触发词。中英并列 —— 历史里两种语言混着出现是常态。
//
// 分"禁止"与"要求"两类只是为了可读性, 判定上等价: 两类都是"必须继续成立的
// 条件", 丢了都会让模型做出错误动作。
var constraintMarkers = []string{
	// 中文: 禁止
	"不许", "不准", "不要", "不得", "禁止", "严禁", "别改", "不能改", "切勿",
	"绝对不", "千万不",
	// 中文: 要求
	"必须", "只能", "务必", "一定要", "一定不",
	// 英文 (大小写不敏感)
	"must not", "do not", "don't", "never ", "must ", "shall not",
	"cannot", "can't", "required to", "only ",
}

// keyRe 判别性 token: 文件路径样式的串。
//
// 刻意**不含**普通单词 —— 普通单词在摘要里被同义改写是正常的, 拿它当判据会造出
// 大量假阳性。路径 (含 `.` 或 `/`) 则不同: 摘要被明确要求保留文件路径, 而一个
// 路径被换写成别的路径就是真的换了个东西。
//
// 两个分支: 带目录的路径 (a/b/c.go) 与裸文件名 (x.go)。扩展名限定为常见几类,
// 避免把句子末尾的 "..." 或 "e.g." 当成文件。
var keyRe = regexp.MustCompile(
	`(?:[\w.\-]+/)+[\w.\-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|rb|md|json|yaml|yml|toml|html|css|sql|sh|txt)` +
		`|[\w\-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|rb|md|json|yaml|yml|toml|html|css|sql|sh|txt)`)

// backtickRe 反引号里的内容 —— 写任务时用反引号标注的通常是必须逐字对待的符号
// (函数名/常量/命令)。长度限死, 反引号包一段散文的情况不算约束 key。
var backtickRe = regexp.MustCompile("`([^`\n]{1,60})`")

// sentenceSplitters 断句符。
//
// 刻意**不含**裸的 `.`: "不许改 x.go" 里的点一旦断句, key 就被劈成 "x" 与 "go",
// 抽取直接失效。英文句末的点靠 "'. '" (点+空格) 这个组合认 —— 路径里不会出现
// 空格, 所以这个区分是可靠的。
var sentenceSplitters = []string{"。", "！", "？", "；", "!", "?", ";", "\n", ". "}

// ExtractHardConstraints 从**即将被折叠**的消息里抽出硬约束。
//
// 只扫被折叠的那一段: 尾部保护的消息不参与折叠, 它们留在上下文里, 其中的约束
// 不会丢。系统提示词**刻意不扫** —— 它不在 messages 里, 折叠也从不碰它, 扫它
// 只会把"本来就活着"的约束算进核验基数, 稀释掉真正的信号。
func ExtractHardConstraints(messages []types.Message) []HardConstraint {
	seen := map[string]bool{}
	var out []HardConstraint

	for _, msg := range messages {
		// 只看 user 与 system 侧: assistant 说过的话不是对它的约束。
		if msg.Type != types.MessageTypeUser && msg.Type != types.MessageTypeSystem {
			continue
		}
		text := extractText(msg)
		if text == "" {
			continue
		}
		for _, sent := range splitSentences(text) {
			if !looksLikeConstraint(sent) {
				continue
			}
			c := HardConstraint{Text: clampText(sent, 300), Keys: constraintKeys(sent)}
			if seen[c.Text] {
				continue
			}
			seen[c.Text] = true
			out = append(out, c)
		}
	}
	return out
}

// splitSentences 断句。
func splitSentences(text string) []string {
	out := []string{text}
	for _, sep := range sentenceSplitters {
		var next []string
		for _, s := range out {
			next = append(next, strings.Split(s, sep)...)
		}
		out = next
	}
	var kept []string
	for _, s := range out {
		if t := strings.TrimSpace(s); t != "" {
			kept = append(kept, t)
		}
	}
	return kept
}

// looksLikeConstraint 判断一个句子是不是在陈述约束。
func looksLikeConstraint(sent string) bool {
	low := strings.ToLower(sent)
	for _, m := range constraintMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// constraintKeys 抽一条约束的判别性 token (去重 + 排序, 保证结果稳定)。
func constraintKeys(sent string) []string {
	set := map[string]bool{}
	for _, m := range keyRe.FindAllString(sent, -1) {
		set[m] = true
	}
	for _, m := range backtickRe.FindAllStringSubmatch(sent, -1) {
		if len(m) > 1 && strings.TrimSpace(m[1]) != "" {
			set[strings.TrimSpace(m[1])] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CheckConstraints 拿摘要正文核验约束清单。
//
// 判据是 key 的**逐字节出现**, 大小写不敏感 (摘要把 x.go 写成 X.go 属于改内容,
// 但把路径整体保留下来时不区分大小写是合理的宽容; 保守起见这里不做更宽的近似)。
func CheckConstraints(summary string, cs []HardConstraint) ConstraintReport {
	rep := ConstraintReport{Total: len(cs)}
	low := strings.ToLower(summary)
	for _, c := range cs {
		if !c.Verifiable() {
			continue // 不可核验的既不算保住也不算丢, 见 Unverifiable()
		}
		rep.Verifiable++
		lost := false
		for _, k := range c.Keys {
			if !strings.Contains(low, strings.ToLower(k)) {
				lost = true
				break
			}
		}
		if lost {
			rep.Missing = append(rep.Missing, c)
		} else {
			rep.Preserved++
		}
	}
	return rep
}

// ConstraintRetryMarker 重压提示词里约束块的**开头一句**。
//
// 单独导出是为了让测试能问一个**可证伪**的问题: "这条请求到底是不是一次带约束的
// 重压"。直接断言"请求正文里含 x.go"是恒真的 —— 历史正文里本来就写着 x.go, 于是
// "重压时忘了把约束传下去"这种实现照样全绿, 而它恰恰是本能力唯一要防的失效
// (写下时真的踩过一次: 把 summarize 的第三个参数改成 nil, 全部测试仍然通过)。
// 只有拿这段独属于重压路径的标记去问, 断言才咬得住。
const ConstraintRetryMarker = "CRITICAL: Your previous summary OMITTED the following hard constraints"

// constraintPromptBlock 把丢失的约束渲染成重压提示词里的一段。
//
// 逐字列出原句而不是给出关键词: 模型要能判断"这条约束在说什么"才谈得上保留它。
// 用编号列表而不是散文 —— 编号让"哪几条"在回读时能一一对上。
func constraintPromptBlock(missing []HardConstraint) string {
	if len(missing) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(ConstraintRetryMarker)
	sb.WriteString(" that MUST survive compaction. Reproduce each one verbatim, keeping every ")
	sb.WriteString("file path and identifier exactly as written:\n")
	for i, c := range missing {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, c.Text))
	}
	sb.WriteString("\nEverything else about the summary requirements is unchanged.\n\n")
	return sb.String()
}

// clampText 按 rune 截断 (与 truncate 不同: 这里明确要保留首尾关键的路径 token,
// 所以只砍尾部, 不插省略号打断可能的路径)。
func clampText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
