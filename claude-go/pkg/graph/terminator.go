package graph

// terminator.go —— 可插拔循环终止器 (design/01 §4.4 的自适应退出)。
//
// # 为什么 Until 不够
//
// LoopPolicy.Until 是 condition.go 的四类原子条件, 全部只对**当前轮的单个结果**
// 求值 (ok/fail/score 比较/output 含子串)。而生产里 5 个 mode
// (creative_media / app_composite / game_composite / novel_writing / swarm_novel)
// 用的是 pkg/agent.AdaptiveTerminator 的**五路信号组合**:
//
//	① quality_pass   本轮分数达标           → 停            (单轮判据, Until 能表达)
//	② converged      相邻两轮分差 < ε        → 停            (跨轮)
//	③ degradation    连续 N 轮下降 > δ       → 停 + 回滚      (跨轮 + 计数状态)
//	④ strategy_shift 收敛但转换额度还有      → **不停**, 改回灌 (跨轮 + 计数状态)
//	⑤ best-of-N      终止时回到历史最高分那轮 → 改产出        (跨轮 + 历史产出)
//
// ②③⑤ 需要看历史轮次, ④ 根本不是"停/不停"而是"改下一轮的输入", ⑤ 会改变节点
// 最终产出是哪一轮的。把它们硬塞成 `score >= 6` 的后果不是"近似", 而是**②③⑤三路
// 直接消失**: 图在无望方向上跑满 MaxIterations, 且交付最后一轮 (可能是劣化后的) 产出。
//
// # 接口形状的取舍
//
//  1. **既给全历史, 又允许有状态**。看似冗余, 但两者都必要:
//     判据里绝大多数量 (相邻分差、历史最高分、轮数) 都能从 Rounds 现算, 不该逼实现
//     方自己攒; 而"连续退化计数"与"已用策略转换次数"**不能**从分数序列唯一还原 ——
//     退化计数在分数回升时被清零、在小幅下降时被保留 (见 adaptiveTerminator.Decide
//     里 else-if 的两个分支), 只看分数序列无法重建。所以终止器是 per-loop 实例
//     (可有状态), 同时 Decide 拿到 Rounds (不必自己攒)。
//  2. **回滚返回轮号而不是产出文本**。让终止器回传 BestOutput 字符串等于把同一份
//     产出在内核里复制一遍, 而且引擎无法校验它是否真的来自某一轮 (可以是编造的)。
//     返回轮号后引擎自己去 Rounds 里取, 并能拒绝"回滚到一个失败轮"这种非法建议。
//  3. **Rollback 用独立布尔而不是 BestRound>0 表示**。轮号 0 是合法轮 (第一轮),
//     用 0 当"未设置"会让"回滚到第 0 轮"静默失效 —— 与 spec.go 里
//     MinShards/Spawn 上限"0 是缺省"的同款坑, 那个坑已经踩过一次。
//
// # 与 Until 的关系
//
// 互斥 (validate.go 强制)。Until 的语义一字不变, 终止器是并存的另一条路 ——
// 两者都能决定退出, 并存等于把"第几轮停"交给引擎的求值顺序, 那不是语义而是巧合。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// 终止器作用域 (LoopContext.Scope): 节点级 Loop 还是组级 loop-group。
// 同一个终止器实现可以两处复用, 但要能在 journal 里区分是哪一层停的。
const (
	TerminatorScopeNode  = "node"
	TerminatorScopeGroup = "group"
)

// TerminatorAdaptive 内置终止器名: 复刻 pkg/agent.AdaptiveTerminator 的五路信号。
const TerminatorAdaptive = "adaptive"

// LoopRound 一轮循环的记录 (只读)。
type LoopRound struct {
	// Iteration 轮次号, 与 NodeInput.Iteration / journal loop.iteration 的
	// iteration 字段**同一口径** (0 起)。
	// 注意 resume 续跑的组循环里 Iteration 不等于在 Rounds 中的下标 (历史轮次
	// 不在本进程内重跑, Rounds 从 resume 起点开始), 所以引擎按 Iteration 值查回滚
	// 目标而不是按下标。
	Iteration int
	// Result 该轮的结果 (节点级 = 该节点本轮结果; 组级 = 该轮 ResultFrom 节点的结果)。
	Result NodeResult
}

