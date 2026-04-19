# Computer Use 底层工具设计文档

> claude-go 新增底层工具: 让 AI Agent 通过截图→分析→操作的闭环控制计算机

## 1. 背景与目标

### 1.1 行业现状

| 模型/产品 | 方案 | 特点 |
|-----------|------|------|
| **Anthropic Computer Use** | tool_use 接口 + 原子动作 | 截图→模型分析→结构化动作→新截图，闭环 |
| **OpenAI Operator/CUA** | Responses API + 浏览器代理 | 任务→浏览器操作→安全护栏 |
| **Google Project Mariner** | Gemini + 浏览器代理 | 多步网页任务，高风险需确认 |
| **智谱 AutoGLM / CogAgent** | 视觉 GUI Agent | 屏幕理解→规划→触控/键鼠 |
| **Qwen BrowserQwen** | 浏览器扩展 + Workstation | 页面上下文采集与模型推理解耦 |

### 1.2 学术论文

| 论文 | 会议 | 核心贡献 |
|------|------|----------|
| **OSWorld** | NeurIPS 2024 | 桌面+Web 开放式任务基准，真实 OS 环境 |
| **SeeAct** | ICML 2024 | 多模态 Web grounding (HTML/DOM + 视觉) |
| **WebVoyager** | ACL 2024 | 端到端网页导航/信息查找，跨站点长程交互 |
| **CogAgent** | — | 视觉 GUI Agent，高分辨率 UI 理解 |
| **UFO/AppAgent/ScreenAgent** | — | 桌面 GUI 自动化: 规划器+执行器+观测 |

### 1.3 设计目标

1. **原子化动作**: 截图、鼠标点击、键盘输入、滚动、等待 等基本原语
2. **多平台支持**: macOS (screencapture + cliclick) / Linux (scrot + xdotool)
3. **安全优先**: 权限分级、操作确认、敏感信息过滤
4. **可观测性**: 每步返回截图+结果，便于 Agent 自我纠错
5. **与现有工具系统集成**: 作为 builtin tool 注册，复用权限框架

## 2. 技术方案

### 2.1 架构分层

```
┌─────────────────────────────────────────────┐
│             LLM Agent (tool_use)            │
├─────────────────────────────────────────────┤
│           Computer Use Tools (4个)           │
│  ScreenCapture │ MouseAction │ KeyboardAction│ ScreenSearch │
├─────────────────────────────────────────────┤
│            Platform Adapter                  │
│  macOS: screencapture + cliclick             │
│  Linux: scrot/grim + xdotool                │
│  Browser: chromedp (CDP)                     │
├─────────────────────────────────────────────┤
│          Security / Permission               │
│  权限检查 │ 速率限制 │ 敏感区域过滤         │
└─────────────────────────────────────────────┘
```

### 2.2 工具定义

#### 2.2.1 ScreenCapture (截屏)

```json
{
  "name": "ScreenCapture",
  "description": "截取当前屏幕或指定区域的截图，返回 base64 编码的 PNG",
  "input_schema": {
    "type": "object",
    "properties": {
      "region": {
        "type": "object",
        "description": "截取区域 (可选，不指定=全屏)",
        "properties": {
          "x": {"type": "integer"},
          "y": {"type": "integer"},
          "width": {"type": "integer"},
          "height": {"type": "integer"}
        }
      },
      "scale": {
        "type": "number",
        "description": "缩放比例 (0.25~1.0，降低发送给模型的图片大小)",
        "default": 0.5
      }
    }
  }
}
```

#### 2.2.2 MouseAction (鼠标操作)

```json
{
  "name": "MouseAction",
  "description": "执行鼠标操作: 移动、点击、双击、右键、拖拽",
  "input_schema": {
    "type": "object",
    "properties": {
      "action": {
        "type": "string",
        "enum": ["move", "click", "double_click", "right_click", "drag"]
      },
      "x": {"type": "integer", "description": "目标 X 坐标"},
      "y": {"type": "integer", "description": "目标 Y 坐标"},
      "end_x": {"type": "integer", "description": "拖拽终点 X (仅 drag)"},
      "end_y": {"type": "integer", "description": "拖拽终点 Y (仅 drag)"},
      "screenshot_after": {"type": "boolean", "default": true}
    },
    "required": ["action", "x", "y"]
  }
}
```

#### 2.2.3 KeyboardAction (键盘操作)

