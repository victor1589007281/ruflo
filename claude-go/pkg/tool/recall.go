// recall.go —— 13.7 池化的**验收度量** (D4.2): 「工具段 token 不随池子增长」与
// 「漏召率趋零」这两句话, 在这里第一次变成可以算出来的两个数。
//
// ── 为什么单列一个文件: 这两条之前**没有被算过** ─────────────────────────────
//
// 池化本身 (pool.go / pooltools.go) 早就实现了, 但它的两条验收口径一直停留在
// 散文层面。这很危险, 因为两条都是"看起来当然成立"的那种话:
//
//	· 工具段 token 不随池子增长 —— 池子从 19 件涨到 500 件时, 广告面确实只多出
//	  已加载的那几件, 所以听起来当然成立。但**没人算过基数**: 包装三件自身的
//	  schema 也是工具段的固定成本, 池化到底省了多少、什么规模下才开始省, 都没数。
//	· 漏召率趋零 —— 更危险。现在的检索是**子串命中**: 查询词得逐字出现在资产的
//	  name/description 里才召回。实测 (见 recall_test.go 的保留集) 自然语言查询的
//	  漏召率是 50%: "影响面" 召不回 code_intel_query, "who calls this function"
//	  也召不回。而漏召的表现是 pool_search 返回"没有匹配资产" —— 模型会当作
//	  「这个能力不存在」而放弃。这是静默的: 没有任何地方会红。
//
// 题库 Q32 点名了这件事: 运维场景里漏召比误召严重得多 (该用的工具没给, Agent
// 会用错误的工具硬凑结论), 所以召回要偏宽, 精度交给执行前的参数校验去兜。
//
// ── token 数: 这是**口径**不是计量 ─────────────────────────────────────────
//
// SurfaceTokens 算的是字节数/4 (与 pkg/codeintel 的 Tokens 同一口径)。它**不是**
// 上游 provider 报的 token 数 —— 那是要真发一次请求才知道的。它的用途是**比较**:
// 同一个保留集在"池化前 vs 池化后"、"池子 19 件 vs 500 件"下的相对大小。验收
// 要的也正是比较 ("不随池子增长"), 不是绝对值。
//
// 所以这个数不能用来说"省了多少钱" —— 那需要真实计量。用它做趋势判断是对的,
// 做绝对值结论是错的。
package tool

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// SurfaceTokens 估算一组工具定义占的 token 段大小。
//
// 口径 = (名称 + 描述 + schema) 的字节数 / 4, 与 pkg/codeintel 同一算法。用它做
// 配置之间的**相对**比较 (见文件头「口径不是计量」)。
func SurfaceTokens(tools []types.APITool) int {
	n := 0
	for _, t := range tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return n / 4
}

// RecallCase 是保留集里的一条: 一个**真实会出现的查询**, 与它必须召回的资产。
//
// Want 是"至少要召回到其中之一"的意思 (一个意图常有多种工具能满足), 不是
// "必须全部召回" —— 后者会把合理的替代路径算成漏召。
type RecallCase struct {
	// Query 用户/模型真会打出来的话。**刻意收自然语言**: 逐字关键词查询
	// (直接抄工具名) 是子串检索唯一能过的情形, 拿它做保留集会得到 100% 召回率
	// 与一个恒真的验收。
	Query string
	// Want 满足这个意图的资产名 (任一命中即算召回)。
	Want []string
	// Note 说明这条用例为什么在保留集里 (它守的是哪种失败)。
	Note string
}

// RecallMiss 一次漏召。
type RecallMiss struct {
	Case RecallCase
	// Got 这次实际召回了什么。空 = 一条都没召回 —— 那是最坏的情形, 因为它与
	// "池子里没有这个能力"在模型看来完全一样。
	Got []string
}

// RecallReport 保留集上的一次跑分。
type RecallReport struct {
	Total int
	// Hit 至少召回一条 Want 的用例数。
	Hit int
	// Empty 召回结果**为空**的用例数。单列它是因为它的危害与"召回了错的东西"
	// 不同: 空结果会被读成"没有这个能力", 而错召回至少还给了模型一个东西去试。
	Empty int
	Miss  []RecallMiss
}

