// recall_test.go —— D4.2「漏召率趋零」的判据。
//
// ── 保留集的描述是**抄来的**, 不是编的 ─────────────────────────────────────
//
// 下面 realPool 里每一条 description 都是从对应工具的 Description() 里逐字摘的
// (名字与出处见各条注释)。这一点是硬要求: 如果描述是编的, 那么这个保留集测的是
// "我编的描述好不好搜", 而真正要测的是"**项目里实际的描述**好不好搜" —— 后者才
// 是可发现性问题, 前者只是自说自话。
//
// 摘要会失真: 有些描述被截短了 (原描述更长)。但截短的方向是**让检索更容易**
// (词更少 → 命中比例更高), 所以它不会把漏召掩盖过去, 只会把漏召率报得偏低。
//
// ── 这个保留集守的是"词面检索的天花板", 不是"检索实现有多好" ──────────────
//
// 见 crossLingual 那一档的说明: 有些查询在本项目的检索形态下**本来就召不回**,
// 如实记档, 而不是把它们折进总漏召率里凑一个好看的数。
package tool

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// ── 保留集 ────────────────────────────────────────────────────────────────

// realPool 是从**真实工具描述**摘下来的文档集 (见文件头)。
//
// 只收了会下沉到池里的那类工具 (媒体生成 / 代码智能 / 进化), 基础工具
// (Read/Bash/Grep) 按设计不进池 —— 它们永远在广告面上。
func realPool() []Doc {
	return []Doc{
		// pkg/tool/builtin/codeintel_tools.go:194
		//
		// 别名是这一档的**关键**: 描述里没有任何一个词与 "who calls this function"
		// 重合。别名本身就是"部署方声明这句话问的是我" —— 见 recall.go 里
		// Doc.Aliases 那段 (为什么这是可审计的, 以及为什么不去猜同义词)。
		{Name: "code_intel_query", Aliases: []string{
			"who calls this function", "find callers", "call hierarchy",
			"调用方", "谁调用", "影响面", "引用关系",
		}, Text: `Query the code intelligence graph via GitNexus or Graphify CLI.
Query types:
- navigate / find_refs / cross_shard / impact → GitNexus structural queries
- path / explain / communities / god_nodes / surprises → Graphify semantic queries
- query (generic) → routed to both engines`},
		// pkg/tool/builtin/media_gen.go:70
		{Name: "GenerateImage", Aliases: []string{"图片生成", "生成图片", "画图"}, Text: `Render SVG or HTML source code into a real PNG image file using headless Chrome. Use this to "generate images": write SVG (preferred for drawings/diagrams) or HTML/CSS, then call this tool to produce the actual image file. Provide exactly one of "svg" or "html", plus "output_path" (.png).`},
		// pkg/tool/builtin/media_gen.go:158
		{Name: "GenerateVideo", Aliases: []string{"视频生成", "生成视频", "做视频"}, Text: `Generate a real video file (.mp4) by executing a Python script you write. Write python code (matplotlib.animation with FFMpegWriter, or render frames and call ffmpeg via subprocess) that saves the video to the path given in environment variable OUTPUT_PATH (also sys.argv[1]). matplotlib, numpy and ffmpeg are available.`},
		// pkg/tool/builtin/media_gen.go:275
		{Name: "GenerateSpeech", Text: `Synthesize real speech audio (.wav) from text. Uses piper neural TTS for English (natural voice) and espeak-ng for Chinese/other languages. Provide "text" and "output_path" (.wav).`},
		// pkg/tool/builtin/media_gen.go:388
		{Name: "GenerateGIF", Text: `Render an animated GIF from an HTML document that contains CSS @keyframes animations. Write HTML/CSS where the motion is expressed via CSS animations (the renderer deterministically seeks each frame, so playback speed is exact). Provide "html" and "output_path" (.gif).`},
		// pkg/tool/builtin/media_gen.go:466
		{Name: "GeneratePPTX", Text: `Generate a PowerPoint .pptx file. Two modes: ...`},
		// pkg/tool/builtin/media_gen.go:558
		{Name: "GenerateChart", Text: `Render a data chart to PNG using Apache ECharts. Provide "option": a valid ECharts option object (bar/line/pie/scatter/candlestick/radar/sankey/map/...), plus "output_path" (.png). Use this for data visualizations; for flowcharts/diagrams prefer mermaid.`},
		// pkg/tool/builtin/evotools.go:300
		{Name: "evo_propose", Text: `提交进化候选。kind=induce: 立即跑一次确定性工作流归纳 (AWM, 零 LLM 成本), 从高奖励的真实运行里归纳可复用阶段序列; kind=prompt: 提交一份 prompt 候选正文。产物一律落 proposed 态, 不影响运行期行为, 必须经 evo_smoke + evo_promote 才生效。`},
		// pkg/tool/builtin/evotools.go:237 (对偶资产: 与 evo_propose 同前缀, 是误召的试金石)
		//
		// pkg/tool/builtin/evotools.go:237 —— ⚠️ 这条**原来是编的**: 之前这里写的是
		// "查看单个进化环境的状态与历史。", 那段文字在生产代码里**根本不存在**
		// (grep 全仓只命中本测试自己)。它恰好比真描述短得多、也更容易被检索到,
		// 于是保留集里 "查看进化环境" 那一条**在假描述上是通过的**。
		// 现在逐字换成真描述 —— 见那个测试里的说明。
		{Name: "evo_inspect", Text: `查看进化产物与学习闭环状态。不带参数=闭环健康度总览 (轨迹/奖励/经验/shadow 技能);带 id=看某份草案的谱系(父版本)、归纳依据(样本量/奖励均值/参与的 run)与当前状态。晋升任何东西之前应当先 inspect 它的证据。`},
	}
}

// EnglishQueries 是**词面有通路**的那一档: 查询里的词确实出现在某条描述里。
//
// 这一档的验收是硬性的 —— 漏召率必须是 0。任何一条漏了, 都是实现缺陷。
func EnglishQueries() []RecallCase {
	return []RecallCase{
		{Query: "generate video", Want: []string{"GenerateVideo"}, Note: "词序与描述不同 ('Generate a real video file') —— 子串检索对这个查询是碰运气"},
		{Query: "make an mp4", Want: []string{"GenerateVideo"}, Note: "mp4 只出现在描述的括号里"},
		{Query: "text to speech", Want: []string{"GenerateSpeech"}, Note: "描述里没有 'text to speech' 这个短语, 只有 speech/text 两个词"},
		{Query: "TTS", Want: []string{"GenerateSpeech"}, Note: "缩写不在描述里 —— 这条**允许**漏 (见 crossLingual 的立场), 但它在英文档里, 记下来"},
		{Query: "chart", Want: []string{"GenerateChart"}, Note: "单词查询"},
		{Query: "data visualization", Want: []string{"GenerateChart"}, Note: "'visualizations' 是复数, 查询是单数"},
		{Query: "find references", Want: []string{"code_intel_query"}, Note: "描述里是 find_refs (下划线); 查询写的是两个词"},
		{Query: "impact analysis", Want: []string{"code_intel_query"}, Note: "描述里只有裸词 'impact'"},
		{Query: "powerpoint", Want: []string{"GeneratePPTX"}, Note: "名字里是 PPTX, 描述里是 PowerPoint —— 两边不同形"},
		{Query: "animated gif", Want: []string{"GenerateGIF"}, Note: "描述里有 animated 与 GIF (大小写不同)"},
		{Query: "svg to png", Want: []string{"GenerateImage"}, Note: "svg/png 都在, 但中间隔着 'to'"},
	}
}

