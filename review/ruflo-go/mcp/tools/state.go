package tools

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/pkg/guidance"
	"github.com/ruflo/ruflo-go/pkg/hooks"
	nlp "github.com/ruflo/ruflo-go/pkg/neural"
)

var agentSeq int64

// Shared in-process state for MCP tool handlers.
type sharedState struct {
	mu sync.RWMutex

	agents   map[string]*api.Agent
	swarms   map[string]*swarmRecord
	tasks    map[string]*api.TaskDefinition
	memory   map[string]map[string]*api.MemoryEntry // namespace -> key -> entry
	sessions map[string]*sessionRecord
	neural   *neuralState
	hooksLog []hookInvocation
	worker   workerState

	memoryInitialized bool
	hookReg           *hooks.HookRegistry
	hookExec          *hooks.HookExecutor
	reasoningBank     *hooks.ReasoningBank
	workerMgr         *hooks.WorkerManager
	llmHooks          *hooks.LLMHookBundle
	sona              *nlp.SONACoordinator
	guidancePlane     *guidance.GuidanceControlPlane
}

type swarmRecord struct {
	ID        string            `json:"id"`
	Topology  string            `json:"topology"`
	MaxAgents int               `json:"max_agents"`
	Strategy  string            `json:"strategy"`
	V3Mode    bool              `json:"v3_mode"`
	Status    api.SwarmStatus   `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Extra     map[string]string `json:"extra,omitempty"`
}

type sessionRecord struct {
	ID        string         `json:"id"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type neuralState struct {
	Patterns  []api.Pattern `json:"patterns"`
	LastTrain time.Time     `json:"last_train,omitempty"`
}

type hookInvocation struct {
	Name      string           `json:"name"`
	Args      map[string]any   `json:"args,omitempty"`
	Result    hooks.HookResult `json:"result"`
	Timestamp time.Time        `json:"timestamp"`
}

type workerState struct {
	Jobs     []string       `json:"jobs"`
	Dispatch map[string]int `json:"dispatch_counts"`
}

var globalState = &sharedState{
	agents:   make(map[string]*api.Agent),
	swarms:   make(map[string]*swarmRecord),
	tasks:    make(map[string]*api.TaskDefinition),
	memory:   make(map[string]map[string]*api.MemoryEntry),
	sessions: make(map[string]*sessionRecord),
	neural: &neuralState{
		Patterns: make([]api.Pattern, 0),
	},
	worker: workerState{
		Jobs:     make([]string, 0),
		Dispatch: make(map[string]int),
	},
	hookReg:       hooks.NewRegistry(),
	reasoningBank: hooks.NewReasoningBank(),
	workerMgr:     hooks.NewWorkerManager(),
	guidancePlane: guidance.NewGuidanceControlPlane(),
}

func init() {
	globalState.hookExec = hooks.NewExecutor(globalState.hookReg)
	globalState.llmHooks = hooks.NewLLMHookBundle(globalState.reasoningBank)
	patPath := filepath.Join(resolveDataDir(), "neural", "patterns.json")
	globalState.sona = nlp.NewSONACoordinator(nlp.DefaultSONAConfig(), patPath)
}

func now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
