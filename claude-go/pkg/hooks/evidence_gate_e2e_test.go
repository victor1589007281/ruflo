package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// 这是 D0.6 的**跨仓端到端**验收: claude-go 的真实 hook 执行器, 驱动平台的
// 真实 agent-hooks 二进制。
//
// 为什么不各自测一半就够: 这条链路上两个仓库只靠**进程契约**相连 —— stdin 喂什么
// JSON、退出码 2 表示什么。各自单测都绿, 而契约对不上时表现是"钩子装了但从不拦",
// 与"钩子压根没装"在面板上一模一样。只有把两端真接上, 才照得出这个。
//
// AIOPS_AGENT_HOOKS_BIN 未设置时跳过: 两台机器上不一定都编得出对面那个二进制,
// 而这条用例的价值恰恰在于"真的跑了对面那个", 用替身跑就退回成自测了。
func TestEvidenceGateEndToEndWithPlatformBinary(t *testing.T) {
	bin := os.Getenv("AIOPS_AGENT_HOOKS_BIN")
	if bin == "" {
		t.Skip("设置 AIOPS_AGENT_HOOKS_BIN=<agent-hooks 绝对路径> 后本用例才有意义")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("AIOPS_AGENT_HOOKS_BIN 指向的文件不存在: %v", err)
	}

	cfg := []types.HookConfig{{
		Event:    types.HookEventPreToolUse,
		HookType: types.HookTypeCommand,
		Command:  bin + " evidence-gate",
		If:       "mcp_gw_submit_finding",
		Timeout:  5000,
	}}
	r := NewRunner(cfg, "e2e-session")

	// ① 无证据的结论 ⇒ 必须被拦 (退出码 2 → claude-go 翻成 Decision=block)。
	blocked, err := r.RunPreToolUseHooks("mcp_gw_submit_finding",
		json.RawMessage(`{"title":"连接池耗尽","severity":"critical"}`))
	if err != nil {
		t.Fatalf("hook 执行本身不应报错 (拦截走的是 decision 不是 error): %v", err)
	}
	if blocked == nil || (blocked.Decision != "block" && blocked.Decision != "deny") {
		t.Fatalf("无证据的结论必须被拦下, got %+v", blocked)
	}
	if blocked.Reason == "" {
		t.Fatal("拦截必须带原因 —— 模型要靠它知道下一步该做什么, " +
			"只说'被拦了'会让它重试同一件事")
	}

	// ② 带证据 ⇒ 必须放行。这是配对验证: 只验"能拦"不验"会放"的话,
	// 一个恒拦截的实现(比如路径写错导致命令永远失败退出 2)也会全绿,
	// 而它在生产上的表现是"这个员工什么都干不了"。
	ok, err := r.RunPreToolUseHooks("mcp_gw_submit_finding",
		json.RawMessage(`{"title":"连接池耗尽","severity":"critical",`+
			`"evidence":"SHOW PROCESSLIST 显示 480/500 连接处于 Sleep > 300s"}`))
	if err != nil {
		t.Fatalf("hook 执行报错: %v", err)
	}
	if ok != nil && (ok.Decision == "block" || ok.Decision == "deny") {
		t.Fatalf("带证据的结论不应被拦, got %+v", ok)
	}

	// ③ 条件没匹配上的工具 ⇒ 钩子根本不该跑。
	// 让 hook 命令指向一个不存在的路径来验: 若它真跑了, 会因命令失败而报错;
	// 没跑就什么都不会发生。这比"看返回值"更硬 —— 返回值可能是 nil 的多种成因。
	never := []types.HookConfig{{
		Event:    types.HookEventPreToolUse,
		HookType: types.HookTypeCommand,
		Command:  filepath.Join(t.TempDir(), "definitely-not-here") + " evidence-gate",
		If:       "mcp_gw_submit_finding",
		Timeout:  5000,
	}}
	r2 := NewRunner(never, "e2e-session")
	if out, err := r2.RunPreToolUseHooks("mcp_gw_sql_query", json.RawMessage(`{}`)); err != nil || out != nil {
		t.Fatalf("条件不匹配的工具不应触发这个 hook, got out=%+v err=%v", out, err)
	}
}

// 闸门二进制自己的行为面 (它是个独立进程, 值得单独钉)。
func TestEvidenceGateBinaryContract(t *testing.T) {
	bin := os.Getenv("AIOPS_AGENT_HOOKS_BIN")
	if bin == "" {
		t.Skip("设置 AIOPS_AGENT_HOOKS_BIN 后本用例才有意义")
	}

	run := func(t *testing.T, stdin string) (int, string) {
		t.Helper()
		cmd := exec.Command(bin, "evidence-gate")
		cmd.Stdin = strings.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("运行失败: %v", err)
		}
		return code, stderr.String()
	}

	// 退出码的语义是契约本身 (claude-go 认 2 = 拦截)。改成在 stdout 里写 JSON
	// 也能work, 但 hook 由 sh -c 跑, 任何一句日志都会污染 stdout —— 拦截语义
	// 不该建立在"输出干净"这个假设上。
	if code, _ := run(t, `{"event":"PreToolUse","tool_name":"mcp_gw_submit_finding","tool_input":{"title":"x"}}`); code != 2 {
		t.Errorf("无证据应退出码 2, got %d", code)
	}
	if code, _ := run(t, `{"event":"PreToolUse","tool_name":"mcp_gw_submit_finding","tool_input":{"evidence":"SHOW SLAVE STATUS 显示 Seconds_Behind=0"}}`); code != 0 {
		t.Errorf("带证据应退出码 0, got %d", code)
	}
	// 输入读不懂时放行: 闸门管的是"结论有没有证据"(质量约束), 不是权限约束,
	// 质量约束失效时该降级放行, 不该用我自己的解析 bug 去拦它的正常工作。
	if code, _ := run(t, `这不是 JSON`); code != 0 {
		t.Errorf("输入不可解析时应放行(退出码 0), got %d", code)
	}
}
