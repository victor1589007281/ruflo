package memory

// index_format_test.go —— 索引形态的呈现。
//
// 这套测试守的是**推送改拉取**这条路能不能走通: 索引要在模型眼里"够认出相关性",
// 又要在账本上"真的比正文小一个数量级"。前者坏了模型会取错条 (而正文仍然进得来,
// 现象是变笨不是报错); 后者坏了等于白做 —— 索引和正文一样大就没人受益。
//
// 还有一条隐性的: 索引里的 id 必须能与 memory_read 对上。id 印错一位, 工具就查不到,
// 而这条路径上没有任何东西会报错。

import (
	"strings"
	"testing"
	"time"
)

func idxEntry(id, content string, topics ...string) *MemoryEntry {
	return &MemoryEntry{
		ID:        id,
		Content:   content,
		Topics:    topics,
		Source:    "team_result",
		CreatedAt: time.Date(2026, 9, 18, 10, 15, 30, 0, time.UTC),
	}
}

// 空结果返回空串 —— 调用方据此决定不注入这个块。返回 "<memory_index></memory_index>"
// 也能"看起来对", 但会让每轮请求多一个空块, 且它与"没有记忆"无法区分。
func Test索引_空结果返回空串(t *testing.T) {
	if got := FormatIndexForPrompt(nil); got != "" {
		t.Fatalf("无命中应当返回空串, 得到 %q", got)
	}
	if got := FormatIndexForPrompt([]*MemoryEntry{}); got != "" {
		t.Fatalf("空切片应当返回空串, 得到 %q", got)
	}
}

// id 必须原样出现在索引里 —— 它是模型调 memory_read 的唯一句柄。
func Test索引_打印可用于取回的id(t *testing.T) {
	es := []*MemoryEntry{idxEntry("20260918-101530-0001", "团队完成了 JD 分析", "面试")}
	got := FormatIndexForPrompt(es)

	if !strings.Contains(got, "id=20260918-101530-0001") {
		t.Fatalf("索引里没有可原样复制的 id:\n%s", got)
	}
	if !strings.HasPrefix(got, "<memory_index>") || !strings.Contains(got, "</memory_index>") {
		t.Fatalf("索引块缺少边界标签:\n%s", got)
	}
}

// 有未截断快照的标注 full=yes, 没有的不标 —— 模型据此知道哪条**值得**花一次读盘。
func Test索引_标注哪条有全文(t *testing.T) {
	withFull := idxEntry("a-1", "有原文的摘要")
	withFull.Ref = "0123456789abcdef0123456789abcdef"
	noFull := idxEntry("b-2", "正文即全部")

	got := FormatIndexForPrompt([]*MemoryEntry{withFull, noFull})

	lineWith := lineOf(t, got, "a-1")
	lineWithout := lineOf(t, got, "b-2")
	if !strings.Contains(lineWith, "full=yes") {
		t.Fatalf("有 Ref 的那条应当标注 full=yes: %q", lineWith)
	}
	if strings.Contains(lineWithout, "full=yes") {
		t.Fatalf("没有 Ref 的那条不该标注 full=yes: %q", lineWithout)
	}
}

// 索引按条占**一行**。正文里的换行会把"一条"读成好几条 —— 模型数出来的条数
// 与真实命中数对不上, 后面所有关于"还有几条没看"的判断都跟着错。
func Test索引_多条正文压成一行(t *testing.T) {
	e := idxEntry("c-3", "第一行\n第二行\r\n第三行")
	got := FormatIndexForPrompt([]*MemoryEntry{e})

	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasPrefix(l, "- id=") {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("一条记忆应当只占一行, 实际占 %d 行:\n%s", lines, got)
	}
	if strings.Contains(got, "第二行") && !strings.Contains(got, "第一行 第二行") {
		t.Fatalf("换行没有被折叠成空格:\n%s", got)
	}
}