// ChineseQueries 是中文查询那一档。**保留集里必须有这一档**: 模型会用中文提问,
// 而这正是子串检索最先崩的地方 (中文不写空格 → strings.Fields 切不出词)。
//
// 期望落地在哪一边取决于描述的语言:
//   - 描述本身是中文的 (evo_*) → 必须召回
//   - 描述是英文的 (Generate*) → 见 crossLingual
func ChineseQueries() []RecallCase {
	return []RecallCase{
		{Query: "视频生成", Want: []string{"GenerateVideo"}, Note: "★ 原来是 0 命中: 中文单字段被切成一个词, 描述里没有这个子串。归到 crossLingual 还是这里, 取决于我们认不认为它该被召回 (见 crossLingual 的立场)"},
		{Query: "图片生成", Want: []string{"GenerateImage"}, Note: "同上"},
		{Query: "影响面", Want: []string{"code_intel_query"}, Note: "★ 原来是 0 命中: '影响面' 在英文描述里根本没有对应的字"},
		// 下面这两条是同语言的, 但**失效的方式不同**, 值得并排看:
		{Query: "进化候选", Want: []string{"evo_propose"},
			Note: "描述是中文的 ('提交进化候选') 且**逐字包含**查询 —— 子串检索也能过。留着它是为了证明「改进了」不等于「这条也改进了」 (它本来就绿)"},
		{Query: "查看进化环境", Want: []string{"evo_inspect"},
			Note: "★ 这条才是子串检索的典型失效: 描述是 '查看**单个**进化环境的状态与历史', 每个字都在, 却不连续 —— 子串匹配要的是**连续**, 而自然语言不保证连续。旧实现在这条上返回**空**"},
	}
}

// aliasOnlyQueries 是**只有别名才能召回**的那一档。
//
// ── 为什么它必须自成一档, 不能并进 EnglishQueries ────────────────────────
//
// 这一档里, 查询的**每一个**词都不在目标描述里:
//
//	"who calls this function" —— code_intel_query 的描述里没有 who/calls/this/function
//	"视频生成"                 —— GenerateVideo 的描述里一个中文字都没有
//
// 它们与 EnglishQueries 那一档的差别是本质的: 那一档的漏召是**实现没做好**
// (分词、融合、排序), 这一档的漏召是**词面上没有通路**, 再好的排序器也变不出来。
//
// 把它们并在一起, 得到的那个漏召率会同时被"能修的"与"修不了的"推高, 于是它
// 既不能指导动作 (去修哪一块? ), 也掩盖了真正的进展 (修好了分词, 这个数几乎不动)。
//
// 这一档的判据是: **声明了别名就必须召回**。它测的不是排序质量, 是"声明那条
// 通路有没有接上" —— 别名声明了却召不回, 与没声明是一回事, 而后者看起来像正常。
func aliasOnlyQueries() []RecallCase {
	return []RecallCase{
		{Query: "who calls this function", Want: []string{"code_intel_query"}, Note: "描述里没有 who/calls/function 任何一个词 —— 靠别名声明"},
		{Query: "视频生成", Want: []string{"GenerateVideo"}, Note: "描述全是英文, 中文查询词面无通路 —— 靠别名声明"},
		{Query: "图片生成", Want: []string{"GenerateImage"}, Note: "同上"},
	}
}

// crossLingualQueries 是**中文查询打英文描述**这一档。
//
// ── 为什么单列而不并进总漏召率 ────────────────────────────────────────────
//
// 这一档在当前的检索形态下**本来就召不回**: 中文的"视频"与英文的 "video" 没有
// 任何共同的字符, 词面检索无论怎么融合都跨不过去。把它折进总漏召率, 得到的
// 那个数会同时被两件事推高 —— 一件是能修的 (分词/融合), 一件是不能修的
// (需要 embedder)。一个混起来的数不能指导任何动作。
//
// 所以: 这一档如实记账, 它的用途是标出"词面检索的天花板在哪", 而不是当 KPI。
// 真要把这一档做上去, 唯一的路是 pkg/memory/embed.go 那条 embedder 通路 ——
// 那是另一个决定 (给检索热路径引入一个网络依赖), 不在这里顺手做。
//
// 这一档必须**双向都对**才算召回 —— 见下面 TestRecall_CrossLingualIsADocumentedCeiling
// 里关于"有意为之的漏召"那段: 只要关联是单向的, 从一个方向进去看到的候选里
// 没有另一个方向的东西, 那一侧的模型就永远发现不了它。
//
// ⚠️ "影响面" **不**在这一档 —— 它看着像跨语言, 实际不是: 它的两个字 "影响"
// 逐字出现在 evo_propose 的描述里 ("产物一律落 proposed 态, **不**影响运行期
// 行为")。它有自己的档, 见下面 misleadingQueries。
func crossLingualQueries() []RecallCase {
	return []RecallCase{
		{Query: "视频生成", Want: []string{"GenerateVideo"}, Note: "中文 → 英文描述, 词面无通路"},
		{Query: "图片生成", Want: []string{"GenerateImage"}, Note: "同上"},
		{Query: "视频剪辑拼接", Want: []string{"GenerateVideo"}, Note: "★ 真实的**多跳**查询: 用户想的是'把几段拼起来', 而工具其实是'用 Python 脚本生成' —— 没有任何一个词重合, 上一跳的候选也引不到它"},
	}
}

// misleadingQueries 是**词面误导**档: 查询的词确实出现了, 但出现在一个与意图
// 无关的资产里 —— 于是召回"成功"了, 召回的是错的东西。
//
// ── 为什么它必须自成一档 (这是被实测逼出来的) ────────────────────────────
//
// "影响面" 问的是代码影响面 (impact / blast radius), 描述在 code_intel_query 里
// (全英文)。而池里有一份**中文**描述: evo_propose 的 "产物一律落 proposed 态,
// 不影响运行期行为"。两个字 "影响" 一撞, BM25 就把 evo_propose 提上来了 ——
// 而 code_intel_query 一个词都对不上, 排不上名。
//
// 这一类与跨语言档的差别是本质的:
//
//	· 跨语言 (视频生成) → 结果**空**, 模型知道没找到
//	· 词面误导 (影响面) → 结果**非空且连贯**, 模型会去用那个错的东西
//
// 后者要危险得多, 而它**不是任何一个检索算法的缺陷**: "影响" 确实是个词, 确实
// 出现在那段描述里, 语料里也确实稀有 (IDF 不会压制它)。词面检索在这里没有任何
// 可用的信号去分辨"影响面"与"不影响"。
//
// 唯一的答案是**人为声明** (code_intel_query 的别名里有"影响面") —— 于是这一档
// 同时是别名机制的第二条理由: 它不只补跨语言, 也补同语言同词异义。
func misleadingQueries() []RecallCase {
	return []RecallCase{
		{Query: "影响面", Want: []string{"code_intel_query"},
			Note: "★ '影响' 与 evo_propose 描述里的 '不影响运行期行为' 撞车 —— 裸池会召回到 evo_propose (错的那个), 而且它看起来完全合理"},
	}
}

