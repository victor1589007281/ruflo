package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Session persistence",
	}
	cmd.AddCommand(
		sessionSaveCmd(),
		sessionRestoreCmd(),
		sessionListCmd(),
		sessionDeleteCmd(),
	)
	return cmd
}

func sessionSaveCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "save",
		Short: "Save session snapshot",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "session_save", map[string]any{
				"session_id": id,
				"data":       map[string]any{},
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Session id (optional)")
	return c
}

func sessionRestoreCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "restore",
		Short: "Restore session",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "session_restore", map[string]any{
				"session_id": id,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Session id (required)")
	_ = c.MarkFlagRequired("id")
	return c
}

func sessionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "session_list", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}

func sessionDeleteCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "delete",
		Short: "Delete session",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "session_delete", map[string]any{
				"session_id": id,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Session id (required)")
	_ = c.MarkFlagRequired("id")
	return c
}
