// Package evolution — ledger.go：13.8.3「ledger 折叠器」（吸收 13.6 F8）。
//
// 设计核心（docforge planning-evo-track.html §13.8.3）：
//
//	事件账本不可变，一切指标由账本折叠派生。指标 = 纯函数（FoldSpec），账本 = 唯一输入；
//	Fold(t0, t1) 对同一账本区间确定性；定义修正 → 改 From/Acc → 全历史重算。
//
// 账本即仓内既有数据源，不新增采集管线：
//
//	<state>/metrics/*.jsonl          — ts 为 RFC3339 字符串（time.Time 序列化）
//	<state>/evolution/rewards.jsonl  — ts 为 unix-milli int64
//	<state>/evolution/experiments/   — 实验记录（canary 胜率、uplift）
//	<state>/skills/*/SKILL.md        — frontmatter audit 行谱系（晋升生存审计）
//
// 双 ts 格式由 Entry 统一吸收：解析时两种都认，Fold 内按统一 unix-nano 比较。
// 每个 FoldSpec 必须带单测锁定语义（输入样例 → 期望输出），治「指标口径漂移」。
//
// 折叠指标的两条纪律：
//  1. From 返回 NaN 表示「该条不参与本指标」——绝不用 0 表示排除，否则 sum 类
//     指标会把无关行计成 0 值事件、max 类会把下界拉到 0。
//  2. Fold 返回值是「区间折叠值」而非「最新快照」；gauge 型指标（如 learning_cost_ratio）
//     由调用方把折叠值写成 gauge 快照，ledger 只负责纯函数部分。
package evolution

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/learners"
)

// maxLedgerLineBytes 单行账本条目扫描上限。账本行都是小 JSON，超过即视为损坏行
// （与 skillaudit.loadRewards 的 4MiB scanner 同思想）。
const maxLedgerLineBytes = 4 << 20

// Entry 账本条目：折叠器统一输入单元，字段为既有账本行字段的并集。
type Entry struct {
	TS     int64             // unix-nano（双格式解析后统一）
	Module string            // metrics/*.jsonl 行的 module
	Name   string            // metrics 行的 name / rewards 行的 source（见 Source）
	Source string            // rewards 行 source（gate.compile 等）；metrics 行为空
	Value  float64           // 数值
	Labels map[string]string // metrics 行 labels（llm.jsonl: model/source/status 等）
	RunID  string
	NodeID string // rewards 行 node_id
	Team   string
	Weight float64 // rewards 行 weight
	Raw    float64 // rewards 行 raw（未 clamp 原值）
	Line   string  // 原始 JSON 行（From 回调需要更细字段时自行二次解码）
}

// FoldSpec 指标折叠规格：指标 = From(Entry) → Acc 纯函数。
type FoldSpec struct {
	Name string                       // 指标名（如 "evo_avg_quality"）
	From func(Entry) float64          // 提取；返回 NaN 表示该条不参与本指标
	Acc  func(acc, v float64) float64 // 折叠（初值 Seed，首个值即 Acc(seed, v)）
}

// ---------------------------------------------------------------------------
// 账本读取
// ---------------------------------------------------------------------------

// readMetricsLedger 读 <state>/metrics/*.jsonl 的全部行（ts=RFC3339 字符串）。
// 损坏行静默跳过（账本不可变原则：读取器不写不删，只解释）。
func readMetricsLedger(stateDir string) []Entry {
	dir := filepath.Join(stateDir, "metrics")
	ents, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	sort.Strings(ents) // 文件序确定性（多文件覆盖同名时间戳时跨文件序固定）
	var out []Entry
	for _, path := range ents {
		out = append(out, readMetricsFile(path)...)
	}
	return out
}

// readMetricsFile 读单个 metrics jsonl。
func readMetricsFile(path string) []Entry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLedgerLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			TS     json.RawMessage   `json:"ts"`
			Module string            `json:"module"`
			Name   string            `json:"name"`
			Value  float64           `json:"value"`
			Labels map[string]string `json:"labels"`
			RunID  string            `json:"run_id"`
			Team   string            `json:"team"`
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		ts := parseEntryTS(row.TS)
		if ts == 0 {
			continue
		}
		out = append(out, Entry{
			TS: ts, Module: row.Module, Name: row.Name, Value: row.Value,
			Labels: row.Labels, RunID: row.RunID, Team: row.Team, Line: line,
		})
	}
	return out
}

