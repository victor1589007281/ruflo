// solvable_test.go —— 任务集**可解性**的验收 (opt-in)。
//
// # 为什么这一条不可省
//
// TestH41_初始状态必须是失败的 证明了初始态 FAIL。但 FAIL 有两个来源: "规格没被
// 满足" (好) 与 "规格自相矛盾" (灾难)。后者在 A/B 台上的读数是**两组都 0%** ——
// 看起来像"这个改动没有效果", 而真相是这个任务谁都做不出来。
//
// 手工核过一遍不可靠: 规格里的位置下标、矩阵行列式、版本比较的边界, 都是写错一个
// 数字就恒假而不自知的东西。所以用**参考实现**把每个 fixture 真的跑到 PASS。
//
// # 为什么参考实现不放在仓库里
//
// 它就是这些任务的答案。放在仓库里, 任何在这台机器上跑的 agent 只要读一次
// tests/longhorizon/ 就能拿到答案, 整个场景台立刻失去意义。所以参考实现由
// 调用方通过环境变量指一个**仓库外**的目录, 本测试只负责 "把它贴进去、跑、断言过"。
//
// 运行:
//
//	LONGHORIZON_SOLUTION_DIR=/tmp/h41sol go test -run TestH41_规格可解 ./tests/longhorizon/
//
// 目录里每个任务一个文件, 命名 <任务ID>.go (如 lexer.go)。
package longhorizon

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const solutionDirEnv = "LONGHORIZON_SOLUTION_DIR"

// TestH41_规格可解 —— 每个 fixture 贴上参考实现后必须通过自己的验证命令。
func TestH41_规格可解(t *testing.T) {
	dir := os.Getenv(solutionDirEnv)
	if dir == "" {
		t.Skipf("未设置 %s —— 参考实现在仓库外, 需要时手动指一个目录", solutionDirEnv)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("%s=%s 不可读: %v", solutionDirEnv, dir, err)
	}

	for _, task := range TaskSet {
		t.Run(task.ID, func(t *testing.T) {
			src := filepath.Join(dir, task.ID+".go")
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("读不到参考实现 %s: %v\n(没有它就无法证明规格可解, "+
					"而不可解的规格在 A/B 上表现为「两组都 0%%」)", src, err)
			}

			work := t.TempDir()
			if err := Materialize(work, task.Fixture); err != nil {
				t.Fatalf("展开 fixture 失败: %v", err)
			}

			// 参考实现覆盖到 fixture 的实现文件上, 其余 (**尤其是 _test.go**)
			// 一个字节都不动 —— 一旦允许连测试一起换, 这条就退化成
			// "参考实现能让自己写的测试通过", 证明不了任何事。
			target := filepath.Join(work, task.Fixture, task.Fixture+".go")
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("fixture %q 里没有实现文件 %s —— 参考实现无处可贴",
					task.Fixture, filepath.Join(task.Fixture, task.Fixture+".go"))
			}
			if err := os.WriteFile(target, b, 0o644); err != nil {
				t.Fatalf("写入参考实现失败: %v", err)
			}

			cmd := exec.Command(task.VerifyCmd[0], task.VerifyCmd[1:]...)
			cmd.Dir = work
			cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("粘上参考实现后仍然不过 —— **规格本身有问题**, "+
					"这个任务在任何 run 里都不可能成功 (A/B 两组都会是 0%%, "+
					"看起来像「没有差异」):\n%s", out)
			}
			if bytes.Contains(out, []byte("no test files")) {
				t.Fatalf("验证命令没有跑到任何测试 —— fixture %q 的 _test.go 丢了, "+
					"退出码 0 是假的", task.Fixture)
			}
		})
	}
}
