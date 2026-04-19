package computeruse

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Region 截图区域 (为空表示全屏)。
type Region struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// CaptureResult 截图结果。
type CaptureResult struct {
	Base64PNG string `json:"base64_png"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// CaptureScreen 截取屏幕。scale: 0.25~1.0 用于缩小图片。
func CaptureScreen(ctx context.Context, region *Region, scale float64) (*CaptureResult, error) {
	p := DetectPlatform()
	if p.ScreenCaptureBin == "" {
		return nil, fmt.Errorf("截图工具不可用 (平台: %s)", p.OS)
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("cu-capture-%d.png", os.Getpid()))
	defer os.Remove(tmpFile)

	var cmd *exec.Cmd
	switch p.OS {
	case "darwin":
		cmd = buildDarwinCapture(ctx, tmpFile, region)
	case "linux":
		cmd = buildLinuxCapture(ctx, p.ScreenCaptureBin, tmpFile, region)
	default:
		return nil, fmt.Errorf("不支持的平台: %s", p.OS)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("截图失败: %w, output: %s", err, string(out))
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return nil, fmt.Errorf("读取截图失败: %w", err)
	}

	if scale > 0 && scale < 1.0 {
		data, err = scaleDown(data, scale)
		if err != nil {
			return nil, fmt.Errorf("缩放失败: %w", err)
		}
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	return &CaptureResult{
		Base64PNG: b64,
		Width:     0, // 实际宽高可从 PNG 头解析，此处简化
		Height:    0,
	}, nil
}

func buildDarwinCapture(ctx context.Context, outPath string, region *Region) *exec.Cmd {
	args := []string{"-x", "-t", "png"}
	if region != nil && region.Width > 0 && region.Height > 0 {
		// -R x,y,w,h 截取指定区域
		args = append(args, "-R",
			fmt.Sprintf("%d,%d,%d,%d", region.X, region.Y, region.Width, region.Height))
	}
	args = append(args, outPath)
	return exec.CommandContext(ctx, "screencapture", args...)
}

func buildLinuxCapture(ctx context.Context, bin, outPath string, region *Region) *exec.Cmd {
	base := filepath.Base(bin)
	switch {
	case strings.Contains(base, "grim"):
		args := []string{"-t", "png"}
		if region != nil && region.Width > 0 && region.Height > 0 {
			args = append(args, "-g",
				fmt.Sprintf("%d,%d %dx%d", region.X, region.Y, region.Width, region.Height))
		}
		args = append(args, outPath)
		return exec.CommandContext(ctx, bin, args...)

	case strings.Contains(base, "scrot"):
		args := []string{}
		if region != nil && region.Width > 0 && region.Height > 0 {
			args = append(args, "-a",
				fmt.Sprintf("%d,%d,%d,%d", region.X, region.Y,
					region.X+region.Width, region.Y+region.Height))
		}
		args = append(args, outPath)
		return exec.CommandContext(ctx, bin, args...)

	default: // import (ImageMagick)
		args := []string{"-window", "root"}
		if region != nil && region.Width > 0 && region.Height > 0 {
			args = append(args, "-crop",
				strconv.Itoa(region.Width)+"x"+strconv.Itoa(region.Height)+
					"+"+strconv.Itoa(region.X)+"+"+strconv.Itoa(region.Y))
		}
		args = append(args, outPath)
		return exec.CommandContext(ctx, bin, args...)
	}
}

// scaleDown 用最近邻采样缩小 PNG (纯标准库，无外部依赖)。
func scaleDown(pngData []byte, scale float64) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, err
	}

	bounds := src.Bounds()
	newW := int(float64(bounds.Dx()) * scale)
	newH := int(float64(bounds.Dy()) * scale)
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	for y := 0; y < newH; y++ {
		srcY := bounds.Min.Y + int(float64(y)/scale)
		for x := 0; x < newW; x++ {
			srcX := bounds.Min.X + int(float64(x)/scale)
			r, g, b, a := src.At(srcX, srcY).RGBA()
			dst.Set(x, y, color.RGBA{
				R: uint8(r >> 8), G: uint8(g >> 8),
				B: uint8(b >> 8), A: uint8(a >> 8),
			})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