// readRewardsLedger 读 <state>/evolution/rewards.jsonl（ts=unix-milli int64）。
// 与 learners.LoadRewards 的差异：保留 weight/raw/team/node_id 全字段 + 双格式 ts，
// 供 FoldSpec 按需取用；行损坏静默跳过。
func readRewardsLedger(stateDir string) []Entry {
	path := filepath.Join(stateDir, "evolution", "rewards.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLedgerLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			TS     json.RawMessage `json:"ts"`
			RunID  string          `json:"run_id"`
			NodeID string          `json:"node_id"`
			Source string          `json:"source"`
			Value  float64         `json:"value"`
			Weight float64         `json:"weight"`
			Raw    float64         `json:"raw"`
			Team   string          `json:"team"`
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		ts := parseEntryTS(row.TS)
		if ts == 0 {
			continue
		}
		out = append(out, Entry{
			TS: ts, RunID: row.RunID, NodeID: row.NodeID, Source: row.Source,
			Value: row.Value, Weight: row.Weight, Raw: row.Raw, Team: row.Team, Line: line,
		})
	}
	return out
}

// parseEntryTS 双格式 ts 解析：RFC3339 字符串 → unix-nano；unix-milli int → unix-nano。
// 返回 0 = 无法解析（调用方跳过该行）。
func parseEntryTS(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UnixNano()
			}
		}
		return 0
	}
	var ms int64
	if json.Unmarshal(raw, &ms) == nil {
		return ms * int64(time.Millisecond)
	}
	return 0
}

