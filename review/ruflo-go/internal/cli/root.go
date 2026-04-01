// Package cli 实现 Ruflo 的命令行界面：使用 cobra 组装根命令、持久化全局 Flag（配置路径、verbose、输出格式），
// 并挂载 agent、swarm、memory、hooks 等业务子命令。多数子命令通过包内 CallTool 调用已注册的 MCP 工具，与 MCP 模式共享同一实现。
//
// 全局状态（ConfigPath、Verbose、OutputFormat、ToolRegistry）由 cmd/ruflo 在 Execute 前初始化；PersistentPreRun 在每条命令执行前刷新 Flag 到包变量。
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version 为根命令 Version 字段及 update check 等占位输出使用的内置版本号。
const version = "3.5.0"

// NewRoot 构造 ruflo 根命令：注册持久化 Flag、挂载全部子命令、绑定标准输出/错误流，并设置 SilenceUsage 等交互行为。
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:     "ruflo",
		Short:   "Ruflo V3 - AI Agent Orchestration Platform",
		Long:    "Ruflo V3 - AI Agent Orchestration Platform (Go runtime)",
		Version: version,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			Verbose, _ = cmd.Flags().GetBool("verbose")
			OutputFormat, _ = cmd.Flags().GetString("format")
			ConfigPath, _ = cmd.Flags().GetString("config")
		},
		SilenceUsage: true,
	}

	root.PersistentFlags().String("config", "", "Path to claude-flow.config.json")
	root.PersistentFlags().Bool("verbose", false, "Verbose logging")
	root.PersistentFlags().String("format", "text", "Output format: json or text")

	root.AddCommand(
		newAgentCmd(),
		newSwarmCmd(),
		newMemoryCmd(),
		newHooksCmd(),
		newNeuralCmd(),
		newSessionCmd(),
		newConfigCmd(),
		newStatusCmd(),
		newDoctorCmd(),
		newMCPCmd(),
		newTaskCmd(),
		newInitCmd(),
		newDaemonCmd(),
		newSecurityCmd(),
		newPerformanceCmd(),
		newProvidersCmd(),
		newPluginsCmd(),
		newWorkflowCmd(),
		newHiveMindCmd(),
		newStartCmd(),
		newMigrateCmd(),
		newProcessCmd(),
		newDeploymentCmd(),
		newClaimsCmd(),
		newEmbeddingsCmd(),
		newAnalyzeCmd(),
		newRouteCmd(),
		newProgressCmd(),
		newIssuesCmd(),
		newUpdateCmd(),
		newRuvectorCmd(),
		newBenchmarkCmd(),
		newGuidanceCmd(),
		newApplianceCmd(),
		newTransferCmd(),
		newCleanupCmd(),
		newAutopilotCmd(),
	)
	root.AddCommand(newCompletionsCmd(root))

	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	return root
}

// Must 若 err 非 nil 则向 stderr 打印并 os.Exit(1)，供可选的快捷错误处理路径使用。
func Must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
