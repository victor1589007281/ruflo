// reward_sources.go —— RewardBus 的奖励源接线 + gate Span 采集
// (design/03 §4.1 第 5 种 Kind、§4.2 奖励源接线清单)。
//
// # 这个文件补的两个洞
//
// ① **gate Span 从不写**: §4.1 要求五种 Kind, 生产只写出 tool_call/turn/node。
// 缺的 gate 只能在这一层写 —— 门禁跑在团队层 (团队 cwd 里真跑 go build/go test、
// 或对最终产出做一次内容评审), pkg/engine 完全看不到它。gate Span 与 gate 奖励
// **同一处发出**, 保证"奖励有值但轨迹里查不到它凭什么"这种断链不会发生。
//
// ② **奖励源只有 3/8**: 设计按价值列了 8 行奖励源, 生产只接了门禁与内容评审。
// 本文件把三个**真实已存在但被丢弃**的信号接上:
//
//   - user.steer  ← RefineTeam: 用户主动要求精修, 定义上就是"上一轮产出不达标"
//     的显式人类判断。此前只写进 RefineHistory 供人看, 不入学习。
//   - review.panel ← review-panel 工作流融合后的 overall 分 (0-100, 截尾均值抗离群)。
//     此前只 MarshalIndent 进 stage 产出文本, 学习侧要重新解析散文才能拿到。
//   - latency     ← 团队真实执行时长。设计 §4.2 第 8 行明确要求 cost/latency 作
//     shaping 负项防"堆 token 刷分"; 时长是当前唯一在团队层零成本可得的真实计量。
//
// 没接的三行 (e2e / verdict.heuristic / cost) 在报告里如实标未做, 不凑数。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

// 新增奖励源名 (格式即契约, 与 RewardSourceWeight 的权重表对应)。
const (
	RewardSourceUserSteer    = "user.steer"    // 用户精修 = 对上一轮的负反馈
	RewardSourceUserExplicit = "user.explicit" // 用户显式评分 (/team rate)
	RewardSourceReviewPanel  = "review.panel"  // 评审团融合分
	RewardSourceLatency      = "latency"       // 时长 shaping 负项
)

// ---------------------------------------------------------------------------
// gate Span (design/03 §4.1 Kind=gate)
// ---------------------------------------------------------------------------

// gateSpanInput 一次门禁判定的轨迹素材。
type gateSpanInput struct {
	Gate    string  // 门禁名 (= 奖励源名, 如 gate.compile)
	Node    string  // 被判定的节点/阶段; 全局门禁留空
	Input   string  // 被判定的对象 (产出正文 / 工作目录)
	Detail  string  // 判定明细 (编译报错全文 / 评审 issues)
	Score   float64 // 归一化 [-1,1], 与奖励值同源
	Raw     any     // 原始值 (0-100 分 / pass / 报错摘要)
	Pass    bool
	Start   time.Time
	Skipped bool // 门禁未真跑 (无 go.mod / 无 cwd): 记轨迹但不发奖励
}

// writeGateSpan 写一条 gate Span。TraceStore 未装配时零成本 no-op。
//
// 为什么连"跳过"也写: §4.5 的教训是"静默跳过会让人以为门禁过了"。轨迹里留一条
// skipped=true 的 gate Span, 事后能分清"编译过了"与"根本没编译"。奖励侧仍然不发
// (没跑过就不是证据), 两者不矛盾 —— 轨迹记事实, 奖励记证据。
func (ptm *ProductionTeamManager) writeGateSpan(ctx context.Context, team *ProductionTeam, in gateSpanInput) {
	if ptm == nil || ptm.traceStore == nil || team == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := trace.From(ctx)
	start := in.Start
	if start.IsZero() {
		start = time.Now()
	}
	attrs := map[string]any{
		"gate":   in.Gate,
		"pass":   in.Pass,
		"score":  in.Score,
		"team":   team.Name,
		"status": gateStatusLabel(in),
	}
	if in.Raw != nil {
		attrs["raw"] = in.Raw
	}
	ptm.traceStore.Write(tracestore.Span{
		TraceID:   ids.RunID,
		SpanID:    newSpanID(),
		ParentID:  ids.TurnID,
		Kind:      tracestore.KindGate,
		Name:      in.Gate,
		NodeID:    firstNonEmpty(in.Node, ids.NodeID),
		TurnID:    ids.TurnID,
		InputRef:  ptm.traceStore.MakeRef(in.Input),
		OutputRef: ptm.traceStore.MakeRef(in.Detail),
		Attrs:     attrs,
		TS:        start.UnixMilli(),
		DurMS:     time.Since(start).Milliseconds(),
	})
}

