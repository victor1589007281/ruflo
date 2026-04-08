// 权限系统单元测试
// 测试: 权限模式、规则匹配、工具访问控制
package unit

import (
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/types"
)

// TestPermissionModeBypass bypass 模式允许所有操作
func TestPermissionModeBypass(t *testing.T) {
	checker := permissions.NewChecker(types.PermissionModeBypass)

	result := checker.Check("Shell", nil, false)
	if result.Behavior != types.PermissionAllow {
		t.Errorf("bypass 模式应允许 Shell, 实际 %s", result.Behavior)
	}

	result = checker.Check("Write", nil, false)
	if result.Behavior != types.PermissionAllow {
		t.Errorf("bypass 模式应允许 Write, 实际 %s", result.Behavior)
	}
}

// TestPermissionModePlan Plan 模式只允许只读
func TestPermissionModePlan(t *testing.T) {
	checker := permissions.NewChecker(types.PermissionModePlan)

	// 只读操作允许
	result := checker.Check("Read", nil, true)
	if result.Behavior != types.PermissionAllow {
		t.Errorf("Plan 模式应允许只读 Read, 实际 %s", result.Behavior)
	}

	// 写入操作拒绝
	result = checker.Check("Write", nil, false)
	if result.Behavior != types.PermissionDeny {
		t.Errorf("Plan 模式应拒绝写入 Write, 实际 %s", result.Behavior)
	}
}

// TestPermissionModeDefault default 模式: 只读允许, 写入需要确认
func TestPermissionModeDefault(t *testing.T) {
	checker := permissions.NewChecker(types.PermissionModeDefault)

	result := checker.Check("Read", nil, true)
	if result.Behavior != types.PermissionAllow {
		t.Errorf("default 模式应允许只读, 实际 %s", result.Behavior)
	}

	result = checker.Check("Shell", nil, false)
	if result.Behavior != types.PermissionAsk {
		t.Errorf("default 模式下 Shell 应需要确认, 实际 %s", result.Behavior)
	}
}

// TestPermissionDenyRules deny 规则优先级最高
func TestPermissionDenyRules(t *testing.T) {
	checker := permissions.NewChecker(types.PermissionModeBypass)
	checker.AddDenyRule(types.PermissionRule{
		ToolName: "Shell",
		Source:   "test",
	})

	result := checker.Check("Shell", json.RawMessage(`{"command":"rm -rf"}`), false)
	if result.Behavior != types.PermissionDeny {
		t.Errorf("deny 规则应覆盖 bypass 模式, 实际 %s", result.Behavior)
	}

	// 其他工具不受影响
	result = checker.Check("Write", nil, false)
	if result.Behavior != types.PermissionAllow {
		t.Errorf("其他工具不应受 deny 规则影响, 实际 %s", result.Behavior)
	}
}

// TestPermissionAllowRules allow 规则优先于模式
func TestPermissionAllowRules(t *testing.T) {
	checker := permissions.NewChecker(types.PermissionModeDefault)
	checker.AddAllowRule(types.PermissionRule{
		ToolName: "Shell",
		Source:   "settings",
	})

	result := checker.Check("Shell", nil, false)
	if result.Behavior != types.PermissionAllow {
		t.Errorf("allow 规则应允许 Shell, 实际 %s", result.Behavior)
	}
}

// TestPermissionPathPrefix 路径前缀规则 /src/* 匹配子路径
func TestPermissionPathPrefix(t *testing.T) {
	checker := permissions.NewChecker(types.PermissionModeBypass)
	checker.AddDenyRule(types.PermissionRule{
		ToolName: "Read",
		Pattern:  "/src/*",
		Source:   "test",
	})
	in := json.RawMessage(`{"path":"/src/foo.go"}`)
	r := checker.Check("Read", in, true)
	if r.Behavior != types.PermissionDeny {
		t.Errorf("期望 Deny, 得 %s", r.Behavior)
	}
	r = checker.Check("Read", json.RawMessage(`{"path":"/other/foo.go"}`), true)
	if r.Behavior != types.PermissionAllow {
		t.Errorf("前缀外应允许, 得 %s", r.Behavior)
	}
}

// TestPermissionGlobPattern filepath.Match 风格
func TestPermissionGlobPattern(t *testing.T) {
	checker := permissions.NewChecker(types.PermissionModeDefault)
	checker.AddAllowRule(types.PermissionRule{
		ToolName: "Read",
		Pattern:  "*.go",
		Source:   "test",
	})
	in := json.RawMessage(`{"path":"main.go"}`)
	r := checker.Check("Read", in, true)
	if r.Behavior != types.PermissionAllow {
		t.Errorf("glob 应匹配 .go, 得 %s", r.Behavior)
	}
}

// TestPermissionModeAuto auto：只读允许，写入询问
func TestPermissionModeAuto(t *testing.T) {
	c := permissions.NewChecker(types.PermissionModeAuto)
	if r := c.Check("Read", nil, true); r.Behavior != types.PermissionAllow {
		t.Errorf("auto 只读应 Allow, 得 %s", r.Behavior)
	}
	if r := c.Check("Write", nil, false); r.Behavior != types.PermissionAsk {
		t.Errorf("auto 写入应 Ask, 得 %s", r.Behavior)
	}
}

// TestPermissionModeDontAsk dontAsk：将 Ask 转为 Deny
func TestPermissionModeDontAsk(t *testing.T) {
	c := permissions.NewChecker(types.PermissionModeDontAsk)
	r := c.Check("Shell", nil, false)
	if r.Behavior != types.PermissionDeny {
		t.Errorf("dontAsk 应对写入类 Deny, 得 %s", r.Behavior)
	}
}

// TestPermissionModeAcceptEdits acceptEdits：Write/StrReplace 自动允许
func TestPermissionModeAcceptEdits(t *testing.T) {
	c := permissions.NewChecker(types.PermissionModeAcceptEdits)
	if r := c.Check("Write", nil, false); r.Behavior != types.PermissionAllow {
		t.Errorf("Write 应 Allow, 得 %s", r.Behavior)
	}
	if r := c.Check("StrReplace", nil, false); r.Behavior != types.PermissionAllow {
		t.Errorf("StrReplace 应 Allow, 得 %s", r.Behavior)
	}
	if r := c.Check("Shell", nil, false); r.Behavior != types.PermissionAsk {
		t.Errorf("Shell 应 Ask, 得 %s", r.Behavior)
	}
}

// TestContentRuleDeny 内容规则拒绝
func TestContentRuleDeny(t *testing.T) {
	c := permissions.NewChecker(types.PermissionModeBypass)
	c.AddContentRule(types.ContentRule{
		ToolName: "Shell",
		Pattern:  "rm -rf",
		Behavior: types.PermissionDeny,
		Source:   "policy",
	})
	r := c.CheckGlobal("Shell", json.RawMessage(`{"command":"rm -rf /"}`), false, nil)
	if r.Behavior != types.PermissionDeny {
		t.Errorf("内容规则应 Deny, 得 %s", r.Behavior)
	}
}

// TestCheckGlobalToolDeny 工具层 Deny 优先于 bypass 模式
func TestCheckGlobalToolDeny(t *testing.T) {
	c := permissions.NewChecker(types.PermissionModeBypass)
	toolDeny := &types.PermissionResult{Behavior: types.PermissionDeny, Reason: "plan"}
	r := c.CheckGlobal("Write", nil, false, toolDeny)
	if r.Behavior != types.PermissionDeny {
		t.Errorf("工具 Deny 应保留, 得 %s", r.Behavior)
	}
}
