package cli

import (
	"encoding/json"
	"fmt"

	"github.com/ruflo/ruflo-go/pkg/plugins"
	"github.com/spf13/cobra"
)

func newPluginsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Plugin registry under .claude-flow/plugins/",
	}
	mgr := plugins.NewPluginManager("")
	cmd.AddCommand(
		pluginsListCmd(mgr),
		pluginsInstallCmd(mgr),
		pluginsUninstallCmd(mgr),
		pluginsEnableCmd(mgr),
		pluginsDisableCmd(mgr),
	)
	return cmd
}

func pluginsListCmd(mgr *plugins.PluginManager) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed plugins",
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := mgr.List()
			if err != nil {
				return err
			}
			if OutputFormat == "json" {
				enc := json.NewEncoder(Stdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"plugins": list})
			}
			if len(list) == 0 {
				_, _ = fmt.Fprintln(Stdout(), "(no plugins)")
				return nil
			}
			for _, p := range list {
				en := "off"
				if p.Enabled {
					en = "on"
				}
				_, _ = fmt.Fprintf(Stdout(), "%s\t%s\t%s\t%s\n", p.Name, p.Version, en, p.Description)
			}
			return nil
		},
	}
}

func pluginsInstallCmd(mgr *plugins.PluginManager) *cobra.Command {
	var name, version, desc string
	c := &cobra.Command{
		Use:   "install",
		Short: "Install or update a plugin entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name required")
			}
			if version == "" {
				version = "0.0.0"
			}
			return mgr.Install(name, version, desc)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Plugin id")
	c.Flags().StringVar(&version, "version", "", "Version")
	c.Flags().StringVar(&desc, "description", "", "Description")
	return c
}

func pluginsUninstallCmd(mgr *plugins.PluginManager) *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove a plugin",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name required")
			}
			return mgr.Uninstall(name)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Plugin id")
	return c
}

func pluginsEnableCmd(mgr *plugins.PluginManager) *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "enable",
		Short: "Enable a plugin",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name required")
			}
			return mgr.Enable(name)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Plugin id")
	return c
}

func pluginsDisableCmd(mgr *plugins.PluginManager) *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "disable",
		Short: "Disable a plugin",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name required")
			}
			return mgr.Disable(name)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Plugin id")
	return c
}
