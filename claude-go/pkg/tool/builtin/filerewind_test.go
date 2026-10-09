// filerewind_test.go —— H3-1 回滚出口的单测。
//
// 方案 §21 H3-1 的验收原文是: **「改坏文件后回滚, 文件内容逐字节等于快照; 且
// Restore 的非测试调用方 ≥ 1」**。
//
// # 判据必须落在磁盘上
//
// "有调用方"是装配态, 一击就假绿: 只要有一行生产代码写着 `h.Restore(...)`, 无论
// 它跑不跑得到、参数对不对, 这条验收都过了。所以本文件的主臂不数调用方, 而是
// **跑一遍真实路径**: 用真的 Write 工具改文件 ⇒ 用真的 Rewind 工具回退 ⇒ 把
// 字节从磁盘读回来比对。这同时覆盖了"调用方存在"与"调用方正确", 而前者只是后者的
// 一个必要不充分条件。
//
// # 每个"成功"都配一条"失败必须报错"
//
// 回退这个能力的失败模式是不对称的: 成功只是省了几步, 而**谎报成功会直接损坏
// 工作区** —— 模型以为文件已经回去了, 于是继续在错误状态上往下做。所以每一条
// "退回去了"的用例旁边, 都有一条"退不了时必须报错、且文件一个字节都不能动"。
//
// # 与 FileHistory 单测的分工
//
// filehistory_test.go 测的是存储层 (版本命名/淘汰/去重); 本文件测的是**工具层**:
// 缺省回退一步的语义、连续回退不原地打转、失败被结构化成模型的下一步动作。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

// rewindTctx 造一个工具上下文 (Cwd=/ 使相对路径不参与; 传入的都是绝对路径)。
func rewindTctx(sessionID string) *tool.ToolContext {
	return &tool.ToolContext{Cwd: "/", SessionID: sessionID}
}

// callRewind 调一次 Rewind 工具。
func callRewind(t *testing.T, path, ref, sessionID string) *tool.ToolResult {
	t.Helper()
	in := fmt.Sprintf(`{"path":%q`, path)
	if ref != "" {
		in += fmt.Sprintf(`,"ref":%q`, ref)
	}
	in += "}"
	res, err := NewRewindTool().Call(context.Background(), json.RawMessage(in), rewindTctx(sessionID))
	if err != nil {
		t.Fatalf("Rewind 返回了 error (工具错误应走 IsError 而不是 error): %v", err)
	}
	if res == nil {
		t.Fatal("Rewind 返回了 nil 结果")
	}
	return res
}

// callRewindOK 调一次 Rewind 并要求成功。
func callRewindOK(t *testing.T, path, ref, sessionID string) string {
	t.Helper()
	res := callRewind(t, path, ref, sessionID)
	if res.IsError {
		t.Fatalf("Rewind 失败: %s", res.Content)
	}
	return res.Content
}

// readFileOrFatal 从磁盘原样读回内容 —— 本文件所有"回退成功"的判据都落在这里。
func readFileOrFatal(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回文件失败: %v", err)
	}
	return string(b)
}

// ============================================================================
// 核心: 方案验收原文
// ============================================================================

// TestH31_RewindRestoresBytesFromDisk 改坏文件后回滚, 磁盘内容逐字节等于快照。
//
// 走的全是生产路径: Write 工具 (它会 checkpointBeforeWrite/After) 改文件,
// Rewind 工具回退, 判据从磁盘读回。中间没有任何一处直接碰 FileHistory。
func TestH31_RewindRestoresBytesFromDisk(t *testing.T) {
	newTestHistory(t)
	p := filepath.Join(t.TempDir(), "x.go")

	const good = "package main\n\nfunc main() { println(\"ok\") }\n"
	if err := os.WriteFile(p, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}

	// 模型把文件改坏了 (真实调用路径, 因此检查点真的产生了两个版本)。
	const broken = "package main\n\nfunc main() { THIS DOES NOT COMPILE\n"
	writeViaTool(t, p, broken)
	if got := readFileOrFatal(t, p); got != broken {
		t.Fatalf("前置条件不成立: Write 之后磁盘内容不是坏版本:\n%q", got)
	}

	// 回退一步。
	callRewindOK(t, p, "", "")

	if got := readFileOrFatal(t, p); got != good {
		t.Fatalf("回退后磁盘内容不等于快照。\n实得: %q\n期望: %q", got, good)
	}
}

