package hooks

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DaemonEntry tracks one background loop.
type DaemonEntry struct {
	Name     string
	Interval time.Duration
	Handler  func(ctx context.Context) error
	Running  bool
	LastRun  time.Time
	cancel   context.CancelFunc
}

// DaemonStatus is a snapshot for observability.
type DaemonStatus struct {
	Name     string        `json:"name"`
	Running  bool          `json:"running"`
	LastRun  time.Time     `json:"last_run,omitempty"`
	Interval time.Duration `json:"interval"`
}

// DaemonManager runs named periodic hooks/workers.
type DaemonManager struct {
	mu      sync.RWMutex
	daemons map[string]*DaemonEntry
	wg      sync.WaitGroup
}

// NewDaemonManager pre-registers built-in daemons with no-op handlers (replace via Register).
func NewDaemonManager() *DaemonManager {
	m := &DaemonManager{daemons: make(map[string]*DaemonEntry)}
	noop := func(context.Context) error { return nil }
	_ = m.Register("MetricsDaemon", 30*time.Second, noop)
	_ = m.Register("SwarmMonitorDaemon", 10*time.Second, noop)
	_ = m.Register("LearningDaemon", 60*time.Second, noop)
	return m
}

// Register defines or replaces a daemon configuration (not started until Start).
func (m *DaemonManager) Register(name string, interval time.Duration, handler func(ctx context.Context) error) error {
	if m == nil {
		return fmt.Errorf("hooks: nil DaemonManager")
	}
	if name == "" || interval <= 0 || handler == nil {
		return ErrInvalidRegistration
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.daemons[name]; ok && existing.Running {
		return fmt.Errorf("hooks: daemon %q is running", name)
	}
	m.daemons[name] = &DaemonEntry{Name: name, Interval: interval, Handler: handler}
	return nil
}

// Start runs the daemon loop until Stop or context cancel.
func (m *DaemonManager) Start(name string) error {
	if m == nil {
		return fmt.Errorf("hooks: nil DaemonManager")
	}
	m.mu.Lock()
	d, ok := m.daemons[name]
	if !ok || d == nil {
		m.mu.Unlock()
		return fmt.Errorf("hooks: unknown daemon %q", name)
	}
	if d.Running {
		m.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.Running = true
	h := d.Handler
	iv := d.Interval
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				m.mu.Lock()
				if ent := m.daemons[name]; ent != nil {
					ent.Running = false
				}
				m.mu.Unlock()
				return
			case <-t.C:
				runAt := time.Now().UTC()
				_ = h(ctx)
				m.mu.Lock()
				if ent := m.daemons[name]; ent != nil {
					ent.LastRun = runAt
				}
				m.mu.Unlock()
			}
		}
	}()
	return nil
}

// Stop cancels a single daemon.
func (m *DaemonManager) Stop(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	d, ok := m.daemons[name]
	if !ok || d == nil || !d.Running {
		m.mu.Unlock()
		return
	}
	if d.cancel != nil {
		d.cancel()
	}
	d.Running = false
	d.cancel = nil
	m.mu.Unlock()
}

// StartAll starts every registered daemon.
func (m *DaemonManager) StartAll() error {
	if m == nil {
		return fmt.Errorf("hooks: nil DaemonManager")
	}
	m.mu.RLock()
	names := make([]string, 0, len(m.daemons))
	for n := range m.daemons {
		names = append(names, n)
	}
	m.mu.RUnlock()
	for _, n := range names {
		if err := m.Start(n); err != nil {
			return err
		}
	}
	return nil
}

// StopAll stops every daemon.
func (m *DaemonManager) StopAll() {
	if m == nil {
		return
	}
	m.mu.RLock()
	names := make([]string, 0, len(m.daemons))
	for n := range m.daemons {
		names = append(names, n)
	}
	m.mu.RUnlock()
	for _, n := range names {
		m.Stop(n)
	}
	m.wg.Wait()
}

// Status returns a snapshot per daemon.
func (m *DaemonManager) Status() map[string]DaemonStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]DaemonStatus, len(m.daemons))
	for n, d := range m.daemons {
		if d == nil {
			continue
		}
		out[n] = DaemonStatus{
			Name:     n,
			Running:  d.Running,
			LastRun:  d.LastRun,
			Interval: d.Interval,
		}
	}
	return out
}

// IsRunning reports whether a daemon ticker loop is active.
func (m *DaemonManager) IsRunning(name string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.daemons[name]
	return ok && d != nil && d.Running
}
