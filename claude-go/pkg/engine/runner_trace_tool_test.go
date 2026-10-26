package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/evolution/tracestore"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/trace"
)

func TestRunIsolatedWritesToolTimingAndPermissionPolicy(t *testing.T) {
	id, cfg := stateIdentity()
	u := newStateUpstream(t, 1, "done")
	e := newStateEngine(t, u, cfg, t.TempDir())
	ts := tracestore.New(statestore.NewFileStore(filepath.Join(t.TempDir(), "statestore")))
	e.TraceStore = ts
	ctx := trace.With(context.Background(), trace.IDs{RunID: "run-tool-trace", NodeID: "ops", TurnID: "turn-1"})
	if _, err := e.RunIsolated(ctx, "排查", IsolatedRunOptions{MaxTurns: 3, Stage: id.Stage}); err != nil {
		t.Fatalf("RunIsolated failed: %v", err)
	}
	spans, err := ts.ReadRun("run-tool-trace")
	if err != nil {
		t.Fatal(err)
	}
	var toolSpans, nodeSpans int
	for _, s := range spans {
		switch s.Kind {
		case tracestore.KindToolCall:
			toolSpans++
			if s.Name != "noop_tool" || s.ParentID != "turn-1" || s.TS == 0 || s.DurMS < 0 {
				t.Errorf("invalid tool span identity/timing: %+v", s)
			}
			if s.Attrs["status"] != "completed" || s.Attrs["permission_mode"] != "acceptEdits" || s.Attrs["isolated"] != true {
				t.Errorf("tool span lost effective execution policy/status: %+v", s.Attrs)
			}
			out, err := ts.Resolve(s.OutputRef)
			if err != nil || out != "noop ok" {
				t.Errorf("tool result must remain reconstructable: out=%q err=%v", out, err)
			}
		case tracestore.KindNode:
			nodeSpans++
			if s.Attrs["permission_mode"] != "acceptEdits" {
				t.Errorf("node span lost effective permission mode: %+v", s.Attrs)
			}
			if _, ok := s.Attrs["allowed_tools_set"]; !ok {
				t.Errorf("node span must distinguish unset and empty tool whitelist: %+v", s.Attrs)
			}
		}
	}
	if toolSpans != 1 || nodeSpans != 1 {
		t.Fatalf("expected one tool and one node span, tool=%d node=%d spans=%+v", toolSpans, nodeSpans, spans)
	}
}
