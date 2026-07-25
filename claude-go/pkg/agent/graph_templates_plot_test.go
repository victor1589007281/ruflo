package agent

// graph_templates_plot_test —— plot_simulate / plot_predict 迁到图引擎的等价性验收。
//
// ---------------------------------------------------------------------------
// 四个维度 (照 graph_templates_test.go / graph_templates_ensemble_test.go 的做法)
// ---------------------------------------------------------------------------
//
//	① 阶段序列 (名字 + 角色 + 状态) —— REPORT.md / 门禁统计 / dashboard / 织叙侧
//	   "走向评估"面板读的就是它。这两个 mode 的旧执行器各返回**一条**记录, 图路径的
//	   顶层节点也必须**恰好一个** —— 把引擎内部 Phase 拆成节点会在这一维上立刻红。
//	② LLM 调用次数 —— 一次 Predict 是几十次调用, 图层默认 6 次重试会让 token 账翻七倍;
//	   零重试这件事全靠这一维守住。
//	③ 峰值并发 —— 桩里带真实 sleep。plot_predict 的 3 个分析师由 swarm_intel 自己的
//	   FanOutCollect 并行 (MaxConcurrency=3), 图的并发闸只看到 1 个节点 ——
//	   这一维证明"图不该也没有改掉引擎内部的并行度"。
//	④ 提示词 —— 内核是 swarm_intel 引擎, 提示词变了就是模型产出变了。plot_simulate 按
//	   (sys, user) **逐字**对比; plot_predict 分两半 (原因见下一节): **调用种类的多重集**
//	   (decompose/scout/independent/diversity/debate/summary 各几次) + **确定性那几类的
//	   用户提示词逐字**。
//
// 外加三组: **飞书通知序列逐字**、**产出文本逐字/逐字段比对**、**失败路径的错误文案同形**。
// 通知序列这一维是刻意加的: 它是 TimeoutSec 那条决策唯一可观测的地方 —— Engine.Predict
// 用 ctx.Deadline() 反推预算并把它播成 "⏱️ 预测总预算 15m0s (decompose 1m30s)",
// 图节点一设超时这句就变, 而提示词里看不出来。
//
// ---------------------------------------------------------------------------
// plot_predict 里哪些东西在**旧路径上就是不确定的** (逐条实测)
// ---------------------------------------------------------------------------
//
// 这一节不是给测试找台阶, 是这条测试能不能立住的前提: 拿一个本来就随机的量做等价
// 断言, 得到的是一个**随机红**的测试, 比没有测试更糟 (红了没人信)。
// 下面每条都是把同一份桩输入跑两遍、实测看到差异之后写下的:
//
//  1. **调用记录顺序** —— Engine.predict / debateRound 用 swarm_intel.FanOutCollect 起
//     分析师, 它在各 goroutine 里 `results = append(...)` (fanout.go), 且 branches 是
//     map, 启动序本身随机。
//  2. **参与辩论的角色名单** —— Boids 判定预测过于相似时调 forceDiversity, 而它是
//     `result[len(result)-1] = newPred` (engine.go), 即用"反共识分析师"**顶掉最后完成
//     的那一位**。谁是最后一位取决于 goroutine 调度 ⇒ 实测一次是 {乐观,悲观,反共识}、
//     另一次是 {中立,乐观,反共识}。名单进了辩论提示词的"其他分析师:"块, 也进了产出
//     JSON 的 rationale[]。
//  3. **辩论提示词里的概率串** —— CompressDebateView / `map[...]` 直接打印 map,
//     键序随机 ("主角胜出:50% 反派胜出:30%" 与 "反派胜出:30% 僵局:20% 主角胜出:50%")。
//     forceDiversity 的用户提示词同样嵌了一个 map 打印 (`当前共识: %v`)。
//
// 于是 ④ 在 plot_predict 上分两半:
//
//	确定性的三类 (decompose / scout / independent) —— 用户提示词**逐字**比对。
//	  它们才是"图有没有改掉喂给引擎的东西"的真正证据: objective、结果空间、证据列表、
//	  每位分析师的角色与偏向, 全在这三类里。
//	其余三类 (diversity / debate / summary) —— 只比**调用种类的多重集**(各几次)。
//	  少一次辩论轮 / 多一次强制差异化都会在这一维现形, 而随机的键序不会。
//
// 数值本身仍逐字段比对: 桩让所有分析师给出**完全相同**的概率分布, 于是融合结果与
// "谁被顶掉/谁先完成"都无关, consensus/probability/Lower95/Upper95 全是确定值。
// 理据按"条数 + 去掉 [角色] 前缀后的正文集合"比对 (角色名随第 2 条飘)。
//
// ---------------------------------------------------------------------------
// HOME 隔离: 两条路径必须各自跑在干净的持久化目录上
// ---------------------------------------------------------------------------
//
// swarm_intel.DefaultConfig() 的 DataDir = ~/.claude-go/swarm_intel, 引擎会在那里读写
// 信素 (PheromoneStore) / 历史预测 (PredictionHistory) / 推理库 (ReasoningBank)。
// 先跑旧路径再跑图路径而不换 HOME 的话, 图路径会读到旧路径刚写下的历史先验 ⇒
// 多出一条 "📚 找到 N 条历史推理路径可复用" 的证据、提示词随之变长 ⇒ ④ 必红,
// 而根因与图引擎毫无关系。每条路径一个独立 t.TempDir() 是这条测试成立的前提。
//
// ---------------------------------------------------------------------------
// 变异反证 (五条都真改坏跑过一次, 真实 FAIL 输出见报告)
// ---------------------------------------------------------------------------
//
//	· 节点 TimeoutSec 从 0 改成 600   → 通知序列不等价:
//	                                    旧 "⏱️ 预测总预算 15m0s (decompose 1m30s)"
//	                                    新 "⏱️ 预测总预算 10m0s (decompose 1m0s)"
//	                                    (= 引擎分级预算真被改掉了的可观测证据)
//	                                    并同时红在 "节点 TimeoutSec = 600, 必须为 0"
//	· RunNode 里把引擎调两次           → ② "LLM 调用次数不等价: 旧=10 新=20" +
//	                                    调用种类分布 "debate×3 vs debate×6"
//	· 图里多摆一个节点 (拆 Phase 的形态) → ① "阶段数不等价 旧=1 新=2" +
//	                                    "图节点数 = 2, 期望恰好 1"
//	· RunNode 不回填 Output            → "产出文本不等价" + "产出缺少 ```json 围栏"
//	· 灰度判据挪到 LLMClient 检查之前   → 错误文案从
//	                                    "plot-simulate 需要 LLMClient 以创建 swarm_intel.Engine"
//	                                    变成 "graph_adapter: 图执行失败 (无节点完成)",
//	                                    且 journal 被提前打开

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
)

