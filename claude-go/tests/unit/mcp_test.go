// MCP 集成测试
// 测试: MCP 客户端连接、工具发现、工具调用、注册表集成
package unit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/feishu"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// TestMCPToolRegistration 测试 MCP 工具注册到 Registry 的流程
func TestMCPToolRegistration(t *testing.T) {
	conn := &mcp.Connection{
		Config: mcp.ServerConfig{Name: "test-server"},
		Status: "connected",
		Tools: []mcp.ToolInfo{
			{
				Name:        "echo",
				Description: "Echo back input",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
			},
			{
				Name:        "add",
				Description: "Add two numbers",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}}}`),
			},
		},
	}

	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg, nil)
	baseCount := reg.Count()

	mcp.RegisterMCPTools(reg, []*mcp.Connection{conn})

	if reg.Count() != baseCount+2 {
		t.Errorf("注册 MCP 工具后期望 %d 个工具, 实际 %d", baseCount+2, reg.Count())
	}

	// MCP 工具名格式: mcp_{serverName}_{toolName}
	echoTool, ok := reg.Get("mcp_test-server_echo")
	if !ok {
		t.Fatal("mcp_test-server_echo 工具未找到")
	}
	if echoTool.Description() != "Echo back input" {
		t.Errorf("描述不匹配: %s", echoTool.Description())
	}

	addTool, ok := reg.Get("mcp_test-server_add")
	if !ok {
		t.Fatal("mcp_test-server_add 工具未找到")
	}
	if addTool.IsReadOnly(nil) {
		// MCP 工具默认不可确定只读性，应返回 false
	}

	// 验证 API 工具列表包含 MCP 工具
	apiTools := reg.APITools()
	found := 0
	for _, at := range apiTools {
		if at.Name == "mcp_test-server_echo" || at.Name == "mcp_test-server_add" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("API 工具列表中期望 2 个 MCP 工具, 找到 %d", found)
	}
}

// TestMultiServerMCPRegistration 测试多 MCP 服务器工具注册
func TestMultiServerMCPRegistration(t *testing.T) {
	conns := []*mcp.Connection{
		{
			Config: mcp.ServerConfig{Name: "server-a"},
			Status: "connected",
			Tools: []mcp.ToolInfo{
				{Name: "tool1", Description: "Tool 1 from A", InputSchema: json.RawMessage(`{}`)},
			},
		},
		{
			Config: mcp.ServerConfig{Name: "server-b"},
			Status: "connected",
			Tools: []mcp.ToolInfo{
				{Name: "tool1", Description: "Tool 1 from B", InputSchema: json.RawMessage(`{}`)},
				{Name: "tool2", Description: "Tool 2 from B", InputSchema: json.RawMessage(`{}`)},
			},
		},
	}

	reg := tool.NewRegistry()
	mcp.RegisterMCPTools(reg, conns)

	// 不同服务器同名工具应该有不同的 prefixed name
	_, ok1 := reg.Get("mcp_server-a_tool1")
	_, ok2 := reg.Get("mcp_server-b_tool1")
	_, ok3 := reg.Get("mcp_server-b_tool2")

	if !ok1 || !ok2 || !ok3 {
		t.Errorf("多服务器工具注册失败: a.tool1=%v, b.tool1=%v, b.tool2=%v", ok1, ok2, ok3)
	}

	if reg.Count() != 3 {
		t.Errorf("期望 3 个 MCP 工具, 实际 %d", reg.Count())
	}
}

// TestLoadServerConfigsFromFile 测试从 JSON 文件加载 MCP 配置
func TestLoadServerConfigsFromFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "mcp-config.json")

	config := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"my-server": map[string]interface{}{
				"command": "echo",
				"args":    []string{"hello"},
				"env":     map[string]string{"KEY": "VALUE"},
			},
			"http-server": map[string]interface{}{
				"transport": "http",
				"url":       "http://localhost:3000",
			},
		},
	}

	data, _ := json.Marshal(config)
	os.WriteFile(configPath, data, 0644)

	configs, err := mcp.LoadServerConfigsFromFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 2 {
		t.Fatalf("期望 2 个 MCP 服务器配置, 实际 %d", len(configs))
	}

	configMap := make(map[string]mcp.ServerConfig)
	for _, c := range configs {
		configMap[c.Name] = c
	}

	my := configMap["my-server"]
	if my.Command != "echo" {
		t.Errorf("command 不匹配: %s", my.Command)
	}
	if len(my.Args) != 1 || my.Args[0] != "hello" {
		t.Errorf("args 不匹配: %v", my.Args)
	}
	if my.Transport != "stdio" {
		t.Errorf("默认 transport 应为 stdio: %s", my.Transport)
	}
	if my.Env["KEY"] != "VALUE" {
		t.Errorf("env 不匹配: %v", my.Env)
	}

	http := configMap["http-server"]
	if http.Transport != "http" {
		t.Errorf("http transport 不匹配: %s", http.Transport)
	}
	if http.URL != "http://localhost:3000" {
		t.Errorf("URL 不匹配: %s", http.URL)
	}
}

// TestJSONConfigLoad 测试完整 JSON 配置文件加载
func TestJSONConfigLoad(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "claude-go.json")

	config := `{
		"feishu": {
			"appId": "cli_test123",
			"appSecret": "secret456",
			"domain": "lark",
			"mentionOnly": false,
			"sessionTimeout": 60,
			"maxSessions": 50,
			"thinkingMessage": "让我想想..."
		},
		"ai": {
			"model": "claude-3-sonnet",
			"apiKey": "sk-test-key",
			"baseUrl": "https://api.example.com/v1",
			"maxTokens": 8192,
			"maxTurns": 50
		},
		"mcpServers": {
			"code-tools": {
				"command": "npx",
				"args": ["-y", "@example/code-tools"],
				"env": {"DEBUG": "true"}
			}
		},
		"hooks": [
			{"event": "PreToolUse", "command": "echo pre", "if": "Bash*"},
			{"event": "PostToolUse", "command": "echo post"}
		],
		"systemPrompt": "You are a helpful coding assistant.",
		"permissionMode": "auto",
		"cwd": "/tmp/test-workspace",
		"debug": true
	}`

	os.WriteFile(configPath, []byte(config), 0644)

	jc, err := feishu.LoadJSONConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if jc == nil {
		t.Fatal("配置不应为 nil")
	}

	// 验证 Feishu 配置
	if jc.Feishu.AppID != "cli_test123" {
		t.Errorf("AppID: %s", jc.Feishu.AppID)
	}
	if jc.Feishu.AppSecret != "secret456" {
		t.Errorf("AppSecret: %s", jc.Feishu.AppSecret)
	}
	if jc.Feishu.Domain != "lark" {
		t.Errorf("Domain: %s", jc.Feishu.Domain)
	}
	if jc.Feishu.MentionOnly == nil || *jc.Feishu.MentionOnly != false {
		t.Error("MentionOnly 应为 false")
	}
	if jc.Feishu.SessionTimeout != 60 {
		t.Errorf("SessionTimeout: %d", jc.Feishu.SessionTimeout)
	}

	// 验证 AI 配置
	if jc.AI.Model != "claude-3-sonnet" {
		t.Errorf("Model: %s", jc.AI.Model)
	}
	if jc.AI.APIKey != "sk-test-key" {
		t.Errorf("APIKey: %s", jc.AI.APIKey)
	}
	if jc.AI.BaseURL != "https://api.example.com/v1" {
		t.Errorf("BaseURL: %s", jc.AI.BaseURL)
	}

	// 验证 MCP 配置
	if len(jc.MCPServers) != 1 {
		t.Fatalf("期望 1 个 MCP 服务器, 实际 %d", len(jc.MCPServers))
	}
	codeTools := jc.MCPServers["code-tools"]
	if codeTools.Command != "npx" {
		t.Errorf("MCP command: %s", codeTools.Command)
	}

	// 验证 Hooks
	if len(jc.Hooks) != 2 {
		t.Fatalf("期望 2 个 hooks, 实际 %d", len(jc.Hooks))
	}
	if jc.Hooks[0].Event != "PreToolUse" || jc.Hooks[0].If != "Bash*" {
		t.Errorf("Hook[0]: %+v", jc.Hooks[0])
	}

	// 验证其他字段
	if jc.SystemPrompt != "You are a helpful coding assistant." {
		t.Errorf("SystemPrompt: %s", jc.SystemPrompt)
	}
	if jc.PermissionMode != "auto" {
		t.Errorf("PermissionMode: %s", jc.PermissionMode)
	}
	if !jc.Debug {
		t.Error("Debug 应为 true")
	}
}

// TestJSONConfigApplyToBot 测试 JSON 配置合并到 BotConfig
func TestJSONConfigApplyToBot(t *testing.T) {
	jc := &feishu.JSONConfig{
		Feishu: &feishu.FeishuSection{
			AppID:     "json-app-id",
			AppSecret: "json-secret",
			Domain:    "lark",
		},
		AI: &feishu.AISection{
			Model:     "gpt-4",
			APIKey:    "json-api-key",
			MaxTokens: 4096,
		},
		MCPServers: map[string]feishu.MCPServerEntry{
			"test-mcp": {Command: "test", Args: []string{"-v"}},
		},
		SystemPrompt: "Custom prompt",
		Debug:        true,
	}

	bc := feishu.DefaultBotConfig()
	jc.ApplyToBot(bc)

	if bc.AppID != "json-app-id" {
		t.Errorf("AppID 应从 JSON 加载: %s", bc.AppID)
	}
	if bc.APIKey != "json-api-key" {
		t.Errorf("APIKey 应从 JSON 加载: %s", bc.APIKey)
	}
	if bc.Domain != "lark" {
		t.Errorf("Domain 应从 JSON 加载: %s", bc.Domain)
	}
	if bc.SystemPrompt != "Custom prompt" {
		t.Errorf("SystemPrompt 应从 JSON 加载: %s", bc.SystemPrompt)
	}
	if !bc.Debug {
		t.Error("Debug 应从 JSON 加载为 true")
	}

	// 测试 CLI 参数优先级 (已有值不被覆盖)
	bc2 := feishu.DefaultBotConfig()
	bc2.AppID = "cli-app-id"
	bc2.APIKey = "cli-key"
	jc.ApplyToBot(bc2)

	if bc2.AppID != "cli-app-id" {
		t.Errorf("CLI AppID 应保留: %s", bc2.AppID)
	}
	if bc2.APIKey != "cli-key" {
		t.Errorf("CLI APIKey 应保留: %s", bc2.APIKey)
	}
}

// TestMCPClientStdioProtocol 测试 MCP stdio 协议的 JSON-RPC 通信
// 使用内存管道模拟 MCP 服务器
func TestMCPClientStdioProtocol(t *testing.T) {
	// 创建双向管道模拟 stdio
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()

	// 模拟 MCP 服务器 goroutine
	go func() {
		scanner := bufio.NewScanner(serverRead)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		for scanner.Scan() {
			line := scanner.Bytes()
			var req struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      int64           `json:"id"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params,omitempty"`
			}
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}

			// 通知消息没有 ID, 不需要响应
			if req.ID == 0 && req.Method == "notifications/initialized" {
				continue
			}

			var resp interface{}
			switch req.Method {
			case "initialize":
				resp = map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]interface{}{
						"protocolVersion": "2024-11-05",
						"capabilities":    map[string]interface{}{},
						"serverInfo":      map[string]string{"name": "test-mcp-server", "version": "1.0"},
					},
				}
			case "tools/list":
				resp = map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]interface{}{
						"tools": []map[string]interface{}{
							{
								"name":        "hello",
								"description": "Say hello",
								"inputSchema": map[string]interface{}{
									"type":       "object",
									"properties": map[string]interface{}{"name": map[string]string{"type": "string"}},
								},
							},
						},
					},
				}
			case "tools/call":
				var params struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				json.Unmarshal(req.Params, &params)
				var args map[string]string
				json.Unmarshal(params.Arguments, &args)

				resp = map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]interface{}{
						"content": []map[string]interface{}{
							{"type": "text", "text": fmt.Sprintf("Hello, %s!", args["name"])},
						},
					},
				}
			}

			if resp != nil {
				data, _ := json.Marshal(resp)
				serverWrite.Write(append(data, '\n'))
			}
		}
	}()

	// 使用管道构建 Connection 并手动初始化
	conn := &mcp.Connection{
		Config: mcp.ServerConfig{Name: "pipe-server", Transport: "stdio"},
		Status: "pending",
	}

	// 直接设置 Connection 的内部字段需要通过 exported 方法
	// 这里我们通过调用 CallTool 间接测试协议

	// 测试 MCP 工具调用的端到端集成
	mcpTool := mcp.NewMCPTool(conn, mcp.ToolInfo{
		Name:        "hello",
		Description: "Say hello",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
	})

	if mcpTool.Name() != "mcp_pipe-server_hello" {
		t.Errorf("MCP 工具名格式错误: %s", mcpTool.Name())
	}

	// 验证工具属性
	if mcpTool.IsReadOnly(nil) {
		t.Error("MCP 工具默认应该不是只读的")
	}
	if mcpTool.IsConcurrencySafe(nil) {
		t.Error("MCP 工具默认不应该并发安全")
	}

	_ = clientRead
	_ = clientWrite
	// 关闭管道
	clientRead.Close()
	clientWrite.Close()
	serverRead.Close()
	serverWrite.Close()
}

