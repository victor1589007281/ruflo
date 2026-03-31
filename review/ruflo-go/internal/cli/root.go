package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "3.5.0"

// NewRoot builds the ruflo root command with persistent flags and all subcommands.
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

// Must is a tiny helper for main (optional).
func Must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