// ---------------------------------------------------------------------------
// 测试装置
// ---------------------------------------------------------------------------

// plotCall 一次 LLM 调用的 (系统提示词, 用户提示词)。
type plotCall struct{ sys, user string }

// plotStubLLM 可记录调用次数/峰值并发/逐条提示词的桩 LLMClient。
//
// 产出**只依赖 system prompt** 是刻意的 (与 ensStubLLM 同款): 引擎内部并行时
// 调用序不确定, 产出若带序号会让融合结果每次都不同, 数值就没法逐字段比对了。
type plotStubLLM struct {
	delay time.Duration

	mu       sync.Mutex
	calls    []plotCall
	inFlight int
	peak     int
}

func (s *plotStubLLM) SimpleComplete(_ context.Context, sys, user string) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, plotCall{sys: sys, user: user})
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
	return plotStubReply(sys), nil
}

func (s *plotStubLLM) snapshot() (calls, peak int, recorded []plotCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls), s.peak, append([]plotCall(nil), s.calls...)
}

// plotStubOutcomes 预测结果空间 (decompose 产出与各分析师的概率键必须一致,
// 否则 parseAgentPrediction 会把概率全归零, 融合结果就成了退化值)。
var plotStubOutcomes = []string{"主角胜出", "反派胜出", "僵局"}

