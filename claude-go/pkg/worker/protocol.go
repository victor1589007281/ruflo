// Package worker 远程 Agent 运行时 (design/02 §3.3 L3 + design/01 §4.9)。
//
// # 这个包补的是哪一段断链
//
// 改造前的事实: `pkg/cluster` 有完整的租约式任务队列与 worker 拉取循环
// (`cmd/claude-go/main.go` 的 `workerCmd`), 但执行体是桩 ——
// `executeWorkerTask` 只把 payload 回显成 JSON (自带注释「v1 简化实现…R3 后续接
// 完整引擎」)。同时 `pkg/agent.RuntimeRegistry` 里只有 `localRuntime` 一种实现,
// 控制面**没有任何办法把一个图节点派到别的进程上**。
//
// 本包提供三件东西, 把两头接起来:
//
//	① Broker      控制面侧: 把 cluster.Queue 包装成 agent.AgentRuntime 的远程实现,
//	              并把 cluster.Registry 里存活的 worker 同步进 agent.RuntimeRegistry
//	              (靠 Heartbeat 续租, 掉线由既有租约机制剔除)。
//	② Worker      worker 侧: 注册/心跳/拉取/**用既有本地执行路径真跑**/续租/
//	              流式回传 NodeEvent/回报终态。
//	③ RuntimeFactory  控制面侧的接线点: 把 RuntimeRegistry 包成既有的
//	              `agent.CreateAgentFunc` 契约, 于是 `ExecuteSingleStage` →
//	              `we.factory(...)` 这条**既有**调用链自动获得放置能力,
//	              journal/hook/重试/门禁全部不动 (见 factory.go 的注释)。
//
// # 为什么不新造一套传输
//
// 仓内已有 HTTP 传输 (`pkg/cluster/http.go`: /cluster/pull|complete|fail|extend|
// heartbeat) 与 StateStore 持久化的队列。本包只补了一个**事件回传端点**
// (`/cluster/node-events`), 因为原有协议里 worker 只能回报终态, 没有流式通道。
//
// # 真源与 fail-closed
//
// 事件流是**观测流**, 队列状态才是**真源**: 控制面只把 worker 上报的
// started/output 事件转发给调用方, 终态 (done/failed) 一律由队列里的任务状态
// 决定。理由: 事件走的是 best-effort HTTP, 丢一条 delta 无所谓, 但"worker 说自己
// 成功了"若不落队列就等于没成功——把终态交给事件流会让丢包变成静默成功。
package worker

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
)

// TaskKindStage stage 任务的 Kind (与 cmd/claude-go 既有 worker 拉取的 kinds 一致)。
const TaskKindStage = "stage"

// DefaultEventsPath worker → 控制面 的事件回传端点。
//
// 挂在 /cluster/ 前缀下与既有端点同族 (同一 mux、同一 Bearer 中间件覆盖),
// 但**不在 pkg/cluster 内实现** —— 事件订阅是控制面 runtime 的私有状态,
// 放进 cluster.Mount 会让队列包反向依赖 agent 语义。
const DefaultEventsPath = "/cluster/node-events"

// CapWorkerPrefix 把任务钉到指定 worker 的合成能力标签前缀。
//
// 为什么需要它: 队列只有 RequireCaps 布尔过滤, 没有"投递给谁"的概念。而
// design/01 §4.9 的 `Placement.Affinity: team` 要求同团队节点落**同一** worker
// (共享 cwd, 否则上一阶段写的代码在下一阶段消失)。做法: 每个 worker 上报
// `worker:<自己的名字>` 这一条标签, 控制面派任务时把它写进 RequireCaps ——
// 于是"钉住"用既有的过滤器就实现了, pkg/cluster 零改动。
const CapWorkerPrefix = "worker:"

// WorkerCap 返回钉住某个 worker 的能力标签。
func WorkerCap(name string) string { return CapWorkerPrefix + name }

