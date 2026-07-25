// runtime.go —— AgentRuntime 接口与放置调度 (design/01 §4.9)。
//
// # 为什么接口定在 pkg/agent 而不是 pkg/graph
//
// 设计稿把它写在 `pkg/graph/runtime.go`。但真正要被收编的三个执行器
// (`cliAgentRunner` / `sessionAgentRunner` / `sandbox` 的 K8s Job) 全都活在
// pkg/agent 及其上层, 而 pkg/graph 是**纯调度内核、不认识 agent 语义**——把
// AgentRuntime 放进 pkg/graph 会让内核反向依赖 agent 概念 (RunMetadata、
// ToolProfile、团队 cwd), 违背"图是纯数据 + 纯调度"的分层。
//
// 折中: 接口与放置语义定在这里, 图侧继续用它已有的单方法 `NodeRunner`;
// `graph_adapter` 的 `stageNodeRunner` 是二者的桥 (它已经是 pkg/agent 的类型)。
// 这样 pkg/graph 不需要任何改动就能落在任意 runtime 上。
//
// # 与 pkg/cluster 的 caps 的关系
//
// `pkg/cluster/queue.go` 的 `RequireCaps []string` 是**分布式任务队列**的标签
// 过滤 (worker 拉取时按标签筛), 它只做"有没有"的布尔匹配, 无打分无亲和。
// 本文件的 `Placement` 是**放置策略**: 硬约束过滤 + 软偏好打分 + 亲和。
// 两者名字相近但层次不同 —— 核查时曾把 caps 误当成 Placement 的落地, 这里写清:
// caps 是 Placement 求解后、跨机下发时用于路由的投影。
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// RuntimeCaps 一个 runtime 能提供的能力。
//
// 用显式字段而非 map[string]bool: 能力集是有限且稳定的, 显式字段让
// "哪些能力存在"可被编译器与阅读者一眼看清; Extra 留给第三方 runtime。
type RuntimeCaps struct {
	Bash        bool // 可落盘执行 shell
	Browser     bool // 有无头浏览器
	K8sSandbox  bool // 可下发 K8s Job
	GPU         bool
	MaxParallel int      // 该 runtime 的并发上限; <=0 视为不限
	Extra       []string // 第三方能力标签
}

// Has 判断是否具备某个能力标签 (与 Placement.Require 的取值域一致)。
func (c RuntimeCaps) Has(cap string) bool {
	switch strings.ToLower(strings.TrimSpace(cap)) {
	case "":
		return true
	case "bash":
		return c.Bash
	case "browser":
		return c.Browser
	case "k8s-sandbox", "k8s":
		return c.K8sSandbox
	case "gpu":
		return c.GPU
	}
	for _, e := range c.Extra {
		if strings.EqualFold(e, cap) {
			return true
		}
	}
	return false
}

// NodeEventKind runtime 流式回传的事件类型。
type NodeEventKind string

const (
	NodeEventStarted NodeEventKind = "started"
	NodeEventOutput  NodeEventKind = "output" // 增量产出 (流式)
	NodeEventDone    NodeEventKind = "done"
	NodeEventFailed  NodeEventKind = "failed"
)

// NodeEvent runtime 执行过程中回传的事件。
type NodeEvent struct {
	Kind   NodeEventKind
	RunID  string
	NodeID string
	Delta  string // NodeEventOutput 的增量文本
	Output string // NodeEventDone 的完整产出
	Err    string // NodeEventFailed 的错误
}

// RuntimeNodeTask 交给 runtime 执行的自包含任务描述。
//
// **自包含**是关键 (design/02 §3.3): 远程 runtime 不该依赖本机状态目录, 所以
// prompt/角色/工作区都在这里显式带上, 而不是让对端去读 ~/.claude-go。
type RuntimeNodeTask struct {
	RunID        string
	NodeID       string
	Role         string
	SystemPrompt string
	UserPrompt   string
	Workspace    string // 团队 cwd; 空 = runtime 自行决定
	ToolProfile  string // 显式工具画像 (取代角色名子串推断)
	MaxTurns     int
	Placement    *Placement
}

// AgentRuntime 一个可执行节点任务的运行时。
type AgentRuntime interface {
	Name() string
	Capabilities() RuntimeCaps
	// Execute 返回流式事件通道。实现必须在结束时关闭通道, 否则调用方会泄漏 goroutine。
	Execute(ctx context.Context, task RuntimeNodeTask) (<-chan NodeEvent, error)
	Cancel(runID, nodeID string) error
}

