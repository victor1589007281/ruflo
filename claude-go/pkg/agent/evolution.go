// Evolution — 自动进化引擎, 从执行经验中学习并持续改进。
//
// 设计参考 (业界顶级方案融合):
//   - EvolveR (2025): 离线自蒸馏 + 在线交互闭环, ExpBase 战略原则库
//   - ERL (2026): 轨迹反思 → 可迁移启发式, 选择性检索注入
//   - Live-Evo (2026): 经验权重动态衰减, 有用经验增强/误导经验淘汰
//   - ruflo v3 ReasoningBank: 短/长期模式 + 质量评分 + 晋升/剪枝/去重
//   - ruflo v3 SONA: 轨迹记录 → 后台挖掘高置信步骤为新模式
//
// 四步进化流水线 (RECORD → DISTILL → RETRIEVE → EVOLVE):
//
//   1. RECORD: 每次 Agent 执行后记录完整轨迹 (输入/输出/错误/耗时/角色)
//   2. DISTILL: LLM 从成功/失败轨迹中提炼战略原则和错误模式
//   3. RETRIEVE: 下次执行前按角色+目标检索相关经验, 注入 Agent prompt
//   4. EVOLVE: 根据使用反馈更新质量分, 晋升/淘汰/去重
//
// 三类经验:
//   - RoleExperience: 角色特定知识 ("architect 设计API时应...")
//   - ErrorPattern: 报错→解决方案 ("遇到X错误时, 用Y方法")
//   - GeneralPrinciple: 跨角色通用原则 ("并行任务注意资源竞争")
//
//	┌──────────────────────────────────────────────────────────┐
//	│ EvolutionEngine                                          │
//	│  RecordTrajectory()  → 记录执行轨迹                      │
//	│  LearnFromTeam()     → LLM 批量提炼经验 (团队完成后)     │
//	│  RetrieveFor()       → BM25 检索相关经验 (执行前)        │
//	│  FormatForPrompt()   → 格式化为 Agent 可用的 prompt 段   │
//	│  RecordFeedback()    → 更新经验质量分 (EMA)              │
//	│  Consolidate()       → 去重/剪枝/晋升 (后台定期)         │
//	└──────────────────────────────────────────────────────────┘
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Experience 经验条目 (V2: 结构化+生命周期+注入追踪)。
type Experience struct {
	ID           string    `json:"id"`
	Category     string    `json:"category"`              // "role", "error", "general"
	Role         string    `json:"role,omitempty"`         // 角色 (category=role 时有值)
	Content      string    `json:"content"`                // 经验/原则文本
	Quality      float64   `json:"quality"`                // 0.0-1.0 (EMA 更新)
	UsageCount   int       `json:"usageCount"`             // 被检索使用的次数
	SuccessCount int       `json:"successCount"`           // 使用后任务成功的次数
	Tags         []string  `json:"tags,omitempty"`         // 标签
	Source       string    `json:"source"`                 // 来源 (team/stage)
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`

	// V2 新增字段
	Pattern    string   `json:"pattern,omitempty"`    // "strategy"|"pattern"|"antidote"|"evidence"
	Lifecycle  string   `json:"lifecycle,omitempty"`  // "proposed"|"validated"|"promoted"|"active"|"decaying"|"archived"
	SourceTeam string   `json:"sourceTeam,omitempty"` // 来源团队 (跨团队迁移追踪)
	MinHashSig []uint64 `json:"minHash,omitempty"`    // MinHash 签名 (快速去重)

	// 注入效果追踪 (参考 Live-Evo 动态权重)
	InjectionCount   int `json:"injectionCount,omitempty"`   // 被注入次数
	InjectionSuccess int `json:"injectionSuccess,omitempty"` // 注入后成功次数

	// UCB 选择辅助
	SelectionCount int `json:"selectionCount,omitempty"` // 被 UCB 选中次数 (含探索)
}

// SuccessRate 成功率。
func (e *Experience) SuccessRate() float64 {
	if e.UsageCount == 0 {
		return 0.5
	}
	return float64(e.SuccessCount) / float64(e.UsageCount)
}

// InjectionUplift 注入效果: 注入后成功率。
func (e *Experience) InjectionUplift() float64 {
	if e.InjectionCount == 0 {
		return 0.5
	}
	return float64(e.InjectionSuccess) / float64(e.InjectionCount)
}

// InjectionRecord 注入追踪记录。
type InjectionRecord struct {
	ExpIDs   []string `json:"expIds"`
	TaskID   string   `json:"taskId"`
	TeamName string   `json:"teamName"`
	Role     string   `json:"role"`
	Success  bool     `json:"success"`
	// Score RewardBus 加权分 [-1,1] (design/03 §4.3a)。Success 只是 Score>0 的投影;
	// 保留连续分是为了不在这一层把加权证据压回 bool 丢掉。老数据无此字段(0)。
	Score     float64   `json:"score,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Trajectory 执行轨迹。
type Trajectory struct {
	ID        string    `json:"id"`
	RunID     string    `json:"runId,omitempty"` // trace 四元组 episode id (design/03 §4.1 E0), 与 llm.jsonl 的 RunID 可 join
	TeamName  string    `json:"teamName"`
	StageName string    `json:"stageName"`
	Role      string    `json:"role"`
	Objective string    `json:"objective"`
	Input     string    `json:"input"`            // 截断的 prompt
	Output    string    `json:"output"`           // 截断的结果
	Error     string    `json:"error,omitempty"`
	Success   bool      `json:"success"`
	Duration  string    `json:"duration"`
	Timestamp time.Time `json:"timestamp"`
}

// DreamRecorderInterface V3: Evolution → Dreaming 交叉学习接口
type DreamRecorderInterface interface {
	RecordSession(record interface{})
}

// EvolutionEngine 自动进化引擎 (V2: 注入追踪+MinHash+UCB+生命周期)。
type EvolutionEngine struct {
	experiences  []*Experience
	trajectories []Trajectory
	injections   []InjectionRecord // V2: 注入效果追踪
	llm          LLMClient
	dataDir      string
	mu           sync.RWMutex
	nextID       int

	// V2 统计缓存
	totalSelections int     // UCB 全局选择计数
	totalDistilled  int     // 历史提炼总数 (用于 survival_rate)
	baselineSuccess float64 // 无注入的基线成功率 (滑动窗口)
	baselineTotal   int     // 基线样本数

	// V3: 交叉学习 — 高质量经验自动注入 Dreaming
	MemoryIngestFn func(content, source string, topics []string)
}

// RewardEvent 奖励事件 (design/03 §4.2 RewardBus 的落盘雏形)。
// 历史缺陷: content_gate 的 0-100 连续分是现成 dense reward 却用完即丢
// (design/03 §1.4); 现所有奖励源统一落 <evolution>/rewards.jsonl,
// P3 的 RewardBus/学习器直接消费该文件, 格式即契约。
type RewardEvent struct {
	TS     int64   `json:"ts"`                // unix milli
	RunID  string  `json:"run_id,omitempty"`  // trace 四元组 episode id
	NodeID string  `json:"node_id,omitempty"` // 阶段/节点粒度奖励时非空
	Source string  `json:"source"`            // gate.content|gate.compile|episode|user.explicit|...
	Value  float64 `json:"value"`             // 归一化 [-1,1]
	Weight float64 `json:"weight,omitempty"`  // 源可信度权重 (0=按 Source 取默认, 见 RewardSourceWeight)
	Raw    any     `json:"raw,omitempty"`     // 原始值 (0-100 分 / 状态字符串等)
	Team   string  `json:"team,omitempty"`
}

// 奖励源可信度权重 (design/03 §4.2 "源可信度"): 确定性门禁 > LLM 评分 > episode 终态。
//
// 定值依据 (不是拍脑袋, 而是"这条信号能被伪造/漂移的程度"):
//   - gate.compile / gate.test: 真跑 go build / go test -race, 只信 exit code,
//     同一份代码重复跑结论一致, 无主观性 ⇒ 满权重 1.0 (设计里价值排第 1 的那类)。
//   - user.explicit: 人的显式反馈可信度高, 但稀疏且含情绪噪声 ⇒ 0.8。
//   - gate.content: 单次 LLM 打分, 同一产出重跑分数方差可达 ±10 分 ⇒ 0.5。
//   - episode: 团队终态只有 3 档, 且被 fail-open 语义污染
//     (delivered_with_remediation 也记 0.5), 粒度最粗 ⇒ 0.3。
//   - 未知源: 0.5 —— 不给 0 (新源接进来不该被静默忽略), 也不给 1.0
//     (未经校准的信号不该压倒确定性门禁)。
const (
	rewardWeightDeterministic = 1.0  // 确定性工具门禁 (编译/测试/lint)
	rewardWeightUser          = 0.8  // 人的显式反馈
	rewardWeightLLMJudge      = 0.5  // LLM 评分类
	rewardWeightEpisode       = 0.3  // 运行终态
	rewardWeightShaping       = 0.2  // cost/latency 等 shaping 负项 (真实计量但非质量判断)
	rewardWeightHeuristic     = 0.15 // turn verdict 等启发式过程信号 (最弱: 只说明没崩)
	rewardWeightUnknown       = 0.5  // 未登记的源
)

// 已接线的奖励源名 (格式即契约: rewards.jsonl 的 source 值 / skillaudit 依赖)。
const (
	RewardSourceGateCompile = "gate.compile"
	RewardSourceGateTest    = "gate.test"
	RewardSourceGateContent = "gate.content"
	RewardSourceEpisode     = "episode"
	// RewardSourceGateE2E 下游平台回传的 e2e/验收门禁结论 (design/03 §4.2 第 6 行)。
	// 唯一入口是 dashboard 的 POST /api/runs/{id}/feedback —— 本进程算不出"部署出去
	// 跑不跑得起来", 只能由下游回传, 所以它与本地确定性门禁同权重。
	RewardSourceGateE2E = "gate.e2e"
	// RewardSourceVerdictHeuristic turn 级启发式裁决 (design/03 §4.2 第 7 行)。
	// 由 stop_reason + 工具成败推断, 是**过程**信号不是质量判断 ⇒ 弱权重。
	RewardSourceVerdictHeuristic = "verdict.heuristic"
	// RewardSourceGatePrefix 前缀过滤用: 所有门禁类奖励 (含未来新增的 gate.lint 等)。
	RewardSourceGatePrefix = "gate."
)

// RewardSourceWeight 返回某奖励源的默认可信度权重。
func RewardSourceWeight(source string) float64 {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case RewardSourceGateCompile, RewardSourceGateTest, "gate.lint", RewardSourceGateE2E:
		return rewardWeightDeterministic
	case RewardSourceUserExplicit, RewardSourceUserSteer, "user.feedback":
		return rewardWeightUser
	case RewardSourceGateContent, RewardSourceReviewPanel, "gate.review", "llm.judge":
		return rewardWeightLLMJudge
	case RewardSourceEpisode:
		return rewardWeightEpisode
	case RewardSourceLatency, "cost":
		// shaping 负项: 是真实计量 (时长/token) 而非质量判断, 不该与"做得好不好"
		// 同权重竞争 —— 它只负责在其他信号打平时把"又慢又贵"的那个往下压。
		return rewardWeightShaping
	case RewardSourceVerdictHeuristic:
		// 设计原文: "现启发式保留为弱信号 (weight 低)"。它推断的是"这轮跑完没跑完、
		// 工具报没报错", 与"做出来的东西对不对"是两回事 —— 一个把工具全调成功、
		// 内容全错的 run 在它眼里是满分。所以必须比 episode 还低: episode 至少还
		// 反映了交付判定, 而它只反映过程没崩。
		return rewardWeightHeuristic
	default:
		return rewardWeightUnknown
	}
}

