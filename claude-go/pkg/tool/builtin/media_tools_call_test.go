package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/tool"
)

func hasChromeBin() bool {
	for _, b := range []string{"google-chrome", "chromium", "chromium-browser", "chrome"} {
		if _, err := exec.LookPath(b); err == nil {
			return true
		}
	}
	return false
}

// 触发测试: 直接调用各媒体工具的 Call(), 产出真实文件并校验 (这是模型实际走的路径)。
func TestMediaToolsCall(t *testing.T) {
	dir := t.TempDir()
	tctx := &tool.ToolContext{Cwd: dir}
	ctx := context.Background()
	call := func(to interface {
		Call(context.Context, json.RawMessage, *tool.ToolContext) (*tool.ToolResult, error)
	}, in map[string]any) *tool.ToolResult {
		raw, _ := json.Marshal(in)
		r, err := to.Call(ctx, raw, tctx)
		if err != nil {
			t.Fatalf("Call 返回 err: %v", err)
		}
		return r
	}
	mustFile := func(name string) {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Size() == 0 {
			t.Fatalf("产物缺失/为空: %s (%v)", name, err)
		}
	}

	// GeneratePPTX (可编辑模式) — 纯 Go, 无需 Chrome
	r := call(NewGeneratePPTXTool(), map[string]any{
		"output_path": "deck.pptx",
		"title":       "测试",
		"slides": []map[string]any{
			{"title": "第一页", "bullets": []string{"要点A", "要点B"}, "notes": "备注"},
			{"title": "第二页", "bullets": []string{"X"}},
		},
	})
	if r.IsError {
		t.Fatalf("GeneratePPTX 失败: %s", r.Content)
	}
	mustFile("deck.pptx")
	t.Logf("GeneratePPTX: %s", r.Content)

	if !hasChromeBin() {
		t.Log("无 Chrome, 跳过 GenerateImage/GIF/Chart")
		return
	}

	// GenerateImage (SVG → PNG)
	r = call(NewGenerateImageTool(), map[string]any{
		"output_path": "pic.png",
		"svg":         `<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100"><rect width="200" height="100" fill="#3498db"/><text x="20" y="55" fill="#fff">hi</text></svg>`,
		"width":       200, "height": 100,
	})
	if r.IsError {
		t.Fatalf("GenerateImage 失败: %s", r.Content)
	}
	mustFile("pic.png")

	// GenerateChart (ECharts option → PNG)
	r = call(NewGenerateChartTool(), map[string]any{
		"output_path": "chart.png",
		"option":      map[string]any{"xAxis": map[string]any{"type": "category", "data": []string{"A", "B", "C"}}, "yAxis": map[string]any{"type": "value"}, "series": []map[string]any{{"type": "bar", "data": []int{3, 7, 2}}}},
		"width":       640, "height": 360,
	})
	if r.IsError {
		t.Fatalf("GenerateChart 失败: %s", r.Content)
	}
	mustFile("chart.png")

	// GenerateGIF (HTML CSS动画 → GIF)
	r = call(NewGenerateGIFTool(), map[string]any{
		"output_path":  "anim.gif",
		"html":         `<!doctype html><html><head><style>.b{width:40px;height:40px;background:#e74c3c;position:absolute;animation:m 1s linear infinite}@keyframes m{from{left:0}to{left:160px}}</style></head><body><div class="b"></div></body></html>`,
		"width":        220, "height": 80, "fps": 10, "duration_sec": 1,
	})
	if r.IsError {
		t.Fatalf("GenerateGIF 失败: %s", r.Content)
	}
	mustFile("anim.gif")
	t.Logf("GenerateGIF: %s", r.Content)
}
