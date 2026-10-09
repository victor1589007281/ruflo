// poolrecall_test.go —— D4.2 验收口径在**生产路径**上跑一遍。
//
// ── 为什么这一条必须在这里, 而不是在 pkg/tool ──────────────────────────────
//
// pkg/tool/recall_test.go 测的是**管线**: 给了正确的 Doc 列表, 分词/融合/排序
// 是否给出正确的召回。它证明不了"池子把正确的 Doc 喂进去了" —— 名字、描述、
// 别名是不是真的从工具对象上取下来的, 是装配侧的事, 而装配错的时候管线那边
// 一片绿。
//
// 这正是本项目反复出现的那个坑的形状: 两侧各自正确, 接缝没有人看。所以这里
// 用一个**真的 poolIndex** (经 buildPoolIndex 装配) 跑同一批保留集。
//
// 两条断言的差别值得写下来:
//
//	· pkg/tool 那条绿 = 检索算法对
//	· 这条绿        = 池子确实在用那个算法, 且描述/别名真的接上了
//
// 少任何一条, 上线后的表现都可能是"查不到"。
package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// fakeToolWithAliases 是带检索别名声明的最小 Tool (实现 tool.RecallAliasProvider)。
//
// 它存在的意义是**测可选接口真的被认了**: 如果 buildPoolIndex 忘了做那个类型
// 断言, 别名会被静默丢掉, 而池子看起来一切正常 —— 只是那句中文查询再也召不回。
type fakeToolWithAliases struct {
	fakeToolForDesc
	aliases []string
}

func (f *fakeToolWithAliases) RecallAliases() []string { return f.aliases }

// poolRecallFixture 装一个**内容取自真实工具描述**的池。
//
// 描述与 pkg/tool/recall_test.go 的 realPool 同源 (都逐字抄自各工具的
// Description())。这里多出来的东西是**装配**: 名字从 reg 上取、描述从
// Description() 取、别名从 RecallAliases() 取。整个池子里没有任何一处手工拼检索键。
func poolRecallFixture(t *testing.T) *poolIndex {
	t.Helper()
	reg := tool.NewRegistry()
	descs := []struct {
		name, desc string
		aliases    []string
	}{
		{
			name: "code_intel_query",
			desc: "Query the code intelligence graph via GitNexus or Graphify CLI.\n" +
				"Query types:\n" +
				"- navigate / find_refs / cross_shard / impact → GitNexus structural queries\n" +
				"- path / explain / communities / god_nodes / surprises → Graphify semantic queries",
			aliases: []string{"who calls this function", "find callers", "调用方", "影响面"},
		},
		{
			name: "GenerateImage",
			desc: "Render SVG or HTML source code into a real PNG image file using headless Chrome. " +
				`Use this to "generate images": write SVG (preferred for drawings/diagrams) or HTML/CSS.`,
			aliases: []string{"图片生成"},
		},
		{
			name: "GenerateVideo",
			desc: "Generate a real video file (.mp4) by executing a Python script you write. " +
				"matplotlib, numpy and ffmpeg are available.",
			aliases: []string{"视频生成"},
		},
		{
			name: "GenerateChart",
			desc: "Render a data chart to PNG using Apache ECharts. " +
				`Provide "option": a valid ECharts option object (bar/line/pie/scatter/candlestick).`,
		},
		{
			name: "evo_propose",
			desc: "提交进化候选。kind=induce: 立即跑一次确定性工作流归纳 (AWM, 零 LLM 成本), " +
				"从高奖励的真实运行里归纳可复用阶段序列。",
		},
		{
			name: "evo_inspect",
			// ⚠️ 这里原来是 "查看单个进化环境的状态与历史。" —— 那是**编的**,
			// 生产代码里没有这段文字。现在逐字抄自 evotools.go:237。
			desc: "查看进化产物与学习闭环状态。不带参数=闭环健康度总览 (轨迹/奖励/经验/shadow 技能);带 id=看某份草案的谱系(父版本)、归纳依据(样本量/奖励均值/参与的 run)与当前状态。晋升任何东西之前应当先 inspect 它的证据。",
		},
	}
	pool := tool.NewPool()
	for _, d := range descs {
		if len(d.aliases) > 0 {
			reg.Register(&fakeToolWithAliases{fakeToolForDesc{name: d.name, desc: d.desc}, d.aliases})
		} else {
			reg.Register(&fakeToolForDesc{name: d.name, desc: d.desc})
		}
		pool.AddMember(d.name)
	}
	return buildPoolIndex(pool, reg, nil, nil)
}

