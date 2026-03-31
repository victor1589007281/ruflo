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
	dataDir   = defaultDataDir
)

// SetDataDir overrides the root for file-backed tool state (e.g. tests).
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

func swarmStatePath() string {
	return filepath.Join(resolveDataDir(), "swarm", "swarm-state.json")
}

func tasksStorePath() string {
	return filepath.Join(resolveDataDir(), "tasks", "store.json")
}

type agentsFile struct {
	Agents map[string]*api.Agent `json:"agents"`
}

type swarmFile struct {
	Swarms map[string]*swarmRecord `json:"swarms"`
}

type tasksFile struct {
	Tasks map[string]*api.TaskDefinition `json:"tasks"`
}

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

// restoreAgentSeq sets agentSeq from the highest numeric suffix among loaded "agent-N" IDs
// so new spawns do not collide after process restart.
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

type sessionsFile struct {
	Sessions map[string]*sessionRecord `json:"sessions"`
}

func sessionsStorePath() string {
	return filepath.Join(resolveDataDir(), "sessions", "store.json")
}

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

func neuralStorePath() string {
	return filepath.Join(resolveDataDir(), "neural", "state.json")
}

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

func saveNeuralToDisk() {
	globalState.mu.RLock()
	cp := neuralState{
		Patterns:  append([]api.Pattern(nil), globalState.neural.Patterns...),
		LastTrain: globalState.neural.LastTrain,
	}
	globalState.mu.RUnlock()
	_ = writeJSONFile(neuralStorePath(), cp)
}

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
