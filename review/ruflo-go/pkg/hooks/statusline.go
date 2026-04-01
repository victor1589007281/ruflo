package hooks

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StatuslineData aggregates counters shown in the terminal status bar.
type StatuslineData struct {
	ActiveAgents  int           `json:"active_agents"`
	RunningTasks  int           `json:"running_tasks"`
	MemoryEntries int           `json:"memory_entries"`
	PatternsCount int           `json:"patterns_count"`
	HooksFired    int           `json:"hooks_fired"`
	WorkersActive int           `json:"workers_active"`
	SwarmStatus   string        `json:"swarm_status"`
	LastHookEvent string        `json:"last_hook_event"`
	Uptime        time.Duration `json:"uptime"`
	StartTime     time.Time     `json:"start_time"`
}

// StatuslineGenerator builds a compact status line and JSON snapshot.
type StatuslineGenerator struct {
	mu   sync.RWMutex
	data StatuslineData
}

// Update replaces the current snapshot.
func (g *StatuslineGenerator) Update(data StatuslineData) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.data = data
}

// Generate returns a single-line status string.
func (g *StatuslineGenerator) Generate() string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	d := g.data
	g.mu.RUnlock()
	swarm := d.SwarmStatus
	if swarm == "" {
		swarm = "unknown"
	}
	return fmt.Sprintf(
		"[ruflo] agents:%d tasks:%d mem:%d patterns:%d hooks:%d workers:%d swarm:%s",
		d.ActiveAgents,
		d.RunningTasks,
		d.MemoryEntries,
		d.PatternsCount,
		d.HooksFired,
		d.WorkersActive,
		swarm,
	)
}

// JSON returns a JSON encoding of the current data.
func (g *StatuslineGenerator) JSON() ([]byte, error) {
	if g == nil {
		return []byte("{}"), nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return json.Marshal(g.data)
}

// SetFromWorkerOutput merges numeric/string fields from worker stdout-style maps (snake_case or camelCase).
func (g *StatuslineGenerator) SetFromWorkerOutput(workerName string, output map[string]any) {
	if g == nil || output == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.data
	if v, ok := numField(output, "active_agents", "activeAgents"); ok {
		d.ActiveAgents = v
	}
	if v, ok := numField(output, "running_tasks", "runningTasks"); ok {
		d.RunningTasks = v
	}
	if v, ok := numField(output, "memory_entries", "memoryEntries", "mem"); ok {
		d.MemoryEntries = v
	}
	if v, ok := numField(output, "patterns_count", "patternsCount", "patterns"); ok {
		d.PatternsCount = v
	}
	if v, ok := numField(output, "hooks_fired", "hooksFired", "hooks"); ok {
		d.HooksFired = v
	}
	if v, ok := numField(output, "workers_active", "workersActive", "workers"); ok {
		d.WorkersActive = v
	}
	if s, ok := strField(output, "swarm_status", "swarmStatus", "swarm"); ok {
		d.SwarmStatus = s
	}
	if s, ok := strField(output, "last_hook_event", "lastHookEvent"); ok {
		d.LastHookEvent = s
	} else if workerName != "" {
		d.LastHookEvent = workerName
	}
	g.data = d
}

func numField(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case int:
				return t, true
			case int64:
				return int(t), true
			case float64:
				return int(t), true
			case json.Number:
				i, err := t.Int64()
				if err == nil {
					return int(i), true
				}
			case string:
				if i, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
					return i, true
				}
			}
		}
	}
	return 0, false
}

func strField(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t, true
				}
			}
		}
	}
	return "", false
}
