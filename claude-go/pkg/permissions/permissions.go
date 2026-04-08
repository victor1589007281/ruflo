// Package permissions 实现工具权限检查系统。
// 对应 TS 源码: review/claude/src/utils/permissions/permissions.ts
//
// 权限规则来源:
//   - settings.json (项目/用户)
//   - .claude/settings.json
//   - CLI 参数 (--allowedTools 等)
//   - Hook 决策 (pre-tool-use hook)
package permissions

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// Checker 权限检查器
type Checker struct {
	Mode          types.PermissionMode
	AllowRules    []types.PermissionRule
	DenyRules     []types.PermissionRule
	AskRules      []types.PermissionRule
	ContentRules  []types.ContentRule
}

// NewChecker 创建权限检查器
func NewChecker(mode types.PermissionMode) *Checker {
	return &Checker{Mode: mode}
}

// Check 检查工具是否可以执行（不含单工具 CheckPermissions，仅全局规则 + 模式）。
// 用于测试或仅需全局策略的场景；执行路径请使用 CheckGlobal。
func (c *Checker) Check(toolName string, input json.RawMessage, isReadOnly bool) types.PermissionResult {
	return c.CheckGlobal(toolName, input, isReadOnly, nil)
}

// CheckGlobal 在工具执行路径上合并全局检查器与单工具的 CheckPermissions 结果。
//
// 权限优先级链算法（从高到低，命中即生效或进入下一层；中文说明便于与产品文档对齐）：
//
//  1. 【拒绝规则 DenyRules】始终最高：任意一条 matchRule 命中 → 立即 Deny，不再看其余层。
//     用于黑名单（如危险 Shell、敏感路径）。
//
//  2. 【工具自带 CheckPermissions】若 toolPerm 非 nil 且 Behavior 为 deny → 立即 Deny。
//     体现工具域内硬约束（例如 Plan 模式下写工具自行拒绝）。
//     若为 allow/ask，不在这里结束，继续交给内容规则与模式，避免绕过全局策略。
//
//  3. 【内容规则 ContentRules】在工具名匹配前提下，用 Pattern 对输入 JSON/路径做匹配：
//     命中则采用该规则的 Behavior（allow/deny/ask），用于细粒度内容策略。
//
//  4. 【模式 PermissionMode】bypass / plan / auto / dontAsk / acceptEdits / default：
//     在未被上述层明确放行或拒绝时，决定默认是允许、拒绝还是询问。
//
//  5. 【允许规则 AllowRules】最后应用：任意一条 matchRule 命中 → Allow。
//     作为白名单兜底，可覆盖模式层产生的 Ask（便于用户显式 alwaysAllow）。
//
//  6. 若仍无明确允许且模式要求询问 → Ask；dontAsk 模式下将 Ask 转为 Deny。
//
// 注意：Deny 永远优先于 Allow；Allow 规则排在模式之后，用于在「默认要确认」时仍允许特定工具/路径。
func (c *Checker) CheckGlobal(
	toolName string,
	input json.RawMessage,
	isReadOnly bool,
	toolPerm *types.PermissionResult,
) types.PermissionResult {
	// 第 1 层：全局拒绝规则（最高优先级）
	for _, rule := range c.DenyRules {
		if matchRule(rule, toolName, input) {
			return types.PermissionResult{
				Behavior: types.PermissionDeny,
				Reason:   "被 alwaysDeny 规则匹配: " + rule.Pattern,
				Source:   rule.Source,
			}
		}
	}

	// 第 2 层：工具自带权限（仅 deny 直接短路）
	if toolPerm != nil && toolPerm.Behavior == types.PermissionDeny {
		return *toolPerm
	}

	// 第 3 层：内容规则
	for _, cr := range c.ContentRules {
		if matchContentRule(cr, toolName, input) {
			return c.applyDontAsk(types.PermissionResult{
				Behavior: cr.Behavior,
				Reason:   "内容规则匹配: " + cr.Pattern,
				Source:   cr.Source,
			})
		}
	}

	// 第 4 层：模式
	modeRes := c.modeDecision(toolName, isReadOnly)

	// 第 5 层：允许规则（可覆盖模式产生的 Ask）
	for _, rule := range c.AllowRules {
		if matchRule(rule, toolName, input) {
			return types.PermissionResult{
				Behavior: types.PermissionAllow,
				Reason:   "被 alwaysAllow 规则匹配",
				Source:   rule.Source,
			}
		}
	}

	return c.applyDontAsk(modeRes)
}

