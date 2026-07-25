package agent

// graph_templates_ensemble_test —— ensemble_extract / review_panel 迁到图引擎的等价性验收。
//
// ---------------------------------------------------------------------------
// 四个维度 (照 graph_templates_test.go / orchestrated_equiv_test.go 的做法)
// ---------------------------------------------------------------------------
//
//	① 阶段序列 (名字 + 角色 + 状态) —— REPORT.md / 门禁统计 / dashboard / 下游平台读的就是它。
//	   这一维对这两个 mode 尤其致命: 旧执行器返回**一条**记录, 而 map+reduce 直接站顶层
//	   会返回两条 (故图里用只跑 1 轮的 loop-group 把它们收成一个顶层节点)。
//	② LLM 调用次数 —— 多跑一次就是多烧一次 token (零重试这件事全靠这一维守住)。
//	③ 峰值并发 —— 桩里带真实 sleep, 让"3 路并行"在时间上真的可分辨。
//	④ 每路提示词逐字 —— 内核是裸 completion, 提示词变了就是模型产出变了。
//	   这两个 mode 的提示词差异全在 system prompt (差异化视角), 故按 (sys,user) 对比。
//
// 外加两组这两个 mode 专有的验收: **融合产出逐字段比对** (投票/截尾均值搬进内核后
// 数值必须一致) 与 **全路失败时的错误文案同形**。
//
// ---------------------------------------------------------------------------
// 为什么有三处刻意按"集合"而不是"序列"比对
// ---------------------------------------------------------------------------
//
// 旧路径的 swarm_intel.FanOutCollect **按完成顺序**追加分支结果 (fanout.go:76 起,
// 且 branches 是 map, goroutine 启动序本身随机), 于是下面三处在旧路径上是不确定的:
//
//	· 节点/边证据的累积顺序 (fuseExtractions 里 append(a.node.Evidence, ...))
//	· 边的相对顺序 —— 旧排序键只到 (confidence, src), 同 src 的多条边靠 map 遍历序定序
//	· 批注顺序与"取第一位有 summary 的"总评 (fuseReviews)
//
// 图路径按分片序 (= 视角声明序) 融合, 是确定性的。所以这几处只能按集合比对 ——
// 按序列比对会得到一个**随机红**的测试。这不是放松断言: 确定性本身另有内核侧的
// 13 次重跑比对测试 (pkg/graph/reduce_fuse_test.go) 钉住。
//
// ---------------------------------------------------------------------------
// 变异反证 (证明这些断言不是许愿; 每条都真改坏跑过一次, 见报告)
// ---------------------------------------------------------------------------
//
//	· 把 Policies.MaxParallel 从 4 改成 1        → ③ 报 "峰值并发不等价 旧=3 新=1"
//	· 把 map 的 MaxShards 从 3 改成 2           → ② 报 "LLM 调用次数不等价 旧=3 新=2"
//	· 系统提示词去掉 ensembleJSONOnlySuffix     → ④ 报 3 条提示词不等价
//	· 顶层直接摆 map+reduce (去掉 loop-group)   → ① 报 "阶段数不等价 旧=1 新=2"
//	· report 节点不套 ```json 壳                → 融合比对报 "产出里找不到 JSON 对象"

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
)

// ---------------------------------------------------------------------------
// 测试装置
// ---------------------------------------------------------------------------

// ensCall 一次 LLM 调用的 (系统提示词, 用户提示词)。
type ensCall struct{ sys, user string }

// ensStubLLM 可记录调用次数/峰值并发/逐条提示词的桩 LLMClient。
//
// 产出**只依赖 system prompt** (= 哪一路视角) 是刻意的: 融合结果因此完全确定,
// 才可以逐字段比对。产出带调用序号的话, 并发抖动会让融合产出每次都不同。
type ensStubLLM struct {
	delay time.Duration
	out   func(sys string) string
	// failAll true = 每路都返回错误 (全路失败路径)。
	failAll bool
	// failLens 命中该视角子串的那一路返回错误 (单路失败路径)。
	failLens string

	mu       sync.Mutex
	calls    []ensCall
	inFlight int
	peak     int
}

func (s *ensStubLLM) SimpleComplete(_ context.Context, sys, user string) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, ensCall{sys: sys, user: user})
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	if s.failAll {
		return "", fmt.Errorf("桩: 后端不可用")
	}
	if s.failLens != "" && strings.Contains(sys, s.failLens) {
		return "", fmt.Errorf("桩: 该路失败")
	}
	return s.out(sys), nil
}

