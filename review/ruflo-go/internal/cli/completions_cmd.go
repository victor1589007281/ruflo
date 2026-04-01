package cli

import (
	"os"

	"github.com/spf13/cobra"
)

// 本文件实现 completions 命令组：为根命令生成各 Shell 的补全脚本，直接调用 cobra 的 Gen*Completion 写入 stdout。

// newCompletionsCmd 构建「completions」父命令，子命令 bash/zsh/fish/powershell 分别生成对应补全脚本。
func newCompletionsCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completions",
		Short: "Generate shell completion scripts",
	}
	cmd.AddCommand(
		// bash：生成 Bash 补全脚本。
		&cobra.Command{
			Use:   "bash",
			Short: "Bash completions",
			RunE: func(cmd *cobra.Command, args []string) error {
				return root.GenBashCompletion(os.Stdout)
			},
		},
		// zsh：生成 Zsh 补全脚本。
		&cobra.Command{
			Use:   "zsh",
			Short: "Zsh completions",
			RunE: func(cmd *cobra.Command, args []string) error {
				return root.GenZshCompletion(os.Stdout)
			},
		},
		// fish：生成 Fish 补全脚本（含描述）。
		&cobra.Command{
			Use:   "fish",
			Short: "Fish completions",
			RunE: func(cmd *cobra.Command, args []string) error {
				return root.GenFishCompletion(os.Stdout, true)
			},
		},
		// powershell：生成 PowerShell 补全脚本。
		&cobra.Command{
			Use:   "powershell",
			Short: "PowerShell completions",
			RunE: func(cmd *cobra.Command, args []string) error {
				return root.GenPowerShellCompletion(os.Stdout)
			},
		},
	)
	return cmd
}
