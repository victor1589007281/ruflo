package computeruse

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ──────────────────── ScreenCapture Tool ────────────────────

type ScreenCaptureTool struct {
	limiter *RateLimiter
}

func NewScreenCaptureTool(cfg SecurityConfig) *ScreenCaptureTool {
	return &ScreenCaptureTool{limiter: NewRateLimiter(cfg)}
}

func (t *ScreenCaptureTool) Name() string { return "ScreenCapture" }
func (t *ScreenCaptureTool) Description() string {
	return "截取当前屏幕或指定区域的截图。返回 base64 编码的 PNG 图片数据和尺寸。用于 AI 观察屏幕内容。"
}

func (t *ScreenCaptureTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
	"type": "object",
	"properties": {
		"region": {
			"type": "object",
			"description": "截取区域 (不指定=全屏)",
			"properties": {
				"x": {"type": "integer"},
				"y": {"type": "integer"},
				"width": {"type": "integer"},
				"height": {"type": "integer"}
			}
		},
		"scale": {
			"type": "number",
			"description": "缩放比例 0.25~1.0 (降低传给模型的图片大小)",
			"default": 0.5
		}
	}
}`)
}

type screenCaptureInput struct {
	Region *Region  `json:"region,omitempty"`
	Scale  *float64 `json:"scale,omitempty"`
}

func (t *ScreenCaptureTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	if err := t.limiter.Allow(); err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	var in screenCaptureInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "参数解析失败: " + err.Error(), IsError: true}, nil
	}

	scale := 0.5
	if in.Scale != nil && *in.Scale > 0 && *in.Scale <= 1.0 {
		scale = *in.Scale
	}

	result, err := CaptureScreen(ctx, in.Region, scale)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	respJSON, _ := json.Marshal(map[string]interface{}{
		"status":     "success",
		"base64_png": result.Base64PNG,
		"width":      result.Width,
		"height":     result.Height,
	})
	return &tool.ToolResult{Content: string(respJSON)}, nil
}

func (t *ScreenCaptureTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
func (t *ScreenCaptureTool) IsReadOnly(_ json.RawMessage) bool       { return true }

func (t *ScreenCaptureTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx.PermissionMode == types.PermissionModeBypass {
		return nil
	}
	return &types.PermissionResult{
		Behavior: types.PermissionAsk,
		Reason:   "computer-use: 截取屏幕截图",
	}
}

// ──────────────────── MouseAction Tool ────────────────────

type MouseActionTool struct {
	limiter *RateLimiter
}

func NewMouseActionTool(cfg SecurityConfig) *MouseActionTool {
	return &MouseActionTool{limiter: NewRateLimiter(cfg)}
}

func (t *MouseActionTool) Name() string { return "MouseAction" }
func (t *MouseActionTool) Description() string {
	return "执行鼠标操作: 移动(move)、点击(click)、双击(double_click)、右键(right_click)、拖拽(drag)。"
}

func (t *MouseActionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
	"type": "object",
	"properties": {
		"action": {
			"type": "string",
			"enum": ["move", "click", "double_click", "right_click", "drag"],
			"description": "鼠标动作类型"
		},
		"x": {"type": "integer", "description": "目标 X 坐标"},
		"y": {"type": "integer", "description": "目标 Y 坐标"},
		"end_x": {"type": "integer", "description": "拖拽终点 X (仅 drag)"},
		"end_y": {"type": "integer", "description": "拖拽终点 Y (仅 drag)"},
		"screenshot_after": {"type": "boolean", "default": true, "description": "操作后自动截图"}
	},
	"required": ["action", "x", "y"]
}`)
}

