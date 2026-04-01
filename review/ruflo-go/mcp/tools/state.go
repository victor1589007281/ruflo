// Package tools 实现 ruflo MCP 侧的大量工具处理器：共享内存状态、磁盘持久化、
// 以及向 mcp.ToolRegistry 的批量注册。包级说明仅此一处，避免与其它 tools/*.go 重复。
//
// 全局状态与序列号见 state.go / persist.go；具体 RPC 工具按领域拆分到 agent、swarm、memory 等文件。
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

// agentSeq 为 agent_spawn 分配 agent-{N} 时使用的原子递增序列，进程重启后由 persist 恢复上限。
var agentSeq int64

// sharedState 是 MCP 各工具共享的进程内数据中心：Agent/Swarm/Task/Memory/Session/Neural 等
// 均在此 map 中维护，并与 hooks、SONA、治理平面等子系统引用一并持有。
type sharedState struct {
	mu sync.RWMutex

	agents   map[string]*api.Agent                  // 代理 ID -> 代理实体
	swarms   map[string]*swarmRecord                // 蜂群 ID -> 蜂群记录
	tasks    map[string]*api.TaskDefinition         // 任务 ID -> 任务定义
	memory   map[string]map[string]*api.MemoryEntry // 命名空间 -> (key -> 内存条目)，无统一后端时的回退存储
	sessions map[string]*sessionRecord              // 会话 ID -> 会话快照
	neural   *neuralState                           // 轻量「神经」模式列表与最近训练时间
	hooksLog []hookInvocation                       // 最近钩子调用日志（有界截断）
	worker   workerState                            // Worker 任务与派发计数占位

	memoryInitialized bool                           // 是否已执行过 memory_init 等初始化路径
	hookReg           *hooks.HookRegistry            // 钩子注册表
	hookExec          *hooks.HookExecutor            // 钩子执行器
	reasoningBank     *hooks.ReasoningBank           // 推理库（路由、模式存储）
	workerMgr         *hooks.WorkerManager           // 后台 Worker 管理
	llmHooks          *hooks.LLMHookBundle           // LLM 前后置钩子与指标
	sona              *nlp.SONACoordinator           // SONA 轨迹与信号协调器
	guidancePlane     *guidance.GuidanceControlPlane // 治理控制面（供扩展工具使用）
}

// swarmRecord 表示一次 swarm_init 创建的蜂群元数据与生命周期状态。
type swarmRecord struct {
	ID        string            `json:"id"`         // 蜂群唯一 ID（如 swarm-N）
	Topology  string            `json:"topology"`   // 拓扑：hierarchical、mesh 等
	MaxAgents int               `json:"max_agents"` // 容量上限
	Strategy  string            `json:"strategy"`   // 策略：如 specialized
	V3Mode    bool              `json:"v3_mode"`    // 是否 V3 全量模式标记
	Status    api.SwarmStatus   `json:"status"`     // 运行/已停止等
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Extra     map[string]string `json:"extra,omitempty"` // 扩展键值
}

// sessionRecord 表示可持久化的会话快照（任意 JSON 负载）。
type sessionRecord struct {
	ID        string         `json:"id"`
	Data      map[string]any `json:"data"` // 会话自定义数据
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// neuralState 保存 MCP neural_* 工具使用的模式列表与上次训练时间戳。
type neuralState struct {
	Patterns  []api.Pattern `json:"patterns"`
	LastTrain time.Time     `json:"last_train,omitempty"`
}

// hookInvocation 记录一次钩子调用的名称、入参摘要、结果与时间。
type hookInvocation struct {
	Name      string           `json:"name"`
	Args      map[string]any   `json:"args,omitempty"`
	Result    hooks.HookResult `json:"result"`
	Timestamp time.Time        `json:"timestamp"`
}

// workerState 为 Worker 相关 MCP 工具的轻量占位状态（任务 ID 列表、按触发器计数）。
type workerState struct {
	Jobs     []string       `json:"jobs"`
	Dispatch map[string]int `json:"dispatch_counts"`
}

// globalState 为全包共享的单例；init 中会装配 hookExec、llmHooks、sona 等。
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