// MissRate 漏召率 (0-1)。Total=0 时返回 -1 —— **不是 0**。
//
// 空保留集的"漏召率 0%"是一个恒真的假结论 (与 D4.1 里空闸门集合上的"全过"同型)。
// 调用方必须把 -1 当成"没测过"而不是"测过了没问题"。
func (r RecallReport) MissRate() float64 {
	if r.Total == 0 {
		return -1
	}
	return float64(r.Total-r.Hit) / float64(r.Total)
}

// EvaluateRecall 在保留集上跑一次检索, 算出漏召率。
//
// search 由调用方给 (池内检索实现); 这个函数不认识池, 只认"查询 → 召回名单"。
// 这样度量与实现解耦: 换了检索算法 (子串 → RRF), 保留集与判据都不用动。
//
// search 返回 nil 或空切片都算空结果 (两者在模型侧无法区分)。
func EvaluateRecall(search func(query string) []string, cases []RecallCase) RecallReport {
	var rep RecallReport
	rep.Total = len(cases)
	for _, c := range cases {
		got := search(c.Query)
		hit := false
		for _, g := range got {
			for _, w := range c.Want {
				if g == w {
					hit = true
				}
			}
		}
		if hit {
			rep.Hit++
			continue
		}
		if len(got) == 0 {
			rep.Empty++
		}
		rep.Miss = append(rep.Miss, RecallMiss{Case: c, Got: got})
	}
	return rep
}

// MissSummary 给日志/页面的一句话。空保留集**必须**说"没测过"。
func (r RecallReport) MissSummary() string {
	if r.Total == 0 {
		return "保留集是空的 —— 漏召率**没测过**, 不是「没有漏召」"
	}
	return formatMissRate(r)
}

// formatMissRate 把漏召报告格式化成 "x/y 漏召 (z%)", 并在存在空结果时追加一句解释。
//
// 注意：空结果要单独点出来 —— 「召回了但内容为空」会被读成「池内没有这个能力」,
// 与「真的没命中」是两种不同的故障, 合并成一个百分比会掩盖前者。
func formatMissRate(r RecallReport) string {
	rate := r.MissRate()
	pct := int(rate*100 + 0.5)
	s := strings.Builder{}
	s.WriteString(itoa(r.Total-r.Hit) + "/" + itoa(r.Total) + " 漏召 (" + itoa(pct) + "%)")
	if r.Empty > 0 {
		s.WriteString(", 其中 " + itoa(r.Empty) + " 条**召回结果为空**" +
			" —— 空结果会被读成「池内没有这个能力」")
	}
	return s.String()
}

// itoa 局部小实现 (避免为一处引入 strconv 的可读性噪声)。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// SurfaceReport 是"工具段 token 常数化"的验收依据。
//
// 它把两个数并排给出: 池子有多大 (members), 广告面实际下发多少 token (tokens)。
// 验收要的是**后者不随前者涨** —— 只报前者会让人以为池子越大越贵, 只报后者则
// 看不出池子涨了多少 (也就看不出这个常数是在多大的池子上成立的)。
type SurfaceReport struct {
	// Members 池成员总数 (下沉面)。
	Members int
	// Advertised 这一轮实际下发的工具数 (基础面 + 包装三件 + 已加载 + 常驻)。
	Advertised int
	// Tokens 下发的工具段 token 口径 (见 SurfaceTokens)。
	Tokens int
	// BaselineTokens 同样是这些工具**完全不做池化**时的 token 口径 ——
	// 也就是"假如所有成员都在广告面上"。它与 Tokens 的差就是池化省下的量。
	//
	// 两个数要一起给: 只给 Tokens 的话, 一个"池子里本来就没几件"的部署看起来
	// 与一个"池子很大但广告面压住了"的部署一模一样。
	BaselineTokens int
}

