package wechat

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestMermaidLocalAsset 验证 mermaid.min.js 已本地内嵌, 且渲染走本地 file:// 资源(无需 CDN)。
func TestMermaidLocalAsset(t *testing.T) {
	if len(mermaidJS) < 1000 {
		t.Fatal("mermaid.min.js 未内嵌 (go:embed 资源缺失)")
	}
	if p := mermaidAssetPath(); p == "" {
		t.Fatal("本地资源落地失败, mermaidAssetPath 为空")
	}
	tag := mermaidScriptTag()
	if bytes.Contains([]byte(tag), []byte("jsdelivr")) {
		t.Errorf("仍在用 CDN 而非本地内嵌: %s", tag)
	}
}

// TestRenderMermaidPNG 真实渲染一张 mermaid 图 (需 Chrome), 验证本地资源链路端到端可用。
func TestRenderMermaidPNG(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过浏览器集成测试")
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("无 Chrome, 跳过")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	png, err := RenderMermaidPNG(ctx, chrome, "graph LR\n  A[开始]-->B[结束]", 60*time.Second)
	if err != nil {
		t.Fatalf("mermaid 渲染失败 (本地资源链路): %v", err)
	}
	if len(png) == 0 || !bytes.HasPrefix(png, []byte{0x89, 0x50, 0x4E, 0x47}) {
		t.Fatalf("产物不是有效 PNG (%d bytes)", len(png))
	}
	t.Logf("mermaid PNG ok: %d bytes", len(png))
}
