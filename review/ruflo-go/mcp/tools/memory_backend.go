package tools

import (
	"os"
	"sync"

	"github.com/ruflo/ruflo-go/internal/config"
	"github.com/ruflo/ruflo-go/pkg/memory"
)

var (
	unifiedMu   sync.RWMutex
	unifiedMem  *memory.UnifiedMemoryService
	memDBPath   string
	memInitOnce sync.Once
)

// SetUnifiedMemory wires the SQLite+HNSW backend for MCP memory tools.
func SetUnifiedMemory(s *memory.UnifiedMemoryService, dbPath string) {
	unifiedMu.Lock()
	defer unifiedMu.Unlock()
	unifiedMem = s
	memDBPath = dbPath
}

func getUnifiedMemory() *memory.UnifiedMemoryService {
	unifiedMu.RLock()
	defer unifiedMu.RUnlock()
	return unifiedMem
}

// InitDefaultMemory opens the configured DB once (for tests or late binding).
func InitDefaultMemory() error {
	var err error
	memInitOnce.Do(func() {
		if getUnifiedMemory() != nil {
			return
		}
		var cfg config.RufloConfig
		cfg, err = config.LoadDefault()
		if err != nil {
			return
		}
		var path string
		path, err = cfg.ResolveMemoryPath()
		if err != nil {
			return
		}
		var svc *memory.UnifiedMemoryService
		svc, err = memory.NewUnifiedMemoryService(path, cfg.EmbeddingDim, cfg.HNSWEF)
		if err != nil {
			return
		}
		if err = svc.Initialize(); err != nil {
			return
		}
		SetUnifiedMemory(svc, path)
	})
	return err
}

// ReopenMemory closes the current DB, optionally deletes the file, and opens a fresh service.
func ReopenMemory(force bool) error {
	unifiedMu.Lock()
	defer unifiedMu.Unlock()
	if unifiedMem != nil {
		_ = unifiedMem.Close()
		unifiedMem = nil
	}
	if force && memDBPath != "" {
		_ = os.Remove(memDBPath)
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return err
	}
	path, err := cfg.ResolveMemoryPath()
	if err != nil {
		return err
	}
	svc, err := memory.NewUnifiedMemoryService(path, cfg.EmbeddingDim, cfg.HNSWEF)
	if err != nil {
		return err
	}
	if err := svc.Initialize(); err != nil {
		return err
	}
	unifiedMem = svc
	memDBPath = path
	return nil
}
