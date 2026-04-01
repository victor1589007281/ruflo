// memory_backend.go：为 memory_* MCP 工具提供可选的统一内存后端（SQLite + HNSW），
// 与进程内 map 回退并存；支持测试注入、默认配置懒加载与强制重开库文件。

package tools

import (
	"os"
	"sync"

	"github.com/ruflo/ruflo-go/internal/config"
	"github.com/ruflo/ruflo-go/pkg/memory"
)

var (
	unifiedMu   sync.RWMutex
	unifiedMem  *memory.UnifiedMemoryService // 非 nil 时 memory 工具优先走统一后端
	memDBPath   string                       // 当前打开的数据库路径，供 ReopenMemory 删除
	memInitOnce sync.Once                    // 保证 InitDefaultMemory 只执行一次
)

// SetUnifiedMemory 由测试或宿主预先注入已构造的 UnifiedMemoryService 及路径。
func SetUnifiedMemory(s *memory.UnifiedMemoryService, dbPath string) {
	unifiedMu.Lock()
	defer unifiedMu.Unlock()
	unifiedMem = s
	memDBPath = dbPath
}

// getUnifiedMemory 读锁下返回当前统一内存服务指针（可能为 nil）。
func getUnifiedMemory() *memory.UnifiedMemoryService {
	unifiedMu.RLock()
	defer unifiedMu.RUnlock()
	return unifiedMem
}

// InitDefaultMemory 若尚未注入 unifiedMem，则按默认配置加载 RufloConfig、解析内存路径并打开 UnifiedMemoryService。
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

// ReopenMemory 关闭当前库；force 为 true 且已知 memDBPath 时删除库文件；再按配置重新打开服务。
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
