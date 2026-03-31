package hooks

import (
	"sort"
	"sync"
)

// HookRegistryStats summarizes registry contents.
type HookRegistryStats struct {
	TotalRegistered int
	EnabledCount    int
	DisabledCount   int
	EventCounts     map[HookEvent]int
}

// HookRegistry stores hook registrations by event and global name index.
type HookRegistry struct {
	mu      sync.RWMutex
	byEvent map[HookEvent][]HookRegistration
	byName  map[string]HookRegistration
}

// NewRegistry returns an empty registry.
func NewRegistry() *HookRegistry {
	return &HookRegistry{
		byEvent: make(map[HookEvent][]HookRegistration),
		byName:  make(map[string]HookRegistration),
	}
}

// Register adds a hook. Name must be unique across all events.
func (r *HookRegistry) Register(event HookEvent, handler HookHandler, priority int, name string) error {
	if handler == nil || name == "" {
		return ErrInvalidRegistration
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; exists {
		return ErrDuplicateName
	}
	reg := HookRegistration{Event: event, Handler: handler, Priority: priority, Name: name, Enabled: true}
	r.byName[name] = reg
	r.byEvent[event] = append(r.byEvent[event], reg)
	return nil
}

// Unregister removes a hook by its unique name.
func (r *HookRegistry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.byName[name]
	if !ok {
		return false
	}
	delete(r.byName, name)
	list := r.byEvent[reg.Event]
	out := list[:0]
	for _, x := range list {
		if x.Name != name {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		delete(r.byEvent, reg.Event)
	} else {
		r.byEvent[reg.Event] = out
	}
	return true
}

// GetForEvent returns enabled hooks for an event sorted by descending priority (higher runs first).
func (r *HookRegistry) GetForEvent(event HookEvent) []HookRegistration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.byEvent[event]
	var cp []HookRegistration
	for _, reg := range list {
		if reg.Enabled {
			cp = append(cp, reg)
		}
	}
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].Priority != cp[j].Priority {
			return cp[i].Priority > cp[j].Priority
		}
		return cp[i].Name < cp[j].Name
	})
	return cp
}

// Get returns a registration copy by unique name.
func (r *HookRegistry) Get(name string) (*HookRegistration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.byName[name]
	if !ok {
		return nil, false
	}
	cp := reg
	return &cp, true
}

// Enable turns a hook on by name.
func (r *HookRegistry) Enable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.byName[name]
	if !ok {
		return ErrHookNotFound
	}
	reg.Enabled = true
	r.byName[name] = reg
	r.syncEnabledInEventListLocked(reg.Event, name, true)
	return nil
}

// Disable turns a hook off by name (excluded from GetForEvent).
func (r *HookRegistry) Disable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.byName[name]
	if !ok {
		return ErrHookNotFound
	}
	reg.Enabled = false
	r.byName[name] = reg
	r.syncEnabledInEventListLocked(reg.Event, name, false)
	return nil
}

func (r *HookRegistry) syncEnabledInEventListLocked(ev HookEvent, name string, enabled bool) {
	list := r.byEvent[ev]
	for i := range list {
		if list[i].Name == name {
			list[i].Enabled = enabled
			r.byEvent[ev] = list
			return
		}
	}
}

// Has reports whether a hook name is registered.
func (r *HookRegistry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byName[name]
	return ok
}

// Size returns the number of registered hooks.
func (r *HookRegistry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// Clear removes all registrations.
func (r *HookRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName = make(map[string]HookRegistration)
	r.byEvent = make(map[HookEvent][]HookRegistration)
}

// GetStats returns aggregate registry statistics.
func (r *HookRegistry) GetStats() HookRegistryStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := HookRegistryStats{
		TotalRegistered: len(r.byName),
		EventCounts:     make(map[HookEvent]int),
	}
	for _, reg := range r.byName {
		if reg.Enabled {
			st.EnabledCount++
		} else {
			st.DisabledCount++
		}
		st.EventCounts[reg.Event]++
	}
	return st
}

// List returns all registrations (arbitrary order).
func (r *HookRegistry) List() []HookRegistration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]HookRegistration, 0, len(r.byName))
	for _, reg := range r.byName {
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
