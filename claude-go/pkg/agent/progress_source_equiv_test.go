package agent

// progress_source_equiv_test.go —— design/01 §4.3「三源归一」的等价性证明。
//
// ---------------------------------------------------------------------------
// 这组测试在钉什么
// ---------------------------------------------------------------------------
//
// 归一的对象是**进度真源**。同一个 pipeline 团队在两条路径上跑:
//
//	旧路径 (CLAUDE_GO_GRAPH_ENGINE 未设): 进度真源 = <dataDir>/checkpoints.json
//	新路径 (CLAUDE_GO_GRAPH_ENGINE=1):    进度真源 = <dataDir>/graph-journal/journal.jsonl
//
// 「归一」的验收标准不是"两个文件都还在", 而是**四个生命周期场景下, 两条路径对
// "哪些阶段重跑、哪些吃缓存" 的判定逐项一致**。判定口径只有一个可观测量:
// 本轮真正被派发给 runner 的阶段集合 (stageProbe 采集), 因为用户能感知的
// "重跑了没有 / 吃到旧产出没有" 就是它。
//
// 四个场景 (与 design/01 §4.3 列的恢复语义一一对应):
//
//	S1 首跑             —— 无任何历史进度, 全部阶段执行
//	S2 崩溃续跑         —— 上一轮跑完前 3 个阶段后进程死掉 (无终态记录), 续跑只补后 3 个
//	S3 refine 整体重跑  —— RefineTeam(feedback, "") 全部重跑
//	S4 refine 按阶段    —— RefineTeam(feedback, "article-writing") 只重跑目标及其后续
//	S5 换目标首跑       —— 团队带着上一轮残留进度, 用**新目标**重新起跑
//
// S5 是这组测试挖出真缺陷的那一条, 见 TestProgressSourceEquivalence 的表注。
//
// ---------------------------------------------------------------------------
// 为什么不用更直觉的"比对两个文件内容"
// ---------------------------------------------------------------------------
//
// 两条路径的存储形态本就不同 (map[stage]*Checkpoint 快照 vs append-only 事件流),
// 逐字段比对必然要写一层翻译, 而翻译层写错时测试会**跟着错**——它证明的是
// "我的翻译自洽", 不是"恢复语义一致"。改比对**行为的可观测投影**: 谁被派发了。
// 这个量在两条路径上是同一个物理事件 (factory 造 runner → Execute), 无需翻译。
//
// ---------------------------------------------------------------------------
// 变异反证 (证明上面这些断言不是许愿式的)
// ---------------------------------------------------------------------------
//
// 逐个改坏后本文件必红, 且**红在语义正确的那一格**:
//
//	M1 clearRunProgress 漏掉 graph-journal (= 修复前的形态)
//	   → S3 与 S5 双红, 新路径 got=[] (零阶段执行, 直接吃旧产出)
//
//	M2 恢复模式下也调 clearRunProgress (过度清空)
//	   → S2 与 S4 双红, 新路径 got 是全部 6 个阶段 (该复用的前序被强行重跑)
//	   —— 这一格证明测试不只会抓"少清", 也会抓"多清"。单向断言很容易被
//	      "干脆全清一遍" 这种糊弄式修复骗过。
//
//	M3 refine 按阶段时不写 node.invalidated 事件
//	   → S4 单红, 新路径 got=[] (用户反馈静默消失, 即本轮之前修过的那个真 bug)
//
// 三个变异覆盖了失效语义的三个方向: 少清 / 多清 / 失效不生效。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 探针: 记录本轮真正被派发执行的阶段
// ---------------------------------------------------------------------------

// stageProbe 采集"本轮哪些角色真的被造出来跑了一次"。
// techblog 的 6 个阶段角色两两不同, 故 role 集合 ≡ 执行阶段集合。
type stageProbe struct {
	mu    sync.Mutex
	roles []string
}

func (p *stageProbe) add(role string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roles = append(p.roles, role)
}

// stages 返回本轮执行过的阶段名 (已按工作流声明序去重排序, 便于逐项比对)。
func (p *stageProbe) stages(wf *WorkflowDef) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range p.roles {
		seen[r] = true
	}
	var out []string
	for _, st := range wf.Stages {
		if seen[st.Role] {
			out = append(out, st.Name)
		}
	}
	return out
}

type srcProbeRunner struct {
	role  string
	probe *stageProbe
}

func (r *srcProbeRunner) Execute(_ context.Context, _ string) (string, error) {
	r.probe.add(r.role)
	// 返回一段够长的普通技术文章, 避开内容门禁与 planner/researcher 的伪工具调用校验。
	return "# 文章\n\n本轮新产出。" + strings.Repeat("论据充分的段落内容。", 60), nil
}

// ---------------------------------------------------------------------------
// 两条路径各自的"上一轮进度"播种
// ---------------------------------------------------------------------------