// 注意这一档**刻意不收**"池里根本没有的能力"那类查询 (例如"把服务重启一下")。
//
// 那种查询返回空是**正确行为**, 不是天花板 —— 把它算进漏召率, 等于把"我们没做
// 这个能力"记成"检索不行", 然后拿它去驱动检索优化。两件不同的事混进同一个
// 分子, 得到的数就不能指导任何动作 (这条和 total==0 返回 -1 是同一条纪律:
// 数字必须指得出下一步动作)。

// unanticipatedQueries 是**故意不按别名写法说**的那一档。
//
// ── 这一档存在的理由, 以及它为什么不能省 ────────────────────────────────
//
// 上面 ③④⑤ 三档在带别名的池上都是 0% 漏召。但那个 0% 有一个**必须说出来的**
// 前提: 别名与保留集是**同一个人在同一轮里写的**。写别名时脑子里想的正是那几条
// 查询的说法, 于是"0% 漏召"里有一部分是"我把答案抄进了别名表"。
//
// 这不是作弊 —— 别名机制的设计意图就是"有人预见到这种说法就声明一条"。但把它
// 当成"检索能听懂任意说法"就错了, 且是**方向错误**的错: 它会让人以为可以不用
// 继续加别名。
//
// 所以这一档单列: 每一句都是同一个意图的**另一种说法**, 且**刻意不与任何
// 已声明的别名逐字重合**。它量的是"别名机制**没有**覆盖到的部分有多大",
// 也就是这个机制的天花板在哪。
//
// 它的用途**不是**当 KPI: 这一档不可能靠"再加几条别名"降到 0 (加完了它就不再
// 是"未预见"了)。它的用途是当**放大镜**: 把别名的缺口摊开给人看, 让"要不要
// 再声明一条"变成一个有据可依的决定。
//
// 判据是**记数, 不设阈值** —— 设个"必须 < 50%"的阈值只会让人去改这一档的
// 句子, 而不是去补别名。
func unanticipatedQueries() []RecallCase {
	return []RecallCase{
		{Query: "把几段素材拼成一段片子", Want: []string{"GenerateVideo"},
			Note: "同一个意图 (要视频), 说法与已声明别名 (视频生成/做视频/生成一个视频) 不重合"},
		{Query: "把数字画成一张图", Want: []string{"GenerateChart"},
			Note: "已声明的是 图表/数据可视化/柱状图 等名词短语; 这条是**动词句**"},
		{Query: "让这段话能出声", Want: []string{"GenerateSpeech"},
			Note: "已声明的是 语音合成/文字转语音/配音; 这条绕开了所有术语"},
		{Query: "这段代码动一下会影响哪些地方", Want: []string{"code_intel_query"},
			Note: "已声明的是 影响面/谁会受影响/改了会怎样; 这条更长、更口语"},
	}
}

// ── 检索实现: 与池内实际用的那条路径同构 ──────────────────────────────────

// rankDocs 是保留集测的那条检索路径。它与 pooltools.go 里 poolIndex.rank 调用
// 的是**同一组**函数 (Tokenize / RankBM25 / RankCoverage / FuseRRF) —— 否则保留
// 集测的就是一个测试专用的假实现, 绿也说明不了生产路径召回得怎么样。
func rankDocs(e docsRanker, query string, limit int) []string {
	return e.Rank(query, limit)
}

// docsRanker 用真实的融合管线实现 tool.Ranker。
type docsRanker struct{ docs []Doc }

