// persist.go 负责将 MCP 工具依赖的内存状态序列化到 .claude-flow（或可覆盖的 dataDir）下 JSON 文件，
// 使多次 CLI/MCP 调用之间可恢复 Agent/Swarm/Task/Session/Neural 等；并在启动时根据已加载 ID
// 重建 agentSeq、swarmSeq 等原子序列，避免重启后 ID 碰撞。
package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/ruflo/ruflo-go/api"
)

const defaultDataDir = ".claude-flow"

var (
	dataDirMu sync.RWMutex
	dataDir   = defaultDataDir // 根目录，下挂 agents/、swarm/、tasks/ 等子路径
)

// SetDataDir 覆盖文件状态根路径（测试或自定义数据目录时使用）。
func SetDataDir(d string) {
	dataDirMu.Lock()
	defer dataDirMu.Unlock()
	if d == "" {
		d = defaultDataDir
	}
	dataDir = d
}

func resolveDataDir() string {
	dataDirMu.RLock()
	defer dataDirMu.RUnlock()
	return dataDir
}

func agentsStorePath() string {
	return filepath.Join(resolveDataDir(), "agents", "store.json")
}

// swarmStatePath 返回 swarm 状态 JSON 路径。
func swarmStatePath() string {
	return filepath.Join(resolveDataDir(), "swarm", "swarm-state.json")
}

// tasksStorePath 返回 tasks 持久化 JSON 路径。
func tasksStorePath() string {
	return filepath.Join(resolveDataDir(), "tasks", "store.json")
}

// agentsFile 为 agents/store.json 的顶层结构。
type agentsFile struct {
	Agents map[string]*api.Agent `json:"agents"`
}

// swarmFile 为 swarm/swarm-state.json 的顶层结构。
type swarmFile struct {
	Swarms map[string]*swarmRecord `json:"swarms"`
}

// tasksFile 为 tasks/store.json 的顶层结构。
type tasksFile struct {
	Tasks map[string]*api.TaskDefinition `json:"tasks"`
}

// loadAgentsFromDisk 从磁盘读取 agents 映射并合并进 globalState（失败则静默跳过）。
func loadAgentsFromDisk() {
	p := agentsStorePath()
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var f agentsFile
	if json.Unmarshal(b, &f) != nil || f.Agents == nil {
		return
	}
	globalState.mu.Lock()
	for id, a := range f.Agents {
		if a != nil {
			globalState.agents[id] = a
		}
	}
	globalState.mu.Unlock()
}

func saveAgentsToDisk() {
	globalState.mu.RLock()
	cp := make(map[string]*api.Agent, len(globalState.agents))
	for k, v := range globalState.agents {
		cp[k] = v
	}
	globalState.mu.RUnlock()
	f := agentsFile{Agents: cp}
	_ = writeJSONFile(agentsStorePath(), f)
}

func loadSwarmFromDisk() {
	p := swarmStatePath()
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var f swarmFile
	if json.Unmarshal(b, &f) != nil || f.Swarms == nil {
		return
	}
	globalState.mu.Lock()
	for id, s := range f.Swarms {
		if s != nil {
			globalState.swarms[id] = s
		}
	}
	globalState.mu.Unlock()
}

func saveSwarmToDisk() {
	globalState.mu.RLock()
	cp := make(map[string]*swarmRecord, len(globalState.swarms))
	for k, v := range globalState.swarms {
		cp[k] = v
	}
	globalState.mu.RUnlock()
	f := swarmFile{Swarms: cp}
	_ = writeJSONFile(swarmStatePath(), f)
}

// loadTasksFromDisk 加载任务定义到 globalState。
func loadTasksFromDisk() {
	p := tasksStorePath()
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var f tasksFile
	if json.Unmarshal(b, &f) != nil || f.Tasks == nil {
		return
	}
	globalState.mu.Lock()
	for id, t := range f.Tasks {
		if t != nil {
			globalState.tasks[id] = t
		}
	}
	globalState.mu.Unlock()
}

// saveTasksToDisk 将任务映射写入磁盘。
func saveTasksToDisk() {
	globalState.mu.RLock()
	cp := make(map[string]*api.TaskDefinition, len(globalState.tasks))
	for k, v := range globalState.tasks {
		cp[k] = v
	}
	globalState.mu.RUnlock()
	f := tasksFile{Tasks: cp}
	_ = writeJSONFile(tasksStorePath(), f)
}

// writeJSONFile 将 v 以缩进 JSON 写入 path：先写临时文件再 rename，降低半写损坏概率。
func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// restoreAgentSeq 根据已加载的 agent-{N} ID 中最大 N 重置 agentSeq，避免重启后新 ID 与旧数据冲突。
func restoreAgentSeq() {
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	var maxSeq int64
	for id := range globalState.agents {
		var n int64
		if _, err := fmt.Sscanf(id, "agent-%d", &n); err == nil && n > maxSeq {
			maxSeq = n
		}
	}
	atomic.StoreInt64(&agentSeq, maxSeq)
}

