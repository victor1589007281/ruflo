package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ruflo/ruflo-go/internal/cli"
	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/mcp/tools"
	"golang.org/x/term"
)

func main() {
	reg := mcp.NewToolRegistry()
	if err := tools.RegisterAll(reg); err != nil {
		fmt.Fprintf(os.Stderr, "ruflo: register tools: %v\n", err)
		os.Exit(1)
	}
	cli.ToolRegistry = reg

	stdinFd := int(os.Stdin.Fd())
	// Claude Code and similar hosts spawn `ruflo` with no argv beyond the binary and a piped stdin.
	// If the user passes any subcommand (e.g. `ruflo status`), always run the CLI even when stdin is a pipe.
	autoMCP := len(os.Args) == 1 && !term.IsTerminal(stdinFd)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if autoMCP {
		if err := cli.RunMCPServer(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "ruflo mcp: %v\n", err)
			os.Exit(1)
		}
		return
	}

	root := cli.NewRoot()
	root.SilenceErrors = true
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
