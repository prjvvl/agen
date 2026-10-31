package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// cmdReplay re-runs a recorded run offline (3.6, part): it assembles the
// run's own bundle (GetDefinition, by the run's definition_digest) with
// its harness.json pointed at the run's saved cassette (GetCassette)
// instead of a live model, then drives it locally with `agen-host run`
// (the same standalone path `agen-host run <bundle> --input ...` already
// uses) — deterministic, offline, no Hub task involved. Only works for a
// run of a deployment that had recordInteractions on; GetCassette
// answers not_found otherwise.
//
// This replays the run exactly as recorded, start to finish; resuming
// from a turn N with a live model from there on (`--turn`/`--model`)
// needs a provider that can switch mid-run from replay to live, which
// does not exist yet — a larger piece of engine work, deliberately not
// attempted in this slice.
func (e *env) cmdReplay(ctx context.Context, args []string) error {
	var cf clientFlags
	fs := e.flags("replay", &cf)
	hostBin := fs.String("host-bin", "", "agen-host executable (default: next to agen, $AGEN_HOST_BIN, or on PATH)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("agen replay <run-id> [--host-bin PATH]")
	}
	c, err := e.client(cf)
	if err != nil {
		return err
	}
	cass, err := c.GetCassette(ctx, connect.NewRequest(&agenv1.GetCassetteRequest{RunId: pos[0]}))
	if err != nil {
		return err
	}
	run := cass.Msg.Run
	def, err := c.GetDefinition(ctx, connect.NewRequest(&agenv1.GetDefinitionRequest{Digest: run.DefinitionDigest}))
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "agen-replay-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for path, content := range def.Msg.Definition.Files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return err
		}
	}
	cassettePath := filepath.Join(dir, "x-agen", "_replay-cassette.json")
	if err := os.WriteFile(cassettePath, []byte(cass.Msg.CassetteJson), 0o644); err != nil {
		return err
	}
	harnessPath := filepath.Join(dir, "x-agen", "harness.json")
	raw, err := os.ReadFile(harnessPath)
	if err != nil {
		return fmt.Errorf("the run's own bundle has no x-agen/harness.json: %w", err)
	}
	var harness map[string]any
	if err := json.Unmarshal(raw, &harness); err != nil {
		return fmt.Errorf("x-agen/harness.json: %w", err)
	}
	harness["provider"] = "replay"
	harness["cassette"] = "x-agen/_replay-cassette.json"
	rewritten, err := json.Marshal(harness)
	if err != nil {
		return err
	}
	if err := os.WriteFile(harnessPath, rewritten, 0o644); err != nil {
		return err
	}
	bin, err := findHostBin(*hostBin)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "replaying run %s (%s/%s) from its recorded cassette:\n\n", run.Id, run.Namespace, run.Deployment)
	cmd := exec.CommandContext(ctx, bin, "run", dir, "--input", run.Input)
	cmd.Stdout = e.stdout
	cmd.Stderr = e.stderr
	return cmd.Run()
}
