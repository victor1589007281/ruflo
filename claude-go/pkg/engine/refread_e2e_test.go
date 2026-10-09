package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

// 端到端: 一条被压缩期折叠的 tool_result, 模型是否真能按占位里的路径读回全文?
//
// 这条链跨了两个包 (compact 落盘 / tool 读回), 而 pkg/tool 不能 import
// pkg/compact。两边目录名不一致时, 单独的包测试全绿, 线上却读不回 —— 只有把
// "折叠 -> 按占位给的路径 Read" 整条拉直才能发现。
func TestFoldedRefIsReadableThroughToolPath(t *testing.T) {
	cwd := t.TempDir()
	reg := tool.NewRegistry()
	builtin.RegisterBaseToolsWithStore(reg, builtin.NewTaskStore(t.TempDir()), nil)
	tctx := &tool.ToolContext{Cwd: cwd, MaxToolResultChars: 16000}

	// 折叠一条超阈值的 tool_result (压缩期路径)。
	original := "REF-PAYLOAD-" + strings.Repeat("Q", compact.MicroCompactThreshold+2000)
	store := compact.NewToolResultRefStore(filepath.Join(cwd, compact.ToolResultRefDir()))
	blk := types.ContentBlock{
		Type: types.ContentBlockToolResult, ToolUseID: "c1", Content: original,
	}
	stubbed := compact.ToolResultStubWithRef(blk, store)

	// 从占位里抠出路径 —— 模拟模型按它去 Read。
	// 按 `full_artifact: ` 键取值 —— 路径独占一行, 不带说明文字。
	// (先前按目录名做子串截取, 把行尾的说明一起吃进了文件名, 于是"读不回"
	//  的假红掩盖了真问题; 键值行解析没有这个歧义。)
	var refPath string
	for _, line := range strings.Split(stubbed, "\n") {
		if v, ok := strings.CutPrefix(line, "full_artifact: "); ok {
			refPath = strings.TrimSpace(v)
			break
		}
	}
	if refPath == "" {
		t.Fatalf("占位里没有引用路径:\n%s", stubbed)
	}

	// 按占位给的路径 Read。
	input, _ := json.Marshal(map[string]string{"path": refPath})
	out := tool.RunTools(context.Background(), []types.ContentBlock{{
		Type: types.ContentBlockToolUse, ID: "c2", Name: "Read", Input: input,
	}}, reg, tctx, nil)

	if len(out) == 0 || len(out[0].Content) == 0 {
		t.Fatal("Read 没返回结果")
	}
	got := out[0].Content[0].Content
	if strings.Contains(got, "未知工具") || strings.Contains(got, "不存在") || strings.Contains(got, "no such file") {
		t.Fatalf("按占位给的路径读不回: %s", got[:min(300, len(got))])
	}
	if !strings.Contains(got, "REF-PAYLOAD-") {
		t.Fatalf("读回的不是折叠前的原文:\n%s", got[:min(300, len(got))])
	}
	// 读回的内容必须**不再**被落一份新 artifact (自指保护)。
	if strings.Contains(got, "full_artifact:") {
		t.Error("读引用文件时又落了一层 artifact —— 自指保护没生效, 正文永远进不了上下文")
	}
	_ = os.Stat
}
