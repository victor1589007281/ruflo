// evolution_triad_test.go —— 13.8.6 P2 经验三段式闸口的语义锁定。
//
// 规划锚点 (Trajectory-Informed Memory, arXiv:2603.10600): 经验蒸馏输出强制
// {现象→根因→下一步动作} 三段, 纯叙述性经验不入库。
//
// 核心断言:
//  1. 三段齐备的条目入库, Content 由三段确定性渲染 (检索路径契约不变)。
//  2. 缺根因或缺动作的纯叙述条目被拒收 —— LLM 路径与启发式路径同一闸口。
//  3. M3 验收判据: 拒收后经验库不产生任何非三段式条目 (IsTriadic 全量成立)。
//  4. LLM 老格式 (content 字段) 走兜底解析, 解析不出同样拒收。
package agent

import (
	"context"
	"strings"
	"testing"
)

// stubDistillLLM 可编程的 SimpleComplete 假件 (三段式 JSON / 老格式 / 垃圾)。
type stubDistillLLM struct{ reply string }

func (f stubDistillLLM) SimpleComplete(_ context.Context, _, _ string) (string, error) {
	return f.reply, nil
}

func trajSuccess() Trajectory {
	return Trajectory{RunID: "run-t", TeamName: "t", StageName: "impl", Role: "coder",
		Objective: "实现登录接口", Success: true,
		Output: strings.Repeat("决策: 采用 JWT 无状态鉴权方案。\n", 20)}
}

func trajFailure() Trajectory {
	return Trajectory{RunID: "run-f", TeamName: "t", StageName: "impl", Role: "coder",
		Objective: "实现登录接口", Success: false, Error: "compilation failed: undefined: Auth"}
}

// 三段齐备 → 入库, 字段与 Content 渲染一致。
func TestTriad_三段齐备入库(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	ee.LearnFromStage(trajFailure())

	if got := len(ee.experiences); got != 1 {
		t.Fatalf("三段式失败经验应入库 1 条, got %d", got)
	}
	exp := ee.experiences[0]
	if !exp.IsTriadic() {
		t.Fatalf("入库条目必须三段齐备: symptom=%q rootCause=%q action=%q", exp.Symptom, exp.RootCause, exp.Action)
	}
	want := renderTriad(exp.Symptom, exp.RootCause, exp.Action)
	if exp.Content != want {
		t.Fatalf("Content 应由三段渲染: want %q got %q", want, exp.Content)
	}
	if !strings.Contains(exp.Content, "根因") || !strings.Contains(exp.Content, "动作") {
		t.Errorf("渲染 Content 应保留三段标记: %q", exp.Content)
	}
}

// 成功轨迹提不出根因 (无关键模式) → 拒收, 不产生纯叙述条目。
func TestTriad_无根因的成功拒收(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	traj := trajSuccess()
	traj.Output = strings.Repeat("这次跑得很好, 一切顺利。\n", 30) // 无任何模式行
	ee.LearnFromStage(traj)

	if got := len(ee.experiences); got != 0 {
		t.Fatalf("无根因的成功 (纯叙述) 必须拒收, got %d 条", got)
	}
}

// 失败但 suggestFix 无话可说 (不可能, 但闸口本身要硬) → 拒收。
func TestTriad_无动作拒收(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	if !ee.appendTriadicLocked(&Experience{Symptom: "s", RootCause: "r", Action: ""}) {
		// 预期拒收
	} else {
		t.Fatal("缺动作必须拒收")
	}
	if got := len(ee.experiences); got != 0 {
		t.Fatalf("缺动作条目不应入库, got %d", got)
	}
}

// LLM 输出老格式 content (纯叙述) → 拒收, 不回退入库。
func TestTriad_LLM纯叙述拒收(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), stubDistillLLM{reply: `[
		{"category":"role","role":"coder","content":"该角色完成了登录接口的开发, 表现不错","tags":["登录"]}
	]`})
	ee.llmDistill(context.Background(), []Trajectory{trajSuccess()}, "t")

	if got := len(ee.experiences); got != 0 {
		t.Fatalf("LLM 纯叙述条目必须拒收, got %d: %+v", got, ee.experiences)
	}
}

