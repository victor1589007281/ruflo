// 块标量 (`key: |` / `key: >`) 解析测试。
//
// 事故背景 (2026-11-28): 用户级技能 agent-reach / ima-skill 的 description 用
// 块标量写, 旧解析器把值读成字面量 ">" / "|"。技能清单里于是出现
// "- agent-reach: >" —— 描述正是路由的唯一信号, 这两个技能等于不可路由。
package asset

import "testing"

func TestSplitFrontmatterFoldedScalar(t *testing.T) {
	src := "---\n" +
		"name: agent-reach\n" +
		"description: >\n" +
		"  Give your agent eyes to see the entire internet.\n" +
		"  17 platforms via CLI.\n" +
		"\n" +
		"  Use when user asks to search.\n" +
		"triggers:\n" +
		"  - search\n" +
		"---\n\nbody\n"
	fm := SplitFrontmatter(src)
	if !fm.Present {
		t.Fatal("frontmatter 应被识别")
	}
	want := "Give your agent eyes to see the entire internet. 17 platforms via CLI.\nUse when user asks to search."
	if got := fm.Get("description"); got != want {
		t.Fatalf("折叠标量解析错误:\nwant %q\n got %q", want, got)
	}
	// 块之后的顶层键不能被吞进块里。
	if got := fm.List("triggers"); len(got) != 1 || got[0] != "search" {
		t.Fatalf("块标量之后的列表键被吞掉了: %#v", got)
	}
}

func TestSplitFrontmatterLiteralScalar(t *testing.T) {
	src := "---\n" +
		"name: ima-skill\n" +
		"description: |\n" +
		"  统一的 IMA OpenAPI 技能。\n" +
		"  支持笔记管理和知识库操作。\n" +
		"homepage: https://ima.qq.com\n" +
		"---\n\nbody\n"
	fm := SplitFrontmatter(src)
	want := "统一的 IMA OpenAPI 技能。\n支持笔记管理和知识库操作。"
	if got := fm.Get("description"); got != want {
		t.Fatalf("字面量标量解析错误:\nwant %q\n got %q", want, got)
	}
	if got := fm.Get("homepage"); got != "https://ima.qq.com" {
		t.Fatalf("块之后的键解析错误: %q", got)
	}
}

func TestSplitFrontmatterBlockScalarChomping(t *testing.T) {
	for _, header := range []string{"|-", "|+", ">-", ">+"} {
		src := "---\nk: " + header + "\n  第一行\n  第二行\n---\n"
		fm := SplitFrontmatter(src)
		if got := fm.Get("k"); got == "" || got == header {
			t.Fatalf("chomping 头 %q 未被识别, got %q", header, got)
		}
	}
}