// EvaluateSurface 算一次广告面的 token 占用。
//
// filtered 是**这一轮真正要下发的**工具集 (已过池过滤); unfiltered 是过滤前的
// 全量表。两者都传进来而不是只传 filtered: 没有全量表就算不出"省了多少", 而
// "省了多少"正是这条验收要回答的。
func EvaluateSurface(filtered, unfiltered []types.APITool, members int) SurfaceReport {
	return SurfaceReport{
		Members:        members,
		Advertised:     len(filtered),
		Tokens:         SurfaceTokens(filtered),
		BaselineTokens: SurfaceTokens(unfiltered),
	}
}

// SavingsNote 给页面的一句话。它刻意**不给百分比**外的绝对结论 ——
// token 口径是估算, 见文件头。
func (s SurfaceReport) SavingsNote() string {
	if s.BaselineTokens == 0 {
		return "全量为空 —— 省下多少无从谈起"
	}
	if s.Tokens >= s.BaselineTokens {
		// 广告面不小于全量: 池化没起作用 (池子太小, 或包装三件自身的成本超过了
		// 下沉的面)。这是一个**真实可能发生**的结果, 不能粉饰成"持平"。
		return "池化没有减少工具段 —— 池子太小, 或包装三件自身的 schema 成本超过了下沉的面"
	}
	pct := int(float64(s.BaselineTokens-s.Tokens)/float64(s.BaselineTokens)*100 + 0.5)
	return "广告面 " + itoa(s.Advertised) + " 件 (池成员 " + itoa(s.Members) +
		" 件), 工具段约为全量的 " + itoa(100-pct) + "% (口径=字节/4, 用于比较不用作绝对计量)"
}

// ── 检索的分词 ────────────────────────────────────────────────────────────
//
// 检索原来按 `strings.Fields` 切词, 于是两种最常见的查询**必然**漏召:
//
//	· 中文查询: "视频生成" 是**一个**词 (中文不写空格), 而描述里没有这个子串 →
//	  0 命中。而模型是会用中文提问的。
//	· 标识符: 描述里写的是 "find_refs", 查询写的是 "find references" →
//	  两个词都对不上精确子串。
//
// Tokenize 修的就是这两条。它的输出是**候选匹配单元**, 打分侧按重叠计分。
//
// 召回偏宽的取舍 (题库 Q32): 这里刻意**宁滥勿缺** —— 多切几个 token 只会让排名
// 更松, 而精度由 pool_load 之后的执行前校验去兜。反过来 (切得保守) 的代价是
// 静默漏召, 而漏召无法被观测。
func Tokenize(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	var out []string
	for _, field := range strings.FieldsFunc(s, func(r rune) bool {
		return !isTokenRune(r)
	}) {
		out = append(out, splitFieldTokens(field)...)
	}
	return dedupeStrings(out)
}

// isTokenRune 保留字母/数字/CJK。下划线与连字符是**分隔符**(标识符要拆开),
// 所以不保留。
func isTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case isCJK(r):
		return true
	}
	return false
}

// isCJK 覆盖中日韩统一表意文字 (够用: 本项目的资产描述只有中英两种)。
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // 基本区
		(r >= 0x3400 && r <= 0x4DBF) // 扩展 A
}

// splitFieldTokens 把一个字段拆成检索 token。
//
//	· 含 CJK → 逐字 + 相邻双字 (bigram)。单字保留是因为"库"这类单词查询真实存在;
//	  bigram 是因为单字匹配太宽 ("数" 会命中一大片)。两者都给, 打分侧按长度加权。
//	· 纯 ASCII → 原样。**不做词干还原**: 英文形态学 (references/reference) 若要
//	  覆盖, 靠资产侧声明别名 (见 Pool.AddAlias), 而不是靠猜词根 —— 猜错了会把
//	  不相干的资产排到前面, 而那是误召, 也是静默的。
func splitFieldTokens(field string) []string {
	if field == "" {
		return nil
	}
	runes := []rune(field)
	hasCJK := false
	for _, r := range runes {
		if isCJK(r) {
			hasCJK = true
			break
		}
	}
	if !hasCJK {
		return []string{field}
	}
	out := make([]string, 0, len(runes)*2)
	for i, r := range runes {
		out = append(out, string(r))
		if i+1 < len(runes) {
			out = append(out, string(runes[i:i+2]))
		}
	}
	return out
}

