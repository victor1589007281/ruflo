// unknown_block_test.go — R-H3 在 Harness 出站路径上的验收。
//
// types 包的单测证明"反序列化再序列化"能保真；这里证明**真的发出去的请求体**
// 也保真。两者缺一不可：一个只在单元测试里成立的保真层，如果 harness 的
// splitInput 或 engine 的 messagesToAPI 把未知块过滤掉，线上仍然是静默丢数据。
//
// 判据用原始片段而不是"字段值相等"：要求的正是逐字节不变，重新编码后键序/
// 数字格式变化都算破坏（上游的签名/校验字段会因此对不上）。
package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/types"
)

// unknownBlockJSON 是一个"未来才存在"的块：类型不认识，且带两个未知 metadata。
const unknownBlockJSON = `{"type":"server_tool_use","id":"stu_01","name":"web_search","input":{"query":"上海天气"},"cache_control":{"type":"ephemeral","ttl":300},"future_meta":{"trace":"abc-123","n":7}}`

func TestRunTurn_UnknownBlockAndMetadataReachUpstreamByteExact(t *testing.T) {
	var future types.ContentBlock
	if err := json.Unmarshal([]byte(unknownBlockJSON), &future); err != nil {
		t.Fatalf("未知块不应导致反序列化失败: %v", err)
	}

	up := &runnerUpstream{text: "好的"}
	r := newTestRunner(t, up, 2, nil)
	req := textTurn("s-unknown", "t-unknown-block", "这句话带一个未知块")
	req.Input = []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "这句话带一个未知块"},
		future,
	}
	if _, err := r.RunTurn(context.Background(), req, nil); err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}

	body := up.lastBody()
	if body == "" {
		t.Fatal("没有捕获到上游请求体")
	}
	// 类型不认识不等于可以丢：整段必须原样出现在请求里。
	if !strings.Contains(body, unknownBlockJSON) {
		t.Fatalf("未知块未逐字节透传。请求体:\n%s", body)
	}
	if !strings.Contains(body, `"cache_control":{"type":"ephemeral","ttl":300}`) {
		t.Fatalf("cache_control 未知 metadata 丢失。请求体:\n%s", body)
	}
	if !strings.Contains(body, `"future_meta":{"trace":"abc-123","n":7}`) {
		t.Fatalf("future_meta 未知 metadata 丢失。请求体:\n%s", body)
	}
}

// TestRunTurn_UnknownBlockSurvivesSecondTurnHistory 钉住"历史重发"这条更容易漏的
// 路径：第 1 轮的未知块会作为历史进入第 2 轮请求，保真必须跨轮成立。
func TestRunTurn_UnknownBlockSurvivesSecondTurnHistory(t *testing.T) {
	var future types.ContentBlock
	if err := json.Unmarshal([]byte(unknownBlockJSON), &future); err != nil {
		t.Fatal(err)
	}

	up := &runnerUpstream{text: "记得"}
	r := newTestRunner(t, up, 2, nil)
	first := textTurn("s-unknown-2", "t-u1", "先看这个块")
	first.Input = []types.ContentBlock{
		{Type: types.ContentBlockText, Text: "先看这个块"},
		future,
	}
	if _, err := r.RunTurn(context.Background(), first, nil); err != nil {
		t.Fatalf("第 1 轮失败: %v", err)
	}
	if _, err := r.RunTurn(context.Background(), textTurn("s-unknown-2", "t-u2", "继续说"), nil); err != nil {
		t.Fatalf("第 2 轮失败: %v", err)
	}

	body := up.lastBody()
	if !strings.Contains(body, unknownBlockJSON) {
		t.Fatalf("第 2 轮历史重发丢了未知块。请求体:\n%s", body)
	}
}
