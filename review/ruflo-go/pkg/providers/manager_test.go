package providers

import (
	"context"
	"testing"

	"github.com/ruflo/ruflo-go/api"
)

func TestInitialize(t *testing.T) {
	t.Parallel()
	m := NewProviderManager()
	defer m.Destroy()
	if err := m.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func TestGetUsage(t *testing.T) {
	t.Parallel()
	m := NewProviderManager()
	defer m.Destroy()
	m.RegisterProvider(&mockLLM{
		name: "usage-mock",
		complete: func(context.Context, api.LLMRequest) (*api.LLMResponse, error) {
			return &api.LLMResponse{Text: "ok", Usage: api.LLMUsage{TotalTokens: 42}}, nil
		},
	})
	_, err := m.CompleteWithFallback(context.Background(), api.LLMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	u := m.GetUsage()
	um := u["usage-mock"]
	if um.Requests != 1 || um.Tokens != 42 {
		t.Fatalf("usage: %#v", um)
	}
}

func TestClearCache(t *testing.T) {
	t.Parallel()
	m := NewProviderManager()
	m.RegisterProvider(&mockLLM{name: "c1"})
	_, _ = m.CompleteWithFallback(context.Background(), api.LLMRequest{})
	if len(m.GetUsage()) == 0 {
		t.Fatal("expected usage before clear")
	}
	m.ClearCache()
	if len(m.GetUsage()) != 0 {
		t.Fatalf("usage after clear: %#v", m.GetUsage())
	}
}

func TestDestroy(t *testing.T) {
	t.Parallel()
	m := NewProviderManager()
	m.RegisterProvider(&mockLLM{name: "d1"})
	m.Destroy()
	if len(m.ListProviders()) != 0 {
		t.Fatalf("expected no providers: %v", m.ListProviders())
	}
}