func (s *ensStubLLM) snapshot() (calls, peak int, recorded []ensCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls), s.peak, append([]ensCall(nil), s.calls...)
}

func newEnsStubExecutor(llm *ensStubLLM) *WorkflowExecutor {
	return &WorkflowExecutor{llm: llm, notify: func(_, _ string) {}}
}

// ensSortedPrompts 把记录的调用压成可比对的有序集合 (旧路径的调用顺序不确定, 见文件头)。
func ensSortedPrompts(calls []ensCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.sys+"\x00"+c.user)
	}
	sort.Strings(out)
	return out
}

// ensJSONBody 从产出文本里取出融合 JSON (两条路径的产出都是 "…结果:\n\n```json\n{…}\n```\n")。
func ensJSONBody(t *testing.T, what, out string) []byte {
	t.Helper()
	i := strings.Index(out, "{")
	j := strings.LastIndex(out, "}")
	if i < 0 || j <= i {
		t.Fatalf("%s: 产出里找不到 JSON 对象 (前 120 字: %q)", what, ensHead(out, 120))
	}
	return []byte(out[i : j+1])
}

func ensHead(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ensNearly 浮点比较: 旧路径把分数除以 100 再乘回 100 (clamp01(score/100) → ×100),
// 内核直接对原始 0-100 求截尾均值。两者数学上相同但二进制末位可能差一点点,
// 拿 == 比会得到一个**看起来是等价性失败、实际是浮点表示**的红。
func ensNearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// ---------------------------------------------------------------------------
// 数据集: 三路差异化抽取产出
// ---------------------------------------------------------------------------
//
// 设计约束 (让融合结果与"哪一路先完成"无关, 见文件头):
//   - 共有节点「张三」在三路里的 type/first_chapter/证据**逐字相同** ⇒ "取第一个非空"
//     与证据累积顺序都不影响结果;
//   - 独有节点各只出现一路 ⇒ 无累积歧义;
//   - 三路各带一条边, 其中两条同 src ⇒ 正好覆盖"旧排序键只到 (confidence,src)"那处不确定性。

const ensSharedEvidence = `{"chapter":"第1章","quote":"张三登场"}`

func ensExtractReply(sys string) string {
	shared := `{"type":"character","name":"张三","first_chapter":"第1章","evidence":[` + ensSharedEvidence + `]}`
	switch {
	case strings.Contains(sys, "人物"):
		return `{"nodes":[` + shared + `,{"type":"character","name":"李四"}],` +
			`"edges":[{"type":"rel","src":"张三","dst":"李四","label":"师徒"}]}`
	case strings.Contains(sys, "事件"):
		return "```json\n" + `{"nodes":[` + shared + `,{"type":"event","name":"决战"}],` +
			`"edges":[{"type":"affects","src":"决战","dst":"张三"}]}` + "\n```"
	default: // 伏笔/物品/世界观
		return `{"nodes":[` + shared + `,{"type":"item","name":"玉佩"}],` +
			`"edges":[{"type":"owns","src":"张三","dst":"玉佩"}]}`
	}
}

// ensReviewReply 三位评审的多维打分。
//
// 三个维度**每位都打分** (不留缺席维度): 缺席维度是内核与上游的一处刻意偏离
// (上游取 map 零值 0 参与截尾, 内核只对真给了分的求均值), 把它混进等价性数据集
// 会让这条测试同时验两件事, 红了也分不清是哪件。缺席行为另有内核侧测试。
// summary 三位逐字相同 ⇒ "取第一位有 summary 的"与完成顺序无关。
func ensReviewReply(sys string) string {
	dims := func(plot, character, prose float64) string {
		return fmt.Sprintf(`{"dimension":"plot","score":%.0f,"summary":"情节评述"},`+
			`{"dimension":"character","score":%.0f,"summary":"人物评述"},`+
			`{"dimension":"prose","score":%.0f,"summary":"文笔评述"}`, plot, character, prose)
	}
	switch {
	case strings.Contains(sys, "严苛"):
		return `{"overall":72,"summary":"总体评述","dimensions":[` + dims(80, 70, 60) + `],` +
			`"annotations":[{"dimension":"plot","note":"第二章转折突兀","severity":"major"}]}`
	case strings.Contains(sys, "代入"):
		return `{"overall":78,"summary":"总体评述","dimensions":[` + dims(82, 71, 61) + `],` +
			`"annotations":[{"dimension":"character","note":"动机交代不足"}]}`
	default: // 文笔与叙事张力
		return `{"overall":75,"summary":"总体评述","dimensions":[` + dims(78, 69, 59) + `],` +
			`"annotations":[{"dimension":"prose","note":"长句偏多"}]}`
	}
}

// ensRunBothPaths 用同一个桩产出各跑一遍旧执行器与图路径, 返回两侧的 (阶段序列, 调用快照)。
func ensRunBothPaths(t *testing.T, name string, run func(*WorkflowExecutor, *ProductionTeam) ([]StageResult, error),
	mkStub func() *ensStubLLM) (oldRes, newRes []StageResult, oldLLM, newLLM *ensStubLLM,
	oldErr, newErr error, newTeam *ProductionTeam) {
	t.Helper()

	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "") // 显式清空: 不受外部环境影响
	oldLLM = mkStub()
	oldTeam := newStubTeam(t, name+"-old")
	oldRes, oldErr = run(newEnsStubExecutor(oldLLM), oldTeam)
	if _, err := os.Stat(filepath.Join(oldTeam.dataDir, "graph-journal")); !os.IsNotExist(err) {
		t.Fatalf("开关未设时不该产生 graph-journal (说明默认行为已被改成图路径): stat err=%v", err)
	}

	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")
	newLLM = mkStub()
	newTeam = newStubTeam(t, name+"-new")
	newRes, newErr = run(newEnsStubExecutor(newLLM), newTeam)
	return
}