// applyDontAsk 在 dontAsk 模式下将 Ask 统一转为 Deny（内容规则与模式层共用）。
func (c *Checker) applyDontAsk(r types.PermissionResult) types.PermissionResult {
	if r.Behavior == types.PermissionAsk && c.Mode == types.PermissionModeDontAsk {
		return types.PermissionResult{
			Behavior: types.PermissionDeny,
			Reason:   "dontAsk 模式: 不再询问，自动拒绝",
		}
	}
	return r
}

func (c *Checker) modeDecision(toolName string, isReadOnly bool) types.PermissionResult {
	switch c.Mode {
	case types.PermissionModeBypass:
		return types.PermissionResult{Behavior: types.PermissionAllow, Reason: "bypass 模式"}

	case types.PermissionModePlan:
		if !isReadOnly {
			return types.PermissionResult{Behavior: types.PermissionDeny, Reason: "Plan 模式: 仅允许只读操作"}
		}
		return types.PermissionResult{Behavior: types.PermissionAllow, Reason: "Plan 模式: 只读操作"}

	case types.PermissionModeAuto:
		if isReadOnly {
			return types.PermissionResult{Behavior: types.PermissionAllow, Reason: "auto 模式: 只读工具自动允许"}
		}
		return types.PermissionResult{Behavior: types.PermissionAsk, Reason: "auto 模式: 写入类工具需确认"}

	case types.PermissionModeAcceptEdits:
		if isReadOnly {
			return types.PermissionResult{Behavior: types.PermissionAllow, Reason: "acceptEdits 模式: 只读工具自动允许"}
		}
		if isFileEditTool(toolName) {
			return types.PermissionResult{Behavior: types.PermissionAllow, Reason: "acceptEdits 模式: 文件编辑工具自动允许"}
		}
		return types.PermissionResult{Behavior: types.PermissionAsk, Reason: "acceptEdits 模式: 非文件编辑的写入需确认"}

	case types.PermissionModeDontAsk:
		// 与 default 相同：先产生 Ask，再由 CheckGlobal 将 Ask 转为 Deny
		fallthrough
	default: // PermissionModeDefault、dontAsk 及未知模式
		if isReadOnly {
			return types.PermissionResult{Behavior: types.PermissionAllow, Reason: "只读操作自动允许"}
		}
		if c.Mode == types.PermissionModeDontAsk {
			return types.PermissionResult{Behavior: types.PermissionAsk, Reason: "dontAsk: 写入类工具本会询问，将自动拒绝"}
		}
		return types.PermissionResult{Behavior: types.PermissionAsk, Reason: "default 模式: 需要用户确认"}
	}
}

func isFileEditTool(name string) bool {
	switch name {
	case "Write", "StrReplace", "MultiEdit":
		return true
	default:
		return false
	}
}

// AddAllowRule 添加允许规则
func (c *Checker) AddAllowRule(rule types.PermissionRule) {
	c.AllowRules = append(c.AllowRules, rule)
}

// AddDenyRule 添加拒绝规则
func (c *Checker) AddDenyRule(rule types.PermissionRule) {
	c.DenyRules = append(c.DenyRules, rule)
}

// AddContentRule 添加基于工具输入内容的规则
func (c *Checker) AddContentRule(rule types.ContentRule) {
	c.ContentRules = append(c.ContentRules, rule)
}