// TestPoolRecall_ProductionPathMeetsTheHoldout 生产路径跑同一批保留集。
func TestPoolRecall_ProductionPathMeetsTheHoldout(t *testing.T) {
	idx := poolRecallFixture(t)

	// 与 pkg/tool 的 EnglishQueries 同口径: 词面有通路的查询必须零漏召。
	english := []tool.RecallCase{
		{Query: "generate video", Want: []string{"GenerateVideo"}},
		{Query: "animated chart", Want: []string{"GenerateChart"}},
		{Query: "find references", Want: []string{"code_intel_query"}},
		{Query: "svg to png", Want: []string{"GenerateImage"}},
		{Query: "candlestick", Want: []string{"GenerateChart"}, Note: "只出现在描述括号里的一个词"},
	}
	rep := tool.EvaluateRecall(func(q string) []string { return idx.Rank(q, 8) }, english)
	if rep.Total == 0 {
		t.Fatal("保留集是空的 —— 这个测试什么也没测")
	}
	if rep.MissRate() != 0 {
		t.Fatalf("生产路径上词面有通路的查询必须零漏召, 实际 %s\n%s",
			rep.MissSummary(), recallMissDetail(rep))
	}
}

// TestPoolRecall_AliasesSurviveAssembly 别名必须活着穿过装配。
//
// 这是本文件存在的**主要理由**: 如果 buildPoolIndex 少了那个 `t.(tool.RecallAliasProvider)`
// 类型断言, 管线那边全绿, 只有这里会红。
func TestPoolRecall_AliasesSurviveAssembly(t *testing.T) {
	idx := poolRecallFixture(t)
	// 先确认这些查询在**没有任何词面重合**的前提下被召回 —— 也就是它们只可能
	// 靠别名过线。断言写成"命中目标"而不是"命中非空", 否则误召也能让它绿。
	for _, c := range []tool.RecallCase{
		{Query: "who calls this function", Want: []string{"code_intel_query"}},
		{Query: "视频生成", Want: []string{"GenerateVideo"}},
		{Query: "影响面", Want: []string{"code_intel_query"}},
	} {
		rep := tool.EvaluateRecall(func(q string) []string { return idx.Rank(q, 8) },
			[]tool.RecallCase{c})
		if rep.Hit != 1 {
			t.Fatalf("别名没活着穿过装配: %q 召不回 %v\n实际: %v", c.Query, c.Want, idx.Rank(c.Query, 8))
		}
	}
}

