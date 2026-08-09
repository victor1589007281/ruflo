// planmode.go 实现计划模式（Plan Mode）切换工具。
//
// 设计说明:
//   - EnterPlanMode: 进入计划模式后，引擎侧通常仅允许只读工具（与 TS 版 Claude Code 行为对齐）。
//   - ExitPlanMode: 退出计划模式，可附带最终计划文本供会话记录。
//   - 本包使用原子变量维护全局计划模式标志，便于引擎或其它包通过 PlanModeActive 查询。
//
// 对应概念: Claude Code 的 plan / 只读探索阶段。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	EnterPlanModeToolName = "EnterPlanMode"
	ExitPlanModeToolName  = "ExitPlanMode"
)

// planSessions 会话级计划模式开关。
// Key = sessionID (chatID)，避免全局标志在多会话进程中泄露。
var (
	planSessions   = make(map[string]bool)
	planSessionsMu sync.RWMutex
)

// PlanModeActive 兼容旧接口（检查是否有任何会话处于计划模式）。
// 新代码应使用 PlanModeActiveForSession。
func PlanModeActive() bool {
	planSessionsMu.RLock()
	defer planSessionsMu.RUnlock()
	for _, v := range planSessions {
		if v {
			return true
		}
	}
	return false
}

// PlanModeActiveForSession 返回指定会话是否处于计划模式。
func PlanModeActiveForSession(sessionID string) bool {
	planSessionsMu.RLock()
	defer planSessionsMu.RUnlock()
	return planSessions[sessionID]
}

// NewPlanModeChecker 返回一个绑定到指定会话的 plan 检查函数，
// 用于 engine.Config.DynamicPlanCheck。
func NewPlanModeChecker(sessionID string) func() bool {
	return func() bool {
		return PlanModeActiveForSession(sessionID)
	}
}

func setPlanMode(on bool) {
	SetPlanModeForSession("_global", on)
}

// SetPlanModeForSession 写指定会话的 plan mode 标志。导出给装配方 (CLI/飞书) 注入
// engine.Config.PlanModeSetter, 使引擎自动 Plan 与模型 EnterPlanMode/ExitPlanMode
// 工具共用**同一个**会话级 flag (单一状态源)。
func SetPlanModeForSession(sessionID string, on bool) {
	planSessionsMu.Lock()
	defer planSessionsMu.Unlock()
	if on {
		planSessions[sessionID] = true
	} else {
		delete(planSessions, sessionID)
	}
}

// planSessionID 从 ToolContext 取会话 key, 未携带时回落全局 "_global"。
// 会话级隔离: 多会话进程 (飞书) 下 A 会话进入 plan 不再污染 B 会话。
func planSessionID(tctx *tool.ToolContext) string {
	if tctx != nil && tctx.SessionID != "" {
		return tctx.SessionID
	}
	return "_global"
}

// --- EnterPlanModeTool ---

// EnterPlanModeTool 进入计划模式：仅做状态切换与确认文案，输入为空对象 {}。
type EnterPlanModeTool struct{}

// NewEnterPlanModeTool 构造 EnterPlanMode 工具实例。
func NewEnterPlanModeTool() *EnterPlanModeTool {
	return &EnterPlanModeTool{}
}

func (t *EnterPlanModeTool) Name() string { return EnterPlanModeToolName }

func (t *EnterPlanModeTool) Description() string {
	return `进入计划模式：在此模式下应优先制定方案与澄清需求，通常仅使用只读工具，避免直接改文件或执行写入类操作。`
}

func (t *EnterPlanModeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)
}

func (t *EnterPlanModeTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *EnterPlanModeTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

func (t *EnterPlanModeTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 打开计划模式标志并返回中文提示。
func (t *EnterPlanModeTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	if len(input) > 0 && string(input) != "{}" && string(input) != "null" {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(input, &m); err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
		}
		if len(m) > 0 {
			return &tool.ToolResult{Content: "EnterPlanMode 不需要任何字段，请传入空对象 {}。", IsError: true}, nil
		}
	}
	SetPlanModeForSession(planSessionID(tctx), true)
	return &tool.ToolResult{
		Content: "已进入计划模式：请先用只读方式调研与规划，确认方案后再退出计划模式执行改动。",
	}, nil
}

// --- ExitPlanModeTool ---

type exitPlanModeInput struct {
	Plan string `json:"plan,omitempty"`
}

// ExitPlanModeTool 退出计划模式，可选附带 plan 文本（例如最终方案摘要）。
type ExitPlanModeTool struct{}

// NewExitPlanModeTool 构造 ExitPlanMode 工具实例。
func NewExitPlanModeTool() *ExitPlanModeTool {
	return &ExitPlanModeTool{}
}

func (t *ExitPlanModeTool) Name() string { return ExitPlanModeToolName }

func (t *ExitPlanModeTool) Description() string {
	return `退出计划模式，并可选择性地提交一份计划文本（plan），用于会话记录或后续执行参考。`
}

func (t *ExitPlanModeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"plan": {
				"type": "string",
				"description": "可选。退出时附带的计划或结论摘要，便于写入对话上下文。"
			}
		},
		"additionalProperties": false
	}`)
}

func (t *ExitPlanModeTool) IsReadOnly(_ json.RawMessage) bool { return false }

func (t *ExitPlanModeTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 允许在计划模式下调用（否则无法退出计划模式）。
func (t *ExitPlanModeTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 关闭计划模式；若提供 plan，则写入计划文件 (PlanFileDir 已配置时) 并在返回内容中回显路径。
func (t *ExitPlanModeTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in exitPlanModeInput
	if len(input) > 0 && string(input) != "null" {
		if err := json.Unmarshal(input, &in); err != nil {
			return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
		}
	}
	SetPlanModeForSession(planSessionID(tctx), false)

	savedPath := ""
	if in.Plan != "" && tctx != nil && strings.TrimSpace(tctx.PlanFileDir) != "" {
		if path, err := persistPlanFile(tctx.PlanFileDir, planSessionID(tctx), in.Plan); err == nil {
			savedPath = path
		} else {
			// 落盘失败不阻断退出计划模式, 只在结果里提示。
			savedPath = fmt.Sprintf("(计划文件写入失败: %v)", err)
		}
	}

	var b strings.Builder
	b.WriteString("已退出计划模式。")
	if in.Plan != "" {
		b.WriteString("\n\n附带的计划摘要：\n")
		b.WriteString(in.Plan)
	}
	if savedPath != "" {
		b.WriteString("\n\n计划已保存至: " + savedPath)
		b.WriteString("\n实施阶段请先读取该计划文件，严格按其执行。")
	}
	return &tool.ToolResult{Content: b.String()}, nil
}

// persistPlanFile 把提交的计划写入 <PlanFileDir>/plan-<session>-<timestamp>.md。
// 目录不存在时自动创建。返回写入的绝对路径。
func persistPlanFile(dir, sessionID, plan string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建计划目录失败: %w", err)
	}
	name := "plan-" + sanitizePlanName(sessionID) + "-" + time.Now().Format("20060102-150405") + ".md"
	path := filepath.Join(dir, name)
	content := fmt.Sprintf("# 实施计划\n\n- 会话: %s\n- 提交时间: %s\n\n%s\n",
		sessionID, time.Now().Format("2006-01-02 15:04:05"), plan)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// sanitizePlanName 清理 sessionID 用于文件名 (仅保留字母数字与 .-_ )。
func sanitizePlanName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "global"
	}
	return b.String()
}
