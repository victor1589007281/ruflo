package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ruflo/ruflo-go/internal/config"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var wizard bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Initialize a Claude Flow / Ruflo project layout",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			cfg := config.Default()
			if wizard {
				r := bufio.NewReader(os.Stdin)
				fmt.Fprint(Stdout(), "Topology [hierarchical]: ")
				if s, _ := r.ReadString('\n'); strings.TrimSpace(s) != "" {
					cfg.Topology = strings.TrimSpace(s)
				}
				fmt.Fprint(Stdout(), "Max agents [8]: ")
				if s, _ := r.ReadString('\n'); strings.TrimSpace(s) != "" {
					if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
						cfg.MaxAgents = n
					}
				}
				fmt.Fprint(Stdout(), "Memory backend [hybrid]: ")
				if s, _ := r.ReadString('\n'); strings.TrimSpace(s) != "" {
					cfg.MemoryBackend = strings.TrimSpace(s)
				}
			}

			flowDir := filepath.Join(wd, ".claude-flow")
			subdirs := []string{
				"agents",
				"swarm",
				"memory",
			}
			for _, d := range subdirs {
				if err := os.MkdirAll(filepath.Join(flowDir, d), 0o755); err != nil {
					return fmt.Errorf("mkdir %s: %w", d, err)
				}
			}

			cfgPath := filepath.Join(wd, "claude-flow.config.json")
			if ConfigPath != "" {
				cfgPath = ConfigPath
			}
			b, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
				return fmt.Errorf("write config: %w", err)
			}
			_, _ = fmt.Fprintf(Stdout(), "Initialized %s and %s\n", flowDir, cfgPath)
			return nil
		},
	}
	c.Flags().BoolVar(&wizard, "wizard", false, "Interactive prompts for topology, max agents, and memory backend")
	return c
}
