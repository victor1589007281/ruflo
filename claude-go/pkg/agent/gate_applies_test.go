// gate_applies_test.go — H0-3 验证闸门放行的验收测试。
//
// 背景: 210 个 trace 里 gate span 只有 19 条, 全部是 gate.content;
// gate.compile / gate.test 为 0 —— 因为判据只看工作流的 ProducesCode 声明,
// 而当前运行的团队没有一个命中, 两个门禁在真实运行里是死代码。
//
// 放宽为"声明产码 **或** 本次 run 真的写了 Go 文件"。放宽必须带守卫:
//   - 无 go.mod ⇒ 不跑 (否则 "no main module" 误判, 触发无意义修复轮)
//   - 没真的写 .go ⇒ 不跑 (否则写作类工作流在 Go 仓库里被误抓)
//
// 这两条守卫都有事故来源, 见 gateApplies 的注释。下面的测试一半是在证**不**该跑。
package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFileMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func newGateTeam(cwd, workflow string, started time.Time) *ProductionTeam {
	return &ProductionTeam{Cwd: cwd, Workflow: workflow, StartedAt: started}
}

// 守卫 1: 空 Cwd 永不跑门禁 (原本如此, 防止在无仓库上下文里跑 go build)。
func TestH03_EmptyCwdNeverRunsGate(t *testing.T) {
	ptm := &ProductionTeamManager{}
	if ptm.gateApplies(newGateTeam("", "prose-thing", time.Now())) {
		t.Fatal("Cwd 为空却判定要跑门禁")
	}
}

// 守卫 2: 声明产码的工作流照常跑 (回归: 放宽不得把原有能力改坏)。
func TestH03_DeclaredProducerStillGated(t *testing.T) {
	dir := t.TempDir() // 刻意不建 go.mod: 声明产码时不该被守卫挡住
	ptm := &ProductionTeamManager{}
	if !ptm.gateApplies(newGateTeam(dir, "development", time.Now())) {
		t.Fatal("development 是声明产码工作流, 却被判定不跑门禁 —— 原有能力被改坏")
	}
}

// 守卫 3 (事故防线): 未声明产码 + 无 go.mod ⇒ 不跑门禁。
//
// 这条守的是 workflowProducesCode 注释里记的那起事故:
// "no main module" 被当成编译失败, 再触发 coder 修复轮, 单团队烧掉数百万 token。
func TestH03_NoGoModuleIsNotGated(t *testing.T) {
	dir := t.TempDir()
	writeFileMtime(t, filepath.Join(dir, "notes.go"), time.Now().Add(time.Hour))
	ptm := &ProductionTeamManager{}
	// "techblog" 是写作类工作流, 不在产码白名单里。
	if ptm.gateApplies(newGateTeam(dir, "techblog", time.Now())) {
		t.Fatal("无 go.mod 的目录仍被判要跑门禁 —— 会重演 'no main module' 烧 token 事故")
	}
}

// 守卫 4: 未声明产码 + 有 go.mod + 本次没写 .go ⇒ 不跑门禁。
//
// 写作/调研类工作流可能恰好跑在一个 Go 仓库里; 它们没产出代码, 不该被拉去编译。
func TestH03_NoWriteIsNotGated(t *testing.T) {
	dir := t.TempDir()
	started := time.Now()
	writeFileMtime(t, filepath.Join(dir, "go.mod"), started.Add(-time.Hour))
	// 一个很旧的 .go 文件: 团队开始**之前**就存在, 不算本次产出。
	writeFileMtime(t, filepath.Join(dir, "old.go"), started.Add(-time.Hour))

	ptm := &ProductionTeamManager{}
	if ptm.gateApplies(newGateTeam(dir, "research", started)) {
		t.Fatal("本次 run 没写任何 .go 文件, 却被判要跑门禁")
	}
}

// 判据 1 (H0-3 主判据): 未声明产码, 但本次 run 真的写了 .go ⇒ **必须**跑门禁。
//
// 这是本次放宽要收住的那类 run —— 它在改动前完全不设防。
func TestH03_WrittenGoFileUnlocksGate(t *testing.T) {
	dir := t.TempDir()
	started := time.Now()
	writeFileMtime(t, filepath.Join(dir, "go.mod"), started.Add(-time.Hour))
	// 团队开始**之后**写下的 .go 文件 = 本次 run 的产出。
	writeFileMtime(t, filepath.Join(dir, "pkg", "new.go"), started.Add(time.Minute))

	ptm := &ProductionTeamManager{}
	if !ptm.gateApplies(newGateTeam(dir, "research", started)) {
		t.Fatal("本次 run 写了 .go 文件却仍不跑门禁 —— 编译/测试无人担保 (实测该形态 gate span = 0)")
	}
}

// 判据 2: 开始时间未知 (零值) 时按"未写入"处理 —— 宁可漏跑, 不可误跑。
//
// 理由: 无法区分"本次写的"与"历史遗留的", 此时判要跑门禁会把整个存量仓库
// 当成本次产出, 在大仓库上代价极高。fail-safe 取向与其它守卫一致。
func TestH03_ZeroStartTimeIsNotGated(t *testing.T) {
	dir := t.TempDir()
	writeFileMtime(t, filepath.Join(dir, "go.mod"), time.Now())
	writeFileMtime(t, filepath.Join(dir, "x.go"), time.Now().Add(time.Hour))

	ptm := &ProductionTeamManager{}
	if ptm.gateApplies(newGateTeam(dir, "research", time.Time{})) {
		t.Fatal("StartedAt 为零值时仍判要跑门禁 —— 无法区分本次产出与存量代码")
	}
}

// 守卫 5: vendor / .git / testdata 下的 .go 不算本次产出。
func TestH03_VendorGoFilesDoNotUnlockGate(t *testing.T) {
	dir := t.TempDir()
	started := time.Now()
	writeFileMtime(t, filepath.Join(dir, "go.mod"), started.Add(-time.Hour))
	for _, sub := range []string{"vendor", ".git", "testdata"} {
		writeFileMtime(t, filepath.Join(dir, sub, "dep.go"), started.Add(time.Minute))
	}

	ptm := &ProductionTeamManager{}
	if ptm.gateApplies(newGateTeam(dir, "research", started)) {
		t.Fatal("仅 vendor/.git/testdata 下有 .go 变动就触发门禁 —— 那些不是本 run 的产出")
	}
}
