package agent

// graph_templates_ensemble —— ensemble_extract / review_panel 两个"合议"mode 的图装配
// (design/01 §五, 承接 M4 orchestrated 那条"专属内核"路线)。
//
// ---------------------------------------------------------------------------
// 这两个 mode 缺的是什么, 现在补上了什么
// ---------------------------------------------------------------------------
//
// 上一轮 modeGraphNotTemplated 给它们记的缺口是**两项**:
//
//	(a) 裸 completion 的 NodeRunner —— 它们的分支不是 ExecuteSingleStage, 而是
//	    we.llm 的一次裸调用 (无工具/无角色模板合并/无黑板交接)。硬塞 stageNodeRunner
//	    等于悄悄给这些分支发了 Bash/Write 权限并改掉提示词;
//	(b) 投票融合 / 截尾均值这两种确定性 reduce 策略 —— 内核当时只有 runner/concat/longest。
//
// (b) 已由 pkg/graph/reduce_fuse.go 补齐 (vote / trimmed_mean)。(a) 由本文件补齐:
// ensembleNodeRunner 是这两个 mode 专属的 NodeRunner, 与 orchNodeRunner 同款做法 ——
// 共用 runGraphSpec 的调度/journal/hook/拦截器/预算, 只换节点执行内核。
//
// ---------------------------------------------------------------------------
// 图的形状: 为什么是 loop-group 包着 map→reduce
// ---------------------------------------------------------------------------
//
//	loop-group "ensemble-extract" (role=graph-swarm, max_iterations=1)
//	  ├─ lenses  (agent, 确定性零 LLM)  产出 = N 行视角清单
//	  ├─ extract (map, source=prev:lenses, split=lines)  每片一路裸 completion
//	  ├─ fuse    (reduce, strategy=vote/trimmed_mean)     引擎直接算出, 零 LLM
//	  └─ report  (agent, 确定性零 LLM)  把融合结果套回旧路径的输出文本格式
//
// 三个"看起来绕"的选择, 每个都是等价性逼出来的:
//
//  1. **外层必须是 loop-group (只跑 1 轮)**, 不能让 map/reduce 直接站在顶层。
//     runGraphSpec 回译 []StageResult 时遍历的是 spec.Nodes (顶层节点), 顶层放
//     map+reduce 就会得到**两条**阶段记录, 而旧执行器返回的是**一条**
//     (Name="ensemble-extract" / Role="graph-swarm")。阶段序列是 REPORT.md / 门禁
//     统计 / dashboard / 下游平台读的东西, 多一条就是行为变更。组把 map→reduce 收进
//     一个顶层节点, 组产出 = ResultFrom 成员产出 —— 这正是 subgraph 形态 (§4.1 预留,
//     尚未实现) 该干的事, 组级循环跑 1 轮是它现有的等价表达。
//     组内成员事件仍进 journal 与 team.Stages 的实时快照 (teamGraphHooks.trackDynamic),
//     于是可观测性反而比旧路径强 —— 旧路径 3 路扇出在 dashboard 上完全看不见。
//  2. **视角清单要有一个自己的节点 (lenses)**, 而不是直接写在 map 策略里。
//     MapPolicy.Source 只有 prev / prev:<id> / param:<键> / objective 四种来源, 没有
//     "字面量集合"这一种; param 来源由 graphRunParams 统一供给 (团队/工作流派生量,
//     不该为一个 mode 塞进去)。用一个确定性零 LLM 节点把 N 条视角吐成 N 行, map 再按
//     行切 —— 视角因此作为**数据**流经图: journal 的 map.expanded 里能直接看到这次
//     扇出用的是哪三条视角, 而不是藏在某个 Go 常量里。
//  3. **report 节点只做文本套壳**。旧路径的产出是
//     "合议抽取(N 路投票)结果:\n\n```json\n<融合 JSON>\n```\n", 而 reduce 的产出是裸
//     JSON (内核不产提示词也不产文案)。少了这层壳, 下游按 "```json" 找产出的平台会
//     读空 —— 那是最典型的"图上跑通了、平台看不见"。
//
// ---------------------------------------------------------------------------
// 刻意保留 / 刻意偏离 (逐条, 因为"少了什么"比"多了什么"更容易埋雷)
// ---------------------------------------------------------------------------
//
//  1. **零重试**: 旧路径 FanOutCollect 对每个分支只调一次 (无重试)。故图级
//     DefaultRetry 与每个节点的 Retry 全部显式填 MaxRetries=0 —— 不填会落到直译器的
//     图层默认 6 次, 一路失败就变成 7 次 LLM 调用, token 账直接翻。
//  2. **超时两层照搬**: 分支 = MapPolicy 所在节点的 TimeoutSec (分片继承它) 对应
//     PerBranchTimeout; 整组 = 组节点 TimeoutSec 对应 TotalTimeout。
//  3. **CompleteDiag 只有 ensemble_extract 用**: 旧 executeEnsembleExtract 在
//     we.llm 是 *api.Client 时走 CompleteDiag 取诊断元数据, executeReviewPanel 一律走
//     SimpleComplete。两者请求体与 max_tokens 完全相同 (见 api/client.go), 但调用形态
//     照抄, 免得"顺手统一"改掉了一条线上的诊断能力。
//  4. **空响应/不可解析的分支仍算 completed**: 旧路径把它们从 docs 里剔掉但不当错误
//     (只进 diag 串)。图上同构 —— 分片 completed, 由 reduce 的样本闸 (fuseParseDocs)
//     剔除并在 Err 里写清原因。判 failed 会让 map 的 shards_failed 与旧路径的语义分叉。
//  5. **融合数值与旧路径的已知差异**, 全部来自内核 reduce_fuse.go 的三处刻意偏离
//     (见该文件头): 缺席维度不算 0 分 / 排序键补齐到全键 / 不做 Level-4 JSON 修复;
//     另外融合 JSON 多了自述字段 samples (与 dimensions[].samples/trimmed)。
//     **review_panel 还有一处必须说清**: 上游 fuseReviews 曾经过 TrimmedFuse 做跨维
//     归一化 (把 0-100 评分当概率分布除以总和), 那是真 bug, 已在 331f79e 改用
//     TrimmedFuseRaw。本文件的等价性基准是**修好之后**的 fuseReviews, 拿修复前的
//     产出做基准会对不上 (差一个 1/Σ 的系数)。
//  6. **docs 顺序**: 旧路径 FanOutCollect 按**完成顺序**追加结果 (且 branches 是 map,
//     goroutine 启动序随机), 于是 fuseExtractions 里"属性/标签取第一个非空"、
//     fuseReviews 里"总评取第一位有 summary 的"这些规则在旧路径上**不确定** ——
//     同一份输入两次运行可能得到不同的融合产出。图路径按分片序 (= 视角声明序) 融合,
//     是确定性的。这是一处**行为改善而非等价**, 记账在此: 灰度打开后这两个 mode 的
//     产出从"随完成顺序抖动"变成"稳定可重放"。
//  7. **黑板**: runGraphSpec 会写 <节点>-result / -status (与 pipeline 对齐), 旧路径
//     不写。属新增可观测数据, 不改任何既有读取方 (它们读的是 StageResult.Output)。
//
// 等价性由 graph_templates_ensemble_test.go 四维比对钉住 (阶段序列 / LLM 调用次数 /
// 峰值并发 / 每路提示词逐字), 外加融合产出逐字段比对与变异反证。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/swarm_intel"
)