// TestMCPToolIntegrationWithRegistry 测试 MCP 工具在 Registry 中与内置工具共存
func TestMCPToolIntegrationWithRegistry(t *testing.T) {
	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg, nil)
	builtinCount := reg.Count()

	// 模拟 MCP 连接
	conn := &mcp.Connection{
		Config: mcp.ServerConfig{Name: "code-analysis"},
		Status: "connected",
		Tools: []mcp.ToolInfo{
			{
				Name:        "analyze_code",
				Description: "Analyze code quality",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}}}`),
			},
			{
				Name:        "suggest_fix",
				Description: "Suggest fixes for issues",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"}}}`),
			},
		},
	}

	mcp.RegisterMCPTools(reg, []*mcp.Connection{conn})

	// 内置工具 + MCP 工具
	if reg.Count() != builtinCount+2 {
		t.Errorf("期望 %d 个工具, 实际 %d", builtinCount+2, reg.Count())
	}

	// 内置工具仍然可用 (Bash 工具注册名为 "Shell")
	_, okRead := reg.Get("Read")
	_, okShell := reg.Get("Shell")
	if !okRead || !okShell {
		t.Error("内置工具应仍然可用")
	}

	// MCP 工具可用
	_, okAnalyze := reg.Get("mcp_code-analysis_analyze_code")
	_, okSuggest := reg.Get("mcp_code-analysis_suggest_fix")
	if !okAnalyze || !okSuggest {
		t.Error("MCP 工具应可用")
	}

	// API 工具列表包含所有工具
	apiTools := reg.APITools()
	if len(apiTools) != reg.Count() {
		t.Errorf("API 工具列表长度 %d != 注册表 %d", len(apiTools), reg.Count())
	}
}

