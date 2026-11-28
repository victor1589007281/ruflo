package internal_hook

// IterationGuard（工具驱逐）的测试。
//
// 为什么值得单独测：驱逐是**在线的、不可逆的**干预——工具一旦摘掉本次运行内不再
// 恢复。判据错一次就可能把任务唯一出路堵死，所以三条边界都要钉住：
// 阈值语义（第几次触发才驱逐）、按工具而不是按签名计数、关掉开关时行为回到 P2。
//
// 注意检测器的真实语义：滑窗里一旦攒够 Threshold 个相同签名，**之后每次**同参
// 调用都会继续命中（尾部三连仍然成立）。所以"触发一次"= 连续 Threshold 次同参，
// 想让它只触发一次就必须用不同的调用把滑窗冲掉（drain 辅助函数）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

const sameArgs = `{"path":"/tmp/a.txt"}`

// drain 用 Window 次互不相同的调用把滑窗清空，让下一次同参调用重新从零计数。
func drain(d *LoopDetector) {
	for i := 0; i < d.Window; i++ {
		d.Observe("Noise", []byte(`{"i":`+string(rune('a'+i))+`}`))
	}
}

// triggerOnce 连续调用同参 Threshold 次，产生恰好一次触发。
func triggerOnce(t *testing.T, d *LoopDetector, tool string) string {
	t.Helper()
	drain(d)
	var sug string
	looped := false
	for j := 0; j < d.Threshold; j++ {
		looped, sug = d.Observe(tool, []byte(sameArgs))
	}
	if !looped {
		t.Fatalf("%s: 连续 %d 次同参调用应触发", tool, d.Threshold)
	}
	return sug
}

func TestLoopDetectorEvictsAfterThreshold(t *testing.T) {
	d := NewLoopDetector()
	d.EvictAfter = 2
	if d.IsEvicted("Read") {
		t.Fatal("初始不应有驱逐")
	}
	triggerOnce(t, d, "Read")
	if d.IsEvicted("Read") {
		t.Fatal("第一次触发不应驱逐 (EvictAfter=2)")
	}
	if got := d.TriggerCountFor("Read"); got != 1 {
		t.Fatalf("触发计数 = %d, 期望 1", got)
	}
	sug := triggerOnce(t, d, "Read")
	if !d.IsEvicted("Read") {
		t.Fatal("第二次触发应驱逐")
	}
	if got := d.EvictedTools(); len(got) != 1 || got[0] != "Read" {
		t.Fatalf("驱逐列表 = %v", got)
	}
	// 驱逐后的建议文本必须告诉模型"工具没了"，否则模型只会以为这次调用被拒。
	if !strings.Contains(sug, "移除") {
		t.Fatalf("驱逐时的建议要说明工具已被移除: %q", sug)
	}
	// 之后再撞上（滑窗仍在）也要带上这句 —— 幂等复现。
	_, again := d.Observe("Read", []byte(sameArgs))
	if !strings.Contains(again, "移除") {
		t.Fatalf("已驱逐工具再次触发也要说明: %q", again)
	}
}

func TestLoopDetectorEvictDisabled(t *testing.T) {
	d := NewLoopDetector()
	d.EvictAfter = 0 // 关掉驱逐 = P2 行为
	for i := 0; i < 5; i++ {
		triggerOnce(t, d, "Grep")
	}
	if d.IsEvicted("Grep") {
		t.Fatal("EvictAfter=0 时永不驱逐")
	}
	if d.TriggerCountFor("Grep") != 5 {
		t.Fatalf("触发计数仍要记（观测用）, got %d", d.TriggerCountFor("Grep"))
	}
}

// 计数按**工具**而不是按签名：换了参数继续磨同一个工具同样是没换路，
// 而"拦调用"只挡得住完全相同的那一个签名。
func TestLoopDetectorEvictCountsPerToolAcrossArgs(t *testing.T) {
	d := NewLoopDetector()
	d.EvictAfter = 2
	// 第一组参数：同参 3 连 → 触发一次，还不到 EvictAfter
	for j := 0; j < d.Threshold; j++ {
		d.Observe("Grep", []byte(`{"q":"a"}`))
	}
	if d.IsEvicted("Grep") {
		t.Fatal("第一次触发后不应驱逐")
	}
	// 换一组**完全不同的参数**继续磨：滑窗被新签名重置 -> 再攒 3 连 -> 第二次触发
	for j := 0; j < d.Threshold; j++ {
		d.Observe("Grep", []byte(`{"q":"b"}`))
	}
	if !d.IsEvicted("Grep") {
		t.Fatalf("不同参数但同一工具累计触发 2 次后应驱逐; 触发计数=%d", d.TriggerCountFor("Grep"))
	}
}

// 每工具独立计数：Read 触发两次不连坐 Grep。
func TestLoopDetectorEvictIsPerTool(t *testing.T) {
	d := NewLoopDetector()
	d.EvictAfter = 2
	triggerOnce(t, d, "Read")
	triggerOnce(t, d, "Read")
	if !d.IsEvicted("Read") {
		t.Fatal("Read 应被驱逐")
	}
	if d.IsEvicted("Grep") {
		t.Fatal("Grep 不应被连坐")
	}
}

// 驱逐必须能从 hook trace 里看见：ndjson 的 progress(phase:"hook") 事件只带
// hookResultSummary 的标签，不新开协议字段。没有这个标签，运维只能从"工具
// 突然没人调了"倒推，而那时已经分不清是驱逐还是模型自己换了路。
func TestLoopDetectorEvictSurfacesInHookResult(t *testing.T) {
	d := NewLoopDetector()
	d.EvictAfter = 1 // 第一次触发就驱逐，省掉一轮
	h := NewLoopDetectorInputHook(d, nil)
	ctx := func(id string) *HookContext {
		return &HookContext{Ctx: context.Background(), ToolUseBlocks: []types.ContentBlock{{
			Type: types.ContentBlockToolUse, ID: id, Name: "Read", Input: json.RawMessage(sameArgs),
		}}}
	}
	// 攒到 Threshold 的那一次：拦截 + 驱逐，结果里要带 EvictedTools
	var res *HookResult
	for i := 0; i < d.Threshold; i++ {
		r, err := h.Execute(ctx("tu-" + string(rune('a'+i))))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if r != nil {
			res = r
		}
	}
	if res == nil {
		t.Fatal("触发轮应有 HookResult")
	}
	if len(res.EvictedTools) != 1 || res.EvictedTools[0] != "Read" {
		t.Fatalf("EvictedTools = %v, 期望 [Read]", res.EvictedTools)
	}
	if got := hookResultSummary(res); got != "tools_evicted" {
		t.Fatalf("hook trace 标签 = %q, 期望 tools_evicted", got)
	}
	// 第二次（已驱逐后再调）：不再算"新驱逐"，标签回落，但仍然拦截
	r2, _ := h.Execute(ctx("tu-z"))
	if r2 == nil || len(r2.EvictedTools) != 0 {
		t.Fatalf("重复调用不应重复报驱逐: %+v", r2)
	}
	if got := hookResultSummary(r2); got == "tools_evicted" {
		t.Fatal("没有新驱逐时不应打 tools_evicted")
	}
	if r2.ToolUseBlocks != nil {
		t.Fatalf("已驱逐工具的调用必须被拦下, 实际放行 %d 个", len(r2.ToolUseBlocks))
	}
}
