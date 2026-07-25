// smoke.go —— 晋升前多档冒烟 (design/03 §4.5 H6, test-before-train → test-before-promote)。
//
// # 它与 replay.Run 的分工
//
// replay.Run 回答"给定同样的输入, 这个产物是否让产出更好" —— 单档、比分数。
// Smoke 回答一个**完全不同**的问题: "这个产物在弱模型上会不会崩"。
//
// hermes 的做法是训练前用小/中/大三档模型各跑 3 step × 16 completion, 专门测环境
// 加载、prompt 构造、解析鲁棒性、verifier 正确性 —— 因为"技能/prompt 对弱模型不鲁棒"
// 是线上劣化的常见来源: 强模型能从一段含糊的 prompt 里猜出意图, 弱模型会直接跑偏或
// 产出无法解析的东西。只在主模型上验过就晋升, 等于把风险留给了 fallback 生效的那一刻。
//
// # 三条判据 (全部要过, fail-closed)
//
//  1. **解析鲁棒性**: 每档的失败率 (报错 / 空产出 / 确定性断言不过) 不超过 MaxFailRate。
//  2. **奖励覆盖**: 每档能打出分的样本占比不低于 MinCoverage —— 覆盖率塌了说明评估器
//     在这个档位上根本没工作, 那时"平均分还不错"是假的 (只有少数样本参与平均)。
//  3. **多档可用**: 至少 MinTiers 个档位真的跑了。
//
// 第 3 条是本文件里唯一容易被"优化掉"的一条, 所以写死为**拒绝**而不是警告: 只配了
// 一个档位的冒烟不是 H6, 它是单档回放换了个名字。档位不可用 (未配置 candidate) 记
// Available=false 并计入拒绝理由, 绝不当作"这档通过了"。
package replay

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Tier 一个模型档位。
//
// Candidate 由宿主构造 —— 只有宿主知道各档位对应哪个 provider/model, 以及怎么把
// 被测产物 (技能正文 / prompt 版本 / 图模板) 注进去。本包不猜。
type Tier struct {
	Name  string // 档位名, 如 "primary" / "fallback" / "local"
	Model string // 模型标识 (仅用于报告)
	// Candidate 该档位下的被测产物; nil = 该档位未配置。
	Candidate Candidate
}

// SmokeConfig 冒烟参数。
type SmokeConfig struct {
	// Samples 每个任务在每个档位上采样几次; <=0 取 2。
	// 采样 >1 才能看出"偶尔崩"——只跑一次的鲁棒性结论没有意义。
	Samples int
	// MinTiers 至少要有几个档位真的跑了; <=0 取 2 (H6 的"多档"最小值)。
	MinTiers int
	// MaxFailRate 单档可容忍的失败率上限; <=0 取 0.2。
	MaxFailRate float64
	// MinCoverage 单档的评分覆盖率下限; <=0 取 0.8。
	MinCoverage float64
	// Concurrency / TaskTimeout / PassScore / Gate 直接透给 Harness。
	Concurrency int
	TaskTimeout time.Duration
	PassScore   float64
	Gate        GateRunner
	// OutDir 非空时每档的原始结果流式落盘到 <OutDir>/smoke-<tier>.jsonl。
	OutDir string
}

func (c SmokeConfig) withDefaults() SmokeConfig {
	if c.Samples <= 0 {
		c.Samples = 2
	}
	if c.MinTiers <= 0 {
		c.MinTiers = 2
	}
	if c.MaxFailRate <= 0 {
		c.MaxFailRate = 0.2
	}
	if c.MinCoverage <= 0 {
		c.MinCoverage = 0.8
	}
	return c
}

