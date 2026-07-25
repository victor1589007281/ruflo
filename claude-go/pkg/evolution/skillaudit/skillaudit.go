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
	Verdict   string  // promote | retire | hold | (空=非 shadow 不裁决)
}

// AuditResult 一次审计的汇总。
type AuditResult struct {
	Evaluated int
	Promoted  []string
	Retired   []string
	Held      []string
}

// rewardRow rewards.jsonl 的最小字段。
type rewardRow struct {
	TS    int64   `json:"ts"`
	Value float64 `json:"value"`
	Team  string  `json:"team,omitempty"`
}

// Audit 扫描 skillsDir 下的 shadow 技能, 依据 <state>/evolution/rewards.jsonl 的
// 奖励证据裁决晋升/退役。apply=true 时真正改写 SKILL.md 的 status; false 为 dry-run。
func Audit(skillsDir, rewardsPath string, apply bool) (*AuditResult, error) {
	rewards := loadRewards(rewardsPath)
	// 全局奖励均值作基线; 无奖励时无法裁决
	if len(rewards) == 0 {
		return &AuditResult{}, nil
	}

	res := &AuditResult{}
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
		var sum float64
		var n int
		for _, r := range rewards {
			if created.IsZero() || r.TS >= created.UnixMilli() {
				sum += r.Value
				n++
			}
		}
		st.Samples = n
		if n > 0 {
			st.RewardAvg = sum / float64(n)
		}

		switch {
		case n < minSamples:
			st.Verdict = "hold"
			res.Held = append(res.Held, st.Name)
		case st.RewardAvg >= promoteThreshold:
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

// Promote/Retire 手动改状态 (evo 操作台的 promote/rollback, 带谱系记录)。
// 只在 status 为 shadow/active/archived 间迁移; 锁定判据不受此影响 (人工覆盖需留痕)。
func SetStatus(skillPath, newStatus string) error {
	if newStatus != "shadow" && newStatus != "active" && newStatus != "archived" {
		return fmt.Errorf("skillaudit: 非法状态 %q (仅 shadow/active/archived)", newStatus)
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