// TestPoolRecall_EmptyResultMessageDoesNotLie 走真正的 Call 路径, 验那句话。
//
// 上面 pkg/tool 那条测的是 EmptyRecallNote 本身; 这条测的是 **pool_search 真的
// 在用那个函数**, 且传进去的池大小是**真实的池大小** (不是 0、也不是硬编码)。
// 传错分母的后果很具体: 报成"池是空的"会把人支使去查装配, 而池子其实是满的。
func TestPoolRecall_EmptyResultMessageDoesNotLie(t *testing.T) {
	idx := poolRecallFixture(t)
	st := &PoolSearchTool{idx: idx, pool: tool.NewPool(), policy: nil}

	res, err := st.Call(context.Background(), json.RawMessage(`{"query":"zzz-no-such"}`), &tool.ToolContext{})
	if err != nil {
		t.Fatalf("pool_search 不该返回 error: %v", err)
	}
	if res.IsError {
		t.Fatalf("检索落空不是错误: %+v", res)
	}
	// 池里有 6 件 —— 报出的分母必须是 6。
	if !strings.Contains(res.Content, "6 件") {
		t.Fatalf("未命中说明里要报出**真实**的池大小 (6 件), 报错分母会把人支使去查错的地方: %q", res.Content)
	}
	if strings.Contains(res.Content, "池是空的") {
		t.Fatalf("池里有 6 件时说「池是空的」是假话: %q", res.Content)
	}
	// 中文查询的**误召**: 这条比上一条值钱, 因为误召更隐蔽。
	//
	// 实测过一次: 查询 "把日志里的慢查询找出来" 这个池里没有任何东西能答,
	// 但它命中了 evo_propose / evo_inspect —— 只因为 BM25 在单字 '的'/'里'/'出'
	// 上给了非零分, 于是两款**中文**资产被提名, 挤掉了本该出现的位置。
	//
	// 模型拿到两个像模像样的候选会去用它们。这是"召回为空"的反面, 但同样错,
	// 且更难发现 —— 空结果至少看起来可疑。
	res, _ = st.Call(context.Background(), json.RawMessage(`{"query":"把日志里的慢查询找出来"}`), &tool.ToolContext{})
	if !res.IsError && strings.Contains(res.Content, "池内命中") {
		t.Fatalf("这个池里没有任何资产能回答这个查询 —— 不得靠虚词提名硬凑出结果 (误召比漏召更隐蔽, 因为候选看起来是像样的): %q",
			res.Content)
	}
	if !strings.Contains(res.Content, "6 件") {
		t.Fatalf("没召回时要说清池里有 6 件、只是词对不上: %q", res.Content)
	}
	if strings.Contains(res.Content, "池内没有匹配") || strings.Contains(res.Content, "池是空的") {
		t.Fatalf("两句都是假话 (池里有 6 件, 且召不回不等于没有这个能力): %q", res.Content)
	}
	// 单字查询的兜底: 滤到空时退回未滤 token。
	//
	// "图" 是一个字, significantTokens 会把它滤掉 (单字太宽), 于是走兜底那支
	// 退回 ["图"] —— 而 GenerateImage 的别名 "图片生成" 里有它, 所以应该召回到。
	// 去掉兜底的话这里会得到空结果, 于是一个**本来问得出来**的查询被报成"没有"。
	res, err = st.Call(context.Background(), json.RawMessage(`{"query":"图"}`), &tool.ToolContext{})
	if err != nil {
		t.Fatalf("单字查询不该崩: %v", err)
	}
	if !strings.Contains(res.Content, "GenerateImage") {
		t.Fatalf("单字查询要走兜底那支 (滤到空 → 退回未滤), 否则它被报成「池内没有」: %q", res.Content)
	}
}