// TierReport 单档冒烟结果。
type TierReport struct {
	Tier      string   `json:"tier"`
	Model     string   `json:"model,omitempty"`
	Available bool     `json:"available"`
	Samples   int      `json:"samples"`
	Failures  int      `json:"failures"` // 报错 + 空产出 + 确定性断言不过
	ZeroTurns int      `json:"zero_turns"`
	Scored    int      `json:"scored"` // 拿到分数的样本数
	FailRate  float64  `json:"fail_rate"`
	Coverage  float64  `json:"coverage"`
	MeanScore float64  `json:"mean_score"`
	Passed    bool     `json:"passed"`
	Notes     []string `json:"notes,omitempty"`
}

// SmokeReport 一次多档冒烟的汇总。
type SmokeReport struct {
	Product   string       `json:"product"`
	Tiers     []TierReport `json:"tiers"`
	TiersRun  int          `json:"tiers_run"`
	Passed    bool         `json:"passed"`
	Blocking  []string     `json:"blocking,omitempty"` // 拒绝理由 (Passed=false 时非空)
	StartedAt string       `json:"started_at"`
}

// Smoke 对同一份任务集在多个模型档位上跑冒烟。
//
// judge 可为 nil —— 此时只做确定性断言 (Task.Expect/Task.Gate)。**这不是降级**:
// H6 关心的"解析不崩 / 覆盖正常"本身就大半是确定性可判的, 没有 judge 时依然是一次
// 有效的鲁棒性冒烟, 只是没有质量分。
func Smoke(ctx context.Context, product string, tasks []Task, tiers []Tier, judge Judge, cfg SmokeConfig) (*SmokeReport, error) {
	cfg = cfg.withDefaults()
	rep := &SmokeReport{Product: product, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if len(tasks) == 0 {
		rep.Blocking = append(rep.Blocking, "任务集为空: 无从冒烟 (先沉淀 EvalEnv 任务集, 见 §4.5)")
		return rep, nil
	}
	if len(tiers) == 0 {
		rep.Blocking = append(rep.Blocking, "未提供任何模型档位")
		return rep, nil
	}

	// 采样 = 把同一任务复制 Samples 份。指纹里掺入采样序号, 否则续跑判重会把
	// 同一任务的多次采样当成一条 (Task.Fingerprint 是内容哈希)。
	sampled := make([]Task, 0, len(tasks)*cfg.Samples)
	for _, t := range tasks {
		for s := 0; s < cfg.Samples; s++ {
			st := t
			st.ID = fmt.Sprintf("%s#s%d", t.ID, s)
			if s > 0 {
				// 只改 Input 的一个不可见后缀而不改 Objective: Objective 是真正喂给
				// 产物的东西, 掺东西会改变被测行为。Input 只参与指纹与上下文拼接。
				st.Input = t.Input + strings.Repeat("​", s)
			}
			sampled = append(sampled, st)
		}
	}

	for _, tier := range tiers {
		tr := TierReport{Tier: tier.Name, Model: tier.Model}
		if tier.Candidate == nil {
			tr.Notes = append(tr.Notes, "该档位未配置 candidate, 未运行 (不计为通过)")
			rep.Tiers = append(rep.Tiers, tr)
			continue
		}
		tr.Available = true

		hcfg := Config{
			Concurrency: cfg.Concurrency,
			TaskTimeout: cfg.TaskTimeout,
			PassScore:   cfg.PassScore,
			Gate:        cfg.Gate,
		}
		if cfg.OutDir != "" {
			hcfg.OutPath = fmt.Sprintf("%s/smoke-%s.jsonl", strings.TrimRight(cfg.OutDir, "/"), sanitizeTier(tier.Name))
		}
		h, err := New(hcfg, judge)
		if err != nil {
			return nil, fmt.Errorf("smoke: 档位 %s 构造 harness 失败: %w", tier.Name, err)
		}
		r, err := h.Run(ctx, tier.Candidate, sampled)
		_ = h.Close()
		if err != nil {
			return nil, fmt.Errorf("smoke: 档位 %s 运行失败: %w", tier.Name, err)
		}

		tr.Samples = r.Ran
		var sum float64
		for _, res := range r.Results {
			switch {
			case res.Err != "":
				tr.Failures++
			case res.ZeroTurn:
				tr.Failures++
				tr.ZeroTurns++
			case res.Score <= 0:
				// 确定性断言未命中 (硬否决 → 0 分) 也算鲁棒性失败: 弱模型最典型的
				// 症状就是"有输出但格式/内容不对"。
				tr.Failures++
			default:
				tr.Scored++
				sum += res.Score
			}
		}
		if tr.Samples > 0 {
			tr.FailRate = float64(tr.Failures) / float64(tr.Samples)
			tr.Coverage = float64(tr.Scored) / float64(tr.Samples)
		}
		if tr.Scored > 0 {
			tr.MeanScore = sum / float64(tr.Scored)
		}
		tr.Passed = tr.Samples > 0 && tr.FailRate <= cfg.MaxFailRate && tr.Coverage >= cfg.MinCoverage
		if tr.Samples == 0 {
			tr.Notes = append(tr.Notes, "该档位 0 个样本实际执行 (全部被续跑跳过?)")
		}
		if tr.FailRate > cfg.MaxFailRate {
			tr.Notes = append(tr.Notes, fmt.Sprintf("失败率 %.0f%% 超过上限 %.0f%% (解析鲁棒性不足)",
				tr.FailRate*100, cfg.MaxFailRate*100))
		}
		if tr.Coverage < cfg.MinCoverage {
			tr.Notes = append(tr.Notes, fmt.Sprintf("评分覆盖率 %.0f%% 低于下限 %.0f%% (评估器在该档位未正常工作, 均分不可信)",
				tr.Coverage*100, cfg.MinCoverage*100))
		}
		rep.Tiers = append(rep.Tiers, tr)
	}

	sort.SliceStable(rep.Tiers, func(i, j int) bool { return rep.Tiers[i].Tier < rep.Tiers[j].Tier })
	for _, tr := range rep.Tiers {
		if tr.Available {
			rep.TiersRun++
		}
	}
	// fail-closed 汇总: 档位数不足 → 拒绝; 任一已跑档位不过 → 拒绝。
	if rep.TiersRun < cfg.MinTiers {
		rep.Blocking = append(rep.Blocking, fmt.Sprintf(
			"只有 %d 个档位实际运行, 少于要求的 %d —— 单档冒烟不是 H6, 不予放行", rep.TiersRun, cfg.MinTiers))
	}
	for _, tr := range rep.Tiers {
		if tr.Available && !tr.Passed {
			rep.Blocking = append(rep.Blocking, fmt.Sprintf("档位 %s 未通过: %s",
				tr.Tier, strings.Join(tr.Notes, "; ")))
		}
	}
	rep.Passed = len(rep.Blocking) == 0
	return rep, nil
}

// Format 人读格式。
func (r *SmokeReport) Format() string {
	var b strings.Builder
	verdict := "拒绝"
	if r.Passed {
		verdict = "通过"
	}
	fmt.Fprintf(&b, "多档冒烟 [%s] 产物=%s 档位运行 %d\n", verdict, r.Product, r.TiersRun)
	for _, t := range r.Tiers {
		if !t.Available {
			fmt.Fprintf(&b, "  %-10s 未配置 (不计通过)\n", t.Tier)
			continue
		}
		fmt.Fprintf(&b, "  %-10s 样本 %d 失败 %d (%.0f%%) 覆盖 %.0f%% 均分 %.3f %s\n",
			t.Tier, t.Samples, t.Failures, t.FailRate*100, t.Coverage*100, t.MeanScore, passLabel(t.Passed))
		for _, n := range t.Notes {
			fmt.Fprintf(&b, "             · %s\n", n)
		}
	}
	for _, blk := range r.Blocking {
		fmt.Fprintf(&b, "  ✗ %s\n", blk)
	}
	return b.String()
}

func passLabel(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// sanitizeTier 档位名进文件名前消毒 (档位名可能来自配置)。
func sanitizeTier(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tier"
	}
	return b.String()
}
