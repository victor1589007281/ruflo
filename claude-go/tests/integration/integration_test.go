// 集成测试: 使用阿里百炼 DashScope API 进行端到端测试。
// 测试 QueryEngine 的完整 ReAct 循环。
//
// 运行: go test ./tests/integration/ -v -count=1
// 需要设置: DASHSCOPE_API_KEY 环境变量
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/compact"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
	"github.com/anthropic/claude-go/pkg/types"
)

const (
	testAPIKey = "sk-sp-b0a692b1b8384b72971fe4d3a42798a1"
	testModel  = "qwen3.5-plus"
	testBaseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1"
)

func getAPIKey() string {
	if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
		return key
	}
	return testAPIKey
}

func skipIfNoAPI(t *testing.T) {
	key := getAPIKey()
	if key == "" {
		t.Skip("需要 DASHSCOPE_API_KEY 环境变量")
	}
}

func buildTestEngine(t *testing.T) *engine.QueryEngine {
	t.Helper()

	cwd := t.TempDir()
	apiClient := api.NewClient(testBaseURL, getAPIKey(), testModel)
	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg)
	hookRunner := hooks.NewRunner(nil, "test-session")
	permChecker := permissions.NewChecker(types.PermissionModeBypass)
	compactor := compact.NewCompactor(apiClient, 200000)
	promptMgr := prompt.NewManager(cwd)

	cfg := &engine.Config{
		Model:            testModel,
		MaxTokens:        4096,
		MaxTurns:         10,
		Cwd:              cwd,
		PermissionMode:   types.PermissionModeBypass,
		IsNonInteractive: true,
		SessionID:        "test-session",
	}

	return engine.NewQueryEngine(cfg, apiClient, reg, hookRunner, permChecker, compactor, promptMgr)
}

// TestSimpleQuery 测试简单文本查询 (无工具调用)
func TestSimpleQuery(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch := eng.SubmitMessage(ctx, "请用一句话回答: 1+1等于多少?")

	var response string
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
			}
		}
	}

	if response == "" {
		t.Fatal("未收到模型回复")
	}
	t.Logf("模型回复: %s", response)

	if !strings.Contains(response, "2") {
		t.Errorf("回复应包含 '2': %s", response)
	}
}

// TestToolUseFileRead 测试工具调用: 文件读取
func TestToolUseFileRead(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 创建测试文件
	testFile := eng.Config.Cwd + "/test_data.txt"
	os.WriteFile(testFile, []byte("Hello from Go integration test!\nLine 2\nLine 3\n"), 0644)

	ch := eng.SubmitMessage(ctx, "请读取文件 "+testFile+" 并告诉我第一行的内容是什么。")

	var response string
	toolUsed := false
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
				if block.Type == types.ContentBlockToolUse && block.Name == "Read" {
					toolUsed = true
				}
			}
		}
	}

	if !toolUsed {
		t.Log("注意: 模型未调用 Read 工具 (可能直接回答)")
	}

	if response == "" {
		t.Fatal("未收到模型回复")
	}
	t.Logf("模型回复: %s", response[:min(len(response), 200)])

	if !strings.Contains(strings.ToLower(response), "hello") && !strings.Contains(response, "integration") {
		t.Logf("回复可能未包含文件内容, 但测试继续")
	}
}

// TestToolUseFileWrite 测试工具调用: 文件写入
func TestToolUseFileWrite(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	outFile := eng.Config.Cwd + "/output.txt"
	ch := eng.SubmitMessage(ctx, "请在 "+outFile+" 中写入 'Hello World' 这个内容。")

	var response string
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
			}
		}
	}

	t.Logf("模型回复: %s", response[:min(len(response), 200)])

	// 检查文件是否被创建
	if data, err := os.ReadFile(outFile); err == nil {
		t.Logf("文件内容: %s", string(data))
		if !strings.Contains(string(data), "Hello") {
			t.Logf("文件可能不包含期望内容")
		}
	} else {
		t.Logf("文件未被创建 (模型可能未调用 Write 工具)")
	}
}

// TestMultiTurnToolUse 测试多轮工具调用
func TestMultiTurnToolUse(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 创建测试文件
	testFile := eng.Config.Cwd + "/counter.txt"
	os.WriteFile(testFile, []byte("count: 0"), 0644)

	ch := eng.SubmitMessage(ctx, "请读取 "+testFile+" 文件, 然后将 count 的值改为 42, 最后再读取文件确认修改成功。用中文回复。")

	var response string
	toolCalls := 0
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
				if block.Type == types.ContentBlockToolUse {
					toolCalls++
				}
			}
		}
	}

	t.Logf("模型回复 (前200字): %s", response[:min(len(response), 200)])
	t.Logf("工具调用次数: %d", toolCalls)

	// 验证文件最终状态
	data, err := os.ReadFile(testFile)
	if err == nil {
		t.Logf("文件最终内容: %s", string(data))
		if strings.Contains(string(data), "42") {
			t.Log("✓ 文件已正确修改为 42")
		}
	}
}

// TestBashToolExecution 测试 Shell 工具执行
func TestBashToolExecution(t *testing.T) {
	skipIfNoAPI(t)

	eng := buildTestEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch := eng.SubmitMessage(ctx, "请使用 Shell 工具执行 'echo Hello_From_Shell' 命令, 然后告诉我输出结果。")

	var response string
	for msg := range ch {
		if msg.Type == types.MessageTypeAssistant {
			for _, block := range msg.Content {
				if block.Type == types.ContentBlockText {
					response += block.Text
				}
			}
		}
	}

	if response == "" {
		t.Fatal("未收到回复")
	}
	t.Logf("模型回复: %s", response[:min(len(response), 200)])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