// dedupeStrings 就地去重并丢弃空串, 保持首次出现的顺序; 入参为空时返回 nil。
//
// 注意：复用入参底层数组 (out := in[:0]) —— 调用方持有的原切片会被就地改写,
// 需要保留原切片时必须先自行拷贝。
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ── 检索本体: 两路召回 + RRF 融合 ──────────────────────────────────────────
//
// 单路子串打分的失败方式不是"排得不好", 是**排不出来**: `score > 0` 这个门槛
// 把"没逐字命中"的资产直接扔掉, 于是自然语言的问法得到的是空集。空集在模型
// 那里等于"这个能力不存在"。
//
// 这里换成两路互补的召回再融合:
//
//	· BM25 —— 按**稀有度**加权。查询里那个罕见的词 (「影响面」里的「影响」)
//	  命中时给的分高。它的问题是长描述吃亏 (长度归一化) 且常见词几乎不给分。
//	· 词面覆盖 —— 按**查询词被覆盖的比例**给分, 不看词有多常见。它的问题是把
//	  "覆盖了三个常见词"也算成好命中, 单看它会误召。
//
// 两者确实会打架 (BM25 偏向"有个稀有词命中", 覆盖偏向"常见词全中了"), 这正是
// 融合有意义的前提 —— 两个排序器永远一致的话, 融合就只是多算一遍。
//
// ── 题库 Q32 点名的 RRF, 这里要说清楚它在本项目里**不是**什么 ─────────────
//
// Q32 的原话是"工具名高度相似时 (get_slow_log vs get_error_log) 纯向量检索会
// 混淆, 需要 RRF 混合 BM25"。那句话里的第二路是**向量**。本文件这两路都是词面
// 的 —— 项目里的 embedder (pkg/memory/embed.go) 默认关着, 把一条网络依赖塞进
// 检索热路径不是这里该顺手做的决定。
//
// 所以要说清楚: **RRF 在这里解决的是"排不出来", 不是"跨语言/同义"。** 一个
// 中文查询打到一个全英文描述的工具上, 无论怎么融合都召不回 —— 那要靠描述本身
// 带中文 (见 pooltools.go 的可发现性说明) 或靠 embedder。这条界限在
// EmptyRecallNote 里如实讲给模型听, 也在保留集里单独记一档 (见 recall_test.go
// 的 crossLingual 集合): 把它折进总漏召率, 会让人以为检索"差一点", 而实际是
// **这一类查询根本没有词面通路**。

// Doc 是检索引擎看到的一份文档。Name 与 Text 分开: 名字命中比正文命中更说明
// 意图 (字段加权), 合成一个字符串就表达不了这件事。
type Doc struct {
	Name string
	Text string
	// Aliases 是**人为声明**的检索别名 ("who calls this function" 之于
	// code_intel_query)。它按名字的权重计入 —— 别名不是描述的一部分, 是有人
	// 特意写下来"这句话是在问这个"的, 可信度与名字同级。
	//
	// ── 为什么需要它, 以及它为什么不是权宜之计 ────────────────────────────
	//
	// 词面检索有一个**修不掉**的天花板: 用户的词只要不出现在描述里, 就永远召
	// 不回。同语言的词汇不匹配 ("找调用方" vs 描述里的 find_refs) 与跨语言
	// ("视频生成" vs GenerateVideo) 是同一个病。
	//
	// 这个天花板有一条**便宜且正确**的绕法: 让声明的人去说"这句话问的是我"。
	// 它比 embedder 好的地方在于它是**可审计的** —— 一条别名错了, 你能看见是
	// 谁写的、写的什么; 一个向量召回错了, 你只知道"距离近"。而它比"把描述改长"
	// 好的地方在于不动产品面 (描述是下发给模型的原文, 改它会影响每一次调用)。
	//
	// 所以: 别名是**部署侧**的声明 (RecallAliasProvider), 检索不会自己去猜同义词。
	// 猜词根/猜同义的那条路我刻意没走 —— 猜错了是静默的误召。
	Aliases []string
}

