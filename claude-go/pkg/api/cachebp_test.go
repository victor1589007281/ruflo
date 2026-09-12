package api

import (
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

func msg(role string, content any) types.APIMessage {
	raw, _ := json.Marshal(content)
	return types.APIMessage{Role: role, Content: raw}
}

func lastHasCacheControl(t *testing.T, m types.APIMessage) bool {
	t.Helper()
	var c []map[string]any
	if err := json.Unmarshal(m.Content, &c); err != nil {
		return false
	}
	if len(c) == 0 {
		return false
	}
	return c[len(c)-1]["cache_control"] != nil
}

// 纯字符串的最后一条消息应被包成内容块并贴上断点
func TestMarkBreakpoint_StringContent(t *testing.T) {
	msgs := []types.APIMessage{msg("user", "hi"), msg("user", "最后一条")}
	markMessageCacheBreakpoint(msgs)
	if !lastHasCacheControl(t, msgs[1]) {
		t.Fatal("最后一条消息应带上 cache_control")
	}
}

// 数组形态：只贴最后一个块，前面的不动
func TestMarkBreakpoint_BlockContent(t *testing.T) {
	msgs := []types.APIMessage{msg("user", []map[string]any{
		{"type": "text", "text": "a"},
		{"type": "text", "text": "b"},
	})}
	markMessageCacheBreakpoint(msgs)
	var c []map[string]any
	json.Unmarshal(msgs[0].Content, &c)
	if c[0]["cache_control"] != nil {
		t.Error("不应动第一个块")
	}
	if c[1]["cache_control"] == nil {
		t.Error("最后一个块应带 cache_control")
	}
}

// 已有断点不重复贴（上游对重复标记会报错）
func TestMarkBreakpoint_Idempotent(t *testing.T) {
	msgs := []types.APIMessage{msg("user", []map[string]any{
		{"type": "text", "text": "a", "cache_control": map[string]string{"type": "ephemeral"}},
	})}
	markMessageCacheBreakpoint(msgs)
	var c []map[string]any
	json.Unmarshal(msgs[0].Content, &c)
	v, _ := c[0]["cache_control"].(map[string]any)
	if v["type"] != "ephemeral" {
		t.Errorf("原有断点应保持不变, got %v", c[0]["cache_control"])
	}
}

// 空消息 / 空内容不应 panic
func TestMarkBreakpoint_Empty(t *testing.T) {
	markMessageCacheBreakpoint(nil)
	markMessageCacheBreakpoint([]types.APIMessage{msg("user", "")})
	markMessageCacheBreakpoint([]types.APIMessage{{Role: "user"}})
}

// 只动最后一条，历史消息不受影响
func TestMarkBreakpoint_OnlyLast(t *testing.T) {
	msgs := []types.APIMessage{msg("user", "第一条"), msg("assistant", "回复"), msg("user", "第二条")}
	markMessageCacheBreakpoint(msgs)
	for i := 0; i < 2; i++ {
		if lastHasCacheControl(t, msgs[i]) {
			t.Errorf("第 %d 条不应被改动", i)
		}
	}
	if !lastHasCacheControl(t, msgs[2]) {
		t.Error("最后一条应带上断点")
	}
}
