package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/prjvvl/agen/platform/internal/sqltools"
	"github.com/prjvvl/agen/platform/internal/workspace"
)

// cmdMCP runs a built-in MCP tool server on stdin/stdout for a bundle's
// mcp.json, e.g. {"command": "agen", "args": ["mcp", "workspace", "--root", "/work"]}.
func (e *env) cmdMCP(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "sql" {
		return e.cmdMCPSQL(ctx, args[1:])
	}
	if len(args) == 0 || args[0] != "workspace" {
		return usageErr("agen mcp workspace --root DIR [--no-run] [--run-timeout D] | agen mcp sql [--dsn-env NAME]")
	}
	fs := flag.NewFlagSet("mcp workspace", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	root := fs.String("root", "", "the folder the agent may read, change and run commands in (required)")
	noRun := fs.Bool("no-run", false, "leave out the run tool (files only)")
	runTimeout := fs.Duration("run-timeout", 2*time.Minute, "default time limit of one command")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	if *root == "" {
		return usageErr("agen mcp workspace --root DIR: --root is required")
	}
	s, err := workspace.New(*root)
	if err != nil {
		return err
	}
	s.AllowRun = !*noRun
	s.RunTimeout = *runTimeout
	s.MaxRunTimeout = max(s.MaxRunTimeout, *runTimeout)
	return s.Serve(ctx, os.Stdin, e.stdout)
}

// cmdMCPSQL serves read-only SQL tools for the database named in an
// environment variable (set it from a bundle secret in mcp.json "env").
func (e *env) cmdMCPSQL(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp sql", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	name := fs.String("dsn-env", "DATABASE_URL", "environment variable holding the connection string (postgres://... or a SQLite file path)")
	timeout := fs.Duration("timeout", 30*time.Second, "time limit of one query")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := sqltools.Open(os.Getenv(*name))
	if err != nil {
		return fmt.Errorf("%s: %w", *name, err)
	}
	defer s.Close()
	s.Timeout = *timeout
	return s.Serve(ctx, os.Stdin, e.stdout)
}