// 预览要有上限: 没有上限的索引在记忆变多时会长成第二份正文, 收益归零。
func Test索引_预览被截断(t *testing.T) {
	long := strings.Repeat("很长的一段记忆内容", 100)
	got := FormatIndexForPrompt([]*MemoryEntry{idxEntry("d-4", long)})

	line := lineOf(t, got, "d-4")
	if n := len([]rune(line)); n > IndexPreviewRunes+120 {
		t.Fatalf("单行索引 %d 字符, 远超预览上限 %d:\n%s", n, IndexPreviewRunes, line)
	}
	if !strings.Contains(line, "…") {
		t.Fatalf("超长预览应当有截断标记:\n%s", line)
	}
}

// 索引必须比正文小得多 —— 这是改动的全部理由。不量化的话, 哪天它悄悄涨回去
// (比如预览上限被调大、或者每条多印了几行元信息) 没有任何东西会发现。
//
// 注意这个倍率**不是索引的固有属性, 它随正文长度走**: 每条索引的固定开销
// (id + 日期 + 主题 ≈ 60 字符) 摊在 120 字符的预览上, 正文越短摊得越不划算。
// 所以这里按**生产实际长度**取样 (团队产出入库前 Content 上限 3000、注入前
// 再按 4096 截), 而不是随便编一个长度 —— 长度选错会把结论取反。
func Test索引_显著小于正文(t *testing.T) {
	var full []*MemoryEntry
	for i := 0; i < 7; i++ {
		full = append(full, idxEntry(
			"e-"+string(rune('a'+i)),
			strings.Repeat("团队产出的一段相当长的正文。", 250), // ≈3000 字符, 生产常态
			"team_result", "jd",
		))
	}

	indexLen := len(FormatIndexForPrompt(full))
	bodyLen := len(FormatForPrompt(full))
	if bodyLen == 0 {
		t.Fatal("正文形态不该为空 (对照组失效)")
	}
	if indexLen*5 > bodyLen {
		t.Fatalf("索引 %d 字符 vs 正文 %d 字符: 压缩不到 5 倍, 换来的收益撑不起这次改动",
			indexLen, bodyLen)
	}
}

// 反之, 短记忆上索引**不比正文省**, 甚至略大 —— 这不是缺陷, 是这条路的适用边界,
// 得在盘上写死: 它保证了改动的下界是"不亏太多", 而不是"记忆一短就翻车"。
//
// 真要收紧的话该动的是 IndexPreviewRunes 或每条元信息, 而不是到这里改断言 ——
// 断言写松了, 上面那条 5 倍线就形同虚设。
func Test索引_短记忆上不比正文大太多(t *testing.T) {
	var short []*MemoryEntry
	for i := 0; i < 7; i++ {
		short = append(short, idxEntry("s-"+string(rune('a'+i)), "很短的一条记忆。", "t"))
	}

	indexLen := len(FormatIndexForPrompt(short))
	bodyLen := len(FormatForPrompt(short))
	if indexLen > bodyLen*2 {
		t.Fatalf("短记忆上索引 %d 字符 / 正文 %d 字符: 开销失控, 元信息印得太多了",
			indexLen, bodyLen)
	}
}

// 两种形态必须能用同一批检索结果互换 —— 调用方 (hook) 只是选一个函数,
// 不重新检索。这条守着"切换形态不改变检索本身", 遥测的命中数才能跨模式比较。
func Test索引_与正文形态命中条数一致(t *testing.T) {
	es := []*MemoryEntry{idxEntry("f-1", "甲"), idxEntry("f-2", "乙"), idxEntry("f-3", "丙")}

	if n := strings.Count(FormatIndexForPrompt(es), "- id="); n != len(es) {
		t.Fatalf("索引印出 %d 条, 检索结果有 %d 条", n, len(es))
	}
	if n := strings.Count(FormatForPrompt(es), "- "); n != len(es) {
		t.Fatalf("正文印出 %d 条, 检索结果有 %d 条", n, len(es))
	}
}

// lineOf 取索引里含 id 的那一整行。
func lineOf(t *testing.T, s, id string) string {
	t.Helper()
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "id="+id) {
			return l
		}
	}
	t.Fatalf("索引里找不到 id=%s:\n%s", id, s)
	return ""
}
