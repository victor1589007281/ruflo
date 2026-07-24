// Package console 进化操作台 (design/03 §4.7): 检视学习闭环健康度。
//
// v1 提供只读检视 (evo_list_envs / evo_status 的 CLI 形态): 汇总 TraceStore 轨迹、
// rewards.jsonl 奖励、experiences.json 经验, 报告"学习是否真的在发生"。
// 这是 §4.7 操作台的地基 —— 先能观测, 再谈 propose/smoke/promote 闭环。
//
// 护栏原则 (Hermes H7 锁定字段): 本包只读, 不改任何进化产物; 未来写操作经独立
// 受控接口, 治理参数机械锁定。
package console

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Report 学习闭环健康度快照。
type Report struct {
	StateDir       string         `json:"state_dir"`
	TraceRuns      int            `json:"trace_runs"`  // 有轨迹的 run 数
	TraceSpans     int            `json:"trace_spans"` // 总 span 数
	TraceBlobs     int            `json:"trace_blobs"` // 内容寻址 blob 数
	Rewards        int            `json:"rewards"`     // 奖励事件数
	RewardBySource map[string]int `json:"reward_by_source"`
	RewardMean     float64        `json:"reward_mean"`
	Experiences    int            `json:"experiences"`   // 经验条数
	Trajectories   int            `json:"trajectories"`  // 团队轨迹条数
	ShadowSkills   int            `json:"shadow_skills"` // status: shadow 的技能数
	LoopHealth     string         `json:"loop_health"`   // healthy | partial | open
	Notes          []string       `json:"notes"`
}

// Inspect 扫描 stateDir 下的进化制品, 生成健康度报告。
func Inspect(stateDir string) (*Report, error) {
	r := &Report{StateDir: stateDir, RewardBySource: map[string]int{}}

	// 1. TraceStore: statestore/log/trace-*.jsonl + statestore/blob/
	traceDir := filepath.Join(stateDir, "statestore", "log")
	if entries, err := os.ReadDir(traceDir); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "trace-") || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			r.TraceRuns++
			r.TraceSpans += countLines(filepath.Join(traceDir, e.Name()))
		}
	}
	blobDir := filepath.Join(stateDir, "statestore", "blob")
	_ = filepath.Walk(blobDir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			r.TraceBlobs++
		}
		return nil
	})

	// 2. rewards.jsonl (evolution/ 下)
	var rewardSum float64
	rewardsPath := filepath.Join(stateDir, "evolution", "rewards.jsonl")
	forEachJSONL(rewardsPath, func(line []byte) {
		var ev struct {
			Source string  `json:"source"`
			Value  float64 `json:"value"`
		}
		if json.Unmarshal(line, &ev) == nil {
			r.Rewards++
			r.RewardBySource[ev.Source]++
			rewardSum += ev.Value
		}
	})
	if r.Rewards > 0 {
		r.RewardMean = rewardSum / float64(r.Rewards)
	}

	// 3. experiences.json / trajectories.json (数组长度)
	r.Experiences = countJSONArray(filepath.Join(stateDir, "evolution", "experiences.json"))
	r.Trajectories = countJSONArray(filepath.Join(stateDir, "evolution", "trajectories.json"))

	// 4. shadow 技能
	r.ShadowSkills = countShadowSkills(filepath.Join(stateDir, "skills"))

	// 5. 健康度判定 (design/03 §1.2 三开环的反向验证)
	r.assess()
	return r, nil
}

func (r *Report) assess() {
	// 学习闭环健康 = 有轨迹 + 有奖励 + 有经验沉淀
	hasTrace := r.TraceSpans > 0 || r.Trajectories > 0
	hasReward := r.Rewards > 0
	hasExp := r.Experiences > 0
	switch {
	case hasTrace && hasReward && hasExp:
		r.LoopHealth = "healthy"
		r.Notes = append(r.Notes, "轨迹→奖励→经验三环齐备, 学习闭环运转中")
	case hasTrace || hasReward:
		r.LoopHealth = "partial"
		if !hasExp {
			r.Notes = append(r.Notes, "有轨迹/奖励但无经验沉淀: 蒸馏未触发或团队未完成")
		}
	default:
		r.LoopHealth = "open"
		r.Notes = append(r.Notes, "无轨迹无奖励: 该 stateDir 尚未跑过团队, 或学习开环 (见 design/03 §1.2)")
	}
	if r.ShadowSkills > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d 个 shadow 技能待进化门禁晋升 (design/03 §4.3c)", r.ShadowSkills))
	}
	if r.TraceBlobs > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d 个内容寻址 blob (prompt/产出去重存储)", r.TraceBlobs))
	}
}

// Format 人读格式。
func (r *Report) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "进化操作台 · 学习闭环健康度 [%s]\n", strings.ToUpper(r.LoopHealth))
	fmt.Fprintf(&b, "  状态目录: %s\n", r.StateDir)
	fmt.Fprintf(&b, "  轨迹:     %d runs / %d spans / %d blobs\n", r.TraceRuns, r.TraceSpans, r.TraceBlobs)
	fmt.Fprintf(&b, "  奖励:     %d 事件, 均值 %.3f\n", r.Rewards, r.RewardMean)
	if len(r.RewardBySource) > 0 {
		srcs := make([]string, 0, len(r.RewardBySource))
		for s := range r.RewardBySource {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		for _, s := range srcs {
			fmt.Fprintf(&b, "            %-16s %d\n", s, r.RewardBySource[s])
		}
	}
	fmt.Fprintf(&b, "  经验:     %d 条\n", r.Experiences)
	fmt.Fprintf(&b, "  团队轨迹: %d 条\n", r.Trajectories)
	fmt.Fprintf(&b, "  shadow 技能: %d\n", r.ShadowSkills)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  · %s\n", n)
	}
	return b.String()
}

// --- helpers ---

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}

func forEachJSONL(path string, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) > 0 {
			fn(line)
		}
	}
}

func countJSONArray(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var arr []json.RawMessage
	if json.Unmarshal(data, &arr) == nil {
		return len(arr)
	}
	// 也许是 {experiences:[...]} 包裹
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) == nil {
		for _, v := range obj {
			var a []json.RawMessage
			if json.Unmarshal(v, &a) == nil && len(a) > 0 {
				return len(a)
			}
		}
	}
	return 0
}

func countShadowSkills(skillsDir string) int {
	n := 0
	_ = filepath.Walk(skillsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(path, "SKILL.md") {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			head := string(data)
			if len(head) > 500 {
				head = head[:500]
			}
			if strings.Contains(head, "status: shadow") {
				n++
			}
		}
		return nil
	})
	return n
}
