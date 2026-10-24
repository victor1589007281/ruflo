package search

import (
	"strings"
	"testing"
)

// TestExtractHTMLBlockquoteTerminates 是栈溢出的回归。
//
// 修复前 renderBlock(blockquote) 调 renderMarkdown(n), 后者对非根节点又走
// renderBlock(n) —— 同一个 blockquote 无限自递归, 进程 fatal。真实页面里
// <blockquote> 很常见, 所以这条不是理论边界。
func TestExtractHTMLBlockquoteTerminates(t *testing.T) {
	page := extractHTML(`<html><body>
		<article><h1>标题</h1><p>正文段落。</p>
		<blockquote><p>引用内容</p></blockquote>
		</article></body></html>`)
	if !strings.Contains(page.Markdown, "正文段落") {
		t.Fatalf("正文丢失: %q", page.Markdown)
	}
	if !strings.Contains(page.Markdown, "引用内容") {
		t.Fatalf("引用内容丢失: %q", page.Markdown)
	}
	if !strings.Contains(page.Markdown, "> ") {
		t.Fatalf("blockquote 未渲染成引用: %q", page.Markdown)
	}
}

// TestExtractHTMLNestedBlockquote 覆盖嵌套引用: 子引用也必须终止。
func TestExtractHTMLNestedBlockquote(t *testing.T) {
	page := extractHTML(`<html><body><main>
		<blockquote><p>外层</p><blockquote><p>内层</p></blockquote></blockquote>
	</main></body></html>`)
	for _, want := range []string{"外层", "内层"} {
		if !strings.Contains(page.Markdown, want) {
			t.Fatalf("缺少 %q: %q", want, page.Markdown)
		}
	}
}

// TestExtractHTMLSkipsNoise 确认去噪仍生效 (重构 render 路径时的护栏)。
func TestExtractHTMLSkipsNoise(t *testing.T) {
	page := extractHTML(`<html><body><article><p>保留</p>
		<script>alert(1)</script><nav>导航</nav><footer>页脚</footer>
		</article></body></html>`)
	if !strings.Contains(page.Markdown, "保留") {
		t.Fatalf("正文丢失: %q", page.Markdown)
	}
	for _, noise := range []string{"alert(1)", "导航", "页脚"} {
		if strings.Contains(page.Markdown, noise) {
			t.Fatalf("噪声 %q 未剔除: %q", noise, page.Markdown)
		}
	}
}
