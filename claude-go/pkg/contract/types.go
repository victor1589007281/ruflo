// Package contract defines core contract types for the Agent Harness.
// These types represent interface contracts and method signatures discovered
// from source code, used by both pkg/agent and pkg/toolskill.
package contract

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Store holds all discovered interface contracts for a project.
type Store struct {
	mu           sync.RWMutex
	Contracts    map[string]*InterfaceContract `json:"contracts"`
	Implementors map[string][]string           `json:"implementors"`
	Version      string                        `json:"version"`
}

// InterfaceContract represents a discovered Go interface.
type InterfaceContract struct {
	Package    string                 `json:"package"`
	Name       string                 `json:"name"`
	Methods    map[string]*MethodSig  `json:"methods"`
	FilePath   string                 `json:"file_path,omitempty"`
}

// MethodSig represents a method signature.
type MethodSig struct {
	Name    string   `json:"name"`
	Params  []string `json:"params,omitempty"`
	Results []string `json:"results,omitempty"`
}

// NewStore creates an empty contract store.
func NewStore() *Store {
	return &Store{
		Contracts:    make(map[string]*InterfaceContract),
		Implementors: make(map[string][]string),
	}
}

// Save persists the store to a JSON file.
func (s *Store) Save(path string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// Load reads the store from a JSON file.
func (s *Store) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Unmarshal(data, s)
}

// GetInterface retrieves an interface contract by fully qualified name.
func (s *Store) GetInterface(fqName string) (*InterfaceContract, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.Contracts[fqName]
	return c, ok
}

// GetImplementors returns all known implementors of an interface.
func (s *Store) GetImplementors(fqName string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Implementors[fqName]
}

// ContractVersion represents a snapshot of an interface contract.
type ContractVersion struct {
	Version   int       `json:"version"`
	Author    string    `json:"author"` // Agent ID
	Timestamp time.Time `json:"timestamp"`
	Diff      string    `json:"diff,omitempty"` // diff from previous version
	Breaking  bool      `json:"breaking"`       // whether this breaks compatibility
}

// InterfaceContractV2 extends InterfaceContract with versioning.
// We keep backward compat by embedding the old fields.
type InterfaceContractV2 struct {
	InterfaceContract
	Versions []ContractVersion `json:"versions"`
	Current  MethodSig         `json:"current"` // latest version
	Locked   bool              `json:"locked"`
	LockedBy string            `json:"locked_by,omitempty"`
}