// plotStubReply 按系统提示词分派桩产出。
//
// 匹配顺序是语义: "预测分析师。输出简洁" (总结) 与 "反共识分析师" 都含 "分析师",
// 必须先判更具体的那条, 否则总结那次调用会返回一段 JSON, 产出文本随之变形。
func plotStubReply(sys string) string {
	switch {
	case strings.Contains(sys, "社会模拟"):
		return `{"scenarios":[` +
			`{"name":"和解","probability":0.6,"description":"两派在第三方斡旋下达成妥协","key_events":["密谈","让步"]},` +
			`{"name":"决裂","probability":0.4,"description":"矛盾激化, 联盟破裂","key_events":["公开对峙"]}],` +
			`"emergent_behaviors":["群体极化","信息级联","非正式领导涌现"],` +
			`"summary":"桩: 社会模拟综合分析"}`
	case strings.Contains(sys, "预测问题设计专家"):
		b, _ := json.Marshal(plotStubOutcomes)
		return `{"question":"故事将走向何方","horizon":"medium","outcome_type":"scenario",` +
			`"outcomes":` + string(b) + `,"context":"桩: 相关背景","constraints":["桩: 约束一"]}`
	case strings.Contains(sys, "信息分析师"):
		return `{"evidence":["桩证据甲","桩证据乙","桩证据丙"]}`
	case strings.Contains(sys, "预测分析师"): // "你是预测分析师。输出简洁、有洞察的总结。"
		return "桩: 群体预测总结正文"
	default: // 三个分析师的独立预测 / 反共识分析师 / 辩论轮更新, 一律同一份分布
		return `{"predictions":{"主角胜出":0.5,"反派胜出":0.3,"僵局":0.2},` +
			`"confidence":0.7,"rationale":"桩: 基于证据的推理","evidence":["桩证据甲"]}`
	}
}

// plotSortedPrompts 把记录的调用压成可比对的有序集合 (引擎内部并行 ⇒ 调用序不确定)。
func plotSortedPrompts(calls []plotCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.sys+"\x00"+c.user)
	}
	sort.Strings(out)
	return out
}

// plotCallKind 一次调用属于哪一类 (按系统提示词判)。
//
// 匹配顺序是语义: 辩论轮的系统提示词是 "你是<角色>。更新预测,只输出JSON。", 而那个
// <角色> 可能正好是"反共识分析师" —— 先判 "反共识分析师" 会把一次辩论调用记成强制
// 差异化, 于是"辩论少跑了一轮"这件事就被一次假的 diversity 抵消掉了。
func plotCallKind(sys string) string {
	switch {
	case strings.Contains(sys, "社会模拟"):
		return "simulate"
	case strings.Contains(sys, "预测问题设计专家"):
		return "decompose"
	case strings.Contains(sys, "信息分析师"):
		return "scout"
	case strings.Contains(sys, "更新预测"):
		return "debate"
	case strings.Contains(sys, "反共识分析师"):
		return "diversity"
	case strings.Contains(sys, "给出独立概率预测"):
		return "independent"
	case strings.Contains(sys, "预测分析师"):
		return "summary"
	}
	return "unknown(" + sys + ")"
}

// plotDeterministicKinds 用户提示词在旧路径上就确定的那几类 (见文件头)。
var plotDeterministicKinds = map[string]bool{"simulate": true, "decompose": true, "scout": true, "independent": true}

// plotKindHistogram 调用种类的多重集 (排序后可直接逐项比对)。
func plotKindHistogram(calls []plotCall) []string {
	n := map[string]int{}
	for _, c := range calls {
		n[plotCallKind(c.sys)]++
	}
	out := make([]string, 0, len(n))
	for k, v := range n {
		out = append(out, fmt.Sprintf("%s×%d", k, v))
	}
	sort.Strings(out)
	return out
}

// plotDeterministicPrompts 确定性那几类的 (sys, user) 逐字, 排序后比对。
func plotDeterministicPrompts(calls []plotCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		if plotDeterministicKinds[plotCallKind(c.sys)] {
			out = append(out, c.sys+"\x00"+c.user)
		}
	}
	sort.Strings(out)
	return out
}

// plotProbe 一次运行的全部观测: LLM 调用 + 飞书通知。
//
// 通知也要收: 它是 TimeoutSec / 引擎配置这类"提示词里看不出来"的改动唯一可观测的地方,
// 也是灰度打开后运维在飞书上直接看到的东西 —— 多一条少一条都是可见的行为变更。
type plotProbe struct {
	llm *plotStubLLM

	mu       sync.Mutex
	notifies []string
}

func newPlotProbe(delay time.Duration) *plotProbe {
	return &plotProbe{llm: &plotStubLLM{delay: delay}}
}

func (p *plotProbe) executor() *WorkflowExecutor {
	return &WorkflowExecutor{llm: p.llm, notify: func(_, msg string) {
		p.mu.Lock()
		p.notifies = append(p.notifies, msg)
		p.mu.Unlock()
	}}
}

func (p *plotProbe) notifySeq() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.notifies...)
}