// Placement 放置策略 (design/01 §4.9)。
type Placement struct {
	// Require 硬约束: runtime 必须具备全部这些能力, 否则被过滤掉。
	Require []string `json:"require,omitempty"`
	// Prefer 软偏好: "local" | "remote:<name>" | "any"。只影响打分不影响可行性。
	Prefer string `json:"prefer,omitempty"`
	// Affinity "team" 表示同团队节点尽量落同一 runtime (共享 cwd)。
	// 产码工作流必须如此: 编译门禁在 <cwd>/go.mod 上跑, 节点散落到不同工作区
	// 会让"上一阶段写的代码"在下一阶段消失。
	Affinity string `json:"affinity,omitempty"`
	// AffinityKey 亲和分组键 (通常是团队名)。Affinity 非空时必填。
	AffinityKey string `json:"affinity_key,omitempty"`
}

// ErrNoRuntime 没有满足硬约束的 runtime。
var ErrNoRuntime = errors.New("agent: 没有满足放置约束的 runtime")

// RuntimeRegistry runtime 注册与放置求解。
type RuntimeRegistry interface {
	Register(rt AgentRuntime, lease time.Duration)
	Unregister(name string)
	Heartbeat(name string)
	// Pick 按放置策略选一个 runtime。约束不满足时返回 ErrNoRuntime。
	Pick(p *Placement) (AgentRuntime, error)
	List() []string
}

type runtimeEntry struct {
	rt       AgentRuntime
	lease    time.Duration
	lastBeat time.Time
}

// memRuntimeRegistry 进程内注册表 (design/01 §4.9 的 v1 实现)。
//
// 租约的意义: 远程 runtime 掉线后不会主动注销, 若不按心跳过期剔除, Pick 会一直
// 把任务派给一个已经死掉的 worker。本地 runtime 传 lease<=0 表示永不过期。
type memRuntimeRegistry struct {
	mu sync.RWMutex
	m  map[string]*runtimeEntry
	// affinity 记住每个亲和键上次选中的 runtime 名, 实现 Affinity:"team"。
	affinity map[string]string
}

// NewRuntimeRegistry 构造进程内注册表。
func NewRuntimeRegistry() RuntimeRegistry {
	return &memRuntimeRegistry{m: map[string]*runtimeEntry{}, affinity: map[string]string{}}
}

func (r *memRuntimeRegistry) Register(rt AgentRuntime, lease time.Duration) {
	if rt == nil || rt.Name() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[rt.Name()] = &runtimeEntry{rt: rt, lease: lease, lastBeat: time.Now()}
}

func (r *memRuntimeRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, name)
	// 亲和记录一并清掉, 否则会把任务钉在一个已注销的 runtime 上。
	for k, v := range r.affinity {
		if v == name {
			delete(r.affinity, k)
		}
	}
}

func (r *memRuntimeRegistry) Heartbeat(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.m[name]; ok {
		e.lastBeat = time.Now()
	}
}