// RecallAliasProvider 是资产**可选**实现的**检索**别名声明。
//
// 做成独立可选接口 (与 D4.1 的 LimitCoverageProvider 同一手法): 如果塞进
// Tool 接口的必选方法, 那会让几十个实现方为了满足签名而返回一个空切片 ——
// 那是"没有别名"伪装成"声明过了, 就是没有"。
//
// ── 方法名为什么是 RecallAliases 而不是 Aliases ────────────────────────────
//
// pkg/tool/tool.go:87 已经有一个 AliasedTool, 语义是"这个工具还能被这些**名字**
// 调到" (Bash 报 "Shell"、TaskStop 报 "KillShell"), 消费者是 Registry.Get。
// 那是**派发**用的, 与检索无关。
//
// 两者一度共用 `Aliases() []string` 这个签名 —— 于是池装配处那句可选类型断言
// 会把每一个带注册别名的工具都当成带检索别名的工具, 把**名字**读成**检索词**。
// 两个方向都会坏:
//
//	· 注册别名漏进检索词表 → 一条谁都不会这么说的通路, 无害但是假的;
//	· 反方向才是真伤的: 为检索给工具写上中文说法, Registry.Get 会开始接受
//	  "视频生成" 当**工具名** —— 一次纯检索的改动改变了工具派发行为。
//
// 同签名不同语义的实现接口, 类型断言是分辨不出来的。所以这里把方法名分开:
// 名字本身就是要断言的那件事, 而一个改错的注入会立刻让
// TestPoolRecall_RegistryAliasIsNotARecallTerm 变红。
type RecallAliasProvider interface {
	RecallAliases() []string
}

// Ranked 是排好序的一条: 文档下标 + 原始得分。得分传出来是给融合用的 ——
// 融合需要知道**哪些条目是同分的** (见 FuseRRF 的并列处理)。
type Ranked struct {
	Idx   int
	Score float64
}

// BM25 参数。k1/b 取的是文献里的常规值, 没有针对本项目的语料调过 ——
// 它们只影响排序的相对高低, 而验收看的是召回**有没有**(MissRate), 不是 NDCG。
// 要调也该等有了真实查询日志再调, 而不是凭保留集过拟合。
const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// rrfK 是 RRF 的平滑常数。60 是原论文的取值; 它决定"排第一"相对"排第十"
	// 领先多少 —— k 越大, 各路排名的差别被压得越平 (更依赖两路都提名)。
	rrfK = 60.0
	// nameFieldBoost: 名字里的词按几倍计入词频。名字命中比正文命中更说明意图。
	nameFieldBoost = 3
)

// docTokens 把一份文档摊平成词袋 (名字与别名加权)。
func docTokens(d Doc) []string {
	head := Tokenize(d.Name)
	for _, a := range d.Aliases {
		head = append(head, Tokenize(a)...)
	}
	body := Tokenize(d.Text)
	out := make([]string, 0, len(head)*nameFieldBoost+len(body))
	for i := 0; i < nameFieldBoost; i++ {
		out = append(out, head...)
	}
	return append(out, body...)
}

