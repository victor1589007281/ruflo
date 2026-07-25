package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/learners"
)

// newStructureLoop 造一个装了 d/e 的循环, 并在 stateDir 里预置真实数据。
func newStructureLoop(t *testing.T, cfg StructureConfig) (*EvolutionLoop, string) {
	t.Helper()
	state := t.TempDir()
	evoDir := filepath.Join(state, "evolution")
	if err := os.MkdirAll(evoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ee := NewEvolutionEngine(evoDir, nil)
	l := NewEvolutionLoop(ee, newCountingSink(), EvolutionLoopConfig{IdleAfter: -1})
	if l == nil {
		t.Fatal("NewEvolutionLoop 返回 nil")
	}
	cfg.StateDir = state
	l.EnableStructureLearning(cfg)
	return l, state
}

// 写 3 个同形状高奖励 run, 让 AWM 归纳刚好达标。
func seedInducibleRuns(t *testing.T, state string) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	var rw strings.Builder
	var trajs []learners.TrajectoryRow
	for i, id := range []string{"r1", "r2", "r3"} {
		row := learners.RewardRow{TS: base.Add(time.Duration(i) * time.Minute).UnixMilli(),
			RunID: id, Source: RewardSourceGateCompile, Value: 1, Weight: 1, Team: "t"}
		data, _ := json.Marshal(row)
		rw.Write(data)
		rw.WriteByte('\n')
		for j, stage := range []string{"design", "implement"} {
			trajs = append(trajs, learners.TrajectoryRow{
				RunID: id, TeamName: "t", StageName: stage, Objective: "为订单服务补齐分布式事务",
				Input: "做" + stage, Output: strings.Repeat("真实产出内容。", 20), Success: true,
				Timestamp: base.Add(time.Duration(i)*time.Minute + time.Duration(j)*time.Second),
			})
		}
	}
	dir := filepath.Join(state, "evolution")
	if err := os.WriteFile(filepath.Join(dir, "rewards.jsonl"), []byte(rw.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(trajs)
	if err := os.WriteFile(filepath.Join(dir, "trajectories.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// 未装配 d/e 时空闲相位行为不变 (向后兼容硬要求)。
func TestRunStructureLearners_未装配则无副作用(t *testing.T) {
	ee := NewEvolutionEngine(t.TempDir(), nil)
	l := NewEvolutionLoop(ee, nil, EvolutionLoopConfig{})
	l.runStructureLearners(context.Background()) // 不 panic
	var nilLoop *EvolutionLoop
	nilLoop.runStructureLearners(context.Background())
	nilLoop.EnableStructureLearning(StructureConfig{})
}

// 空闲相位真的会跑 AWM 归纳并落草案 —— 这是 d 通电的判据。
func TestRunStructureLearners_空闲相位产出草案(t *testing.T) {
	l, state := newStructureLoop(t, StructureConfig{})
	seedInducibleRuns(t, state)

	l.runStructureLearners(context.Background())

	props := learners.ListProposals(state)
	if len(props) != 1 {
		t.Fatalf("空闲相位应归纳出 1 份草案, got %d", len(props))
	}
	if props[0].Kind != learners.ProposalWorkflow || props[0].Status != "proposed" {
		t.Errorf("应是 proposed 态的 workflow 草案: %+v", props[0])
	}
	if props[0].CreatedBy != "learners.InduceWorkflows" {
		t.Errorf("CreatedBy 应留痕产出者, got %q", props[0].CreatedBy)
	}
}

// LearnIdle 经 execute 走到结构学习 (证明挂载点在真实调度路径上, 不是旁路函数)。
func TestExecute_空闲请求触发结构学习(t *testing.T) {
	l, state := newStructureLoop(t, StructureConfig{})
	seedInducibleRuns(t, state)

	l.execute(context.Background(), LearnRequest{Kind: LearnIdle})

	if got := len(learners.ListProposals(state)); got != 1 {
		t.Fatalf("LearnIdle 应经 execute 触发归纳, got %d 份草案", got)
	}
	if l.Stats.IdleRuns.Load() != 1 {
		t.Errorf("IdleRuns 应计数, got %d", l.Stats.IdleRuns.Load())
	}
}

// team_done 相位**不**跑结构学习: 归纳要跨多个 run, 每次完成都跑纯属白烧 CPU。
func TestExecute_团队完成不跑结构学习(t *testing.T) {
	l, state := newStructureLoop(t, StructureConfig{})
	seedInducibleRuns(t, state)

	l.execute(context.Background(), LearnRequest{Kind: LearnTeamDone, Team: "t"})

	if got := len(learners.ListProposals(state)); got != 0 {
		t.Fatalf("交付路径的学习相位不该跑归纳, got %d 份草案", got)
	}
}

// H3: 未注入独立 reflector 时 prompt 进化必须跳过, 绝不退而用主模型自评。
func TestEvolvePrompts_无独立反思器则跳过(t *testing.T) {
	l, state := newStructureLoop(t, StructureConfig{Reflector: nil})
	// 造一个负奖励节点 (design 阶段)
	row := learners.RewardRow{TS: time.Now().UnixMilli(), RunID: "r1", NodeID: "design",
		Source: RewardSourceGateContent, Value: -0.8, Weight: 0.5}
	d1, _ := json.Marshal(row)
	row.RunID, row.TS = "r2", time.Now().UnixMilli()+1
	d2, _ := json.Marshal(row)
	if err := os.WriteFile(filepath.Join(state, "evolution", "rewards.jsonl"),
		append(append(d1, '\n'), append(d2, '\n')...), 0o644); err != nil {
		t.Fatal(err)
	}

	l.evolvePrompts(context.Background(), l.structure.cfg)

	for _, p := range learners.ListProposals(state) {
		if p.Kind == learners.ProposalPrompt {
			t.Fatalf("未注入独立 reflector 时不该产 prompt 草案 (§4.2 H3): %+v", p)
		}
	}
}

// 导出默认关: 不显式开启就不该产出语料文件。
func TestRunStructureLearners_导出默认关(t *testing.T) {
	l, state := newStructureLoop(t, StructureConfig{})
	seedInducibleRuns(t, state)
	l.runStructureLearners(context.Background())
	if _, err := os.Stat(filepath.Join(state, "evolution", "export")); err == nil {
		t.Error("导出默认关 (设计明写), 不该建出 export 目录")
	}
}

// 显式开启后导出真的产出 (e 可通电的判据)。
func TestRunStructureLearners_显式开启后导出语料(t *testing.T) {
	l, state := newStructureLoop(t, StructureConfig{ExportEnabled: true, ExportDPO: true})
	seedInducibleRuns(t, state)
	l.runStructureLearners(context.Background())
	if _, err := os.Stat(filepath.Join(state, "evolution", "export", "sft.jsonl")); err != nil {
		t.Fatalf("开启后应产出 SFT 语料: %v", err)
	}
}

// env 只能打开导出, 不能关掉显式配置 (单向开关, 避免"配置说开环境说关"的死结)。
func TestEnableStructureLearning_env单向打开导出(t *testing.T) {
	t.Setenv("CLAUDE_GO_EVO_EXPORT", "1")
	l, _ := newStructureLoop(t, StructureConfig{ExportEnabled: false})
	if !l.structure.cfg.ExportEnabled {
		t.Error("env=1 应能打开导出")
	}

	t.Setenv("CLAUDE_GO_EVO_EXPORT", "0")
	l2, _ := newStructureLoop(t, StructureConfig{ExportEnabled: true})
	if !l2.structure.cfg.ExportEnabled {
		t.Error("env=0 不该关掉显式开启的导出 (单向)")
	}
}

// StateDir 未给时应从引擎 dataDir 推导 (dataDir = <state>/evolution)。
func TestEnableStructureLearning_从引擎推导StateDir(t *testing.T) {
	state := t.TempDir()
	ee := NewEvolutionEngine(filepath.Join(state, "evolution"), nil)
	l := NewEvolutionLoop(ee, nil, EvolutionLoopConfig{})
	l.EnableStructureLearning(StructureConfig{})
	if l.structure.cfg.StateDir != state {
		t.Errorf("StateDir 应推导为 %q, got %q", state, l.structure.cfg.StateDir)
	}
	if l.structure.cfg.MaxPromptEvolvePerRound != 1 {
		t.Errorf("每轮 prompt 改写数默认应为 1 (多改会让 uplift 无法归因), got %d",
			l.structure.cfg.MaxPromptEvolvePerRound)
	}
}

// resolveStagePrompt: 歧义即放弃, 门禁类节点排除。
func TestResolveStagePrompt_歧义放弃与门禁排除(t *testing.T) {
	if _, _, ok := resolveStagePrompt("gate.compile"); ok {
		t.Error("门禁类 NodeID 没有 prompt 可改, 应排除")
	}
	if _, _, ok := resolveStagePrompt(""); ok {
		t.Error("空节点名应排除")
	}
	if _, _, ok := resolveStagePrompt("绝不可能存在的阶段名xyzzy"); ok {
		t.Error("匹配不到任何工作流阶段时应放弃")
	}
	// 真实工作流里存在的阶段应能解析出 workflow/stage 形式的 target。
	found := false
	for _, wf := range ListWorkflows() {
		for _, st := range wf.Stages {
			if strings.TrimSpace(st.Prompt) == "" {
				continue
			}
			if target, prompt, ok := resolveStagePrompt(st.Name); ok {
				found = true
				if !strings.Contains(target, "/") || prompt == "" {
					t.Errorf("target 应为 workflow/stage 形式且带 prompt: %q", target)
				}
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Skip("当前工作流注册表里没有唯一命名且带 StageDef.Prompt 的阶段, 跳过正向断言")
	}
}

// FallbackReflector: 没有 fallback 档位时必须返回 nil, 绝不退而用主模型。
func TestFallbackReflector_无fallback返回nil(t *testing.T) {
	if FallbackReflector(nil) != nil {
		t.Error("nil client 应返回 nil")
	}
}

// collectFailureEvidence 只取负奖励 run 的证据 (拿成功 run 的产出去"反思失败"会带偏)。
func TestCollectFailureEvidence_只取负奖励run(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "evolution")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	trajs := []learners.TrajectoryRow{
		{RunID: "bad", StageName: "design", Output: "这是失败产出", Error: "编译不过"},
		{RunID: "good", StageName: "design", Output: "这是成功产出"},
	}
	data, _ := json.Marshal(trajs)
	if err := os.WriteFile(filepath.Join(dir, "trajectories.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	ev := collectFailureEvidence(state, learners.NodeWeakness{Node: "design", NegRunIDs: []string{"bad"}})
	if len(ev) != 1 {
		t.Fatalf("只该取 bad 那一条, got %v", ev)
	}
	if !strings.Contains(ev[0], "编译不过") || !strings.Contains(ev[0], "失败产出") {
		t.Errorf("证据应含报错与产出: %q", ev[0])
	}
}