// LLM 输出三段式 → 全部入库且字段齐备 (M3: 新增条目 100% 三段式)。
func TestTriad_LLM三段式入库(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), stubDistillLLM{reply: `[
		{"category":"error","role":"coder","symptom":"编译失败 undefined: Auth","rootCause":"auth 包未导入","action":"检查 import 并补齐声明","tags":["compile"]},
		{"category":"role","role":"reviewer","symptom":"评审通过","rootCause":"测试覆盖到位","action":"保持先写测试的顺序","tags":["review"]},
		{"category":"role","role":"legacy","content":"老格式的叙述性经验, 没有三段","tags":[]}
	]`})
	ee.llmDistill(context.Background(), []Trajectory{trajSuccess()}, "t")

	if got := len(ee.experiences); got != 2 {
		t.Fatalf("三段式 2 条入库 + 纯叙述 1 条拒收, got %d", got)
	}
	for _, exp := range ee.experiences {
		if !exp.IsTriadic() {
			t.Fatalf("入库条目必须三段齐备: %+v", exp)
		}
	}
}

// 老格式 content 带 "现象→根因→动作" 约定 → 兜底解析后入库。
func TestTriad_老格式content兜底解析(t *testing.T) {
	s, r, a := parseTriadContent("编译失败 → auth 包未导入 → 检查 import")
	if s == "" || r == "" || a == "" {
		t.Fatalf("箭头约定应解析出三段, got (%q,%q,%q)", s, r, a)
	}
	// 不带箭头的纯叙述解析失败 → 全空 → 拒收路径。
	if s2, r2, a2 := parseTriadContent("该角色完成了开发"); s2 != "" || r2 != "" || a2 != "" {
		t.Fatalf("纯叙述应解析失败, got (%q,%q,%q)", s2, r2, a2)
	}
}

// heuristicDistill (无 LLM 回退路径) 同样执行三段闸口。
func TestTriad_启发式路径同闸口(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	// 失败轨迹 → 入库 (有错误类型+建议)。
	// 成功但无模式 → 拒收。
	ee.heuristicDistill([]Trajectory{trajFailure(), trajSuccess()}, "t")

	for _, exp := range ee.experiences {
		if !exp.IsTriadic() {
			t.Fatalf("启发式路径产生了非三段式条目: %+v", exp)
		}
	}
	if got := len(ee.experiences); got < 1 {
		t.Fatalf("失败轨迹应至少入 1 条, got %d", got)
	}
	// 成功条目必须带根因 (关键模式) 才入库; trajSuccess 有模式行, 所以应有 role 条。
	hasRole := false
	for _, exp := range ee.experiences {
		if exp.Category == "role" {
			hasRole = true
			if !strings.Contains(exp.RootCause, "关键模式") {
				t.Errorf("成功条目根因应含关键模式: %q", exp.RootCause)
			}
		}
	}
	if !hasRole {
		t.Error("带模式行的成功轨迹应入一条 role 条目")
	}
}

// 反事实学习也是三段式。
func TestTriad_反事实三段式(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	ee.LearnCounterfactual(trajFailure())

	if got := len(ee.experiences); got != 1 {
		t.Fatalf("反事实应入库 1 条, got %d", got)
	}
	if exp := ee.experiences[0]; !exp.IsTriadic() || !strings.Contains(exp.Action, "策略") {
		t.Fatalf("反事实条目应三段齐备且含替代策略: %+v", exp)
	}
}

// 去重不虚增计数: 同一条轨迹学两遍, 第二遍被 MinHash+Jaccard 挡掉。
func TestTriad_重复去重计数不虚增(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	ee.LearnFromStage(trajFailure())
	before := len(ee.experiences)
	distilled := ee.totalDistilled
	ee.LearnFromStage(trajFailure()) // 同轨迹再来一遍

	if len(ee.experiences) != before {
		t.Fatalf("重复轨迹不应再入库: before=%d after=%d", before, len(ee.experiences))
	}
	if ee.totalDistilled != distilled {
		t.Fatalf("去重丢弃不应推进 totalDistilled: %d → %d", distilled, ee.totalDistilled)
	}
}