// clampReward 把奖励值钳到 [-1,1] (契约: 任何源写入的 value 都在此区间)。
func clampReward(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// RecordReward 追加奖励事件到 rewards.jsonl (追加写, 失败静默——奖励缺一条不应影响交付)。
func (ee *EvolutionEngine) RecordReward(ev RewardEvent) {
	if ee == nil || ee.dataDir == "" {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	// 权重落盘而不是只在读侧算: 权重表将来会调, 已发生的奖励应保留当时的可信度。
	if ev.Weight <= 0 {
		ev.Weight = RewardSourceWeight(ev.Source)
	}
	ev.Value = clampReward(ev.Value)
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	ee.mu.Lock()
	defer ee.mu.Unlock()
	_ = os.MkdirAll(ee.dataDir, 0o755)
	f, err := os.OpenFile(filepath.Join(ee.dataDir, "rewards.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// ============================================================================
// RewardBus 读侧: 奖励聚合 (design/03 §4.3a 升级点3 "反馈从 stage 二值 → 加权聚合")
//
// 历史缺陷: rewards.jsonl 只有写方, 全仓唯一读方是两个离线 CLI ⇒ RewardBus 是
// 只写日志而不是总线。这里给学习器一个读侧入口: 按 run(+节点/源) 聚合出一个
// [-1,1] 的加权分, 替掉 "sr.Status == TaskCompleted" 这个二值。
// ============================================================================

const (
	rewardTailScanBytes = 1 << 20 // 默认尾部扫描窗口 1MiB
	rewardAggMaxEvents  = 200     // 单次聚合最多纳入的事件数
)

// RewardQuery 奖励聚合的过滤条件。
//
// RunID 必填: 跨 run 混算会把别的任务的成败记到本次头上 (rewards.jsonl 是全局流水)。
type RewardQuery struct {
	RunID        string // 必填, 空则不聚合
	Team         string // 非空 = 只算该团队
	NodeID       string // 非空 = 只算该节点(阶段)粒度的奖励
	SourcePrefix string // 非空 = 只算 source 以此为前缀的奖励 (如 "gate.")
	MaxEvents    int    // 参与聚合的最大事件数 (<=0 用默认)
	MaxScanBytes int64  // 尾部扫描窗口字节数 (<=0 用默认)
}

// RewardAggregate 一次聚合的结果。
type RewardAggregate struct {
	Score     float64        // 加权分, 钳在 [-1,1]; Count=0 时为 0
	Count     int            // 参与聚合的事件数 (0 = 无奖励证据 ⇒ 调用方必须回退)
	WeightSum float64        // 权重和 (诊断用)
	Sources   map[string]int // 各源命中数 (诊断用)
}

// HasEvidence 是否存在奖励证据。无证据时调用方必须回退到原有信号 (stage 二值),
// 否则没接奖励源的工作流会从"二值反馈"退化成"无反馈"。
func (a RewardAggregate) HasEvidence() bool { return a.Count > 0 }

// AggregateRewards 倒序扫 rewards.jsonl 尾部窗口, 聚合出加权奖励分。
//
// 三条关键规则:
//  1. 只读尾部固定窗口 + 事件数上限: rewards.jsonl 是只追加的长流水, 全量 load 会
//     随运行时长线性变慢并吃内存, 而学习反馈只关心最近的证据。
//  2. 同 (source, node_id) 只取最新一条: 门禁会在"失败→自动修复→重跑"里对同一节点
//     打多次分, 若全部平均, 修复后的好结果会被修复前的坏分数拖回去。
//  3. 权重优先用事件自带的 Weight (落盘时的可信度), 缺失(老数据)才按 source 取默认。
func (ee *EvolutionEngine) AggregateRewards(q RewardQuery) RewardAggregate {
	agg := RewardAggregate{Sources: map[string]int{}}
	if ee == nil || ee.dataDir == "" || strings.TrimSpace(q.RunID) == "" {
		return agg
	}
	maxEvents := q.MaxEvents
	if maxEvents <= 0 {
		maxEvents = rewardAggMaxEvents
	}
	scanBytes := q.MaxScanBytes
	if scanBytes <= 0 {
		scanBytes = rewardTailScanBytes
	}

	// 与 RecordReward 的追加写互斥, 避免读到写了一半的行 (读锁足够: 聚合不写状态)。
	ee.mu.RLock()
	lines := readTailLines(filepath.Join(ee.dataDir, "rewards.jsonl"), scanBytes)
	ee.mu.RUnlock()

	prefix := strings.ToLower(strings.TrimSpace(q.SourcePrefix))
	seen := make(map[string]bool, len(lines))
	weighted := 0.0
	for i := len(lines) - 1; i >= 0; i-- { // 倒序 = 从最新往回
		var ev RewardEvent
		if err := json.Unmarshal(lines[i], &ev); err != nil {
			continue // 半行/脏行跳过, 不能让一条坏行毁掉整次聚合
		}
		if ev.RunID != q.RunID {
			continue
		}
		if q.Team != "" && ev.Team != q.Team {
			continue
		}
		if q.NodeID != "" && ev.NodeID != q.NodeID {
			continue
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(ev.Source), prefix) {
			continue
		}
		key := ev.Source + "\x00" + ev.NodeID
		if seen[key] {
			continue // 同源同节点的旧观测被新观测取代
		}
		seen[key] = true

		w := ev.Weight
		if w <= 0 {
			w = RewardSourceWeight(ev.Source)
		}
		weighted += w * clampReward(ev.Value)
		agg.WeightSum += w
		agg.Count++
		agg.Sources[ev.Source]++
		if agg.Count >= maxEvents {
			break
		}
	}
	if agg.WeightSum > 0 {
		agg.Score = clampReward(weighted / agg.WeightSum)
	}
	return agg
}

// StageRewardScore 某 run 内某阶段(节点)的加权奖励分。第二返回值=是否有证据。
//
// 只认"同 run 同节点"的证据是刻意的: run 级奖励 (全局编译/测试门禁、episode 终态)
// 都发生在所有阶段之后, 且把它归因到单个阶段会让"修复阶段"被它正要修的那次失败
// 倒打一耙 (修复门禁失败时, 修复阶段执行在失败奖励之后)。
func (ee *EvolutionEngine) StageRewardScore(runID, team, node string) (float64, bool) {
	agg := ee.AggregateRewards(RewardQuery{RunID: runID, Team: team, NodeID: node})
	return agg.Score, agg.HasEvidence()
}

// RunRewardScore 整个 run 的加权奖励分 (含 episode 终态)。第二返回值=是否有证据。
func (ee *EvolutionEngine) RunRewardScore(runID, team string) (float64, bool) {
	agg := ee.AggregateRewards(RewardQuery{RunID: runID, Team: team})
	return agg.Score, agg.HasEvidence()
}

// GateRewardScore 整个 run 的**门禁类**加权奖励分 (gate.compile/gate.test/gate.content...)。
// 刻意排除 episode: episode 就是本 run 的交付状态, 与"全阶段通过"高度共线, 混进来
// 会让门禁的坏消息被终态的好消息中和掉。
func (ee *EvolutionEngine) GateRewardScore(runID, team string) (float64, bool) {
	agg := ee.AggregateRewards(RewardQuery{RunID: runID, Team: team, SourcePrefix: RewardSourceGatePrefix})
	return agg.Score, agg.HasEvidence()
}

// readTailLines 读文件尾部至多 maxBytes 字节, 按行切分并丢弃被截断的首行。
// 返回顺序与文件顺序一致 (调用方自行倒序遍历)。
func readTailLines(path string, maxBytes int64) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	offset := int64(0)
	size := st.Size()
	if size > maxBytes {
		offset = size - maxBytes
	}
	buf := make([]byte, size-offset)
	// 只用真正读到的字节: 并发追加/截断时 ReadAt 会短读并返回 EOF, 若仍用整个 buf,
	// 尾部的零字节会被当成行内容参与解析。
	n, err := f.ReadAt(buf, offset)
	if n <= 0 {
		return nil
	}
	if err != nil && n < len(buf) {
		buf = buf[:n]
	}
	if offset > 0 {
		// 窗口起点大概率落在某行中间, 丢掉这半行 (否则解析出错误的事件)。
		if idx := bytes.IndexByte(buf, '\n'); idx >= 0 {
			buf = buf[idx+1:]
		} else {
			return nil
		}
	}
	raw := bytes.Split(buf, []byte("\n"))
	out := make([][]byte, 0, len(raw))
	for _, ln := range raw {
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// NewEvolutionEngine 创建进化引擎。
func NewEvolutionEngine(dataDir string, llm LLMClient) *EvolutionEngine {
	ee := &EvolutionEngine{
		llm:     llm,
		dataDir: dataDir,
	}
	ee.load()
	ee.migrateV2Fields()
	return ee
}

// migrateV2Fields 为旧数据补充 V2 新字段默认值。
func (ee *EvolutionEngine) migrateV2Fields() {
	for _, exp := range ee.experiences {
		if exp.Lifecycle == "" {
			if exp.Quality >= 0.6 && exp.UsageCount > 0 {
				exp.Lifecycle = "active"
			} else if exp.Quality >= 0.3 {
				exp.Lifecycle = "validated"
			} else {
				exp.Lifecycle = "proposed"
			}
		}
		if exp.Pattern == "" {
			switch exp.Category {
			case "error":
				exp.Pattern = "antidote"
			case "role":
				exp.Pattern = "strategy"
			default:
				exp.Pattern = "pattern"
			}
		}
		if len(exp.MinHashSig) == 0 {
			exp.MinHashSig = computeMinHash(evolutionTokenize(exp.Content), 64)
		}
	}
}

// --- RECORD: 记录执行轨迹 ---

// RecordTrajectory 记录一条 Agent 执行轨迹。
// 每次 workflow stage / swarm subtask 完成后调用。
func (ee *EvolutionEngine) RecordTrajectory(t Trajectory) {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	if t.ID == "" {
		ee.nextID++
		t.ID = fmt.Sprintf("traj-%d-%d", time.Now().Unix(), ee.nextID)
	}
	if t.Timestamp.IsZero() {
		t.Timestamp = time.Now()
	}
	// 智能截断: 保留前部 + 后部关键上下文，避免丢失重要信息
	t.Input = smartTruncateEvolution(t.Input, 4000)
	t.Output = smartTruncateEvolution(t.Output, 6000)

	ee.trajectories = append(ee.trajectories, t)

	// 保持轨迹数量在合理范围
	if len(ee.trajectories) > 500 {
		ee.trajectories = ee.trajectories[len(ee.trajectories)-500:]
	}

	ee.persistTrajectories()
}

// --- DISTILL: LLM 提炼经验 ---

// LearnFromTeamSync 同步版本: 阻塞直到经验提炼完成。
// 适用于需要保证后续操作能立即使用新经验的场景 (如测试、连续工作流)。
func (ee *EvolutionEngine) LearnFromTeamSync(ctx context.Context, teamName string) int {
	before := len(ee.experiences)
	ee.LearnFromTeam(ctx, teamName)
	ee.mu.RLock()
	after := len(ee.experiences)
	ee.mu.RUnlock()
	return after - before
}

// LearnFromTeam 团队执行完成后, 从轨迹中提炼经验。
// 参考: EvolveR 离线自蒸馏 + ruflo v3 SONA runBackgroundLoop
func (ee *EvolutionEngine) LearnFromTeam(ctx context.Context, teamName string) {
	ee.mu.RLock()
	var teamTrajs []Trajectory
	for _, t := range ee.trajectories {
		if t.TeamName == teamName {
			teamTrajs = append(teamTrajs, t)
		}
	}
	ee.mu.RUnlock()

	if len(teamTrajs) == 0 {
		return
	}

	log.Printf("[Evolution] 从团队 %s 的 %d 条轨迹中提炼经验...", teamName, len(teamTrajs))

	// LLM 驱动的经验提炼 (带超时保护)
	if ee.llm != nil {
		distillCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		ee.llmDistill(distillCtx, teamTrajs, teamName)
	} else {
		ee.heuristicDistill(teamTrajs, teamName)
	}

	ee.persistExperiences()
	log.Printf("[Evolution] 经验提炼完成, 当前共 %d 条经验", len(ee.experiences))

	// V3: 交叉学习 — 将高质量经验注入到 FactStore/Dreaming
	if ee.MemoryIngestFn != nil {
		ee.mu.RLock()
		for _, exp := range ee.experiences {
			if exp.Quality >= 0.6 && exp.SourceTeam == teamName &&
				time.Since(exp.CreatedAt) < 5*time.Minute {
				ee.MemoryIngestFn(exp.Content, "evolution:"+teamName, exp.Tags)
			}
		}
		ee.mu.RUnlock()
	}
}

// LearnFromStage 单阶段增量学习 (双向: 成功+失败都提炼, 参考 MiniMax M2.7)。
func (ee *EvolutionEngine) LearnFromStage(traj Trajectory) {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	if traj.Error != "" && !traj.Success {
		content := fmt.Sprintf("[%s] 执行「%s」失败: %s → 建议: 检查参数和前置依赖",
			traj.Role, truncateResult(traj.Objective, 80), truncateResult(traj.Error, 150))
		if !ee.isDuplicate(content) {
			ee.nextID++
			ee.totalDistilled++
			ee.experiences = append(ee.experiences, &Experience{
				ID:         fmt.Sprintf("exp-inc-%d-%d", time.Now().Unix(), ee.nextID),
				Category:   "error",
				Role:       traj.Role,
				Content:    content,
				Quality:    0.4,
				Source:     traj.TeamName + "/" + traj.StageName,
				Tags:       []string{traj.Role, "incremental", "failure"},
				Pattern:    "antidote",
				Lifecycle:  "proposed",
				SourceTeam: traj.TeamName,
				MinHashSig: computeMinHash(evolutionTokenize(strings.ToLower(content)), 64),
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			})
		}
	} else if traj.Success && traj.Output != "" && len(traj.Output) > 100 {
		content := ee.distillSuccessHeuristic(traj)
		if content != "" && !ee.isDuplicate(content) {
			ee.nextID++
			ee.totalDistilled++
			ee.experiences = append(ee.experiences, &Experience{
				ID:         fmt.Sprintf("exp-suc-%d-%d", time.Now().Unix(), ee.nextID),
				Category:   "role",
				Role:       traj.Role,
				Content:    content,
				Quality:    0.6,
				Source:     traj.TeamName + "/" + traj.StageName,
				Tags:       []string{traj.Role, "incremental", "success"},
				Pattern:    "strategy",
				Lifecycle:  "proposed",
				SourceTeam: traj.TeamName,
				MinHashSig: computeMinHash(evolutionTokenize(strings.ToLower(content)), 64),
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			})
		}
	}
}

// distillSuccessHeuristic 从成功执行中启发式提炼经验。
func (ee *EvolutionEngine) distillSuccessHeuristic(traj Trajectory) string {
	output := traj.Output
	if len(output) > 500 {
		output = output[:500]
	}
	// 提取关键模式: 文件创建/修改, 技术决策, 测试覆盖
	var patterns []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		isPattern := strings.Contains(line, "func ") || strings.Contains(line, "type ") ||
			strings.Contains(line, "interface") || strings.Contains(line, "package ") ||
			strings.Contains(line, "决策") || strings.Contains(line, "选择") ||
			strings.Contains(line, "方案") || strings.Contains(line, "设计")
		if isPattern && len(line) > 10 && len(line) < 200 {
			patterns = append(patterns, line)
			if len(patterns) >= 3 {
				break
			}
		}
	}
	if len(patterns) == 0 {
		return ""
	}
	return fmt.Sprintf("[%s] 成功执行「%s」— 关键模式: %s",
		traj.Role, truncateResult(traj.Objective, 60), strings.Join(patterns, "; "))
}

func (ee *EvolutionEngine) llmDistill(ctx context.Context, trajs []Trajectory, teamName string) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("团队: %s\n\n执行轨迹:\n", teamName))

	for _, t := range trajs {
		status := "✅ 成功"
		if !t.Success {
			status = "❌ 失败"
		}
		sb.WriteString(fmt.Sprintf("\n--- 阶段: %s (角色: %s) [%s] ---\n", t.StageName, t.Role, status))
		sb.WriteString(fmt.Sprintf("目标: %s\n", t.Objective))
		if t.Output != "" {
			output := t.Output
			if len(output) > 1000 {
				output = output[:1000] + "..."
			}
			sb.WriteString(fmt.Sprintf("输出摘要: %s\n", output))
		}
		if t.Error != "" {
			sb.WriteString(fmt.Sprintf("错误: %s\n", t.Error))
		}
		sb.WriteString(fmt.Sprintf("耗时: %s\n", t.Duration))
	}

	sysPrompt := `你是经验提炼专家。分析多Agent团队的执行轨迹，提炼可复用的经验教训。

输出严格JSON数组 (不要解释):
[
  {"category":"role","role":"角色名","content":"该角色的具体经验教训","tags":["关键词"]},
  {"category":"error","role":"相关角色","content":"问题描述→解决方案","tags":["错误类型"]},
  {"category":"general","content":"跨角色的通用原则","tags":["关键词"]}
]

提炼规则:
1. 从成功轨迹提炼"什么做得好、为什么有效" (category=role 或 general)
2. 从失败轨迹提炼"出了什么问题、如何避免/解决" (category=error)
3. 每条经验必须是具体、可操作的 (不要泛泛而谈)
4. 最多提炼10条最有价值的经验
5. content 用中文, 简洁明确 (1-3句话)`

	resp, err := ee.llm.SimpleComplete(ctx, sysPrompt, sb.String())
	if err != nil {
		log.Printf("[Evolution] LLM 提炼失败, 回退到启发式: %v", err)
		ee.heuristicDistill(trajs, teamName)
		return
	}

	resp = strings.TrimSpace(resp)
	if idx := strings.Index(resp, "```"); idx >= 0 {
		resp = resp[idx+3:]
		resp = strings.TrimPrefix(resp, "json")
		if end := strings.Index(resp, "```"); end >= 0 {
			resp = resp[:end]
		}
	}
	resp = strings.TrimSpace(resp)

	var extracted []struct {
		Category string   `json:"category"`
		Role     string   `json:"role"`
		Content  string   `json:"content"`
		Tags     []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(resp), &extracted); err != nil {
		log.Printf("[Evolution] 解析 LLM 结果失败: %v", err)
		ee.heuristicDistill(trajs, teamName)
		return
	}

	ee.mu.Lock()
	defer ee.mu.Unlock()

	for _, ext := range extracted {
		if ext.Content == "" {
			continue
		}

		// 去重: 检查是否已有高度相似的经验
		if ee.isDuplicate(ext.Content) {
			continue
		}

		ee.nextID++
		ee.totalDistilled++
		pattern := "pattern"
		switch ext.Category {
		case "error":
			pattern = "antidote"
		case "role":
			pattern = "strategy"
		}
		exp := &Experience{
			ID:         fmt.Sprintf("exp-%d-%d", time.Now().Unix(), ee.nextID),
			Category:   ext.Category,
			Role:       ext.Role,
			Content:    ext.Content,
			Quality:    0.5,
			Tags:       ext.Tags,
			Source:      teamName,
			Pattern:    pattern,
			Lifecycle:  "proposed",
			SourceTeam: teamName,
			MinHashSig: computeMinHash(evolutionTokenize(strings.ToLower(ext.Content)), 64),
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}
		ee.experiences = append(ee.experiences, exp)
	}
}

// heuristicDistill 启发式经验提炼 (v2: 增强失败记录 + 模式提取)。
// 参考: 人类从错误中学习比从成功中学习更高效 (负强化学习)。
func (ee *EvolutionEngine) heuristicDistill(trajs []Trajectory, teamName string) {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	for _, t := range trajs {
		// v2: 失败轨迹增强 — 提取具体错误类型并记录上下文
		if t.Error != "" || !t.Success {
			errorType := classifyError(t.Error)
			content := fmt.Sprintf("[%s/%s] 执行「%s」失败 (%s): %s\n建议: %s",
				t.Role, t.StageName, truncateResult(t.Objective, 80),
				errorType, truncateResult(t.Error, 150),
				suggestFix(errorType, t.Error))
			if !ee.isDuplicate(content) {
				ee.nextID++
				ee.totalDistilled++
				ee.experiences = append(ee.experiences, &Experience{
					ID:         fmt.Sprintf("exp-%d-%d", time.Now().Unix(), ee.nextID),
					Category:   "error",
					Role:       t.Role,
					Content:    content,
					Quality:    0.4,
					Source:     teamName,
					Tags:       []string{t.Role, "error", errorType},
					Pattern:    "antidote",
					Lifecycle:  "proposed",
					SourceTeam: teamName,
					MinHashSig: computeMinHash(evolutionTokenize(strings.ToLower(content)), 64),
					CreatedAt:  time.Now(),
					UpdatedAt:  time.Now(),
				})
			}
		}

		if t.Success && t.Output != "" {
			content := fmt.Sprintf("[%s/%s] 成功完成「%s」(耗时 %s)",
				t.Role, t.StageName, truncateResult(t.Objective, 100), t.Duration)
			if !ee.isDuplicate(content) {
				ee.nextID++
				ee.totalDistilled++
				ee.experiences = append(ee.experiences, &Experience{
					ID:         fmt.Sprintf("exp-%d-%d", time.Now().Unix(), ee.nextID),
					Category:   "role",
					Role:       t.Role,
					Content:    content,
					Quality:    0.5,
					Source:     teamName,
					Tags:       []string{t.Role, "success"},
					Pattern:    "strategy",
					Lifecycle:  "proposed",
					SourceTeam: teamName,
					MinHashSig: computeMinHash(evolutionTokenize(strings.ToLower(content)), 64),
					CreatedAt:  time.Now(),
					UpdatedAt:  time.Now(),
				})
			}
		}
	}
}

// classifyError 分类错误类型 (v2: 用于精准检索和学习)。
func classifyError(errStr string) string {
	lower := strings.ToLower(errStr)
	switch {
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline"):
		return "timeout"
	case strings.Contains(lower, "compile") || strings.Contains(lower, "syntax") || strings.Contains(lower, "import"):
		return "compilation"
	case strings.Contains(lower, "permission") || strings.Contains(lower, "denied"):
		return "permission"
	case strings.Contains(lower, "not found") || strings.Contains(lower, "undefined"):
		return "not_found"
	case strings.Contains(lower, "空转") || strings.Contains(lower, "角色扮演"):
		return "idle_output"
	default:
		return "runtime"
	}
}

// suggestFix 根据错误类型生成修复建议。
func suggestFix(errorType, _ string) string {
	switch errorType {
	case "timeout":
		return "增加超时时间或拆分任务为更小的子任务"
	case "compilation":
		return "检查导入声明和类型定义是否完整"
	case "idle_output":
		return "Agent可能未理解任务，需要更明确的prompt和示例"
	case "not_found":
		return "检查依赖项和引用路径是否正确"
	default:
		return "检查日志详情，考虑重试或调整策略"
	}
}

func (ee *EvolutionEngine) isDuplicate(content string) bool {
	tokens := evolutionTokenize(strings.ToLower(content))
	sig := computeMinHash(tokens, 64)
	for _, existing := range ee.experiences {
		// 先用 MinHash 快速筛选, 再精确验证
		if len(existing.MinHashSig) > 0 && minHashSimilarity(sig, existing.MinHashSig) > 0.6 {
			if jaccardSimilarity(strings.ToLower(content), strings.ToLower(existing.Content)) > 0.7 {
				return true
			}
		} else if len(existing.MinHashSig) == 0 {
			if jaccardSimilarity(strings.ToLower(content), strings.ToLower(existing.Content)) > 0.7 {
				return true
			}
		}
	}
	return false
}

// RecordInjection 记录一次经验注入 (V2: 注入效果追踪)。
// 二值入口保留 (语义等价于 ±1 分), 老调用方不受影响。
func (ee *EvolutionEngine) RecordInjection(expIDs []string, taskID, teamName, role string, success bool) {
	ee.RecordInjectionScored(expIDs, taskID, teamName, role, boolFeedbackScore(success))
}

// RecordInjectionScored 记录一次经验注入 (加权分版本)。
// score>0 记为成功 (Uplift 指标口径不变), 同时把连续分落到 InjectionRecord.Score
// 供后续分析 —— 否则加权证据在这一步又被压回一个 bool 丢掉。
func (ee *EvolutionEngine) RecordInjectionScored(expIDs []string, taskID, teamName, role string, score float64) {
	if ee == nil {
		return
	}
	ee.mu.Lock()
	defer ee.mu.Unlock()

	score = clampReward(score)
	success := score > 0
	rec := InjectionRecord{
		ExpIDs:    expIDs,
		TaskID:    taskID,
		TeamName:  teamName,
		Role:      role,
		Success:   success,
		Score:     score,
		Timestamp: time.Now(),
	}
	ee.injections = append(ee.injections, rec)
	if len(ee.injections) > 2000 {
		ee.injections = ee.injections[len(ee.injections)-2000:]
	}

	// 更新每条经验的注入统计
	for _, id := range expIDs {
		for _, exp := range ee.experiences {
			if exp.ID == id {
				exp.InjectionCount++
				if success {
					exp.InjectionSuccess++
				}
				break
			}
		}
	}

	ee.persistInjections()
}

// InjectionUpliftGlobal 全局注入提升率 (V2指标: evo_injection_uplift)。
// Uplift = P(success|injected) - P(success|baseline)
func (ee *EvolutionEngine) InjectionUpliftGlobal() float64 {
	ee.mu.RLock()
	defer ee.mu.RUnlock()

	if len(ee.injections) == 0 {
		return 0
	}
	injSuccess, injTotal := 0, 0
	for _, r := range ee.injections {
		if len(r.ExpIDs) > 0 {
			injTotal++
			if r.Success {
				injSuccess++
			}
		}
	}
	if injTotal == 0 {
		return 0
	}
	injRate := float64(injSuccess) / float64(injTotal)
	baseline := ee.baselineSuccess
	if baseline == 0 {
		baseline = 0.5 // 默认基线
	}
	return injRate - baseline
}

// UpdateBaseline 更新无注入的基线成功率 (用于 Uplift 计算)。
// 二值入口保留 (语义等价于 ±1 分)。
func (ee *EvolutionEngine) UpdateBaseline(success bool) {
	ee.UpdateBaselineScored(boolFeedbackScore(success))
}

// UpdateBaselineScored 按加权分更新基线成功率。
// 把 [-1,1] 线性映到成功率轴 [0,1]: score=+1 → 1.0, score=-1 → 0.0,
// 端点与旧二值实现完全一致; 中间分给出部分信用 (如内容门禁 60/100 → 0.2)。
func (ee *EvolutionEngine) UpdateBaselineScored(score float64) {
	if ee == nil {
		return
	}
	target := (clampReward(score) + 1) / 2
	ee.mu.Lock()
	defer ee.mu.Unlock()
	ee.baselineTotal++
	alpha := 0.1
	ee.baselineSuccess = (1-alpha)*ee.baselineSuccess + alpha*target
}

// LearnCounterfactual 反事实学习 (V2 P6): 从失败轨迹生成 "如果…会更好" 的假设。
func (ee *EvolutionEngine) LearnCounterfactual(traj Trajectory) {
	if traj.Success || traj.Error == "" {
		return
	}
	ee.mu.Lock()
	defer ee.mu.Unlock()

	errType := classifyError(traj.Error)
	fix := suggestFix(errType, traj.Error)
	content := fmt.Sprintf("[反事实] %s 执行「%s」失败(%s), 如果采用以下策略可能更好: %s",
		traj.Role, truncateResult(traj.Objective, 60), errType, fix)

	if !ee.isDuplicate(content) {
		ee.nextID++
		ee.experiences = append(ee.experiences, &Experience{
			ID:        fmt.Sprintf("exp-cf-%d-%d", time.Now().Unix(), ee.nextID),
			Category:  "general",
			Role:      traj.Role,
			Content:   content,
			Quality:   0.35,
			Source:    traj.TeamName + "/" + traj.StageName,
			Tags:      []string{traj.Role, "counterfactual", errType},
			Pattern:   "strategy",
			Lifecycle: "proposed",
			SourceTeam: traj.TeamName,
			MinHashSig: computeMinHash(evolutionTokenize(strings.ToLower(content)), 64),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})
		ee.totalDistilled++
	}
}

// persistInjections 持久化注入记录。
func (ee *EvolutionEngine) persistInjections() {
	if ee.dataDir == "" {
		return
	}
	_ = os.MkdirAll(ee.dataDir, 0755)
	data, err := json.MarshalIndent(ee.injections, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(ee.dataDir, "injections.json"), data, 0644)
}

// --- RETRIEVE: 检索相关经验 ---

// RetrieveFor 按角色和目标检索相关经验。
// V2: BM25+IDF + 同义词扩展 + UCB探索/利用 + 注入Uplift加权 + 生命周期过滤。
func (ee *EvolutionEngine) RetrieveFor(role, objective string, topK int) []*Experience {
	if topK <= 0 {
		topK = 5
	}

	ee.mu.RLock()
	defer ee.mu.RUnlock()

	if len(ee.experiences) == 0 {
		return nil
	}

	// V2: 同义词扩展查询词
	queryTerms := evolutionTokenize(objective)
	queryTerms = expandSynonyms(queryTerms)
	if len(queryTerms) == 0 {
		return nil
	}

	docCount := float64(len(ee.experiences))
	docFreq := make(map[string]int)
	allDocTerms := make([][]string, len(ee.experiences))
	for i, exp := range ee.experiences {
		terms := evolutionTokenize(exp.Content + " " + strings.Join(exp.Tags, " "))
		allDocTerms[i] = terms
		seen := make(map[string]bool)
		for _, t := range terms {
			if !seen[t] {
				docFreq[t]++
				seen[t] = true
			}
		}
	}

	type scored struct {
		exp   *Experience
		score float64
	}
	var candidates []scored

	for i, exp := range ee.experiences {
		// V2: 生命周期过滤 — 仅检索 validated/promoted/active
		if exp.Lifecycle == "archived" || exp.Lifecycle == "decaying" {
			continue
		}
		if exp.Quality < 0.05 {
			continue
		}

		expTerms := allDocTerms[i]
		bm25 := bm25WithIDF(queryTerms, expTerms, docFreq, docCount)
		if bm25 < 0.005 {
			continue
		}

		roleBoost := 1.0
		if role != "" && strings.EqualFold(exp.Role, role) {
			roleBoost = 2.0
		}
		if exp.Category == "general" {
			roleBoost = math.Max(roleBoost, 1.3)
		}

		qualityWeight := 0.5 + exp.Quality*0.5
		successBoost := 1.0 + exp.SuccessRate()*0.5

		// V2: 注入效果加权 (参考 Live-Evo 动态权重)
		upliftBoost := 1.0
		if exp.InjectionCount >= 3 {
			upliftBoost = 0.5 + exp.InjectionUplift()
		}

		// V2: 生命周期加权 — promoted/active 经验加分
		lifecycleBoost := 1.0
		switch exp.Lifecycle {
		case "active":
			lifecycleBoost = 1.3
		case "promoted":
			lifecycleBoost = 1.2
		case "proposed":
			lifecycleBoost = 0.8
		}

		hoursSince := time.Since(exp.UpdatedAt).Hours()
		freshness := 1.0 / (1.0 + hoursSince/720.0)

		score := bm25 * roleBoost * qualityWeight * successBoost * upliftBoost * lifecycleBoost * (1.0 + freshness)

		// V2: UCB 探索加分 (参考 Bandit: 少使用的经验获得探索奖励)
		if ee.totalSelections > 0 && exp.SelectionCount >= 0 {
			c := 1.0
			ucbBonus := c * math.Sqrt(math.Log(float64(ee.totalSelections+1))/float64(exp.SelectionCount+1))
			score += ucbBonus * 0.1
		}

		candidates = append(candidates, scored{exp, score})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// V2: MinHash 加速去重 (替代部分 Jaccard)
	var deduped []scored
	for _, c := range candidates {
		isDup := false
		for _, d := range deduped {
			if len(c.exp.MinHashSig) > 0 && len(d.exp.MinHashSig) > 0 {
				if minHashSimilarity(c.exp.MinHashSig, d.exp.MinHashSig) > 0.5 {
					isDup = true
					break
				}
			} else if jaccardSimilarity(strings.ToLower(c.exp.Content), strings.ToLower(d.exp.Content)) > 0.6 {
				isDup = true
				break
			}
		}
		if !isDup {
			deduped = append(deduped, c)
		}
		if len(deduped) >= topK {
			break
		}
	}

	result := make([]*Experience, len(deduped))
	for i, c := range deduped {
		c.exp.UsageCount++
		c.exp.SelectionCount++
		c.exp.UpdatedAt = time.Now()
		result[i] = c.exp
	}
	ee.totalSelections += len(result)
	return result
}

// synonymMap 中英文同义词表 (V2: 检索增强)。
var synonymMap = map[string][]string{
	"error":       {"错误", "异常", "失败", "bug"},
	"handling":    {"处理", "解决", "修复"},
	"concurrent":  {"并发", "并行", "goroutine"},
	"timeout":     {"超时", "deadline"},
	"api":         {"接口", "endpoint", "服务"},
	"database":    {"数据库", "存储", "db"},
	"test":        {"测试", "验证", "检验"},
	"performance": {"性能", "优化", "效率"},
	"security":    {"安全", "权限", "认证"},
	"design":      {"设计", "架构", "方案"},
}

// expandSynonyms 同义词扩展查询词 (V2 P4)。
func expandSynonyms(terms []string) []string {
	expanded := make([]string, 0, len(terms)*2)
	seen := make(map[string]bool)
	for _, t := range terms {
		if !seen[t] {
			expanded = append(expanded, t)
			seen[t] = true
		}
		if syns, ok := synonymMap[t]; ok {
			for _, syn := range syns {
				synTokens := evolutionTokenize(syn)
				for _, st := range synTokens {
					if !seen[st] {
						expanded = append(expanded, st)
						seen[st] = true
					}
				}
			}
		}
	}
	return expanded
}

// FormatForPrompt 格式化经验为 Agent 可用的 prompt 段。
func FormatExperiencesForPrompt(experiences []*Experience) string {
	if len(experiences) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n<learned_experiences>\n")
	sb.WriteString("以下是从过往执行中提炼的经验教训, 请参考:\n\n")

	for _, exp := range experiences {
		icon := "💡"
		switch exp.Category {
		case "error":
			icon = "⚠️"
		case "role":
			icon = "🎯"
		}

		sb.WriteString(fmt.Sprintf("%s %s", icon, exp.Content))
		if exp.SuccessRate() > 0.7 {
			sb.WriteString(" (高成功率)")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("</learned_experiences>\n")
	return sb.String()
}

// --- EVOLVE: 反馈 + 质量更新 ---

// boolFeedbackScore 把旧的二值反馈映射到 [-1,1] 分轴 (成功 +1 / 失败 -1),
// 保证"加权分路径"与"二值路径"在端点上完全等价, 老调用方行为不变。
func boolFeedbackScore(success bool) float64 {
	if success {
		return 1
	}
	return -1
}

// scoreToEMAReward 把 [-1,1] 加权奖励分映射到经验质量 EMA 的 reward 轴。
// 端点与旧二值语义严格一致: score=+1 → 1.0 (成功), score=-1 → -0.1
// (失败惩罚刻意很轻, 防一次失败就抹掉一条高质经验)。
// 中间线性: 正分按分给, 负分按 1/10 衰减惩罚。
func scoreToEMAReward(score float64) float64 {
	score = clampReward(score)
	if score >= 0 {
		return score
	}
	return 0.1 * score
}

// RecordFeedback 记录经验使用反馈 (v2: 质量可降)。
// 使用 EMA 更新质量分。失败时 reward<0 会降低质量，多次失败会快速淘汰低质经验。
// 参考: 人类学习中的"负强化" — 错误经验反复验证为无效时应被遗忘。
//
// 二值入口保留: 未接奖励源的调用方 (swarm 等) 继续用它, 语义等价于 ±1 分。
func (ee *EvolutionEngine) RecordFeedback(expID string, success bool) {
	ee.RecordFeedbackScored(expID, boolFeedbackScore(success))
}

// RecordFeedbackScored 按 RewardBus 加权分记录经验使用反馈 (design/03 §4.3a)。
// score ∈ [-1,1]; >0 视为该次使用成功 (计入 SuccessCount)。
func (ee *EvolutionEngine) RecordFeedbackScored(expID string, score float64) {
	if ee == nil {
		return
	}
	ee.mu.Lock()
	defer ee.mu.Unlock()

	reward := scoreToEMAReward(score)
	for _, exp := range ee.experiences {
		if exp.ID == expID {
			exp.UsageCount++
			if score > 0 {
				exp.SuccessCount++
			}
			alpha := 0.3
			exp.Quality = (1-alpha)*exp.Quality + alpha*reward
			if exp.Quality < 0 {
				exp.Quality = 0
			}
			exp.UpdatedAt = time.Now()
			break
		}
	}
}

// RecordBatchFeedback 批量更新: 检索到的经验用于了某次执行, 按结果反馈。
func (ee *EvolutionEngine) RecordBatchFeedback(expIDs []string, success bool) {
	ee.RecordBatchFeedbackScored(expIDs, boolFeedbackScore(success))
}

// RecordBatchFeedbackScored 批量更新 (加权分版本)。
func (ee *EvolutionEngine) RecordBatchFeedbackScored(expIDs []string, score float64) {
	for _, id := range expIDs {
		ee.RecordFeedbackScored(id, score)
	}
}

// --- CONSOLIDATE: 去重 + 剪枝 + 晋升 ---

// Consolidate 整理经验库 (V2: MinHash+LSH去重 + 生命周期状态机 + 晋升/淘汰)。
func (ee *EvolutionEngine) Consolidate() {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	if len(ee.experiences) == 0 {
		return
	}

	before := len(ee.experiences)

	// 1. 生命周期状态转换 + 时间衰减
	for _, exp := range ee.experiences {
		ageDays := time.Since(exp.CreatedAt).Hours() / 24

		// Ebbinghaus 遗忘曲线: 未使用经验质量衰减
		if exp.UsageCount == 0 && ageDays > 7 {
			decay := 0.05 * (ageDays / 7)
			exp.Quality -= decay
			if exp.Quality < 0 {
				exp.Quality = 0
			}
		}

		// 生命周期状态机
		switch exp.Lifecycle {
		case "proposed":
			if exp.UsageCount > 0 && exp.SuccessRate() >= 0.3 {
				exp.Lifecycle = "validated"
			} else if ageDays > 7 && exp.UsageCount == 0 {
				exp.Lifecycle = "archived"
			}
		case "validated":
			if exp.InjectionCount >= 3 && exp.InjectionUplift() >= 0.5 {
				exp.Lifecycle = "promoted"
			} else if exp.Quality < 0.15 {
				exp.Lifecycle = "decaying"
			}
		case "promoted":
			if exp.UsageCount >= 5 && exp.SuccessRate() >= 0.5 {
				exp.Lifecycle = "active"
			} else if exp.Quality < 0.1 {
				exp.Lifecycle = "decaying"
			}
		case "active":
			if exp.Quality < 0.1 || (exp.UsageCount == 0 && ageDays > 30) {
				exp.Lifecycle = "decaying"
			}
		case "decaying":
			if ageDays > 3 && exp.Quality < 0.05 {
				exp.Lifecycle = "archived"
			}
		}

		// 确保 MinHash 签名存在
		if len(exp.MinHashSig) == 0 {
			exp.MinHashSig = computeMinHash(evolutionTokenize(exp.Content), 64)
		}
	}

	// 2. 剪枝: 移除 archived 和低质量经验
	var kept []*Experience
	for _, exp := range ee.experiences {
		if exp.Lifecycle == "archived" {
			continue
		}
		ageDays := time.Since(exp.CreatedAt).Hours() / 24
		if exp.Quality < 0.05 && ageDays > 3 {
			continue
		}
		if exp.UsageCount == 0 && ageDays > 14 {
			continue
		}
		kept = append(kept, exp)
	}

	// 3. MinHash+LSH 去重 (V2: O(n*k) 替代 O(n²))
	buckets := make(map[uint64][]int) // LSH 桶 → 候选索引
	bands, rows := 8, 8              // 64 hash → 8 bands * 8 rows
	for i, exp := range kept {
		if len(exp.MinHashSig) < bands*rows {
			continue
		}
		for b := 0; b < bands; b++ {
			h := fnvHash64(exp.MinHashSig[b*rows : (b+1)*rows])
			buckets[h] = append(buckets[h], i)
		}
	}

	merged := make(map[int]bool)
	var deduped []*Experience
	for i := 0; i < len(kept); i++ {
		if merged[i] {
			continue
		}
		best := kept[i]
		// 仅在同桶内比较
		for b := 0; b < bands && len(best.MinHashSig) >= bands*rows; b++ {
			h := fnvHash64(best.MinHashSig[b*rows : (b+1)*rows])
			for _, j := range buckets[h] {
				if j <= i || merged[j] {
					continue
				}
				sim := minHashSimilarity(kept[i].MinHashSig, kept[j].MinHashSig)
				if sim > 0.6 {
					merged[j] = true
					if kept[j].Quality > best.Quality {
						best = kept[j]
						merged[i] = true
					}
					best.UsageCount += kept[j].UsageCount
					best.SuccessCount += kept[j].SuccessCount
				}
			}
		}
		deduped = append(deduped, best)
	}

	// 4. 限制总量 (按综合分排序)
	if len(deduped) > 200 {
		sort.Slice(deduped, func(i, j int) bool {
			si := deduped[i].Quality*0.5 + deduped[i].SuccessRate()*0.3 + deduped[i].InjectionUplift()*0.2
			sj := deduped[j].Quality*0.5 + deduped[j].SuccessRate()*0.3 + deduped[j].InjectionUplift()*0.2
			return si > sj
		})
		deduped = deduped[:200]
	}

	ee.experiences = deduped
	ee.persistExperiences()

	after := len(ee.experiences)
	if before != after {
		log.Printf("[Evolution] 整理: %d → %d 条经验 (剪枝 %d)", before, after, before-after)
	}
}

// --- 持久化 ---

func (ee *EvolutionEngine) persistExperiences() {
	if ee.dataDir == "" {
		return
	}
	os.MkdirAll(ee.dataDir, 0755)
	data, err := json.MarshalIndent(ee.experiences, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(ee.dataDir, "experiences.json"), data, 0644)
}

func (ee *EvolutionEngine) persistTrajectories() {
	if ee.dataDir == "" {
		return
	}
	os.MkdirAll(ee.dataDir, 0755)
	data, err := json.MarshalIndent(ee.trajectories, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(ee.dataDir, "trajectories.json"), data, 0644)
}

func (ee *EvolutionEngine) load() {
	if ee.dataDir == "" {
		return
	}

	if data, err := os.ReadFile(filepath.Join(ee.dataDir, "experiences.json")); err == nil {
		_ = json.Unmarshal(data, &ee.experiences)
	}
	if data, err := os.ReadFile(filepath.Join(ee.dataDir, "trajectories.json")); err == nil {
		_ = json.Unmarshal(data, &ee.trajectories)
	}
	// V2: 加载注入记录
	if data, err := os.ReadFile(filepath.Join(ee.dataDir, "injections.json")); err == nil {
		_ = json.Unmarshal(data, &ee.injections)
	}
}

// Stats 统计信息。
func (ee *EvolutionEngine) Stats() EvolutionStats {
	ee.mu.RLock()
	defer ee.mu.RUnlock()

	stats := EvolutionStats{
		TotalExperiences:  len(ee.experiences),
		TotalTrajectories: len(ee.trajectories),
	}

	totalQ := 0.0
	for _, exp := range ee.experiences {
		switch exp.Category {
		case "role":
			stats.RoleExperiences++
		case "error":
			stats.ErrorPatterns++
		case "general":
			stats.GeneralPrinciples++
		}
		stats.TotalUsageCount += exp.UsageCount
		totalQ += exp.Quality
	}
	if len(ee.experiences) > 0 {
		stats.AvgQuality = totalQ / float64(len(ee.experiences))
	}

	return stats
}

// EvolutionStats 进化统计。
type EvolutionStats struct {
	TotalExperiences  int `json:"totalExperiences"`
	TotalTrajectories int `json:"totalTrajectories"`
	RoleExperiences   int `json:"roleExperiences"`
	ErrorPatterns     int `json:"errorPatterns"`
	GeneralPrinciples int `json:"generalPrinciples"`
	TotalUsageCount   int `json:"totalUsageCount"`   // 经验被注入的总次数
	AvgQuality        float64 `json:"avgQuality"`    // 平均质量分
}

// DataDir 返回进化数据目录 (<state>/evolution)。
//
// 供 pkg/evolution/learners 与 evo 操作台定位 rewards.jsonl / trajectories.json ——
// 它们读的是"格式即契约"的磁盘文件, 但路径这件事只有引擎自己知道。
func (ee *EvolutionEngine) DataDir() string {
	if ee == nil {
		return ""
	}
	return ee.dataDir
}

// CollectMetrics 采集进化引擎全部 18 项持续观测指标。
func (ee *EvolutionEngine) CollectMetrics(c interface{ Record(module, name string, value float64) }) {
	if c == nil {
		return
	}
	ee.mu.RLock()
	defer ee.mu.RUnlock()

	// 基础计数
	c.Record("evolution", "evo_experience_count", float64(len(ee.experiences)))
	c.Record("evolution", "evo_trajectory_count", float64(len(ee.trajectories)))

	// === 维度一: 学习质量 (Learn) ===

	// #1 evo_distill_rate: 每条轨迹产出的经验数
	if len(ee.trajectories) > 0 {
		c.Record("evolution", "evo_distill_rate", float64(len(ee.experiences))/float64(len(ee.trajectories)))
	}

	// #2 evo_survival_rate: 非 archived 的经验占历史提炼总数
	if ee.totalDistilled > 0 {
		alive := 0
		for _, e := range ee.experiences {
			if e.Lifecycle != "archived" {
				alive++
			}
		}
		c.Record("evolution", "evo_survival_rate", float64(alive)/float64(ee.totalDistilled))
	}

	// #3 evo_pattern_diversity: 不同 Pattern 类型的数量
	patterns := map[string]bool{}
	for _, e := range ee.experiences {
		if e.Pattern != "" {
			patterns[e.Pattern] = true
		}
	}
	c.Record("evolution", "evo_pattern_diversity", float64(len(patterns)))

	// === 维度二: 检索效能 (Retrieve) ===

	// #4 evo_retrieval_hit_rate: 注入后任务成功占比
	injTotal, injSuccess := 0, 0
	for _, r := range ee.injections {
		if len(r.ExpIDs) > 0 {
			injTotal++
			if r.Success {
				injSuccess++
			}
		}
	}
	if injTotal > 0 {
		c.Record("evolution", "evo_retrieval_hit_rate", float64(injSuccess)/float64(injTotal))
	}

	// #6 evo_retrieval_coverage: 被使用过的经验占比
	usedCount := 0
	for _, e := range ee.experiences {
		if e.UsageCount > 0 {
			usedCount++
		}
	}
	if len(ee.experiences) > 0 {
		c.Record("evolution", "evo_retrieval_coverage", float64(usedCount)/float64(len(ee.experiences)))
	}

	// === 维度三: 进化效果 (Evolve) ===

	// #7 evo_task_success_trend: 最近 N 条轨迹成功率
	succCount, failCount := 0, 0
	window := ee.trajectories
	if len(window) > 50 {
		window = window[len(window)-50:]
	}
	for _, t := range window {
		if t.Success {
			succCount++
		} else {
			failCount++
		}
	}
	total := succCount + failCount
	if total > 0 {
		c.Record("evolution", "evo_task_success_trend", float64(succCount)/float64(total))
	}

	// #8 evo_injection_uplift
	if injTotal > 0 {
		injRate := float64(injSuccess) / float64(injTotal)
		baseline := ee.baselineSuccess
		if baseline == 0 {
			baseline = 0.5
		}
		c.Record("evolution", "evo_injection_uplift", injRate-baseline)
	}

	// #9 evo_error_recurrence: 最近轨迹中同类错误重复率
	errTypes := map[string]int{}
	recentTrajs := ee.trajectories
	if len(recentTrajs) > 100 {
		recentTrajs = recentTrajs[len(recentTrajs)-100:]
	}
	for _, t := range recentTrajs {
		if !t.Success && t.Error != "" {
			errTypes[classifyError(t.Error)]++
		}
	}
	maxRecur := 0
	for _, cnt := range errTypes {
		if cnt > maxRecur {
			maxRecur = cnt
		}
	}
	c.Record("evolution", "evo_error_recurrence", float64(maxRecur))

	// === 维度四: 效率指标 (Efficiency) ===
	// #13 evo_growth_rate: 经验增长率 (本周新增/总量)
	weekAgo := time.Now().Add(-7 * 24 * time.Hour)
	newThisWeek := 0
	for _, e := range ee.experiences {
		if e.CreatedAt.After(weekAgo) {
			newThisWeek++
		}
	}
	if len(ee.experiences) > 0 {
		c.Record("evolution", "evo_growth_rate", float64(newThisWeek)/float64(len(ee.experiences)))
	}

	// === 维度五: 泛化能力 (Generalization) ===

	// #14 evo_cross_team_transfer: 跨团队使用率
	crossTeamUse := 0
	totalUse := 0
	for _, r := range ee.injections {
		for _, id := range r.ExpIDs {
			totalUse++
			for _, e := range ee.experiences {
				if e.ID == id && e.SourceTeam != "" && e.SourceTeam != r.TeamName {
					crossTeamUse++
					break
				}
			}
		}
	}
	if totalUse > 0 {
		c.Record("evolution", "evo_cross_team_transfer", float64(crossTeamUse)/float64(totalUse))
	}

	// #16 evo_abstraction_rate: general 经验占比
	generalCount := 0
	for _, e := range ee.experiences {
		if e.Category == "general" {
			generalCount++
		}
	}
	if len(ee.experiences) > 0 {
		c.Record("evolution", "evo_abstraction_rate", float64(generalCount)/float64(len(ee.experiences)))
	}

	// #17 evo_lifecycle_promoted: promoted+active 占比
	promotedCount := 0
	for _, e := range ee.experiences {
		if e.Lifecycle == "promoted" || e.Lifecycle == "active" {
			promotedCount++
		}
	}
	if len(ee.experiences) > 0 {
		c.Record("evolution", "evo_lifecycle_promoted", float64(promotedCount)/float64(len(ee.experiences)))
	}

	// #18 evo_counterfactual_gen: 反事实经验数
	cfCount := 0
	for _, e := range ee.experiences {
		for _, tag := range e.Tags {
			if tag == "counterfactual" {
				cfCount++
				break
			}
		}
	}
	c.Record("evolution", "evo_counterfactual_gen", float64(cfCount))

	// 保留原有基础指标
	allSucc, allFail := 0, 0
	for _, t := range ee.trajectories {
		if t.Success {
			allSucc++
		} else {
			allFail++
		}
	}
	if allSucc+allFail > 0 {
		c.Record("evolution", "evo_success_rate", float64(allSucc)/float64(allSucc+allFail))
		c.Record("evolution", "evo_fail_trajectory_pct", float64(allFail)/float64(allSucc+allFail))
	}

	var qualSum, qualMin, qualMax float64
	qualMin = 1.0
	for i, exp := range ee.experiences {
		qualSum += exp.Quality
		if i == 0 || exp.Quality < qualMin {
			qualMin = exp.Quality
		}
		if exp.Quality > qualMax {
			qualMax = exp.Quality
		}
	}
	if len(ee.experiences) > 0 {
		c.Record("evolution", "evo_utilization_rate", float64(usedCount)/float64(len(ee.experiences)))
		c.Record("evolution", "evo_avg_quality", qualSum/float64(len(ee.experiences)))
		c.Record("evolution", "evo_quality_min", qualMin)
		c.Record("evolution", "evo_quality_max", qualMax)
	}
}

// --- 工具函数 ---

// evolutionTokenize 中英文混合分词。
// 英文: 按空格/标点分割为单词。
// 中文: 无天然分隔符, 使用字符 unigram + bigram 策略 (无需分词器也能有效检索)。
func evolutionTokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var latin strings.Builder
	var cjkChars []rune

	flushLatin := func() {
		if latin.Len() > 1 {
			tokens = append(tokens, latin.String())
		}
		latin.Reset()
	}
	flushCJK := func() {
		// CJK unigrams
		for _, c := range cjkChars {
			tokens = append(tokens, string(c))
		}
		// CJK bigrams (更有意义的中文匹配)
		for i := 0; i+1 < len(cjkChars); i++ {
			tokens = append(tokens, string(cjkChars[i])+string(cjkChars[i+1]))
		}
		cjkChars = cjkChars[:0]
	}

	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fff {
			flushLatin()
			cjkChars = append(cjkChars, r)
		} else if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			flushCJK()
			latin.WriteRune(r)
		} else {
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()
	return tokens
}

// bm25WithIDF BM25 with proper IDF weighting (replaces simpleBM25 for retrieval).
func bm25WithIDF(queryTerms, docTerms []string, docFreq map[string]int, totalDocs float64) float64 {
	if len(docTerms) == 0 {
		return 0
	}
	tf := make(map[string]int)
	for _, t := range docTerms {
		tf[t]++
	}

	k1 := 1.5
	b := 0.75
	avgDL := 30.0
	dl := float64(len(docTerms))

	score := 0.0
	queryTF := make(map[string]int)
	for _, qt := range queryTerms {
		queryTF[qt]++
	}

	matchedTerms := 0
	for qt, qtf := range queryTF {
		if tf[qt] > 0 {
			matchedTerms++
			dtf := float64(tf[qt])
			tfScore := (dtf * (k1 + 1)) / (dtf + k1*(1-b+b*(dl/avgDL)))
			// IDF: log((N - df + 0.5) / (df + 0.5) + 1)
			df := float64(docFreq[qt])
			idf := math.Log((totalDocs-df+0.5)/(df+0.5) + 1.0)
			score += tfScore * idf * float64(qtf)
		}
	}

	if len(queryTF) > 0 {
		coverage := float64(matchedTerms) / float64(len(queryTF))
		score *= (1.0 + coverage)
	}

	return score
}

func simpleBM25(queryTerms, docTerms []string) float64 {
	if len(docTerms) == 0 {
		return 0
	}
	tf := make(map[string]int)
	for _, t := range docTerms {
		tf[t]++
	}

	k1 := 1.5
	b := 0.75
	avgDL := 30.0
	dl := float64(len(docTerms))

	score := 0.0
	queryTF := make(map[string]int)
	for _, qt := range queryTerms {
		queryTF[qt]++
	}

	matchedTerms := 0
	for qt, qtf := range queryTF {
		if tf[qt] > 0 {
			matchedTerms++
			dtf := float64(tf[qt])
			// BM25 TF saturation with doc length normalization
			tfScore := (dtf * (k1 + 1)) / (dtf + k1*(1-b+b*(dl/avgDL)))
			// query term frequency boost
			score += tfScore * float64(qtf)
		}
	}

	// coverage bonus: reward documents that match more query terms
	if len(queryTF) > 0 {
		coverage := float64(matchedTerms) / float64(len(queryTF))
		score *= (1.0 + coverage)
	}

	return score
}

// smartTruncateEvolution 智能截断: 保留前部+后部，中间用省略号连接。
// 避免只保留开头导致丢失关键的输出/错误信息。
func smartTruncateEvolution(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	headLen := maxLen * 2 / 3
	tailLen := maxLen - headLen - 30
	if tailLen < 100 {
		tailLen = 100
		headLen = maxLen - tailLen - 30
	}
	return s[:headLen] + "\n...(truncated middle)...\n" + s[len(s)-tailLen:]
}

// computeMinHash 计算 MinHash 签名 (V2: 替代 O(n²) Jaccard)。
// k 个哈希函数, 每个取所有 shingle 的最小哈希值。
func computeMinHash(tokens []string, k int) []uint64 {
	if len(tokens) == 0 {
		return nil
	}
	sig := make([]uint64, k)
	for i := range sig {
		sig[i] = ^uint64(0) // max uint64
	}
	for _, t := range tokens {
		for i := 0; i < k; i++ {
			h := fnvHashString(t, uint64(i*0x517cc1b727220a95+0x6c62272e07bb0142))
			if h < sig[i] {
				sig[i] = h
			}
		}
	}
	return sig
}

// minHashSimilarity 估算 MinHash 签名的 Jaccard 相似度。
func minHashSimilarity(a, b []uint64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	match := 0
	for i := 0; i < n; i++ {
		if a[i] == b[i] {
			match++
		}
	}
	return float64(match) / float64(n)
}

// fnvHashString FNV-1a 哈希 (带 seed)。
func fnvHashString(s string, seed uint64) uint64 {
	h := seed ^ 0xcbf29ce484222325
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 0x100000001b3
	}
	return h
}

// fnvHash64 对 uint64 切片做 FNV-1a (用于 LSH 桶)。
func fnvHash64(vals []uint64) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, v := range vals {
		h ^= v
		h *= 0x100000001b3
	}
	return h
}

func jaccardSimilarity(a, b string) float64 {
	tokensA := evolutionTokenize(a)
	tokensB := evolutionTokenize(b)
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return 0
	}

	setA := make(map[string]bool)
	for _, t := range tokensA {
		setA[t] = true
	}
	setB := make(map[string]bool)
	for _, t := range tokensB {
		setB[t] = true
	}

	intersection := 0
	for t := range setA {
		if setB[t] {
			intersection++
		}
	}

	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
