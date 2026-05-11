// Package contract defines core contract types for the Agent Harness.
// These types represent interface contracts and method signatures discovered
// from source code, used by both pkg/agent and pkg/toolskill.
package contract

import (
	"encoding/json"
	"os"
	"sync"
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
