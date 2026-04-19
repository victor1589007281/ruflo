package computeruse

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

// MouseActionType 鼠标操作类型。
type MouseActionType string

const (
	MouseMove       MouseActionType = "move"
	MouseClick      MouseActionType = "click"
	MouseDoubleClick MouseActionType = "double_click"
	MouseRightClick MouseActionType = "right_click"
	MouseDrag       MouseActionType = "drag"
)

// MouseInput 鼠标操作输入。
type MouseInput struct {
	Action MouseActionType `json:"action"`
	X      int             `json:"x"`
	Y      int             `json:"y"`
	EndX   int             `json:"end_x,omitempty"` // drag 终点
	EndY   int             `json:"end_y,omitempty"`
}

// ExecuteMouse 执行鼠标操作。
func ExecuteMouse(ctx context.Context, input MouseInput) error {
	p := DetectPlatform()
	if !p.Available {
		return fmt.Errorf("computer-use 不可用: %s", p.MissingHint)
	}

	switch p.OS {
	case "darwin":
		return darwinMouse(ctx, p.InputBin, input)
	case "linux":
		return linuxMouse(ctx, p.InputBin, input)
	default:
		return fmt.Errorf("不支持的平台: %s", p.OS)
	}
}

// cliclick 命令格式:
//   m:x,y   - 移动
//   c:x,y   - 点击
//   dc:x,y  - 双击
//   rc:x,y  - 右键点击
//   dd:x,y  - 按下 (drag start)
//   du:x,y  - 松开 (drag end)
func darwinMouse(ctx context.Context, bin string, input MouseInput) error {
	coords := strconv.Itoa(input.X) + "," + strconv.Itoa(input.Y)
	var args []string

	switch input.Action {
	case MouseMove:
		args = []string{"m:" + coords}
	case MouseClick:
		args = []string{"c:" + coords}
	case MouseDoubleClick:
		args = []string{"dc:" + coords}
	case MouseRightClick:
		args = []string{"rc:" + coords}
	case MouseDrag:
		endCoords := strconv.Itoa(input.EndX) + "," + strconv.Itoa(input.EndY)
		args = []string{"dd:" + coords, "du:" + endCoords}
	default:
		return fmt.Errorf("未知鼠标操作: %s", input.Action)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("鼠标操作失败: %w, output: %s", err, string(out))
	}
	return nil
}

// xdotool 命令:
//   mousemove x y
//   click 1 (左) / 3 (右)
//   mousemove + click + click (双击)
//   mousedown 1 → mousemove → mouseup 1 (拖拽)
func linuxMouse(ctx context.Context, bin string, input MouseInput) error {
	xs, ys := strconv.Itoa(input.X), strconv.Itoa(input.Y)
	isXdotool := true // xdotool vs ydotool

	switch input.Action {
	case MouseMove:
		return runCmd(ctx, bin, "mousemove", "--", xs, ys)

	case MouseClick:
		if err := runCmd(ctx, bin, "mousemove", "--", xs, ys); err != nil {
			return err
		}
		if isXdotool {
			return runCmd(ctx, bin, "click", "1")
		}
		return runCmd(ctx, bin, "click", "0xC0001", "0", "0")

	case MouseDoubleClick:
		if err := runCmd(ctx, bin, "mousemove", "--", xs, ys); err != nil {
			return err
		}
		if err := runCmd(ctx, bin, "click", "--repeat", "2", "1"); err != nil {
			return err
		}
		return nil

	case MouseRightClick:
		if err := runCmd(ctx, bin, "mousemove", "--", xs, ys); err != nil {
			return err
		}
		return runCmd(ctx, bin, "click", "3")

	case MouseDrag:
		exs, eys := strconv.Itoa(input.EndX), strconv.Itoa(input.EndY)
		if err := runCmd(ctx, bin, "mousemove", "--", xs, ys); err != nil {
			return err
		}
		if err := runCmd(ctx, bin, "mousedown", "1"); err != nil {
			return err
		}
		if err := runCmd(ctx, bin, "mousemove", "--", exs, eys); err != nil {
			return err
		}
		return runCmd(ctx, bin, "mouseup", "1")

	default:
		return fmt.Errorf("未知鼠标操作: %s", input.Action)
	}
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v 失败: %w, output: %s", name, args, err, string(out))
	}
	return nil
}
