// Package cli implements the `agen` command: a client of the Hub API
// (deploy, scale, ps, logs, ...) and the server modes (hub serve, nest run,
// and the all-in-one `agen up`).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/pki"
	"github.com/prjvvl/agen/platform/internal/version"
)

type env struct {
	stdout, stderr io.Writer
	stdin          io.Reader // nil: os.Stdin
	home           string
}

type usageError string

func (u usageError) Error() string { return "usage: " + string(u) }

func usageErr(s string) error { return usageError(s) }

const topUsage = `agen — run and manage agent fleets

Platform:
  agen up                         Hub + local Nest in one process (SQLite)
  agen down                       stop a running 'agen up' (drains instances)
  agen ui                         sign-in link for the web UI
  agen hub serve                  run a Hub
  agen nest run --hub URL         run a Nest Manager
  agen migrate --to URL           copy the local store into Postgres (distributed)

Fleet (client of the Hub; --hub/--token or AGEN_HUB/AGEN_TOKEN, else the local config from agen up):
  agen deploy <bundle-dir>        create or update a deployment (--validate: check only)
  agen scale <name> <n>           set desired instances
  agen ps [--all]                 deployments (--all: every instance on every nest)
  agen logs <name> [-f]           operational logs
  agen stop <name>                stop all instances and hold at zero (paused)
  agen start <name>               resume a stopped deployment
  agen rm <name>                  delete a deployment
  agen run <name> <input>         submit a task and wait for its result
  agen call <name> <text>         A2A message to the deployment (wakes it if asleep)
  agen tasks [name]               recent tasks
  agen trace <task-id|trace-id>   span tree of a trace, across agents
  agen triggers <name>            trigger events (fired, missed, rejected)
  agen webhook-secret <name> <trigger>  create/rotate a webhook secret
  agen nests                      enrolled nests
  agen approvals | approve <id> | deny <id>
  agen join-token                 one-time token for 'agen nest run'
  agen token create --scope S     API token
  agen whoami                     the current token's identity and scopes
  agen version
`

// Main runs the CLI and returns the exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	home := os.Getenv("AGEN_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".agen")
		}
	}
	e := &env{stdout: stdout, stderr: stderr, home: home}
	if len(args) == 0 {
		fmt.Fprint(stderr, topUsage)
		return 2
	}
	cmds := map[string]func(context.Context, []string) error{
		"up": e.cmdUp, "down": e.cmdDown, "ui": e.cmdUI, "migrate": e.cmdMigrate, "store": e.cmdStore, "secret": e.cmdSecret, "hub": e.cmdHub, "nest": e.cmdNest,
		"deploy": e.cmdDeploy, "scale": e.cmdScale, "ps": e.cmdPs, "logs": e.cmdLogs, "stop": e.cmdStop, "start": e.cmdStart, "rm": e.cmdRm,
		"run": e.cmdRun, "call": e.cmdCall, "resolve": e.cmdResolve, "triggers": e.cmdTriggers, "trace": e.cmdTrace, "webhook-secret": e.cmdWebhookSecret, "tasks": e.cmdTasks, "nests": e.cmdNests, "approvals": e.cmdApprovals,
		"approve": e.decide(true), "deny": e.decide(false), "join-token": e.cmdJoinToken, "token": e.cmdToken, "whoami": e.cmdWhoami,
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, version.String("agen"))
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, topUsage)
		return 0
	}
	f, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "agen: unknown command %q\n\n%s", args[0], topUsage)
		return 2
	}
	if err := f(ctx, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "agen:", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

// localConfig is written by `agen up` so local commands need no flags.
type localConfig struct {
	Hub   string `json:"hub"`
	Token string `json:"token"`
}

func loadLocalConfig(home string) (localConfig, error) {
	var c localConfig
	b, err := os.ReadFile(filepath.Join(home, "local.json"))
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// saveLocalConfig writes local.json readable by the owner only (0600, also
// enforced on an existing file; on Windows the file inherits the user
// profile's ACL).
func saveLocalConfig(home string, c localConfig) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	p := filepath.Join(home, "local.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}

// clientFlags are shared by every Hub client command.
type clientFlags struct {
	hub, token, namespace, caHash string
	json                          bool
}

func (e *env) flags(name string, cf *clientFlags) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.StringVar(&cf.hub, "hub", os.Getenv("AGEN_HUB"), "Hub URL")
	fs.StringVar(&cf.token, "token", os.Getenv("AGEN_TOKEN"), "API token")
	fs.StringVar(&cf.namespace, "n", "default", "namespace")
	fs.BoolVar(&cf.json, "json", false, "print the API response as JSON")
	fs.StringVar(&cf.caHash, "ca-hash", os.Getenv("AGEN_CA_HASH"), "pin the Hub CA of an https Hub")
	return fs
}

// parse parses flags that may appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func (e *env) client(cf clientFlags) (agenv1connect.HubServiceClient, error) {
	if cf.hub == "" || cf.token == "" {
		lc, err := loadLocalConfig(e.home)
		switch {
		case err != nil && cf.hub == "":
			return nil, errors.New("no Hub configured: run 'agen up', or pass --hub and --token (AGEN_HUB / AGEN_TOKEN)")
		case cf.hub != "" && (err != nil || strings.TrimRight(cf.hub, "/") != strings.TrimRight(lc.Hub, "/")):
			// Never send the local admin token to another Hub.
			return nil, errors.New("--token (or AGEN_TOKEN) is required for " + cf.hub)
		}
		if cf.hub == "" {
			cf.hub = lc.Hub
		}
		if cf.token == "" {
			cf.token = lc.Token
		}
	}
	base := http.DefaultTransport
	if cf.caHash != "" {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = pki.PinnedTLS(cf.caHash, pki.HostOf(cf.hub))
		base = tr
	}
	hc := &http.Client{Transport: bearer{token: cf.token, base: base}}
	return agenv1connect.NewHubServiceClient(hc, strings.TrimRight(cf.hub, "/"), connect.WithProtoJSON()), nil
}

// printJSON writes a response message as indented protojson.
func (e *env) printJSON(m proto.Message) error {
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: false}.Marshal(m)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(e.stdout, string(b))
	return err
}