// readLedger 全量账本：metrics + rewards，按 ts 升序（稳定排序 → 同 ts 行序确定）。
func readLedger(stateDir string) []Entry {
	all := append(readMetricsLedger(stateDir), readRewardsLedger(stateDir)...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	return all
}

// Fold 对 [from, to) 区间跑全部 specs，返回 指标名 → 折叠值。
//
// 确定性保证：账本文件序固定 + 行序固定 + 稳定排序 → 同一账本区间同一结果。
// 无一 spec 命中任何条目时该指标缺失（返回的 map 无键）——调用方以「键缺失=无数据」
// 处理，不落 0（0 是有意义的折叠值，不是无数据）。
func Fold(stateDir string, from, to time.Time, specs ...FoldSpec) map[string]float64 {
	entries := readLedger(stateDir)
	return foldEntries(entries, from, to, specs...)
}

// foldEntries Fold 的纯函数核心（账本切片已在外部读好）——单测直接打这里。
func foldEntries(entries []Entry, from, to time.Time, specs ...FoldSpec) map[string]float64 {
	lo, hi := from.UnixNano(), to.UnixNano()
	out := make(map[string]float64, len(specs))
	if len(specs) == 0 {
		return out
	}
	// 区间内条目只过一遍，全部 spec 共享（O(N·specs) 而非 O(N·specs·读盘)）。
	for _, e := range entries {
		if e.TS < lo || e.TS >= hi {
			continue
		}
		for _, spec := range specs {
			v := spec.From(e)
			if math.IsNaN(v) {
				continue
			}
			prev, seen := out[spec.Name]
			if !seen {
				// 首个命中值：以 0 为种子跑一次 Acc —— Acc 必须对 acc=0 良定义
				// （sum/mean/max/min/count 类全部满足；这就把 Seed 从契约里省掉了）。
				out[spec.Name] = spec.Acc(0, v)
				continue
			}
			out[spec.Name] = spec.Acc(prev, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 内置 FoldSpec（13.8.2 六新指标 + 基础均值）
// ---------------------------------------------------------------------------

// FoldLearnLLMTokens L1: learn_llm_tokens —— 学习路径 LLM token 消耗。
// 口径：llm.jsonl 中 labels.source == "evolution" 的 llm_total_tokens 求和。
// 产方：学习路径 LLM 调用 ctx 带 WithLLMMetrics{Source:"evolution"}（13.8-P0 通电）。
func FoldLearnLLMTokens() FoldSpec {
	return FoldSpec{
		Name: "learn_llm_tokens",
		From: func(e Entry) float64 {
			if e.Module != "llm" || e.Name != "llm_total_tokens" || e.Labels["source"] != "evolution" {
				return math.NaN()
			}
			return e.Value
		},
		Acc: func(acc, v float64) float64 { return acc + v },
	}
}

// FoldTotalLLMTokens L1: 全部 LLM token 消耗（learning_cost_ratio 分母）。
func FoldTotalLLMTokens() FoldSpec {
	return FoldSpec{
		Name: "total_llm_tokens",
		From: func(e Entry) float64 {
			if e.Module != "llm" || e.Name != "llm_total_tokens" {
				return math.NaN()
			}
			return e.Value
		},
		Acc: func(acc, v float64) float64 { return acc + v },
	}
}

// FoldLearningCostRatio L1: learning_cost_ratio = 学习 token ÷ 总 token。
// 分母为 0 时返回 0（无花费即无超支，闸门（≤10%）视为通过——fail-open 口径，
// 与「无数据」键缺失区分开：分母 0 是明确的「没有花费」，ratio=0 是合法快照）。
// 注意：这是两个 spec 的派生指标，须与上述两个 token spec 同批 Fold 后再算；
// 单独 Fold 时若缺 token spec，比值恒 0。
func FoldLearningCostRatio(learn, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return learn / total
}

// FoldRewardDistKS L1: reward_dist_ks —— 奖励分布漂移 KS 统计量。
// 口径：[from, mid) 与 [mid, to) 两窗口的 reward Value（clamp 后）分布的
// 双样本 Kolmogorov–Smirnov D 统计量（权重无关，逐事件计数）。
// 单侧窗口不足 2 个事件时返回 NaN→键缺失（样本不足不判漂移）。
// 用途：hacking 侦测——奖励分布骤变（如普遍满分）说明评估器被投机。
func FoldRewardDistKS() FoldSpec {
	return FoldSpec{
		Name: "reward_dist_ks",
		From: func(e Entry) float64 {
			if e.Source == "" && e.Module == "" {
				return math.NaN()
			}
			if e.Module != "" { // metrics 行不是奖励事件
				return math.NaN()
			}
			return e.Value
		},
		Acc: func(acc, v float64) float64 { return math.NaN() }, // 占位：真值由 ksRewards 二阶段算
	}
}

// FoldRollbackCount L2: rollback_count —— evo_rollback 触发数。
// 口径：evolution.jsonl 中 evo_rollback_count 事件（由 EvoRollbackTool 产方写入）。
func FoldRollbackCount() FoldSpec {
	return FoldSpec{
		Name: "rollback_count",
		From: func(e Entry) float64 {
			if e.Module != "evolution" || e.Name != "evo_rollback_count" {
				return math.NaN()
			}
			return e.Value
		},
		Acc: func(acc, v float64) float64 { return acc + v },
	}
}

// FoldCanaryWinRate L2: canary_win_rate —— 灰度实验 uplift>0 占比。
// 口径：<state>/evolution/experiments/*.json 全部实验，对每个实验以
// learners.SplitByTime(StartedAtMS) 切 rewards，uplift = MeanRunScore(after) - MeanRunScore(before)
// （与 evo_status 展示口径一致），uplift>0 计 1。返回 win/total。
// 注意：这是「实验级」折叠，不逐行——所以不走 From/Acc 主通道，独立函数。
func FoldCanaryWinRate(stateDir string) (wins, total int) {
	expDir := filepath.Join(stateDir, "evolution", "experiments")
	files, _ := filepath.Glob(filepath.Join(expDir, "*.json"))
	if len(files) == 0 {
		return 0, 0
	}
	rows := learners.LoadRewards(filepath.Join(stateDir, "evolution", "rewards.jsonl"))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var exp learners.Experiment
		if json.Unmarshal(data, &exp) != nil {
			continue
		}
		if exp.StartedAtMS <= 0 {
			continue
		}
		base, cand := learners.SplitByTime(rows, exp.StartedAtMS)
		baseScore, baseN := learners.MeanRunScore(base)
		candScore, candN := learners.MeanRunScore(cand)
		if baseN == 0 || candN == 0 {
			continue // 两侧都有样本才算可判定
		}
		total++
		if candScore > baseScore {
			wins++
		}
	}
	return wins, total
}

// FoldPromoteSurvival L2: promote_survival_30d —— 晋升技能 30 天生存率。
// 口径：遍历 <state>/skills/*/SKILL.md frontmatter 的 audit 行谱系
// （`audit: <status>@<RFC3339> reward_avg=... samples=...`，skillaudit.rewriteStatus 写入）。
// 一个技能的谱系中存在 active@T1 且其后 archived@T2（T2-T1 < 30d）→ 该晋升不生存。
// 返回 (survived, total)：total=有过晋升且已可判定的技能数（末次状态 active=生存中，
// 或 archived 且间隔 ≥30d=生存，archived 且间隔 <30d=不生存）。
func FoldPromoteSurvival(stateDir string, now time.Time) (survived, total int) {
	skillRoot := filepath.Join(stateDir, "skills")
	dirs, _ := os.ReadDir(skillRoot)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		path := filepath.Join(skillRoot, d.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		hist := parseAuditLineage(string(data))
		if len(hist) == 0 {
			continue
		}
		// 找最后一次 active（晋升点），看其后的第一段 archived。
		var promoted time.Time
		var retired time.Time
		for _, h := range hist {
			switch h.status {
			case "active":
				promoted = h.at
			case "archived":
				if !promoted.IsZero() && retired.IsZero() {
					retired = h.at
				}
			}
		}
		if promoted.IsZero() {
			continue // 从未晋升，不参与本指标
		}
		total++
		if retired.IsZero() {
			survived++ // 晋升后未退场 → 生存中
			continue
		}
		if retired.Sub(promoted) >= 30*24*time.Hour {
			survived++ // 退场但活过 30 天 → 生存
		}
	}
	_ = now
	return survived, total
}

// auditLineageEntry audit 行谱系的一项。
type auditLineageEntry struct {
	status string
	at     time.Time
}

// parseAuditLineage 从 SKILL.md 全文提取 audit 行谱系（按出现序）。
// audit 行格式（skillaudit.rewriteStatus）：`audit: <status>@<RFC3339> reward_avg=%.3f samples=%d`。
func parseAuditLineage(content string) []auditLineageEntry {
	var out []auditLineageEntry
	for _, ln := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "audit:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "audit:"))
		atIdx := strings.IndexByte(rest, '@')
		if atIdx <= 0 {
			continue
		}
		status := rest[:atIdx]
		tsPart := rest[atIdx+1:]
		if sp := strings.IndexByte(tsPart, ' '); sp > 0 {
			tsPart = tsPart[:sp]
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(tsPart))
		if err != nil {
			continue
		}
		out = append(out, auditLineageEntry{status: status, at: at})
	}
	return out
}

// ---------------------------------------------------------------------------
// 告警阈值（13.8.5，驾驶舱闸门的数据化落地）
// ---------------------------------------------------------------------------

// 告警阈值常量（13.8.5 表格原文值）。
const (
	// LearningCostRatioWarn 学习成本占比告警线（> 0.10 持续 1h 告警——持续性由
	// 驾驶舱按时间窗重算，这里给单点阈值）。
	LearningCostRatioWarn = 0.10
	// RewardDistKSWarn 单日奖励分布漂移 KS 告警线（> 0.3 → 评估器被投机嫌疑）。
	RewardDistKSWarn = 0.3
	// CanaryWinRateFloor 周灰度胜率下限（< 0.4 连续 2 周 → 进化方向性问题）。
	CanaryWinRateFloor = 0.4
	// PromoteSurvivalFloor 30 天晋升生存率下限（< 0.7 → 晋升闸过松）。
	PromoteSurvivalFloor = 0.7
)

// snapshotCacheKey FoldSnapshot 结果缓存的账本指纹：文件数 + 每文件 (名字, 大小, mtime)。
// mtime/size 任一变化（产方 append 一行）→ 指纹变 → 缓存失效。文件名集合也参与
// （新增/删除模块文件同样失效）。
type snapshotCacheKey struct {
	files string
}

// snapshotCache FoldSnapshot 的进程内结果缓存。账本 300MB+ 量级时全量重折叠
// 一次 ~15s，而驾驶舱前端会连续刷新——同一账本指纹直接复用上次快照。
// 账本 append-only，指纹只随新行变化，不会掩盖真实更新。
var snapshotCache = struct {
	mu     sync.Mutex
	key    snapshotCacheKey
	expiresAt time.Time
	value  Snapshot
	has    bool
}{}

// snapshotCacheTTL 缓存有效期。指纹未变时也无条件到期（墙上时钟推进本身改变
// 快照语义：1h 窗口滑动、dayStart 换日），TTL 取分钟级与驾驶舱刷新节奏对齐。
const snapshotCacheTTL = 60 * time.Second

// FoldSnapshot 一次驾驶舱快照：六新指标 + 阈值判定，L1/L2 分层返回。
// 全部由既有账本折叠导出，无新采集管线。
//
// 两次优化（治「进化量化页 15s」）：
//  1. 账本全量只读一次，三个折叠窗口（1h token / 单日 KS / 7d 回滚）共享同一
//     entries 切片走 foldEntries 纯核心——旧实现经 Fold() 各自 readLedger 一遍，
//     300MB 账本读+解析 3 遍。
//  2. 结果缓存：按账本指纹（各 jsonl 的 size+mtime）+ 60s TTL 复用上次快照，
//     连续刷新 / 多个调用方（dashboard handler + agent CollectMetrics）不再
//     重复全量折叠。
func FoldSnapshot(stateDir string, now time.Time) Snapshot {
	key := fingerprintLedger(stateDir)
	snapshotCache.mu.Lock()
	if snapshotCache.has && snapshotCache.key == key && now.Before(snapshotCache.expiresAt) {
		v := snapshotCache.value
		snapshotCache.mu.Unlock()
		return v
	}
	snapshotCache.mu.Unlock()

	snap := foldSnapshotUncached(stateDir, now)

	snapshotCache.mu.Lock()
	snapshotCache.key, snapshotCache.value = key, snap
	snapshotCache.expiresAt = now.Add(snapshotCacheTTL)
	snapshotCache.has = true
	snapshotCache.mu.Unlock()
	return snap
}

// foldSnapshotUncached FoldSnapshot 的无缓存实现（单测与缓存回退用）。
func foldSnapshotUncached(stateDir string, now time.Time) Snapshot {
	snap := Snapshot{Now: now}

	// 账本全量只读一次；窗口折叠（1h / 单日 / 7d）与奖励 KS 共享同一 entries。
	entries := readLedger(stateDir)

	// L1: 近 1 小时窗口的学习成本。
	t1h := now.Add(-time.Hour)
	toks := foldEntries(entries, t1h, now, FoldLearnLLMTokens(), FoldTotalLLMTokens())
	snap.LearnLLMTokens1h = toks["learn_llm_tokens"]
	snap.TotalLLMTokens1h = toks["total_llm_tokens"]
	snap.LearningCostRatio = FoldLearningCostRatio(snap.LearnLLMTokens1h, snap.TotalLLMTokens1h)
	snap.LearningCostOver = snap.LearningCostRatio > LearningCostRatioWarn

	// L1: 单日奖励分布漂移。
	dayStart := now.Truncate(24 * time.Hour)
	snap.RewardDistKS = ksRewardWindow(entries, dayStart, now)
	snap.RewardDistKSHot = !math.IsNaN(snap.RewardDistKS) && snap.RewardDistKS > RewardDistKSWarn

	// L2: 灰度胜率（累计）。
	snap.CanaryWins, snap.CanaryTotal = FoldCanaryWinRate(stateDir)

	// L2: 晋升 30 天生存。
	snap.Survived, snap.Promoted = FoldPromoteSurvival(stateDir, now)
	if snap.Promoted > 0 {
		snap.PromoteSurvival = float64(snap.Survived) / float64(snap.Promoted)
	}
	snap.PromoteSurvivalLow = snap.Promoted > 0 && snap.PromoteSurvival < PromoteSurvivalFloor

	// L2: 回滚次数（近 7 天）。
	rollbacks := foldEntries(entries, now.Add(-7*24*time.Hour), now, FoldRollbackCount())
	snap.RollbackCount7d = rollbacks["rollback_count"]

	// L2: 注入配对 uplift (13.8.4) —— 折叠器在 ledger_uplift.go。
	snap.InjectionUplift = FoldInjectionUplift(stateDir)

	return snap
}

// fingerprintLedger 账本指纹：metrics/*.jsonl + evolution/rewards.jsonl 每文件的
// (相对路径, size, mtime-nano) 拼接。任一文件追加/换出 → 指纹变。读失败（目录
// 尚未建立）返回空指纹——空账本折叠本来就便宜，缓存是否命中无所谓。
func fingerprintLedger(stateDir string) snapshotCacheKey {
	var sb strings.Builder
	for _, pattern := range []string{"metrics/*.jsonl", "evolution/rewards.jsonl"} {
		paths, _ := filepath.Glob(filepath.Join(stateDir, pattern))
		sort.Strings(paths)
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			sb.WriteString(p)
			sb.WriteByte('\x00')
			sb.WriteString(strconv.FormatInt(info.Size(), 10))
			sb.WriteByte('\x00')
			sb.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 10))
			sb.WriteByte('\x00')
		}
	}
	return snapshotCacheKey{files: sb.String()}
}

