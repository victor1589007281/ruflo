package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

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

func init() {
	loadAgentsFromDisk()
	loadSwarmFromDisk()
	loadTasksFromDisk()
}