// plotRunBothPaths 用同一份桩产出各跑一遍旧路径与图路径。
//
// 两条路径各自一个 HOME (见文件头): 共用 HOME 会让第二条路径读到第一条写下的
// 历史先验, 提示词随之变长 —— 那是一条与图引擎无关的假红。
func plotRunBothPaths(t *testing.T, name string, delay time.Duration,
	run func(*WorkflowExecutor, *ProductionTeam) ([]StageResult, error),
) (oldRes, newRes []StageResult, oldP, newP *plotProbe, oldErr, newErr error, newTeam *ProductionTeam) {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "") // 显式清空: 不受外部环境影响
	oldP = newPlotProbe(delay)
	oldTeam := newStubTeam(t, name+"-old")
	oldRes, oldErr = run(oldP.executor(), oldTeam)
	if _, err := os.Stat(filepath.Join(oldTeam.dataDir, "graph-journal")); !os.IsNotExist(err) {
		t.Fatalf("开关未设时不该产生 graph-journal (说明默认行为已被改成图路径): stat err=%v", err)
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")
	newP = newPlotProbe(delay)
	newTeam = newStubTeam(t, name+"-new")
	newRes, newErr = run(newP.executor(), newTeam)
	return
}

// plotJSONBody 从产出文本里取出 JSON (两条路径的产出都是 "…结果（…）:\n\n```json\n{…}\n```\n")。
func plotJSONBody(t *testing.T, what, out string) []byte {
	t.Helper()
	i := strings.Index(out, "{")
	j := strings.LastIndex(out, "}")
	if i < 0 || j <= i {
		t.Fatalf("%s: 产出里找不到 JSON 对象 (前 120 字: %q)", what, ensHead(out, 120))
	}
	return []byte(out[i : j+1])
}

// ---------------------------------------------------------------------------
// 一、plot_simulate
// ---------------------------------------------------------------------------

// plotSimulateExpectedSeq 旧执行器真实产出的阶段序列 (写死为**独立**期望:
// 若两条路径同时被改坏, 只比"两边相等"是发现不了的)。
var plotSimulateExpectedSeq = []string{"plot-simulate|plot-simulator|completed"}

// TestPlotSimulate图内核四维等价 ①②③④ + 产出逐字。
func TestPlotSimulate图内核四维等价(t *testing.T) {
	wf := plotSimulateWorkflow()
	const objective = "第三章 张三与李四决裂"
	oldRes, newRes, oldP, newP, oldErr, newErr, newTeam := plotRunBothPaths(t, "plot-sim", 0,
		func(we *WorkflowExecutor, team *ProductionTeam) ([]StageResult, error) {
			return we.executePlotSimulate(context.Background(), wf, objective, team)
		})
	if oldErr != nil {
		t.Fatalf("旧 executePlotSimulate 失败: %v", oldErr)
	}
	if newErr != nil {
		t.Fatalf("图路径失败: %v", newErr)
	}
	// 硬证据: 图路径一定会开 journal
	if _, err := os.Stat(filepath.Join(newTeam.dataDir, "graph-journal")); err != nil {
		t.Fatalf("开关打开后应经图引擎跑 (graph-journal 应存在): %v", err)
	}

	// ① 阶段序列
	assertSameSeq(t, "plot_simulate 旧执行器 vs 独立期望", plotSimulateExpectedSeq, tplStageSeq(oldRes))
	assertSameSeq(t, "plot_simulate 图路径 vs 独立期望", plotSimulateExpectedSeq, tplStageSeq(newRes))

	// ② LLM 调用次数: social 模拟是**单 prompt** 形态, 恰好一次。
	oc, oPeak, oldCalls := oldP.llm.snapshot()
	nc, nPeak, newCalls := newP.llm.snapshot()
	if oc != nc || oc != 1 {
		t.Errorf("LLM 调用次数不等价: 旧=%d 新=%d, 期望均为 1 (零重试是这一维守住的)", oc, nc)
	}
	// ③ 峰值并发 (单 prompt ⇒ 恒 1; 这一维在 simulate 上是"不该被图引擎并行化"的守卫)
	if oPeak != nPeak {
		t.Errorf("峰值并发不等价: 旧=%d 新=%d", oPeak, nPeak)
	}
	// ④ 提示词逐字 (simulate 全程串行, 不存在任何顺序不确定性)
	assertSameSeq(t, "plot_simulate 提示词", plotSortedPrompts(oldCalls), plotSortedPrompts(newCalls))
	// 飞书通知序列逐字 (起始/引擎进度/收尾三类都必须一字不差)
	assertSameSeq(t, "plot_simulate 通知序列", oldP.notifySeq(), newP.notifySeq())
	if len(newCalls) == 1 {
		// 独立期望: 系统提示词与 Agents=3/Rounds=2 这两个参数必须原样传下去
		if newCalls[0].sys != "你是社会模拟与群体行为专家。只输出JSON。" {
			t.Errorf("系统提示词被改: %q", newCalls[0].sys)
		}
		for _, want := range []string{objective, "Agent 数量: 3", "模拟轮数: 2"} {
			if !strings.Contains(newCalls[0].user, want) {
				t.Errorf("用户提示词缺少 %q (前 200 字: %q)", want, ensHead(newCalls[0].user, 200))
			}
		}
	}

	// 产出文本**逐字**相同 (simulate 没有并行, 不存在顺序不确定性)
	if oldRes[0].Output != newRes[0].Output {
		t.Errorf("产出文本不等价:\n旧: %q\n新: %q", ensHead(oldRes[0].Output, 300), ensHead(newRes[0].Output, 300))
	}
	if !strings.HasPrefix(newRes[0].Output, "剧情模拟结果（2 个情景）:") {
		t.Errorf("产出缺少旧格式头 (前 60 字: %q)", ensHead(newRes[0].Output, 60))
	}
	if !strings.Contains(newRes[0].Output, "```json") {
		t.Error("产出缺少 ```json 围栏 (织叙侧 parseSimulation 按它取产出)")
	}
	var sim plotSimJSON
	if err := json.Unmarshal(plotJSONBody(t, "图路径", newRes[0].Output), &sim); err != nil {
		t.Fatalf("图路径产出不是合法 JSON: %v", err)
	}
	if len(sim.Scenarios) != 2 || len(sim.Emergent) != 3 || sim.Mode != "social" {
		t.Errorf("产出 JSON 结构变了: mode=%q 情景=%d 涌现=%d", sim.Mode, len(sim.Scenarios), len(sim.Emergent))
	}
}

