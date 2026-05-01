package observability

import (
	"log"
	"os"
	"path/filepath"
)

// System 是 observability 包的全局初始化入口。
// 在一个地方统一初始化: Bus + HookRegistry + MetricsEmitter + JSONLStore + PromptObservatory。
type System struct {
	Bus         *Bus
	Registry    *HookRegistry
	Emitter     *MetricsEmitter
	JSONLStore  *JSONLStore
	Observatory *PromptObservatory
	ActiveSpans *ActiveSpanStore
	Rotator     *DailyRotator
}

// InitSystem 初始化完整可观测体系。
//
// stateDir: 用于存放 JSONL 和 SQLite 的根目录 (如 ~/.claude-go/state)
func InitSystem(stateDir string) (*System, error) {
	bus := NewBus()
	globalBus = bus // 替换全局总线

	reg := &HookRegistry{}
	globalRegistry = reg

	// MetricsEmitter 桥接到现有 metrics.Collector
	emitter := NewMetricsEmitter(nil)
	reg.RegisterLLM(emitter)
	reg.RegisterStage(emitter)
	reg.RegisterTask(emitter)
	reg.RegisterTeam(emitter)
	reg.RegisterCollaboration(emitter)
	reg.RegisterPrompt(emitter)

	// JSONL 事件存储
	jsonlDir := filepath.Join(stateDir, "observability")
	store, err := NewJSONLStore(jsonlDir)
	if err != nil {
		return nil, err
	}
	jsonlSub := NewJSONLStoreSubscriber(store)
	bus.Subscribe(jsonlSub)

	// Prompt Observatory (SQLite)
	dbPath := filepath.Join(stateDir, "prompt_observatory.db")
	observatory, err := NewPromptObservatory(dbPath)
	if err != nil {
		log.Printf("[observability] PromptObservatory 初始化失败 (sqlite3 驱动可能未安装): %v", err)
		observatory = nil
	} else {
		promptAdapter := NewPromptHookAdapter(observatory)
		reg.RegisterPrompt(promptAdapter)
	}

	// BusSubscriber 将 bus 事件分发给 hook registry
	busSub := NewBusSubscriber(reg)
	bus.Subscribe(busSub)

	// 活跃 span 存储
	spanStore := NewActiveSpanStore()

	// 每日轮转
	rotator := StartDailyRotator(store)

	sys := &System{
		Bus:         bus,
		Registry:    reg,
		Emitter:     emitter,
		JSONLStore:  store,
		Observatory: observatory,
		ActiveSpans: spanStore,
		Rotator:     rotator,
	}

	log.Printf("[observability] 系统已初始化: JSONL=%s DB=%s", jsonlDir, dbPath)
	return sys, nil
}

// Close 关闭所有资源。
func (s *System) Close() error {
	if s.Rotator != nil {
		s.Rotator.Stop()
	}
	if s.JSONLStore != nil {
		_ = s.JSONLStore.Close()
	}
	if s.Observatory != nil {
		_ = s.Observatory.Close()
	}
	return nil
}

// DefaultStateDir 返回默认状态目录。
func DefaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude-go/state"
	}
	return filepath.Join(home, ".claude-go", "state")
}

// globalSystem 进程级默认系统。
var globalSystem *System

// GlobalSystem 返回全局系统。
func GlobalSystem() *System { return globalSystem }

// SetGlobalSystem 设置全局系统。
func SetGlobalSystem(s *System) { globalSystem = s }

// InitDefaultSystem 用默认路径初始化全局系统。
func InitDefaultSystem() (*System, error) {
	sys, err := InitSystem(DefaultStateDir())
	if err != nil {
		return nil, err
	}
	globalSystem = sys
	return sys, nil
}

// EnsureInitialized 确保全局系统已初始化 (幂等)。
func EnsureInitialized() {
	if globalSystem != nil {
		return
	}
	if _, err := InitDefaultSystem(); err != nil {
		log.Printf("[observability] 默认初始化失败: %v", err)
	}
}
