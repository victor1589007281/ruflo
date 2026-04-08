// askuser.go 实现 AskUserQuestion 工具：向终端用户提出单选/多选问题。
// 对应 TS 源码: review/claude/src/tools/AskUserQuestionTool/AskUserQuestion.tsx
//
// 设计要点:
//   - 只读（不写仓库文件）、并发安全（各调用独立读 stdin；实际产品中常串行化交互）。
//   - requiresUserInteraction：非交互模式（ToolContext.IsNonInteractive）下必须在 CheckPermissions
//     或 Call 中失败；本实现两者兼顾——CheckPermissions 先 Deny，Call 再次兜底。
//
// 算法说明:
//  1. 校验 question 非空，options 至少 2 项且每项 id、label 非空，id 互不重复。
//  2. 将问题与选项打印到 stdout，提示输入格式：
//       - 单选: 输入一个 option id
//       - 多选: 输入多个 id，以英文逗号分隔
//  3. 使用 bufio 从 stdin 读取一行，trim 后解析。
//  4. 校验所选 id 均为合法选项；多选去重保序；将最终选择格式化为明确文本返回给模型。
package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const AskUserQuestionToolName = "AskUserQuestion"

// askUserOption 单个选项（与 JSON Schema 中 items 一致）。
type askUserOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// askUserInput 对应 LLM 传入的 JSON。
type askUserInput struct {
	Question      string          `json:"question"`
	Options       []askUserOption `json:"options"`
	AllowMultiple bool            `json:"allow_multiple,omitempty"`
}

// AskUserQuestionTool 终端交互式提问工具。
type AskUserQuestionTool struct{}

// NewAskUserQuestionTool 构造 AskUserQuestion 工具实例。
func NewAskUserQuestionTool() *AskUserQuestionTool {
	return &AskUserQuestionTool{}
}

func (t *AskUserQuestionTool) Name() string { return AskUserQuestionToolName }

func (t *AskUserQuestionTool) Description() string {
	return `Asks the user a multiple-choice or single-choice question via the terminal (stdin). In non-interactive mode the tool cannot run.`
}

func (t *AskUserQuestionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"question": {
				"type": "string",
				"description": "The question to present to the user."
			},
			"options": {
				"type": "array",
				"minItems": 2,
				"items": {
					"type": "object",
					"properties": {
						"id": {"type": "string", "description": "Stable option id returned when selected."},
						"label": {"type": "string", "description": "Human-readable label shown to the user."}
					},
					"required": ["id", "label"]
				},
				"description": "At least two options; each must have id and label."
			},
			"allow_multiple": {
				"type": "boolean",
				"description": "If true, user may select multiple options (comma-separated ids)."
			}
		},
		"required": ["question", "options"]
	}`)
}

func (t *AskUserQuestionTool) IsReadOnly(_ json.RawMessage) bool { return true }

func (t *AskUserQuestionTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 非交互模式下拒绝执行，避免挂起或读到意外 stdin。
func (t *AskUserQuestionTool) CheckPermissions(_ json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx != nil && tctx.IsNonInteractive {
		return &types.PermissionResult{
			Behavior: types.PermissionDeny,
			Reason:   "非交互模式无法向用户提问（AskUserQuestion 需要终端 stdin）",
		}
	}
	return nil
}

// Call 打印问题并从 stdin 读取用户选择。
func (t *AskUserQuestionTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	if tctx != nil && tctx.IsNonInteractive {
		return &tool.ToolResult{
			Content: "错误: 非交互模式下无法执行 AskUserQuestion",
			IsError: true,
		}, nil
	}

	var in askUserInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}

	if strings.TrimSpace(in.Question) == "" {
		return &tool.ToolResult{Content: "错误: question 不能为空", IsError: true}, nil
	}
	if len(in.Options) < 2 {
		return &tool.ToolResult{Content: fmt.Sprintf("错误: options 至少需要 2 项，当前 %d 项", len(in.Options)), IsError: true}, nil
	}

	idSet := make(map[string]struct{}, len(in.Options))
	for i, opt := range in.Options {
		oid := strings.TrimSpace(opt.ID)
		olab := strings.TrimSpace(opt.Label)
		if oid == "" || olab == "" {
			return &tool.ToolResult{Content: fmt.Sprintf("错误: options[%d] 的 id 与 label 均不能为空", i), IsError: true}, nil
		}
		if _, dup := idSet[oid]; dup {
			return &tool.ToolResult{Content: fmt.Sprintf("错误: 重复的 option id %q", oid), IsError: true}, nil
		}
		idSet[oid] = struct{}{}
		in.Options[i].ID = oid
		in.Options[i].Label = olab
	}

	fmt.Fprintln(os.Stdout, in.Question)
	for _, opt := range in.Options {
		fmt.Fprintf(os.Stdout, "  [%s] %s\n", opt.ID, opt.Label)
	}
	if in.AllowMultiple {
		fmt.Fprint(os.Stdout, "\n请输入一个或多个选项 id，以英文逗号分隔: ")
	} else {
		fmt.Fprint(os.Stdout, "\n请输入一个选项 id: ")
	}

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("读取用户输入失败: %v", err), IsError: true}, nil
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return &tool.ToolResult{Content: "错误: 未收到任何输入", IsError: true}, nil
	}

	var selected []string
	if in.AllowMultiple {
		parts := strings.Split(line, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			selected = append(selected, p)
		}
		if len(selected) == 0 {
			return &tool.ToolResult{Content: "错误: 多选模式下未解析到任何有效的 id", IsError: true}, nil
		}
		selected = uniquePreserveOrder(selected)
	} else {
		if strings.Contains(line, ",") {
			return &tool.ToolResult{Content: "错误: 单选模式下不能包含逗号，请只输入一个 id", IsError: true}, nil
		}
		selected = []string{line}
	}

	for _, sid := range selected {
		if _, ok := idSet[sid]; !ok {
			return &tool.ToolResult{Content: fmt.Sprintf("错误: 无效的选项 id %q", sid), IsError: true}, nil
		}
	}

	// 组装人类可读摘要，便于模型消费。
	labelByID := make(map[string]string, len(in.Options))
	for _, opt := range in.Options {
		labelByID[opt.ID] = opt.Label
	}
	var out strings.Builder
	out.WriteString("用户已作答。\n")
	if in.AllowMultiple {
		out.WriteString("选择类型: 多选\n")
	} else {
		out.WriteString("选择类型: 单选\n")
	}
	for _, sid := range selected {
		fmt.Fprintf(&out, "- id=%q label=%q\n", sid, labelByID[sid])
	}

	return &tool.ToolResult{Content: strings.TrimSpace(out.String())}, nil
}

// uniquePreserveOrder 在保留首次出现顺序的前提下去重。
func uniquePreserveOrder(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	var out []string
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