func gateStatusLabel(in gateSpanInput) string {
	switch {
	case in.Skipped:
		return "skipped"
	case in.Pass:
		return "pass"
	default:
		return "fail"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// newSpanID 生成 span id。刻意不引 uuid 依赖: trace 包已有 ID 生成器, 且 span id
// 只需在一个 run 内唯一。
func newSpanID() string { return trace.NewID("s") }

// ---------------------------------------------------------------------------
// user.steer: 精修 = 用户对上一轮的负反馈 (design/03 §4.2 第 4 行)
// ---------------------------------------------------------------------------

// steerRewardValue 精修负信号的强度。
//
// 为什么是 -0.5 而不是 -1: 用户来精修**不等于**上一轮全错 —— 大多数精修是"方向对
// 但要补/要改口径"。给满负分会把"基本可用但需润色"和"彻底跑偏"混成一档, 而后者
// 本来就有 episode=-1 与门禁负分在承担。取半档, 让它成为可叠加的弱负信号。
//
// 权重侧 (RewardSourceWeight) 给 user.steer 0.8 —— 人的判断可信度高。
const steerRewardValue = -0.5

// recordSteerReward 把一次精修记成对被纠偏范围的负奖励 (design/03 §4.2 "steer 修正")。
//
// 归因粒度: 指定了 targetStage 就挂到该阶段 (用户明确说了是哪一段不行);
// 未指定 (整体重跑) 则挂 run 级 —— 不猜某个阶段, 猜错会污染无关阶段的学习反馈。
//
// RunID 取自团队上一轮执行的 run: 精修发生在**上一轮结束之后**, 若用"当前"的
// trace (RefineTeam 是同步方法, ctx 里根本没有 run) 会得到空 RunID, 奖励落盘后
// 永远聚合不到 —— 这正是"写了但聚合不到"的经典失效, 必须显式带上一轮的 RunID。
func (ptm *ProductionTeamManager) recordSteerReward(team *ProductionTeam, runID, feedback, targetStage string) {
	if ptm == nil || ptm.evolution == nil || team == nil {
		return
	}
	if strings.TrimSpace(runID) == "" {
		return // 无 run 归因的奖励是死数据, 不写
	}
	ptm.evolution.RecordReward(RewardEvent{
		RunID:  runID,
		NodeID: targetStage,
		Source: RewardSourceUserSteer,
		Value:  steerRewardValue,
		Raw:    truncateResult(feedback, 300),
		Team:   team.Name,
	})
}

// ---------------------------------------------------------------------------
// user.explicit: 用户显式评分 (design/03 §4.2 第 3 行, "最高价值信号为零" 的修复)
// ---------------------------------------------------------------------------

// RateTeam 记录用户对某团队产出的显式评分 (1-5 星)。
//
// 这是设计里价值最高、此前**完全缺失**的信号源: 没有任何入口能让人告诉系统
// "这次做得好/不好"。1-5 星线性映射到 [-1,1] (1★→-1, 3★→0, 5★→+1) ——
// 3 星是"可用但平庸", 映射到 0 正好表示"不作为正例也不作为负例"。
//
// 返回错误而不是静默忽略: 评分是人主动给的, 拿不到 run 或团队不存在时必须让人知道
// (静默丢弃人类反馈是最不该做的 fail-open)。
// LastRunForChat 找某会话最近一个团队的已完成 run (steer 奖励挂点用)。
// 没有则 ok=false——纯聊天会话不产生 steer 奖励, 这是刻意的: 没有运行上下文
// 的打断不是"对某次运行的修正", 记了只会污染奖励流。
func (ptm *ProductionTeamManager) LastRunForChat(chatID string) (teamName, runID string, ok bool) {
	if ptm == nil {
		return "", "", false
	}
	ptm.mu.RLock()
	defer ptm.mu.RUnlock()
	for _, t := range ptm.teams {
		if t.ChatID != chatID {
			continue
		}
		t.mu.Lock()
		rid := t.LastRunID
		t.mu.Unlock()
		if rid != "" {
			return t.Name, rid, true
		}
	}
	return "", "", false
}

func (ptm *ProductionTeamManager) RateTeam(name string, stars int, comment string) error {
	if ptm == nil {
		return fmt.Errorf("团队管理器未初始化")
	}
	if stars < 1 || stars > 5 {
		return fmt.Errorf("评分须为 1-5 星, 收到 %d", stars)
	}
	ptm.mu.RLock()
	team, ok := ptm.teams[name]
	ptm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("团队 %q 不存在", name)
	}
	if ptm.evolution == nil {
		return fmt.Errorf("进化引擎未装配, 评分无处记录")
	}
	team.mu.Lock()
	runID := team.LastRunID
	team.mu.Unlock()
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("团队 %q 还没有已完成的运行可供评分", name)
	}
	ptm.evolution.RecordReward(RewardEvent{
		RunID:  runID,
		Source: RewardSourceUserExplicit,
		Value:  float64(stars-3) / 2.0,
		Raw:    map[string]any{"stars": stars, "comment": truncateResult(comment, 300)},
		Team:   team.Name,
	})
	return nil
}