// ---------------------------------------------------------------------------
// 一、ensemble_extract
// ---------------------------------------------------------------------------

// ensembleExtractExpectedSeq 旧执行器真实产出的阶段序列 (写死为**独立**期望:
// 若两条路径同时被改坏, 只比"两边相等"是发现不了的)。
var ensembleExtractExpectedSeq = []string{"ensemble-extract|graph-swarm|completed"}

// TestEnsembleExtract图内核阶段序列与调用次数等价 ①②
func TestEnsembleExtract图内核阶段序列与调用次数等价(t *testing.T) {
	wf := graphExtractSwarmWorkflow()
	const objective = "第一章 张三下山"
	oldRes, newRes, oldLLM, newLLM, oldErr, newErr, newTeam := ensRunBothPaths(t, "ens-ext",
		func(we *WorkflowExecutor, team *ProductionTeam) ([]StageResult, error) {
			return we.executeEnsembleExtract(context.Background(), wf, objective, team)
		},
		func() *ensStubLLM { return &ensStubLLM{out: ensExtractReply} })
	if oldErr != nil {
		t.Fatalf("旧 executeEnsembleExtract 失败: %v", oldErr)
	}
	if newErr != nil {
		t.Fatalf("图路径失败: %v", newErr)
	}
	// 硬证据: 图路径一定会开 journal
	if _, err := os.Stat(filepath.Join(newTeam.dataDir, "graph-journal")); err != nil {
		t.Fatalf("开关打开后应经图引擎跑 (graph-journal 应存在): %v", err)
	}

	assertSameSeq(t, "ensemble_extract 旧执行器 vs 独立期望", ensembleExtractExpectedSeq, tplStageSeq(oldRes))
	assertSameSeq(t, "ensemble_extract 图路径 vs 独立期望", ensembleExtractExpectedSeq, tplStageSeq(newRes))
	assertSameSeq(t, "ensemble_extract 旧 vs 新", tplStageSeq(oldRes), tplStageSeq(newRes))

	oc, _, oldCalls := oldLLM.snapshot()
	nc, _, newCalls := newLLM.snapshot()
	if oc != nc || oc != len(extractLenses) {
		t.Errorf("LLM 调用次数不等价: 旧=%d 新=%d, 期望均为视角数 %d (零重试是这一维守住的)", oc, nc, len(extractLenses))
	}
	// ④ 每路提示词逐字 (按集合: 两条路径的分支启动顺序都不保证)
	assertSameSeq(t, "ensemble_extract 每路提示词", ensSortedPrompts(oldCalls), ensSortedPrompts(newCalls))
	// 独立期望: 系统提示词 = 前缀 + 视角 + 只输出 JSON; 用户提示词 = objective
	want := make([]string, 0, len(extractLenses))
	for _, lens := range extractLenses {
		want = append(want, "你是小说设定分析师。"+lens+ensembleJSONOnlySuffix+"\x00"+objective)
	}
	sort.Strings(want)
	assertSameSeq(t, "ensemble_extract 提示词 vs 独立期望", want, ensSortedPrompts(newCalls))

	// 融合产出: 逐字段比对 (投票融合搬进内核后数值必须一致)
	ensAssertSameExtraction(t, oldRes[0].Output, newRes[0].Output)
	// 产出文本外壳: 旧路径的 "合议抽取(N 路投票)结果:" 头必须还在 (下游按它/```json 取产出)
	for _, tc := range []struct{ what, out string }{{"旧", oldRes[0].Output}, {"新", newRes[0].Output}} {
		if !strings.HasPrefix(tc.out, "合议抽取(3 路投票)结果:") {
			t.Errorf("%s路径产出缺少旧格式头 (前 60 字: %q)", tc.what, ensHead(tc.out, 60))
		}
		if !strings.Contains(tc.out, "```json") {
			t.Errorf("%s路径产出缺少 ```json 围栏", tc.what)
		}
	}
}

