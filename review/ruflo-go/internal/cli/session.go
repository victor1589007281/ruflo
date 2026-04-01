package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// 本文件实现 session 命令组：会话的创建、结束、保存、恢复、列举与删除，对应 session_save/delete/restore/list 等 MCP 工具。

// newSessionCmd 构建「session」根子命令，挂载 start、end、save、restore、list、delete。
func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Session persistence",
	}
	cmd.AddCommand(
		sessionStartCmd(),
		sessionEndCmd(),
		sessionSaveCmd(),
		sessionRestoreCmd(),
		sessionListCmd(),
		sessionDeleteCmd(),
	)
	return cmd
}

func sessionStartCmd() *cobra.Command {
	var id string
	c := &cobra.Command{
		Use:   "start",
		Short: "Start / create session (session_save)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(cmd.Context(), "session_save", map[string]any{
				"session_id": id,
				"data":       map[string]any{},
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Session id (optional; server may assign if empty)")
	return c
}

func sessionEndCmd() *cobra.Command {
	var id string
	var exportMetrics bool
	c := &cobra.Command{
		Use:   "end",
		Short: "End session (session_delete)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = exportMetrics // accepted for TS CLI parity; end is session_delete only
			raw, err := CallTool(cmd.Context(), "session_delete", map[string]any{
				"session_id": id,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&id, "id", "", "Session id")
	c.Flags().BoolVar(&exportMetrics, "export-metrics", false, "Run session-end hook with metrics before delete")
	_ = c.MarkFlagRequired("id")
	return c
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
