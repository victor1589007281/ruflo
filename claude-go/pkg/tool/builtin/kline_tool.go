package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/browser"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

const KLineToolName = "FetchKLine"

type klineInput struct {
	Symbol string `json:"symbol"`
	Period string `json:"period,omitempty"`
	Count  int    `json:"count,omitempty"`
}

// KLineTool 获取股票 K 线数据的工具。
type KLineTool struct {
	client *browser.FinanceClient
}

// NewKLineTool 构造 K 线工具 (内含一个默认的东财 FinanceClient)。
func NewKLineTool() *KLineTool {
	return &KLineTool{client: browser.NewFinanceClient()}
}

// Name 返回工具对外名 "FetchKLine"。
func (t *KLineTool) Name() string { return KLineToolName }

// Description 返回给模型看的工具说明 (支持的代码写法与数据源)。
func (t *KLineTool) Description() string {
	return `获取 A 股股票 K 线数据（日/周/月线），返回 OHLCV 数据。
支持代码: 600519(贵州茅台), 000001(平安银行), SH600036, SZ000858 等。
数据源: 东方财富，免费无需 API key。`
}

// InputSchema 返回入参 JSON Schema (symbol 必填; period 枚举 daily/weekly/monthly)。
func (t *KLineTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol": {
				"type": "string",
				"description": "股票代码，如 600519、000001、SH600036"
			},
			"period": {
				"type": "string",
				"enum": ["daily", "weekly", "monthly"],
				"description": "K线周期：daily(日线)、weekly(周线)、monthly(月线)，默认 daily"
			},
			"count": {
				"type": "integer",
				"description": "返回数据条数，默认 120"
			}
		},
		"required": ["symbol"]
	}`)
}

// IsReadOnly 恒只读: 只发 HTTP 查询, 不落盘也不改远端状态。
func (t *KLineTool) IsReadOnly(_ json.RawMessage) bool        { return true }
// IsConcurrencySafe 恒可并发: 只读且 client 无共享可变状态。
func (t *KLineTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }
// CheckPermissions 不附加 Deny —— 只读行情查询交由上层策略决定是否放行。
func (t *KLineTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Call 校验 symbol 后向数据源取 K 线, 失败一律以 IsError 结果回注 (不返回 error 给编排层)。
func (t *KLineTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in klineInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.Symbol) == "" {
		return &tool.ToolResult{Content: "错误: symbol 不能为空", IsError: true}, nil
	}

	kl, err := t.client.FetchKLine(ctx, in.Symbol, in.Period, in.Count)
	if err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("获取 K 线失败: %v", err), IsError: true}, nil
	}

	text := browser.FormatKLineText(kl)
	return &tool.ToolResult{Content: text}, nil
}