// ensAssertSameExtraction 融合抽取产出逐字段比对 (节点按序列, 边按集合, 见文件头)。
func ensAssertSameExtraction(t *testing.T, oldOut, newOut string) {
	t.Helper()
	var o, n exDoc
	if err := json.Unmarshal(ensJSONBody(t, "旧融合产出", oldOut), &o); err != nil {
		t.Fatalf("旧融合产出解析失败: %v", err)
	}
	if err := json.Unmarshal(ensJSONBody(t, "新融合产出", newOut), &n); err != nil {
		t.Fatalf("新融合产出解析失败: %v", err)
	}
	nodeKey := func(x exNode) string {
		ev := make([]string, 0, len(x.Evidence))
		for _, e := range x.Evidence {
			ev = append(ev, e.Chapter+"/"+e.Quote)
		}
		sort.Strings(ev) // 证据累积顺序在旧路径上随完成顺序变
		return fmt.Sprintf("%s|%s|%.4f|%d|%s|%v", x.Type, x.Name, x.Confidence, x.Votes, x.FirstChapter, ev)
	}
	oldNodes := make([]string, 0, len(o.Nodes))
	newNodes := make([]string, 0, len(n.Nodes))
	for _, x := range o.Nodes {
		oldNodes = append(oldNodes, nodeKey(x))
	}
	for _, x := range n.Nodes {
		newNodes = append(newNodes, nodeKey(x))
	}
	// 节点顺序按序列比: 排序键 (confidence desc, name asc) 在两侧都是全序
	assertSameSeq(t, "融合节点 (含 confidence/votes/证据)", oldNodes, newNodes)

	edgeKey := func(x exEdge) string {
		return fmt.Sprintf("%s|%s|%s|%s|%.4f|%d", x.Type, x.Src, x.Dst, x.Label, x.Confidence, x.Votes)
	}
	oldEdges := make([]string, 0, len(o.Edges))
	newEdges := make([]string, 0, len(n.Edges))
	for _, x := range o.Edges {
		oldEdges = append(oldEdges, edgeKey(x))
	}
	for _, x := range n.Edges {
		newEdges = append(newEdges, edgeKey(x))
	}
	sort.Strings(oldEdges) // 旧排序键只到 (confidence, src): 同 src 的多条边顺序不确定
	sort.Strings(newEdges)
	assertSameSeq(t, "融合边 (含 confidence/votes, 按集合)", oldEdges, newEdges)

	// 投票语义的独立断言: 三路都提到的节点 confidence=1 且 votes=3
	for _, x := range n.Nodes {
		if x.Name != "张三" {
			continue
		}
		if x.Votes != 3 || !ensNearly(x.Confidence, 1.0) {
			t.Errorf("三路共有节点应 votes=3 confidence=1.0, got votes=%d conf=%.4f", x.Votes, x.Confidence)
		}
	}
}