// TestPoolRecall_SyntheticPoolStaysConstant-ish 广告面与检索规模解耦的旁证。
//
// 这条不测 token (那是 pkg/tool 的 SurfaceReport 测的), 它测**检索本身**在池子
// 变大时不会开始返回一堆噪声: 池子从 6 件涨到 600 件, 同一个查询的第一名必须
// 不变。变红说明 IDF/长度归一化里有一项随着语料规模漂了。
func TestPoolRecall_TopHitStableAsPoolGrows(t *testing.T) {
	idx := poolRecallFixture(t)
	before := idx.Rank("generate video", 8)
	if len(before) == 0 || before[0] != "GenerateVideo" {
		t.Fatalf("小池下第一名应为 GenerateVideo, got %v", before)
	}

	reg := tool.NewRegistry()
	for _, a := range idx.assets {
		reg.Register(&fakeToolForDesc{name: a.Name, desc: a.Desc})
	}
	pool := tool.NewPool()
	for _, a := range idx.assets {
		pool.AddMember(a.Name)
	}
	// 灌 600 件合成资产 (描述与主题无关, 模仿一个真实的大池子)。
	for i := 0; i < 600; i++ {
		name := "synth_" + itoaTest(i)
		reg.Register(&fakeToolForDesc{name: name, desc: "miscellaneous helper for assorted internal bookkeeping tasks"})
		pool.AddMember(name)
	}
	big := buildPoolIndex(pool, reg, nil, nil)
	after := big.Rank("generate video", 8)
	if len(after) == 0 || after[0] != "GenerateVideo" {
		t.Fatalf("池子涨到 %d 件后第一名变了: %v\n"+
			"这说明 IDF 或长度归一化随语料规模漂了 —— 大池子里检索会开始返回噪声", len(big.assets), after)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func recallMissDetail(rep tool.RecallReport) string {
	var sb strings.Builder
	for _, m := range rep.Miss {
		sb.WriteString("  · " + m.Case.Query + " → ")
		if len(m.Got) == 0 {
			sb.WriteString("(空)")
		} else {
			sb.WriteString(strings.Join(m.Got, ", "))
		}
		sb.WriteString("  [want " + strings.Join(m.Case.Want, "|") + "]\n")
	}
	return sb.String()
}

// 编译期确认这两个 fake 真的满足它们该满足的接口 —— 少一个方法都要在这里红,
// 而不是等到某次装配的运行时类型断言静默失败。
var (
	_ tool.Tool                = (*fakeToolWithAliases)(nil)
	_ tool.RecallAliasProvider = (*fakeToolWithAliases)(nil)
	_ tool.Ranker              = (*poolIndex)(nil)
)

// 未使用的 types 引用 (fake 签名需要) 由编译期断言兜住。
var _ = func() *types.PermissionResult { return nil }

// registryAliasTool 只实现 AliasedTool (工具**名**的别名), 不实现检索别名。
type registryAliasTool struct{ fakeToolForDesc }

func (r *registryAliasTool) Aliases() []string { return []string{"VideoGen"} }

// TestPoolRecall_RegistryAliasIsNotARecallTerm 工具**名**的别名不是检索词。
//
// ── 这条测试是发现了一个真缺陷之后补的 ─────────────────────────────────────
//
// tool.AliasedTool (pkg/tool/tool.go:87) 的语义是"这个工具还能被这些**名字**调到"
// (Bash 报 "Shell"、TaskStop 报 "KillShell"), 消费者是 Registry.Get。
//
// 而 tool.RecallAliasProvider 声明的是"这句话问的是我" —— 消费者是检索。
//
// 两者**方法签名曾经逐字相同** (`Aliases() []string`) —— 于是那个可选类型断言把
// 每一个带注册别名的工具都当成了带检索别名的工具, 把**名字**读成了**检索词**。
// 修法是把方法名分开 (RecallAliases); 这条测试是那个修的守卫。
//
// 两个方向都会坏, 且坏法不同:
//
//	· 眼下 (假别名): "VideoGen" 进了检索词表 —— 一条谁都不会这么说的通路,
//	  无害但也不是"没别名的工具就没有额外通路"。
//	· 反方向更糟: 若为了检索给某个工具写上中文说法, Registry.Get 会开始
//	  接受 "视频生成" 当成**工具名** —— 一次纯检索的改动改变了工具派发行为。
//
// 判据取的是**反方向** (注册别名不得进检索词表): 它同时钉住了两个方向 ——
// 一旦有人再把两个接口合并, 这条立刻红。
func TestPoolRecall_RegistryAliasIsNotARecallTerm(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(&registryAliasTool{fakeToolForDesc{
		name: "GenerateVideo",
		desc: "Generate a real video file (.mp4) by executing a Python script you write.",
	}})
	// 注册别名仍然必须生效 (那是它本来的语义, 没有被这次改动动过)。
	if _, ok := reg.Get("VideoGen"); !ok {
		t.Fatal("注册别名本身必须仍然可用 —— 修的是语义串台, 不是把注册别名废掉")
	}

	pool := tool.NewPool()
	pool.AddMember("GenerateVideo")
	idx := buildPoolIndex(pool, reg, nil, nil)
	if len(idx.assets) != 1 {
		t.Fatalf("池内应有 1 件资产, got %d", len(idx.assets))
	}
	for _, a := range idx.assets[0].Aliases {
		if a == "VideoGen" {
			t.Fatal("工具**名**的别名漏进了检索词表 —— 两个 Aliases() 语义串台了。 " +
				"注册别名是「这个工具还能被这些名字调到」, 检索别名是「这句话问的是我」, " +
				"同一个方法签名的可选断言会把前者读成后者")
		}
	}
}

// TestPoolRecall_RecallAliasIsNotAToolName 反方向: 检索别名不得变成可调用的工具名。
//
// 这条与上一条是同一枚硬币的两面, 合起来才把"两个接口不许合并"钉死。
// 只测一个方向的话, 有人把 RecallAliases 的实现**同时**接到 AliasedTool 上
// (让检索词也能派发) 仍然是绿的 —— 而那是真伤的那一半: 一次纯检索的改动
// 就这样改变了工具派发行为。
// ⚠️ 这条**必须**跑生产工具, 不能跑 fixture —— 第一版就是跑 fixture 的, 于是
// 注入「给生产工具同时加上 Aliases()」时**全绿**: 测的是测自己造的工具, 那个
// 工具当然没被改坏。又是「能力在 fixture 里」那个形状, 只不过方向反过来了。
func TestPoolRecall_RecallAliasIsNotAToolName(t *testing.T) {
	reg := tool.NewRegistry()
	pool := tool.NewPool()
	for _, tl := range realRecallTools() {
		reg.Register(tl)
		pool.AddMember(tl.Name())
	}

	// ① 反方向: 任何一个生产工具的检索别名, 都不能在注册表里查得到。
	leaked := 0
	for _, tl := range realRecallTools() {
		ap, ok := tl.(tool.RecallAliasProvider)
		if !ok {
			continue
		}
		for _, a := range ap.RecallAliases() {
			got, ok := reg.Get(a)
			if !ok {
				continue
			}
			t.Errorf("检索别名 %q 在注册表里能查到 %s —— 一次纯检索的声明改变了**工具派发**。"+
				"RecallAliases 只喂检索, 拼进 Registry 的别名表意味着模型可以「调用」一个中文短语",
				a, got.Name())
			leaked++
		}
		// 这个工具自己也不该实现 AliasedTool (那是"它还有别的名字"的另一回事)。
		if at, ok := tl.(tool.AliasedTool); ok {
			t.Errorf("%s 同时实现了 AliasedTool (%v) 与 RecallAliasProvider —— 两个接口重新黏在一起了",
				tl.Name(), at.Aliases())
			leaked++
		}
	}

	// ② 正方向: 同一批别名在**池检索**里必须通路 (否则①就成了"把功能关掉")。
	idx := buildPoolIndex(pool, reg, nil, nil)
	for _, c := range []struct{ query, want string }{
		{"视频生成", "GenerateVideo"},
		{"who calls this function", "code_intel_query"},
	} {
		hits := idx.Rank(c.query, 5)
		if len(hits) == 0 || hits[0] != c.want {
			t.Errorf("检索别名 %q 在池检索里没通路 (%v, 期望首位 %s)—— ①要求它不进注册表, "+
				"但没要求它被关掉", c.query, hits, c.want)
			leaked++
		}
	}
	if leaked > 0 {
		t.Fatalf("%d 处两个别名接口的语义串台", leaked)
	}
}

// realRecallTools 是**生产构造器**造出来的工具 (不是 fixture)。
//
// 这份清单就是"哪些工具声明了检索别名"的可审计来源: 谁想知道覆盖到哪了,
// 看这里, 而不是去 grep 实现文件。
func realRecallTools() []tool.Tool {
	return []tool.Tool{
		NewCodeIntelQueryTool(),
		NewGenerateImageTool(),
		NewGenerateVideoTool(),
		NewGenerateSpeechTool(),
		NewGenerateGIFTool(),
		NewGeneratePPTXTool(),
		NewGenerateChartTool(),
		NewEvoInspectTool(),
		NewEvoProposeTool(),
	}
}

// TestPoolRecall_ProductionToolsDeclareTheirQuestions 别名必须真的写在生产工具上。
//
// ── 这条测试挡的是什么 ──────────────────────────────────────────────────
//
// 上面那批 fixture 测试**全部可以在没有任何一个生产工具声明别名的情况下全绿** ——
// 因为别名是测里手工喂进去的。那种绿说明机制通的, 说明不了**它被用上了**。
//
// 这正是本项目反复栽的那个坑: 「能力在」与「接上了」是两件事, 而前者绿得很像后者。
//
// 这条把两者分开: 池子从生产构造器装配, 别名只能来自工具自己实现的
// RecallAliases()。任何一个工具把它删了, 这里立刻红。
func TestPoolRecall_ProductionToolsDeclareTheirQuestions(t *testing.T) {
	reg := tool.NewRegistry()
	pool := tool.NewPool()
	var missing []string
	for _, tl := range realRecallTools() {
		reg.Register(tl)
		pool.AddMember(tl.Name())
		if _, ok := tl.(tool.RecallAliasProvider); !ok {
			missing = append(missing, tl.Name())
		}
	}
	if len(missing) > 0 {
		t.Fatalf("这些工具没有声明检索别名: %v —— 它们的描述全是英文/schema 术语, "+
			"中文提问在词面上没有任何通路。别名是**可选**接口, 所以漏声明不会报错, "+
			"只会静默地查不到", missing)
	}
	idx := buildPoolIndex(pool, reg, nil, nil)

	// 每一条都要求**只**召回目标 (而不是"目标在里面就行") —— 见下面 影响面 那条:
	// 它命中的是错的那个, 而那种错比召回空更危险 (模型会拿它当答案用)。
	cases := []struct {
		query, want, why string
	}{
		{"视频生成", "GenerateVideo", "中文打英文描述 —— 纯词面结构上过不去"},
		{"图片生成", "GenerateImage", "同上"},
		{"做个PPT", "GeneratePPTX", "名字里是 PPTX, 描述里是 PowerPoint —— 两边不同形"},
		{"who calls this function", "code_intel_query", "描述里是 find_refs, 没有 who/calls/function"},
		{"find callers", "code_intel_query", "引擎术语与提问用语不同"},
		{"谁会受影响", "code_intel_query", "中文打英文描述"},
		{"数据可视化", "GenerateChart", "描述里是 visualizations (复数) 且只是顺带一提"},
		{"文字转语音", "GenerateSpeech", "描述里是 TTS/Synthesize"},
	}
	for _, c := range cases {
		hits := idx.Rank(c.query, 5)
		if len(hits) == 0 || hits[0] != c.want {
			t.Errorf("%q → %v, 期望首位是 %s (%s)", c.query, hits, c.want, c.why)
		}
	}
}

// TestPoolRecall_MisleadingAliasIsWorseThanNoAlias 别名不许把**别的**问题抢过来。
//
// ── 为什么这条单列, 而不是并进上面那条 ──────────────────────────────────
//
// "影响面" 是本项目里一个真实的坏例子: 它**看着像**跨语言 (中文打英文描述),
// 实际不是 —— "影响" 两个字逐字出现在 evo_propose 的英文... 不, 中文描述里:
// "产物一律落 proposed 态, **不**影响运行期行为"。
//
// 于是它有一条**假的通路**: 词面命中 evo_propose, 而问的人要的是 code_intel_query。
//
// 这比召回**空**更危险, 因为模型分不出"这一条是词碰巧撞上的"与"这一条是对
// 的" —— 空结果它至少知道自己没查到。所以验收口径必须区分这两种: 只看
// "有没有召回"的话, 这一档**看起来是达标的**。
func TestPoolRecall_MisleadingAliasIsWorseThanNoAlias(t *testing.T) {
	reg := tool.NewRegistry()
	pool := tool.NewPool()
	for _, tl := range realRecallTools() {
		reg.Register(tl)
		pool.AddMember(tl.Name())
	}
	hasEvoPropose := false
	for _, n := range pool.MemberNames() {
		if n == "evo_propose" {
			hasEvoPropose = true
		}
	}
	if !hasEvoPropose {
		t.Skip("池里没有 evo_propose, 这条错召的载体不在")
	}
	hits := buildPoolIndex(pool, reg, nil, nil).Rank("影响面", 5)
	if len(hits) > 0 && hits[0] != "code_intel_query" {
		t.Fatalf("「影响面」首位召回 %v —— 期望 code_intel_query。"+
			"若首位是 evo_propose, 那是词面撞上了它描述里的「不影响运行期行为」: "+
			"这种**非空但错**的召回比空结果更危险, 模型会拿它当答案用", hits)
	}
}

// TestHoldoutDescriptionsAreQuotedFromProduction 保留集里的描述必须真的来自生产代码。
//
// ── 这条测试是抓到一个**已经发生的漂移**之后补的 ────────────────────────
//
// pkg/tool/recall_test.go 的文件头写着"描述是**抄来的**, 不是编的", 并论证了
// 为什么这一点是硬要求: 编出来的描述测的是"我编的描述好不好搜", 而真正要测的
// 是"**项目里实际的描述**好不好搜"。
//
// 但那条承诺**当时没有任何东西守着** —— 它只是一段注释。实际结果是:
// evo_inspect 那一条里写着 "查看单个进化环境的状态与历史。", 而这句话**在全仓
// 任何生产代码里都不存在** (grep 只命中测试自己)。真描述是
// "查看进化产物与学习闭环状态。不带参数=闭环健康度总览…"。
//
// 假描述恰好**更短、更容易被检索到**, 于是 "查看进化环境" 那一条在假描述上是
// 过的 —— 一个靠假数据维持的绿。
//
// 注释守不住的东西得用测试守: 这条把保留集的 `Text:` 字面量直接抽出来, 逐条
// 要求它是某个**生产工具 Description()** 的子串。
//
// ⚠️ 有一个已知的、**故意保留**的例外: 描述允许被**截短**。文件头解释了为什么
// 截短是安全的方向 (词更少 → 命中比例更高 → 只会把漏召率报得偏低, 不会掩盖
// 漏召)。所以判据是"是生产的**前缀**", 不是"逐字相等"。
func TestHoldoutDescriptionsAreQuotedFromProduction(t *testing.T) {
	// 读保留集源码 (同包外的文件, 走相对路径: 测试的 cwd 是本包目录)。
	src, err := os.ReadFile(filepath.Join("..", "recall_test.go"))
	if err != nil {
		t.Fatalf("读不到保留集源码: %v", err)
	}

	// 生产里所有工具的 Description() —— 这是"抄"的来源。
	prod := map[string]string{}
	for _, tl := range realRecallTools() {
		prod[tl.Name()] = tl.Description()
	}
	// 保留集里还收了几个不在 realRecallTools 里的 (如 GenerateGIF/GenerateSpeech 的
	// 对偶), 一并收进来, 否则它们会被误报成漂移。
	for _, tl := range []tool.Tool{NewGenerateGIFTool(), NewGenerateSpeechTool()} {
		prod[tl.Name()] = tl.Description()
	}
	if len(prod) == 0 {
		t.Fatal("没有采到任何生产描述 —— 这条测试的前提不成立")
	}

	// 抽 `{Name: "X", Text: `...`}` 的字面量。
	re := regexp.MustCompile("(?s)\\{Name:\\s*\"([A-Za-z_0-9]+)\",\\s*(?://[^\\n]*\\n\\s*)*Text:\\s*`([^`]*)`")
	ms := re.FindAllStringSubmatch(string(src), -1)
	if len(ms) < 5 {
		t.Fatalf("只抽到 %d 条保留集描述 —— 抽取正则与源码形态对不上了, 这条测试已失效", len(ms))
	}
	bad := 0
	for _, m := range ms {
		name, text := m[1], m[2]
		d, ok := prod[name]
		if !ok {
			t.Errorf("保留集里的 %s 不是生产工具 (构造器清单里没有它) —— 得先把它加进 realRecallTools", name)
			bad++
			continue
		}
		if !isQuotedFrom(text, d) {
			t.Errorf("保留集里 %s 的描述**在生产代码里找不到**:\n  保留集: %q\n  生产:   %q\n"+
				"描述是编的, 那么这一档测的是「我编的描述好不好搜」, 不是「项目里实际的描述好不好搜」",
				name, truncateRunes(foldSpace(text), 80), truncateRunes(foldSpace(d), 80))
			bad++
		}
	}
	if bad > 0 {
		t.Fatalf("%d 条保留集描述与生产漂移", bad)
	}
}

// isQuotedFrom 判断保留集里的描述是不是**从生产描述上抄下来的** (允许截短)。
//
// ── 为什么不是简单的 strings.Contains ─────────────────────────────────────
//
// 文件头承诺"允许截短", 而截短在源码里的写法是省略号: 保留集里写着
//
//	"Generate a PowerPoint .pptx file. Two modes: ..."
//
// 省略号**后面还有原文没抄**, 所以它不是生产描述的子串 —— 用 Contains 判会把
// 这个**有意的**截短报成"编的", 而那正是这条测试唯一不该报的东西。
//
// 所以判据写成: 按省略号切开, **每一段**都要出现在生产描述里, 且**顺序一致**。
// 这样:
//
//	· 截短 → 每段都在, 顺序对 → 过 (意图如此)
//	· 编造 → 那一段找不到 → 红 (这才是要抓的)
//	· 改词序 → 后一段的起点跑到前一段前面 → 红
func isQuotedFrom(holdout, prod string) bool {
	p := foldSpace(prod)
	cursor := 0
	for _, seg := range strings.FieldsFunc(foldSpace(holdout), func(r rune) bool {
		return r == '…' // 省略号按**单字符**切: strings.FieldsFunc 吃的是 rune
	}) {
		// "..." 三个点不是单个 rune, 单独处理。
		for _, s2 := range strings.Split(seg, "...") {
			s2 = strings.TrimSpace(s2)
			if s2 == "" {
				continue
			}
			at := strings.Index(p[cursor:], s2)
			if at < 0 {
				return false
			}
			cursor += at + len(s2)
		}
	}
	return true
}

// foldSpace 折叠空白 (保留集里描述常被写成一行, 生产里是多行拼接)。
func foldSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
