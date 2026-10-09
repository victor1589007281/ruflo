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

// BP2/BP3 两点: 最后一条 + 最近一条 assistant, 更早的历史不动。
//
// 为什么是"最近一条 assistant"而不是 len-2: 一轮里 assistant 之后可能跟着多条
// tool_result (一次发起多个工具调用), len-2 会落在这一轮**内部**, 下一轮还会继续
// 增长 —— 拿一个还会动的边界当缓存键, 等于每轮换一次键。
func TestMarkBreakpoint_LastAndLastAssistant(t *testing.T) {
	msgs := []types.APIMessage{msg("user", "第一条"), msg("assistant", "回复"), msg("user", "第二条")}
	markMessageCacheBreakpoint(msgs)
	if lastHasCacheControl(t, msgs[0]) {
		t.Error("第 0 条 (更早的历史) 不应被改动")
	}
	if !lastHasCacheControl(t, msgs[1]) {
		t.Error("BP2: 最近一条 assistant 应带上断点")
	}
	if !lastHasCacheControl(t, msgs[2]) {
		t.Error("BP3: 最后一条应带上断点")
	}
}

// 一轮内多条 tool_result 时, BP2 落在那个 assistant 上, 不落在 tool_result 中间。
func TestCacheBreakpointIndexes_ToolResultRun(t *testing.T) {
	msgs := []types.APIMessage{
		msg("user", "问题"),
		msg("assistant", "调用工具"),
		msg("user", []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "r1"}}),
		msg("user", []map[string]any{{"type": "tool_result", "tool_use_id": "t2", "content": "r2"}}),
		msg("user", []map[string]any{{"type": "tool_result", "tool_use_id": "t3", "content": "r3"}}),
	}
	got := cacheBreakpointIndexes(msgs, 2)
	if len(got) != 2 {
		t.Fatalf("应有 2 个断点, got %v", got)
	}
	if got[0] != 4 {
		t.Errorf("BP3 应是最后一条 (4), got %d", got[0])
	}
	if got[1] != 1 {
		t.Errorf("BP2 应是那条 assistant (1), 而不是 tool_result 中间, got %d", got[1])
	}
}

// 只有一轮 (没有 assistant) 时, BP2 与 BP3 重合, 不应重复贴。
func TestCacheBreakpointIndexes_NoAssistantYet(t *testing.T) {
	msgs := []types.APIMessage{msg("user", "第一条"), msg("user", "第二条")}
	got := cacheBreakpointIndexes(msgs, 2)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("无 assistant 时只应有一个断点且落在最后, got %v", got)
	}
}

// 断点数不超过 max, 且这是**硬上限** —— 上游对超过 4 个断点是整个请求 400。
func TestCacheBreakpointIndexes_RespectsMax(t *testing.T) {
	msgs := make([]types.APIMessage, 0, 10)
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			msgs = append(msgs, msg("user", "u"))
		} else {
			msgs = append(msgs, msg("assistant", "a"))
		}
	}
	for _, max := range []int{1, 2, 3, 4} {
		if got := cacheBreakpointIndexes(msgs, max); len(got) > max {
			t.Errorf("max=%d 时产出 %d 个断点", max, len(got))
		}
	}
	if got := cacheBreakpointIndexes(msgs, 0); got != nil {
		t.Errorf("max<=0 应返回 nil, got %v", got)
	}
}

// BP1/BP2: 断点打在块 0 **和**稳定段末尾 (倒数第二块), 最后那块变量不碰。
//
// engine 的分层是「base(稳定) → memory(检索结果) → task(本轮任务)」, task 块
// 每轮必变; memory 块在同一 run 内不变、跨 run 会变 (记忆库被运行自身改写)。
// 只打稳定段末尾的话, 缓存条目 = base+memory, 跨 run 必失效, 而 base 那段从没被
// 单独缓存过 —— 这是改前的缺陷, 下面第三条断言就是对着它写的。
func TestMarkSystemCacheBreakpoint_StableBlock(t *testing.T) {
	blocks := []map[string]interface{}{
		{"type": "text", "text": "稳定的 base 提示词"},
		{"type": "text", "text": "检索出来的记忆块"},
		{"type": "text", "text": "本轮的任务指令 (每轮都变)"},
	}
	if !markSystemCacheBreakpoint(blocks) {
		t.Fatal("应成功打上断点")
	}
	if blocks[1]["cache_control"] == nil {
		t.Error("断点应打在稳定段末尾 (倒数第二块 = 记忆块)")
	}
	// base 块必须**自己**带一个断点: memory 块跨 run 会变, 只有 base 上单独有断点,
	// 那截前缀才可能跨 run 命中。
	if blocks[0]["cache_control"] == nil {
		t.Error("块 0 (base) 应单独带断点 —— 否则记忆一变, 整段前缀连 base 一起作废")
	}
	if blocks[2]["cache_control"] != nil {
		t.Error("每轮都变的任务块不应被打断点 (打了等于每轮换缓存键)")
	}
	// 幂等: 已有断点不再重复贴 (上游对重复标记报错)。
	if markSystemCacheBreakpoint(blocks) {
		t.Error("已有断点时应返回 false")
	}
	// 空数组不应 panic。
	if markSystemCacheBreakpoint(nil) {
		t.Error("空 system 不应返回 true")
	}
}