// TestEnsembleExtract图内核峰值并发等价 ③ 桩带真实 sleep, 让"3 路并行"在时间上可测。
func TestEnsembleExtract图内核峰值并发等价(t *testing.T) {
	wf := graphExtractSwarmWorkflow()
	mk := func() *ensStubLLM { return &ensStubLLM{delay: 40 * time.Millisecond, out: ensExtractReply} }
	_, _, oldLLM, newLLM, oldErr, newErr, _ := ensRunBothPaths(t, "ens-par",
		func(we *WorkflowExecutor, team *ProductionTeam) ([]StageResult, error) {
			return we.executeEnsembleExtract(context.Background(), wf, "标的", team)
		}, mk)
	if oldErr != nil || newErr != nil {
		t.Fatalf("执行失败: 旧=%v 新=%v", oldErr, newErr)
	}
	_, oldPeak, _ := oldLLM.snapshot()
	_, newPeak, _ := newLLM.snapshot()
	if oldPeak != len(extractLenses) {
		t.Errorf("旧路径峰值并发应为 %d (MaxConcurrency=4 容得下全部视角), got %d", len(extractLenses), oldPeak)
	}
	if newPeak != oldPeak {
		t.Errorf("峰值并发不等价: 旧=%d 新=%d (分片被串行化 / 并发闸取值不同都会命中这条)", oldPeak, newPeak)
	}
}

// TestEnsembleExtract全路失败时错误文案同形 全部分支报错时两条路径都必须返回**带每路诊断串**的
// 错误 —— 少了诊断串, "3 路都废了"只能靠翻 journal 里每个分片的原文。
func TestEnsembleExtract全路失败时错误文案同形(t *testing.T) {
	wf := graphExtractSwarmWorkflow()
	mk := func() *ensStubLLM { return &ensStubLLM{failAll: true, out: ensExtractReply} }
	oldRes, _, oldLLM, newLLM, oldErr, newErr, _ := ensRunBothPaths(t, "ens-fail",
		func(we *WorkflowExecutor, team *ProductionTeam) ([]StageResult, error) {
			return we.executeEnsembleExtract(context.Background(), wf, "标的", team)
		}, mk)
	if oldErr == nil || newErr == nil {
		t.Fatalf("全路失败时两条路径都应报错: 旧=%v 新=%v", oldErr, newErr)
	}
	if len(oldRes) != 0 {
		t.Errorf("旧路径全路失败时不返回阶段记录, got %d 条", len(oldRes))
	}
	for _, tc := range []struct {
		what string
		err  error
	}{{"旧", oldErr}, {"新", newErr}} {
		if !strings.Contains(tc.err.Error(), "无任何可解析的抽取结果") {
			t.Errorf("%s路径错误文案不同形: %v", tc.what, tc.err)
		}
		if !strings.Contains(tc.err.Error(), "err=") {
			t.Errorf("%s路径错误缺少每路诊断串: %v", tc.what, tc.err)
		}
	}
	oc, _, _ := oldLLM.snapshot()
	nc, _, _ := newLLM.snapshot()
	if oc != nc || oc != len(extractLenses) {
		t.Errorf("失败路径 LLM 调用次数不等价: 旧=%d 新=%d (期望 %d, 即失败不重试)", oc, nc, len(extractLenses))
	}
}

// ---------------------------------------------------------------------------
// 二、review_panel
// ---------------------------------------------------------------------------

var reviewPanelExpectedSeq = []string{"review-panel|review-swarm|completed"}

// TestReviewPanel图内核四维等价与融合数值等价
//
// **基准以 331f79e 修完跨维归一化之后的 fuseReviews 为准**: 修复前 dimensions 被当
// 概率分布除以各维之和 (80/70/60 → 38/33/29), 拿修复前的产出做基准会差一个 1/Σ 系数。
func TestReviewPanel图内核四维等价与融合数值等价(t *testing.T) {
	wf := reviewPanelWorkflow()
	const objective = "请评审第一章"
	oldRes, newRes, oldLLM, newLLM, oldErr, newErr, _ := ensRunBothPaths(t, "rv-panel",
		func(we *WorkflowExecutor, team *ProductionTeam) ([]StageResult, error) {
			return we.executeReviewPanel(context.Background(), wf, objective, team)
		},
		func() *ensStubLLM { return &ensStubLLM{out: ensReviewReply} })
	if oldErr != nil {
		t.Fatalf("旧 executeReviewPanel 失败: %v", oldErr)
	}
	if newErr != nil {
		t.Fatalf("图路径失败: %v", newErr)
	}
	assertSameSeq(t, "review_panel 旧执行器 vs 独立期望", reviewPanelExpectedSeq, tplStageSeq(oldRes))
	assertSameSeq(t, "review_panel 图路径 vs 独立期望", reviewPanelExpectedSeq, tplStageSeq(newRes))

	oc, _, oldCalls := oldLLM.snapshot()
	nc, _, newCalls := newLLM.snapshot()
	if oc != nc || oc != len(reviewLenses) {
		t.Errorf("LLM 调用次数不等价: 旧=%d 新=%d, 期望均为 %d", oc, nc, len(reviewLenses))
	}
	assertSameSeq(t, "review_panel 每路提示词", ensSortedPrompts(oldCalls), ensSortedPrompts(newCalls))
	want := make([]string, 0, len(reviewLenses))
	for _, lens := range reviewLenses {
		want = append(want, "你是小说评审专家。"+lens+ensembleJSONOnlySuffix+"\x00"+objective)
	}
	sort.Strings(want)
	assertSameSeq(t, "review_panel 提示词 vs 独立期望", want, ensSortedPrompts(newCalls))

	ensAssertSameReview(t, oldRes[0].Output, newRes[0].Output)
	for _, tc := range []struct{ what, out string }{{"旧", oldRes[0].Output}, {"新", newRes[0].Output}} {
		if !strings.HasPrefix(tc.out, "合议评审(3 位, 截尾均值)结果:") {
			t.Errorf("%s路径产出缺少旧格式头 (前 60 字: %q)", tc.what, ensHead(tc.out, 60))
		}
	}
}