// ensembleJSONOnlySuffix 系统提示词末尾的"只输出 JSON"约束 (两个 mode 逐字相同)。
const ensembleJSONOnlySuffix = " 严格按用户要求只输出一个 JSON。"

// ensembleGraphKind 一个合议 mode 的图装配参数。
//
// 做成数据而不是两份代码: 两个 mode 的差异只有 6 处字面量 (节点名/角色/视角表/
// 系统提示词前缀/reduce 策略/输出文案), 抄两遍必然漂移 —— 而漂移的表现是"某一个
// mode 的产出格式悄悄变了", 只有下游平台会先发现。
type ensembleGraphKind struct {
	// Mode 模式名 (仅用于日志与错误文案)。
	Mode string
	// GroupID 顶层组节点 ID = 旧路径 StageResult.Name。
	GroupID string
	// Role 组节点与全部成员的角色 = 旧路径 StageResult.Role。
	Role string
	// LensNodeID / BranchNodeID / FuseNodeID / ReportNodeID 组内成员 ID。
	// 它们会出现在 journal 与 team.Stages 的实时快照里 (<组>#it0/<成员>), 故取可读名。
	LensNodeID   string
	BranchNodeID string
	FuseNodeID   string
	ReportNodeID string
	// Lenses 差异化视角表 (与旧路径同一个包级变量, 不复制字面量)。
	Lenses []string
	// SysPrefix 系统提示词前缀 ("你是小说设定分析师。" / "你是小说评审专家。")。
	SysPrefix string
	// UseCompleteDiag 是否走 *api.Client.CompleteDiag (仅 ensemble_extract)。
	UseCompleteDiag bool
	// Reduce 聚合策略 (vote / trimmed_mean) 与样本下限。
	ReduceStrategy string
	MinSamples     int
	// FanOut 并发/超时口径 (照抄旧路径给 FanOutCollect 的那份 config)。
	FanOut swarm_intel.FanOutConfig
	// OutputFormat 产出文本格式, 两个 %s/%d 依次是 有效路数 与 融合 JSON。
	OutputFormat string
	// StartNotify / DoneNotify 通知文案模板 (与旧路径逐字一致, 飞书侧观感不变)。
	StartNotify string
	// FailErrFormat 无任何可用样本时的错误文案 (%s = 诊断串)。
	FailErrFormat string
}

