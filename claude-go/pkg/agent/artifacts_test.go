package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newArtifactTeam 造一个只带落点信息的团队。同包测试可直接设私有 dataDir
// (graph_adapter_test.go 的 newStubTeam 只设 dataDir 不设 Cwd, 采不到落点 B)。
func newArtifactTeam(name, dataDir, cwd string, started, finished time.Time) *ProductionTeam {
	return &ProductionTeam{
		Name:       name,
		Workflow:   "artifact-test",
		Objective:  "测试产物采集",
		Agents:     map[string]*BGAgent{},
		CreatedAt:  started,
		StartedAt:  started,
		FinishedAt: finished,
		Cwd:        cwd,
		dataDir:    dataDir,
	}
}

// writeFileAt 写文件并可指定 mtime (归属判据靠 mtime, 必须能精确构造)。
func writeFileAt(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func findRef(refs []ArtifactRef, root, rel string) *ArtifactRef {
	for i := range refs {
		if refs[i].Root == root && refs[i].Rel == rel {
			return &refs[i]
		}
	}
	return nil
}

func countRel(refs []ArtifactRef, rel string) int {
	n := 0
	for _, r := range refs {
		if r.Rel == rel {
			n++
		}
	}
	return n
}

func relsIn(refs []ArtifactRef, root string) []string {
	var out []string
	for _, r := range refs {
		if r.Root == root {
			out = append(out, r.Rel)
		}
	}
	return out
}

// TestArtifactRootsDefaultStateDirDedup 默认配置 (stateDir=<cwd>/.claude-go, dataDir
// 嵌套在 cwd 内) 下: 根不重复, 且 dataDir 里的平台文件不得被当成 cwd 工作产物重复计数。
func TestArtifactRootsDefaultStateDirDedup(t *testing.T) {
	cwd := t.TempDir()
	dataDir := filepath.Join(cwd, ".claude-go", "teams", "t1")
	started := time.Now().Add(-time.Hour)
	team := newArtifactTeam("t1", dataDir, cwd, started, time.Time{})

	writeFileAt(t, filepath.Join(dataDir, "REPORT.md"), "# 报告", time.Time{})
	writeFileAt(t, filepath.Join(dataDir, "team.json"), "{}", time.Time{})
	writeFileAt(t, filepath.Join(dataDir, "media", "poster.png"), "PNG", time.Time{})
	writeFileAt(t, filepath.Join(cwd, "main.go"), "package main", time.Time{})

	roots := ArtifactRoots(team)
	if len(roots) != 2 {
		t.Fatalf("默认配置应返回 data+work 两个根, got %d: %+v", len(roots), roots)
	}
	if roots[0].Kind != ArtifactRootData || roots[1].Kind != ArtifactRootWork {
		t.Fatalf("根类别不符: %+v", roots)
	}
	if roots[0].Path == roots[1].Path {
		t.Fatalf("两个根路径重复: %+v", roots)
	}
	if !strings.HasPrefix(roots[0].Path, roots[1].Path+string(os.PathSeparator)) {
		t.Fatalf("默认配置下 dataDir 必须嵌套在 cwd 内 (本用例前提): %+v", roots)
	}

	refs, err := CollectArtifacts(team)
	if err != nil {
		t.Fatalf("采集应成功: %v", err)
	}
	// 去重: REPORT.md 只能出现一次, 且归 data 根 (不是 cwd 工作产物)。
	if n := countRel(refs, "REPORT.md"); n != 1 {
		t.Fatalf("REPORT.md 应只出现 1 次, got %d (refs=%+v)", n, refs)
	}
	if findRef(refs, ArtifactRootData, "REPORT.md") == nil {
		t.Fatalf("REPORT.md 应归 data 根: %+v", refs)
	}
	if findRef(refs, ArtifactRootData, "media/poster.png") == nil {
		t.Fatalf("dataDir/media 下的产物应被采到: %+v", refs)
	}
	for _, rel := range relsIn(refs, ArtifactRootWork) {
		if strings.HasPrefix(rel, ".claude-go/") {
			t.Fatalf("work 根不得把平台状态目录当工作产物: %q (refs=%+v)", rel, refs)
		}
	}
	if findRef(refs, ArtifactRootWork, "main.go") == nil {
		t.Fatalf("cwd 侧文件应被采到: %+v", refs)
	}
}

// TestArtifactRootsSeparateStateDir 生产配置 (stateDir=~/.claude-go 式, 与 cwd 无关):
// 返回两个互不包含的根。
func TestArtifactRootsSeparateStateDir(t *testing.T) {
	cwd := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "teams", "t2")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	team := newArtifactTeam("t2", dataDir, cwd, time.Now().Add(-time.Hour), time.Time{})

	roots := ArtifactRoots(team)
	if len(roots) != 2 {
		t.Fatalf("应返回两个根, got %d: %+v", len(roots), roots)
	}
	a, b := roots[0].Path, roots[1].Path
	if strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator)) || a == b {
		t.Fatalf("两个根应互不相交: %q vs %q", a, b)
	}
}