// ensAssertSameReview 融合评审产出逐字段比对。
func ensAssertSameReview(t *testing.T, oldOut, newOut string) {
	t.Helper()
	var o, n rvDoc
	if err := json.Unmarshal(ensJSONBody(t, "旧融合产出", oldOut), &o); err != nil {
		t.Fatalf("旧融合产出解析失败: %v", err)
	}
	if err := json.Unmarshal(ensJSONBody(t, "新融合产出", newOut), &n); err != nil {
		t.Fatalf("新融合产出解析失败: %v", err)
	}
	if !ensNearly(o.Overall, n.Overall) {
		t.Errorf("融合 overall 不等价: 旧=%.6f 新=%.6f", o.Overall, n.Overall)
	}
	if !ensNearly(o.Consensus, n.Consensus) {
		t.Errorf("共识度不等价: 旧=%.6f 新=%.6f", o.Consensus, n.Consensus)
	}
	if o.Summary != n.Summary {
		t.Errorf("总评文案不等价:\n旧=%q\n新=%q", o.Summary, n.Summary)
	}
	if len(o.Dimensions) != len(n.Dimensions) {
		t.Fatalf("维度数不等价: 旧=%d 新=%d", len(o.Dimensions), len(n.Dimensions))
	}
	for i := range o.Dimensions {
		od, nd := o.Dimensions[i], n.Dimensions[i]
		if od.Dimension != nd.Dimension {
			t.Errorf("第 %d 个维度名不等价 (维度须按名排序): 旧=%q 新=%q", i+1, od.Dimension, nd.Dimension)
			continue
		}
		if !ensNearly(od.Score, nd.Score) {
			t.Errorf("维度 %q 的截尾均值不等价: 旧=%.9f 新=%.9f", od.Dimension, od.Score, nd.Score)
		}
		if od.Summary != nd.Summary {
			t.Errorf("维度 %q 的评述不等价: 旧=%q 新=%q", od.Dimension, od.Summary, nd.Summary)
		}
	}
	annKeys := func(as []rvAnn) []string {
		out := make([]string, 0, len(as))
		for _, a := range as {
			out = append(out, a.Dimension+"|"+a.Severity+"|"+a.Quote+"|"+a.Note+"|"+a.Suggestion)
		}
		sort.Strings(out) // 批注顺序在旧路径上随完成顺序变
		return out
	}
	assertSameSeq(t, "融合批注 (按集合)", annKeys(o.Annotations), annKeys(n.Annotations))

	// 独立期望: 三位打 80/82/78 的维度, 截尾均值 = 剔最高最低后剩 80
	for _, d := range n.Dimensions {
		if d.Dimension == "plot" && !ensNearly(d.Score, 80) {
			t.Errorf("plot 维截尾均值应为 80 (80/82/78 剔两端), got %.6f", d.Score)
		}
	}
	// overall = 三位 overall (72/78/75) 的中位数 = 75
	if !ensNearly(n.Overall, 75) {
		t.Errorf("融合 overall 应为中位数 75, got %.6f", n.Overall)
	}
}

