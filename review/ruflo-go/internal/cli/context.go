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

// RunTool invokes an MCP tool and prints the result using the global output format.
func RunTool(ctx context.Context, name string, args map[string]any) error {
	raw, err := CallTool(ctx, name, args)
	if err != nil {
		return err
	}
	return FprintResult(Stdout(), raw)
}

// CallTool invokes a registered MCP tool with JSON-object arguments.
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

// Stdout is the default writer for commands.
func Stdout() io.Writer { return os.Stdout }
