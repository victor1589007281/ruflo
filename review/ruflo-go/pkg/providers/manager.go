package providers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/api"
)

// SelectionStrategy picks how the manager chooses a backend.
type SelectionStrategy string

const (
	StrategyRoundRobin  SelectionStrategy = "round-robin"
	StrategyLeastLoaded SelectionStrategy = "least-loaded"
	StrategyCostBased   SelectionStrategy = "cost-based"
)

// ProviderUsage tracks per-provider call volume and spend (best-effort from responses).
type ProviderUsage struct {
	Requests int64
	Tokens   int64
	Cost     float64
	Errors   int64
}

// ProviderManager registers LLM backends, selects among them, and runs a fallback chain.
type ProviderManager struct {
	mu         sync.RWMutex
	byName     map[string]LLMProvider
	order      []string
	fallback   []string
	load       map[string]*atomic.Int64
	roundRobin atomic.Uint64
	usage      map[string]*ProviderUsage
}

// NewProviderManager returns an empty manager.
func NewProviderManager() *ProviderManager {
	return &ProviderManager{
		byName: make(map[string]LLMProvider),
		load:   make(map[string]*atomic.Int64),
		usage:  make(map[string]*ProviderUsage),
	}
}

func (m *ProviderManager) ensureUsageLocked(name string) *ProviderUsage {
	u := m.usage[name]
	if u == nil {
		u = &ProviderUsage{}
		m.usage[name] = u
	}
	return u
}

func (m *ProviderManager) recordAttempt(name string, resp *api.LLMResponse, err error, estCost float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.ensureUsageLocked(name)
	u.Requests++
	if err != nil {
		u.Errors++
		return
	}
	if resp != nil {
		tok := resp.Usage.TotalTokens
		if tok <= 0 {
			tok = resp.Usage.InputTokens + resp.Usage.OutputTokens
		}
		u.Tokens += int64(tok)
		u.Cost += estCost
	}
}

// Initialize auto-registers providers when standard API keys are present in the environment.
func (m *ProviderManager) Initialize() error {
	if k := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); k != "" {
		m.RegisterProvider(&OpenAIProvider{APIKey: k})
	}
	if strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")) != "" {
		m.RegisterProvider(NewGoogleProvider())
	}
	if p := NewCohereProviderFromEnv(); strings.TrimSpace(p.APIKey) != "" {
		m.RegisterProvider(p)
	}
	if strings.TrimSpace(os.Getenv("RUVECTOR_BASE_URL")) != "" {
		m.RegisterProvider(NewRuVectorProviderFromEnv())
	}
	return nil
}

// GetUsage returns a snapshot of usage counters per provider name.
func (m *ProviderManager) GetUsage() map[string]ProviderUsage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]ProviderUsage, len(m.usage))
	for name, u := range m.usage {
		if u == nil {
			continue
		}
		out[name] = ProviderUsage{
			Requests: u.Requests,
			Tokens:   u.Tokens,
			Cost:     u.Cost,
			Errors:   u.Errors,
		}
	}
	return out
}

// ClearCache resets usage accounting and the round-robin cursor.
func (m *ProviderManager) ClearCache() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usage = make(map[string]*ProviderUsage)
	m.roundRobin.Store(0)
}

// Destroy unregisters all providers and clears internal state.
func (m *ProviderManager) Destroy() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byName = make(map[string]LLMProvider)
	m.order = nil
	m.fallback = nil
	m.load = make(map[string]*atomic.Int64)
	m.usage = make(map[string]*ProviderUsage)
	m.roundRobin.Store(0)
}

// RegisterProvider adds or replaces a provider keyed by Name().
func (m *ProviderManager) RegisterProvider(p LLMProvider) {
	if p == nil {
		return
	}
	name := p.Name()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byName[name]; !ok {
		m.order = append(m.order, name)
	}
	m.byName[name] = p
	if m.load[name] == nil {
		m.load[name] = new(atomic.Int64)
	}
}

// GetProvider returns a registered provider by name.
func (m *ProviderManager) GetProvider(name string) (LLMProvider, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.byName[strings.TrimSpace(name)]
	return p, ok
}

// ListProviders returns registered names in registration order.
func (m *ProviderManager) ListProviders() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// SetFallbackChain defines the order used by CompleteWithFallback (names must be registered).
func (m *ProviderManager) SetFallbackChain(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallback = append([]string(nil), names...)
}

// FallbackChain returns a copy of the configured fallback order.
func (m *ProviderManager) FallbackChain() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.fallback...)
}

// SelectProvider picks one backend using strategy; req is used for cost-based selection.
func (m *ProviderManager) SelectProvider(strategy string, req api.LLMRequest) (LLMProvider, error) {
	s := SelectionStrategy(strings.ToLower(strings.TrimSpace(strategy)))
	if s == "" {
		s = StrategyRoundRobin
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.order) == 0 {
		return nil, errors.New("providers: no providers registered")
	}
	switch s {
	case StrategyRoundRobin, "round_robin", "rr":
		i := m.roundRobin.Add(1) - 1
		name := m.order[int(i)%len(m.order)]
		return m.byName[name], nil
	case StrategyLeastLoaded, "least_loaded":
		var best string
		var bestLoad int64 = math.MaxInt64
		for _, name := range m.order {
			var ld int64
			if x := m.load[name]; x != nil {
				ld = x.Load()
			}
			if ld < bestLoad {
				bestLoad, best = ld, name
			}
		}
		return m.byName[best], nil
	case StrategyCostBased, "cost", "cost_based":
		var best LLMProvider
		var bestCost float64 = math.MaxFloat64
		for _, name := range m.order {
			p := m.byName[name]
			c := p.EstimateCost(req)
			if c < bestCost {
				bestCost, best = c, p
			}
		}
		return best, nil
	default:
		return nil, fmt.Errorf("providers: unknown strategy %q", strategy)
	}
}

// CompleteWithFallback tries providers in FallbackChain order (or all registered if unset).
func (m *ProviderManager) CompleteWithFallback(ctx context.Context, req api.LLMRequest) (*api.LLMResponse, error) {
	m.mu.RLock()
	chain := append([]string(nil), m.fallback...)
	if len(chain) == 0 {
		chain = append([]string(nil), m.order...)
	}
	byName := make(map[string]LLMProvider, len(m.byName))
	for k, v := range m.byName {
		byName[k] = v
	}
	m.mu.RUnlock()

	if len(chain) == 0 {
		return nil, errors.New("providers: no providers registered")
	}

	var lastErr error
	for _, name := range chain {
		p, ok := byName[name]
		if !ok {
			continue
		}
		ld := m.load[name]
		if ld != nil {
			ld.Add(1)
		}
		resp, err := p.Complete(ctx, req)
		if ld != nil {
			ld.Add(-1)
		}
		m.recordAttempt(name, resp, err, p.EstimateCost(req))
		if err == nil && resp != nil {
			return resp, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("providers: fallback exhausted")
}

// HealthCheckAll runs HealthCheck on every registered provider.
func (m *ProviderManager) HealthCheckAll(ctx context.Context) map[string]error {
	m.mu.RLock()
	names := append([]string(nil), m.order...)
	byName := make(map[string]LLMProvider, len(m.byName))
	for k, v := range m.byName {
		byName[k] = v
	}
	m.mu.RUnlock()

	out := make(map[string]error, len(names))
	for _, name := range names {
		p := byName[name]
		out[name] = p.HealthCheck(ctx)
	}
	return out
}