func (d docsRanker) Rank(query string, limit int) []string {
	q := strings.TrimSpace(query)
	if q == "" {
		out := make([]string, 0, len(d.docs))
		for _, doc := range d.docs {
			out = append(out, doc.Name)
		}
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	fused := FuseRRF([][]Ranked{RankBM25(q, d.docs), RankCoverage(q, d.docs)})
	out := make([]string, 0, len(fused))
	for _, r := range fused {
		out = append(out, d.docs[r.Idx].Name)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ── 测试 1: 词面有通路的那一档必须零漏召 ──────────────────────────────────

func TestRecall_EnglishQueriesAllHit(t *testing.T) {
	r := docsRanker{docs: realPool()}
	rep := EvaluateRecall(func(q string) []string { return rankDocs(r, q, 8) }, EnglishQueries())
	if rep.Total == 0 {
		t.Fatal("保留集是空的 —— 这个测试什么也没测")
	}
	if rep.MissRate() != 0 {
		t.Fatalf("词面有通路的查询必须零漏召, 实际 %s\n漏召明细:\n%s",
			rep.MissSummary(), missDetail(rep))
	}
	// 空结果是更严重的一种漏召 (模型会读成"能力不存在"), 单独断言。
	if rep.Empty != 0 {
		t.Fatalf("不得有任何一条查询返回空结果, 实际 %d 条:\n%s", rep.Empty, missDetail(rep))
	}
}

// ── 测试 2: 中文查询打到中文描述上必须命中 ────────────────────────────────

func TestRecall_ChineseQueryAgainstChineseDescription(t *testing.T) {
	pool := realPool()
	r := docsRanker{docs: pool}
	cases := []RecallCase{}
	for _, c := range ChineseQueries() {
		// 只挑描述确实是中文的那几条 (want 是 evo_*)。
		for _, want := range c.Want {
			if strings.HasPrefix(want, "evo_") {
				cases = append(cases, c)
				break
			}
		}
	}
	if len(cases) == 0 {
		t.Fatal("中文描述那一档是空的 —— 这个测试什么也没测")
	}
	rep := EvaluateRecall(func(q string) []string { return rankDocs(r, q, 8) }, cases)
	if rep.MissRate() != 0 {
		t.Fatalf("中文查询打中文描述必须零漏召 (描述与查询同语言, 词面是有通路的), 实际 %s\n%s",
			rep.MissSummary(), missDetail(rep))
	}
}

// ── 测试 3: 子串检索在中文上必然归零 (这条守的是"我们真的修了它") ──────────

// TestRecall_SubstringScorerCannotDoChinese 把**旧实现** (纯子串) 跑一遍。
//
// 留着这条不是为了怀旧: 它是"分词那一步确实在起作用"的反向证据。哪天有人把
// Tokenize 换回 strings.Fields, 测试 2 会红, 但红得不明所以; 这一条会直接说
// "旧形态在这个查询上召回了 0 条"。
//
// ── 这张用例表本身就是一次修正的产物 ──────────────────────────────────────
//
// 最初写的三条里有一条 ("进化候选") 在旧实现下**居然召回了** —— 因为描述里逐字
// 含有这个子串。这说明旧实现的失效面比我一开始以为的窄: 它不是"完全做不了中文",
// 而是"只认得逐字子串"。所以这张表改成了**改写过的说法** (换语言 / 换词序 / 加
// 虚词), 那才是真实的失效面。这个修正很重要 —— 判据写宽了会得到一个恒真的
// "旧实现不行", 而它挡不住真正的回归。
func TestRecall_SubstringScorerCannotDoChinese(t *testing.T) {
	pool := realPool()
	legacy := func(query string) []string {
		q := strings.ToLower(strings.TrimSpace(query))
		var out []string
		for _, d := range pool {
			score := 0
			for _, term := range strings.Fields(q) {
				if strings.Contains(strings.ToLower(d.Name), term) {
					score += 3
				}
				if strings.Contains(strings.ToLower(d.Text), term) {
					score++
				}
			}
			if score > 0 {
				out = append(out, d.Name)
			}
		}
		return out
	}
	// ⚠️ 断言是"召回**不到目标**", 不是"召回结果为空" —— 这两件事不一样。
	// 旧实现跑中文查询时并非总是空结果, 它可能返回一批**不相干**的资产
	// (评分恰好命中某个子串)。把判据写成 len(got) != 0 会在"有结果但全错"时
	// 变红并给出误导性的结论 —— 而"有结果但全错"是误召, 是漏召的孪生兄弟,
	// 且更隐蔽: 模型拿到一个像模像样的候选就去用了。
	// ⚠️ "进化候选" **不**在这张表里 —— 实测旧实现能召回它, 因为 "提交进化候选。"
	// 这段描述里**逐字**含 "进化候选" 这个子串。这正是为什么保留集不能拿逐字
	// 关键词来编: 那种查询会让子串检索也拿到 100%, 于是一个恒真的验收看起来
	// 像"检索没问题"。真正的失效面是"换了说法"与"换了语言", 所以要拿**改写过
	// 的**说法去测。
	for _, c := range []RecallCase{
		{Query: "视频生成", Want: []string{"GenerateVideo"}},    // 换了语言
		{Query: "影响面", Want: []string{"code_intel_query"}},  // 换了语言
		{Query: "候选提交", Want: []string{"evo_propose"}},      // ★ 同语言, 只换了词序
		{Query: "查看一下环境的状态", Want: []string{"evo_inspect"}}, // ★ 同语言, 加了虚词
	} {
		got := legacy(c.Query)
		for _, g := range got {
			for _, w := range c.Want {
				if g == w {
					t.Fatalf("旧实现 (子串打分) 居然召回了 %q → %v; 这条测试的前提没了, "+
						"下面「旧形态召不回」的结论也就不成立 —— 请重新确认修复的必要性", c.Query, got)
				}
			}
		}
		t.Logf("旧实现: %q → %v (含目标: 否)", c.Query, got)
	}
	// 新实现对同一条查询必须召回目标 —— 两边都测, 才说明是**改动**带来的差别。
	// 这里带上别名 (生产上别名由部署侧声明, 保留集测的是"声明了就能召"这条通路)。
	aliased := realPool()
	for i := range aliased {
		if aliased[i].Name == "GenerateVideo" {
			aliased[i].Aliases = []string{"生成视频"}
		}
	}
	r := docsRanker{docs: aliased}
	rep := EvaluateRecall(func(q string) []string { return rankDocs(r, q, 8) },
		[]RecallCase{{Query: "生成视频", Want: []string{"GenerateVideo"}}})
	if rep.Hit != 1 {
		t.Fatal("新实现 (+ 别名) 也召不回 '生成视频' —— 别名那条通路没接上")
	}
	// 而没有别名的裸池, 这一条**仍然召不回** —— 这正是"别名不是装饰"的证据:
	// 声明与不声明之间有可观测的差别。
	bare := EvaluateRecall(func(q string) []string { return rankDocs(docsRanker{docs: barePool(pool)}, q, 8) },
		[]RecallCase{{Query: "生成视频", Want: []string{"GenerateVideo"}}})
	if bare.Hit != 0 {
		t.Fatalf("裸池 (无别名) 不该能召回跨语言查询 —— 若能, 说明有别的东西在起作用, "+
			"而别名这一层的必要性需要重新论证: %s", bare.MissSummary())
	}
}

// ── 测试 4a: 声明了别名就必须召回 ──────────────────────────────────────────

// TestRecall_DeclaredAliasMustRecall 别名那条通路必须真的接上。
//
// 别名声明了却召不回, 与没声明是**一回事** —— 而后者看起来像正常 (谁会去查一条
// 没写下来的声明? )。所以这条测试守的不是排序质量, 是"声明 → 生效"这条链。
func TestRecall_DeclaredAliasMustRecall(t *testing.T) {
	pool := realPool()
	r := docsRanker{docs: pool}
	rep := EvaluateRecall(func(q string) []string { return rankDocs(r, q, 8) }, aliasOnlyQueries())
	if rep.Total == 0 {
		t.Fatal("别名那一档是空的 —— 这个测试什么也没测")
	}
	if rep.MissRate() != 0 {
		t.Fatalf("声明了别名的资产必须能被那句话召回, 实际 %s\n%s",
			rep.MissSummary(), missDetail(rep))
	}
}

// TestRecall_CasesDoNotDriftBetweenBuckets 同一档里的用例必须真的属于那一档。
//
// 档与档之间是会漂的: 给一个英文档的用例加上别名之后, 它就该挪到别名档去 ——
// 留在英文档会让那个"词面有通路必须零漏召"的硬指标**虚高** (它开始靠别名过线,
// 而不是靠检索本体)。这条交叉检查把这种漂移变成红的。
func TestRecall_CasesDoNotDriftBetweenBuckets(t *testing.T) {
	pool := realPool()
	// 把没写别名的池当基准: 一份用例若**已经**能靠词面召回, 它就不属于别名档。
	bare := docsRanker{docs: barePool(pool)}
	for _, c := range aliasOnlyQueries() {
		rep := EvaluateRecall(func(q string) []string { return rankDocs(bare, q, 8) },
			[]RecallCase{{Query: c.Query, Want: c.Want}})
		if rep.Hit > 0 {
			t.Errorf("别名档里的用例 %q 其实**已经**能靠词面召回 (%v) —— 它该挪进 "+
				"EnglishQueries, 否则那一档的零漏召指标就不是检索本体挣来的",
				c.Query, c.Want)
		}
	}
	// 反向: 英文档里的用例不该依赖别名 (否则那条硬指标名不副实)。
	for _, c := range EnglishQueries() {
		rep := EvaluateRecall(func(q string) []string { return rankDocs(bare, q, 8) },
			[]RecallCase{{Query: c.Query, Want: c.Want}})
		if rep.Hit == 0 {
			t.Errorf("英文档里的用例 %q 在**无别名**池上召不回 —— 它该挪进 aliasOnlyQueries "+
				"(或说明它其实是跨语言档), 否则 EnglishQueries 的零漏召是别名撑起来的", c.Query)
		}
	}
}

// barePool 去掉所有别名 (对照用: 量的是"词面检索本身能走多远")。
func barePool(docs []Doc) []Doc {
	out := make([]Doc, len(docs))
	for i, d := range docs {
		d.Aliases = nil
		out[i] = d
	}
	return out
}

// ── 测试 4: 跨语言检索的天花板如实记账 ────────────────────────────────────

// TestRecall_CrossLingualIsADocumentedCeiling 记下这一档实际能召回多少。
//
// **它不 fail。** 这一档在词面检索下本来就召不回, 让它红等于把一个已知的、
// 需要另一个决定才能修的限制, 伪装成"回归"。它存在的意义是把天花板这个数
// 显式写进测试输出 —— 谁要动检索, 先看见这里还剩多少跨语言查询召不回。
func TestRecall_CrossLingualIsADocumentedCeiling(t *testing.T) {
	// **裸池** (去别名): 这一档问的是"没有人为声明时, 词面检索能走多远"。
	// 带别名跑就变成在测别名表全不全, 那是另一件事。
	r := docsRanker{docs: barePool(realPool())}
	rep := EvaluateRecall(func(q string) []string { return rankDocs(r, q, 8) }, crossLingualQueries())
	rate := rep.MissRate()
	if rate < 0 {
		t.Fatal("跨语言那一档是空的 —— 天花板这个数就无从记起了")
	}
	if rate == 0 {
		// 这不是好事而是信号: 说明有人接上了 embedder (或者往描述里塞了中文),
		// 那这一档该从"天花板"改成硬指标, 并把这条注释一起改掉。
		t.Fatalf("跨语言查询居然全都召回了 —— 天花板被抬高了。请把这一档移到 "+
			"EnglishQueries 当硬指标 (并更新 recall.go 里那段「RRF 在这里不是向量检索」的说明): \n%s",
			rep.MissSummary())
	}
	// ── 漏了就必须是**空**, 不能是"召回了些不相干的东西" ──────────────────────
	//
	// 这条断言是**注入自测逼出来的**: 我往检索里塞了一个"无差别提名第一份文档"
	// 的缺陷, 期待这一档变红 —— 结果全绿。原因是这条测试只 t.Logf 了 MissSummary,
	// 而那个数在"空结果 4 条"变成"空结果 3 条"时**照样非零**, 于是跑得过去。
	//
	// 而这个问题不是小事: 这一档的查询(中文问英文工具)若能撞出**非空**结果, 那只
	// 可能是中文描述里的巧合字面命中 —— 描述是英文的, 召回来的东西与查询毫无关系。
	// 模型拿到它会去用 (它看起来像模像样), 这比空结果糟得多。
	//
	// 判据因此落在 Empty 上: 这一档要么空 (当前的真实情况), 要么命中 (rate==0 那
	// 一支已经在上面拦住了)。中间那种"给了些垃圾"没有存在的理由。
	if rep.Empty != rep.Total {
		t.Fatalf("跨语言档里 %d/%d 条召回了**非空**结果 —— 中文查询打英文描述时,"+
			"非空只可能是巧合字面命中, 模型会拿它当真去用 (比空结果更糟):\n%s",
			rep.Total-rep.Empty, rep.Total, missDetail(rep))
	}
	t.Logf("跨语言天花板: %s —— 这一类查询在词面检索下无通路且**结果为空格**, 不是回归",
		rep.MissSummary())
}

// ── 测试 4b: 词面误导 —— 召回了错的东西, 比空结果更危险 ────────────────────

// TestRecall_MisleadingWordCollision 把"词面误导"这个失败模式钉住。
//
// 它同时验两件相反的事:
//
//	① 裸池上**确实会**召回到错的那个 (evo_propose) —— 这是这个失败模式的证据。
//	   若这里变红, 说明有别的机制挡住了它, 那这段说明就该改写。
//	② 带别名的池上**必须**召回到对的那个 —— 别名是唯一解, 验它真的有效。
//
// 只写②会让人以为"检索挺好"; 只写①会让人以为"没救"。两条一起才是完整的画面:
// 这是词面检索解决不了的问题, 而声明能解决它。
func TestRecall_MisleadingWordCollision(t *testing.T) {
	c := misleadingQueries()
	if len(c) == 0 {
		t.Fatal("误导档是空的 —— 这个测试什么也没测")
	}
	bare := docsRanker{docs: barePool(realPool())}
	onBare := bare.Rank(c[0].Query, 8)
	if len(onBare) == 0 {
		t.Fatalf("裸池上 %q 竟然是空结果 —— 这个失败模式的证据没了 "+
			"(说明词面检索在这里没有误召; 那 misleadingQueries 的说法要更新)", c[0].Query)
	}
	// 裸池召回的第一名应当**不是**目标 (否则就不叫误导了)。
	if onBare[0] == c[0].Want[0] {
		t.Fatalf("裸池上居然直接召回了目标 %v —— 那这一条该挪进 EnglishQueries, "+
			"别名对它的必要性需要重新论证", onBare)
	}
	t.Logf("词面误导的证据: %q → %v (第一名是错的, 而它看起来完全合理)", c[0].Query, onBare)

	// ② 别名池上必须召回目标。
	aliased := docsRanker{docs: realPool()}
	rep := EvaluateRecall(func(q string) []string { return aliased.Rank(q, 8) }, c)
	if rep.MissRate() != 0 {
		t.Fatalf("声明了别名之后必须召回目标 —— 否则这个词面误导无解: %s\n%s",
			rep.MissSummary(), missDetail(rep))
	}
}

// ── 测试 5: 融合与排序的性质 ──────────────────────────────────────────────

// TestFuseRRF_TiedScoresShareARank 并列必须同名次。
//
// 这条守的是一个**会被 boost 放大的**坑: 如果并列按切片下标算名次, 两个原始分
// 完全相同的文档会得到不同的融合分, 而这个差值在乘上个性化 boost 之后会翻转
// 排序 —— 于是"陈老师成功过 12 次的工具"反而排到了后面, 而没有任何地方会红。
func TestFuseRRF_TiedScoresShareARank(t *testing.T) {
	// 三份文档在 A 路同分 (并列第 1), 在 B 路 B 档靠前。
	listA := []Ranked{{Idx: 0, Score: 5}, {Idx: 1, Score: 5}, {Idx: 2, Score: 5}}
	listB := []Ranked{{Idx: 2, Score: 9}, {Idx: 0, Score: 1}, {Idx: 1, Score: 1}}
	fused := FuseRRF([][]Ranked{listA, listB})

	// A 路并列第 1 → 0/1/2 从 A 路拿到**相同**的 1/(k+1)。
	// B 路 0 与 1 也并列 (第 2) → 两者再拿相同的 1/(k+2)。
	// 所以 0 与 1 的总分必须逐位相等。
	score := map[int]float64{}
	for _, r := range fused {
		score[r.Idx] = r.Score
	}
	if score[0] != score[1] {
		t.Fatalf("A 路并列、B 路也并列的两份文档融合分必须相等, got idx0=%.10f idx1=%.10f", score[0], score[1])
	}
	// 反向: 2 在 B 路单独第一, 必须高于 0/1。如果这里也相等, 说明名次算错了。
	if !(score[2] > score[0]) {
		t.Fatalf("B 路单独第一应高于并列两条, got idx2=%.10f idx0=%.10f", score[2], score[0])
	}
}

// TestFuseRRF_RanksByListPosition 名次按**排序位置**走, 不是按原始分值的差。
//
// BM25 与覆盖率两路的原始分**不同量纲** (一个是可以大于 1 的 IDF 加权和, 一个
// 是 0-1 的比例), 直接相加是没有意义的。RRF 的全部价值就在于它只用名次 ——
// 这条测试把这个性质钉住: 把原始分整体放大 1000 倍, 结果必须逐位不变。
func TestFuseRRF_RanksByListPosition(t *testing.T) {
	base := []Ranked{{Idx: 0, Score: 3}, {Idx: 1, Score: 2}, {Idx: 2, Score: 1}}
	scaled := []Ranked{{Idx: 0, Score: 3000}, {Idx: 1, Score: 2000}, {Idx: 2, Score: 1000}}
	a := FuseRRF([][]Ranked{base, base})
	b := FuseRRF([][]Ranked{scaled, scaled})
	if len(a) != len(b) {
		t.Fatalf("长度不一致: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Idx != b[i].Idx || a[i].Score != b[i].Score {
			t.Fatalf("原始分放大不应改变融合结果, pos %d: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestRankBM25_RareTokenBeatsCommonToken 稀有词命中应压过常见词命中。
//
// 这是 BM25 存在的理由 —— 没有这条, 覆盖率那一路单独就够了, 两路融合也就没有
// 意义 (两个排序器永远一致时, 融合只是多算一遍)。
func TestRankBM25_RareTokenBeatsCommonToken(t *testing.T) {
	// 两份文档**命中的那个词在语料里的稀有度不同**: "database" 在多份文档里都有
	// (df 高 → IDF 低), "candlestick" 只在一份里 (df 低 → IDF 高)。词频与文档
	// 长度都对齐, 所以分差只可能来自 IDF。
	docs := []Doc{
		{Name: "d0", Text: "database"},
		{Name: "d1", Text: "database"},
		{Name: "d2", Text: "database"},
		{Name: "rare_hit", Text: "candlestick"},
	}
	ranked := RankBM25("candlestick", docs)
	if len(ranked) != 1 {
		t.Fatalf("只有一份文档含 candlestick, 应只提名它, got %d", len(ranked))
	}
	if ranked[0].Idx != 3 {
		t.Fatalf("应提名 rare_hit (idx3), got idx=%d", ranked[0].Idx)
	}
	rareScore := ranked[0].Score

	// 换个查询, 只问那个常见词: 命中三份, 得分逐份与上面的稀有词比。
	common := RankBM25("database", docs)
	if len(common) != 3 {
		t.Fatalf("三份文档含 database, 应提名三份, got %d", len(common))
	}
	if !(common[0].Score < rareScore) {
		t.Fatalf("稀有词的 BM25 分应高于常见词 (IDF 在起作用): 稀有 %.6f vs 常见 %.6f",
			rareScore, common[0].Score)
	}
}

// TestRankCoverage_CoversAllQueryTermsBeatsOne 覆盖率看的是**覆盖了几个查询词**,
// 而不是某个词出现了几次。这条是覆盖率那一路与 BM25 的不同之处。
func TestRankCoverage_CoversAllQueryTermsBeatsOne(t *testing.T) {
	docs := []Doc{
		{Name: "one_term_many_times", Text: "export export export export export export"},
		{Name: "both_terms", Text: "export import"},
	}
	ranked := RankCoverage("export import", docs)
	if len(ranked) != 2 {
		t.Fatalf("两份都该被提名, got %d", len(ranked))
	}
	if ranked[0].Idx != 1 {
		t.Fatalf("覆盖两个查询词的应排前, got idx=%d (want 1)", ranked[0].Idx)
	}
	if ranked[0].Score != 1.0 {
		t.Fatalf("全覆盖的覆盖率应为 1, got %v", ranked[0].Score)
	}
}

// ── 测试 5b: 验收报告里那两个数 (实测, 不是断言) ────────────────────────────

// TestD42_ReportTheNumbers 把 D4.2 汇报要用到的数**算出来打出来**。
//
// 这条**不 fail** (除非算不出来), 因为测试回答"对不对", 而报告要写"差多少"。
// "漏召率从 50% 降到 0" 这句话必须有个出处 —— 全绿的测试推不出任何百分比。
//
// 跑法: go test ./pkg/tool/ -run TestD42_ReportTheNumbers -v
func TestD42_ReportTheNumbers(t *testing.T) {
	ds := realPool()
	// 旧实现原样复刻 (子串打分), 用于对照。
	legacyRank := func(query string, limit int) []string {
		q := strings.ToLower(strings.TrimSpace(query))
		var ss []struct {
			name  string
			score int
		}
		for _, d := range ds {
			s := 0
			for _, term := range strings.Fields(q) {
				if strings.Contains(strings.ToLower(d.Name), term) {
					s += 3
				}
				if strings.Contains(strings.ToLower(d.Text), term) {
					s++
				}
			}
			if s > 0 {
				ss = append(ss, struct {
					name  string
					score int
				}{d.Name, s})
			}
		}
		for i := 0; i < len(ss); i++ {
			for j := i + 1; j < len(ss); j++ {
				if ss[j].score > ss[i].score {
					ss[i], ss[j] = ss[j], ss[i]
				}
			}
		}
		var out []string
		for _, s := range ss {
			out = append(out, s.name)
		}
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	newRank := func(query string, limit int) []string {
		fused := FuseRRF([][]Ranked{RankBM25(query, ds), RankCoverage(query, ds)})
		var out []string
		for _, r := range fused {
			out = append(out, ds[r.Idx].Name)
		}
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}

	// 四档分开报, 且**每档用该档该测的那个池** —— 混在一起的那个数不能指导
	// 动作, 用错池的那个数则是错的 (见下面 ④ 的说明):
	//
	//	① / ④ 用**裸池** (去别名): 它们问的是"词面检索本身能走多远"
	//	②      用裸池: 测的是分词, 与别名无关
	//	③      用**带别名的池**: 那一档的全部意义就是别名
	bare := barePool(ds)
	bareRank := func(query string, limit int) []string {
		fused := FuseRRF([][]Ranked{RankBM25(query, bare), RankCoverage(query, bare)})
		var out []string
		for _, r := range fused {
			out = append(out, bare[r.Idx].Name)
		}
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	bareLegacyRank := func(query string, limit int) []string {
		q := strings.ToLower(strings.TrimSpace(query))
		var out []string
		for _, d := range bare {
			s := 0
			for _, term := range strings.Fields(q) {
				if strings.Contains(strings.ToLower(d.Name), term) {
					s += 3
				}
				if strings.Contains(strings.ToLower(d.Text), term) {
					s++
				}
			}
			if s > 0 {
				out = append(out, d.Name)
			}
		}
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}

	// ② 中文查询打中文描述 (与别名无关: 描述本身就是中文)。
	zhCases := []RecallCase{}
	for _, c := range ChineseQueries() {
		for _, w := range c.Want {
			if strings.HasPrefix(w, "evo_") {
				zhCases = append(zhCases, c)
				break
			}
		}
	}

	buckets := []struct {
		name          string
		cases         []RecallCase
		pool, aliased bool // pool: 用裸池 (true) 还是带别名池 (false)
	}{
		{"① 英文, 词面有通路 (硬指标)", EnglishQueries(), true, false},
		{"② 中文查询打中文描述", zhCases, true, false},
		{"③ 只有别名能通路", aliasOnlyQueries(), false, true},
		{"④ 跨语言天花板 (元指标)", crossLingualQueries(), true, false},
		{"⑤ 词面误导 (召回错的那个)", misleadingQueries(), true, false},
	}
	for _, b := range buckets {
		lg, nw := legacyRank, newRank
		if b.pool {
			lg, nw = bareLegacyRank, bareRank
		}
		l := EvaluateRecall(func(q string) []string { return lg(q, 8) }, b.cases)
		n := EvaluateRecall(func(q string) []string { return nw(q, 8) }, b.cases)
		if l.MissRate() < 0 || n.MissRate() < 0 {
			t.Fatalf("%s: 保留集是空的, 报不出数", b.name)
		}
		poolKind := "裸池"
		if b.aliased {
			poolKind = "带别名"
		}
		t.Logf("%-30s [%s] 旧 %s → 新 %s", b.name, poolKind, shortMiss(l), shortMiss(n))
	}

	// ④ / ⑤ 还要再报一遍**带别名的池** —— 那一遍才是上线后的真实行为。
	//
	// 裸池那一遍量的是"词面检索本身的天花板", 它解释了**为什么**需要别名;
	// 带别名那一遍量的是"别名把天花板顶到了哪"。两个数都要有: 只报裸池会显得
	// 这两档没救, 只报别名池会把"还有多少说法没被预见到"藏起来。
	//
	// ⚠️ ⑤ 在带别名池上必须**看首位**, 不能只看"有没有召回" —— 它的旧行为就是
	// "召回了, 但召回的是错的那个", 一个只看命中与否的指标会把它报成达标。
	aliasedRank := func(query string, limit int) []string {
		fused := FuseRRF([][]Ranked{RankBM25(query, ds), RankCoverage(query, ds)})
		var out []string
		for _, r := range fused {
			out = append(out, ds[r.Idx].Name)
		}
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	for _, b := range []struct {
		name  string
		cases []RecallCase
	}{{"④ 跨语言天花板", crossLingualQueries()}, {"⑤ 词面误导", misleadingQueries()},
		{"⑥ 未预见说法 (别名覆盖不到的)", unanticipatedQueries()}} {
		rep := EvaluateRecall(func(q string) []string { return aliasedRank(q, 8) }, b.cases)
		t.Logf("%-30s [带别名] 新 %s", b.name, shortMiss(rep))
		firstHit := 0
		for _, c := range b.cases {
			hits := aliasedRank(c.Query, 8)
			if len(hits) > 0 && containsStr(c.Want, hits[0]) {
				firstHit++
			}
		}
		t.Logf("%-30s [带别名] 首位命中 %d/%d", b.name, firstHit, len(b.cases))
	}

	// ⚠️⚠️ 汇报时**必须**带上这一句, 否则上面的 0% 会被读成"检索能听懂任意说法":
	//
	//	③④⑤ 三档的 0% 是**带别名池 + 别名与保留集同源**下的 0%。别名是"有人
	//	预见到这种说法就声明一条", 所以它证明的是**机制接通了、天花板可移动**,
	//	不是"任意说法都能召回"。真正的泛化界限在 ⑥ 那一档。
	t.Logf("⚠️ ③④⑤ 的 0%% 前提: 别名与保留集同一人所写 —— 它证明机制通了, 不证明能听懂任意说法 (见 ⑥)")

	// ⚠️ 一条必须写进汇报的**负面**发现: ① 那一档旧实现也是 0% 漏召。
	// 也就是说**英文词面查询这一档分辨不出新旧实现** —— 子串打分对"词都在描述里"
	// 的查询本来就是能过的。修复的真实收益集中在 ②(分词) 与 ③(别名通路),
	// 报成绩时不能笼统说"漏召率下降", 那个说法会把 ① 也算成战果。

	// 广告面 token 的常数化。
	advertised := []Doc{
		{Name: "pool_search", Text: strings.Repeat("Search the deferred tool & skill pool by keyword. ", 4)},
		{Name: "pool_load", Text: strings.Repeat("Load a pooled asset into the advertised surface. ", 4)},
		{Name: "pool_release", Text: strings.Repeat("Release a pooled asset. ", 4)},
		{Name: "Read", Text: "Read a file"},
		{Name: "Bash", Text: "Run a shell command"},
	}
	for _, extra := range []int{0, 100, 900} {
		all := append([]Doc{}, advertised...)
		all = append(all, ds...)
		for i := 0; i < extra; i++ {
			all = append(all, Doc{Name: "synth_" + itoa(i),
				Text: strings.Repeat("miscellaneous helper for assorted internal bookkeeping ", 4)})
		}
		rep := EvaluateSurface(toAPITools(advertised), toAPITools(all), len(ds)+extra)
		t.Logf("池成员 %4d 件 → 广告面 %d 件, 工具段 %5d (全量 %6d, 约 %.0f%%)",
			rep.Members, rep.Advertised, rep.Tokens, rep.BaselineTokens,
			float64(rep.Tokens)/float64(rep.BaselineTokens)*100)
	}
}

// ── 测试 6: 空保留集与空结果 ───────────────────────────────────────────────

// TestRecall_EmptyHoldoutIsNotZeroMissRate 空保留集必须报"没测过"(-1)。
//
// 这是 D4.1 那条「空闸门集合上的全过恒真」的同型问题: Total=0 时漏召率 0%
// 是一个恒真的假结论, 而它长得跟"测过了没问题"一模一样。
func TestRecall_EmptyHoldoutIsNotZeroMissRate(t *testing.T) {
	rep := EvaluateRecall(func(string) []string { return nil }, nil)
	if rep.MissRate() != -1 {
		t.Fatalf("空保留集的漏召率必须是 -1 (没测过), got %v", rep.MissRate())
	}
	if !strings.Contains(rep.MissSummary(), "没测过") {
		t.Fatalf("空保留集的说明必须说「没测过」, got %q", rep.MissSummary())
	}
	// 反过来: 非空但零漏召时, 不该再出现"没测过"。
	ok := EvaluateRecall(func(string) []string { return []string{"a"} },
		[]RecallCase{{Query: "q", Want: []string{"a"}}})
	if ok.MissRate() != 0 {
		t.Fatalf("全命中时漏召率应为 0, got %v", ok.MissRate())
	}
	if strings.Contains(ok.MissSummary(), "没测过") {
		t.Fatalf("零漏召的说明不得出现「没测过」, got %q", ok.MissSummary())
	}
}

// TestEmptyRecallNote_NeverClaimsTheCapabilityIsAbsent 空结果那句话不许说错。
//
// 旧文案是"池内没有匹配资产"。池子里**确实有**这个能力、只是问法对不上时,
// 这句话是假的, 而模型会照它行事 (判定"能力不存在" → 放弃或拿别的工具硬凑)。
func TestEmptyRecallNote_NeverClaimsTheCapabilityIsAbsent(t *testing.T) {
	// ① 池里有人, 但没一个词对上 → 不得说"池内没有匹配资产"。
	note := EmptyRecallNote("视频生成", 42, false)
	if strings.Contains(note, "池内没有匹配资产") {
		t.Fatalf("池里有 42 件时, 「池内没有匹配资产」是一句**假话**: %q", note)
	}
	for _, want := range []string{"42", "词面", "不等于"} {
		if !strings.Contains(note, want) {
			t.Errorf("这句话里必须出现 %q —— 它负责把「召回为空」与「能力不存在」分开: %q", want, note)
		}
	}
	// ② 池是空的 → 必须说是池空的 (那是装配问题, 换关键词没用), 且不得引导用户换词。
	empty := EmptyRecallNote("视频生成", 0, false)
	if !strings.Contains(empty, "池是空的") {
		t.Fatalf("池为空时要说池是空的 (与问法无关), got %q", empty)
	}
	if strings.Contains(empty, "换一个更贴近") {
		t.Fatalf("池为空时让人换关键词是误导 —— 换词不会有任何变化: %q", empty)
	}
	// ③ fallback 开关只影响那一句追加, 不影响上面两种事实的表述。
	if !strings.Contains(EmptyRecallNote("q", 5, true), "未命中回退") {
		t.Error("fallback 为真时要补上未命中回退那句 (13.7-P2)")
	}
	if strings.Contains(EmptyRecallNote("q", 5, false), "未命中回退") {
		t.Error("fallback 为假时不得出现未命中回退那句")
	}
}

// ── 测试 7: 工具段 token 的常数化 ─────────────────────────────────────────

// TestSurfaceTokens_AdvertisedDoesNotGrowWithPool 广告面不随池子涨。
//
// 这是 D4.2 的第一条验收口径。它测的不是"省了多少", 是"**广告面是否只由已加载
// 的那几件决定**": 池子从 9 件涨到 900 件, 广告面必须逐字节不变。
func TestSurfaceTokens_AdvertisedDoesNotGrowWithPool(t *testing.T) {
	full := realPool()
	advertised := []Doc{
		// 包装三件 + 已加载的两件 —— 池子多大都只有这些。
		{Name: "pool_search", Text: "Search the deferred tool & skill pool by keyword..."},
		{Name: "pool_load", Text: "Load a pooled asset into the advertised surface..."},
		{Name: "pool_release", Text: "Release a pooled asset..."},
		{Name: "GenerateChart", Text: "Render a data chart to PNG using Apache ECharts..."},
		{Name: "code_intel_query", Text: "Query the code intelligence graph..."},
	}
	small := EvaluateSurface(toAPITools(advertised), toAPITools(append(append([]Doc{}, advertised...), full...)), len(full))
	if small.Members != len(full) {
		t.Fatalf("池成员数应报实际值, got %d", small.Members)
	}
	// 广告面的 token 只由 advertised 决定 —— 与池子大小无关。
	if small.Tokens <= 0 {
		t.Fatal("广告面 token 应为正")
	}
	// 池子翻十倍, 广告面必须一个字节都不涨。
	bigPool := append([]Doc{}, full...)
	for i := 0; i < 100; i++ {
		bigPool = append(bigPool, Doc{Name: "synthetic_" + itoa(i), Text: strings.Repeat("padding description ", 20)})
	}
	big := EvaluateSurface(toAPITools(advertised), toAPITools(append(append([]Doc{}, advertised...), bigPool...)), len(bigPool))
	if big.Tokens != small.Tokens {
		t.Fatalf("广告面 token 不得随池子增长: 池 %d 件时 %d, 池 %d 件时 %d",
			small.Members, small.Tokens, big.Members, big.Tokens)
	}
	if big.BaselineTokens <= small.BaselineTokens {
		t.Fatal("全量 token 必须随池子增长 —— 否则这个对照测不出东西 (两边都在测同一个空集)")
	}
	if big.Advertised != 5 {
		t.Fatalf("广告面件数应恒为 5 (池子多大都一样), got %d", big.Advertised)
	}
}

// TestSurfaceReport_SmallPoolSaysSoInsteadOfClaimingSavings 池子太小时,
// 池化**省不了** —— 这句话必须说出来, 不能粉饰成"持平"。
func TestSurfaceReport_SmallPoolSaysSoInsteadOfClaimingSavings(t *testing.T) {
	// 广告面比全量还大 (包装三件自身的 schema 成本超过了下沉的面) 是**真实会
	// 发生**的情形, 尤其在池子里只有一两件的时候。
	rep := EvaluateSurface(
		toAPITools([]Doc{{Name: "pool_search", Text: strings.Repeat("long wrapper schema ", 50)}}),
		toAPITools([]Doc{{Name: "tiny", Text: "x"}}),
		1,
	)
	if !strings.Contains(rep.SavingsNote(), "没有减少") {
		t.Fatalf("池化没起作用时必须直说, got %q", rep.SavingsNote())
	}
}

// ── 辅助 ──────────────────────────────────────────────────────────────────

func toAPITools(docs []Doc) []types.APITool {
	out := make([]types.APITool, 0, len(docs))
	for _, d := range docs {
		out = append(out, types.APITool{Name: d.Name, Description: d.Text, InputSchema: json.RawMessage("{}")})
	}
	return out
}

// shortMiss 给日志用的一行版 (MissSummary 里那句"空结果会被读成..."太长,
// 逐档对照时挤在一起反而看不清)。
func shortMiss(rep RecallReport) string {
	pct := int(rep.MissRate()*100 + 0.5)
	s := itoa(rep.Total-rep.Hit) + "/" + itoa(rep.Total) + " 漏召 (" + itoa(pct) + "%)"
	if rep.Empty > 0 {
		s += ", 空结果 " + itoa(rep.Empty) + " 条"
	}
	return s
}

func missDetail(rep RecallReport) string {
	var sb strings.Builder
	for _, m := range rep.Miss {
		sb.WriteString("  · ")
		sb.WriteString(strconv.Quote(m.Case.Query))
		sb.WriteString(" → got ")
		if len(m.Got) == 0 {
			sb.WriteString("(空)")
		} else {
			sb.WriteString(strings.Join(m.Got, ", "))
		}
		sb.WriteString("  [want " + strings.Join(m.Case.Want, "|") + "]")
		if m.Case.Note != "" {
			sb.WriteString(" — " + m.Case.Note)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// containsStr 判 s 是否在 list 里 —— 报告里用来算"首位命中"(见 ⑤ 的说明:
// 只看"有没有召回"会把"召回了错的那个"报成达标)。
func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