// StageTask 一个 stage 任务的自包含载荷 (cluster.Task.Payload 的 JSON 编码)。
//
// **自包含**是 design/02 §3.3 的关键约束: 远程 worker 不该靠读本机状态目录来
// 猜这个节点要干什么。所以 role/prompt/工具画像/轮数上限全部随任务下发。
//
// ⚠️ 诚实边界: 系统提示词目前仍由 worker 侧按 role 组装 —— 既有
// `CreateAgentFunc` 契约就是 `(ctx, role, systemPrompt)` 且生产调用方传的
// systemPrompt 是空串 (`pkg/agent/workflow.go:1580`), 提示词在 runner 内部由
// role skills + 环境段拼出。设计稿承诺的"skill 正文随任务下发/内容寻址"属
// SkillStore 中心化 (design/02 R2), 未实现。这里如实透传拿到的 systemPrompt。
type StageTask struct {
	RunID        string   `json:"run_id,omitempty"`
	NodeID       string   `json:"node_id,omitempty"`
	Role         string   `json:"role"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	UserPrompt   string   `json:"user_prompt"`
	Workspace    string   `json:"workspace,omitempty"`    // 需要的团队 cwd; 空=worker 自行决定
	ToolProfile  string   `json:"tool_profile,omitempty"` // 显式工具画像 (取代角色名子串推断)
	MaxTurns     int      `json:"max_turns,omitempty"`
	NodeKind     string   `json:"node_kind,omitempty"` // 节点形态 (agent/gate/...), 供 worker 侧 hints
	Iteration    int      `json:"iteration,omitempty"` // loop 轮次 (0 起)
	Require      []string `json:"require,omitempty"`   // 硬约束回显, 供 worker 自检

	// ── cwd 档位 (design/02 §3.3, 见 workspace.go) ────────────────────────────
	// 全空 = 控制面未配置 WorkspacePolicy ⇒ worker 走原来那条判定, 行为不变。
	//
	// Workspace 的含义随档位变:
	//   local/pvc → **要求**: worker 声明的工作区必须是同一路径 (否则拒绝执行)。
	//   git       → 控制面侧的 team.Cwd (门禁跑的地方); worker 用自己的检出目录,
	//               身份由 (remote, branch) 而不是路径决定。
	WorkspaceMode WorkspaceMode `json:"workspace_mode,omitempty"`
	// WorkspaceVolume pvc 档: 共享卷身份 (worker 必须挂同名卷)。
	WorkspaceVolume string `json:"workspace_volume,omitempty"`
	// WorkspaceHandshake pvc 档: 握手令牌 (= 任务 ID)。控制面派任务前已在共享卷上
	// 写好 <workspace>/.claude-go-ws/<token>.control, worker 读不到就拒绝执行 ——
	// 这是"两边真的是同一份数据"的唯一证据 (光比路径字符串证明不了)。
	WorkspaceHandshake string `json:"workspace_handshake,omitempty"`
	// Git git 档: 约定 git 位置与分支。
	Git *GitWorkspace `json:"git,omitempty"`
}

// EncodeStageTask 序列化为任务载荷。
func EncodeStageTask(st StageTask) (json.RawMessage, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("worker: 编码 stage 载荷失败: %w", err)
	}
	return json.RawMessage(b), nil
}

// DecodeStageTask 解析任务载荷。载荷坏了必须报错而不是当成空任务执行 ——
// 空 prompt 跑出来的产出是"成功的垃圾", 比失败更难查。
func DecodeStageTask(raw json.RawMessage) (StageTask, error) {
	var st StageTask
	if len(raw) == 0 {
		return st, fmt.Errorf("worker: stage 载荷为空")
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("worker: stage 载荷解码失败: %w", err)
	}
	if strings.TrimSpace(st.UserPrompt) == "" && strings.TrimSpace(st.Role) == "" {
		return st, fmt.Errorf("worker: stage 载荷缺少 role 与 user_prompt")
	}
	return st, nil
}

// ToRuntimeTask 转成既有的 agent.RuntimeNodeTask (交给本地 runtime 真跑)。
func (st StageTask) ToRuntimeTask() agent.RuntimeNodeTask {
	return agent.RuntimeNodeTask{
		RunID:        st.RunID,
		NodeID:       st.NodeID,
		Role:         st.Role,
		SystemPrompt: st.SystemPrompt,
		UserPrompt:   st.UserPrompt,
		Workspace:    st.Workspace,
		ToolProfile:  st.ToolProfile,
		MaxTurns:     st.MaxTurns,
	}
}

// Hints 把任务里的节点声明还原成 ctx 里的 NodeExecHints。
//
// 生产 factory (`feishu.SessionManager.CreateAgentRunner`) 是在**创建时**从 ctx
// 读 hints 的 (session.go:1035 的注释: "必须在创建时捕获"), 所以 worker 侧必须
// 在调用 runtime.Execute 之前把 hints 塞回 ctx —— 否则 tool_profile / max_turns
// 跨进程之后静默丢失, 表现为"远程执行的节点工具画像不对"。
func (st StageTask) Hints() agent.NodeExecHints {
	return agent.NodeExecHints{
		Node:        st.NodeID,
		Role:        st.Role,
		Kind:        st.NodeKind,
		ToolProfile: st.ToolProfile,
		MaxTurns:    st.MaxTurns,
		Iteration:   st.Iteration,
	}
}

// ResultProto StageResult 的协议标记 (必须精确匹配)。
//
// 为什么要一个标记: 改造前的桩 worker (`cmd/claude-go/main.go` 的
// executeWorkerTask) 回报的是 `{"worker":...,"echo":...,"status":"completed"}`,
// 这个形状能被任何宽松的解码器"成功"解析成空产出 —— 于是一个什么都没干的 worker
// 会让远程节点静默成功。带版本的协议标记把这类不匹配变成显式失败。
const ResultProto = "claude-go/worker/v1"

// StageResult worker 回报的结果 (cluster.Task.Result 的 JSON 编码)。
type StageResult struct {
	Proto     string `json:"proto"`
	Output    string `json:"output"`
	Worker    string `json:"worker,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
	// Workspace 工作区处理的据实回报 (git 档: 推没推、推了哪个提交)。
	// 控制面据此决定要不要同步/校验, 所以它不是纯观测字段。
	Workspace *WorkspaceReport `json:"workspace,omitempty"`
}

