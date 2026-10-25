package computeruse

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ──────────────────── ScreenCapture Tool ────────────────────

type ScreenCaptureTool struct {
	limiter *RateLimiter
}

// NewScreenCaptureTool 创建截图工具, 并为其装配独立的限速器。
func NewScreenCaptureTool(cfg SecurityConfig) *ScreenCaptureTool {
	return &ScreenCaptureTool{limiter: NewRateLimiter(cfg)}
}

// Name 返回工具名 "ScreenCapture"。
func (t *ScreenCaptureTool) Name() string { return "ScreenCapture" }

// Description 返回给模型看的说明: 截屏并以 base64 PNG 返回, 供 AI 观察屏幕内容。
func (t *ScreenCaptureTool) Description() string {
	return "截取当前屏幕或指定区域的截图。返回 base64 编码的 PNG 图片数据和尺寸。用于 AI 观察屏幕内容。"
}

// InputSchema 声明入参: 可选 region (x/y/width/height) 与缩放比例 scale (0.25~1.0)。
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

// Call 执行截图: 先过限速闸, 再按 scale 降采样, 最后以 base64 PNG 返回。
//
// 算法解读：
//
//	原理：把"截屏 → 可选降采样 → base64 编码"收成一次工具调用; 降采样是为了把送给
//	      模型的图片压小 —— 视觉 token 与像素数正相关, 全尺寸截图是上下文里最贵的一项。
//	关键不变量：scale 必须落在 (0, 1.0] —— 越界 (含 <=0 与非数字) 一律回落到默认 0.5,
//	      否则缩放因子会把图放大或缩成 0 像素。另有一条更隐蔽的不变量: scale 只作用于
//	      **返回的图片**, 不改变鼠标/键盘事件使用的坐标系 —— 模型看到的是缩略图, 但它
//	      报出的坐标必须是真实屏幕坐标, 这一步换算只能由模型侧完成。
//	代价与边界：返回的 width/height 恒为 0 (见 CaptureScreen 里的"此处简化"), 所以模型
//	      无法据返回尺寸把图片坐标反推回屏幕坐标; 限速超限时返回的是 IsError 结果而不是
//	      Go error。
//	设计权衡：默认 0.5 而非 1.0 —— 用少量细节换一半体积; 需要精确定位时由调用方显式传 scale。
func (t *ScreenCaptureTool) Call(ctx context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	if err := t.limiter.Allow(); err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	var in screenCaptureInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: "参数解析失败: " + err.Error(), IsError: true}, nil
	}

	// 越界的 scale 不报错, 静默回落到 0.5 —— 截图是"尽量给一张"的语义, 为一个缩放
	// 参数让整次调用失败不划算。
	scale := 0.5
	if in.Scale != nil && *in.Scale > 0 && *in.Scale <= 1.0 {
		scale = *in.Scale
	}

	result, err := CaptureScreen(ctx, in.Region, scale)
	if err != nil {
		return &tool.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	// width/height 透传的是 CaptureScreen 的返回值 —— 当前实现恒为 0 (见 capture.go),
	// 所以模型拿到的宽高字段不代表图片真实尺寸, 定位要靠图片本身。
	respJSON, _ := json.Marshal(map[string]interface{}{
		"status":     "success",
		"base64_png": result.Base64PNG,
		"width":      result.Width,
		"height":     result.Height,
	})
	return &tool.ToolResult{Content: string(respJSON)}, nil
}