// seedLegacyProgress 往 checkpoints.json 写入若干已完成阶段 (旧路径的进度真源)。
func seedLegacyProgress(t *testing.T, dataDir string, stages []string) {
	t.Helper()
	cps := map[string]*Checkpoint{}
	for _, name := range stages {
		cps[name] = &Checkpoint{
			StageName: name, Status: "completed",
			Output: "上一轮产出:" + name, SavedAt: time.Unix(1, 0),
		}
	}
	data, err := json.MarshalIndent(cps, "", "  ")
	if err != nil {
		t.Fatalf("seedLegacyProgress marshal: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("seedLegacyProgress mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "checkpoints.json"), data, 0o644); err != nil {
		t.Fatalf("seedLegacyProgress write: %v", err)
	}
}

// seedGraphProgress 往 graph-journal 写入若干 node.completed (新路径的进度真源)。
//
// finished 为空表示**上一轮没有落终态** —— 即进程被杀/崩溃。这是刻意的:
// 崩溃续跑正是 journal 相对快照文件的核心卖点, 若只造"跑完了"的 journal,
// Replay 的 `Finished && completed → 清空缓存` 那一支会把所有场景都抹平, 测不出差异。
func seedGraphProgress(t *testing.T, dataDir, runID string, stages []string, finished string) {
	t.Helper()
	dir := filepath.Join(dataDir, "graph-journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seedGraphProgress mkdir: %v", err)
	}
	var b strings.Builder
	enc := func(v map[string]any) {
		line, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("seedGraphProgress marshal: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	seq := 0
	next := func() int { seq++; return seq }
	enc(map[string]any{"seq": next(), "ts": 1, "type": "run.created", "run_id": runID,
		"data": map[string]any{"graph": "techblog", "resume": true}})
	for _, name := range stages {
		enc(map[string]any{"seq": next(), "ts": 1, "type": "node.completed", "run_id": runID,
			"node_id": name, "data": map[string]any{"output": "上一轮产出:" + name}})
	}
	if finished != "" {
		enc(map[string]any{"seq": next(), "ts": 1, "type": "run.finished", "run_id": runID,
			"data": map[string]any{"status": finished}})
	}
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("seedGraphProgress write: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 场景表
// ---------------------------------------------------------------------------

// progressScenario 一个恢复语义场景。seeded 是"上一轮已完成的阶段"(两条路径同样播种),
// act 是触发的生命周期动作, want 是**两条路径都必须重跑**的阶段集合。
type progressScenario struct {
	name   string
	seeded []string
	// prevStatus 播种时团队的状态 (影响 RunTeam 的 isResume 判定)。
	prevStatus TeamStatus
	// seedFinished 上一轮 journal 里 run.finished 的 status。
	// 空 = 上一轮**没有落终态** (进程被杀/崩溃)。两种都要测: Replay 对
	// "已完整跑完的 run" 与 "半途而废的 run" 走的是不同分支, 只测一种会漏掉另一种。
	seedFinished string
	act          func(t *testing.T, ptm *ProductionTeamManager, team *ProductionTeam)
	want         []string
	note         string
}

const (
	tbObjective    = "写一篇关于向量数据库的技术博客"
	tbNewObjective = "写一篇关于列式存储引擎的技术博客"
)

var techblogStages = []string{
	"source-analysis", "investigation", "fact-checking",
	"article-writing", "self-critique", "formatting",
}

func progressScenarios() []progressScenario {
	first3 := techblogStages[:3]
	last3 := techblogStages[3:]
	return []progressScenario{
		{
			name:       "S1_首跑",
			seeded:     nil,
			prevStatus: TeamStatusCreated,
			act: func(t *testing.T, ptm *ProductionTeamManager, team *ProductionTeam) {
				if err := ptm.RunTeam(team.Name, tbObjective); err != nil {
					t.Fatalf("RunTeam: %v", err)
				}
			},
			want: techblogStages,
			note: "无历史进度 → 全部阶段执行",
		},
		{
			name:       "S2_崩溃续跑",
			seeded:     first3,
			prevStatus: TeamStatusFailed, // 同目标 + failed ⇒ RunTeam 判定 isResume
			act: func(t *testing.T, ptm *ProductionTeamManager, team *ProductionTeam) {
				if err := ptm.RunTeam(team.Name, tbObjective); err != nil {
					t.Fatalf("RunTeam: %v", err)
				}
			},
			want: last3,
			note: "前 3 阶段吃缓存, 只补后 3 个",
		},
		{
			name:       "S3_refine整体重跑",
			seeded:     techblogStages,
			prevStatus: TeamStatusCompleted,
			act: func(t *testing.T, ptm *ProductionTeamManager, team *ProductionTeam) {
				if err := ptm.RefineTeam(team.Name, "请补充基准测试数据", ""); err != nil {
					t.Fatalf("RefineTeam: %v", err)
				}
			},
			want: techblogStages,
			note: "反馈必须作用到每一个阶段 → 全部重跑",
		},
		{
			name:       "S4_refine按阶段",
			seeded:     techblogStages,
			prevStatus: TeamStatusCompleted,
			act: func(t *testing.T, ptm *ProductionTeamManager, team *ProductionTeam) {
				if err := ptm.RefineTeam(team.Name, "请补充基准测试数据", "article-writing"); err != nil {
					t.Fatalf("RefineTeam: %v", err)
				}
			},
			want: last3,
			note: "目标阶段及其后续重跑, 前序复用",
		},
		{
			name:       "S5_换目标首跑",
			seeded:     techblogStages,
			prevStatus: TeamStatusFailed, // 目标不同 ⇒ 即便 failed 也不是 resume
			act: func(t *testing.T, ptm *ProductionTeamManager, team *ProductionTeam) {
				if err := ptm.RunTeam(team.Name, tbNewObjective); err != nil {
					t.Fatalf("RunTeam: %v", err)
				}
			},
			want: techblogStages,
			note: "换了目标就是新的一次运行, 旧目标的阶段产出一条都不能复用",
		},
		{
			// S5 的姊妹场景: 上一轮**正常收尾**。Replay 有一条
			// `Finished && status==completed → 清空缓存` 的分支专门兜它,
			// 于是它在修复前就是过的 —— 正因为如此才必须显式测:
			// 只测这一条会得出"图路径没问题"的错误结论 (S5 才是漏的那半)。
			name:         "S5b_换目标首跑_上一轮正常收尾",
			seeded:       techblogStages,
			prevStatus:   TeamStatusCompleted,
			seedFinished: "completed",
			act: func(t *testing.T, ptm *ProductionTeamManager, team *ProductionTeam) {
				if err := ptm.RunTeam(team.Name, tbNewObjective); err != nil {
					t.Fatalf("RunTeam: %v", err)
				}
			},
			want: techblogStages,
			note: "上一轮已交付 + 换目标 → 仍是新的一次运行, 全部重跑",
		},
	}
}

// runProgressScenario 在指定路径 (graphOn) 上跑一个场景, 返回本轮真正执行的阶段。
func runProgressScenario(t *testing.T, graphOn bool, sc progressScenario) []string {
	t.Helper()
	if graphOn {
		t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")
	} else {
		t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "") // 显式清空: 不受外部环境影响
	}

	tmp := t.TempDir()
	probe := &stageProbe{}
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(tmp, "teams"),
		Factory: func(_ context.Context, role, _ string) (AgentRunner, error) {
			return &srcProbeRunner{role: role, probe: probe}, nil
		},
		Notify: func(_, _ string) {},
	})
	team, err := ptm.CreateTeam("eq", "techblog", tbObjective, "test")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	const seedRunID = "run-eq-seed"
	if len(sc.seeded) > 0 {
		// 两条路径**同样播种**: 各自往自己的进度真源写同一批已完成阶段。
		// 崩溃语义 (finished="") 见 seedGraphProgress 的注释。
		seedLegacyProgress(t, team.dataDir, sc.seeded)
		seedGraphProgress(t, team.dataDir, seedRunID, sc.seeded, sc.seedFinished)
	}
	team.mu.Lock()
	team.Status = sc.prevStatus
	if len(sc.seeded) > 0 {
		// LastRunID 是按阶段失效 (InvalidateFrom) 定位 run 的依据, 必须与播种一致,
		// 否则失效事件会被 Replay 按 RunID 过滤掉 —— 那是修过的一个静默失败形态。
		team.LastRunID = seedRunID
		for _, name := range sc.seeded {
			team.Stages = append(team.Stages, StageResult{
				Name: name, Status: TaskCompleted, Output: "上一轮产出:" + name,
			})
		}
	}
	team.mu.Unlock()
	team.persist()

	sc.act(t, ptm, team)
	team.WaitDone()

	wf := GetWorkflow("techblog")
	return probe.stages(wf)
}

// TestProgressSourceEquivalence 逐场景比对两条进度真源路径的恢复判定。
//
// 判定量 = 本轮真正被派发的阶段集合。任何一项不一致都意味着:
// 灰度开关一开, 用户就会看到"该重跑的没重跑"或"不该重跑的重跑了"。
func TestProgressSourceEquivalence(t *testing.T) {
	for _, sc := range progressScenarios() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			legacy := runProgressScenario(t, false, sc)
			graph := runProgressScenario(t, true, sc)

			if !sameStageSet(legacy, sc.want) {
				t.Errorf("旧路径(checkpoints.json) 执行阶段不符 [%s]\n  want=%v\n  got =%v",
					sc.note, sc.want, legacy)
			}
			if !sameStageSet(graph, sc.want) {
				t.Errorf("新路径(graph-journal) 执行阶段不符 [%s]\n  want=%v\n  got =%v",
					sc.note, sc.want, graph)
			}
			if !sameStageSet(legacy, graph) {
				t.Errorf("两条路径判定不等价 [%s]\n  checkpoints.json=%v\n  graph-journal   =%v",
					sc.note, legacy, graph)
			}
		})
	}
}

func sameStageSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
