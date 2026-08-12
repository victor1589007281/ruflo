package engine

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func TestFilterDynamicMaskedTools(t *testing.T) {
	tools := []types.APITool{
		{Name: "Shell"}, {Name: "Read"}, {Name: "advisor"},
		{Name: "ToolSearch"}, {Name: "media_gen"}, {Name: "LSP"},
	}
	// 无使用历史: 只剩核心集 + ToolSearch
	out := filterDynamicMaskedTools(tools, nil)
	names := map[string]bool{}
	for _, tl := range out {
		names[tl.Name] = true
	}
	if !names["Shell"] || !names["Read"] || !names["ToolSearch"] {
		t.Error("核心集与 ToolSearch 必须保留")
	}
	if names["advisor"] || names["media_gen"] || names["LSP"] {
		t.Error("未使用的长尾工具应被动态收窄")
	}

	// 有使用历史: 已调用工具粘性保留 (即使不在核心集)
	msgs := []types.Message{{
		Type: types.MessageTypeAssistant,
		Content: []types.ContentBlock{{
			Type: types.ContentBlockToolUse, Name: "LSP", ID: "x1",
		}},
	}}
	out = filterDynamicMaskedTools(tools, msgs)
	names = map[string]bool{}
	for _, tl := range out {
		names[tl.Name] = true
	}
	if !names["LSP"] {
		t.Error("已实际调用的工具应粘性保留")
	}
	if names["advisor"] || names["media_gen"] {
		t.Error("仍未使用的长尾应继续收窄")
	}
}
