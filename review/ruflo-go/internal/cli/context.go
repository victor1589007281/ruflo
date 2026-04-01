package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ruflo/ruflo-go/mcp"
)

// Global CLI wiring (set from cmd/ruflo before Execute).
var (
	ConfigPath   string
	Verbose      bool
	OutputFormat string // "json" or "text"
	ToolRegistry *mcp.ToolRegistry
)

// RunTool 调用指定名称的 MCP 工具并将原始 JSON 结果交给 FprintResult，使用当前全局 OutputFormat 输出。
func RunTool(ctx context.Context, name string, args map[string]any) error {
	raw, err := CallTool(ctx, name, args)
	if err != nil {
		return err
	}
	return FprintResult(Stdout(), raw)
}

// CallTool 将 args 编码为 JSON，经全局 ToolRegistry 分发给已注册工具并返回原始 JSON 字节；未初始化注册表时返回错误。
func CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	if ToolRegistry == nil {
		return nil, fmt.Errorf("cli: tool registry not initialized")
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return ToolRegistry.Call(ctx, name, b)
}

// FprintResult writes tool output in json or text/table form.
func FprintResult(w io.Writer, raw json.RawMessage) error {
	if OutputFormat == "json" {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	return fprintText(w, raw)
}

func fprintText(w io.Writer, raw json.RawMessage) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		_, err := fmt.Fprintln(w, string(raw))
		return err
	}
	switch {
	case m["agents"] != nil:
		return printAgentsTable(w, raw)
	case m["checks"] != nil:
		var wrap struct {
			Checks json.RawMessage `json:"checks"`
		}
		if err := json.Unmarshal(raw, &wrap); err != nil {
			_, err := fmt.Fprintln(w, string(raw))
			return err
		}
		return printChecksTable(w, wrap.Checks)
	default:
		_, err := fmt.Fprintf(w, "%s\n", prettyCompact(raw))
		return err
	}
}

// prettyCompact 尝试将 JSON 美化缩进，失败则退回原始字符串。
func prettyCompact(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// printAgentsTable 解析含 agents 数组的响应并以制表符对齐列输出 ID/NAME/TYPE/STATE/STATUS。
func printAgentsTable(w io.Writer, raw json.RawMessage) error {
	var wrap struct {
		Agents []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Type   string `json:"type"`
			State  string `json:"state"`
			Status string `json:"status"`
		} `json:"agents"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		_, err := fmt.Fprintln(w, string(raw))
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tNAME\tTYPE\tSTATE\tSTATUS")
	for _, a := range wrap.Agents {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Type, a.State, a.Status)
	}
	_ = tw.Flush()
	_, err := fmt.Fprintf(w, "(%d agents)\n", wrap.Count)
	return err
}

// printChecksTable 将 doctor 类检查结果渲染为 CHECK/OK/DETAIL 表格。
func printChecksTable(w io.Writer, raw json.RawMessage) error {
	var checks []struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(raw, &checks); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHECK\tOK\tDETAIL")
	for _, c := range checks {
		ok := "no"
		if c.OK {
			ok = "yes"
		}
		d := strings.TrimSpace(c.Detail)
		if c.Path != "" {
			d = c.Path + " " + d
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, ok, d)
	}
	return tw.Flush()
}

// Stdout 返回命令默认标准输出 Writer，便于测试或替换。
func Stdout() io.Writer { return os.Stdout }
