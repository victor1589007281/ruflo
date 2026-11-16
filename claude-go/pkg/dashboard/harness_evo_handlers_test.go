package dashboard

// harness_evo_handlers_test.go — 进化运行控制面的纯函数测试:
// 进度锚点推导 + tiers 解析 + 重启归态 (不触 LLM/编排器)。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveHarnessEvoStage(t *testing.T) {
	cases := []struct {
		msg    string
		epochs int
		stage  string
		prog   int
	}{
		{"选题: 训练 4 题 ([a b c d]), 验证 8 题", 2, "选题", 5},
		{"selfcheck: 任务 t1 测试卷已冻结, 跳过 (幂等重入)", 2, "出卷/进化图执行", 15},
		{"新候选 h0-e1-I2 (parent=h0, plan=p3)", 2, "出卷/进化图执行", 15},
		{"地板 floor=1.00 (h0 验证集通过率)", 2, "地板评定", 25},
		{"epoch 1 选点: 前沿=[a] 晋级=[a]", 2, "epoch 1 选点", 43}, // 25+70/2-35/2 (选点在轮内)
		{"epoch 1 完成, 晋级: [a]", 2, "epoch 1 完成", 60},           // 25+70/2
		{"epoch 2 完成, 晋级: [a]", 2, "epoch 2 完成", 95},           // 25+70
		{"动刀部分失败 (1/3): 细节", 2, "出卷/进化图执行", 15},
	}
	for _, c := range cases {
		stage, prog := deriveHarnessEvoStage(c.msg, c.epochs)
		if stage != c.stage || prog != c.prog {
			t.Errorf("derive(%q, %d) = (%q, %d), want (%q, %d)",
				c.msg, c.epochs, stage, prog, c.stage, c.prog)
		}
	}
	// 未命中: 空 stage + 0 prog = 调用方不改动当前值。
	stage, prog := deriveHarnessEvoStage("h0 落盘失败 (继续): 无 git", 2)
	if stage != "" || prog != 0 {
		t.Errorf("未锚定消息应保持现场: got (%q,%d)", stage, prog)
	}
}

func TestParseHarnessEvoTiers(t *testing.T) {
	tiers, err := parseHarnessEvoTiers("lite:1,3|medium:1,1|heavy:3,1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tiers) != 3 || tiers[0].Tier != "lite" || tiers[0].Alpha != 1 || tiers[0].Beta != 3 {
		t.Fatalf("tiers = %+v", tiers)
	}
	if _, err := parseHarnessEvoTiers("lite:1"); err == nil {
		t.Fatal("缺 β 应报错")
	}
	if _, err := parseHarnessEvoTiers(""); err == nil {
		t.Fatal("空 tiers 应报错")
	}
}

// TestHarnessEvoManagerLoadMarksInterruptedRuns 控制面重启后, 落盘里残留的
// running 态必须被诚实标成 failed (进程内执行不跨重启续跑)。
func TestHarnessEvoManagerLoadMarksInterruptedRuns(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "harness-evo", "runs")
	write := func(id, status string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, id), 0o755); err != nil {
			t.Fatal(err)
		}
		content := `{"id":"` + id + `","params":{},"status":"` + status + `","stage":"地板评定","progress":25,"events":[],"workdir":"` + filepath.Join(root, id, "wd") + `","started_at":1760000000}`
		if err := os.WriteFile(filepath.Join(root, id, "status.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("evo-old-running", "running")
	write("evo-old-done", "completed")

	m := &harnessEvoManager{
		runs:    map[string]*harnessEvoRun{},
		cancels: map[string]context.CancelFunc{},
		root:    root,
	}
	m.loadFromDisk()
	if m.runs["evo-old-running"].Status != "failed" {
		t.Fatalf("interrupted run 应标 failed, got %q", m.runs["evo-old-running"].Status)
	}
	if m.runs["evo-old-running"].Error == "" {
		t.Fatal("interrupted run 应带解释性 error")
	}
	if m.runs["evo-old-done"].Status != "completed" {
		t.Fatalf("completed 运行不应被改写, got %q", m.runs["evo-old-done"].Status)
	}
}
