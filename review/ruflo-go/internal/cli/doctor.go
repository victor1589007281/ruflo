package cli

import (
	"context"

	"github.com/spf13/cobra"
)

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