// 两个断点不能超预算: 消息侧固定占 2 个, system 侧最多只能占 2 个,
// 合计 = MaxCacheBreakpoints。超了上游是**整个请求 400**, 不是静默丢断点。
func TestMarkSystemCacheBreakpoint_不超断点预算(t *testing.T) {
	blocks := []map[string]interface{}{
		{"type": "text", "text": "base"},
		{"type": "text", "text": "memory"},
		{"type": "text", "text": "task"},
	}
	markSystemCacheBreakpoint(blocks)
	n := 0
	for _, b := range blocks {
		if b["cache_control"] != nil {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("system 侧断点数 = %d, 期望 2", n)
	}
	// 消息侧最多 2 个 (cachebp_test 里另有专门测试), 合计不得超过上限。
	if msgsMax := 2; n+msgsMax > MaxCacheBreakpoints {
		t.Fatalf("system %d + 消息 %d 超过 MaxCacheBreakpoints=%d", n, msgsMax, MaxCacheBreakpoints)
	}
}

// 只有 base 和 task 两块 (没有记忆块) 时, "稳定段末尾"正好就是块 0 ——
// 两个下标重合, 不能因此贴两次或返回 true 两次。
func TestMarkSystemCacheBreakpoint_无记忆块时两下标重合(t *testing.T) {
	blocks := []map[string]interface{}{
		{"type": "text", "text": "base"},
		{"type": "text", "text": "task"},
	}
	if !markSystemCacheBreakpoint(blocks) {
		t.Fatal("应成功打上断点")
	}
	if blocks[0]["cache_control"] == nil {
		t.Error("块 0 应带断点")
	}
	if blocks[1]["cache_control"] != nil {
		t.Error("task 块不应带断点")
	}
	if markSystemCacheBreakpoint(blocks) {
		t.Error("第二次调用不应再返回 true")
	}
}

// 只有一个块时没有"稳定段末尾"可退, 就打它自己 —— 不能返回 false 把 BP1 丢掉。
func TestMarkSystemCacheBreakpoint_SingleBlock(t *testing.T) {
	blocks := []map[string]interface{}{{"type": "text", "text": "只有一个块"}}
	if !markSystemCacheBreakpoint(blocks) {
		t.Fatal("单块 system 仍应打上 BP1")
	}
	if blocks[0]["cache_control"] == nil {
		t.Error("单块时应打在块 0")
	}
}

// cacheEnabled=false 时**完全不输出** cache_control:
// 上游不支持该字段时的兜底是"400 后自适应关闭", 这里负责不自找麻烦。
func TestBuildSystemParam_NoCacheControlWhenDisabled(t *testing.T) {
	one := buildSystemParam([]string{"s0"}, false)
	if s, ok := one.(string); !ok || s != "s0" {
		t.Errorf("单块 + 关闭缓存应退化为纯字符串, got %#v", one)
	}
	multi := buildSystemParam([]string{"s0", "s1"}, false)
	blocks, ok := multi.([]map[string]interface{})
	if !ok {
		t.Fatalf("多块应是块数组, got %#v", multi)
	}
	for i, b := range blocks {
		if b["cache_control"] != nil {
			t.Errorf("关闭缓存时块 %d 不该带 cache_control", i)
		}
	}
	if buildSystemParam(nil, true) != nil {
		t.Error("空 system 应返回 nil")
	}
	// 开启时单块也要带上 BP1。
	withCache := buildSystemParam([]string{"s0"}, true)
	bs, ok := withCache.([]map[string]interface{})
	if !ok || len(bs) != 1 || bs[0]["cache_control"] == nil {
		t.Errorf("开启缓存时单块应带 cache_control, got %#v", withCache)
	}
}