// IsConcurrencySafe 恒为 true —— 截屏只读且无共享状态。
func (t *ScreenCaptureTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// IsReadOnly 恒为 true —— 截屏不改变屏幕内容。
func (t *ScreenCaptureTool) IsReadOnly(_ json.RawMessage) bool { return true }

// CheckPermissions 除 bypass 模式外一律要求用户确认 (屏幕内容可能含敏感信息)。
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

// NewMouseActionTool 创建鼠标操作工具, 并为其装配独立的限速器。
func NewMouseActionTool(cfg SecurityConfig) *MouseActionTool {
	return &MouseActionTool{limiter: NewRateLimiter(cfg)}
}

// Name 返回工具名 "MouseAction"。
func (t *MouseActionTool) Name() string { return "MouseAction" }

// Description 返回给模型看的说明: 移动 / 点击 / 双击 / 右键 / 拖拽五种鼠标动作。
func (t *MouseActionTool) Description() string {
	return "执行鼠标操作: 移动(move)、点击(click)、双击(double_click)、右键(right_click)、拖拽(drag)。"
}

// InputSchema 声明入参: action + 目标坐标 (drag 另需 end_x/end_y) + 是否事后截图。
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

// Call 执行鼠标动作, 默认在动作后追加一次截图, 让模型能闭环确认落点。
//
// 算法解读：
//
//	原理：把结构化动作交给平台相关的 ExecuteMouse, 由它翻译成 cliclick (darwin) 或
//	      xdotool (linux) 的命令; 动作成功后可选再截一次屏, 把结果一并回给模型。
//	关键不变量：x/y (以及 drag 的 end_x/end_y) 是**真实屏幕坐标** —— 与截图返回的
//	      缩放图无关。模型若按 0.5 缩放图上的像素去报坐标, 落点会偏一倍, 而这条换算
//	      只能由模型侧完成 (它手里只有缩略图, 且返回的 width/height 恒为 0)。
//	代价与边界：事后截图失败被静默吞掉 —— 只丢 screenshot 字段, 动作本身已经生效;
//	      这是刻意的 (动作成败不该由观测手段决定), 但模型会因此看不到结果。
//	设计权衡：screenshot_after 默认 true —— 多一次截图换取模型不必额外发一次
//	      ScreenCapture; 连续高频操作时可显式关掉以省上下文。
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
		"status": "success",
		"action": in.Action,
		"x":      in.X,
		"y":      in.Y,
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

// IsConcurrencySafe 恒为 false —— 鼠标是全局单例资源, 并发操作会互相插队。
func (t *MouseActionTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// IsReadOnly 恒为 false —— 鼠标操作会改变前台应用的界面状态。
func (t *MouseActionTool) IsReadOnly(_ json.RawMessage) bool { return false }

// CheckPermissions 除 bypass 模式外一律要求用户确认。
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

// NewKeyboardActionTool 创建键盘操作工具, 并为其装配独立的限速器。
func NewKeyboardActionTool(cfg SecurityConfig) *KeyboardActionTool {
	return &KeyboardActionTool{limiter: NewRateLimiter(cfg)}
}

// Name 返回工具名 "KeyboardAction"。
func (t *KeyboardActionTool) Name() string { return "KeyboardAction" }

// Description 返回给模型看的说明: 输入文本 / 按单键 / 组合键三类键盘动作。
func (t *KeyboardActionTool) Description() string {
	return "执行键盘操作: 输入文本(type)、按键(key)、组合键(hotkey)。支持 Enter/Tab/Escape 等特殊键和 Ctrl/Cmd+C 等组合键。"
}

// InputSchema 声明入参: action + text/key/modifiers (按 action 取用) + 是否事后截图。
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

// Call 执行键盘动作 (type/key/hotkey), 默认在动作后追加一次截图。
//
// 算法解读：
//
//	原理：按 action 分派到"输入文本 / 按单键 / 组合键"三条路径, 交给 ExecuteKeyboard
//	      翻译成平台输入命令; 动作后默认再截一次屏, 让模型确认文本落到了哪个焦点。
//	关键不变量：action=type 时必须有 text —— 本层不预检, 空文本由 ExecuteKeyboard
//	      以错误收场, 于是"什么都没输入"不会被当成成功。
//	代价与边界：回包只回显 action, **不回显实际输入的文本** —— 长文本被输入法吞掉时
//	      调用方从回包看不出来, 唯一线索是事后截图; 中文等非 ASCII 文本依赖输入法,
//	      逐字符模拟可能丢字。
//	设计权衡：与 MouseAction 共用 screenshot_after 约定, 保持两个工具的反馈方式一致。
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

// IsConcurrencySafe 恒为 false —— 键盘焦点是全局单例, 并发输入会互相穿插。
func (t *KeyboardActionTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// IsReadOnly 恒为 false —— 键盘输入会改变前台应用状态 (可能触发提交/删除等)。
func (t *KeyboardActionTool) IsReadOnly(_ json.RawMessage) bool { return false }

// CheckPermissions 除 bypass 模式外一律要求用户确认。
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

// NewComputerUseStatusTool 创建状态查询工具 (无状态, 不需要限速器)。
func NewComputerUseStatusTool() *ComputerUseStatusTool { return &ComputerUseStatusTool{} }

// Name 返回工具名 "ComputerUseStatus"。
func (t *ComputerUseStatusTool) Name() string { return "ComputerUseStatus" }

// Description 返回给模型看的说明: 检查 computer-use 工具链是否就绪。
func (t *ComputerUseStatusTool) Description() string {
	return "检查 computer-use 工具链可用状态 (平台、截图工具、输入工具是否就绪)"
}

// InputSchema 声明空入参对象。
func (t *ComputerUseStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// Call 汇报本机 computer-use 链路的可用性: 平台、截图命令、输入命令, 不可用时附缺失提示。
func (t *ComputerUseStatusTool) Call(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	p := DetectPlatform()
	resp := map[string]interface{}{
		"os":              p.OS,
		"available":       p.Available,
		"screenshot_tool": p.ScreenCaptureBin,
		"input_tool":      p.InputBin,
	}
	if !p.Available {
		resp["missing_hint"] = p.MissingHint
	}
	data, _ := json.Marshal(resp)
	return &tool.ToolResult{Content: string(data)}, nil
}

// IsConcurrencySafe 恒为 true —— 只读探测, 无共享状态。
func (t *ComputerUseStatusTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// IsReadOnly 恒为 true —— 只探测工具链, 不产生副作用。
func (t *ComputerUseStatusTool) IsReadOnly(_ json.RawMessage) bool { return true }

// CheckPermissions 返回 nil: 状态探测不需要额外授权。
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

	// 必须写 stderr: 这行是启动期诊断, 不是命令输出。写 stdout 会污染
	// `--output-format json` (实测 run.json 第一行就是它), 让"整段 stdout 就是一个
	// JSON"这个契约不成立 —— 现有解析器靠逐行回退才没炸, 但下游用 jq 读整份文件
	// 就会失败。
	fmt.Fprintf(os.Stderr, "[computer-use] 已注册 %d 个工具 (平台: %s, 截图: %s, 输入: %s)\n",
		count, p.OS, p.ScreenCaptureBin, p.InputBin)
}
