package asset

import (
	"fmt"
	"sort"
	"strings"
)

// listing.go —— 把某一种资产的清单渲染成模型可读的片段。
//
// ---------------------------------------------------------------------------
// 为什么只有一个 kind 真的用了它
// ---------------------------------------------------------------------------
//
// 六种资产里, 目前**只有 agent 有模型侧的消费方**: 全仓唯一一个从模型手里接资产
// 名字的工具是 Agent 的 subagent_type (pkg/agent/agent.go)。工作流/插件/工具/hook
// 的名字模型接过之后没有任何东西会去用它 —— 把它们的清单写进提示词, 是给每一次
// 请求白加一段"说了但不生效"的文本, 正是本仓库反复出现的 "声明了但没人读"
// 在提示词层面的翻版。
//
// 所以这里提供的是**渲染器**, 不是"每类资产都该有一份清单"的主张。等哪一类资产
// 真的长出消费方 (例如一个按名字调起工作流的工具), 再让那一类调它 —— 那时加一行,
// 而不是现在先把六份清单铺出去。
//
// 判据与本文件的边界: 只列 active (影子/归档列出来等于绕过灰度), 有上限且截断
// 如实写出 (静默截断会让"模型不知道有它"与"它不存在"变得一样)。

// listingMaxDescRunes 每条描述最多多少字 (rune)。
const listingMaxDescRunes = 100

// Listing 渲染一种 kind 的清单。条数上限由调用方给 —— 预算是调用点的策略
// (agent 清单进**每一次**请求的工具描述里, 它的预算与别的场景不该共用一个数)。
//
// 无内容时返回空串而不是空标签: 调用方按 `if s != ""` 拼接, 一个恒非空的
// <available_x></available_x> 会让每次请求都多带一段无意义文本, 且让"没有这类资产"
// 与"这类资产被读空了"看起来一样。
func (s *Set) Listing(k Kind, tag string, maxItems int) string {
	if s == nil || !k.Valid() {
		return ""
	}
	names := s.Names(k)
	shown := make([]*Asset, 0, len(names))
	for _, n := range names {
		a := s.Get(k, n)
		if a == nil || !a.IsActive() {
			continue
		}
		shown = append(shown, a)
	}
	if len(shown) == 0 {
		return ""
	}
	var omitted int
	if maxItems > 0 && len(shown) > maxItems {
		omitted = len(shown) - maxItems
		shown = shown[:maxItems]
	}
	// Names 已排序, 过滤后仍保序。稳定输出是硬要求: 这份文本进提示词,
	// 顺序抖动会让 prompt 缓存每次都 miss。
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].Name < shown[j].Name })

	var b strings.Builder
	fmt.Fprintf(&b, "\n<available_%s>\n", tag)
	for _, a := range shown {
		b.WriteString("- **")
		b.WriteString(a.Name)
		b.WriteString("**")
		if d := oneLineText(a.Description); d != "" {
			b.WriteString(": ")
			b.WriteString(TruncateRunes(d, listingMaxDescRunes))
		}
		b.WriteString("\n")
	}
	if omitted > 0 {
		// 截断必须可见 —— 静默截断会让模型以为清单是全的, 于是它不会去问还有没有别的。
		fmt.Fprintf(&b, "(还有 %d 个未列出)\n", omitted)
	}
	fmt.Fprintf(&b, "</available_%s>\n", tag)
	return b.String()
}

// KindTag 资产在清单里用的标签名 (复数)。与 pkg/skills 的 <available_skills>
// 保持同一套词汇 —— 模型已经认识那个形状, 换一套只会多学一次。
func KindTag(k Kind) string {
	switch k {
	case KindAgent:
		return "agents"
	case KindWorkflow:
		return "workflows"
	case KindPlugin:
		return "plugins"
	case KindSkill:
		return "skills"
	case KindTool:
		return "tools"
	case KindHook:
		return "hooks"
	}
	return string(k)
}

// oneLineText 把多行描述压成一行: 清单是逐行解析的, 一条多行描述会把后面的条目
// 挤成它的正文 (而模型会照单全收)。
func oneLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