// ensembleExtractKind graph-extract-swarm (mode=ensemble_extract) 的装配参数。
func ensembleExtractKind() ensembleGraphKind {
	return ensembleGraphKind{
		Mode:            "ensemble_extract",
		GroupID:         "ensemble-extract",
		Role:            "graph-swarm",
		LensNodeID:      "extract-lenses",
		BranchNodeID:    "extract-branch",
		FuseNodeID:      "extract-fuse",
		ReportNodeID:    "extract-report",
		Lenses:          extractLenses,
		SysPrefix:       "你是小说设定分析师。",
		UseCompleteDiag: true,
		ReduceStrategy:  graph.ReduceVote,
		// 与旧路径一致: 1 路有效即出数 (confidence 的分母自述为 samples, 不会伪装成
		// 更强的证据)。不写就落 DefaultVoteMinSamples=1, 显式写是为了让"1 路也出数"
		// 这件事在图里看得见。
		MinSamples:    graph.DefaultVoteMinSamples,
		FanOut:        ensembleFanOutConfig(),
		OutputFormat:  "合议抽取(%d 路投票)结果:\n\n```json\n%s\n```\n",
		StartNotify:   "⚡ **合议抽取**: %d 路差异化视角并行抽取中…",
		FailErrFormat: "合议抽取: 无任何可解析的抽取结果 [%s]",
	}
}

