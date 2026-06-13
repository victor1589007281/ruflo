package wechat

import "strings"

import "testing"

// 真实 techblog 输出里出错的有序+嵌套列表 (4 项, 每项 2 个子项)。
const nestedListMD = "如果你正在维护一个基于 DeepSeek API 的 Agent，可以按以下步骤引入 Reasonix 的优化策略：\n\n" +
	"1. **审计 system prompt 和工具定义**\n" +
	"   - 移除所有动态生成内容（当前时间、随机 ID 等）\n" +
	"   - 将动态信息移到 user/assistant 消息中\n\n" +
	"2. **改造消息存储**\n" +
	"   - 从\"可编辑列表\"改为\"追加队列\"\n" +
	"   - 历史消息只读，新消息只追加\n\n" +
	"3. **实现受控压缩**\n" +
	"   - 设定软阈值（如 70% 窗口上限）\n" +
	"   - 压缩时保留前缀和最近 N 轮，中间用摘要替换\n\n" +
	"4. **增加缓存监控**\n" +
	"   - 追踪缓存命中率、节省 token 数\n" +
	"   - 根据监控数据调整阈值策略\n"

func TestNestedOrderedList(t *testing.T) {
	md, _ := LintLists(nestedListMD)
	html, err := MarkdownToHTML(md)
	if err != nil {
		t.Fatalf("MarkdownToHTML: %v", err)
	}
	out := InlineWechatStyles(html)

	// 1) 有序编号必须是 1-4, 且不能出现 5-8 (旧 bug 把子项算成顶层项)。
	for _, want := range []string{">1. </strong>", ">2. </strong>", ">3. </strong>", ">4. </strong>"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少有序编号 %q", want)
		}
	}
	for _, bad := range []string{">5. </strong>", ">6. </strong>", ">7. </strong>", ">8. </strong>"} {
		if strings.Contains(out, bad) {
			t.Errorf("出现了不该有的编号 %q (子项被误当顶层项)", bad)
		}
	}

	// 2) 8 个子项必须是带缩进的项目符 (margin-left)。
	subBullets := []string{
		"移除所有动态生成内容", "将动态信息移到 user/assistant 消息中",
		"历史消息只读，新消息只追加", "设定软阈值",
		"压缩时保留前缀和最近 N 轮", "追踪缓存命中率", "根据监控数据调整阈值策略",
	}
	for _, sb := range subBullets {
		idx := strings.Index(out, sb)
		if idx < 0 {
			t.Errorf("缺少子项内容 %q", sb)
			continue
		}
		// 该子项所在的 <p> 应包含项目符 • 与缩进 margin-left
		pStart := strings.LastIndex(out[:idx], "<p ")
		if pStart < 0 {
			t.Errorf("子项 %q 不在 <p> 内", sb)
			continue
		}
		seg := out[pStart:idx]
		if !strings.Contains(seg, "margin-left:") {
			t.Errorf("子项 %q 缺少缩进 margin-left", sb)
		}
		if !strings.Contains(seg, "•") {
			t.Errorf("子项 %q 缺少项目符 •", sb)
		}
	}

	// 3) 不应残留任何 <li>/<ul>/<ol> 标签。
	for _, tag := range []string{"<li", "<ul", "<ol"} {
		if strings.Contains(out, tag) {
			t.Errorf("仍残留列表标签 %q", tag)
		}
	}
}

// 顶层有序项不缩进, 子项缩进一层。
func TestIndentDepth(t *testing.T) {
	md, _ := LintLists(nestedListMD)
	html, _ := MarkdownToHTML(md)
	out := InlineWechatStyles(html)

	// 顶层 "1. 审计..." 不应有 margin-left
	i1 := strings.Index(out, "审计 system prompt")
	p1 := strings.LastIndex(out[:i1], "<p ")
	if strings.Contains(out[p1:i1], "margin-left:") {
		t.Errorf("顶层有序项不应缩进")
	}
	// 子项 "移除所有动态生成内容" 应有 margin-left:20px
	i2 := strings.Index(out, "移除所有动态生成内容")
	p2 := strings.LastIndex(out[:i2], "<p ")
	if !strings.Contains(out[p2:i2], "margin-left:20px") {
		t.Errorf("一级子项应缩进 20px, 实际: %s", out[p2:i2])
	}
}