// Snapshot 驾驶舱四象限快照（L1 健康度 + L2 效果，L3 资产由既有库存指标覆盖）。
type Snapshot struct {
	Now time.Time

	// L1 健康度
	LearnLLMTokens1h  float64
	TotalLLMTokens1h  float64
	LearningCostRatio float64
	LearningCostOver  bool    // > 0.10
	RewardDistKS      float64 // NaN = 样本不足
	RewardDistKSHot   bool    // > 0.3

	// L2 效果
	CanaryWins         int
	CanaryTotal        int
	CanaryWinRate      float64 // Can
	Promoted           int
	Survived           int
	PromoteSurvival    float64
	PromoteSurvivalLow bool
	RollbackCount7d    float64

	// InjectionUplift (13.8.4) 配对注入 uplift；NaN = 无可判定桶（键缺失语义）。
	InjectionUplift InjectionUpliftPaired
}

// ksRewardWindow 奖励分布两窗口 KS 统计量（窗口内对半切）。
// 任一半窗事件数 < 2 → NaN（样本不足不判漂移）。
func ksRewardWindow(entries []Entry, from, to time.Time) float64 {
	lo, hi := from.UnixNano(), to.UnixNano()
	var a, b []float64
	mid := (lo + hi) / 2
	for _, e := range entries {
		if e.TS < lo || e.TS >= hi || (e.Source == "" && e.Module != "") {
			continue
		}
		if e.Module != "" { // metrics 行不是奖励事件
			continue
		}
		if e.TS < mid {
			a = append(a, e.Value)
		} else {
			b = append(b, e.Value)
		}
	}
	return ksStatistic(a, b)
}