// reviewPanelKind review-panel (mode=review_panel) 的装配参数。
func reviewPanelKind() ensembleGraphKind {
	return ensembleGraphKind{
		Mode:         "review_panel",
		GroupID:      "review-panel",
		Role:         "review-swarm",
		LensNodeID:   "review-lenses",
		BranchNodeID: "review-branch",
		FuseNodeID:   "review-fuse",
		ReportNodeID: "review-report",
		Lenses:       reviewLenses,
		SysPrefix:    "你是小说评审专家。",
		// 旧 executeReviewPanel 只调 SimpleComplete (没有诊断元数据那条线)。
		UseCompleteDiag: false,
		ReduceStrategy:  graph.ReduceTrimmedMean,
		// 生产 reviewLenses 是 3 位, 恰好等于截尾能成立的最小样本数。写死 3 是刻意的:
		// 样本 <3 时"剔最高最低再取均值"执行不了, 内核会拒绝出数而不是静默换算法 ——
		// 旧路径在那种情形下会用普通均值出一个看起来正常的数 (见 reduce_fuse.go 文件头)。
		// 这是**唯一一处刻意不等价**: 3 路里挂 1 路时旧路径出数、图路径拒绝出数。
		MinSamples:    graph.DefaultTrimmedMeanMinSamples,
		FanOut:        swarm_intel.DefaultFanOutConfig(),
		OutputFormat:  "合议评审(%d 位, 截尾均值)结果:\n\n```json\n%s\n```\n",
		StartNotify:   "⚡ **合议评审**: %d 位差异化人格评审并行中…",
		FailErrFormat: "合议评审: 无任何可解析的评审结果 [%s]",
	}
}

// ensembleGraphSpec 按装配参数造图 (形状见文件头)。
//
// 为什么不复用 TranslateWorkflow: 这两个 mode 的 WorkflowDef **声明 0 个 Stages**,
// 直译器无从下手; 而且直译器会应用 per-workflow 覆盖表与关键词 gate 推断, 那两样
// 都能悄悄改掉本图的形状 (与 orchestratedGraphSpec 同款论证)。
func ensembleGraphSpec(wf *WorkflowDef, k ensembleGraphKind) (graph.GraphSpec, error) {
	n := len(k.Lenses)
	if n == 0 {
		return graph.GraphSpec{}, fmt.Errorf("graph_templates_ensemble: %s 的视角表为空", k.Mode)
	}
	// 分支零重试: 旧路径每个分支只调一次。两处 (图级默认 + 节点级) 都填, 否则将来
	// 有人改另一处就会悄悄变成"图层重试 × 分支"的乘法放大 (与 orchestrated 同款纪律)。
	noRetry := &graph.RetryPolicy{MaxRetries: 0}
	det := func(id string) graph.NodeSpec {
		return graph.NodeSpec{
			ID:   id,
			Kind: graph.NodeKindAgent,
			// Deterministic: 本节点零 LLM (视角清单 / 文本套壳)。runner 自解释,
			// 但标出来才能让 hook 载荷与 dashboard 上"这一步不烧钱"这件事看得见。
			Agent:      graph.AgentSpec{Role: k.Role, Deterministic: true},
			Retry:      noRetry,
			TimeoutSec: graphDeterministicBudgetSec,
		}
	}
	branch := graph.NodeSpec{
		ID:    k.BranchNodeID,
		Kind:  graph.NodeKindMap,
		Agent: graph.AgentSpec{Role: k.Role},
		Map: &graph.MapPolicy{
			Source: graph.SourcePrevPrefix + k.LensNodeID,
			Split:  graph.SplitLines,
			// 上下限都取 N: 少一片就是少一路合议 (confidence 的分母变了),
			// 多一片不可能 (视角表是常量)。MinShards 在此的作用是把"视角表被改成
			// 带空行/多行的文案"这种改动挡在开跑之前, 而不是让它静默变成 2 路。
			MaxShards: n,
			MinShards: n,
		},
		Retry: noRetry,
		// 单路超时 = PerBranchTimeout (分片继承 map 节点的 TimeoutSec, 见 shardNodeSpec)。
		TimeoutSec: int(k.FanOut.PerBranchTimeout.Seconds()),
	}
	fuse := graph.NodeSpec{
		ID:    k.FuseNodeID,
		Kind:  graph.NodeKindReduce,
		Agent: graph.AgentSpec{Role: k.Role, Deterministic: true},
		Reduce: &graph.ReducePolicy{
			From:     []string{k.BranchNodeID},
			Strategy: k.ReduceStrategy,
			// RequireAll 必须是 false: 旧路径 3 路里挂 1 路照样融合剩下的两路。
			RequireAll: false,
			MinSamples: k.MinSamples,
		},
		Retry:      noRetry,
		TimeoutSec: graphDeterministicBudgetSec,
	}
	group := graph.NodeSpec{
		ID:    k.GroupID,
		Kind:  graph.NodeKindLoopGroup,
		Agent: graph.AgentSpec{Role: k.Role},
		// 整组预算 = TotalTimeout (旧路径 FanOutCollect 的总超时)。
		TimeoutSec: int(k.FanOut.TotalTimeout.Seconds()),
		Group: &graph.GroupPolicy{
			// 只跑 1 轮: 组在这里的职责是"把 map→reduce 收成一个顶层节点"(见文件头第 1 条),
			// 不是循环。写 1 而不是省略是因为 max_iterations 必填 >0 (无界循环违法)。
			Loop:       graph.LoopPolicy{MaxIterations: 1},
			ResultFrom: k.ReportNodeID,
			Nodes:      []graph.NodeSpec{det(k.LensNodeID), branch, fuse, det(k.ReportNodeID)},
			Edges: []graph.EdgeSpec{
				{From: k.LensNodeID, To: k.BranchNodeID},
				{From: k.BranchNodeID, To: k.FuseNodeID},
				{From: k.FuseNodeID, To: k.ReportNodeID},
			},
		},
	}
	spec := graph.GraphSpec{
		Name:    wf.Name,
		Version: "ensemble-" + k.Mode,
		Meta:    WorkflowGateMeta(wf),
		Policies: graph.GraphPolicies{
			// 并发上限照抄 FanOutCollect 的 MaxConcurrency: 分片是嵌套层叶子, 取 run 级
			// nestSem (容量 = MaxParallel)。用 we.effectiveParallel() 会让并发度随 API
			// 流控在 1..6 之间浮动, 与旧路径固定 4 不同 —— 那是并发度变更。
			MaxParallel:  k.FanOut.MaxConcurrency,
			DefaultRetry: noRetry,
		},
		Nodes: []graph.NodeSpec{group},
	}
	if err := spec.Validate(); err != nil {
		return graph.GraphSpec{}, fmt.Errorf("graph_templates_ensemble: %s 的图非法: %w", k.Mode, err)
	}
	return spec, nil
}