// TestReviewPanel样本不足时图路径拒绝出数 **唯一一处刻意不等价**, 显式钉住:
// 3 路里挂 1 路时旧路径用普通均值出一个看起来正常的数 (截尾在 2 样本上执行不了),
// 图路径按 MinSamples=3 拒绝出数。这条测试的作用是让将来任何人改动这里都必须先面对它。
func TestReviewPanel样本不足时图路径拒绝出数(t *testing.T) {
	wf := reviewPanelWorkflow()
	mk := func() *ensStubLLM { return &ensStubLLM{out: ensReviewReply, failLens: "严苛"} }
	oldRes, _, _, _, oldErr, newErr, _ := ensRunBothPaths(t, "rv-thin",
		func(we *WorkflowExecutor, team *ProductionTeam) ([]StageResult, error) {
			return we.executeReviewPanel(context.Background(), wf, "请评审", team)
		}, mk)
	if oldErr != nil {
		t.Fatalf("旧路径 2/3 位有效时应照样出数: %v", oldErr)
	}
	if len(oldRes) != 1 || !strings.Contains(oldRes[0].Output, "合议评审(2 位") {
		t.Errorf("旧路径应产出 2 位合议结果, got %q", ensHead(oldRes[0].Output, 60))
	}
	if newErr == nil {
		t.Fatal("图路径应因样本不足 (2<MinSamples=3) 拒绝出数, 而不是静默换成普通均值")
	}
	if !strings.Contains(newErr.Error(), "min_samples=3") {
		t.Errorf("图路径的拒绝原因应写清样本闸, got %v", newErr)
	}
}

// ---------------------------------------------------------------------------
// 三、图形状与预算 (等价性的结构侧前提)
// ---------------------------------------------------------------------------

