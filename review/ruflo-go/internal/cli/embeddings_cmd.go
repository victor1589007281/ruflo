package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newEmbeddingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "embeddings",
		Short: "Vector embeddings tools",
	}
	cmd.AddCommand(
		embeddingsEmbedCmd(),
		embeddingsBatchCmd(),
		embeddingsSearchCmd(),
		embeddingsCompareCmd(),
		embeddingsStatusCmd(),
	)
	return cmd
}

func embeddingsEmbedCmd() *cobra.Command {
	var text string
	c := &cobra.Command{
		Use:   "embed",
		Short: "Embed a single text",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "embeddings_generate", map[string]any{"text": text})
		},
	}
	c.Flags().StringVar(&text, "text", "", "Text to embed")
	_ = c.MarkFlagRequired("text")
	return c
}

func embeddingsBatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "batch",
		Short: "Batch embed texts (pass as args)",
		RunE: func(cmd *cobra.Command, args []string) error {
			texts := make([]any, len(args))
			for i, a := range args {
				texts[i] = a
			}
			return RunTool(cmd.Context(), "embeddings_batch", map[string]any{"texts": texts})
		},
	}
}

func embeddingsSearchCmd() *cobra.Command {
	var query string
	var k int
	c := &cobra.Command{
		Use:   "search",
		Short: "Semantic search over stored embeddings",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "embeddings_search", map[string]any{"query": query, "k": k})
		},
	}
	c.Flags().StringVar(&query, "query", "", "Query text")
	c.Flags().IntVar(&k, "k", 5, "Top k")
	_ = c.MarkFlagRequired("query")
	return c
}

func embeddingsCompareCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compare",
		Short: "Compare two texts via embedding similarity (placeholder)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("compare requires two text arguments")
			}
			_, err := fmt.Fprintf(Stdout(), "embeddings compare: stub — texts %q vs %q (use search/embed via MCP for full pipeline)\n", args[0], args[1])
			return err
		},
	}
}

func embeddingsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Embedding service status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunTool(cmd.Context(), "embeddings_init", map[string]any{})
		},
	}
}