// NewStageResult 构造带协议标记的结果。
func NewStageResult(worker, output string, elapsedMS int64) StageResult {
	return StageResult{Proto: ResultProto, Output: output, Worker: worker, ElapsedMS: elapsedMS}
}

// EventBatch worker → 控制面 的事件上报 (一次 HTTP 一批, 也用作 keepalive)。
//
// Events 为空的批 = 纯 keepalive: worker 借它取回控制面的取消指令
// (见 EventAck.Cancel)。
type EventBatch struct {
	Worker string            `json:"worker"`
	TaskID string            `json:"task_id"`
	Events []agent.NodeEvent `json:"events,omitempty"`
	Extra  map[string]string `json:"extra,omitempty"` // 预留 (trace 透传等), 当前不解释
}

// EventAck 控制面对事件上报的应答。
type EventAck struct {
	OK bool `json:"ok"`
	// Cancel true = 控制面要求 worker 取消该任务 (AgentRuntime.Cancel 的跨进程投递)。
	// 走应答而不是反向连接: worker 常在 NAT/Pod 内, 控制面连不上它。
	Cancel bool `json:"cancel,omitempty"`
	// Unknown true = 控制面没有该任务的订阅者 (控制面重启/调用方已放弃)。
	// **不等于要取消**: 订阅关系是观测通道, 断了不代表任务白跑
	// (worker 的 Complete 仍然会落队列, 之后 resume 能捡到)。
	Unknown bool `json:"unknown,omitempty"`
	// Dropped 控制面因订阅缓冲满而丢弃的事件数 (可观测, 不影响正确性)。
	Dropped int `json:"dropped,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// 能力标签 ⇄ RuntimeCaps 的双向投影
//
// runtime.go 的文件头写清了两者的层次关系: cluster 的 caps 是 Placement 求解后
// **用于跨机路由的投影**。这里就是那个投影函数, 双向都要有:
//   - worker 侧: 自己的 RuntimeCaps → 上报给控制面的标签
//   - 控制面侧: worker 上报的标签 → 远程 runtime 的 RuntimeCaps (供 Pick 打分)
// 两个方向必须用同一张表, 否则"worker 说自己有 browser"和"Pick 认为它有
// browser"会对不上, 表现为需要浏览器的节点被派到没浏览器的 worker。
// ─────────────────────────────────────────────────────────────────────────────

// 标签词表 (取值域与 agent.RuntimeCaps.Has 完全一致)。
const (
	CapBash       = "bash"
	CapBrowser    = "browser"
	CapK8sSandbox = "k8s-sandbox"
	CapGPU        = "gpu"
)

// CapsFromRuntime 把 RuntimeCaps 投影成能力标签 (升序去重)。
func CapsFromRuntime(c agent.RuntimeCaps) []string {
	set := map[string]bool{}
	if c.Bash {
		set[CapBash] = true
	}
	if c.Browser {
		set[CapBrowser] = true
	}
	if c.K8sSandbox {
		set[CapK8sSandbox] = true
	}
	if c.GPU {
		set[CapGPU] = true
	}
	for _, e := range c.Extra {
		if e = strings.TrimSpace(e); e != "" {
			set[strings.ToLower(e)] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RuntimeCapsFromLabels 把能力标签还原成 RuntimeCaps。
// 未知标签进 Extra —— design/02 §3.3 要求 stdio MCP 这类 worker 本地资源以
// `mcp:playwright` 形式做标签路由, 词表不该写死。
func RuntimeCapsFromLabels(labels []string) agent.RuntimeCaps {
	var c agent.RuntimeCaps
	seen := map[string]bool{}
	for _, raw := range labels {
		l := strings.ToLower(strings.TrimSpace(raw))
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		switch l {
		case CapBash:
			c.Bash = true
		case CapBrowser:
			c.Browser = true
		case CapK8sSandbox, "k8s":
			c.K8sSandbox = true
		case CapGPU:
			c.GPU = true
		default:
			c.Extra = append(c.Extra, l)
		}
	}
	sort.Strings(c.Extra)
	return c
}

// MergeCaps 合并去重 (升序)。
func MergeCaps(sets ...[]string) []string {
	set := map[string]bool{}
	for _, s := range sets {
		for _, v := range s {
			if v = strings.TrimSpace(v); v != "" {
				set[v] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MissingCaps 返回 require 中 have 不具备的标签 (worker 侧自检用)。
//
// 队列已经按 RequireCaps ⊆ workerCaps 过滤过一遍, 这里是纵深防御: 控制面若因
// 注册表不同步派来一个本 worker 干不了的任务, 必须诚实失败而不是硬跑
// (硬跑的产物是"没有浏览器却声称调研完了"这类假成功)。
func MissingCaps(require, have []string) []string {
	hv := map[string]bool{}
	for _, h := range have {
		hv[strings.ToLower(strings.TrimSpace(h))] = true
	}
	var miss []string
	for _, r := range require {
		k := strings.ToLower(strings.TrimSpace(r))
		if k == "" {
			continue
		}
		if !hv[k] {
			miss = append(miss, k)
		}
	}
	sort.Strings(miss)
	return miss
}

// PlacementCaps 把 Placement 的硬约束投影成队列能力标签。
func PlacementCaps(p *agent.Placement) []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Require))
	for _, r := range p.Require {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			out = append(out, r)
		}
	}
	return out
}