// ---------------------------------------------------------------------------
// 节点执行内核: 裸 completion
// ---------------------------------------------------------------------------

// ensembleNodeRunner 合议 mode 的 graph.NodeRunner 实现。
//
// 三类节点各一段, 按"是不是分片"再按节点 ID 分派 (分片的 node.ID 是 <map>#<序号>,
// 不能用等号匹配)。reduce 节点**不会**到这里 —— vote/trimmed_mean 由引擎直接算出。
type ensembleNodeRunner struct {
	we        *WorkflowExecutor
	team      *ProductionTeam
	objective string
	k         ensembleGraphKind

	mu sync.Mutex
	// diag 每路的诊断串 (按分片序), 复刻旧路径拼给错误文案的那份。
	// 旧路径只在"零可用样本"时把它写进 error, 这里同构 (见 executeEnsembleGraph)。
	diag []string
	// stats 融合结果的统计 (report 节点解析融合 JSON 时填), 供收尾通知复刻旧文案。
	stats ensembleFusedStats
	// gotStats report 节点是否真跑到了 (失败路径下没有统计可报)。
	gotStats bool
}

// ensembleFusedStats 融合产出里通知文案需要的那几个数。
type ensembleFusedStats struct {
	Samples   int     `json:"samples"`
	Overall   float64 `json:"overall"`
	Consensus float64 `json:"consensus"`
	Nodes     []struct {
		Name string `json:"name"`
	} `json:"nodes"`
	Edges []struct {
		Src string `json:"src"`
	} `json:"edges"`
}

