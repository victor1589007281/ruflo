package transport_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/mcp/transport"
)

func TestInitializeHandshake(t *testing.T) {
	srv, pwIn, prOut, done := testServer(t)
	defer done()

	line := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	if _, err := io.WriteString(pwIn, line); err != nil {
		t.Fatal(err)
	}
	raw, err := readLineJSON(t, prOut)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %v", resp.Error)
	}
	if resp.Result.ProtocolVersion == "" {
		t.Fatal("missing protocolVersion")
	}
	if resp.Result.ServerInfo.Name != srv.Name {
		t.Fatalf("server name: %q", resp.Result.ServerInfo.Name)
	}
}

func TestToolsList(t *testing.T) {
	reg := mcp.NewToolRegistry()
	_ = reg.Register(&mcp.MCPTool{
		Name:        "alpha",
		Description: "first",
		Handler: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	})
	srv := mcp.NewMCPServer(reg)
	srv.Name = "test"

	prIn, pwIn := io.Pipe()
	prOut, pwOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- transport.ServeStdio(ctx, srv, prIn, pwOut) }()

	send := func(s string) {
		t.Helper()
		if _, err := io.WriteString(pwIn, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"jsonrpc":"2.0","id":10,"method":"tools/list","params":{}}`)
	raw, err := readLineJSON(t, prOut)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("error: %v", resp.Error)
	}
	if len(resp.Result.Tools) != 1 || resp.Result.Tools[0].Name != "alpha" {
		t.Fatalf("tools: %#v", resp.Result.Tools)
	}
	_ = pwIn.Close()
	select {
	case e := <-errCh:
		if e != nil && e != context.Canceled {
			t.Log(e)
		}
	case <-time.After(time.Second):
	}
}

func TestToolsCall(t *testing.T) {
	reg := mcp.NewToolRegistry()
	_ = reg.Register(&mcp.MCPTool{
		Name: "add",
		Handler: func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"sum":3}`), nil
		},
	})
	srv := mcp.NewMCPServer(reg)

	prIn, pwIn := io.Pipe()
	prOut, pwOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = transport.ServeStdio(ctx, srv, prIn, pwOut) }()

	req := `{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"add","arguments":{"a":1,"b":2}}}`
	if _, err := io.WriteString(pwIn, req+"\n"); err != nil {
		t.Fatal(err)
	}
	raw, err := readLineJSON(t, prOut)
	if err != nil {
		t.Fatal(err)
	}
	var outer struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		t.Fatal(err)
	}
	if outer.Error != nil {
		t.Fatalf("rpc error: %v", outer.Error)
	}
	if len(outer.Result.Content) != 1 || outer.Result.Content[0].Type != "text" {
		t.Fatalf("content: %#v", outer.Result.Content)
	}
	var inner struct {
		Sum int `json:"sum"`
	}
	if err := json.Unmarshal([]byte(outer.Result.Content[0].Text), &inner); err != nil {
		t.Fatal(err)
	}
	if inner.Sum != 3 {
		t.Fatalf("sum=%d", inner.Sum)
	}
	_ = pwIn.Close()
}

func testServer(t *testing.T) (*mcp.MCPServer, io.Writer, io.Reader, func()) {
	t.Helper()
	reg := mcp.NewToolRegistry()
	srv := mcp.NewMCPServer(reg)
	prIn, pwIn := io.Pipe()
	prOut, pwOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- transport.ServeStdio(ctx, srv, prIn, pwOut) }()
	done := func() {
		_ = pwIn.Close()
		cancel()
		select {
		case <-errCh:
		case <-time.After(500 * time.Millisecond):
		}
	}
	return srv, pwIn, prOut, done
}

func readLineJSON(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	br := bufio.NewReader(r)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(line))), nil
}