```json
{
  "name": "KeyboardAction",
  "description": "执行键盘操作: 输入文本、按键组合",
  "input_schema": {
    "type": "object",
    "properties": {
      "action": {
        "type": "string",
        "enum": ["type", "key", "hotkey"]
      },
      "text": {"type": "string", "description": "要输入的文本 (action=type)"},
      "key": {"type": "string", "description": "按键名 (action=key/hotkey)"},
      "modifiers": {
        "type": "array",
        "items": {"type": "string", "enum": ["ctrl", "alt", "shift", "cmd", "super"]},
        "description": "修饰键 (action=hotkey)"
      },
      "screenshot_after": {"type": "boolean", "default": true}
    },
    "required": ["action"]
  }
}
```

#### 2.2.4 ScreenSearch (屏幕搜索)

```json
{
  "name": "ScreenSearch",
  "description": "在当前截图中搜索文本或 UI 元素的位置坐标",
  "input_schema": {
    "type": "object",
    "properties": {
      "query": {"type": "string", "description": "要查找的文本或元素描述"},
      "method": {
        "type": "string",
        "enum": ["ocr", "vision"],
        "default": "vision",
        "description": "搜索方式: ocr=文字识别定位, vision=视觉模型定位"
      }
    },
    "required": ["query"]
  }
}
```

### 2.3 平台适配器

| 能力 | macOS | Linux |
|------|-------|-------|
| **截图** | `screencapture -x -t png` | `scrot` / `grim` (Wayland) |
| **鼠标** | `cliclick` | `xdotool` / `ydotool` |
| **键盘** | `cliclick` + `osascript` | `xdotool` / `ydotool` |
| **坐标系** | Retina 需 / ScaleFactor | DPI 一致 |

### 2.4 安全机制

1. **权限分级**:
   - `ScreenCapture`: 需要 `computer_use:read` 权限
   - `MouseAction` / `KeyboardAction`: 需要 `computer_use:write` 权限
   - 默认 `ask` 模式: 每次操作需用户确认

2. **操作限速**:
   - 最大 60 次/分钟 (防止失控循环)
   - 单次 session 最多 200 步

3. **敏感区域保护**:
   - 密码输入框检测 (OCR + 上下文)
   - 银行/支付类应用窗口拒绝操作

4. **截图脱敏**:
   - 默认不持久化原图
   - 可配置模糊特定区域

## 3. 实现路径

### 3.1 包结构

```
pkg/computeruse/
├── capture.go      # 截图 (平台适配)
├── mouse.go        # 鼠标控制
├── keyboard.go     # 键盘控制
├── search.go       # 屏幕搜索
├── platform.go     # 平台检测与命令封装
├── security.go     # 安全策略
└── tools.go        # Tool 接口注册 (4个工具)
```

### 3.2 依赖

- **零外部 Go 依赖**: 通过 `os/exec` 调用系统命令
- **可选**: `robotgo` (如果需要更精细控制)
- **macOS**: `screencapture` (系统自带), `cliclick` (brew install)
- **Linux**: `scrot` / `grim`, `xdotool` / `ydotool`

### 3.3 工具链检测

启动时自动检测可用工具:
- 如果缺少必要工具，`CheckPermissions` 返回 deny
- 友好的错误提示: "请安装 cliclick: brew install cliclick"

## 4. 与现有系统集成

### 4.1 注册为 builtin tool

在 `pkg/tool/builtin/register.go` 中注册 4 个新工具。

### 4.2 Agent 使用流程

```
Agent: 我需要查看屏幕上的内容
→ tool_use: ScreenCapture {}
← base64 PNG 截图

Agent: 我看到搜索框在左上角，我要点击它
→ tool_use: MouseAction {action: "click", x: 200, y: 50}
← 操作成功 + 新截图

Agent: 输入搜索关键词
→ tool_use: KeyboardAction {action: "type", text: "MINIMAX-W"}
← 操作成功 + 新截图
```

### 4.3 配置

```json
{
  "computerUse": {
    "enabled": true,
    "permissionMode": "ask",
    "maxActionsPerMinute": 60,
    "maxActionsPerSession": 200,
    "screenshotScale": 0.5,
    "sensitiveApps": ["1Password", "Keychain", "银行"]
  }
}
```

## 5. 实现优先级

| 阶段 | 内容 | 时间 |
|------|------|------|
| **P0** | ScreenCapture + MouseAction + KeyboardAction | 当前 |
| **P1** | ScreenSearch (OCR) + 安全策略 | 后续 |
| **P2** | robotgo 集成 + Windows 支持 | 可选 |
