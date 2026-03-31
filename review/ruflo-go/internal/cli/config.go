package cli

import (
	"encoding/json"
	"fmt"

	"github.com/ruflo/ruflo-go/internal/config"
	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	var showPath bool
	c := &cobra.Command{
		Use:   "config",
		Short: "Show effective configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			if showPath {
				p, err := config.ConfigFilePath()
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintln(Stdout(), p)
				return nil
			}
			var cfg config.RufloConfig
			var err error
			if ConfigPath != "" {
				cfg, err = config.Load(ConfigPath, true)
			} else {
				cfg, err = config.LoadDefault()
			}
			if err != nil {
				return err
			}
			if OutputFormat == "json" {
				enc := json.NewEncoder(Stdout())
				enc.SetIndent("", "  ")
				return enc.Encode(cfg)
			}
			b, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(Stdout(), string(b))
			return err
		},
	}
	c.Flags().BoolVar(&showPath, "path", false, "Print resolved config file path only")
	return c
}