// TestArtifactRootsToolResults tool 根 (<cwd>/.claude-go/artifacts/tool-results) 存在时
// 单独成根, 其文件不得同时算进 work 根。
func TestArtifactRootsToolResults(t *testing.T) {
	cwd := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "teams", "t3")
	started := time.Now().Add(-time.Hour)
	team := newArtifactTeam("t3", dataDir, cwd, started, time.Time{})
	writeFileAt(t, filepath.Join(cwd, ".claude-go", "artifacts", "tool-results", "big.txt"), "huge", time.Time{})

	roots := ArtifactRoots(team)
	if len(roots) != 3 {
		t.Fatalf("tool-results 存在时应有 3 个根, got %d: %+v", len(roots), roots)
	}
	refs, err := CollectArtifacts(team)
	if err != nil {
		t.Fatalf("采集应成功: %v", err)
	}
	if findRef(refs, ArtifactRootTool, "big.txt") == nil {
		t.Fatalf("tool 根产物未采到: %+v", refs)
	}
	if n := countRel(refs, "big.txt"); n != 1 {
		t.Fatalf("tool 根文件不应被 work 根重复计数, got %d 次", n)
	}
}

// TestCollectArtifactsBothLandingSpots 媒锻回归: 只写在 team.Cwd 子目录里的产物
// (旧实现只扫 <stateDir>/teams/<team>/ → 漏采 → fail-open 把成品误置 review) 必须被采到;
// 只写在 dataDir/media 的也必须采到; 两者同时存在时都在清单里。
func TestCollectArtifactsBothLandingSpots(t *testing.T) {
	cwd := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "teams", "mf")
	started := time.Now().Add(-time.Hour)
	team := newArtifactTeam("mf", dataDir, cwd, started, time.Time{})

	// ① pipeline agent 把音乐写到团队 cwd 的子目录 (media_gen 的 resolveOutPath 锚定 cwd)
	writeFileAt(t, filepath.Join(cwd, "assets", "song.wav"), "RIFFfakewav", time.Time{})
	// ② creative-v2 把图写到 dataDir/media
	writeFileAt(t, filepath.Join(dataDir, "media", "cover.png"), "\x89PNG\r\n\x1a\n", time.Time{})

	refs, err := CollectArtifacts(team)
	if err != nil {
		t.Fatalf("采集应成功: %v", err)
	}
	wav := findRef(refs, ArtifactRootWork, "assets/song.wav")
	if wav == nil {
		t.Fatalf("只写在 team.Cwd 子目录的音频必须被采到 (媒锻漏采根因): %+v", refs)
	}
	if wav.Size != int64(len("RIFFfakewav")) {
		t.Errorf("size 不对: %d", wav.Size)
	}
	if wav.Source != ArtifactSourceScan {
		t.Errorf("来源应为 scan: %q", wav.Source)
	}
	if findRef(refs, ArtifactRootData, "media/cover.png") == nil {
		t.Fatalf("dataDir/media 下的图必须被采到: %+v", refs)
	}
	if len(refs) != 2 {
		t.Fatalf("应恰好两个产物, got %d: %+v", len(refs), refs)
	}
}

