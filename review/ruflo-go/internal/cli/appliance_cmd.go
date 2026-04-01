package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newApplianceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "appliance",
		Short: "Appliance image build / run (placeholder)",
	}
	cmd.AddCommand(
		// build：占位，构建设备镜像。
		&cobra.Command{
			Use:   "build",
			Short: "Build appliance image",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance build: placeholder")
				return err
			},
		},
		// inspect：占位，检查清单。
		&cobra.Command{
			Use:   "inspect",
			Short: "Inspect appliance manifest",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance inspect: placeholder")
				return err
			},
		},
		// verify：占位，校验签名。
		&cobra.Command{
			Use:   "verify",
			Short: "Verify appliance signature",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance verify: placeholder")
				return err
			},
		},
		// extract：占位，解压层文件。
		&cobra.Command{
			Use:   "extract",
			Short: "Extract appliance layers",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance extract: placeholder")
				return err
			},
		},
		// run：占位，运行容器。
		&cobra.Command{
			Use:   "run",
			Short: "Run appliance container",
			RunE: func(cmd *cobra.Command, args []string) error {
				_, err := fmt.Fprintln(Stdout(), "appliance run: placeholder")
				return err
			},
		},
	)
	return cmd
}
