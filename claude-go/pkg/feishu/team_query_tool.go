package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/textutil"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// TeamQueryTool 让 LLM 能查询团队状态和报告。
// 解决"用户提到团队名时 LLM 找不到"的问题。
type TeamQueryTool struct {
	teamMgr *agent.ProductionTeamManager
}

type teamQueryInput struct {
	Action   string `json:"action"`              // list, get, report
	TeamName string `json:"team_name,omitempty"` // get/report 时必填
}

// NewTeamQueryTool 构造团队查询工具 (mgr 为 nil 时仍注册, 调用时如实报错)。
func NewTeamQueryTool(mgr *agent.ProductionTeamManager) *TeamQueryTool {
	return &TeamQueryTool{teamMgr: mgr}
}

// Name 返回工具名 TeamQuery。
func (t *TeamQueryTool) Name() string { return "TeamQuery" }

// Description 返回给模型看的说明: 三个 action 的语义 + "用户提到团队名时用它"的触发条件。
func (t *TeamQueryTool) Description() string {
	return `查询 Agent Teams 的状态和报告。` +
		`action="list" 列出所有团队及状态; ` +
		`action="get" 获取指定团队详情; ` +
		`action="report" 获取指定团队的完整执行报告。` +
		`当用户提到某个团队名称时，用此工具查找。`
}

// InputSchema 返回输入 schema: action 必填 (list/get/report), team_name 仅 get/report 需要。
func (t *TeamQueryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {"type": "string", "enum": ["list", "get", "report"], "description": "操作类型"},
			"team_name": {"type": "string", "description": "团队名称 (get/report 时必填)"}
		},
		"required": ["action"]
	}`)
}

// IsReadOnly 恒为 true: 三个 action 都只读团队状态与报告文件。
func (t *TeamQueryTool) IsReadOnly(_ json.RawMessage) bool        { return true }

// IsConcurrencySafe 恒为 true: 无共享可变状态, 可与其他工具调用并发。
func (t *TeamQueryTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 返回 nil: 不做工具级权限判定 (只读查询)。
func (t *TeamQueryTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 执行团队查询: list 列全部团队, get 取团队详情, report 取完整执行报告。
//
// get/report 在精确名查不到时会退化成**子串模糊匹配** —— 用户往往只记得团队名的一部分,
// 直接报"未找到"会逼模型去猜; 匹配到多个时把候选列出来让模型自己选。
//
// 注意：report 分支的超长截断用的是**字节切片** (content[:8000]), 中文报告正好
// 切在多字节字符中间时会产出非法 UTF-8; 同文件的 truncateStr 是 rune 安全的。
func (t *TeamQueryTool) Call(_ context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in teamQueryInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("参数错误: %v", err), IsError: true}, nil
	}

	if t.teamMgr == nil {
		return &tool.ToolResult{Content: "团队管理器未初始化", IsError: true}, nil
	}

	switch in.Action {
	case "list":
		teams := t.teamMgr.ListAllTeams()
		if len(teams) == 0 {
			return &tool.ToolResult{Content: "当前没有任何团队。"}, nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("共 %d 个团队:\n\n", len(teams)))
		for _, team := range teams {
			duration := ""
			if !team.FinishedAt.IsZero() {
				duration = fmt.Sprintf(", 耗时 %v", team.FinishedAt.Sub(team.StartedAt).Round(1e9))
			}
			sb.WriteString(fmt.Sprintf("- **%s** [%s] 工作流=%s, 状态=%s%s\n  目标: %s\n",
				team.Name, team.CreatedAt.Format("01-02 15:04"), team.Workflow, team.Status, duration,
				truncateStr(team.Objective, 100)))
		}
		return &tool.ToolResult{Content: sb.String()}, nil

	case "get":
		if in.TeamName == "" {
			return &tool.ToolResult{Content: "请提供 team_name", IsError: true}, nil
		}
		team := t.teamMgr.GetTeam(in.TeamName)
		if team == nil {
			// 模糊匹配: 用户可能只记得部分名称
			teams := t.teamMgr.ListAllTeams()
			var matches []string
			for _, tm := range teams {
				if strings.Contains(tm.Name, in.TeamName) {
					matches = append(matches, tm.Name)
				}
			}
			if len(matches) > 0 {
				return &tool.ToolResult{Content: fmt.Sprintf("未找到团队 %q，可能的匹配: %s", in.TeamName, strings.Join(matches, ", "))}, nil
			}
			return &tool.ToolResult{Content: fmt.Sprintf("未找到团队 %q", in.TeamName)}, nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("## 团队: %s\n", team.Name))
		sb.WriteString(fmt.Sprintf("- 工作流: %s\n", team.Workflow))
		sb.WriteString(fmt.Sprintf("- 目标: %s\n", team.Objective))
		sb.WriteString(fmt.Sprintf("- 状态: %s\n", team.Status))
		sb.WriteString(fmt.Sprintf("- 创建: %s\n", team.CreatedAt.Format("2006-01-02 15:04:05")))
		if !team.StartedAt.IsZero() {
			sb.WriteString(fmt.Sprintf("- 开始: %s\n", team.StartedAt.Format("2006-01-02 15:04:05")))
		}
		if !team.FinishedAt.IsZero() {
			sb.WriteString(fmt.Sprintf("- 完成: %s (耗时 %v)\n", team.FinishedAt.Format("2006-01-02 15:04:05"),
				team.FinishedAt.Sub(team.StartedAt).Round(1e9)))
		}
		if team.Error != "" {
			sb.WriteString(fmt.Sprintf("- 错误: %s\n", team.Error))
		}
		sb.WriteString(fmt.Sprintf("- Agent: %d 个\n\n", len(team.Agents)))
		if len(team.Stages) > 0 {
			sb.WriteString("### 各阶段摘要\n")
			for _, s := range team.Stages {
				status := string(s.Status)
				sb.WriteString(fmt.Sprintf("- **%s** (%s) [%s] 耗时=%s\n", s.Name, s.Role, status, s.Duration))
				if s.Output != "" {
					sb.WriteString(fmt.Sprintf("  %s\n", truncateStr(s.Output, 300)))
				}
			}
		}
		return &tool.ToolResult{Content: sb.String()}, nil

	case "report":
		if in.TeamName == "" {
			return &tool.ToolResult{Content: "请提供 team_name", IsError: true}, nil
		}
		content, path := t.teamMgr.GetTeamReport(in.TeamName)
		if content == "" {
			// 尝试模糊匹配
			teams := t.teamMgr.ListAllTeams()
			for _, tm := range teams {
				if strings.Contains(tm.Name, in.TeamName) {
					content, path = t.teamMgr.GetTeamReport(tm.Name)
					if content != "" {
						break
					}
				}
			}
		}
		if content == "" {
			return &tool.ToolResult{Content: fmt.Sprintf("未找到团队 %q 的报告文件", in.TeamName)}, nil
		}
		if len(content) > 8000 {
			content = content[:8000] + "\n\n...(报告过长，已截断。完整内容见文件: " + path + ")"
		}
		return &tool.ToolResult{Content: content}, nil

	default:
		return &tool.ToolResult{Content: "未知 action，可选: list, get, report", IsError: true}, nil
	}
}

// truncateStr 按字符数截断并补 "..." (委托给 textutil, 保证多字节字符不被切断)。
func truncateStr(s string, maxLen int) string {
	return textutil.Truncate(s, maxLen, "...")
}
