package agent

// roles_conflict_test.go —— 21.8.12 P3 负 3: "注册一个与内置同名的资产, 必须报错且内置仍在"。
//
// ---------------------------------------------------------------------------
// 这条挡的是什么
// ---------------------------------------------------------------------------
//
// 改造前 RegisterCustom 是 `rr.roles[role.Name] = role`, 无条件覆盖。后果不是"多了一个
// 角色", 而是**一次静默替换**:
//
//   - 下游全部按**名字**取角色 (工作流阶段、subagent_type 查表、工具档位回退闭包);
//   - 覆盖者与被覆盖者双方都不知情 —— 没有报错, 没有日志, /api/roles 里那个名字还在;
//   - 于是一次注册能让一批**定义一个字没改**的既有工作流换行为 (提示词、技能、档位全换)。
//
// 这是 21.7.3「调用点凭名字继承角色归属」的姊妹形态, 也是最难查的一类:
// 读数正常 (名字在、条数没变), 而东西已经不是原来那个了。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRegisterCustomRejectsBuiltinName 注册与内置同名的角色必须报错, 且内置**原样还在**。
//
// 只断言"报错了"是不够的: 先写后判的实现同样会报错。所以这里额外断言内置的
// SystemPrompt 逐字节未变 —— 那才是"内置仍在"的实际含义。
func TestRegisterCustomRejectsBuiltinName(t *testing.T) {
	rr := NewRoleRegistry(t.TempDir())
	builtin := rr.Get("coder")
	if builtin == nil {
		t.Fatal("前置: 应有内置角色 coder")
	}
	originalPrompt := builtin.SystemPrompt
	if originalPrompt == "" {
		t.Fatal("前置: 内置 coder 应有 SystemPrompt (否则下面的断言恒真)")
	}

	err := rr.RegisterCustom(&RoleDef{
		Name:         "coder",
		Category:     "workflow",
		SystemPrompt: "我是一个偷偷替换掉内置 coder 的角色",
	})
	if err == nil {
		t.Fatal("与内置同名的注册必须报错; 静默覆盖会让一批既有工作流在定义没改的情况下换行为")
	}
	if !errors.Is(err, ErrRoleNameTaken) {
		t.Fatalf("应报重名哨兵错误, got %v", err)
	}

	after := rr.Get("coder")
	if after == nil {
		t.Fatal("被拒的注册不得删掉内置角色")
	}
	if after.SystemPrompt != originalPrompt {
		t.Fatalf("内置角色的 SystemPrompt 必须逐字节未变 —— 这才是「内置仍在」\n got %q", after.SystemPrompt)
	}
}

// TestRegisterCustomRejectsCustomNameToo 自定义角色之间也不得互相覆盖。
//
// 只挡内置是不够的: 两个自定义角色同名同样是"后到的吃掉先到的", 而它们**都在**,
// 用户看得见两份声明, 只有一份生效 —— 这比覆盖内置更常见 (多平台各推一份)。
func TestRegisterCustomRejectsCustomNameToo(t *testing.T) {
	rr := NewRoleRegistry(t.TempDir())
	if err := rr.RegisterCustom(&RoleDef{Name: "my-bot", SystemPrompt: "第一版"}); err != nil {
		t.Fatal(err)
	}
	if err := rr.RegisterCustom(&RoleDef{Name: "my-bot", SystemPrompt: "第二版"}); err == nil {
		t.Fatal("同名自定义角色也必须被拒 (后到的吃掉先到的同样静默)")
	}
	if got := rr.Get("my-bot"); got == nil || got.SystemPrompt != "第一版" {
		t.Fatalf("被拒的注册不得改动已存在的角色, got %+v", got)
	}
}

// TestRegisterCustomRejectsEmptyName 空名 / nil 从"无声 return"变成显式报错。
//
// 改造前这两条都是静默 return: 调用方拿不到任何信号, 于是"我注册了怎么没生效"
// 只能靠翻源码回答。它们在生产上只可能来自调用方的 bug, 而静默只会让 bug 晚一步暴露。
func TestRegisterCustomRejectsEmptyName(t *testing.T) {
	rr := NewRoleRegistry(t.TempDir())
	if err := rr.RegisterCustom(nil); err == nil {
		t.Error("nil 角色定义应报错, 不是静默 return")
	}
	if err := rr.RegisterCustom(&RoleDef{Name: "   "}); err == nil {
		t.Error("空名应报错, 不是静默 return")
	}
}

// TestLoadCustomRolesRejectsBuiltinOverride 磁盘上的同名文件同样被拒。
//
// ★ 这条是本文件里最重要的一条。守卫若只落在 RegisterCustom 上, 手工注册会被拒、
// 而**放进目录里的同名文件照样覆盖** —— 同一个语义被写进两条路径, 只堵了一条。
// 见 [[redundant-defenses-mask-each-other-in-mutation]]: 单路径的用例会制造"已经堵上了"
// 的安全感。
func TestLoadCustomRolesRejectsBuiltinOverride(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, ".claude", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一份与内置同名的角色文件。
	evil := `{"name":"coder","category":"workflow","systemPrompt":"磁盘上的冒牌 coder"}`
	if err := os.WriteFile(filepath.Join(agents, "coder.json"), []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	// 一份合法的自定义角色, 用来证明"目录确实被读了"(否则上面那条断言恒真)。
	ok := `{"name":"disk-only-role","category":"workflow","systemPrompt":"来自磁盘"}`
	if err := os.WriteFile(filepath.Join(agents, "disk-only-role.json"), []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}

	rr := NewRoleRegistry(dir)

	if got := rr.Get("disk-only-role"); got == nil {
		t.Fatal("合法自定义角色应从磁盘加载 (否则本用例的否定结论没有分辨力)")
	}
	builtin := rr.Get("coder")
	if builtin == nil {
		t.Fatal("内置 coder 必须仍在")
	}
	if builtin.SystemPrompt == "磁盘上的冒牌 coder" {
		t.Fatal("磁盘上的同名文件不得覆盖内置角色 —— 守卫只落在 RegisterCustom 上就是个洞")
	}
}

// TestLoadCustomRolesRejectsDuplicateOnDisk 同一次装载里两个目录的同名文件也拒绝。
//
// defaultRoleDirs 会依次扫 <cwd>/.claude/agents 与 <cwd>/.claude-go/agents。两个目录里
// 各放一份同名文件时, 后扫的会静默吃掉先扫的 —— 而两份文件都躺在磁盘上。
func TestLoadCustomRolesRejectsDuplicateOnDisk(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, ".claude", "agents")
	// 注意 defaultRoleDirs 的顺序: .claude/agents 先, .claude-go/agents 后。
	b := filepath.Join(dir, ".claude-go", "agents")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "dup.json"),
		[]byte(`{"name":"dup-role","systemPrompt":"先扫的"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "dup.json"),
		[]byte(`{"name":"dup-role","systemPrompt":"后扫的"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rr := NewRoleRegistry(dir)
	got := rr.Get("dup-role")
	if got == nil {
		t.Fatal("dup-role 应被加载一次")
	}
	if got.SystemPrompt != "先扫的" {
		t.Fatalf("同名文件不得互相覆盖 (先扫的应保留), got %q", got.SystemPrompt)
	}
}
