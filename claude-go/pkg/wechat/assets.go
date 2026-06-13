package wechat

import (
	_ "embed"
	"os"
	"path/filepath"
	"sync"
)

// 内嵌 mermaid.min.js, 消除对 cdn.jsdelivr.net 的运行时依赖 (离线/被代理挡时仍可渲染)。
// 资源缺失(空)时自动回退到 CDN。
//
//go:embed assets/mermaid.min.js
var mermaidJS []byte

var (
	mermaidPathOnce sync.Once
	mermaidPath     string
)

// mermaidAssetPath 把内嵌的 mermaid.min.js 落地到缓存文件并返回绝对路径; 内嵌为空时返回 ""。
// 用 file:// 引用而非内联 <script>, 避免 minified JS 中的 </script>/正则把脚本标签截断。
func mermaidAssetPath() string {
	mermaidPathOnce.Do(func() {
		if len(mermaidJS) < 1000 {
			return
		}
		dir := filepath.Join(os.TempDir(), "claude-go-assets")
		_ = os.MkdirAll(dir, 0755)
		p := filepath.Join(dir, "mermaid.min.js")
		if st, err := os.Stat(p); err != nil || st.Size() != int64(len(mermaidJS)) {
			if err := os.WriteFile(p, mermaidJS, 0644); err != nil {
				return
			}
		}
		mermaidPath = p
	})
	return mermaidPath
}

// mermaidScriptTag 返回加载 mermaid 的 <script> 标签: 优先本地内嵌, 回退 CDN。
func mermaidScriptTag() string {
	if p := mermaidAssetPath(); p != "" {
		return `<script src="file://` + filepath.ToSlash(p) + `"></script>`
	}
	return `<script src="https://cdn.jsdelivr.net/npm/mermaid@10/dist/mermaid.min.js"></script>`
}
