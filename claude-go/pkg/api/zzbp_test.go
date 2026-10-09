package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// 这些测试锁的是 2026-09-19 从真实 trace 里查实的一个前缀缓存缺陷:
//
//	markCacheBreakpointAt 原本把整条消息 unmarshal 进 `any` 再 json.Marshal 回来。
//	Go 的 map 往返会**按键名字典序重排** —— 于是被贴上断点的那条消息里, 凡是键序
//	不等于字典序的 tool_use, 其 input 字节全部被重写。
//
// 实测 (trace-run-if-question-expand-dd87429232db-2fd81b6eb4758f65):
//   - Write      发射时 {"path":...,"contents":...} → 携带时 {"contents":...,"path":...}
//   - StrReplace 发射时 {"path":...,"old_string":...,"new_string":...} → 字典序
//   - Read / Bash **看不出变化**: 前者单键, 后者 command,description 本就是字典序
//     —— 不是幸免, 是 map 往返对已排序的键序恰好是恒等变换。
//
// 关键在"只打乱一轮": 断点位置每轮都在移动 (BP3=最后一条 / BP2=最后一条 assistant),
// 同一条 tool_use 在它成为 BP2 的那一轮被打乱、下一轮又不是 BP2 于是还原 —— 而**上一轮
// 的缓存里存的是打乱后的字节**, 缓存就断在还原的那一刻。实测断点偏移:
//   t5 @29796 (`[tool_use Write]` 后 3B)、t7 @35895、t11 @25329。

// modelArgs 是模型真实吐出的字节: Write 的键序是 Go 结构体序 (path,contents),
// 刻意与字典序相反 —— 这正是缺陷的放大器。
const modelArgs = `{"path":"/workspace/write_questions.py","contents":"import json\nquestions = []"}`

// strReplaceArgs 同理会暴露: path,old_string,new_string -> 字典序 new_string,old_string,path
const strReplaceArgs = `{"path":"/workspace/w.py","old_string":"a","new_string":"b"}`

// oldMarkCacheBreakpointAt 是**修复前的原样实现**, 只用于对照证明它确实会重排键序。
// 不要在任何生产路径上使用它。
func oldMarkCacheBreakpointAt(messages []types.APIMessage, idx int) bool {
	target := &messages[idx]
	var content any
	if err := json.Unmarshal(target.Content, &content); err != nil {
		return false
	}
	blocks, ok := content.([]any)
	if !ok || len(blocks) == 0 {
		return false
	}
	last, ok := blocks[len(blocks)-1].(map[string]any)
	if !ok {
		return false
	}
	if _, dup := last["cache_control"]; dup {
		return false
	}
	last["cache_control"] = map[string]any{"type": "ephemeral"}
	raw, err := json.Marshal(content)
	if err != nil {
		return false
	}
	target.Content = raw
	return true
}

func mkAssistantWithToolUse(t *testing.T, args string) []types.APIMessage {
	t.Helper()
	content, err := json.Marshal([]types.ContentBlock{
		{Type: types.ContentBlockThinking, Thinking: "write the file"},
		{Type: types.ContentBlockToolUse, ID: "toolu_01", Name: "Write", Input: json.RawMessage(args)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return []types.APIMessage{{Role: "assistant", Content: content}}
}

// ① 核心断言: 贴断点不得改动 tool_use 的 input 半字节。
func TestBreakpointPreservesToolUseBytes(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"Write", modelArgs},
		{"StrReplace", strReplaceArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs := mkAssistantWithToolUse(t, tc.args)
			if !markCacheBreakpointAt(msgs, 0) {
				t.Fatal("断点没贴上")
			}
			got := string(msgs[0].Content)
			if !strings.Contains(got, `"cache_control"`) {
				t.Fatal("断点字段没进去")
			}
			if !strings.Contains(got, tc.args) {
				t.Fatalf("★ input 原字节被改写了\n  期望含: %s\n  实际  : %s", tc.args, got)
			}
		})
	}
}

// ② 对照: 旧实现确实会打乱 —— 这条证明①测的不是空气。
func TestOldImplementationScrambles(t *testing.T) {
	msgs := mkAssistantWithToolUse(t, modelArgs)
	if !oldMarkCacheBreakpointAt(msgs, 0) {
		t.Fatal("旧实现没贴上")
	}
	got := string(msgs[0].Content)
	if strings.Contains(got, modelArgs) {
		t.Skip("旧实现竟然保住了键序 —— 本测试的对照前提已不成立")
	}
	// 打乱后的形态应当只剩字典序一种 (contents 在 path 前)。
	if !strings.Contains(got, `{"contents":`) {
		t.Fatalf("旧实现的重排结果与预期不符: %s", got)
	}
	t.Logf("旧实现把 %s 重排成了字典序 —— 缺陷可复现", modelArgs)
}

// ③ 缓存判据本身: 贴断点前后, 首个不同字节必须落在**整个 tool_use 之后**。
//
// 这条才是"缓存有没有被踢掉"的直接度量 —— 不是"改了多少字节"。
func TestBreakpointFirstDiffIsAfterToolUse(t *testing.T) {
	a := mkAssistantWithToolUse(t, modelArgs) // 会被贴断点
	b := mkAssistantWithToolUse(t, modelArgs) // 不贴

	markCacheBreakpointAt(a, 0)

	x, y := string(a[0].Content), string(b[0].Content)
	n := len(x)
	if len(y) < n {
		n = len(y)
	}
	d := -1
	for i := 0; i < n; i++ {
		if x[i] != y[i] {
			d = i
			break
		}
	}
	if d < 0 {
		t.Fatal("贴与不贴完全一样 —— 断点没生效")
	}
	// input 在渲染里的起点; 差异必须严格晚于 input 的结尾。
	inputStart := strings.Index(y, `"input":`)
	inputEnd := inputStart + len(`"input":`) + len(modelArgs)
	if d < inputEnd {
		t.Fatalf("★ 首个不同字节 @%d 落在 tool_use input 内 (@%d..@%d) —— 前缀缓存会被踢掉",
			d, inputStart, inputEnd)
	}
	t.Logf("首个不同字节 @%d, input 结束于 @%d —— 前缀稳定 OK", d, inputEnd)
}