// matchRule 检查 PermissionRule 是否匹配工具调用（工具名 + 可选路径/模式）。
func matchRule(rule types.PermissionRule, toolName string, input json.RawMessage) bool {
	if !toolNameMatches(rule.ToolName, toolName) {
		return false
	}
	return patternMatches(rule.Pattern, input)
}

func matchContentRule(rule types.ContentRule, toolName string, input json.RawMessage) bool {
	if !toolNameMatches(rule.ToolName, toolName) {
		return false
	}
	return contentPatternMatches(rule.Pattern, input)
}

func toolNameMatches(ruleTool, actual string) bool {
	if ruleTool == "" || ruleTool == "*" {
		return true
	}
	return strings.EqualFold(ruleTool, actual)
}

// patternMatches 支持：空或 *；从 input 提取路径后的精确匹配、前缀路径（/src/*、/src/**）、filepath.Match 通配。
func patternMatches(pattern string, input json.RawMessage) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	pathVal := extractPathFromInput(input)
	if pathVal != "" {
		if matchPathPattern(pattern, pathVal) {
			return true
		}
	}
	// 无路径字段时，对原始 JSON 做精确或 glob
	s := strings.TrimSpace(string(input))
	if s == "" {
		return false
	}
	if s == pattern {
		return true
	}
	ok, err := filepath.Match(pattern, s)
	return err == nil && ok
}

// contentPatternMatches：对紧凑 JSON 串做子串或 glob 匹配。
func contentPatternMatches(pattern string, input json.RawMessage) bool {
	if pattern == "" {
		return false
	}
	compact := compactJSONInput(input)
	if compact == "" {
		return false
	}
	if strings.ContainsAny(pattern, "*?[") {
		ok, err := filepath.Match(pattern, compact)
		if err == nil && ok {
			return true
		}
	}
	return strings.Contains(compact, pattern)
}

func compactJSONInput(input json.RawMessage) string {
	s := strings.TrimSpace(string(input))
	if s == "" {
		return ""
	}
	var v interface{}
	if json.Unmarshal(input, &v) != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

func extractPathFromInput(input json.RawMessage) string {
	var m map[string]interface{}
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	keys := []string{"path", "file_path", "filepath", "target", "target_path", "file", "old_path", "new_path"}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return filepath.Clean(s)
			}
		}
	}
	return ""
}

// matchPathPattern：精确、/prefix/*、/prefix/**、filepath.Match。
func matchPathPattern(pattern, pathVal string) bool {
	pathVal = filepath.Clean(pathVal)
	pattern = filepath.Clean(pattern)

	if pattern == "" || pattern == "." {
		return true
	}
	if pathVal == pattern {
		return true
	}

	if strings.HasSuffix(pattern, string(filepath.Separator)+"*") {
		prefix := strings.TrimSuffix(pattern, string(filepath.Separator)+"*")
		prefix = filepath.Clean(prefix)
		return pathVal == prefix || strings.HasPrefix(pathVal, prefix+string(filepath.Separator))
	}
	if strings.HasSuffix(pattern, string(filepath.Separator)+"**") {
		prefix := strings.TrimSuffix(pattern, string(filepath.Separator)+"**")
		prefix = filepath.Clean(prefix)
		return pathVal == prefix || strings.HasPrefix(pathVal, prefix+string(filepath.Separator))
	}

	// Unix 风格规则在 Windows 上仍常见：统一再试一遍 "/" 形式
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		prefix = filepath.Clean(prefix)
		return pathVal == prefix || strings.HasPrefix(pathVal, prefix+"/") || strings.HasPrefix(pathVal, prefix+string(filepath.Separator))
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		prefix = filepath.Clean(prefix)
		return pathVal == prefix || strings.HasPrefix(pathVal, prefix+"/") || strings.HasPrefix(pathVal, prefix+string(filepath.Separator))
	}

	ok, err := filepath.Match(pattern, pathVal)
	return err == nil && ok
}
