// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"os/exec"
	"regexp"
	"sync"
)

// ToolDetector runtime tool discovery.
// Scans the environment at Agent Harness initialization to build a capability map.
type ToolDetector struct {
	mu    sync.RWMutex
	cache map[string]*ToolCapability // tool name -> capability
}

// ToolCapability describes a tool's capability.
type ToolCapability struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Path      string `json:"path,omitempty"`
	OutputFmt string `json:"output_fmt"` // "json", "text", "checkstyle"
	Priority  int    `json:"priority"`   // 1 = highest
}

// NewToolDetector creates a new ToolDetector.
func NewToolDetector() *ToolDetector {
	return &ToolDetector{cache: make(map[string]*ToolCapability)}
}

// Scan scans the environment for all known native tools.
func (d *ToolDetector) Scan(ctx context.Context) {
	tools := []struct {
		name      string
		checkCmd  []string
		verRegexp string
		outputFmt string
		priority  int
	}{
		{"staticcheck", []string{"staticcheck", "-version"}, `staticcheck ([\d.]+)`, "text", 1},
		{"golangci-lint", []string{"golangci-lint", "--version"}, `[\d.]+`, "text", 2},
		{"go", []string{"go", "version"}, `go([\d.]+)`, "text", 0}, // go always available
		{"gofmt", []string{"gofmt", "--help"}, ``, "text", 0},
		{"goimports", []string{"goimports", "--help"}, ``, "text", 0},
		{"gopls", []string{"gopls", "version"}, `[\d.]+`, "text", 0},
	}

	var wg sync.WaitGroup
	for _, t := range tools {
		wg.Add(1)
		go func(t struct {
			name      string
			checkCmd  []string
			verRegexp string
			outputFmt string
			priority  int
		}) {
			defer wg.Done()
			cap := d.probe(ctx, t.name, t.checkCmd, t.verRegexp, t.outputFmt, t.priority)
			d.mu.Lock()
			d.cache[t.name] = cap
			d.mu.Unlock()
		}(t)
	}
	wg.Wait()
}

func (d *ToolDetector) probe(ctx context.Context, name string, cmd []string, verRe, outputFmt string, priority int) *ToolCapability {
	cap := &ToolCapability{Name: name, OutputFmt: outputFmt, Priority: priority}

	path, err := exec.LookPath(cmd[0])
	if err != nil {
		cap.Available = false
		return cap
	}
	cap.Available = true
	cap.Path = path

	// Get version
	if verRe != "" {
		out, _ := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		if m := regexp.MustCompile(verRe).FindStringSubmatch(string(out)); len(m) > 1 {
			cap.Version = m[1]
		}
	}
	return cap
}

// Get queries tool availability.
func (d *ToolDetector) Get(name string) (*ToolCapability, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.cache[name]
	return c, ok
}

// Resolve resolves the best available tool by capability name.
// For example Resolve("static-check") -> staticcheck/golangci-lint/go vet
func (d *ToolDetector) Resolve(capability string) *ToolCapability {
	chains := map[string][]string{
		"static-check": {"staticcheck", "golangci-lint", "go-vet"},
		"format":       {"gofmt", "goimports"},
		"build":        {"go"},
		"import-fix":   {"goimports", "gofmt"},
	}
	candidates, ok := chains[capability]
	if !ok {
		return nil
	}
	for _, name := range candidates {
		if c, ok := d.Get(name); ok && c.Available {
			return c
		}
	}
	return nil
}

// All returns all detected tools.
func (d *ToolDetector) All() map[string]*ToolCapability {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]*ToolCapability, len(d.cache))
	for k, v := range d.cache {
		out[k] = v
	}
	return out
}

// ScanLanguages scans all registered languages for tool availability.
func (d *ToolDetector) ScanLanguages(reg *LanguageRegistry) {
	for _, profile := range reg.profiles {
		for _, tool := range profile.Tools {
			cap := d.probe(context.Background(), tool.Name, []string{tool.Cmd, "--version"}, ``, tool.OutputFmt, tool.Priority)
			d.mu.Lock()
			d.cache[tool.Name] = cap
			d.mu.Unlock()
		}
	}
}