// LoopContext 一次终止决策的输入 (只读)。
type LoopContext struct {
	NodeID string // 声明期节点 ID (非 journal 限定 ID)
	Scope  string // TerminatorScopeNode | TerminatorScopeGroup
	// MaxIterations 本循环的硬上限 (LoopPolicy.MaxIterations)。终止器要靠它判断
	// "这是最后一轮了"以便给出 best-of-N 回滚。
	MaxIterations int
	// Rounds 已完成的轮次 (含本轮), 按发生序。至少一条。
	Rounds []LoopRound
}

// Current 本轮 (Rounds 末条)。Rounds 恒非空, 引擎保证。
func (lc LoopContext) Current() LoopRound { return lc.Rounds[len(lc.Rounds)-1] }

// TerminateDecision 一次终止决策。
type TerminateDecision struct {
	// Stop true = 本轮之后不再继续。
	Stop bool
	// Signal 触发本决策的信号名 (**必填**)。它是 journal 里回答"为什么停在这一轮"
	// 的唯一凭据; 空信号会被引擎按 "unspecified" 记账并在 journal 里标出来。
	Signal string
	// Detail 人读的补充说明 (进 journal, 可空)。
	Detail string
	// Rollback true = 最终产出取第 BestRound 轮的结果 (best-of-N)。
	// 仅当 TerminatorSpec.AllowRollback 为真时生效, 否则引擎忽略并记账。
	Rollback bool
	// BestRound 回滚目标轮次 (口径同 LoopRound.Iteration)。
	BestRound int
	// FeedbackSuffix 追加到**下一轮**回灌末尾的文案 (仅 Stop=false 时有意义)。
	// 策略转换靠它真正改变下一轮的输入; 空 = 不改回灌。
	FeedbackSuffix string
}

// LoopTerminator 循环终止器。实现方**允许有状态**, 引擎为每一个循环环境
// (每次 runLoop 调用 / 每个 loop-group 节点) 创建一个新实例, 因此 Decide 不必并发安全。
type LoopTerminator interface {
	// Decide 在每一轮结束后被调用一次 (含最后一轮)。
	Decide(LoopContext) TerminateDecision
}

// TerminatorFactory 按声明构造终止器实例。
// **参数校验在这里做**: Validate 会为每个声明了 Terminator 的 Loop 调一次工厂,
// 于是坏参数 (量纲填错/缺必填项) 在开图时就报错, 而不是跑到第二轮才发现判据是死的。
type TerminatorFactory func(TerminatorSpec) (LoopTerminator, error)

var (
	terminatorMu  sync.RWMutex
	terminatorTbl = map[string]TerminatorFactory{
		TerminatorAdaptive: newAdaptiveTerminator,
	}
)

// RegisterTerminator 注册一个终止器工厂。
//
// **重名一律拒绝**而不是覆盖 (与 RegisterGraphOverride 的覆盖语义相反, 刻意):
// 终止判据属治理侧, 一个后注册的同名工厂静默换掉 adaptive 的判据, 会让全部引用
// "adaptive" 的图在不改一个字的情况下换掉退出条件, 事后完全不可考。
func RegisterTerminator(name string, f TerminatorFactory) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("graph: 终止器名不得为空")
	}
	if f == nil {
		return fmt.Errorf("graph: 终止器 %q 的工厂为 nil", name)
	}
	terminatorMu.Lock()
	defer terminatorMu.Unlock()
	if _, dup := terminatorTbl[name]; dup {
		return fmt.Errorf("graph: 终止器 %q 已注册 (重名注册会静默换掉既有图的退出条件, 一律拒绝)", name)
	}
	terminatorTbl[name] = f
	return nil
}

// resolveTerminator 按声明构造实例 (Validate 与执行期共用同一条路径,
// 于是"能开图"与"能构造"永不漂移)。
func resolveTerminator(spec *TerminatorSpec) (LoopTerminator, error) {
	if spec == nil {
		return nil, nil
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return nil, fmt.Errorf("terminator.name 为空 (可插拔终止器必须指名)")
	}
	terminatorMu.RLock()
	f := terminatorTbl[name]
	terminatorMu.RUnlock()
	if f == nil {
		return nil, fmt.Errorf("终止器 %q 未注册 (已注册: %s)", name, strings.Join(registeredTerminators(), ", "))
	}
	t, err := f(*spec)
	if err != nil {
		return nil, fmt.Errorf("终止器 %q 参数非法: %w", name, err)
	}
	if t == nil {
		return nil, fmt.Errorf("终止器 %q 的工厂返回了 nil", name)
	}
	return t, nil
}