var _ graph.NodeRunner = (*ensembleNodeRunner)(nil)

func newEnsembleNodeRunner(we *WorkflowExecutor, team *ProductionTeam, objective string, k ensembleGraphKind) *ensembleNodeRunner {
	return &ensembleNodeRunner{we: we, team: team, objective: objective, k: k,
		diag: make([]string, len(k.Lenses))}
}

// RunNode 见 graph.NodeRunner。
func (r *ensembleNodeRunner) RunNode(ctx context.Context, node graph.NodeSpec, in graph.NodeInput) graph.NodeResult {
	if in.Shard != nil {
		return r.runBranch(ctx, in)
	}
	switch node.ID {
	case r.k.LensNodeID:
		// 视角清单: 一行一条, 交给 map 按行切。零 LLM。
		return graph.NodeResult{Status: graph.NodeStatusCompleted, Output: strings.Join(r.k.Lenses, "\n")}
	case r.k.ReportNodeID:
		return r.runReport(in)
	}
	// 不可达 (图是本文件造的)。**不静默成功**: 静默成功会让一个什么都没做的节点
	// 冒充产出, 表现为"融合结果莫名为空"。
	return graph.NodeResult{Status: graph.NodeStatusFailed,
		Err: fmt.Sprintf("%s: 未知节点 %q (图与 runner 不同源?)", r.k.Mode, node.ID)}
}

// runBranch 一路合议分支 = 一次裸 LLM completion (照抄旧路径的 BranchFunc)。
//
// 系统提示词 = 前缀 + 本路视角 + "只输出一个 JSON"; 用户提示词 = objective。
// 视角来自分片内容 (in.Shard.Value) 而不是 r.k.Lenses[Index]: 那样"图上跑的是哪条
// 视角"才真的由图决定, resume 重放时也不会因为常量表被改而串味。
func (r *ensembleNodeRunner) runBranch(ctx context.Context, in graph.NodeInput) graph.NodeResult {
	if r.we == nil || r.we.llm == nil {
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: r.k.Mode + ": LLMClient 未注入"}
	}
	lens := strings.TrimSpace(in.Shard.Value)
	sys := r.k.SysPrefix + lens + ensembleJSONOnlySuffix
	start := time.Now()

	var (
		text string
		meta string
		err  error
	)
	if cli, ok := r.we.llm.(*api.Client); ok && r.k.UseCompleteDiag {
		text, meta, err = cli.CompleteDiag(ctx, sys, r.objective)
	} else {
		text, err = r.we.llm.SimpleComplete(ctx, sys, r.objective)
	}
	secs := time.Since(start).Seconds()
	bid := fmt.Sprintf("%s-%d", r.k.BranchNodeID, in.Shard.Index)

	switch {
	case err != nil:
		r.setDiag(in.Shard.Index, fmt.Sprintf("%s(%.0fs): err=%v | %s", bid, secs, err, meta))
		return graph.NodeResult{Status: graph.NodeStatusFailed, Err: err.Error()}
	case strings.TrimSpace(text) == "":
		// 空响应: 旧路径不当错误 (只进 diag, 从 docs 里剔掉)。这里同构 —— 判 completed,
		// 由 reduce 的样本闸剔除。判 failed 会让 map 的 shards_failed 与旧路径分叉。
		r.setDiag(in.Shard.Index, fmt.Sprintf("%s(%.0fs): 空响应 | %s", bid, secs, meta))
		return graph.NodeResult{Status: graph.NodeStatusCompleted}
	default:
		r.setDiag(in.Shard.Index, fmt.Sprintf("%s(%.0fs): len=%d | %s", bid, secs, len(text), meta))
		return graph.NodeResult{Status: graph.NodeStatusCompleted, Output: text}
	}
}