// TestH31_ExplicitRefRestoresThatVersion 指定 ref 时恢复的是**那一个**版本。
//
// 缺省回退一步只能退一格; 长程任务里更常见的是"我要退到第 60 轮那次之前", 跨好几
// 个版本。少了这一臂, 一个只会退一步的实现也能让上面那条用例全绿。
func TestH31_ExplicitRefRestoresThatVersion(t *testing.T) {
	h := newTestHistory(t)
	p := filepath.Join(t.TempDir(), "y.go")

	const v1 = "版本一\n"
	if err := os.WriteFile(p, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	writeViaTool(t, p, "版本二\n")
	writeViaTool(t, p, "版本三\n")

	// 取"版本一"的引用 —— 它不在最近两个里, 必须靠显式 ref 才够得着。
	var refV1 string
	for _, ref := range h.Versions(p, "") {
		data, err := os.ReadFile(filepath.Join(h.dir(""), ref))
		if err == nil && string(data) == v1 {
			refV1 = ref
		}
	}
	if refV1 == "" {
		t.Fatalf("版本史里找不到内容为 %q 的版本: %v", v1, h.Versions(p, ""))
	}

	callRewindOK(t, p, refV1, "")
	if got := readFileOrFatal(t, p); got != v1 {
		t.Fatalf("显式 ref 回退后内容错误: %q, 期望 %q", got, v1)
	}
}

// TestH31_ConsecutiveRewindKeepsGoingBack 连续回退必须继续往前, 不能原地打转。
//
// 这条钉住的是**回退后不记新版本**这个决定 (见 filerewind.go 文件头)。
// 若在恢复后记一版 (v4 = v2 的内容), 第二次回退的锚点就成了 v4, PreviousVersion
// 会给出 v3 —— 连按两次等于前后横跳, 而两次都报"成功"。那是本能力最坏的一种坏:
// 看起来在工作, 实际上把模型往错误的版本上推。
func TestH31_ConsecutiveRewindKeepsGoingBack(t *testing.T) {
	newTestHistory(t)
	p := filepath.Join(t.TempDir(), "z.go")

	const v1, v2, v3 = "第一版\n", "第二版\n", "第三版\n"
	if err := os.WriteFile(p, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	writeViaTool(t, p, v2)
	writeViaTool(t, p, v3)

	callRewindOK(t, p, "", "")
	if got := readFileOrFatal(t, p); got != v2 {
		t.Fatalf("第一次回退应到 v2, 实得 %q", got)
	}
	callRewindOK(t, p, "", "")
	if got := readFileOrFatal(t, p); got != v1 {
		t.Fatalf("第二次回退应继续到 v1, 实得 %q —— 回退在原地打转", got)
	}
	// 最旧一版: 再退一步没有目标, 必须报错而不是"恢复成自己"。
	res := callRewind(t, p, "", "")
	if !res.IsError {
		t.Fatalf("已到最旧版本还报成功: %s", res.Content)
	}
	if got := readFileOrFatal(t, p); got != v1 {
		t.Fatalf("失败的回退改动了文件: %q", got)
	}
}

// ============================================================================
// 失败必须报错 (对照臂) —— 每条都同时断言文件一个字节没动
// ============================================================================

// TestH31_NoCheckpointEnabledFailsAndSaysSo 未启用检查点时必须报错。
//
// 这是最重要的一条对照臂: 缺了它, 一个 `if h == nil { return 成功 }` 的实现会
// 让所有"回退成功"的用例仍然全绿, 而生产上 (没接检查点的部署) 每次回退都是一次
// **静默的数据损坏** —— 模型被告知文件已经回去了, 于是继续在坏内容上往下做。
func TestH31_NoCheckpointEnabledFailsAndSaysSo(t *testing.T) {
	SetFileHistory(nil)
	p := filepath.Join(t.TempDir(), "no-checkpoint.txt")
	const original = "原内容\n"
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	res := callRewind(t, p, "", "")
	if !res.IsError {
		t.Fatalf("未启用检查点时回退报成功 —— 这是一条静默损坏工作区的路径: %s", res.Content)
	}
	// 措辞里要能读出"没有版本可退"这个事实, 而不是让模型去猜别的失败原因。
	if !strings.Contains(res.Content, "检查点") {
		t.Fatalf("失败原因没有指出检查点未启用: %s", res.Content)
	}
	if got := readFileOrFatal(t, p); got != original {
		t.Fatalf("失败的回退改动了文件: %q", got)
	}
}

// TestH31_UnknownFileFailsAndLeavesFileAlone 没改过的文件没有版本可退。
//
// 与上一条分开: 检查点是启用着的, 只是这个文件本会话没被 Write/StrReplace 碰过。
// 两种情形的正确处置不同 (前者别再试工具, 后者可能是路径写错了), 读数也不能混。
func TestH31_UnknownFileFailsAndLeavesFileAlone(t *testing.T) {
	newTestHistory(t)
	p := filepath.Join(t.TempDir(), "never-written.txt")
	const original = "从未被工具写过\n"
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	res := callRewind(t, p, "", "")
	if !res.IsError {
		t.Fatalf("无版本史上的文件回退报成功: %s", res.Content)
	}
	if !strings.Contains(res.Content, "没有任何已记录的版本") {
		t.Fatalf("失败原因没有区分出'本会话没改过这个文件': %s", res.Content)
	}
	if got := readFileOrFatal(t, p); got != original {
		t.Fatalf("失败的回退改动了文件: %q", got)
	}
}

// TestH31_ExternallyModifiedFileRefusesToStepBack 磁盘内容不在版本史里时拒退。
//
// 这是"回退"与"覆盖别人的改动"之间唯一的分界。文件被本会话之外的东西改过
// (另一个进程、另一名工程师、一次 git checkout) 时, 缺省回退拿不到锚点 ——
// PreviousVersion 返回空。此时**必须**拒退: 猜一个版本恢复过去会静默吞掉那次外部
// 改动, 而那是不可逆的。
func TestH31_ExternallyModifiedFileRefusesToStepBack(t *testing.T) {
	newTestHistory(t)
	p := filepath.Join(t.TempDir(), "external.txt")

	if err := os.WriteFile(p, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeViaTool(t, p, "v2\n")

	// 外部改动: 不经过任何工具, 直接写盘。
	const external = "被别人改过了\n"
	if err := os.WriteFile(p, []byte(external), 0o644); err != nil {
		t.Fatal(err)
	}

	res := callRewind(t, p, "", "")
	if !res.IsError {
		t.Fatalf("磁盘内容不在版本史里却回退成功了 —— 会静默吞掉外部改动: %s", res.Content)
	}
	if got := readFileOrFatal(t, p); got != external {
		t.Fatalf("失败的回退覆盖了外部改动: %q", got)
	}
}

// TestH31_BadRefFailsAndLeavesFileAlone 非法 / 不存在的 ref 必须报错。
func TestH31_BadRefFailsAndLeavesFileAlone(t *testing.T) {
	newTestHistory(t)
	p := filepath.Join(t.TempDir(), "badref.txt")
	const original = "内容\n"
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	writeViaTool(t, p, "改过了\n")
	current := readFileOrFatal(t, p)

	for _, ref := range []string{
		"deadbeefdeadbeef@v99", // 合法形状但不存在
		"../../../etc/passwd",  // 路径穿越
		"sub/dir@v1",           // 含分隔符
	} {
		res := callRewind(t, p, ref, "")
		if !res.IsError {
			t.Fatalf("ref=%q 回退报成功", ref)
		}
		if got := readFileOrFatal(t, p); got != current {
			t.Fatalf("ref=%q 失败的回退改动了文件: %q", ref, got)
		}
	}
}

// TestH31_EmptyPathFails 空 path 不能当成"当前目录".
func TestH31_EmptyPathFails(t *testing.T) {
	newTestHistory(t)
	res := callRewind(t, "", "", "")
	if !res.IsError {
		t.Fatalf("空 path 报成功: %s", res.Content)
	}
}

// ============================================================================
// 装配与权限
// ============================================================================

// TestH31_RewindRegisteredInCodingProfile 可写档位必须注册回滚出口。
//
// 判据是"注册表里有没有", 而不是"源码里有没有 NewRewindTool" —— 后者是装配态,
// 一次误删 registerFileMutationTools 里那行就能让它假绿, 而回滚出口会重新变成
// 零调用方 (即本任务要修的那个缺口本身)。
func TestH31_RewindRegisteredInCodingProfile(t *testing.T) {
	// 三个都要: admin 走 RegisterBaseToolsWithStore (全量), coding 走档位分支,
	// 而 RegisterBaseTools 本身是 CLI 主路径与嵌套子代理注册表 (cmd/claude-go 的
	// buildEngine 与 runNestedAgent 各调一次) —— 少了最后一个, 子代理能改文件
	// 却不能撤销, 而"子代理"恰恰是最容易把改动做坏的那一层。
	for _, profile := range []ToolProfile{ToolProfileCoding, ToolProfileAdmin} {
		reg := tool.NewRegistry()
		RegisterProfileToolsWithStore(reg, nil, nil, profile)
		if !hasTool(reg, RewindToolName) {
			t.Errorf("档位 %q 没有注册 %s —— 可写文件的地方必须能撤销", profile, RewindToolName)
		}
	}
	base := tool.NewRegistry()
	RegisterBaseTools(base, nil)
	if !hasTool(base, RewindToolName) {
		t.Errorf("RegisterBaseTools 没有注册 %s —— CLI 主路径与子代理注册表都走它", RewindToolName)
	}
	// 对照臂: 只读档位**不该**有它。注册了不会出错 (执行时会失败), 但白白多出
	// 一份每轮重发的 schema —— 而这份 schema 所承诺的能力在这些档位里根本没有。
	for _, profile := range []ToolProfile{ToolProfileAnalysis, ToolProfileResearch, ToolProfileTeam} {
		reg := tool.NewRegistry()
		RegisterProfileToolsWithStore(reg, nil, nil, profile)
		if hasTool(reg, RewindToolName) {
			t.Errorf("只读档位 %q 注册了 %s —— 这些档位写不了文件, 也就没有东西可退", profile, RewindToolName)
		}
	}
}

// 注: "弱模型必须看得见 Rewind" 那条断言不在这里, 而在 pkg/weakmodel ——
// CoreVisibleTools / DefaultMaskedTools 是那边的数据, 判据跟着数据走。放在这里
// 需要 builtin 反向依赖 weakmodel, 为一句断言引入一条包依赖不划算。

// TestH31_DeniedInPlanMode 规划期不可回退。
//
// 回退是一次真实的文件写入。计划文件目录那条例外在这里**不**适用: 规划期唯一该被
// 写的是计划本身, 而回退按版本覆盖任意文件。
func TestH31_DeniedInPlanMode(t *testing.T) {
	tctx := &tool.ToolContext{Cwd: "/", PermissionMode: "plan"}
	perm := NewRewindTool().CheckPermissions(nil, tctx)
	if perm == nil || perm.Behavior != "deny" {
		t.Fatalf("规划期没有拒绝回退: %+v", perm)
	}
	// 对照臂: 正常模式下不拦。
	if perm := NewRewindTool().CheckPermissions(nil, &tool.ToolContext{Cwd: "/"}); perm != nil {
		t.Fatalf("正常模式被拦了: %+v", perm)
	}
}

// TestH31_RewindIsNotConcurrencySafe 回退会覆盖整个文件, 不能与其他写并发。
func TestH31_RewindIsNotConcurrencySafe(t *testing.T) {
	tool := NewRewindTool()
	if tool.IsConcurrencySafe(nil) {
		t.Fatal("Rewind 被标成了并发安全 —— 它做的是整文件覆盖")
	}
	if tool.IsReadOnly(nil) {
		t.Fatal("Rewind 被标成了只读")
	}
}

// hasTool 注册表里有没有这个名字。
func hasTool(reg *tool.Registry, name string) bool {
	for _, n := range reg.Names() {
		if n == name {
			return true
		}
	}
	return false
}