// ---------------------------------------------------------------------------
// review.panel: 评审团融合分 (design/03 §4.2 第 5 行 "落 REPORT.md 文本, 不入库")
// ---------------------------------------------------------------------------

// recordReviewPanelReward 把评审团融合后的 overall (0-100) 记成奖励。
//
// 只认 overall>0: fuseReviews 在全部评审都没给分时会返回 0, 那是"没评上"而不是
// "评了 0 分", 记进去会变成一条捏造的满负分。
func recordReviewPanelReward(ctx context.Context, ee *EvolutionEngine, team *ProductionTeam, overall, consensus float64) {
	if ee == nil || team == nil || overall <= 0 {
		return
	}
	ee.RecordReward(RewardEvent{
		RunID:  trace.From(ctx).RunID,
		NodeID: "review-panel",
		Source: RewardSourceReviewPanel,
		Value:  overall/50.0 - 1.0, // 0-100 → [-1,1], 与 gate.content 同一映射
		Raw:    map[string]any{"overall": overall, "consensus": consensus},
		Team:   team.Name,
	})
}

// ---------------------------------------------------------------------------
// latency: 时长 shaping 负项 (design/03 §4.2 第 8 行 + Hermes H2 阶梯惩罚)
// ---------------------------------------------------------------------------

// 时长预算档位 (秒)。阶梯而非线性: Hermes H2 的经验是"预算内满分、超出后按档递减",
// 线性惩罚会让所有慢任务都被扣分, 淹没"离谱地慢"这个真正想抓的信号。
//
// 档位取值依据 = 本仓真实团队运行的量级: 单阶段 pipeline 数分钟, 多阶段 code 类
// 工作流十几分钟到半小时属正常, 超过一小时基本是卡在某次 LLM 调用或死循环上。
const (
	latencyBudgetSec = 20 * 60 // 20 分钟内不罚
	latencySoftSec   = 45 * 60 // 45 分钟: 轻罚
	latencyHardSec   = 90 * 60 // 90 分钟以上: 重罚
)

// latencyShapingValue 时长 → [-1,0] 的负项 (只罚不奖)。
//
// 刻意**不给正奖励**: 快不等于好 (最快的路径是什么都不做), 给正分会直接激励偷工。
// 时长只能作为负项存在, 这是防 reward hacking 的基本要求 (design/03 §4.6)。
func latencyShapingValue(durSec float64) float64 {
	switch {
	case durSec <= latencyBudgetSec:
		return 0
	case durSec <= latencySoftSec:
		return -0.2
	case durSec <= latencyHardSec:
		return -0.5
	default:
		return -1
	}
}

// recordLatencyReward 发一条时长 shaping 奖励; 值为 0 (预算内) 时不写。
//
// 不写 0 值是有意的: rewards.jsonl 是学习器要倒序扫的流水, 一条"没什么可说"的 0
// 会挤掉窗口里真正有信息的旧事件 (AggregateRewards 的窗口是固定字节数)。
func (ptm *ProductionTeamManager) recordLatencyReward(ctx context.Context, team *ProductionTeam, durSec float64) {
	if ptm == nil || ptm.evolution == nil || team == nil {
		return
	}
	v := latencyShapingValue(durSec)
	if v == 0 {
		return
	}
	ptm.evolution.RecordReward(RewardEvent{
		RunID:  trace.From(ctx).RunID,
		Source: RewardSourceLatency,
		Value:  v,
		Raw:    map[string]any{"duration_sec": durSec, "budget_sec": latencyBudgetSec},
		Team:   team.Name,
	})
}

// ---------------------------------------------------------------------------
// review-panel 产出解析 (供 recordReviewPanelReward 之外的消费方复用)
// ---------------------------------------------------------------------------

// parsePanelOverall 从 review-panel 的 stage 产出里取回 overall/consensus。
//
// 存在的理由: 评审团结果在 stage 产出里是 ```json 包裹的融合文档, 而奖励在
// executeReviewPanel 内部就已发出。这个函数供**离线**消费方 (回放任务集沉淀、
// 导出器质量过滤) 从历史产出反解分数, 不必重跑评审。
func parsePanelOverall(stageOutput string) (overall, consensus float64, ok bool) {
	js := extractJSONObject(stageOutput)
	if js == "" {
		return 0, 0, false
	}
	var doc struct {
		Overall   float64 `json:"overall"`
		Consensus float64 `json:"consensus"`
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil || doc.Overall <= 0 {
		return 0, 0, false
	}
	return doc.Overall, doc.Consensus, true
}
