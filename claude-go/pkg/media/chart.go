// 图表渲染: 用 Apache ECharts 把数据图表 option(JSON) 渲染为图片。
//
// 与 mermaid 同思路 (HTML+JS→截图), 但 echarts 覆盖柱/折/饼/散点/K线/地图/桑基等常见数据图。
// 内嵌 echarts.min.js, 离线/被代理挡时仍可渲染; 资源缺失时回退 CDN。
package media

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

//go:embed assets/echarts.min.js
var echartsJS []byte

var (
	echartsPathOnce sync.Once
	echartsPath     string
)

func echartsAssetPath() string {
	echartsPathOnce.Do(func() {
		if len(echartsJS) < 1000 {
			return
		}
		dir := filepath.Join(os.TempDir(), "claude-go-assets")
		_ = os.MkdirAll(dir, 0755)
		p := filepath.Join(dir, "echarts.min.js")
		if st, err := os.Stat(p); err != nil || st.Size() != int64(len(echartsJS)) {
			if err := os.WriteFile(p, echartsJS, 0644); err != nil {
				return
			}
		}
		echartsPath = p
	})
	return echartsPath
}

func echartsScriptTag() string {
	if p := echartsAssetPath(); p != "" {
		return `<script src="file://` + filepath.ToSlash(p) + `"></script>`
	}
	return `<script src="https://cdn.jsdelivr.net/npm/echarts@5/dist/echarts.min.js"></script>`
}

// BuildEChartsHTML 构造一个加载 ECharts 并 setOption(option) 的完整 HTML 文档。
// optionJSON 是一个合法的 ECharts option 对象的 JSON 字符串。关闭动画以便确定性截图。
func BuildEChartsHTML(optionJSON string, width, height int) string {
	if width <= 0 {
		width = 1000
	}
	if height <= 0 {
		height = 600
	}
	return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8">
%s
<style>html,body{margin:0;padding:0;background:#fff}#chart{width:%dpx;height:%dpx}</style>
</head><body>
<div id="chart"></div>
<script>
(function(){
  var opt = %s;
  if (opt && typeof opt === 'object') { opt.animation = false; }
  var c = echarts.init(document.getElementById('chart'), null, {renderer:'canvas'});
  c.setOption(opt);
})();
</script>
</body></html>`, echartsScriptTag(), width, height, optionJSON)
}