// runReport 把融合 JSON 套回旧路径的产出文本格式, 顺手抽出通知要用的统计。
func (r *ensembleNodeRunner) runReport(in graph.NodeInput) graph.NodeResult {
	fused := in.PrevOutputs[r.k.FuseNodeID]
	if strings.TrimSpace(fused) == "" {
		return graph.NodeResult{Status: graph.NodeStatusFailed,
			Err: fmt.Sprintf("%s: 融合节点 %q 没有产出", r.k.Mode, r.k.FuseNodeID)}
	}
	var st ensembleFusedStats
	if err := json.Unmarshal([]byte(fused), &st); err != nil {
		// 内核产出必然是合法 JSON (MarshalIndent 出来的), 走到这里说明上游被改坏了。
		// 不猜: 有效路数无从得知时宁可失败, 也不要在文案里印一个错的"N 路投票"。
		return graph.NodeResult{Status: graph.NodeStatusFailed,
			Err: fmt.Sprintf("%s: 融合产出不是合法 JSON: %v", r.k.Mode, err)}
	}
	r.mu.Lock()
	r.stats, r.gotStats = st, true
	r.mu.Unlock()
	// 有效路数取融合产出自述的 samples (= 旧路径的 len(docs)), 不另数一遍:
	// 数两遍必然在"哪种分片算有效"上与内核的样本闸分叉。
	return graph.NodeResult{Status: graph.NodeStatusCompleted,
		Output: fmt.Sprintf(r.k.OutputFormat, st.Samples, fused)}
}

func (r *ensembleNodeRunner) setDiag(idx int, s string) {
	r.mu.Lock()
	if idx >= 0 && idx < len(r.diag) {
		r.diag[idx] = s
	}
	r.mu.Unlock()
}

// diagLine 按分片序拼出诊断串 (空位跳过, 与旧路径 strings.Join(diag, " | ") 同形)。
func (r *ensembleNodeRunner) diagLine() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := make([]string, 0, len(r.diag))
	for _, d := range r.diag {
		if strings.TrimSpace(d) != "" {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, " | ")
}

func (r *ensembleNodeRunner) fusedStats() (ensembleFusedStats, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats, r.gotStats
}

// ---------------------------------------------------------------------------
// 入口: 灰度开关命中时取代旧执行器
// ---------------------------------------------------------------------------

// executeEnsembleGraph 用图引擎跑一个合议 mode。
//
// 调用点在 workflow_ensemble.go 两个执行器的开头 (灰度开关命中才进来), 与
// executePipeline 的灰度做法一致 —— 开关未设时下面的代码一行都不执行。
func (we *WorkflowExecutor) executeEnsembleGraph(
	ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam, k ensembleGraphKind,
) ([]StageResult, error) {
	ctx, endSpan := logging.WithSpan(ctx, k.Mode+".graph")
	defer endSpan()

	spec, err := ensembleGraphSpec(wf, k)
	if err != nil {
		return nil, err
	}
	notify := func(m string) {
		if team != nil {
			we.notify(team.ChatID, m)
		}
	}
	notify(fmt.Sprintf(k.StartNotify, len(k.Lenses)))

	// runner 在**调用前**就装配好, 而不是像 orchestrated 那样在工厂回调里造:
	// runGraphSpec 有一条"还没调工厂就返回"的路径 (journal 打不开), 那时闭包变量仍是 nil,
	// 后面 runner.fusedStats() 会空指针 panic —— 一个打不开的目录不该把进程带走。
	// 这里能提前造是因为本 runner 不需要最终 spec (它按节点 ID 分派, ID 是常量)。
	runner := newEnsembleNodeRunner(we, team, objective, k)
	results, runErr := we.runGraphSpec(ctx, wf, spec, objective, team,
		func(graph.GraphSpec) graph.NodeRunner { return runner })
	st, ok := runner.fusedStats()
	if runErr != nil || !ok {
		// 失败路径的错误文案与旧路径同形 (含每路诊断串): 少了它, "3 路都废了"这件事
		// 只能靠翻 journal 里每个分片的原文, 而飞书/REPORT.md 上只剩一句"图执行失败"。
		//
		// 再拼上**组内成员**的失败原因: 顶层只有一个组节点, 它的 Err 是组产出节点
		// (文本套壳) 的 Err —— 于是"reduce 因样本不足拒绝出数"这条真正的原因会被
		// 一句"融合节点没有产出"盖掉。那正好是本 mode 唯一一处刻意不等价的地方,
		// 把它盖掉等于让运维完全看不懂为什么灰度打开后这个团队开始失败。
		detail := runner.diagLine()
		if member := ensembleGroupFailure(team, k); member != "" {
			detail += " | 组内失败: " + member
		}
		msg := fmt.Errorf(k.FailErrFormat, detail)
		if runErr != nil {
			return results, fmt.Errorf("%w (图执行: %v)", msg, runErr)
		}
		return results, msg
	}
	notify(k.doneNotify(st))
	if k.Mode == "review_panel" {
		// design/03 §4.2 第 5 行: 融合 overall 结构化入 RewardBus (旧 executeReviewPanel
		// 在这里调同一个函数)。漏掉它的后果是灰度打开后学习侧少一路奖励信号, 而这件事
		// 在阶段序列/产出文本上完全看不出来。
		recordReviewPanelReward(ctx, we.evolution, team, st.Overall, st.Consensus)
	}
	return results, nil
}