// ④ 只打乱一轮、下一轮还原 —— 复现真实 trace 里的"打乱↔还原"交替。
//
// 这是本缺陷的关键形态: 断点位置**每轮都在移动** (BP3=最后一条 / BP2=最后一条
// assistant)。同一条历史 assistant 在它成为 BP2 的那一轮被打乱, 下一轮 BP2 落到
// 更新的 assistant 上, 那条旧的原字节又回来了 —— 而**上一轮的缓存里存的是打乱后
// 的字节**, 于是缓存断在"回归"的这一刻。
//
// 断言的两件事:
//  1. 共享历史里那条 tool_use 的 input 在两次渲染中逐字节相同;
//  2. 两轮渲染的首个不同字节落在整个 tool_use 之后 (断点本来就该出现在那里)。
func TestBreakpointStableAcrossTurns(t *testing.T) {
	// 第 N 轮: 只有这条 assistant, 它是 BP2 —— 会被打上断点。
	turnN := mkAssistantWithToolUse(t, modelArgs)
	if !markCacheBreakpointAt(turnN, 0) {
		t.Fatal("第 N 轮断点没贴上")
	}
	renderedN := string(turnN[0].Content)

	// 第 N+1 轮: 同一条历史消息后面追加了新消息, BP2 落到新助手身上,
	// 于是这条旧 assistant **不再**被打断点。
	turnN1 := mkAssistantWithToolUse(t, modelArgs)
	turnN1 = append(turnN1,
		types.APIMessage{Role: "user", Content: json.RawMessage(`"继续"`)},
		types.APIMessage{Role: "assistant", Content: json.RawMessage(`"好"`)},
	)
	if !markCacheBreakpointAt(turnN1, len(turnN1)-1) {
		t.Fatal("第 N+1 轮断点没贴上")
	}
	renderedN1 := string(turnN1[0].Content)

	// ① 共享历史里的 tool_use input 必须两轮一致。
	if !strings.Contains(renderedN, modelArgs) {
		t.Fatalf("第 N 轮 input 已被改写: %s", renderedN)
	}
	if !strings.Contains(renderedN1, modelArgs) {
		t.Fatalf("★ 第 N+1 轮 input 与第 N 轮不同 —— 缓存必断\n  tN1: %s", renderedN1)
	}

	// ② 首个不同字节必须落在整个 tool_use 之后 (只应是断点字段的有无)。
	n := len(renderedN)
	if len(renderedN1) < n {
		n = len(renderedN1)
	}
	d := -1
	for i := 0; i < n; i++ {
		if renderedN[i] != renderedN1[i] {
			d = i
			break
		}
	}
	if d < 0 {
		t.Fatal("两轮完全一样 —— 断点位置没移动, 本测试没测到东西")
	}
	inputEnd := strings.Index(renderedN1, `"input":`) + len(`"input":`) + len(modelArgs)
	if d < inputEnd {
		t.Fatalf("★ 首个不同字节 @%d 落在 tool_use input 内 (结束于 @%d) —— 前缀缓存会被踢掉",
			d, inputEnd)
	}
	t.Logf("共享历史逐字节稳定; 首个不同字节 @%d 在 tool_use 之后 (@%d) OK", d, inputEnd)
}

// ⑤ 幂等: 上游对重复 cache_control 报错, 贴过的不能再贴。
func TestBreakpointIdempotent(t *testing.T) {
	msgs := mkAssistantWithToolUse(t, modelArgs)
	if !markCacheBreakpointAt(msgs, 0) {
		t.Fatal("第一次没贴上")
	}
	first := string(msgs[0].Content)
	if markCacheBreakpointAt(msgs, 0) {
		t.Fatal("第二次不该再贴")
	}
	if string(msgs[0].Content) != first {
		t.Fatal("第二次调用改动了内容")
	}
}

// ⑥ 纯字符串形态不能退化 (老行为)。
func TestBreakpointStringContent(t *testing.T) {
	msgs := []types.APIMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}
	if !markCacheBreakpointAt(msgs, 0) {
		t.Fatal("字符串形态没贴上")
	}
	// 结果必须是合法 JSON 且能被解析回内容块。
	var blocks []types.ContentBlock
	if err := json.Unmarshal(msgs[0].Content, &blocks); err != nil {
		t.Fatalf("结果不是合法内容块数组: %v (%s)", err, msgs[0].Content)
	}
	if len(blocks) != 1 || blocks[0].Text != "hello" {
		t.Fatalf("内容块还原不对: %+v", blocks)
	}
}

// ⑦ 越界/防御: idx 非法不能 panic, 非 user/assistant 角色不贴。
func TestBreakpointGuards(t *testing.T) {
	msgs := mkAssistantWithToolUse(t, modelArgs)
	for _, idx := range []int{-1, 1, 99} {
		if markCacheBreakpointAt(msgs, idx) {
			t.Fatalf("idx=%d 不该贴上", idx)
		}
	}
	sys := []types.APIMessage{{Role: "system", Content: json.RawMessage(`"s"`)}}
	if markCacheBreakpointAt(sys, 0) {
		t.Fatal("system 角色不该贴断点")
	}
}
