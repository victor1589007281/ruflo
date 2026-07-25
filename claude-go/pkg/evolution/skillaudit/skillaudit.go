// Package skillaudit 技能进化门禁 (design/03 §4.3c / §4.7)。
//
// 自动提炼的技能默认处 shadow 态 (P0.5 已接线)。本包实现"影子→晋升/退役"的
// 审计门禁: 依据奖励证据裁决 shadow 技能是否晋升 active 或退役 archived。
//
// v1 采用**时序 uplift 审计** (SkillAudit 论文的配对审计的简化落地): 比较技能
// 创建后使用该技能的 run 的奖励均值 vs 全局基线。真正的配对 A/B (带 vs 不带该技能
// 随机分组) 需运行期 skill-usage 打点, 属 E4 深化; 本版先用可得的奖励证据裁决,
// 并把判据与阈值锁定 (Hermes H7 治理参数不可被 agent 改)。
//
// 治理护栏 (design/03 §4.6 四律): 只改 SKILL.md 的 status 字段与 audit 元数据,
// 保留谱系可回滚; 晋升阈值/样本下限锁定为常量。
package skillaudit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/govern"
)

// 锁定的治理参数 (design/03 §4.6 四律: 判据不可被 agent 修改)。
const (
	minSamples       = 3    // 晋升所需最小奖励样本数
	promoteThreshold = 0.3  // 晋升阈值: 相关奖励均值 ≥ 此值
	retireThreshold  = -0.2 // 退役阈值: 相关奖励均值 ≤ 此值
)

// SkillState 一个技能的审计视图。
type SkillState struct {
	Name      string
	Path      string // SKILL.md 路径
	Status    string // shadow | active | archived
	CreatedAt string
	Samples   int     // 参与裁决的奖励样本数
	RewardAvg float64 // 相关奖励均值
	Verdict   string  // promote | retire | hold | reject_escalation | (空=非 shadow 不裁决)
	// RejectReasons 不越权闸的拒绝理由 (Verdict=reject_escalation 时非空)。
	RejectReasons []string
}

// AuditResult 一次审计的汇总。
type AuditResult struct {
	Evaluated int
	Promoted  []string
	Retired   []string
	Held      []string
	// Rejected 奖励达标但被不越权闸拦下的技能 (design/03 §4.6)。
	// 单列一档而不并进 Held: "分数不够先等等"与"越权被拒"要人做的事完全不同。
	Rejected []string
	// RejectReasons 技能名 → 拒绝理由, 供操作台展示。
	RejectReasons map[string][]string
}

// rewardRow rewards.jsonl 的最小字段。
//
// Source/Weight 是本轮补的 (design/03 §4.6「奖励源加权可信度」在这道闸上的落地)。
// 原先取的是**未加权**均值, 于是任何新接进来的弱信号源都会按 1:1 参与晋升判据 ——
// verdict.heuristic 这类"只说明这轮没崩"的过程信号一旦接上, 数量上会压过确定性门禁,
// 把一道 fail-closed 的闸稀释成"跑得多就能晋升"。加权后弱源仍在, 但压不动闸。
type rewardRow struct {
	TS     int64   `json:"ts"`
	Value  float64 `json:"value"`
	Team   string  `json:"team,omitempty"`
	Source string  `json:"source,omitempty"`
	Weight float64 `json:"weight,omitempty"`
}

// weight 取这条奖励的可信度。
//
// 优先用落盘时记下的 Weight (RecordReward 会填, 见 pkg/agent), 缺失才按源名查表 ——
// 与 learners.weightOf 同一策略: 权重表会调, 已发生的奖励应保留当时的可信度。
// 认不出的源给 unknownWeight 而不是 0: 新源不该被静默忽略, 也不该压倒确定性门禁。
func (r rewardRow) weight() float64 {
	if r.Weight > 0 {
		return r.Weight
	}
	if w, ok := auditFallbackWeights[strings.ToLower(strings.TrimSpace(r.Source))]; ok {
		return w
	}
	return auditUnknownWeight
}

// auditFallbackWeights 老数据 (Weight 未落盘) 的兜底表。
//
// 与 pkg/agent.RewardSourceWeight / learners.fallbackWeights 是同一组定值的第三份副本。
// 副本本身是坏味道, 换来的是本包不必依赖 pkg/agent 的运行期类型 (本包只读文件);
// 一致性由 weights_consistency_test.go 焊住 —— 那是外部测试包, 反向 import 只在测试
// 二进制里发生。
var auditFallbackWeights = map[string]float64{
	"gate.compile": 1.0, "gate.test": 1.0, "gate.lint": 1.0, "gate.e2e": 1.0,
	"user.explicit": 0.8, "user.steer": 0.8, "user.feedback": 0.8,
	"gate.content": 0.5, "review.panel": 0.5, "gate.review": 0.5, "llm.judge": 0.5,
	"episode": 0.3,
	"latency": 0.2, "cost": 0.2,
	"verdict.heuristic": 0.15,
}

const auditUnknownWeight = 0.5