// stopTokens 是查询侧要滤掉的**无分辨力**词。
//
// ── 为什么 BM25 那一路不需要这张表, 而覆盖率这一路必须要有 ──────────────────
//
// BM25 手里有 df: "this" 在 12 份文档里出现 9 次, IDF 就趋近 0, 它自己会被压住。
// 覆盖率那一路**故意**不看 df (那是它与 BM25 不重合的理由), 于是 "this" 命中
// 与 "candlestick" 命中得到同样的 +1/len(sig)。
//
// 实测的后果: 查询 "who calls this function" 命中了 GenerateImage 与 GenerateChart
// —— 两份描述里都有 "Use this for..."。正确的资产 (code_intel_query) 反而榜上
// 无名。这就是误召挤掉漏召的典型: 候选位被占满了, 而占位的理由是一个虚词。
//
// 表刻意短: 只收**在任何资产描述里都不承载意图**的英文虚词。收长了会开始
// 误伤 ("path"、"type" 这类词在有些查询里是关键词), 而误伤是静默的。
var stopTokens = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "can": true, "do": true, "does": true, "for": true,
	"from": true, "has": true, "have": true, "how": true, "i": true, "in": true,
	"is": true, "it": true, "its": true, "me": true, "my": true, "of": true,
	"on": true, "or": true, "that": true, "the": true, "their": true,
	"them": true, "then": true, "there": true, "these": true, "this": true,
	"to": true, "us": true, "use": true, "using": true, "was": true, "we": true,
	"what": true, "when": true, "where": true, "which": true, "who": true,
	"why": true, "will": true, "with": true, "you": true, "your": true,
}

// significantTokens 取查询里**有分辨力**的 token: 多字符的 ASCII 实词, 与
// CJK 双字。两路召回**都**用它的输出做查询侧 —— 见下面那条实测的教训。
//
// ── 单字 CJK 为什么必须滤掉 (这是实测撞出来的, 不是推出来的) ───────────────
//
// 一开始只在覆盖率那一路滤, BM25 那一路留着全部 token —— 当时的理由是"BM25 有
// IDF, 常见字自己会被压住"。**在真实的池子规模上这个理由是错的**:
//
//	查询 "把日志里的慢查询找出来", 池里 6 件。
//	BM25 因为单字 '的'/'里'/'出' 与两份**中文**资产 (evo_propose / evo_inspect)
//	重合, 给它们打了**大于零**的分 → 提名 → 它们挤掉了本该出现的位置,
//	而这个查询真正想找的东西一件都没进来。
//
// 根因是 `score > 0` 这个门槛与 IDF 的配合: IDF 把 '的' 的分压得很低, 但只要
// **不是零**, 文档就进候选。IDF 负责排序, 不负责**准入**。所以准入这一关要在
// 查询侧做, 而不能指望 IDF。
//
// ── 但"滤干净"与"滤到空"是两回事 ─────────────────────────────────────────
//
// 全滤掉会得到一个空集, 而空集在这条链路的尽头被读成"没有这个能力"。所以有
// 一条兜底: 若滤完什么都不剩 (例如查询就是单个 "库"), 就退回未滤的 token 集。
// 单词查询因此仍然可用 —— 它只是不再**顺便**把常见字的提名带进来。
func significantTokens(query string) []string {
	var out []string
	for _, t := range Tokenize(query) {
		if len([]rune(t)) < 2 {
			continue
		}
		if stopTokens[t] {
			continue
		}
		out = append(out, t)
	}
	out = dedupeStrings(out)
	if len(out) > 0 {
		return out
	}
	// 兜底: 滤到空 → 用未滤的。**这一支只在"查询本身没有实词"时才走到**。
	return Tokenize(query)
}

// queryTokens 是两路召回共用的查询侧 token 集 (见 significantTokens 的说明)。
func queryTokens(query string) []string { return significantTokens(query) }

