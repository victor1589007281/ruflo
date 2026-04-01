// register.go：批量注册入口，将核心域工具与扩展工具包依次写入 mcp.ToolRegistry（合计 250+ 工具名）。

package tools

import (
	"fmt"
	"os"

	"github.com/ruflo/ruflo-go/mcp"
)

// RegisterAll 向 reg 注册 CLI 与外部 MCP 客户端所需的全部工具：先 InitDefaultMemory（失败仅告警），
// 再追加 agent/swarm/memory/hooks 等内置切片，最后调用各 Register*Tools 扩展注册函数。
func RegisterAll(reg *mcp.ToolRegistry) error {
	if reg == nil {
		return fmt.Errorf("tools: nil registry")
	}
	if err := InitDefaultMemory(); err != nil {
		fmt.Fprintf(os.Stderr, "ruflo: memory init warning: %v\n", err)
	}
	all := make([]*mcp.MCPTool, 0, 320)
	all = append(all, agentTools()...)
	all = append(all, swarmTools()...)
	all = append(all, memoryTools()...)
	all = append(all, hooksTools()...)
	all = append(all, neuralTools()...)
	all = append(all, taskTools()...)
	all = append(all, systemTools()...)
	all = append(all, guidanceTools()...)
	all = append(all, sessionTools()...)
	all = append(all, doctorTool(), statusTool())
	for _, t := range all {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	// Extended tool packs (file-backed under .claude-flow/)
	for _, fn := range []func(*mcp.ToolRegistry) error{
		RegisterConfigTools,
		RegisterWorkflowTools,
		RegisterHiveMindTools,
		RegisterPerformanceTools,
		RegisterSecurityTools,
		RegisterEmbeddingsTools,
		RegisterClaimsTools,
		RegisterProvidersTools,
		RegisterPluginsTools,
		RegisterTransferTools,
		RegisterBrowserTools,
		RegisterWasmTools,
		RegisterTerminalTools,
		RegisterAutopilotTools,
		RegisterGitHubTools,
		RegisterCoordinationTools,
		RegisterDAATools,
		RegisterAnalyzeTools,
		RegisterProgressTools,
		RegisterRuvLLMTools,
		RegisterAidefenceTools,
	} {
		if err := fn(reg); err != nil {
			return err
		}
	}
	globalToolRegistry = reg
	return nil
}