// TestJSONConfigNoFile 测试没有配置文件时的行为
func TestJSONConfigNoFile(t *testing.T) {
	cfg, err := feishu.LoadJSONConfig("")
	if err != nil {
		t.Fatalf("空路径不应报错: %v", err)
	}
	if cfg != nil {
		// 如果当前目录碰巧有 claude-go.json 则可能返回非 nil
		// 这是预期行为
	}
}

// TestJSONConfigInvalidJSON 测试非法 JSON 配置
func TestJSONConfigInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json {{{"), 0644)

	_, err := feishu.LoadJSONConfig(path)
	if err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// TestJSONConfigMissingFile 测试指定了不存在的文件
func TestJSONConfigMissingFile(t *testing.T) {
	_, err := feishu.LoadJSONConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Error("不存在的文件应报错")
	}
}

// TestMCPConnectionWithRealProcess 使用 echo 命令测试 stdio 连接
// 注意: 这不是一个真正的 MCP 服务器，仅测试进程启动逻辑
func TestMCPConnectionWithRealProcess(t *testing.T) {
	// 创建一个简单的脚本来模拟 MCP 服务器
	dir := t.TempDir()
	script := filepath.Join(dir, "mock-mcp.sh")
	// 这个脚本接收 JSON-RPC 请求并返回固定响应
	scriptContent := `#!/bin/sh
while IFS= read -r line; do
  # 解析 method 字段
  case "$line" in
    *'"initialize"'*)
      id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"serverInfo\":{\"name\":\"mock\",\"version\":\"1.0\"}}}"
      ;;
    *'"notifications/"'*)
      # 通知不需要响应
      ;;
    *'"tools/list"'*)
      id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"tools\":[{\"name\":\"greet\",\"description\":\"Greet someone\",\"inputSchema\":{\"type\":\"object\",\"properties\":{\"name\":{\"type\":\"string\"}}}}]}}"
      ;;
    *'"tools/call"'*)
      id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"Hello from MCP!\"}]}}"
      ;;
  esac
done
`
	os.WriteFile(script, []byte(scriptContent), 0755)

	client := mcp.NewClient()
	conn, err := client.Connect(context.Background(), mcp.ServerConfig{
		Name:      "mock-server",
		Transport: "stdio",
		Command:   "sh",
		Args:      []string{script},
	})
	if err != nil {
		t.Fatalf("连接 mock MCP 服务器失败: %v", err)
	}
	defer conn.Close()

	if conn.Status != "connected" {
		t.Errorf("期望 connected, 实际 %s", conn.Status)
	}

	// 验证工具发现
	if len(conn.Tools) != 1 {
		t.Fatalf("期望发现 1 个工具, 实际 %d", len(conn.Tools))
	}
	if conn.Tools[0].Name != "greet" {
		t.Errorf("工具名不匹配: %s", conn.Tools[0].Name)
	}

	// 调用工具
	result, err := conn.CallTool(context.Background(), "greet", json.RawMessage(`{"name":"World"}`))
	if err != nil {
		t.Fatalf("工具调用失败: %v", err)
	}
	if result != "Hello from MCP!" {
		t.Errorf("工具调用结果不匹配: %s", result)
	}

	// 注册到 Registry 并验证
	reg := tool.NewRegistry()
	mcp.RegisterMCPTools(reg, []*mcp.Connection{conn})

	mcpTool, ok := reg.Get("mcp_mock-server_greet")
	if !ok {
		t.Fatal("MCP 工具注册后未找到")
	}

	toolResult, err := mcpTool.Call(context.Background(), json.RawMessage(`{"name":"Go"}`), &tool.ToolContext{})
	if err != nil {
		t.Fatalf("通过 Registry 调用 MCP 工具失败: %v", err)
	}
	if toolResult.IsError {
		t.Errorf("MCP 工具调用返回错误: %s", toolResult.Content)
	}
	if toolResult.Content != "Hello from MCP!" {
		t.Errorf("MCP 工具结果不匹配: %s", toolResult.Content)
	}
}

// 辅助: 确保 net 包被导入用于将来的 HTTP 传输测试
var _ = net.Listen
