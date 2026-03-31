// Package providers implements LLM backends and routing for Ruflo.
package providers

import (
	"context"
	"io"

	"github.com/ruflo/ruflo-go/api"
)

// LLMProvider performs chat completions against a vendor API.
type LLMProvider interface {
	Name() string
	Complete(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error)
	StreamComplete(ctx context.Context, req api.LLMRequest) (io.ReadCloser, error)
	HealthCheck(ctx context.Context) error
	EstimateCost(req api.LLMRequest) float64
}

// CostRecord tracks spend for observability.
type CostRecord struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	USD      float64 `json:"usd"`
}