func (t *MouseActionTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	if err := t.limiter.Allow(); err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	var in struct {
		MouseInput
		ScreenshotAfter *bool `json:"screenshot_after,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "参数解析失败: " + err.Error(), IsError: true}, nil
	}

	if err := ExecuteMouse(ctx, in.MouseInput); err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	resp := map[string]interface{}{
		"status":  "success",
		"action":  in.Action,
		"x":       in.X,
		"y":       in.Y,
	}

	screenshotAfter := true
	if in.ScreenshotAfter != nil {
		screenshotAfter = *in.ScreenshotAfter
	}
	if screenshotAfter {
		if capture, err := CaptureScreen(ctx, nil, 0.5); err == nil {
			resp["screenshot"] = capture.Base64PNG
		}
	}

	respJSON, _ := json.Marshal(resp)
	return &tool.ToolResult{Content: string(respJSON)}, nil
}

func (t *MouseActionTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *MouseActionTool) IsReadOnly(_ json.RawMessage) bool       { return false }

func (t *MouseActionTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx.PermissionMode == types.PermissionModeBypass {
		return nil
	}
	return &types.PermissionResult{
		Behavior: types.PermissionAsk,
		Reason:   "computer-use: 执行鼠标操作",
	}
}

// ──────────────────── KeyboardAction Tool ────────────────────

type KeyboardActionTool struct {
	limiter *RateLimiter
}

func NewKeyboardActionTool(cfg SecurityConfig) *KeyboardActionTool {
	return &KeyboardActionTool{limiter: NewRateLimiter(cfg)}
}

func (t *KeyboardActionTool) Name() string { return "KeyboardAction" }
func (t *KeyboardActionTool) Description() string {
	return "执行键盘操作: 输入文本(type)、按键(key)、组合键(hotkey)。支持 Enter/Tab/Escape 等特殊键和 Ctrl/Cmd+C 等组合键。"
}

func (t *KeyboardActionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
	"type": "object",
	"properties": {
		"action": {
			"type": "string",
			"enum": ["type", "key", "hotkey"],
			"description": "键盘动作: type=输入文本, key=按单键, hotkey=组合键"
		},
		"text": {"type": "string", "description": "要输入的文本 (action=type 时)"},
		"key": {"type": "string", "description": "按键名 (action=key/hotkey 时): enter, tab, escape, space, up, down, left, right, backspace, delete, f1-f8"},
		"modifiers": {
			"type": "array",
			"items": {"type": "string", "enum": ["ctrl", "alt", "shift", "cmd", "super"]},
			"description": "修饰键 (action=hotkey 时)"
		},
		"screenshot_after": {"type": "boolean", "default": true}
	},
	"required": ["action"]
}`)
}

func (t *KeyboardActionTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	if err := t.limiter.Allow(); err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	var in struct {
		KeyboardInput
		ScreenshotAfter *bool `json:"screenshot_after,omitempty"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "参数解析失败: " + err.Error(), IsError: true}, nil
	}

	if err := ExecuteKeyboard(ctx, in.KeyboardInput); err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	resp := map[string]interface{}{
		"status": "success",
		"action": in.Action,
	}

	screenshotAfter := true
	if in.ScreenshotAfter != nil {
		screenshotAfter = *in.ScreenshotAfter
	}
	if screenshotAfter {
		if capture, err := CaptureScreen(ctx, nil, 0.5); err == nil {
			resp["screenshot"] = capture.Base64PNG
		}
	}

	respJSON, _ := json.Marshal(resp)
	return &tool.ToolResult{Content: string(respJSON)}, nil
}

func (t *KeyboardActionTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *KeyboardActionTool) IsReadOnly(_ json.RawMessage) bool       { return false }

func (t *KeyboardActionTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx.PermissionMode == types.PermissionModeBypass {
		return nil
	}
	return &types.PermissionResult{
		Behavior: types.PermissionAsk,
		Reason:   "computer-use: 执行键盘操作",
	}
}

// ──────────────────── ComputerUseStatus Tool ────────────────────

type ComputerUseStatusTool struct{}

func NewComputerUseStatusTool() *ComputerUseStatusTool { return &ComputerUseStatusTool{} }
func (t *ComputerUseStatusTool) Name() string          { return "ComputerUseStatus" }
func (t *ComputerUseStatusTool) Description() string {
	return "检查 computer-use 工具链可用状态 (平台、截图工具、输入工具是否就绪)"
}

func (t *ComputerUseStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *ComputerUseStatusTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	p := DetectPlatform()
	resp := map[string]interface{}{
		"os":                p.OS,
		"available":         p.Available,
		"screenshot_tool":   p.ScreenCaptureBin,
		"input_tool":        p.InputBin,
	}
	if !p.Available {
		resp["missing_hint"] = p.MissingHint
	}
	data, _ := json.Marshal(resp)
	return &tool.ToolResult{Content: string(data)}, nil
}

func (t *ComputerUseStatusTool) IsConcurrencySafe(_ json.RawMessage) bool            { return true }
func (t *ComputerUseStatusTool) IsReadOnly(_ json.RawMessage) bool                   { return true }
func (t *ComputerUseStatusTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// ──────────────────── RegisterAll ────────────────────

// RegisterAll 注册所有 computer-use 工具到 registry。
// 截图工具只要平台有截图命令就可用；鼠标/键盘需要输入工具。
func RegisterAll(registry *tool.Registry, cfg *SecurityConfig) {
	if cfg == nil {
		d := DefaultSecurityConfig()
		cfg = &d
	}

	p := DetectPlatform()
	registry.Register(NewComputerUseStatusTool())
	count := 1

	if p.ScreenCaptureBin != "" {
		registry.Register(NewScreenCaptureTool(*cfg))
		count++
	}

	if p.Available {
		registry.Register(NewMouseActionTool(*cfg))
		registry.Register(NewKeyboardActionTool(*cfg))
		count += 2
	}

	fmt.Printf("[computer-use] 已注册 %d 个工具 (平台: %s, 截图: %s, 输入: %s)\n",
		count, p.OS, p.ScreenCaptureBin, p.InputBin)
}