// TestArtifactAttributionIsolatesTeams 归属隔离: 同一 cwd 下两个团队, 各自清单只含
// 自己时间窗内的文件; cwd 里团队开始前就存在的文件必须被排除。
func TestArtifactAttributionIsolatesTeams(t *testing.T) {
	cwd := t.TempDir()
	base := time.Now().Add(-10 * time.Hour)
	aStart, aEnd := base, base.Add(time.Hour)
	bStart, bEnd := base.Add(2*time.Hour), base.Add(3*time.Hour)

	teamA := newArtifactTeam("A", filepath.Join(t.TempDir(), "teams", "A"), cwd, aStart, aEnd)
	teamB := newArtifactTeam("B", filepath.Join(t.TempDir(), "teams", "B"), cwd, bStart, bEnd)

	writeFileAt(t, filepath.Join(cwd, "preexisting.txt"), "老文件", base.Add(-2*time.Hour))
	writeFileAt(t, filepath.Join(cwd, "a-out", "a.txt"), "A 的产物", aStart.Add(10*time.Minute))
	writeFileAt(t, filepath.Join(cwd, "b-out", "b.txt"), "B 的产物", bStart.Add(10*time.Minute))

	refsA, err := CollectArtifacts(teamA)
	if err != nil {
		t.Fatalf("A 采集失败: %v", err)
	}
	if findRef(refsA, ArtifactRootWork, "a-out/a.txt") == nil {
		t.Fatalf("A 应采到自己的产物: %+v", refsA)
	}
	if findRef(refsA, ArtifactRootWork, "b-out/b.txt") != nil {
		t.Fatalf("A 的清单不得含 B 的文件: %+v", refsA)
	}
	if findRef(refsA, ArtifactRootWork, "preexisting.txt") != nil {
		t.Fatalf("团队开始前就存在的文件必须排除: %+v", refsA)
	}

	refsB, err := CollectArtifacts(teamB)
	if err != nil {
		t.Fatalf("B 采集失败: %v", err)
	}
	if findRef(refsB, ArtifactRootWork, "b-out/b.txt") == nil {
		t.Fatalf("B 应采到自己的产物: %+v", refsB)
	}
	if findRef(refsB, ArtifactRootWork, "a-out/a.txt") != nil {
		t.Fatalf("B 的清单不得含 A 的文件: %+v", refsB)
	}
}

// TestArtifactSkipDirsAndMaterializedRescue 被剪目录里的文件默认不算产物, 但一旦
// 经 MaterializeCode 记账 (硬归属证据) 就必须补回, 且 mtime 早于 StartedAt 也不丢。
func TestArtifactSkipDirsAndMaterializedRescue(t *testing.T) {
	cwd := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "teams", "ms")
	started := time.Now().Add(-time.Hour)
	team := newArtifactTeam("ms", dataDir, cwd, started, time.Time{})

	writeFileAt(t, filepath.Join(cwd, "node_modules", "dep", "index.js"), "noise", time.Time{})
	writeFileAt(t, filepath.Join(cwd, ".git", "objects", "ab", "cd"), "noise", time.Time{})
	// 物化到被剪目录里 + mtime 比 StartedAt 还早 (外部工具改过时间): 两道过滤都得绕过
	writeFileAt(t, filepath.Join(cwd, "node_modules", "pkg", "gen.go"), "package pkg", started.Add(-time.Hour))
	writeFileAt(t, filepath.Join(cwd, "src", "app.go"), "package app", time.Time{})

	refs, err := CollectArtifacts(team)
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if findRef(refs, ArtifactRootWork, "node_modules/dep/index.js") != nil {
		t.Fatalf("node_modules 噪声不应进清单: %+v", refs)
	}
	if len(relsIn(refs, ArtifactRootWork)) != 1 || findRef(refs, ArtifactRootWork, "src/app.go") == nil {
		t.Fatalf("默认只应采到 src/app.go: %+v", refs)
	}

	team.recordMaterialized([]string{"node_modules/pkg/gen.go", "src/app.go", "src/app.go", ""})
	if got := team.MaterializedFiles(); len(got) != 2 {
		t.Fatalf("物化清单应去重后 2 条, got %v", got)
	}
	refs, err = CollectArtifacts(team)
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	rescued := findRef(refs, ArtifactRootWork, "node_modules/pkg/gen.go")
	if rescued == nil {
		t.Fatalf("物化记账过的文件必须补回清单: %+v", refs)
	}
	if rescued.Source != ArtifactSourceMaterialize {
		t.Errorf("补回的文件来源应为 materialize: %q", rescued.Source)
	}
	if app := findRef(refs, ArtifactRootWork, "src/app.go"); app == nil || app.Source != ArtifactSourceMaterialize {
		t.Errorf("既被扫到又被物化的文件应标记为 materialize (硬证据): %+v", app)
	}
	// 删除后不得再出现在清单里 (清单不能说谎)
	if err := os.Remove(filepath.Join(cwd, "node_modules", "pkg", "gen.go")); err != nil {
		t.Fatal(err)
	}
	refs, _ = CollectArtifacts(team)
	if findRef(refs, ArtifactRootWork, "node_modules/pkg/gen.go") != nil {
		t.Fatalf("已删除的物化文件不应在清单里: %+v", refs)
	}
}

