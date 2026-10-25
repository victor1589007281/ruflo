package wechat

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTransformOutsideCode 验证图片替换只发生在正文, 代码块/行内代码里的示例图片语法保持原样。
func TestTransformOutsideCode(t *testing.T) {
	in := strings.Join([]string{
		"![a](a.png)",
		"```go",
		"![b](b.png)",
		"```",
		"行内 `![c](c.png)` 不动",
		"~~~",
		"![d](d.png)",
		"~~~",
		"![e](e.png)",
	}, "\n")
	got := transformOutsideCode(in, func(s string) string {
		return strings.ReplaceAll(s, ".png", ".webp")
	})
	for _, want := range []string{"![a](a.webp)", "![e](e.webp)"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少替换结果 %q\n%s", want, got)
		}
	}
	for _, keep := range []string{"![b](b.png)", "![c](c.png)", "![d](d.png)"} {
		if !strings.Contains(got, keep) {
			t.Errorf("代码内容被误改, 期望保留 %q\n%s", keep, got)
		}
	}
}

// TestReadImageSourceHTTP 验证远程图片可下载并按 Content-Type 补扩展名。
func TestReadImageSourceHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	}))
	defer srv.Close()
	c, err := NewClient(Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, name, err := c.readImageSource(srv.URL+"/pic?id=1", ".")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "PNGDATA" {
		t.Fatalf("内容不符: %q", b)
	}
	if name != "pic.png" {
		t.Fatalf("文件名应为 pic.png, 实际 %q", name)
	}
}

// TestReadImageSourceLocal 验证相对路径按 baseDir 解析。
func TestReadImageSourceLocal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.png"), []byte("LOCAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, name, err := c.readImageSource("./x.png", dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "LOCAL" || name != "x.png" {
		t.Fatalf("读取不符: %q %q", b, name)
	}
}

// TestNewClientProxyValidation 验证非法代理直接报错, 而不是静默直连。
func TestNewClientProxyValidation(t *testing.T) {
	if _, err := NewClient(Config{Proxy: "127.0.0.1:1080"}); err == nil {
		t.Fatal("缺少 scheme 的代理应报错")
	}
	if _, err := NewClient(Config{Proxy: "http://127.0.0.1:1080"}); err != nil {
		t.Fatalf("合法代理不应报错: %v", err)
	}
}