// Audit 扫描 skillsDir 下的 shadow 技能, 依据 <state>/evolution/rewards.jsonl 的
// 奖励证据裁决晋升/退役。apply=true 时真正改写 SKILL.md 的 status; false 为 dry-run。
func Audit(skillsDir, rewardsPath string, apply bool) (*AuditResult, error) {
	rewards := loadRewards(rewardsPath)
	// 全局奖励均值作基线; 无奖励时无法裁决
	if len(rewards) == 0 {
		return &AuditResult{}, nil
	}

	res := &AuditResult{RejectReasons: map[string][]string{}}
	err := filepath.Walk(skillsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(path, "SKILL.md") {
			return nil
		}
		st := parseSkill(path)
		if st == nil || st.Status != "shadow" {
			return nil // 只审计 shadow 技能
		}
		res.Evaluated++

		// v1 简化: 用技能创建时间之后的奖励作为"该技能生效期"证据。
		created := parseTime(st.CreatedAt)
		var weighted, weightSum float64
		var n int
		for _, r := range rewards {
			if created.IsZero() || r.TS >= created.UnixMilli() {
				w := r.weight()
				weighted += w * r.Value
				weightSum += w
				n++
			}
		}
		st.Samples = n
		if weightSum > 0 {
			// 加权均值 (design/03 §4.6 奖励源加权可信度)。样本数仍用条数计 ——
			// minSamples 问的是"有没有攒够观测", 那与可信度是两个维度。
			st.RewardAvg = weighted / weightSum
		}

		switch {
		case n < minSamples:
			st.Verdict = "hold"
			res.Held = append(res.Held, st.Name)
		case st.RewardAvg >= promoteThreshold:
			// 不越权闸 (design/03 §4.6 四律, 见 pkg/evolution/govern): 奖励够高**不等于**
			// 可以晋升。一个 LLM 自己提炼出来的技能若在 frontmatter 里声明了 Bash/Write,
			// 晋升成 active 就等于让被优化的系统自己扩大动作空间。
			//
			// 顺序刻意放在奖励判据**之后**: 拒绝理由要能说清"分数够了但权限面越界",
			// 而不是笼统的"没过闸"。
			if v := govern.CheckSkillPromotion(path, nil); !v.Allowed {
				st.Verdict = "reject_escalation"
				st.RejectReasons = v.Reasons
				res.Rejected = append(res.Rejected, st.Name)
				res.RejectReasons[st.Name] = v.Reasons
				return nil
			}
			st.Verdict = "promote"
			res.Promoted = append(res.Promoted, st.Name)
			if apply {
				_ = rewriteStatus(path, "active", st.RewardAvg, n)
			}
		case st.RewardAvg <= retireThreshold:
			st.Verdict = "retire"
			res.Retired = append(res.Retired, st.Name)
			if apply {
				_ = rewriteStatus(path, "archived", st.RewardAvg, n)
			}
		default:
			st.Verdict = "hold"
			res.Held = append(res.Held, st.Name)
		}
		return nil
	})
	return res, err
}

// SetStatus 手动改状态 (evo 操作台的 promote/rollback, 带谱系记录)。
// 只在 status 为 shadow/active/archived 间迁移; 锁定判据不受此影响 (人工覆盖需留痕)。
//
// **晋升方向 (→active) 同样过不越权闸**: 手动通道若能绕过, 那机械强制就只是给自动
// 通道加的装饰 —— 而 §4.7 的 evo_promote 工具正是走这条手动通道, agent 可达。
// 收紧方向 (→shadow/archived) 不检查: 把权限面变小永远不是提权。
func SetStatus(skillPath, newStatus string) error {
	if newStatus != "shadow" && newStatus != "active" && newStatus != "archived" {
		return fmt.Errorf("skillaudit: 非法状态 %q (仅 shadow/active/archived)", newStatus)
	}
	if newStatus == "active" {
		if v := govern.CheckSkillPromotion(skillPath, nil); !v.Allowed {
			return v.Error()
		}
	}
	return rewriteStatus(skillPath, newStatus, 0, 0)
}

// --- helpers ---

func loadRewards(path string) []rewardRow {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []rewardRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r rewardRow
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func parseSkill(path string) *SkillState {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	st := &SkillState{Path: path, Status: "active"} // 无 status 字段默认 active
	head := string(data)
	if len(head) > 800 {
		head = head[:800]
	}
	for _, ln := range strings.Split(head, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "name:"):
			st.Name = strings.TrimSpace(strings.TrimPrefix(ln, "name:"))
		case strings.HasPrefix(ln, "status:"):
			st.Status = strings.TrimSpace(strings.TrimPrefix(ln, "status:"))
		case strings.HasPrefix(ln, "created_at:"):
			st.CreatedAt = strings.TrimSpace(strings.TrimPrefix(ln, "created_at:"))
		}
	}
	if st.Name == "" {
		st.Name = filepath.Base(filepath.Dir(path))
	}
	return st
}

// rewriteStatus 改写 SKILL.md frontmatter 的 status + audit 元数据 (谱系留痕)。
func rewriteStatus(path, status string, rewardAvg float64, samples int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	auditLine := fmt.Sprintf("audit: %s@%s reward_avg=%.3f samples=%d", status, time.Now().Format(time.RFC3339), rewardAvg, samples)
	var out []string
	statusSet := false
	inFrontmatter := false
	fmCount := 0
	auditWritten := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "---" {
			fmCount++
			if fmCount == 1 {
				inFrontmatter = true
			} else if fmCount == 2 {
				// frontmatter 结束前补 status/audit
				if !statusSet {
					out = append(out, "status: "+status)
				}
				if !auditWritten {
					out = append(out, auditLine)
				}
				inFrontmatter = false
			}
			out = append(out, ln)
			continue
		}
		if inFrontmatter && strings.HasPrefix(trimmed, "status:") {
			out = append(out, "status: "+status)
			statusSet = true
			continue
		}
		if inFrontmatter && strings.HasPrefix(trimmed, "audit:") {
			out = append(out, auditLine)
			auditWritten = true
			continue
		}
		out = append(out, ln)
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
