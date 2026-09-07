// paired.go —— 13.8.6 P1: skillaudit 配对归因 (SkillAudit 论文, arXiv:2606.14239)。
//
// 规划锚点 (docforge planning-evo-track §13.8.6 P1): 「带/不带该技能的配对轨迹对照」;
// 「13.7.4 的选择留痕恰好提供 per-skill 证据; 晋升门禁从加权均值升级为配对差值」。
//
// v1 (skillaudit.go) 的判据是时序加权均值: 技能创建后的全部奖励均值。它的盲区是
// **归因**: 奖励好可能只是同期整体就好, 与该技能无关。13.7.4 的 policy_decision
// Span (attrs.skills, 反解自 <role_skills> 段) 记录了每个 run 实际注入了哪些技能,
// 于是可以做真正的配对对照:
//
//	with    = 注入了技能 S 的 run 的奖励均值
//	without = 同池未注入 S 的 run 的奖励均值
//	delta   = with − without        (配对差值, 替代时序均值做判据)
//
// 判据升级而非替换: 有配对证据 (双侧样本都达 minSamples) 用 delta 裁决; 没有则
// 回退 v1 时序均值 —— 老数据、没开 tracestore 的部署, 行为与之前完全一致。
//
// 简化取舍 (注释即承诺): 本版做**全池配对** (不按 stage/role 分桶)。run 级桶会因
// 一个 run 跨多 stage 而过碎, 桶内样本进一步跌破 minSamples; 桶内对照是后续深化。
package skillaudit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// runEvidence 一个 run 的 policy_decision 留痕 (13.7.4 选择留痕)。
type runEvidence struct {
	Stage  string
	Role   string
	TS     int64 // 首条 policy span 的时间 (unix-milli), 用于技能创建时间过滤
	Skills map[string]bool
}

// runScore 一个 run 的奖励聚合 (行内按源加权, 与 rewardRow.weight 同口径)。
type runScore struct {
	weighted  float64
	weightSum float64
	n         int // 奖励行数 (run 内)
}

// mean run 的加权奖励均值。
func (s runScore) mean() float64 {
	if s.weightSum <= 0 {
		return 0
	}
	return s.weighted / s.weightSum
}

// readPolicySkills 扫描 trace log 目录 (<state>/statestore/log/trace-*.jsonl),
// 提取全部 policy_decision Span, 构建 runID → runEvidence。
//
// 直接读文件而非 import tracestore: 本包"只读文件"的纪律 (见 auditFallbackWeights
// 的注释), 不为读一行 JSON 拖入 statestore 运行期依赖。Span 字段 (trace_id/kind/
// attrs.skills) 由 tracestore.Span 的 json tag 锁定, 两侧漂移由单测焊住。
func readPolicySkills(traceLogDir string) map[string]runEvidence {
	out := map[string]runEvidence{}
	files, _ := filepath.Glob(filepath.Join(traceLogDir, "trace-*.jsonl"))
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var row struct {
				TraceID string `json:"trace_id"`
				Kind    string `json:"kind"`
				TS      int64  `json:"ts"`
				Attrs   struct {
					Stage  string `json:"stage"`
					Role   string `json:"role"`
					Skills []any  `json:"skills"`
				} `json:"attrs"`
			}
			if json.Unmarshal([]byte(line), &row) != nil || row.Kind != "policy_decision" || row.TraceID == "" {
				continue
			}
			ev, ok := out[row.TraceID]
			if !ok {
				ev = runEvidence{Skills: map[string]bool{}}
			}
			if ev.TS == 0 || (row.TS > 0 && row.TS < ev.TS) {
				ev.TS = row.TS
			}
			if ev.Stage == "" {
				ev.Stage = row.Attrs.Stage
				ev.Role = row.Attrs.Role
			}
			for _, s := range row.Attrs.Skills {
				if name, ok := s.(string); ok && name != "" {
					ev.Skills[name] = true
				}
			}
			out[row.TraceID] = ev
		}
		f.Close()
	}
	return out
}

// runScores 把奖励行按 run 聚合 (RunID 空 = 无法归属 run 的行, 不参与配对)。
func runScores(rewards []rewardRow) map[string]runScore {
	out := map[string]runScore{}
	for _, r := range rewards {
		if r.RunID == "" {
			continue
		}
		sc := out[r.RunID]
		w := r.weight()
		sc.weighted += w * r.Value
		sc.weightSum += w
		sc.n++
		out[r.RunID] = sc
	}
	return out
}

// pairedDelta 技能 S 的配对差值。
//
// 返回 ok=false 表示配对证据不足 (任一侧 run 数 < minSamples), 调用方回退时序判据。
// 时间过滤与时序判据同规: 只认技能创建后的 run (ev.TS >= created)。
func pairedDelta(traces map[string]runEvidence, scores map[string]runScore, skill string, createdUnixMilli int64) (delta float64, withN, withoutN int, ok bool) {
	var withSum, withoutSum float64
	for runID, ev := range traces {
		sc, has := scores[runID]
		if !has || sc.n == 0 {
			continue // 该 run 无奖励证据, 不参与对照
		}
		if createdUnixMilli > 0 && ev.TS > 0 && ev.TS < createdUnixMilli {
			continue // 技能创建前的 run: 与时序判据同规过滤
		}
		if ev.Skills[skill] {
			withSum += sc.mean()
			withN++
		} else {
			withoutSum += sc.mean()
			withoutN++
		}
	}
	if withN < minSamples || withoutN < minSamples {
		return 0, withN, withoutN, false
	}
	return withSum/float64(withN) - withoutSum/float64(withoutN), withN, withoutN, true
}