// TestRecordWBSMaterializationKeepsPaths 物化路径必须被累积 (原实现只记数量指标就丢了
// 路径), 且在没有指标采集器 (metrics=nil) 时同样要记。
func TestRecordWBSMaterializationKeepsPaths(t *testing.T) {
	team := newArtifactTeam("wbs", t.TempDir(), t.TempDir(), time.Now().Add(-time.Minute), time.Time{})
	if team.metrics() != nil {
		t.Fatal("本用例前提: team 无指标采集器")
	}
	o := &Orchestrator{}
	o.recordWBSMaterialization(team, &TaskNode{V2TaskID: "t1", Title: "任务", Role: "coder"},
		[]string{"pkg/a.go", "pkg/b.go"}, team.Cwd)
	got := team.MaterializedFiles()
	if len(got) != 2 || got[0] != "pkg/a.go" || got[1] != "pkg/b.go" {
		t.Fatalf("物化路径未被累积: %v", got)
	}
}

// TestArtifactManifestScannedVsUnscanned fail-open 语义: "扫过且真空"必须与
// "未扫/出错"可区分 —— 这正是下游把采集失败当成"无产物"的误判根因。
func TestArtifactManifestScannedVsUnscanned(t *testing.T) {
	cwd := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "teams", "empty")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Hour)
	// cwd 里只有团队开始前的老文件 → 扫过, 但确实零产物
	writeFileAt(t, filepath.Join(cwd, "old.txt"), "老", started.Add(-time.Hour))

	team := newArtifactTeam("empty", dataDir, cwd, started, time.Time{})
	m := BuildArtifactManifest(team)
	if !m.Scanned || !m.Empty || m.Error != "" || len(m.Files) != 0 {
		t.Fatalf("应为 扫过且真空: scanned=%v empty=%v err=%q files=%d", m.Scanned, m.Empty, m.Error, len(m.Files))
	}
	for _, r := range m.Roots {
		if !r.Scanned {
			t.Fatalf("根 %s 应已扫过: %+v", r.Kind, r)
		}
	}
	if refs, err := CollectArtifacts(team); err != nil || len(refs) != 0 {
		t.Fatalf("扫过且真空时 error 必须为 nil: refs=%v err=%v", refs, err)
	}

	// 无 StartedAt/CreatedAt → 共享根没有归属判据 = "未扫", 不能装成"没有产物"
	blind := newArtifactTeam("blind", dataDir, cwd, time.Time{}, time.Time{})
	mb := BuildArtifactManifest(blind)
	if mb.Scanned || mb.Empty || mb.Error == "" {
		t.Fatalf("缺归属基线应标记未扫: scanned=%v empty=%v err=%q", mb.Scanned, mb.Empty, mb.Error)
	}
	var workStatus *ArtifactRootStatus
	for i := range mb.Roots {
		if mb.Roots[i].Kind == ArtifactRootWork {
			workStatus = &mb.Roots[i]
		}
	}
	if workStatus == nil || workStatus.Scanned || workStatus.Error == "" {
		t.Fatalf("work 根应带未扫原因: %+v", workStatus)
	}
	if _, err := CollectArtifacts(blind); err == nil {
		t.Fatal("未扫成时 CollectArtifacts 必须返回 error (否则调用方会把空清单当成无产物)")
	}

	// 根不存在 ≠ 未扫: 平台从没往那儿写过东西, 是确定性的空
	missing := newArtifactTeam("missing", filepath.Join(t.TempDir(), "never"), cwd, started, time.Time{})
	mm := BuildArtifactManifest(missing)
	if !mm.Scanned || !mm.Empty || mm.Error != "" {
		t.Fatalf("根不存在应算确定性空: scanned=%v empty=%v err=%q", mm.Scanned, mm.Empty, mm.Error)
	}
}