// TestEnsemble图形状与预算 逐条锁住那些"改了不会让上面的测试红、但会悄悄改行为"的字段。
func TestEnsemble图形状与预算(t *testing.T) {
	cases := []struct {
		kind ensembleGraphKind
		wf   *WorkflowDef
	}{
		{ensembleExtractKind(), graphExtractSwarmWorkflow()},
		{reviewPanelKind(), reviewPanelWorkflow()},
	}
	for _, tc := range cases {
		k := tc.kind
		spec, err := ensembleGraphSpec(tc.wf, k)
		if err != nil {
			t.Fatalf("%s 构图失败: %v", k.Mode, err)
		}
		if err := spec.Validate(); err != nil {
			t.Errorf("%s 的图非法: %v", k.Mode, err)
		}
		// ① 顶层必须只有 1 个节点, 且 ID/角色 = 旧路径的 StageResult.Name/Role
		if len(spec.Nodes) != 1 {
			t.Fatalf("%s 顶层应只有 1 个节点 (否则阶段序列会多出记录), got %d", k.Mode, len(spec.Nodes))
		}
		top := spec.Nodes[0]
		if top.ID != k.GroupID || top.Agent.Role != k.Role || top.Kind != graph.NodeKindLoopGroup {
			t.Errorf("%s 顶层节点应为 loop-group %q/%q, got %q/%q/%s",
				k.Mode, k.GroupID, k.Role, top.ID, top.Agent.Role, top.Kind)
		}
		if top.Group.Loop.MaxIterations != 1 {
			t.Errorf("%s 的组只该跑 1 轮 (它是 subgraph 的替身, 不是循环), got %d",
				k.Mode, top.Group.Loop.MaxIterations)
		}
		if top.Group.ResultFrom != k.ReportNodeID {
			t.Errorf("%s 的组产出应取文本套壳节点 %q, got %q", k.Mode, k.ReportNodeID, top.Group.ResultFrom)
		}
		if top.TimeoutSec != int(k.FanOut.TotalTimeout.Seconds()) {
			t.Errorf("%s 整组预算应 = TotalTimeout %ds, got %d",
				k.Mode, int(k.FanOut.TotalTimeout.Seconds()), top.TimeoutSec)
		}
		// ② 并发上限 = FanOutCollect 的 MaxConcurrency (不能用 effectiveParallel: 那会随流控浮动)
		if spec.Policies.MaxParallel != k.FanOut.MaxConcurrency {
			t.Errorf("%s 并发上限应 = MaxConcurrency %d, got %d",
				k.Mode, k.FanOut.MaxConcurrency, spec.Policies.MaxParallel)
		}
		// ③ 零重试: 图级 + 每个节点 (任何一处漏填都会让一路失败变成 7 次 LLM 调用)
		if spec.Policies.DefaultRetry == nil || spec.Policies.DefaultRetry.MaxRetries != 0 {
			t.Errorf("%s 图级默认重试必须为 0 (旧路径每分支只调一次): %+v", k.Mode, spec.Policies.DefaultRetry)
		}
		var walk func(ns []graph.NodeSpec)
		walk = func(ns []graph.NodeSpec) {
			for _, n := range ns {
				if n.Kind != graph.NodeKindLoopGroup && (n.Retry == nil || n.Retry.MaxRetries != 0) {
					t.Errorf("%s 的节点 %q 未显式声明零重试: %+v", k.Mode, n.ID, n.Retry)
				}
				if n.Agent.Role != k.Role {
					t.Errorf("%s 的节点 %q 角色应为 %q, got %q", k.Mode, n.ID, k.Role, n.Agent.Role)
				}
				if n.Group != nil {
					walk(n.Group.Nodes)
				}
			}
		}
		walk(spec.Nodes)
		// ④ map: 分片数上下限都 = 视角数; 单路预算 = PerBranchTimeout; 来源 = 视角清单节点
		var mapNode *graph.NodeSpec
		for i := range top.Group.Nodes {
			if top.Group.Nodes[i].Kind == graph.NodeKindMap {
				mapNode = &top.Group.Nodes[i]
			}
		}
		if mapNode == nil {
			t.Fatalf("%s 的组内没有 map 节点", k.Mode)
		}
		if mapNode.Map.MaxShards != len(k.Lenses) || mapNode.Map.MinShards != len(k.Lenses) {
			t.Errorf("%s 的分片数上下限应都 = 视角数 %d, got max=%d min=%d",
				k.Mode, len(k.Lenses), mapNode.Map.MaxShards, mapNode.Map.MinShards)
		}
		if want := graph.SourcePrevPrefix + k.LensNodeID; mapNode.Map.Source != want {
			t.Errorf("%s 的 map 来源应为 %q, got %q", k.Mode, want, mapNode.Map.Source)
		}
		if mapNode.TimeoutSec != int(k.FanOut.PerBranchTimeout.Seconds()) {
			t.Errorf("%s 单路预算应 = PerBranchTimeout %ds, got %d",
				k.Mode, int(k.FanOut.PerBranchTimeout.Seconds()), mapNode.TimeoutSec)
		}
		// ⑤ reduce 策略与样本闸
		var reduceNode *graph.NodeSpec
		for i := range top.Group.Nodes {
			if top.Group.Nodes[i].Kind == graph.NodeKindReduce {
				reduceNode = &top.Group.Nodes[i]
			}
		}
		if reduceNode == nil {
			t.Fatalf("%s 的组内没有 reduce 节点", k.Mode)
		}
		if reduceNode.Reduce.Strategy != k.ReduceStrategy || reduceNode.Reduce.MinSamples != k.MinSamples {
			t.Errorf("%s 的聚合策略应为 %s/min_samples=%d, got %s/%d", k.Mode,
				k.ReduceStrategy, k.MinSamples, reduceNode.Reduce.Strategy, reduceNode.Reduce.MinSamples)
		}
		if reduceNode.Reduce.RequireAll {
			t.Errorf("%s 的 reduce 不得 require_all (旧路径 3 路挂 1 路照样融合剩下的)", k.Mode)
		}
	}
}

// TestEnsemble两个mode登记在专属内核表 这两个 mode 必须**不在** ModeHasGraphTemplate 里:
// 命中会让 workflow.go 的灰度分发口把它们劫到 stageNodeRunner (= 悄悄给裸 completion
// 分支发工具权限, 且绕过 LLMClient 前置检查/通知/RewardBus 记账)。
func TestEnsemble两个mode登记在专属内核表(t *testing.T) {
	for _, mode := range []string{"ensemble_extract", "review_panel"} {
		if ModeHasGraphTemplate(mode) {
			t.Errorf("mode %q 不该被 ModeHasGraphTemplate 命中 (会被灰度分发口劫到 stageNodeRunner)", mode)
		}
		if _, ok := modeGraphNativeKernel[mode]; !ok {
			t.Errorf("mode %q 应登记在专属内核表里", mode)
		}
		if _, ok := modeGraphNotTemplated[mode]; ok {
			t.Errorf("mode %q 已图化, 不该还留在未做表里", mode)
		}
		doc, verified := ModeGraphTemplateEquivalence(mode)
		if !verified || strings.TrimSpace(doc) == "" {
			t.Errorf("mode %q 应能查到已验证的等价性说明, got (%q, %v)", mode, ensHead(doc, 40), verified)
		}
	}
}
