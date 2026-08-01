package agent

// team_workspace_test.go —— 团队独占工作区（design/02 §1.2「团队 cwd 仍进程级共享」）。
//
// 三类断言，各对着一种会让这个改动变成负资产的失效方式：
//   ① 默认必须**逐字节不变**——默认打开会在不通知下游的情况下改产物落点;
//   ② 打开后两个团队必须**真的不同目录**——只改了字段没建目录等于没隔离;
//   ③ 团队名来自用户输入，**路径穿越必须挡住**——这一条是安全属性而非便利功能。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeam工作区_默认关时与改造前逐字节一致(t *testing.T) {
	t.Setenv("CLAUDE_GO_TEAM_WORKSPACE", "")
	cwd := t.TempDir()
	if got := teamWorkspaceDir(cwd, "alpha"); got != cwd {
		t.Fatalf("默认应原样返回进程 cwd, got %q want %q", got, cwd)
	}
	// 关的时候连目录都不该建 —— 建了就说明这条路径有副作用, 将来"关着也出问题"最难查。
	if entries, err := os.ReadDir(cwd); err == nil && len(entries) != 0 {
		t.Fatalf("默认关时不该在 cwd 下建任何东西, 实际: %v", entries)
	}
}

func TestTeam工作区_开启后两团队互不共享(t *testing.T) {
	t.Setenv("CLAUDE_GO_TEAM_WORKSPACE", "1")
	cwd := t.TempDir()
	a := teamWorkspaceDir(cwd, "alpha")
	b := teamWorkspaceDir(cwd, "beta")
	if a == b {
		t.Fatalf("两个团队拿到同一个目录 (%q) —— 并发产码仍会互相覆盖", a)
	}
	if a == cwd || b == cwd {
		t.Fatalf("开启后不应再返回进程 cwd: a=%q b=%q cwd=%q", a, b, cwd)
	}
	// 目录必须**真建出来**: 只返回路径不建目录时, 下游 os.WriteFile 会因父目录不存在而失败,
	// 表现是"开了隔离就产不出文件"。
	for _, d := range []string{a, b} {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			t.Fatalf("工作目录未建出来: %q err=%v", d, err)
		}
	}
	// 同一团队多次取必须**稳定**同一个目录 —— 精修/重试要接力同一份代码。
	if again := teamWorkspaceDir(cwd, "alpha"); again != a {
		t.Fatalf("同一团队两次取到不同目录: %q vs %q（重试会从空目录开始）", again, a)
	}
}

func TestTeam工作区_团队名路径穿越被挡住(t *testing.T) {
	t.Setenv("CLAUDE_GO_TEAM_WORKSPACE", "1")
	base := t.TempDir()
	cwd := filepath.Join(base, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	// 团队名来自用户输入（飞书消息 / HTTP 载荷）。
	//
	// ⚠️ 变异反证记录: 单独摘掉 `safeTeamDirName` 那道字符净化，本测试**不会红** ——
	// 因为下面那道结构性 `filepath.Rel` 越界检查独立充分（防御纵深）。这不是测试无牙,
	// 是两道闸各自都够；但将来若有人以"冗余"为由删掉任一道, 剩下那道仍能挡住越界,
	// **而"单层目录"这个更弱的性质会失去保护**（`a/b` 这类名字会建出嵌套目录）。
	// 谁要动这两道闸, 先看这条注释。
	for _, evil := range []string{
		"../escape", "../../etc", "..", ".", "/abs", "a/b", "..\\win", "....//x",
	} {
		got := teamWorkspaceDir(cwd, evil)
		rel, err := filepath.Rel(cwd, got)
		if err != nil {
			t.Fatalf("团队名 %q 得到不可比较的路径 %q: %v", evil, got, err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("团队名 %q 逃出了 cwd: got %q (rel %q)", evil, got, rel)
		}
		if _, err := os.Stat(filepath.Join(base, "escape")); err == nil {
			t.Fatalf("团队名 %q 在 cwd 之外建出了目录", evil)
		}
	}
}
