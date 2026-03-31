package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// PluginInfo describes an installed or available plugin entry.
type PluginInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

type registryFile struct {
	Plugins []PluginInfo `json:"plugins"`
}

// PluginManager performs file-backed plugin registry operations.
type PluginManager struct {
	path string
	mu   sync.Mutex
}

// NewPluginManager uses .claude-flow/plugins/registry.json under baseDir (or cwd if empty).
func NewPluginManager(baseDir string) *PluginManager {
	if baseDir == "" {
		baseDir, _ = os.Getwd()
	}
	p := filepath.Join(baseDir, ".claude-flow", "plugins", "registry.json")
	return &PluginManager{path: p}
}

// NewPluginManagerWithPath sets an explicit registry file path.
func NewPluginManagerWithPath(registryPath string) *PluginManager {
	return &PluginManager{path: registryPath}
}

func (m *PluginManager) load() (registryFile, error) {
	var r registryFile
	b, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return r, err
	}
	if len(b) == 0 {
		return r, nil
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	return r, nil
}

func (m *PluginManager) save(r registryFile) error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, b, 0o644)
}

func (m *PluginManager) indexByName(r registryFile) map[string]int {
	out := make(map[string]int)
	for i, p := range r.Plugins {
		out[p.Name] = i
	}
	return out
}

// List returns all plugins from the registry file.
func (m *PluginManager) List() ([]PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load()
	if err != nil {
		return nil, err
	}
	sort.Slice(r.Plugins, func(i, j int) bool { return r.Plugins[i].Name < r.Plugins[j].Name })
	out := make([]PluginInfo, len(r.Plugins))
	copy(out, r.Plugins)
	return out, nil
}

// Install adds or updates a plugin entry (enabled by default).
func (m *PluginManager) Install(name, version, description string) error {
	if name == "" {
		return fmt.Errorf("plugins: name required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load()
	if err != nil {
		return err
	}
	idx := m.indexByName(r)
	pi := PluginInfo{Name: name, Version: version, Description: description, Enabled: true}
	if i, ok := idx[name]; ok {
		r.Plugins[i] = pi
	} else {
		r.Plugins = append(r.Plugins, pi)
	}
	return m.save(r)
}

// Uninstall removes a plugin by name.
func (m *PluginManager) Uninstall(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load()
	if err != nil {
		return err
	}
	next := r.Plugins[:0]
	for _, p := range r.Plugins {
		if p.Name != name {
			next = append(next, p)
		}
	}
	r.Plugins = next
	return m.save(r)
}

// Enable marks a plugin enabled.
func (m *PluginManager) Enable(name string) error {
	return m.setEnabled(name, true)
}

// Disable marks a plugin disabled.
func (m *PluginManager) Disable(name string) error {
	return m.setEnabled(name, false)
}

func (m *PluginManager) setEnabled(name string, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load()
	if err != nil {
		return err
	}
	idx := m.indexByName(r)
	i, ok := idx[name]
	if !ok {
		return fmt.Errorf("plugins: %q not found", name)
	}
	r.Plugins[i].Enabled = on
	return m.save(r)
}

// DefaultPath is the conventional registry path relative to the project root.
func DefaultPath() string {
	return filepath.Join(".claude-flow", "plugins", "registry.json")
}
