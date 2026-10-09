package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestH11_TwoProjectsResolveToDifferentPaths 钉住 H1-1 的验收判据：
// "两个项目的任务文件路径不相等"。
//
// 否定式：断言两个路径**不相等**，而不是断言"路径被赋值了"。改造前两者都是
// os.TempDir()/claude-go-v2-tasks.json，逐字节相等 —— 这条会红。
func TestH11_TwoProjectsResolveToDifferentPaths(t *testing.T) {
	a := TaskStorePathFor("", filepath.Join(t.TempDir(), "proj-a"))
	b := TaskStorePathFor("", filepath.Join(t.TempDir(), "proj-b"))

	if a == b {
		t.Fatalf("两个项目解析出同一个任务文件路径 %q —— 这就是 H1-1 要消灭的共享形态", a)
	}
}

// TestH11_NotUnderTempDir 是上一条的**独立**判据。两条路径不相等仍可能双双落在 /tmp
// 下的两个随机名里；这条路直接把"落在共享临时目录"这件事本身否掉。
func TestH11_NotUnderTempDir(t *testing.T) {
	tmp := os.TempDir()
	for _, proj := range []string{"/proj-a", "/proj-b"} {
		got := TaskStorePathFor("", proj)
		if strings.HasPrefix(got, tmp+string(filepath.Separator)) {
			t.Fatalf("任务文件落在共享临时目录: %q (TempDir=%q)", got, tmp)
		}
	}
}

// TestH11_PathIsUnderProject 路径必须由项目目录派生，而不是别的地方。
func TestH11_PathIsUnderProject(t *testing.T) {
	proj := filepath.Join(t.TempDir(), "proj-a")
	got := TaskStorePathFor("", proj)
	if !strings.HasPrefix(got, proj+string(filepath.Separator)) {
		t.Fatalf("任务文件 %q 不在项目目录 %q 下", got, proj)
	}
}

// TestH11_ExplicitStateDirWins stateDir 显式给了就得听它的 —— 否则 k8s worker 里
// 注入的 CLAUDE_GO_STATE_DIR 会被 cwd 盖掉。
func TestH11_ExplicitStateDirWins(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	got := TaskStorePathFor(state, "/some/project")
	if !strings.HasPrefix(got, state+string(filepath.Separator)) {
		t.Fatalf("给了 stateDir=%q 却解析成 %q", state, got)
	}
}

// TestH11_TwoProjectsAreMutuallyInvisible 钉住 H1-1 的第二条验收：
// "双进程并发跑不同项目，互相不可见"。
//
// 这里用两个独立 TaskStore 实例模拟两个进程 —— 它们各自持有一份内存索引与一个路径，
// 互不共享任何状态（正是两个进程的样子）。**带对照臂**：A 必须看得见自己写的任务，
// 否则"B 看不见 A"会因为写入根本没成功而假绿。
func TestH11_TwoProjectsAreMutuallyInvisible(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	storeA := NewTaskStore(TaskStorePathFor("", dirA))
	storeB := NewTaskStore(TaskStorePathFor("", dirB))

	idA, err := storeA.AddTask("只属于项目 A 的任务", "", "")
	if err != nil {
		t.Fatalf("A 建任务失败: %v", err)
	}

	// 对照臂：A 自己必须看得见 —— 否则下面"B 看不见"是假的。
	if !hasTask(storeA, idA) {
		t.Fatalf("对照臂失败: A 看不见自己刚建的任务 %s；此时'B 看不见'不构成证据", idA)
	}

	// 被测臂：B 必须看不见。
	if hasTask(storeB, idA) {
		t.Fatalf("项目 B 看见了项目 A 的任务 %s —— 两个项目不是互相不可见的", idA)
	}
	for _, task := range storeB.GetAllTasks() {
		if task.ID == idA {
			t.Fatalf("项目 B 的列表里出现了项目 A 的任务 %s", idA)
		}
	}
}

// hasTask 报告台账里有没有这条 ID。
func hasTask(s *TaskStore, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byID[id]
	return ok
}

// TestH11_BPersistsIndependentlyOfA 两边的写入各自落盘，互不踩踏：
// B 写完之后，A 的文件内容必须逐字节不变。这是"共享单文件会互相覆盖"的正面反证。
func TestH11_BPersistsIndependentlyOfA(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	pathA := TaskStorePathFor("", dirA)
	storeA := NewTaskStore(pathA)
	if _, err := storeA.AddTask("A 的任务", "", ""); err != nil {
		t.Fatalf("A 建任务失败: %v", err)
	}
	before, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("读 A 的任务文件失败: %v", err)
	}

	storeB := NewTaskStore(TaskStorePathFor("", dirB))
	if _, err := storeB.AddTask("B 的任务", "", ""); err != nil {
		t.Fatalf("B 建任务失败: %v", err)
	}

	after, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("读 A 的任务文件失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("B 的写入改动了 A 的任务文件 —— 两者仍共享存储\nbefore=%s\nafter=%s", before, after)
	}
}