// ---------------------------------------------------------------------------
// 二、plot_predict
// ---------------------------------------------------------------------------

var plotPredictExpectedSeq = []string{"plot-predict|plot-analyst|completed"}

// TestPlotPredict图内核四维等价 ①②③④ + 融合数值逐字段。
//
// 这条比 simulate 重得多 (一次 Predict = decompose + scout + 3 分析师 + 可能的
// 强制差异化 + 辩论 + 总结), 但正因为重, 它才是"零重试"与"不设超时"两条决策的真验收。
func TestPlotPredict图内核四维等价(t *testing.T) {
	wf := plotPredictWorkflow()
	const objective = "评估三个候选走向的可信度"
	// 带真实 sleep: 让 3 个分析师的并行在时间上可分辨 (③ 这一维没有 sleep 就是空话)。
	oldRes, newRes, oldProbe, newProbe, oldErr, newErr, newTeam := plotRunBothPaths(t, "plot-pred", 20*time.Millisecond,
		func(we *WorkflowExecutor, team *ProductionTeam) ([]StageResult, error) {
			return we.executePlotPredict(context.Background(), wf, objective, team)
		})
	if oldErr != nil {
		t.Fatalf("旧 executePlotPredict 失败: %v", oldErr)
	}
	if newErr != nil {
		t.Fatalf("图路径失败: %v", newErr)
	}
	if _, err := os.Stat(filepath.Join(newTeam.dataDir, "graph-journal")); err != nil {
		t.Fatalf("开关打开后应经图引擎跑 (graph-journal 应存在): %v", err)
	}

	// ①
	assertSameSeq(t, "plot_predict 旧执行器 vs 独立期望", plotPredictExpectedSeq, tplStageSeq(oldRes))
	assertSameSeq(t, "plot_predict 图路径 vs 独立期望", plotPredictExpectedSeq, tplStageSeq(newRes))

	oc, oPeak, oldCalls := oldProbe.llm.snapshot()
	nc, nPeak, newCalls := newProbe.llm.snapshot()
	// ② 次数必须相等; 且必须**大于分析师数**, 否则说明桩把引擎打成了退化路径,
	//    这条测试就退化成"两个都没跑"的空转等价。
	if oc != nc {
		t.Errorf("LLM 调用次数不等价: 旧=%d 新=%d (图层重试会在这一维立刻现形)", oc, nc)
	}
	if oc <= 3 {
		t.Errorf("旧路径只调了 %d 次 LLM, 说明引擎走了退化路径, 本测试失去意义", oc)
	}
	// ③ 峰值并发: 引擎自己的 FanOutCollect(MaxConcurrency=3) 决定, 图只看到 1 个节点。
	if oPeak < 2 {
		t.Errorf("旧路径峰值并发 = %d, 期望 >= 2 (3 个分析师应并行, 否则 ③ 这一维没验到东西)", oPeak)
	}
	if oPeak != nPeak {
		t.Errorf("峰值并发不等价: 旧=%d 新=%d (图引擎不该改掉 swarm 内部并行度)", oPeak, nPeak)
	}
	// ④-上半: 调用种类多重集 (少一轮辩论 / 多一次强制差异化都在这一维现形)
	assertSameSeq(t, "plot_predict 调用种类分布", plotKindHistogram(oldCalls), plotKindHistogram(newCalls))
	// ④-下半: 确定性那几类的提示词**逐字** (objective/结果空间/证据/角色偏向全在这里)
	oldDet, newDet := plotDeterministicPrompts(oldCalls), plotDeterministicPrompts(newCalls)
	if len(newDet) < 5 { // decompose 1 + scout 1 + independent 3
		t.Errorf("确定性提示词只收到 %d 条, 期望 >= 5 —— 这一维等于没验", len(newDet))
	}
	assertSameSeq(t, "plot_predict 确定性提示词逐字", oldDet, newDet)
	// 飞书通知序列逐字: TimeoutSec 那条决策唯一可观测的地方 (见文件头)
	assertSameSeq(t, "plot_predict 通知序列", oldProbe.notifySeq(), newProbe.notifySeq())

	// 产出: 外壳 + 数值逐字段 (列表按集合, 见文件头)
	for _, tc := range []struct{ what, out string }{{"旧", oldRes[0].Output}, {"新", newRes[0].Output}} {
		if !strings.HasPrefix(tc.out, "走向评估结果（共识度 ") {
			t.Errorf("%s路径产出缺少旧格式头 (前 60 字: %q)", tc.what, ensHead(tc.out, 60))
		}
		if !strings.Contains(tc.out, "```json") {
			t.Errorf("%s路径产出缺少 ```json 围栏", tc.what)
		}
	}
	var oldP, newP plotPredictJSON
	if err := json.Unmarshal(plotJSONBody(t, "旧路径", oldRes[0].Output), &oldP); err != nil {
		t.Fatalf("旧路径产出不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal(plotJSONBody(t, "图路径", newRes[0].Output), &newP); err != nil {
		t.Fatalf("图路径产出不是合法 JSON: %v", err)
	}
	if !ensNearly(oldP.Consensus, newP.Consensus) {
		t.Errorf("共识度不等价: 旧=%v 新=%v", oldP.Consensus, newP.Consensus)
	}
	if oldP.Summary != newP.Summary {
		t.Errorf("总结不等价:\n旧=%q\n新=%q", ensHead(oldP.Summary, 120), ensHead(newP.Summary, 120))
	}
	assertSameSeq(t, "plot_predict 走向分布", plotOutcomeKeys(oldP), plotOutcomeKeys(newP))
	// 理据: 按"条数 + 去掉 [角色] 前缀后的正文"比对 (角色集合本身在旧路径上就不确定,
	// 见文件头 forceDiversity 那条)。这一维守的是"理据整段没了/被截断"。
	assertSameSeq(t, "plot_predict 分析师理据正文", plotRationaleBodies(oldP), plotRationaleBodies(newP))
	if len(newP.Rationale) != 3 {
		t.Errorf("理据条数 = %d, 期望 3 (3 位分析师各一条; 少了说明产出被截或分析师挂了)", len(newP.Rationale))
	}
	// 头部文案里的共识度百分比也必须一致 (它是织叙侧直接展示给作者的那个数)
	if oldHead, newHead := plotHeadLine(oldRes[0].Output), plotHeadLine(newRes[0].Output); oldHead != newHead {
		t.Errorf("产出头部文案不等价: 旧=%q 新=%q", oldHead, newHead)
	}
}

// plotOutcomeKeys 把走向分布压成 "名称|概率|下界|上界" 的有序集合。
func plotOutcomeKeys(p plotPredictJSON) []string {
	out := make([]string, 0, len(p.Outcomes))
	for _, o := range p.Outcomes {
		out = append(out, fmt.Sprintf("%s|%.9f|%.9f|%.9f", o.Outcome, o.Probability, o.Lower95, o.Upper95))
	}
	sort.Strings(out)
	return out
}

// plotRationaleBodies 去掉 "[角色] " 前缀后的理据正文 (排序)。
func plotRationaleBodies(p plotPredictJSON) []string {
	out := make([]string, 0, len(p.Rationale))
	for _, r := range p.Rationale {
		if i := strings.Index(r, "] "); i >= 0 {
			r = r[i+2:]
		}
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// plotHeadLine 产出文本的第一行。
func plotHeadLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// 三、灰度与失败路径
// ---------------------------------------------------------------------------

// TestPlotSwarm无LLMClient时错误文案不随灰度改变 LLMClient 前置检查必须**在**灰度判据
// 之前 —— 否则同一个配置错误在开关两侧会报出两句不同的话 (一句是执行器的自我介绍,
// 另一句是图引擎的 "LLMClient 未注入"), 运维排障时会以为是两个故障。
func TestPlotSwarm无LLMClient时错误文案不随灰度改变(t *testing.T) {
	for _, tc := range []struct {
		name, envVal, wantMsg string
		run                   func(*WorkflowExecutor, *ProductionTeam) ([]StageResult, error)
	}{
		{"simulate-关", "", "plot-simulate 需要 LLMClient 以创建 swarm_intel.Engine", nil},
		{"simulate-开", "1", "plot-simulate 需要 LLMClient 以创建 swarm_intel.Engine", nil},
		{"predict-关", "", "plot-predict 需要 LLMClient 以创建 swarm_intel.Engine", nil},
		{"predict-开", "1", "plot-predict 需要 LLMClient 以创建 swarm_intel.Engine", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_GO_GRAPH_ENGINE", tc.envVal)
			we := &WorkflowExecutor{notify: func(_, _ string) {}} // llm 恒 nil
			team := newStubTeam(t, "plot-nollm-"+tc.name)
			var err error
			if strings.HasPrefix(tc.name, "simulate") {
				_, err = we.executePlotSimulate(context.Background(), plotSimulateWorkflow(), "目标", team)
			} else {
				_, err = we.executePlotPredict(context.Background(), plotPredictWorkflow(), "目标", team)
			}
			if err == nil || err.Error() != tc.wantMsg {
				t.Fatalf("错误文案 = %v, 期望 %q", err, tc.wantMsg)
			}
			if _, statErr := os.Stat(filepath.Join(team.dataDir, "graph-journal")); !os.IsNotExist(statErr) {
				t.Error("LLMClient 缺失时不该已经开了 journal (说明灰度判据跑在检查之前)")
			}
		})
	}
}

// TestPlotSwarm引擎失败时错误文案同形 引擎报错时两条路径都必须返回
// (nil, "剧情模拟失败: …") —— 图路径若把它换成图引擎的通用文案, 织叙侧的错误提示
// 会从"剧情模拟失败: 上游 429"退化成一句无信息量的"graph_adapter: 图执行失败"。
//
// **为什么注入 Run 而不是让桩 LLM 全路失败**: swarm_intel 的 ResilientCaller 对每次
// 调用做 4 次带抖动退避的重试, 一条失败路径实测 14 秒 × 两条路径 = 28 秒。那 28 秒
// 全花在 swarm_intel 内部, 两条路径逐字相同, 一点覆盖率都不增加 —— 而 pkg/agent 的
// -race 全量测试本身才 23 秒左右。注入 Run 把验证点收窄到真正要证的那一处:
// **入口如何把内核错误映射成返回值**。
func TestPlotSwarm引擎失败时错误文案同形(t *testing.T) {
	boom := fmt.Errorf("剧情模拟失败: %w", fmt.Errorf("桩: 上游 429"))
	k := plotSimulateKind()
	k.Run = func(context.Context, *swarm_intel.Engine, string, string) (string, string, error) {
		return "", "", boom
	}
	wf := plotSimulateWorkflow()
	we := newPlotProbe(0).executor()

	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "")
	oldRes, oldErr := we.executePlotSwarm(context.Background(), wf, "目标", newStubTeam(t, "plot-fail-old"), k)

	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")
	newTeam := newStubTeam(t, "plot-fail-new")
	newRes, newErr := we.executePlotSwarm(context.Background(), wf, "目标", newTeam, k)

	if oldErr == nil || newErr == nil {
		t.Fatalf("两条路径都应报错: 旧=%v 新=%v", oldErr, newErr)
	}
	if oldErr.Error() != newErr.Error() {
		t.Errorf("错误文案不等价:\n旧=%q\n新=%q", oldErr.Error(), newErr.Error())
	}
	if !strings.HasPrefix(newErr.Error(), "剧情模拟失败: ") {
		t.Errorf("图路径错误文案被图引擎的通用文案盖掉了: %q", newErr.Error())
	}
	if len(oldRes) != 0 || len(newRes) != 0 {
		t.Errorf("失败时不该返回半截阶段: 旧=%d 条 新=%d 条", len(oldRes), len(newRes))
	}
	// 失败也必须真的经过图引擎 (否则这条测试可能是在比较两次旧路径)
	if _, err := os.Stat(filepath.Join(newTeam.dataDir, "graph-journal")); err != nil {
		t.Errorf("图路径失败时也应留下 journal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 四、图形状: 两条硬约束钉在这里
// ---------------------------------------------------------------------------

// TestPlotSwarm图形状零重试且不设超时 这两条都不是"省事", 是行为约束:
//
//	TimeoutSec>0 ⇒ Engine.Predict 用 ctx.Deadline() 反推自己的分级预算,
//	              设**任何**超时 (哪怕更宽松) 都会改掉引擎内部各 Phase 的子预算;
//	MaxRetries>0 ⇒ 一次 Predict 是几十次 LLM 调用, 重试一轮就是成倍的 token。
func TestPlotSwarm图形状零重试且不设超时(t *testing.T) {
	for _, k := range []plotGraphKind{plotSimulateKind(), plotPredictKind()} {
		wf := &WorkflowDef{Name: k.NodeID, Mode: k.Mode}
		spec, err := plotSwarmGraphSpec(wf, k)
		if err != nil {
			t.Fatalf("%s 构图失败: %v", k.Mode, err)
		}
		if err := spec.Validate(); err != nil {
			t.Errorf("%s 的图非法: %v", k.Mode, err)
		}
		if len(spec.Nodes) != 1 {
			t.Fatalf("%s 图节点数 = %d, 期望恰好 1 (拆 Phase 会让阶段序列从 1 条变成 N 条)", k.Mode, len(spec.Nodes))
		}
		n := spec.Nodes[0]
		if n.ID != k.NodeID || n.Agent.Role != k.Role || n.Kind != graph.NodeKindAgent {
			t.Errorf("%s 节点声明不对: id=%q role=%q kind=%q", k.Mode, n.ID, n.Agent.Role, n.Kind)
		}
		if n.TimeoutSec != 0 {
			t.Errorf("%s 节点 TimeoutSec = %d, 必须为 0 (见 graph_templates_plot.go 文件头第 2 条)", k.Mode, n.TimeoutSec)
		}
		if n.Retry == nil || n.Retry.MaxRetries != 0 {
			t.Errorf("%s 节点重试未显式置零: %+v", k.Mode, n.Retry)
		}
		if spec.Policies.DefaultRetry == nil || spec.Policies.DefaultRetry.MaxRetries != 0 {
			t.Errorf("%s 图级默认重试未显式置零: %+v", k.Mode, spec.Policies.DefaultRetry)
		}
		if spec.Version != "plot-"+k.Mode {
			t.Errorf("%s 图版本标记 = %q, 期望 plot-%s", k.Mode, spec.Version, k.Mode)
		}
	}
	if _, err := plotSwarmGraphSpec(nil, plotSimulateKind()); err == nil {
		t.Error("空工作流应报错, 而不是产出一张无名图")
	}
}

// TestPlotSwarmRunner拒绝未知节点 图与 runner 不同源时必须失败而不是静默成功 ——
// 静默成功会让一个什么都没做的节点冒充产出 (表现为"跑完了但产出是空的")。
func TestPlotSwarmRunner拒绝未知节点(t *testing.T) {
	r := newPlotSwarmNodeRunner(newPlotProbe(0).executor(), newStubTeam(t, "plot-unknown"), "目标", plotSimulateKind())
	res := r.RunNode(context.Background(), graph.NodeSpec{ID: "别的节点"}, graph.NodeInput{})
	if res.Status != graph.NodeStatusFailed || !strings.Contains(res.Err, "未知节点") {
		t.Errorf("未知节点应失败, got status=%q err=%q", res.Status, res.Err)
	}
	// 无 LLMClient 时同样不得静默成功
	r2 := newPlotSwarmNodeRunner(&WorkflowExecutor{notify: func(_, _ string) {}}, newStubTeam(t, "plot-nil-llm"), "目标", plotSimulateKind())
	res2 := r2.RunNode(context.Background(), graph.NodeSpec{ID: "plot-simulate"}, graph.NodeInput{})
	if res2.Status != graph.NodeStatusFailed || !strings.Contains(res2.Err, "LLMClient") {
		t.Errorf("LLMClient 缺失应失败, got status=%q err=%q", res2.Status, res2.Err)
	}
}
