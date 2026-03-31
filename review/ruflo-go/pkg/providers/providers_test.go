package providers

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ruflo/ruflo-go/api"
)

type mockLLM struct {
	name      string
	complete  func(context.Context, api.LLMRequest) (*api.LLMResponse, error)
	healthErr error
}

func (m *mockLLM) Name() string { return m.name }

func (m *mockLLM) Complete(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error) {
	if m.complete != nil {
		return m.complete(ctx, req)
	}
	return &api.LLMResponse{Provider: api.LLMProviderAnthropic, Model: "x", Text: "ok"}, nil
}

func (m *mockLLM) StreamComplete(context.Context, api.LLMRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (m *mockLLM) HealthCheck(context.Context) error { return m.healthErr }

func (m *mockLLM) EstimateCost(api.LLMRequest) float64 { return 0.01 }

func TestProviderManager_RegisterAndGet(t *testing.T) {
	t.Parallel()
	m := NewProviderManager()
	p := &mockLLM{name: "alpha"}
	m.RegisterProvider(p)
	got, ok := m.GetProvider("alpha")
	if !ok || got.Name() != "alpha" {
		t.Fatalf("GetProvider: ok=%v name=%q", ok, got)
	}
	if len(m.ListProviders()) != 1 {
		t.Fatalf("ListProviders: %v", m.ListProviders())
	}
}

func TestProviderManager_RoundRobin(t *testing.T) {
	t.Parallel()
	m := NewProviderManager()
	m.RegisterProvider(&mockLLM{name: "a"})
	m.RegisterProvider(&mockLLM{name: "b"})
	req := api.LLMRequest{}
	var names []string
	for i := 0; i < 6; i++ {
		p, err := m.SelectProvider("round-robin", req)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, p.Name())
	}
	want := []string{"a", "b", "a", "b", "a", "b"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("round-robin[%d] got %q want %q", i, names[i], want[i])
		}
	}
}

func TestProviderManager_FallbackChain(t *testing.T) {
	t.Parallel()
	m := NewProviderManager()
	calls := 0
	m.RegisterProvider(&mockLLM{
		name: "first",
		complete: func(context.Context, api.LLMRequest) (*api.LLMResponse, error) {
			calls++
			return nil, context.Canceled
		},
	})
	m.RegisterProvider(&mockLLM{
		name: "second",
		complete: func(context.Context, api.LLMRequest) (*api.LLMResponse, error) {
			return &api.LLMResponse{Text: "from-second"}, nil
		},
	})
	m.SetFallbackChain([]string{"first", "second"})
	resp, err := m.CompleteWithFallback(context.Background(), api.LLMRequest{})
	if err != nil || resp == nil || resp.Text != "from-second" {
		t.Fatalf("err=%v resp=%v", err, resp)
	}
	if calls != 1 {
		t.Fatalf("first provider calls=%d", calls)
	}
}

func TestProviderManager_HealthCheckAll(t *testing.T) {
	t.Parallel()
	m := NewProviderManager()
	m.RegisterProvider(&mockLLM{name: "ok"})
	m.RegisterProvider(&mockLLM{name: "bad", healthErr: context.DeadlineExceeded})
	h := m.HealthCheckAll(context.Background())
	if h["ok"] != nil {
		t.Fatalf("ok health: %v", h["ok"])
	}
	if h["bad"] == nil {
		t.Fatal("expected bad health error")
	}
}
