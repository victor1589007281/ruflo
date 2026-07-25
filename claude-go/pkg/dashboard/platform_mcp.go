package dashboard

// platform_mcp.go — design/02 §3.5 L5「MCP: 新增 platform-mcp-server」的接线。
//
// 设计稿要求把 TaskService/workflows/skills/teams 以 MCP 工具对外, 作为远程
// agent 管理平台的标准入口之一。协议实现在 pkg/platformmcp (与传输无关),
// 本文件只做两件事:
//
//  1. 把 dashboard 已有的数据源 (Provider / agent.ListWorkflows / skills 注册表 /
//     tool 注册表) 与注入的 TaskService 适配成 platformmcp.Backend;
//  2. 把 HTTP 端点挂到 :18080 的 /api/mcp/rpc。
//
// 为什么落在 /api/ 前缀下: pkg/httpauth 的保护前缀白名单是
// {/api/, /wiki/, /sync/, /cluster/}。新增一个能提交任务的对外端点如果挂在
// /mcp 上, 就成了鉴权白名单外的一个洞 —— 白名单式保护的代价就是新增前缀必须
// 显式登记, 与其改白名单不如直接用已被保护的前缀。
//
// 任务能力从哪来: 复用**已有的** SetActionSink 注入点 (cmd/claude-go/main.go
// 注入的是 *agent.FileQueueTaskService)。它同时实现了 agent.TaskService,
// 于是不需要第二个注入点就能拿到 Submit/Get/List —— 少一个注入点就少一处
// "忘了接线所以永远不可用"。未注入时 (只读 :7777 dashboard) 任务类工具如实报错。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/platformmcp"
)

// PlatformMCPPath 是平台 MCP server 的 HTTP 端点路径。
const PlatformMCPPath = "/api/mcp/rpc"

// registerPlatformMCP 把平台 MCP server 挂到给定 mux。
// 由 registerRoutesOn 调用, 因此 :18080 (飞书挂载) 与 :7777 (独立 dashboard)
// 两条装配路径同时具备该端点 —— 后者没有 TaskService, 任务类工具会如实报错。
func (s *Server) registerPlatformMCP(mux *http.ServeMux) {
	mux.Handle(PlatformMCPPath, platformmcp.New(s.PlatformMCPBackend()).Handler())
}

// platformMCPBackend 把 dashboard 的数据源适配成 platformmcp.Backend。
type platformMCPBackend struct{ s *Server }

// PlatformMCPBackend 返回本 dashboard 实例的 MCP Backend。
// 导出以便 stdio 形态 (claude-go platform-mcp-server 子命令) 复用同一份实现,
// 保证 HTTP 与 stdio 两个传输看到的能力完全一致。
func (s *Server) PlatformMCPBackend() platformmcp.Backend { return platformMCPBackend{s: s} }

// taskService 取注入的动作队列消费方并断言成 agent.TaskService。
// 返回 nil 表示本进程没有任务服务 (只读 dashboard)。
func taskService() agent.TaskService {
	if ts, ok := currentActionSink().(agent.TaskService); ok {
		return ts
	}
	return nil
}

func (b platformMCPBackend) Teams() (any, error) { return b.s.provider.ListTeams() }

func (b platformMCPBackend) Team(name string) (any, error) {
	if !safeName(name) {
		return nil, fmt.Errorf("非法团队名 %q", name)
	}
	return b.s.provider.GetTeam(name)
}

// workflowBrief 是 MCP 侧的工作流摘要。刻意只出名称/描述/模式/阶段名:
// WorkflowDef 里还有 prompt 全文, 那些进 MCP 结果就是白烧 agent 的上下文。
type workflowBrief struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mode        string   `json:"mode,omitempty"`
	Stages      []string `json:"stages,omitempty"`
}

func (b platformMCPBackend) Workflows() (any, error) {
	defs := agent.ListWorkflows()
	out := make([]workflowBrief, 0, len(defs))
	for _, wf := range defs {
		brief := workflowBrief{Name: wf.Name, Description: wf.Description, Mode: wf.Mode}
		for _, st := range wf.Stages {
			brief.Stages = append(brief.Stages, st.Name)
		}
		out = append(out, brief)
	}
	return out, nil
}

type skillBrief struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	WhenToUse   string `json:"when_to_use,omitempty"`
	LoadedFrom  string `json:"loaded_from"`
}

func (b platformMCPBackend) Skills() (any, error) {
	list := b.s.freshSkillRegistry().All()
	out := make([]skillBrief, 0, len(list))
	for _, sk := range list {
		out = append(out, skillBrief{
			Name: sk.Name, Description: sk.Description,
			WhenToUse: sk.WhenToUse, LoadedFrom: sk.LoadedFrom,
		})
	}
	return out, nil
}

type toolBrief struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

func (b platformMCPBackend) Tools() (any, error) {
	all := toolRegistry().All()
	out := make([]toolBrief, 0, len(all))
	for _, t := range all {
		out = append(out, toolBrief{Name: t.Name(), Description: t.Description(), InputSchema: t.InputSchema()})
	}
	return out, nil
}

func (b platformMCPBackend) Tasks() (any, error) {
	ts := taskService()
	if ts == nil {
		// 退回只读视图: 磁盘上的 tasks.json 仍能列出来, 比直接报错有用。
		// 但要说清这不是 TaskService 的档案, 免得调用方拿 id 去 task_status 查不到。
		list, err := b.s.provider.ListTasks()
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"source": "tasks.json (只读视图; 本进程未接入 TaskService)",
			"tasks":  list,
		}, nil
	}
	return map[string]any{"source": "taskservice", "tasks": ts.List(agent.TaskFilter{Limit: 100})}, nil
}

func (b platformMCPBackend) SubmitTask(spec platformmcp.TaskSubmit) (any, error) {
	ts := taskService()
	if ts == nil {
		return nil, fmt.Errorf("%w: 本进程未注入 TaskService —— 请直连 :18080 "+
			"(飞书主进程, 已调用 dashboard.SetActionSink) 提交任务", platformmcp.ErrUnavailable)
	}
	team := strings.TrimSpace(spec.Team)
	if team == "" {
		return nil, fmt.Errorf("team 必填 (它是运行身份, TaskService 的幂等键由 team|workflow|objective 派生)")
	}
	if !safeName(team) {
		return nil, fmt.Errorf("非法团队名 %q", team)
	}
	rec, err := ts.Submit(agent.TaskSpec{
		Team:      team,
		Workflow:  strings.TrimSpace(spec.Workflow),
		Objective: strings.TrimSpace(spec.Objective),
		Language:  strings.TrimSpace(spec.Language),
	}, agent.SubmitOpts{Source: "platform-mcp"})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (b platformMCPBackend) TaskStatus(id string) (any, error) {
	ts := taskService()
	if ts == nil {
		return nil, fmt.Errorf("%w: 本进程未注入 TaskService", platformmcp.ErrUnavailable)
	}
	rec, ok := ts.Get(id)
	if !ok {
		return nil, fmt.Errorf("任务 %q 不存在", id)
	}
	return rec, nil
}
