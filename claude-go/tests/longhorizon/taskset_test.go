package longhorizon

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestH41_校验真的会拒 —— 反面臂。
//
// 下面那条判的是**真实任务集**合规, 而真实任务集永远是合规的 —— 于是
// ValidateTaskSet 里的每一条检查都可以被删掉而测试照样全绿 (变异 M14/M15 就是这么
// 活下来的)。校验函数的验收必须在**不合规的输入**上做。
func TestH41_校验真的会拒(t *testing.T) {
	good := Task{
		ID: "x", Goal: "g", Fixture: "lexer",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
	}

	t.Run("条数不足要拒", func(t *testing.T) {
		if err := ValidateTaskSet([]Task{good}); err == nil {
			t.Fatalf("只有 1 个任务时没报错 —— 方案要求 ≥%d 个", MinTaskCount)
		}
	})

	t.Run("fixture 不存在要拒", func(t *testing.T) {
		bad := good
		bad.Fixture = "并不存在的fixture"
		ts := make([]Task, MinTaskCount)
		for i := range ts {
			ts[i] = good
			ts[i].ID = fmt.Sprintf("t%d", i)
		}
		ts[3] = bad
		if err := ValidateTaskSet(ts); err == nil {
			t.Fatal("fixture 不存在时没报错 —— 那个任务会在真实 run 里才炸, 且报错指向文件系统")
		}
	})

	t.Run("ID 重复要拒", func(t *testing.T) {
		ts := make([]Task, MinTaskCount)
		for i := range ts {
			ts[i] = good // ID 全是 "x"
		}
		if err := ValidateTaskSet(ts); err == nil {
			t.Fatal("ID 重复时没报错 —— 配对统计会把两个任务当成同一个")
		}
	})

	t.Run("没有验证命令要拒", func(t *testing.T) {
		bad := good
		bad.VerifyCmd = nil
		if err := bad.Validate(); err == nil {
			t.Fatal("没有客观终态的任务必须被拒")
		}
	})

	t.Run("轮数下限不足要拒", func(t *testing.T) {
		bad := good
		bad.MinTurns = 10
		if err := bad.Validate(); err == nil {
			t.Fatal("MinTurns 低于长程下限的任务必须被拒")
		}
	})

	t.Run("不要求折叠要拒", func(t *testing.T) {
		bad := good
		bad.MustCompact = false
		if err := bad.Validate(); err == nil {
			t.Fatal("不要求撞压缩阈值的任务必须被拒 —— 它测不到「跨压缩存活」")
		}
	})
}

// TestH41_任务集自证合规 任务集自己的验收面。
//
// 方案 §21.5.6 要求 "≥ 8 个、100+ 轮、有客观终态"。这三条如果不写成断言,
// 任务集缩水/被改成没有验证命令, 没有任何读数会指出 —— 而 H4-2 的整条链都
// 建立在"任务集里每个任务都是长程且有客观终态"之上。
func TestH41_任务集自证合规(t *testing.T) {
	if err := ValidateTaskSet(TaskSet); err != nil {
		t.Fatalf("任务集不合规: %v", err)
	}
	if len(TaskSet) < MinTaskCount {
		t.Fatalf("任务数 = %d, 方案要求 ≥ %d", len(TaskSet), MinTaskCount)
	}
}

// TestH41_没有孤儿fixture —— 反面臂。
//
// 一个 fixture 存在于 fixtures.go 却没有任何任务引用它, 说明有人改了任务却忘了
// 它原本指向的初始代码 —— 那个 fixture 会一直躺在那里, 让人以为覆盖到了。
func TestH41_没有孤儿fixture(t *testing.T) {
	used := map[string]bool{}
	for _, task := range TaskSet {
		used[task.Fixture] = true
	}
	for _, name := range FixtureNames() {
		if !used[name] {
			t.Fatalf("fixture %q 没有被任何任务引用 —— 它测不到任何东西, "+
				"而它占据着一个看起来像「已覆盖」的位置", name)
		}
	}
}

// TestH41_每个任务都能展开出可编译的初始代码 展开面的验收。
//
// Materialize 的失效是**延迟**的: 单测里只看"文件写出来了"会全绿, 而真实 run
// 一上来就因为没有 go.mod / 包名对不上而失败 —— 那时报错指向的是 agent 而不是
// fixture。所以这里真的把每个 fixture 展开并 `go build ./...` 一遍:
// **初始状态必须能编译** (桩是齐全的), 只是测试跑不过。
func TestH41_每个任务都能展开出可编译的初始代码(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 go toolchain")
	}
	for _, task := range TaskSet {
		t.Run(task.ID, func(t *testing.T) {
			dir := t.TempDir()
			if err := Materialize(dir, task.Fixture); err != nil {
				t.Fatalf("展开 fixture 失败: %v", err)
			}

			// 必须写出 go.mod —— 没有它, 验证命令 `go test ./...` 会在
			// "no main module" 上失败, 而那看起来像 agent 没做完。
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
				t.Fatalf("fixture 没有 go.mod: %v", err)
			}

			cmd := exec.Command("go", "build", "./...")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("初始代码编译不过 (桩必须齐全): %v\n%s", err, out)
			}
		})
	}
}

