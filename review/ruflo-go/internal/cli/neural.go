package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func newNeuralCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "neural",
		Short: "Neural pattern subsystem",
	}
	cmd.AddCommand(
		neuralTrainCmd(),
		neuralPredictCmd(),
		neuralPatternsCmd(),
		neuralStatusCmd(),
	)
	return cmd
}

func neuralTrainCmd() *cobra.Command {
	var name, desc string
	var score float64
	c := &cobra.Command{
		Use:   "train",
		Short: "Train or register a pattern",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "neural_train", map[string]any{
				"name": name, "description": desc, "score": score,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&name, "name", "", "Pattern name")
	c.Flags().StringVar(&desc, "description", "", "Description")
	c.Flags().Float64Var(&score, "score", 0, "Score")
	return c
}

func neuralPredictCmd() *cobra.Command {
	var query string
	c := &cobra.Command{
		Use:   "predict",
		Short: "Predict pattern from query",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "neural_predict", map[string]any{
				"query": query,
			})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
	c.Flags().StringVar(&query, "query", "", "Query text")
	return c
}

func neuralPatternsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "patterns",
		Short: "List patterns",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "neural_patterns", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}

func neuralStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Neural status",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := CallTool(context.Background(), "neural_status", map[string]any{})
			if err != nil {
				return err
			}
			return FprintResult(Stdout(), raw)
		},
	}
}
