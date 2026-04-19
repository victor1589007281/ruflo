// 动态 MCP 管理器测试
package unit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/dynmcp"
	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

func TestDynMCPManager_AddRemove(t *testing.T) {
	dir := t.TempDir()
	script := writeMockMCPScript(t, dir)

	mgr := dynmcp.NewManager()
	defer mgr.Shutdown()

	cfg := mcp.ServerConfig{Name: "test-dyn", Transport: "stdio", Command: "sh", Args: []string{script}}
	ctx := context.Background()
	if err := mgr.AddServer(ctx, cfg); err != nil {
		t.Fatalf("AddServer 失败: %v", err)
	}

	servers := mgr.ListServers()
	if len(servers) != 1 {
		t.Fatalf("期望 1 个服务器, 实际 %d", len(servers))
	}
	if servers[0].Name != "test-dyn" {
		t.Errorf("名称不匹配: %s", servers[0].Name)
	}
	if servers[0].ToolCount != 1 {
		t.Errorf("期望 1 个工具, 实际 %d", servers[0].ToolCount)
	}

	// 版本号应递增
	v1 := mgr.Version()
	if v1 < 1 {
		t.Errorf("版本号应 >= 1: %d", v1)
	}

	// 移除
	if err := mgr.RemoveServer("test-dyn"); err != nil {
		t.Fatalf("RemoveServer 失败: %v", err)
	}
	if len(mgr.ListServers()) != 0 {
		t.Error("移除后应无服务器")
	}
	v2 := mgr.Version()
	if v2 <= v1 {
		t.Errorf("版本号应递增: v1=%d, v2=%d", v1, v2)
	}

	// 移除不存在的
	if err := mgr.RemoveServer("nonexistent"); err == nil {
		t.Error("移除不存在的服务器应报错")
	}
}

func TestDynMCPManager_RefreshToolsForRegistry(t *testing.T) {
	dir := t.TempDir()
	script := writeMockMCPScript(t, dir)

	mgr := dynmcp.NewManager()
	defer mgr.Shutdown()

	mgr.AddServer(context.Background(), mcp.ServerConfig{Name: "srv-a", Transport: "stdio", Command: "sh", Args: []string{script}})

	reg := tool.NewRegistry()
	builtin.RegisterBaseTools(reg, nil)
	baseCnt := reg.Count()

	mgr.RefreshToolsForRegistry(reg)
	if reg.Count() != baseCnt+1 {
		t.Errorf("refresh 后期望 %d, 实际 %d", baseCnt+1, reg.Count())
	}

	_, ok := reg.Get("mcp_srv-a_greet")
	if !ok {
		t.Error("MCP 工具 mcp_srv-a_greet 未注册")
	}

	// 移除后 refresh
	mgr.RemoveServer("srv-a")
	mgr.RefreshToolsForRegistry(reg)
	if reg.Count() != baseCnt {
		t.Errorf("移除后 refresh 期望 %d, 实际 %d", baseCnt, reg.Count())
	}
}

func TestDynMCPManager_OnChange(t *testing.T) {
	dir := t.TempDir()
	script := writeMockMCPScript(t, dir)

	mgr := dynmcp.NewManager()
	defer mgr.Shutdown()

	called := 0
	mgr.OnChange(func() { called++ })

	mgr.AddServer(context.Background(), mcp.ServerConfig{Name: "cb-test", Transport: "stdio", Command: "sh", Args: []string{script}})
	if called != 1 {
		t.Errorf("OnChange 应被调用 1 次, 实际 %d", called)
	}

	mgr.RemoveServer("cb-test")
	if called != 2 {
		t.Errorf("OnChange 应被调用 2 次, 实际 %d", called)
	}
}

func TestDynMCPManager_ReplaceServer(t *testing.T) {
	dir := t.TempDir()
	script := writeMockMCPScript(t, dir)

	mgr := dynmcp.NewManager()
	defer mgr.Shutdown()

	cfg := mcp.ServerConfig{Name: "dup", Transport: "stdio", Command: "sh", Args: []string{script}}
	mgr.AddServer(context.Background(), cfg)
	mgr.AddServer(context.Background(), cfg) // 替换

	if len(mgr.ListServers()) != 1 {
		t.Error("替换后应仍为 1 个服务器")
	}
}

// writeMockMCPScript 写入测试用 mock MCP 服务器脚本
func writeMockMCPScript(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "mock-mcp.sh")
	content := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"serverInfo\":{\"name\":\"mock\",\"version\":\"1.0\"}}}"
      ;;
    *'"notifications/"'*) ;;
    *'"tools/list"'*)
      id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"tools\":[{\"name\":\"greet\",\"description\":\"Greet\",\"inputSchema\":{\"type\":\"object\",\"properties\":{\"name\":{\"type\":\"string\"}}}}]}}"
      ;;
    *'"tools/call"'*)
      id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"Hello!\"}]}}"
      ;;
  esac
done
`
	os.WriteFile(script, []byte(content), 0755)
	return script
}

// 确保 json 包被使用
var _ = json.Marshal