func (r *memRuntimeRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for n, e := range r.m {
		if e.alive() {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (e *runtimeEntry) alive() bool {
	if e.lease <= 0 {
		return true // 本地 runtime: 永不过期
	}
	return time.Since(e.lastBeat) <= e.lease
}

// Pick 三步: ①租约过滤 ②硬约束过滤 ③打分取最高。
//
// 打分而非"取第一个": Prefer 与 Affinity 是软偏好, 必须能在多个可行 runtime 之间
// 表达倾向。同分时按名字升序取, 保证**确定性**——同一放置请求两次求解结果必须一致,
// 否则同团队的节点会在多个 runtime 之间抖动, Affinity 形同虚设。
func (r *memRuntimeRegistry) Pick(p *Placement) (AgentRuntime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	type cand struct {
		name  string
		rt    AgentRuntime
		score int
	}
	var cands []cand
	for name, e := range r.m {
		if !e.alive() {
			continue
		}
		caps := e.rt.Capabilities()
		if p != nil {
			ok := true
			for _, req := range p.Require {
				if !caps.Has(req) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		cands = append(cands, cand{name: name, rt: e.rt, score: 0})
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w (require=%v)", ErrNoRuntime, placementRequire(p))
	}

	if p != nil {
		prevAff := ""
		if p.Affinity != "" && p.AffinityKey != "" {
			prevAff = r.affinity[p.AffinityKey]
		}
		for i := range cands {
			// 亲和命中给最高权重: 同团队落同一工作区比任何偏好都重要
			// (产码工作流的编译门禁依赖它)。
			if prevAff != "" && cands[i].name == prevAff {
				cands[i].score += 100
			}
			switch {
			case p.Prefer == "" || p.Prefer == "any":
			case p.Prefer == "local":
				if isLocalRuntimeName(cands[i].name) {
					cands[i].score += 10
				}
			case strings.HasPrefix(p.Prefer, "remote:"):
				if cands[i].name == strings.TrimPrefix(p.Prefer, "remote:") {
					cands[i].score += 10
				}
			}
		}
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].name < cands[j].name
	})
	win := cands[0]
	if p != nil && p.Affinity != "" && p.AffinityKey != "" {
		r.affinity[p.AffinityKey] = win.name
	}
	return win.rt, nil
}

func placementRequire(p *Placement) []string {
	if p == nil {
		return nil
	}
	return p.Require
}

// isLocalRuntimeName 判断是否为本地 runtime。约定: 内置本地实现以 "local-" 打头。
func isLocalRuntimeName(n string) bool { return strings.HasPrefix(n, "local-") }

// ─────────────────────────────────────────────────────────────────────────────
// 本地 runtime 实现: 把既有的 AgentRunner 收编为 AgentRuntime
// ─────────────────────────────────────────────────────────────────────────────

// localRuntime 用一个 CreateAgentFunc 适配成 AgentRuntime。
//
// 这是收编 `cliAgentRunner`(local-cli) 与 `sessionAgentRunner`(local-session) 的
// 通路: 两者本来就都由 `CreateAgentFunc` 造出来, 所以适配层只需把
// "造 runner → Execute" 包成流式事件即可, **不需要改动那两个实现**。
type localRuntime struct {
	name    string
	caps    RuntimeCaps
	factory CreateAgentFunc

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // "runID/nodeID" → cancel
}

// NewLocalRuntime 用 CreateAgentFunc 构造一个本地 runtime。
// name 建议以 "local-" 打头以便 Placement 的 Prefer:"local" 生效。
func NewLocalRuntime(name string, caps RuntimeCaps, factory CreateAgentFunc) AgentRuntime {
	if factory == nil {
		return nil
	}
	return &localRuntime{name: name, caps: caps, factory: factory, cancels: map[string]context.CancelFunc{}}
}

func (l *localRuntime) Name() string              { return l.name }
func (l *localRuntime) Capabilities() RuntimeCaps { return l.caps }
func cancelKey(runID, nodeID string) string       { return runID + "/" + nodeID }

func (l *localRuntime) Execute(ctx context.Context, task RuntimeNodeTask) (<-chan NodeEvent, error) {
	runner, err := l.factory(ctx, task.Role, task.SystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("%s: 造 runner 失败: %w", l.name, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	key := cancelKey(task.RunID, task.NodeID)
	l.mu.Lock()
	l.cancels[key] = cancel
	l.mu.Unlock()

	ch := make(chan NodeEvent, 4)
	go func() {
		// 通道必须关闭, 否则调用方 range 不结束 —— 泄漏 goroutine。
		defer close(ch)
		defer func() {
			l.mu.Lock()
			delete(l.cancels, key)
			l.mu.Unlock()
			cancel()
		}()
		// panic 兜住: 单个节点的 runner panic 不该带走整个进程。
		defer func() {
			if r := recover(); r != nil {
				ch <- NodeEvent{Kind: NodeEventFailed, RunID: task.RunID, NodeID: task.NodeID,
					Err: fmt.Sprintf("runner panic: %v", r)}
			}
		}()

		ch <- NodeEvent{Kind: NodeEventStarted, RunID: task.RunID, NodeID: task.NodeID}
		out, err := runner.Execute(cctx, task.UserPrompt)
		if err != nil {
			ch <- NodeEvent{Kind: NodeEventFailed, RunID: task.RunID, NodeID: task.NodeID, Err: err.Error()}
			return
		}
		ch <- NodeEvent{Kind: NodeEventDone, RunID: task.RunID, NodeID: task.NodeID, Output: out}
	}()
	return ch, nil
}

func (l *localRuntime) Cancel(runID, nodeID string) error {
	l.mu.Lock()
	cancel, ok := l.cancels[cancelKey(runID, nodeID)]
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: 无进行中的节点 %s/%s", l.name, runID, nodeID)
	}
	cancel()
	return nil
}

// CollectRuntimeOutput 消费事件流并归约为 (输出, 错误)。
//
// 提供这个助手的原因: 绝大多数调用方只关心终态, 但**必须把通道读到关闭**,
// 否则生产侧 goroutine 会永久阻塞在发送上。把这件容易忘的事收进一个函数。
func CollectRuntimeOutput(ch <-chan NodeEvent) (string, error) {
	var out string
	var errMsg string
	for ev := range ch {
		switch ev.Kind {
		case NodeEventOutput:
			out += ev.Delta
		case NodeEventDone:
			if ev.Output != "" {
				out = ev.Output
			}
		case NodeEventFailed:
			errMsg = ev.Err
		}
	}
	if errMsg != "" {
		return out, errors.New(errMsg)
	}
	return out, nil
}