// ensembleGroupFailure 取组内成员的失败原因 (最多 3 条, 融合节点优先)。
//
// 数据源是 team.Stages 而不是 runGraphSpec 的返回值: 返回值只含**顶层**节点
// (spec.Nodes), 组内成员是运行期 ID (<组>#it0/<成员>), 只有 teamGraphHooks 的实时
// 快照里有 —— 那份快照本来就是为了让 dashboard 看见扇出而刷的, 这里顺带读它,
// 不必再开一条观测通路。融合节点优先是因为它是"为什么没出数"的答案,
// 而套壳节点的"上游没有产出"只是它的回声。
func ensembleGroupFailure(team *ProductionTeam, k ensembleGraphKind) string {
	if team == nil {
		return ""
	}
	prefix := k.GroupID + graph.GroupIterIDInfix
	team.mu.Lock()
	stages := append([]StageResult(nil), team.Stages...)
	team.mu.Unlock()

	var fuseErr string
	var others []string
	for _, sr := range stages {
		if sr.Status != TaskFailed || !strings.HasPrefix(sr.Name, prefix) || strings.TrimSpace(sr.Error) == "" {
			continue
		}
		member := sr.Name[strings.LastIndex(sr.Name, graph.GroupMemberIDSep)+1:]
		line := member + ": " + sr.Error
		if strings.HasPrefix(member, k.FuseNodeID) {
			fuseErr = line
			continue
		}
		others = append(others, line)
	}
	out := others
	if fuseErr != "" {
		out = append([]string{fuseErr}, others...)
	}
	if len(out) > 3 {
		out = append(out[:3:3], fmt.Sprintf("…另有 %d 条", len(out)-3))
	}
	return strings.Join(out, "; ")
}

// doneNotify 收尾通知文案 (与旧路径逐字一致)。
func (k ensembleGraphKind) doneNotify(st ensembleFusedStats) string {
	if k.ReduceStrategy == graph.ReduceTrimmedMean {
		return fmt.Sprintf("✅ 合议评审完成: %d/%d 位有效 → 融合 overall %.0f, 共识 %.0f%%",
			st.Samples, len(k.Lenses), st.Overall, st.Consensus*100)
	}
	return fmt.Sprintf("✅ 合议抽取完成: %d/%d 路有效 → 融合 %d 节点/%d 边",
		st.Samples, len(k.Lenses), len(st.Nodes), len(st.Edges))
}