// RankBM25 按 BM25 给 docs 排序, 返回**得分大于零**的那些 (降序)。
//
// 得分为零的文档不进排名: 它们与查询没有任何共同 token, 放进候选就是随机噪声。
// 覆盖率那一路同理。两路都不提名 = 这个查询在词面上确实没有通路 ——
// 那要如实报出去 (EmptyRecallNote), 不是靠塞噪声来假装召回了。
func RankBM25(query string, docs []Doc) []Ranked {
	qtoks := queryTokens(query)
	if len(qtoks) == 0 || len(docs) == 0 {
		return nil
	}
	tfs := make([]map[string]int, len(docs))
	lens := make([]int, len(docs))
	df := make(map[string]int)
	total := 0
	for i, d := range docs {
		tf := make(map[string]int)
		for _, t := range docTokens(d) {
			tf[t]++
		}
		tfs[i] = tf
		n := 0
		for _, c := range tf {
			n += c
		}
		lens[i] = n
		total += n
		for t := range tf {
			df[t]++
		}
	}
	avg := float64(total) / float64(len(docs))
	if avg <= 0 {
		avg = 1
	}
	N := float64(len(docs))
	out := make([]Ranked, 0, len(docs))
	for i := range docs {
		s := 0.0
		for _, t := range qtoks {
			f := float64(tfs[i][t])
			if f == 0 {
				continue
			}
			n := float64(df[t])
			// IDF 用 BM25 的 +0.5 平滑形式: 一个词在所有文档里都出现时 IDF
			// 仍为正 (趋 0), 不会变成负分把文档往下压。
			idf := math.Log(1 + (N-n+0.5)/(n+0.5))
			s += idf * (f * (bm25K1 + 1)) / (f + bm25K1*(1-bm25B+bm25B*float64(lens[i])/avg))
		}
		if s > 0 {
			out = append(out, Ranked{Idx: i, Score: s})
		}
	}
	sortRanked(out)
	return out
}

// RankCoverage 按"查询的有分辨力 token 被覆盖了几成"排序。
//
// 与 BM25 的差别是**不看词有多稀有**: 一个文档把 "find / references" 两个词都
// 覆盖了就该被提名, 哪怕这两个词在全池里到处都是 (BM25 会给它们接近 0 的 IDF,
// 于是那个文档的总分低到排不上去)。
func RankCoverage(query string, docs []Doc) []Ranked {
	sig := queryTokens(query)
	if len(sig) == 0 || len(docs) == 0 {
		return nil
	}
	out := make([]Ranked, 0, len(docs))
	for i, d := range docs {
		bag := map[string]bool{}
		for _, t := range docTokens(d) {
			bag[t] = true
		}
		hit := 0
		for _, t := range sig {
			if bag[t] {
				hit++
			}
		}
		if hit > 0 {
			out = append(out, Ranked{Idx: i, Score: float64(hit) / float64(len(sig))})
		}
	}
	sortRanked(out)
	return out
}

// sortRanked 降序 + 同分保持下标序。同分保序不是细节: 融合那一步要靠"同分"
// 判断并列, 排序不稳定会让并列关系在一次调用内部就自相矛盾。
func sortRanked(rs []Ranked) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Score > rs[j].Score })
}