// registeredTerminators 已注册的终止器名 (排序, 供报错文案)。
func registeredTerminators() []string {
	terminatorMu.RLock()
	defer terminatorMu.RUnlock()
	out := make([]string, 0, len(terminatorTbl))
	for n := range terminatorTbl {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 内置: adaptive —— pkg/agent.AdaptiveTerminator 五路信号的内核复刻
// ---------------------------------------------------------------------------

// adaptiveTerminator 五路信号终止器。
//
// 判据与顺序**逐条对齐** pkg/agent/adversarial.go 的 AdaptiveTerminator.ShouldTerminate
// (顺序本身是语义: 达标先于最小轮数, 最小轮数先于轮次耗尽, 否则 min_rounds 会把
// 一个已经达标的循环再拖一轮)。
//
// 与 pkg/agent 版的两处**必要**差异, 都是"内核不认识 agent 语义"导致的:
//
//	a) 判据输入是 NodeResult.Score 这一个标量, 不是 EvalScore 的七个维度。
//	   于是 pkg/agent 版里"任一维度 < 4 即不算达标"(MeetsHardPassThreshold 的
//	   灾难性低分闸) 与 ShouldRevert 的"多维回归"这两条**表达不了**。它们属于评分
//	   语义, 该由产出 Score 的 gate 节点自己实现 (gate 可以把"有维度崩了"直接压成
//	   一个低分), 而不是让调度内核认识 EvalScore。这是**已知的能力边界**, 不是桩。
//	b) 没有 ShouldResample (它依赖 Completeness 维度 + 编译历史) 与
//	   RecordBuild/TestResult (依赖编译门禁)。图里这些是独立的 gate 节点 + 条件边,
//	   不该塞进终止器。
//
// 状态只有两项 (degradeCount / shiftCount), 理由见文件头取舍 (1)。
type adaptiveTerminator struct {
	passScore     float64
	epsilon       float64
	degradeDelta  float64
	degradeRounds int
	maxShifts     int
	shiftFeedback string
	minRounds     int

	degradeCount int
	shiftCount   int
}

// newAdaptiveTerminator 构造内置 adaptive 终止器。四个判据阈值必须显式给出,
// 缺一个就报错 —— 理由逐条写在 TerminatorSpec 的字段注释里 (量纲不可推断,
// 猜错的后果是判据变成死路而**毫无报错**)。
func newAdaptiveTerminator(spec TerminatorSpec) (LoopTerminator, error) {
	if spec.PassScore <= 0 {
		return nil, fmt.Errorf("pass_score 必须 > 0 且显式给出 (分数量纲 0-10 还是 0-100 只有图作者知道)")
	}
	if spec.ConvergeEpsilon <= 0 {
		return nil, fmt.Errorf("converge_epsilon 必须 > 0 且显式给出 (量纲不匹配会让收敛早停静默失效)")
	}
	if spec.DegradeDelta <= 0 {
		return nil, fmt.Errorf("degrade_delta 必须 > 0 且显式给出 (量纲不匹配会让退化早停静默失效)")
	}
	if spec.DegradeRounds < 1 {
		return nil, fmt.Errorf("degrade_rounds 必须 >= 1 (连续退化几轮才终止)")
	}
	if spec.MaxStrategyShifts < 0 {
		return nil, fmt.Errorf("max_strategy_shifts 不得为负 (0 = 不做策略转换)")
	}
	if spec.MinRounds < 0 {
		return nil, fmt.Errorf("min_rounds 不得为负 (0 = 不限)")
	}
	return &adaptiveTerminator{
		passScore: spec.PassScore, epsilon: spec.ConvergeEpsilon,
		degradeDelta: spec.DegradeDelta, degradeRounds: spec.DegradeRounds,
		maxShifts: spec.MaxStrategyShifts, shiftFeedback: spec.StrategyShiftFeedback,
		minRounds: spec.MinRounds,
	}, nil
}

func (t *adaptiveTerminator) Decide(lc LoopContext) TerminateDecision {
	cur := lc.Current()
	roundNo := len(lc.Rounds) // 1 起, 与 AdaptiveTerminator 的 round 同口径
	score := cur.Result.Score

	// ① 质量达标 → 立即停 (不回滚: 本轮就是最好的可交付)。
	if score >= t.passScore {
		return TerminateDecision{Stop: true, Signal: "quality_pass",
			Detail: fmt.Sprintf("score=%.3f >= pass=%.3f", score, t.passScore)}
	}

	// ② 未达最小轮数 → 继续 (保证充分探索)。
	if roundNo < t.minRounds {
		return TerminateDecision{Signal: "min_rounds",
			Detail: fmt.Sprintf("第 %d 轮 < min_rounds=%d", roundNo, t.minRounds)}
	}

	// ③ 轮次耗尽 → 停 + best-of-N 回滚。
	// 引擎自己也会在 MaxIterations 处收尾, 但**必须**在这里也判一次: 否则最后一轮
	// 的回滚建议根本没机会给出, ⑤ 这路信号就只在退化时才生效了。
	if cur.Iteration >= lc.MaxIterations-1 {
		d := TerminateDecision{Stop: true, Signal: "max_rounds",
			Detail: fmt.Sprintf("已用满 %d 轮", lc.MaxIterations)}
		t.withRollback(&d, lc)
		return d
	}

	// ④ 退化: 相邻两轮下降超过 δ 计一次; 分数回升清零; 小幅下降 (<=δ, 噪声) 保持不变。
	// 这三个分支就是"退化计数不能从分数序列唯一还原"的原因 (见文件头)。
	if roundNo >= 2 {
		prev := lc.Rounds[len(lc.Rounds)-2].Result.Score
		switch {
		case prev-score > t.degradeDelta:
			t.degradeCount++
		case score >= prev:
			t.degradeCount = 0
		}
	}
	if t.degradeCount >= t.degradeRounds {
		d := TerminateDecision{Stop: true, Signal: "degradation",
			Detail: fmt.Sprintf("连续 %d 轮下降超过 %.3f", t.degradeCount, t.degradeDelta)}
		t.withRollback(&d, lc)
		return d
	}

	// ⑤ 收敛/震荡 → 还有额度就策略转换 (不停, 改回灌), 否则停。
	if roundNo >= 2 {
		delta := score - lc.Rounds[len(lc.Rounds)-2].Result.Score
		if delta >= 0 && delta < t.epsilon {
			if t.shiftCount < t.maxShifts {
				t.shiftCount++
				return TerminateDecision{Signal: "strategy_shift",
					Detail:         fmt.Sprintf("分差 %.3f < ε=%.3f, 第 %d/%d 次策略转换", delta, t.epsilon, t.shiftCount, t.maxShifts),
					FeedbackSuffix: t.shiftFeedback}
			}
			return TerminateDecision{Stop: true, Signal: "converged",
				Detail: fmt.Sprintf("分差 %.3f < ε=%.3f 且策略转换额度已用尽 (%d)", delta, t.epsilon, t.maxShifts)}
		}
	}
	return TerminateDecision{Signal: "improving",
		Detail: fmt.Sprintf("第 %d 轮 score=%.3f, 继续", roundNo, score)}
}

// withRollback 填入 best-of-N 回滚建议。
// 候选**只取 completed 轮**: 回滚到一个失败轮会把已经成功的节点变成失败;
// 同分取最早那轮 (确定性, 且早轮通常更简洁); 最佳就是本轮时不回滚 (无意义)。
func (t *adaptiveTerminator) withRollback(d *TerminateDecision, lc LoopContext) {
	bestIdx := -1
	for i, r := range lc.Rounds {
		if r.Result.Status != NodeStatusCompleted {
			continue
		}
		if bestIdx < 0 || r.Result.Score > lc.Rounds[bestIdx].Result.Score {
			bestIdx = i
		}
	}
	if bestIdx < 0 || bestIdx == len(lc.Rounds)-1 {
		return
	}
	d.Rollback = true
	d.BestRound = lc.Rounds[bestIdx].Iteration
	d.Detail += fmt.Sprintf("; 回滚到第 %d 轮 (score=%.3f)", d.BestRound, lc.Rounds[bestIdx].Result.Score)
}
