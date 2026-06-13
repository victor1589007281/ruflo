package media

import (
	"archive/zip"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildEditablePPTX 验证可编辑 PPTX: 产物是合法 zip, 含必要 OOXML 部件, 且标题/要点文本
// 以真实可编辑文本(<a:t>)写入(而非整页图), 特殊字符被正确转义。
func TestBuildEditablePPTX(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "deck.pptx")
	deck := EditableDeck{
		Title: "测试演示",
		Slides: []EditableSlide{
			{Title: "第一页 & <标题>", Bullets: []string{"要点一", "含<特殊>字符 & 引号\""}, Notes: "这是备注"},
			{Title: "第二页", Subtitle: "副标题", Bullets: []string{"A", "B"}},
		},
	}
	if err := BuildEditablePPTX(out, deck); err != nil {
		t.Fatalf("BuildEditablePPTX 失败: %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("产物不是合法 zip: %v", err)
	}
	defer zr.Close()

	want := map[string]bool{
		"[Content_Types].xml":                false,
		"ppt/presentation.xml":               false,
		"ppt/slides/slide1.xml":              false,
		"ppt/slides/slide2.xml":              false,
		"ppt/slideMasters/slideMaster1.xml":  false,
		"ppt/slideLayouts/slideLayout1.xml":  false,
	}
	var slide1 string
	for _, f := range zr.File {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
		if f.Name == "ppt/slides/slide1.xml" {
			rc, _ := f.Open()
			b := make([]byte, f.UncompressedSize64)
			n, _ := rc.Read(b)
			slide1 = string(b[:n])
			rc.Close()
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("缺少 OOXML 部件: %s", name)
		}
	}
	// 文本应作为真实可编辑文本写入, 且特殊字符转义
	if !strings.Contains(slide1, "<a:t>") {
		t.Error("slide1 未包含可编辑文本 <a:t>")
	}
	if !strings.Contains(slide1, "第一页 &amp; &lt;标题&gt;") {
		t.Errorf("标题未正确转义写入, slide1=%.200s", slide1)
	}
	if !strings.Contains(slide1, "要点一") {
		t.Error("正文要点未写入")
	}
}

// TestBuildEChartsHTML 验证图表 HTML 构造: 含 echarts 脚本、传入 option、关闭动画。
func TestBuildEChartsHTML(t *testing.T) {
	html := BuildEChartsHTML(`{"series":[{"type":"bar","data":[1,2,3]}]}`, 800, 400)
	for _, sub := range []string{"echarts", "setOption", `"type":"bar"`, "animation = false", "id=\"chart\""} {
		if !strings.Contains(html, sub) {
			t.Errorf("ECharts HTML 缺少 %q", sub)
		}
	}
}