// ksStatistic 双样本 KS D 统计量（经验 CDF 最大差）。
func ksStatistic(a, b []float64) float64 {
	if len(a) < 2 || len(b) < 2 {
		return math.NaN()
	}
	sort.Float64s(a)
	sort.Float64s(b)
	var i, j int
	var d, cdfA, cdfB float64
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			cdfA = float64(i+1) / float64(len(a))
			i++
		case a[i] > b[j]:
			cdfB = float64(j+1) / float64(len(b))
			j++
		default:
			av, bv := a[i], b[j]
			for i < len(a) && a[i] == av {
				i++
			}
			for j < len(b) && b[j] == bv {
				j++
			}
			cdfA = float64(i) / float64(len(a))
			cdfB = float64(j) / float64(len(b))
		}
		if diff := math.Abs(cdfA - cdfB); diff > d {
			d = diff
		}
	}
	// 尾部余量（一边先耗尽时另一边的 CDF 继续爬升）
	cdfA = float64(i) / float64(len(a))
	cdfB = float64(j) / float64(len(b))
	if diff := math.Abs(cdfA - cdfB); diff > d {
		d = diff
	}
	return d
}

// --- 别名：供既有调用方沿用旧名 ---
var (
	// FoldSpecs 依需实例化常用 spec 的便捷集（Fold(..., specs...) 由调用方按需取）。
	LearnLLMTokensSpec = FoldLearnLLMTokens()
	TotalLLMTokensSpec = FoldTotalLLMTokens()
	RollbackCountSpec  = FoldRollbackCount()
)
