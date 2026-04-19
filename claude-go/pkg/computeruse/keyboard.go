package computeruse

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// KeyboardActionType 键盘操作类型。
type KeyboardActionType string

const (
	KeyboardType   KeyboardActionType = "type"   // 输入文本
	KeyboardKey    KeyboardActionType = "key"     // 按单个键
	KeyboardHotkey KeyboardActionType = "hotkey"  // 组合键
)

// KeyboardInput 键盘操作输入。
type KeyboardInput struct {
	Action    KeyboardActionType `json:"action"`
	Text      string             `json:"text,omitempty"`      // type 时的文本
	Key       string             `json:"key,omitempty"`       // key/hotkey 时的按键
	Modifiers []string           `json:"modifiers,omitempty"` // hotkey 的修饰键
}

// ExecuteKeyboard 执行键盘操作。
func ExecuteKeyboard(ctx context.Context, input KeyboardInput) error {
	p := DetectPlatform()
	if !p.Available {
		return fmt.Errorf("computer-use 不可用: %s", p.MissingHint)
	}

	switch p.OS {
	case "darwin":
		return darwinKeyboard(ctx, p.InputBin, input)
	case "linux":
		return linuxKeyboard(ctx, p.InputBin, input)
	default:
		return fmt.Errorf("不支持的平台: %s", p.OS)
	}
}

// cliclick 键盘:
//   t:text    - 输入文本
//   kp:key    - 按键 (return, tab, escape, space, delete, arrow-up/down/left/right 等)
//   kd:key    - 按下修饰键
//   ku:key    - 松开修饰键
func darwinKeyboard(ctx context.Context, bin string, input KeyboardInput) error {
	switch input.Action {
	case KeyboardType:
		if input.Text == "" {
			return fmt.Errorf("type 操作需要提供 text")
		}
		cmd := exec.CommandContext(ctx, bin, "t:"+input.Text)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("键盘输入失败: %w, output: %s", err, string(out))
		}
		return nil

	case KeyboardKey:
		key := mapKeyDarwin(input.Key)
		cmd := exec.CommandContext(ctx, bin, "kp:"+key)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("按键失败: %w, output: %s", err, string(out))
		}
		return nil

	case KeyboardHotkey:
		// 按下修饰键 → 按目标键 → 松开修饰键
		var args []string
		for _, mod := range input.Modifiers {
			args = append(args, "kd:"+mapModifierDarwin(mod))
		}
		args = append(args, "kp:"+mapKeyDarwin(input.Key))
		for i := len(input.Modifiers) - 1; i >= 0; i-- {
			args = append(args, "ku:"+mapModifierDarwin(input.Modifiers[i]))
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("组合键失败: %w, output: %s", err, string(out))
		}
		return nil

	default:
		return fmt.Errorf("未知键盘操作: %s", input.Action)
	}
}

// xdotool 键盘:
//   type --clearmodifiers "text"
//   key keyname
//   key modifier+keyname
func linuxKeyboard(ctx context.Context, bin string, input KeyboardInput) error {
	switch input.Action {
	case KeyboardType:
		if input.Text == "" {
			return fmt.Errorf("type 操作需要提供 text")
		}
		return runCmd(ctx, bin, "type", "--clearmodifiers", input.Text)

	case KeyboardKey:
		key := mapKeyLinux(input.Key)
		return runCmd(ctx, bin, "key", key)

	case KeyboardHotkey:
		// xdotool 格式: key modifier+key, 如 "key ctrl+c"
		var mods []string
		for _, mod := range input.Modifiers {
			mods = append(mods, mapModifierLinux(mod))
		}
		combo := strings.Join(append(mods, mapKeyLinux(input.Key)), "+")
		return runCmd(ctx, bin, "key", combo)

	default:
		return fmt.Errorf("未知键盘操作: %s", input.Action)
	}
}

func mapKeyDarwin(key string) string {
	m := map[string]string{
		"enter": "return", "return": "return",
		"tab": "tab", "escape": "escape", "esc": "escape",
		"space": "space", "backspace": "delete", "delete": "fwd-delete",
		"up": "arrow-up", "down": "arrow-down",
		"left": "arrow-left", "right": "arrow-right",
		"home": "home", "end": "end",
		"pageup": "page-up", "pagedown": "page-down",
		"f1": "f1", "f2": "f2", "f3": "f3", "f4": "f4",
		"f5": "f5", "f6": "f6", "f7": "f7", "f8": "f8",
	}
	if v, ok := m[strings.ToLower(key)]; ok {
		return v
	}
	return key
}

func mapModifierDarwin(mod string) string {
	m := map[string]string{
		"ctrl": "ctrl", "control": "ctrl",
		"alt": "alt", "option": "alt",
		"shift": "shift",
		"cmd": "cmd", "command": "cmd", "super": "cmd",
	}
	if v, ok := m[strings.ToLower(mod)]; ok {
		return v
	}
	return mod
}

func mapKeyLinux(key string) string {
	m := map[string]string{
		"enter": "Return", "return": "Return",
		"tab": "Tab", "escape": "Escape", "esc": "Escape",
		"space": "space", "backspace": "BackSpace", "delete": "Delete",
		"up": "Up", "down": "Down", "left": "Left", "right": "Right",
		"home": "Home", "end": "End",
		"pageup": "Prior", "pagedown": "Next",
		"f1": "F1", "f2": "F2", "f3": "F3", "f4": "F4",
	}
	if v, ok := m[strings.ToLower(key)]; ok {
		return v
	}
	return key
}

func mapModifierLinux(mod string) string {
	m := map[string]string{
		"ctrl": "ctrl", "control": "ctrl",
		"alt": "alt", "option": "alt",
		"shift": "shift",
		"cmd": "super", "command": "super", "super": "super",
	}
	if v, ok := m[strings.ToLower(mod)]; ok {
		return v
	}
	return mod
}
