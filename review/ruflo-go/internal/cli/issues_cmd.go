package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 本文件实现 issues 命令组：Issue 认领/释放/列表等占位，与外部工单系统集成前的 CLI 桩。

// newIssuesCmd 构建「issues」根子命令，内联 claim、release、list。
func newIssuesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "issues",
		Short: "Issue tracker helpers (placeholder)",
	}
	cmd.AddCommand(
		// claim：占位，需要 issue id 参数。
		&cobra.Command{
			Use:   "claim",
			Short: "Claim an issue id",
			RunE: func(cmd *cobra.Command, args []string) error {
				if len(args) < 1 {
					return fmt.Errorf("issue id required")
				}
				_, err := fmt.Fprintf(Stdout(), "issues claim %s: placeholder\n", args[0])
				return err
			},
		},
		// release：占位，释放工单认领。
		&cobra.Command{
			Use:   "release",
			Short: "Release issue claim",
			RunE: func(cmd *cobra.Command, args []string) error {
				if len(args) < 1 {
					return fmt.Errorf("issue id required")
				}
				_, err := fmt.Fprintf(Stdout(), "issues release %s: placeholder\n", args[0])
				return err
			},
		},
		// list：占位，列出工单。
		&cobra.Command{
			Use:   "list",
			Short: "List issues (placeholder)",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "issues list: placeholder")
				return err
			},
		},
	)
	return cmd
}