// TestSaveTeamReportWritesArtifactManifest 真实链路: saveTeamReport 落 REPORT.md 时
// 必须同时生成 ARTIFACTS.json, 且字段完整、两个落点都在里面。
func TestSaveTeamReportWritesArtifactManifest(t *testing.T) {
	tmp := t.TempDir()
	cwd := t.TempDir()
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(tmp, "teams"),
		Cwd:     cwd,
		// Factory 不会被调用 (本用例只走报告/清单落盘, 不跑工作流)
		Factory: func(_ context.Context, _, _ string) (AgentRunner, error) { return nil, nil },
		Notify:  func(_, _ string) {},
	})
	team, err := ptm.CreateTeam("mfr", "techblog", "写一篇技术博客", "test")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	team.StartedAt = time.Now().Add(-time.Minute)
	team.FinishedAt = time.Now()
	// 产物: 一个在 cwd (agent 写的), 一个在 dataDir/media (平台写的)
	writeFileAt(t, filepath.Join(cwd, "out", "deliverable.md"), "# 成品", time.Time{})
	writeFileAt(t, filepath.Join(team.dataDir, "media", "chart.png"), "PNG", time.Time{})
	team.recordMaterialized([]string{"out/deliverable.md"})

	reportPath := ptm.saveTeamReport(team, []StageResult{{Name: "写作", Role: "writer", Output: "正文"}})
	if reportPath == "" {
		t.Fatal("REPORT.md 未生成")
	}
	data, err := os.ReadFile(filepath.Join(team.dataDir, ArtifactManifestFile))
	if err != nil {
		t.Fatalf("ARTIFACTS.json 应与报告同期生成: %v", err)
	}
	var m ArtifactManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("清单不是合法 JSON: %v", err)
	}
	if m.Team != "mfr" || m.ScannedAt.IsZero() || m.StartedAt.IsZero() || len(m.Roots) != 2 {
		t.Fatalf("清单字段不完整: %+v", m)
	}
	if !m.Scanned || m.Empty {
		t.Fatalf("应为扫过且有产物: scanned=%v empty=%v", m.Scanned, m.Empty)
	}
	if findRef(m.Files, ArtifactRootWork, "out/deliverable.md") == nil {
		t.Fatalf("cwd 侧成品应在清单里: %+v", m.Files)
	}
	if findRef(m.Files, ArtifactRootData, "media/chart.png") == nil {
		t.Fatalf("dataDir/media 产物应在清单里: %+v", m.Files)
	}
	// REPORT.md 自身也应在清单里 (清单在报告之后生成)
	if findRef(m.Files, ArtifactRootData, "REPORT.md") == nil {
		t.Fatalf("REPORT.md 自身应在清单里: %+v", m.Files)
	}

	// 读回接口: 有清单 → 返回内容; 没清单 → (nil, nil), 与"清单说零产物"可区分
	got, err := ReadArtifactManifest(team.dataDir)
	if err != nil || got == nil || got.Team != "mfr" {
		t.Fatalf("ReadArtifactManifest: %+v err=%v", got, err)
	}
	none, err := ReadArtifactManifest(t.TempDir())
	if err != nil || none != nil {
		t.Fatalf("无清单文件应返回 (nil,nil), got %+v err=%v", none, err)
	}
}
