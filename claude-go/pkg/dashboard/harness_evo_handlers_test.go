package dashboard

// harness_evo_handlers_test.go — 进化运行控制面的纯函数测试:
// 进度锚点推导 + tiers 解析 + 重启归态 (不触 LLM/编排器)。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestHarnessEvoSafeJoin 产物路径越界防护: .. 逃逸 / 绝对路径 / 反斜杠全拒,
// workdir 内的正常相对路径放行。
func TestHarnessEvoSafeJoin(t *testing.T) {
	wd := filepath.Join(t.TempDir(), "wd")
	cases := []struct {
		rel  string
		want bool // true = 放行
	}{
		{"report.json", true},
		{"harnesses/h0/instructions.md", true},
		{"journal/epoch-1/journal.jsonl", true},
		{"../status.json", false},
		{"harnesses/../../status.json", false},
		{"/etc/passwd", false},
		{"..", false},
		{`..\status.json`, false},
	}
	for _, c := range cases {
		full, err := harnessEvoSafeJoin(wd, c.rel)
		if c.want {
			if err != nil {
				t.Fatalf("%q 应放行, got %v", c.rel, err)
			}
			if !strings.HasPrefix(full, wd+string(os.PathSeparator)) {
				t.Fatalf("%q 拼出越界路径 %q", c.rel, full)
			}
		} else if err == nil {
			t.Fatalf("%q 应被拒绝, got %q", c.rel, full)
		}
	}
}

// TestHarnessEvoListFiles 产物清单: 递归列出 workdir 文件, .git 内部跳过,
// 结果按路径排序。
func TestHarnessEvoListFiles(t *testing.T) {
	wd := t.TempDir()
	mk := func(rel, body string) {
		t.Helper()
		p := filepath.Join(wd, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("report.json", "{}")
	mk("harnesses/h0/instructions.md", "# h0")
	mk("journal/epoch-1/journal.jsonl", "{}\n")
	mk(".git/refs/heads/main", "fake-git-internal")
	mk(".gitignore", "wd")

	entries, err := harnessEvoListFiles(wd)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	joined := strings.Join(paths, ",")
	if strings.Contains(joined, ".git/") {
		t.Fatalf(".git 内部文件不应出现: %v", paths)
	}
	if len(entries) != 4 {
		t.Fatalf("应列出 4 个文件 (.git 内部被跳过), got %d: %v", len(entries), paths)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path >= entries[i].Path {
			t.Fatalf("清单未按路径排序: %v", paths)
		}
	}
}

// TestParseHarnessEvoPool 自定义任务池解析: 合法数组 round-trip 成 Task,
// 缺 id/prompt/重复 id/少于 2 题全部拒绝。
func TestParseHarnessEvoPool(t *testing.T) {
	good := []any{
		map[string]any{"id": "t1", "category": "text", "difficulty": 0.3, "prompt": "做题一"},
		map[string]any{"id": "t2", "prompt": "做题二"},
	}
	pool, err := parseHarnessEvoPool(good)
	if err != nil || len(pool) != 2 || pool[0].ID != "t1" || pool[1].Prompt != "做题二" {
		t.Fatalf("合法 pool 应解析成功, got pool=%+v err=%v", pool, err)
	}
	if pool[0].Difficulty != 0.3 {
		t.Fatalf("difficulty 应保留, got %v", pool[0].Difficulty)
	}

	bad := []any{
		map[string]any{"prompt": "缺 id"},
		map[string]any{"id": "t1", "prompt": "p"},
	}
	if _, err := parseHarnessEvoPool(bad); err == nil {
		t.Fatal("缺 id 的题应被拒绝")
	}
	if _, err := parseHarnessEvoPool(good[:1]); err == nil {
		t.Fatal("少于 2 题应被拒绝 (训练/验证切不开)")
	}
	dup := []any{
		map[string]any{"id": "t1", "prompt": "a"},
		map[string]any{"id": "t1", "prompt": "b"},
	}
	if _, err := parseHarnessEvoPool(dup); err == nil {
		t.Fatal("重复 id 应被拒绝")
	}
	if err := validateHarnessEvoPool(nil); err != nil {
		t.Fatalf("nil pool = 内置池, 不应报错: %v", err)
	}
}