// sessionsFile 为 sessions/store.json 的顶层结构。
type sessionsFile struct {
	Sessions map[string]*sessionRecord `json:"sessions"`
}

// sessionsStorePath 返回会话存储路径。
func sessionsStorePath() string {
	return filepath.Join(resolveDataDir(), "sessions", "store.json")
}

// loadSessionsFromDisk 加载会话记录。
func loadSessionsFromDisk() {
	p := sessionsStorePath()
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var f sessionsFile
	if json.Unmarshal(b, &f) != nil || f.Sessions == nil {
		return
	}
	globalState.mu.Lock()
	for id, s := range f.Sessions {
		if s != nil {
			globalState.sessions[id] = s
		}
	}
	globalState.mu.Unlock()
}

// saveSessionsToDisk 持久化会话映射。
func saveSessionsToDisk() {
	globalState.mu.RLock()
	cp := make(map[string]*sessionRecord, len(globalState.sessions))
	for k, v := range globalState.sessions {
		cp[k] = v
	}
	globalState.mu.RUnlock()
	f := sessionsFile{Sessions: cp}
	_ = writeJSONFile(sessionsStorePath(), f)
}

// neuralStorePath 返回 neural 状态 JSON 路径。
func neuralStorePath() string {
	return filepath.Join(resolveDataDir(), "neural", "state.json")
}

// loadNeuralFromDisk 将磁盘上的模式列表与 LastTrain 合并进 globalState.neural。
func loadNeuralFromDisk() {
	p := neuralStorePath()
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var ns neuralState
	if json.Unmarshal(b, &ns) != nil {
		return
	}
	globalState.mu.Lock()
	if len(ns.Patterns) > 0 {
		globalState.neural.Patterns = ns.Patterns
	}
	if !ns.LastTrain.IsZero() {
		globalState.neural.LastTrain = ns.LastTrain
	}
	globalState.mu.Unlock()
}

// saveNeuralToDisk 将 neuralState 快照写入磁盘。
func saveNeuralToDisk() {
	globalState.mu.RLock()
	cp := neuralState{
		Patterns:  append([]api.Pattern(nil), globalState.neural.Patterns...),
		LastTrain: globalState.neural.LastTrain,
	}
	globalState.mu.RUnlock()
	_ = writeJSONFile(neuralStorePath(), cp)
}

// restorePatternSeq 根据 pat-{N} 最大 N 重置 patternSeq。
func restorePatternSeq() {
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	var maxSeq int64
	for _, p := range globalState.neural.Patterns {
		var n int64
		if _, err := fmt.Sscanf(p.ID, "pat-%d", &n); err == nil && n > maxSeq {
			maxSeq = n
		}
	}
	atomic.StoreInt64(&patternSeq, maxSeq)
}

// restoreTaskSeq 根据 task-{N} 最大 N 重置 taskSeq。
func restoreTaskSeq() {
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	var maxSeq int64
	for id := range globalState.tasks {
		var n int64
		if _, err := fmt.Sscanf(id, "task-%d", &n); err == nil && n > maxSeq {
			maxSeq = n
		}
	}
	atomic.StoreInt64(&taskSeq, maxSeq)
}

// restoreSwarmSeq 根据 swarm-{N} 最大 N 重置 swarmSeq。
func restoreSwarmSeq() {
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	var maxSeq int64
	for id := range globalState.swarms {
		var n int64
		if _, err := fmt.Sscanf(id, "swarm-%d", &n); err == nil && n > maxSeq {
			maxSeq = n
		}
	}
	atomic.StoreInt64(&swarmSeq, maxSeq)
}

// restoreSessionSeq 根据 sess-{N} 最大 N 重置 sessionSeq。
func restoreSessionSeq() {
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()
	var maxSeq int64
	for id := range globalState.sessions {
		var n int64
		if _, err := fmt.Sscanf(id, "sess-%d", &n); err == nil && n > maxSeq {
			maxSeq = n
		}
	}
	atomic.StoreInt64(&sessionSeq, maxSeq)
}

func init() {
	// 进程启动时按顺序从磁盘恢复各域状态并重建序列号。
	loadAgentsFromDisk()
	restoreAgentSeq()
	loadSwarmFromDisk()
	restoreSwarmSeq()
	loadTasksFromDisk()
	restoreTaskSeq()
	loadSessionsFromDisk()
	restoreSessionSeq()
	loadNeuralFromDisk()
	restorePatternSeq()
}
