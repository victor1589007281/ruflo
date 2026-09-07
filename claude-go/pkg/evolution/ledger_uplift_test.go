// ledger_uplift_test.go —— 配对 uplift 的语义锁定（13.8.3 纪律：每个 FoldSpec 带单测）。
//
// 核心断言：
//  1. 同桶内"有注入好/无注入差"→ 正 uplift；反之负——方向不能反。
//  2. 无对照的桶（只有注入臂或只有基线臂）不判定——这正是配对法与旧全局法的分野。
//  3. 无 RunID 的注入记录（老数据）被跳过，不猜。
//  4. 节点级奖励分与轨迹成败两口径：全桶有分用连续分，混口径退回成败二值。
//  5. 跨桶按 min(n_inj, n_base) 加权——小桶不被大桶淹没。
package evolution

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/evolution/learners"
)

func writeUpliftFixture(t *testing.T, stateDir string, injections []injectionRec, trajs []learners.TrajectoryRow, rewards []learners.RewardRow) {
	t.Helper()
	evoDir := filepath.Join(stateDir, "evolution")
	if err := os.MkdirAll(evoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if injections != nil {
		b, err := json.MarshalIndent(injections, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evoDir, "injections.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if trajs != nil {
		b, err := json.Marshal(trajs)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evoDir, "trajectories.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if rewards != nil {
		var sb []byte
		for _, r := range rewards {
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			sb = append(sb, append(b, '\n')...)
		}
		if err := os.WriteFile(filepath.Join(evoDir, "rewards.jsonl"), sb, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func injRec(taskID, runID string, score float64) injectionRec {
	return injectionRec{TaskID: taskID, Score: score, Success: score > 0, RunID: runID,
		Timestamp: time.Unix(1757000000, 0)}
}

func trajRow(runID, stage, objective string, success bool) learners.TrajectoryRow {
	return learners.TrajectoryRow{RunID: runID, StageName: stage, Objective: objective, Success: success}
}

func rewardRow(runID, node string, v float64) learners.RewardRow {
	return learners.RewardRow{TS: 1757000000000, RunID: runID, NodeID: node, Source: "gate.test", Value: v, Weight: 1.0}
}

// TestFoldInjectionUplift_同桶配对求差方向 正向：注入臂全成、基线臂全败 → +1。
func TestFoldInjectionUplift_同桶配对求差方向(t *testing.T) {
	dir := t.TempDir()
	injs := []injectionRec{injRec("impl", "run-inj-1", 1), injRec("impl", "run-inj-2", 1)}
	trajs := []learners.TrajectoryRow{
		// 桶键用 ObjectiveSignature(目标)：精确 token 匹配，中文差一个字 bigram 集就不同。
		// 同桶必须逐字一致（'写个爬虫'≠'写爬虫程序'），英文短句靠 stopword 归一。
		trajRow("run-inj-1", "impl", "写个爬虫", true),
		trajRow("run-inj-2", "impl", "写个爬虫", true),
		trajRow("run-base-1", "impl", "写个爬虫", false),
		trajRow("run-base-2", "impl", "写个爬虫", false),
	}
	writeUpliftFixture(t, dir, injs, trajs, nil)

	got := FoldInjectionUplift(dir)
	if got.Buckets != 1 || got.Uplift != 1 {
		t.Fatalf("期望 1 桶 uplift=+1, got buckets=%d uplift=%v (inj=%d base=%d)",
			got.Buckets, got.Uplift, got.InjRuns, got.BaseRuns)
	}
}

// 反向：注入臂反而差 → 负值。
func TestFoldInjectionUplift_注入臂更差给负值(t *testing.T) {
	dir := t.TempDir()
	injs := []injectionRec{injRec("impl", "run-inj-1", -1)}
	trajs := []learners.TrajectoryRow{
		trajRow("run-inj-1", "impl", "review the login code", false),
		trajRow("run-base-1", "impl", "review the login code", true),
	}
	writeUpliftFixture(t, dir, injs, trajs, nil)

	if got := FoldInjectionUplift(dir); got.Buckets != 1 || got.Uplift >= 0 {
		t.Fatalf("注入臂更差应给负 uplift, got %+v", got)
	}
}

// TestFoldInjectionUplift_无对照桶不判定：只有注入臂的桶必须被排除，
// 否则就退回了"只有注入后表现"的旧全局口径。
func TestFoldInjectionUplift_无对照桶不判定(t *testing.T) {
	dir := t.TempDir()
	injs := []injectionRec{injRec("impl", "run-inj-1", 1)}
	trajs := []learners.TrajectoryRow{
		trajRow("run-inj-1", "impl", "写个爬虫", true),
		// 另一节点只有基线臂 —— 桶间不凑对。
		trajRow("run-base-9", "test", "写个爬虫", false),
	}
	writeUpliftFixture(t, dir, injs, trajs, nil)

	if got := FoldInjectionUplift(dir); !math.IsNaN(got.Uplift) {
		t.Fatalf("无对照桶应不判定 (NaN), got %+v", got)
	}
}

// 老数据（无 RunID 的注入记录）跳过不猜。
func TestFoldInjectionUplift_老记录无RunID跳过(t *testing.T) {
	dir := t.TempDir()
	injs := []injectionRec{injRec("impl", "", 1)} // RunID 空
	trajs := []learners.TrajectoryRow{
		trajRow("run-x", "impl", "写个爬虫", true),
		trajRow("run-y", "impl", "写爬虫", false),
	}
	writeUpliftFixture(t, dir, injs, trajs, nil)

	if got := FoldInjectionUplift(dir); !math.IsNaN(got.Uplift) {
		t.Fatalf("无 RunID 的注入记录不能成为配对臂, got %+v", got)
	}
}

// 两口径：全桶有节点级奖励分 → 用连续分；混口径 → 退回成败二值。
func TestFoldInjectionUplift_口径选择(t *testing.T) {
	dir := t.TempDir()
	// 桶 A（连续分口径）: 注入臂 0.75/0.75, 基线臂 0.25 → diff=+0.5。
	injs := []injectionRec{injRec("impl", "run-a1", 1), injRec("impl", "run-a2", 1)}
	trajs := []learners.TrajectoryRow{
		trajRow("run-a1", "impl", "写爬虫", true),
		trajRow("run-a2", "impl", "写爬虫", true),
		trajRow("run-b1", "impl", "写爬虫", true), // 轨迹说成功, 但奖励分差 —— 连续分口径应信后者
	}
	rewards := []learners.RewardRow{
		rewardRow("run-a1", "impl", 0.5),
		rewardRow("run-a2", "impl", 0.5),
		rewardRow("run-b1", "impl", -0.5),
	}
	writeUpliftFixture(t, dir, injs, trajs, rewards)
	got := FoldInjectionUplift(dir)
	// score01: 0.5→0.75, -0.5→0.25。diff = 0.75 - 0.25 = +0.5。
	if got.Buckets != 1 || math.Abs(got.Uplift-0.5) > 1e-9 {
		t.Fatalf("连续分口径期望 uplift=+0.5, got %+v", got)
	}

	// 混口径：补一条无奖励分的行 → 整桶退回成败二值（全成功 → diff=0）。
	trajs = append(trajs, trajRow("run-c1", "impl", "写爬虫", true))
	writeUpliftFixture(t, dir, injs, trajs, rewards)
	if got := FoldInjectionUplift(dir); got.Buckets != 1 || got.Uplift != 0 {
		t.Fatalf("混口径应退回二值 (全成功 diff=0), got %+v", got)
	}
}

// 跨桶加权：大桶 2:2 diff=+0.5, 小桶 1:1 diff=-1 → 加权 (0.5*2 + -1*1)/3 = 0。
func TestFoldInjectionUplift_跨桶min加权(t *testing.T) {
	dir := t.TempDir()
	injs := []injectionRec{
		injRec("impl", "run-i1", 1), injRec("impl", "run-i2", 1), // 大桶两臂 2:2
		injRec("review", "run-r1", 1), // 小桶 1:1
	}
	trajs := []learners.TrajectoryRow{
		trajRow("run-i1", "impl", "写爬虫", true),
		trajRow("run-i2", "impl", "写爬虫", true),
		trajRow("run-b1", "impl", "写爬虫", false),
		trajRow("run-b2", "impl", "写爬虫", false), // diff=+1... 见下: 二值口径
		trajRow("run-r1", "review", "写爬虫", false),
		trajRow("run-rb1", "review", "写爬虫", true), // 小桶 diff=-1
	}
	writeUpliftFixture(t, dir, injs, trajs, nil)
	got := FoldInjectionUplift(dir)
	if got.Buckets != 2 {
		t.Fatalf("应有 2 个判定桶, got %+v", got)
	}
	// 大桶: 注入 1,1 − 基线 0,0 → diff=+1; 小桶: 0−1 → diff=-1。
	// 权重 min(2,2)=2 与 min(1,1)=1 → (2*1 + 1*(-1))/3 = +1/3。
	if math.Abs(got.Uplift-1.0/3.0) > 1e-9 {
		t.Fatalf("min 加权期望 +1/3, got %v", got.Uplift)
	}
}

// 空账本 → NaN 键缺失。
func TestFoldInjectionUplift_空账本NaN(t *testing.T) {
	if got := FoldInjectionUplift(t.TempDir()); !math.IsNaN(got.Uplift) {
		t.Fatalf("空账本应 NaN, got %+v", got)
	}
}
