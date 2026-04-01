package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 claims 命令组：基于声明的协调（授予、撤销、查询、列表）；部分子命令为占位，核心路径通过 claims_* MCP 工具。

// newClaimsCmd 构建「claims」根子命令，挂载 claim、release、handoff、accept-handoff、status、list、board、rebalance。
func newClaimsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claims",
		Short: "Claims-based coordination",
	}
	cmd.AddCommand(
		claimsClaimCmd(),
		claimsReleaseCmd(),
		claimsHandoffCmd(),
		claimsAcceptHandoffCmd(),
		claimsStatusCmd(),
		claimsListCmd(),
		claimsBoardCmd(),
		claimsRebalanceCmd(),
	)
	return cmd
}

func claimsClaimCmd() *cobra.Command {
	var agent, claim string
	c := &cobra.Command{
		Use:   "claim",
		Short: "Grant a claim to an agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "claims_grant", map[string]any{"agent_id": agent, "claim": claim})
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "Agent id")
	c.Flags().StringVar(&claim, "claim", "", "Claim id")
	_ = c.MarkFlagRequired("agent")
	_ = c.MarkFlagRequired("claim")
	return c
}

func claimsReleaseCmd() *cobra.Command {
	var agent, claim string
	c := &cobra.Command{
		Use:   "release",
		Short: "Revoke a claim from an agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "claims_revoke", map[string]any{"agent_id": agent, "claim": claim})
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "Agent id")
	c.Flags().StringVar(&claim, "claim", "", "Claim id")
	_ = c.MarkFlagRequired("agent")
	_ = c.MarkFlagRequired("claim")
	return c
}

func claimsHandoffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "handoff",
		Short: "Hand off claim to another agent (placeholder)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(Stdout(), "claims handoff: placeholder — use grant/revoke via MCP")
			return err
		},
	}
}

func claimsAcceptHandoffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "accept-handoff",
		Short: "Accept a claim handoff (placeholder)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(Stdout(), "claims accept-handoff: placeholder")
			return err
		},
	}
}

func claimsStatusCmd() *cobra.Command {
	var agent, claim string
	c := &cobra.Command{
		Use:   "status",
		Short: "Check if agent holds a claim",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "claims_check", map[string]any{"agent_id": agent, "claim": claim})
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "Agent id")
	c.Flags().StringVar(&claim, "claim", "", "Claim id")
	_ = c.MarkFlagRequired("agent")
	_ = c.MarkFlagRequired("claim")
	return c
}

func claimsListCmd() *cobra.Command {
	var agent string
	c := &cobra.Command{
		Use:   "list",
		Short: "List claims",
		RunE: func(cmd *cobra.Command, args []string) error {
			argsMap := map[string]any{}
			if agent != "" {
				argsMap["agent_id"] = agent
			}
			return RunTool(cmd.Context(), "claims_list", argsMap)
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "Filter by agent id")
	return c
}

func claimsBoardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "board",
		Short: "Claims board view (placeholder)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "claims_list", map[string]any{})
		},
	}
}

// claimsRebalanceCmd 占位：重新平衡各代理持有的声明。
func claimsRebalanceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebalance",
		Short: "Rebalance claims (placeholder)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(Stdout(), "claims rebalance: placeholder")
			return err
		},
	}
}