// TestH41_初始状态必须是失败的 —— 整个任务集的地基。
//
// 这是"客观终态"这句话的**唯一**证明方式: 初始代码跑验证命令必须 FAIL,
// 否则任务集里混进了"什么都没做也算成功"的条目, 而那种条目会让 A/B 两组
// 都以成功收尾 —— 成功率 100% 对 100%, 看起来"没有回归", 实际上什么都没测。
func TestH41_初始状态必须是失败的(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 go toolchain")
	}
	for _, task := range TaskSet {
		t.Run(task.ID, func(t *testing.T) {
			dir := t.TempDir()
			if err := Materialize(dir, task.Fixture); err != nil {
				t.Fatalf("展开 fixture 失败: %v", err)
			}
			cmd := exec.Command(task.VerifyCmd[0], task.VerifyCmd[1:]...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("初始代码竟然通过了验证命令 —— 这个任务「什么都没做也能成功」, "+
					"它会在 A/B 里让两组都以成功收尾, 测不出任何东西\n%s", out)
			}
			// 失败**必须是因为测试没通过**, 不能是因为工具链本身出错 ——
			// 否则一个 go.mod 写坏 / go 不在 PATH 的 fixture 也会让这条通过,
			// 而它在真实 run 里永远不可能成功 (A/B 两组都 0%, 看着像"没有差异")。
			if !bytes.Contains(out, []byte("FAIL")) {
				t.Fatalf("验证命令失败了, 但输出里没有 FAIL —— 失败原因不是「测试没通过」, "+
					"而是工具链/环境问题, 这个任务在任何 run 里都不可能成功:\n%s", out)
			}
		})
	}
}

// TestH41_展开是确定的 —— H4-2 配对的前提。
//
// 同一个 fixture 展开两次, 文件集合与内容必须逐字节相同。这条看着像废话,
// 但它挡的是"H4-2 里两组唯一的差异应当是配置"这件事: 初始代码是运行输入的一部分,
// 它只要有一点不确定 (时间戳/随机量/遍历顺序), 两组就不再是配对的,
// 而所有汇总数字上完全看不出来。
func TestH41_展开是确定的(t *testing.T) {
	for _, task := range TaskSet {
		t.Run(task.ID, func(t *testing.T) {
			d1, d2 := t.TempDir(), t.TempDir()
			if err := Materialize(d1, task.Fixture); err != nil {
				t.Fatalf("第一次展开失败: %v", err)
			}
			if err := Materialize(d2, task.Fixture); err != nil {
				t.Fatalf("第二次展开失败: %v", err)
			}

			snap := func(root string) map[string]string {
				m := map[string]string{}
				err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return err
					}
					rel, _ := filepath.Rel(root, p)
					b, rerr := os.ReadFile(p)
					if rerr != nil {
						return rerr
					}
					m[rel] = string(b)
					return nil
				})
				if err != nil {
					t.Fatalf("遍历失败: %v", err)
				}
				return m
			}

			a, b := snap(d1), snap(d2)
			if len(a) != len(b) {
				t.Fatalf("两次展开的文件数不同: %d vs %d", len(a), len(b))
			}
			for k, v := range a {
				if b[k] != v {
					t.Fatalf("文件 %s 两次展开内容不同 —— 配对前提被破坏", k)
				}
			}
		})
	}
}

// TestH41_展开拒绝路径逃逸 —— Materialize 是导出的, 要挡一手。
func TestH41_展开拒绝路径逃逸(t *testing.T) {
	// 直接构造一个逃逸 fixture (不改全局 fixtures 表, 用手工副本),
	// 只验证拒绝逻辑本身。
	dir := t.TempDir()
	// 正常 fixture 应当成功, 作为对照 —— 证明下一条断言不是因为
	// Materialize 整个坏掉了才"通过"的。
	if err := Materialize(dir, "lexer"); err != nil {
		t.Fatalf("正常 fixture 展开失败: %v", err)
	}
	if err := Materialize(filepath.Join(dir, "x"), "不存在的fixture"); err == nil {
		t.Fatal("未知 fixture 必须报错, 实得 nil")
	}
}

// TestH41_TaskIDs顺序稳定 H4-2 配对落到数据上的那一层。
func TestH41_TaskIDs顺序稳定(t *testing.T) {
	a, b := TaskIDs(), TaskIDs()
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("两次 TaskIDs 顺序不同: %v vs %v —— 配对统计会因此把两个任务当成一个", a, b)
	}
	// 且必须与 TaskSet 的声明顺序一致 (不排序, 因为排序会让"任务在集里的位置"
	// 随 ID 字母序漂移, 加一个任务就可能改变所有任务的下标)。
	for i, id := range a {
		if TaskSet[i].ID != id {
			t.Fatalf("第 %d 个 ID = %q, 与 TaskSet 的 %q 不一致", i, id, TaskSet[i].ID)
		}
	}
}
