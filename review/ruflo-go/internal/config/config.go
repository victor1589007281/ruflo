// Package config loads Ruflo orchestration settings from defaults, environment, and JSON files.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RufloConfig holds runtime orchestration and storage settings.
type RufloConfig struct {
	Topology      string `json:"topology"`
	MaxAgents     int    `json:"max_agents"`
	Strategy      string `json:"strategy"`
	Consensus     string `json:"consensus"`
	MemoryBackend string `json:"memory_backend"`
	MemoryPath    string `json:"memory_path"`
	LogLevel      string `json:"log_level"`
	MCPPort       int    `json:"mcp_port"`
	MCPHost       string `json:"mcp_host"`
	MCPEnabled    bool   `json:"mcp_enabled"`
	NeuralEnabled bool   `json:"neural_enabled"`
	HNSWEF        int    `json:"hnsw_ef"`
	HNSWM         int    `json:"hnsw_m"`
	EmbeddingDim  int    `json:"embedding_dim"`
	APIKeys       struct {
		Anthropic string `json:"anthropic"`
		OpenAI    string `json:"openai"`
		Google    string `json:"google,omitempty"`
	} `json:"api_keys"`
}

// Default returns baseline configuration.
func Default() RufloConfig {
	var c RufloConfig
	c.Topology = "hierarchical"
	c.MaxAgents = 8
	c.Strategy = "specialized"
	c.Consensus = "raft"
	c.MemoryBackend = "hybrid"
	c.MemoryPath = ""
	c.LogLevel = "info"
	c.MCPPort = 3000
	c.MCPHost = "localhost"
	c.MCPEnabled = true
	c.NeuralEnabled = true
	c.HNSWEF = 64
	c.HNSWM = 16
	c.EmbeddingDim = 384
	return c
}

// DataDir returns the Claude Flow data directory (typically ~/.claude-flow).
func DataDir() (string, error) {
	if v := os.Getenv("CLAUDE_FLOW_MEMORY_PATH"); v != "" {
		base := filepath.Clean(v)
		if strings.HasSuffix(base, "memory") {
			return filepath.Dir(base), nil
		}
		return base, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude-flow"), nil
}

// ConfigFilePath resolves the primary JSON config file path.
func ConfigFilePath() (string, error) {
	if p := os.Getenv("RUFLO_CONFIG"); p != "" {
		return p, nil
	}
	if p := os.Getenv("CLAUDE_FLOW_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads JSON from path; missing file is not an error when allowMissing.
func Load(path string, allowMissing bool) (RufloConfig, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			applyEnv(&c)
			return c, nil
		}
		return c, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config: parse %s: %w", path, err)
	}
	applyEnv(&c)
	return c, nil
}

// LoadDefault loads from ConfigFilePath with allowMissing true.
func LoadDefault() (RufloConfig, error) {
	p, err := ConfigFilePath()
	if err != nil {
		c := Default()
		applyEnv(&c)
		return c, nil
	}
	return Load(p, true)
}

func applyEnv(c *RufloConfig) {
	if v := os.Getenv("CLAUDE_FLOW_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("CLAUDE_FLOW_MEMORY_BACKEND"); v != "" {
		c.MemoryBackend = v
	}
	if v := os.Getenv("CLAUDE_FLOW_MEMORY_PATH"); v != "" {
		c.MemoryPath = v
	}
	if v := os.Getenv("CLAUDE_FLOW_MCP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MCPPort = n
		}
	}
	if v := os.Getenv("CLAUDE_FLOW_MCP_HOST"); v != "" {
		c.MCPHost = v
	}
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		c.APIKeys.Anthropic = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		c.APIKeys.OpenAI = v
	}
	if v := os.Getenv("GOOGLE_API_KEY"); v != "" {
		c.APIKeys.Google = v
	}
}

// Save writes configuration as JSON to path (parent dirs must exist).
func Save(path string, c RufloConfig) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// ResolveMemoryPath returns the SQLite path for unified memory.
func (c RufloConfig) ResolveMemoryPath() (string, error) {
	if c.MemoryPath != "" {
		if strings.HasSuffix(c.MemoryPath, ".db") || strings.HasSuffix(c.MemoryPath, ".sqlite") {
			return c.MemoryPath, nil
		}
		return filepath.Join(c.MemoryPath, "memory.db"), nil
	}
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(memDir, "unified.db"), nil
}
