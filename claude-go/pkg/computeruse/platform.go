// Package computeruse 实现跨平台的计算机控制工具 (截屏/鼠标/键盘)。
// 通过 os/exec 调用系统工具，零外部 Go 依赖。
package computeruse

import (
	"fmt"
	"os/exec"
	"runtime"
	"sync"
)

// Platform 当前平台检测结果。
type Platform struct {
	OS string // "darwin", "linux", "windows"

	// 截图工具
	ScreenCaptureBin string // macOS: screencapture, Linux: scrot/grim/import
	// 鼠标/键盘工具
	InputBin string // macOS: cliclick, Linux: xdotool/ydotool

	Available bool   // 工具链是否完整
	MissingHint string // 缺少工具时的安装提示
}

var (
	platformOnce   sync.Once
	detectedPlatform *Platform
)

// DetectPlatform 检测当前平台并确认工具链可用性。
func DetectPlatform() *Platform {
	platformOnce.Do(func() {
		detectedPlatform = detectPlatformImpl()
	})
	return detectedPlatform
}

// detectPlatformImpl 按 runtime.GOOS 分派: 探测各平台截图/输入工具的实际路径, 任一缺失即标记不可用并给安装提示。
//
// 返回的是**工具绝对路径**而非裸命令名 (Linux 侧的 LookPath 结果直接写回字段), 因为调用方要按
// 路径 exec; macOS 的截图工具 screencapture 是系统自带, 故不探测直接写名字。
func detectPlatformImpl() *Platform {
	p := &Platform{OS: runtime.GOOS}

	switch p.OS {
	case "darwin":
		p.ScreenCaptureBin = "screencapture"
		// cliclick 用于鼠标/键盘控制
		if path, err := exec.LookPath("cliclick"); err == nil {
			p.InputBin = path
			p.Available = true
		} else {
			p.Available = false
			p.MissingHint = "请安装 cliclick: brew install cliclick"
		}

	case "linux":
		// 截图: 优先 grim (Wayland) → scrot (X11) → import (ImageMagick)
		for _, bin := range []string{"grim", "scrot", "import"} {
			if path, err := exec.LookPath(bin); err == nil {
				p.ScreenCaptureBin = path
				break
			}
		}
		// 输入: 优先 xdotool (X11) → ydotool (Wayland)
		for _, bin := range []string{"xdotool", "ydotool"} {
			if path, err := exec.LookPath(bin); err == nil {
				p.InputBin = path
				break
			}
		}
		if p.ScreenCaptureBin == "" || p.InputBin == "" {
			p.Available = false
			p.MissingHint = "请安装: sudo apt-get install scrot xdotool"
		} else {
			p.Available = true
		}

	default:
		p.Available = false
		p.MissingHint = fmt.Sprintf("computer-use 暂不支持 %s 平台", p.OS)
	}

	return p
}

// ResetPlatform 重置缓存 (测试用)。
func ResetPlatform() {
	platformOnce = sync.Once{}
	detectedPlatform = nil
}