// FuseRRF 用 Reciprocal Rank Fusion 融合多路排名。
//
// ── 并列为什么必须同名次 ──────────────────────────────────────────────────
//
// 教科书写法是"第 r 名加 1/(k+r)", 直接拿切片下标当 r。那是错的: 下标在**并列**
// 时也会每人差一名, 于是两个原始得分**完全相同**的文档仅仅因为登记顺序不同就
// 得到不同的融合分。乘以个性化 boost 之后, 那点差值会被放大成一次排序翻转 ——
// 而这与 boost 想要表达的东西毫无关系。
//
// 所以这里按**原始得分**算名次, 并列共享同一个名次 (competition ranking)。
// 好处是融合分对"同分"这个事实忠实: 同分就同分, 让后面的 boost 去决定先后。
//
// lists 里每一路都必须是已按 Score 降序排好的 (RankBM25/RankCoverage 的输出)。
func FuseRRF(lists [][]Ranked) []Ranked {
	type acc struct {
		idx   int
		score float64
	}
	byIdx := map[int]*acc{}
	var order []*acc
	for _, list := range lists {
		rank := 0
		for i, r := range list {
			// 与上一条不同分 → 名次跳到当前位置 (并列的名次相同)。
			if i > 0 && r.Score != list[i-1].Score {
				rank = i
			}
			a, ok := byIdx[r.Idx]
			if !ok {
				a = &acc{idx: r.Idx}
				byIdx[r.Idx] = a
				order = append(order, a)
			}
			a.score += 1.0 / (rrfK + float64(rank+1))
		}
	}
	out := make([]Ranked, 0, len(order))
	for _, a := range order {
		out = append(out, Ranked{Idx: a.idx, Score: a.score})
	}
	// 这里**不**用 sortRanked: 融合分相同的条目要保持"首次被提名的顺序"
	// (也就是各路排名靠前的先出现, 再退化到池登记序)。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// Rank 是检索实现的统一形状 (池内检索 / 未来的向量检索都实现它)。
//
// 度量 (EvaluateRecall) 只认这个接口, 不认池 —— 换了算法保留集与判据都不动。
type Ranker interface {
	// Rank 返回查询命中的资产名 (按相关性降序, 最多 limit 条; limit<=0 = 不限)。
	Rank(query string, limit int) []string
}

// EmptyRecallNote 在检索**一条都没召回**时给模型的那句话。
//
// ── 为什么这句话必须分成三种 ───────────────────────────────────────────────
//
// 原来的写法是"池内没有匹配资产"。当池子里**确实有**这个能力、只是问法对不上
// 时, 这句话是**假的**, 而模型会照它行事 (判定"这个能力不存在" → 放弃或硬凑)。
// 三种情形要分开说, 因为读者的下一步动作完全不同:
//
//	poolSize == 0 → 池是空的: 去查装配 (没有资产下沉到池里), 与问法无关
//	poolSize > 0  → 池里有 N 件, 但没有一件与查询有共同词: 换个说法再试,
//	                并说明检索是词面匹配 —— 这**不等于**池内没有这个能力
//
// fallback 为真时补一句池外工具本就可见 (13.7-P2 未命中回退), 这句与上面正交。
func EmptyRecallNote(query string, poolSize int, fallback bool) string {
	var sb strings.Builder
	if poolSize == 0 {
		sb.WriteString("池是空的 —— 这个部署没有把任何资产下沉到池里。")
		sb.WriteString("这不是检索失败 (换关键词也没用), 池外工具本就直接可见可调用。")
		if fallback {
			sb.WriteString(" (未命中回退: 本会话已自动保持全量广告, 基础工具不受池化影响。)")
		}
		return sb.String()
	}
	sb.WriteString("池内有 ")
	sb.WriteString(itoa(poolSize))
	sb.WriteString(" 件资产, 但没有一件与 ")
	sb.WriteString(strconv.Quote(strings.TrimSpace(query)))
	sb.WriteString(" 有共同的词。")
	// 这一句是整条消息的重点: 它把"召回为空"与"能力不存在"分开。
	sb.WriteString("检索走的是**词面**匹配 (不做同义、不做跨语言), 所以这**不等于**池内没有这个能力 —— ")
	sb.WriteString("换一个更贴近工具名或描述的说法再试 (工具描述多为英文; 中文查询若对不上, 试它的英文说法)。")
	sb.WriteString("池外工具 (Read/Bash/Grep 等) 本就直接可见可调用, 也可以直接用现有工具完成任务。")
	if fallback {
		sb.WriteString(" (未命中回退: 本会话已自动保持全量广告, 基础工具不受池化影响。)")
	}
	return sb.String()
}

// TopMissedQueries 取漏召最集中的前 n 条查询 (按 Want 去重)。
//
// 给页面用: 漏召率是一个数, 而"哪几条查询召不回"才是能动手改检索的输入。
func (r RecallReport) TopMissedQueries(n int) []string {
	if n <= 0 || len(r.Miss) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.Miss))
	seen := map[string]bool{}
	for _, m := range r.Miss {
		q := strings.TrimSpace(m.Case.Query)
		if q == "" || seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, q)
		if len(out) >= n {
			break
		}
	}
	sort.Strings(out)
	return out
}
