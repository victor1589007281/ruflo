package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// 本文件实现 doctor 命令：对环境与依赖做诊断检查（配置、磁盘、运行时等），调用 doctor_check MCP 工具。

// newDoctorCmd 构建「doctor」命令，可选 --config-path 覆盖配置路径参与检查。
func newDoctorCmd() *cobra.Command {
	var configPath string
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Run environment diagnostics",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "doctor_check", map[string]any{
				"config_path": configPath,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&configPath, "config-path", "", "Optional config path override")
	return c
}
