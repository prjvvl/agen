package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/templates"
)

// modelChoice is what `agen init` changes in a template's harness.json.
type modelChoice struct {
	model, tier, maxTier, fallback string
}

func (m modelChoice) empty() bool { return m == modelChoice{} }

// cmdInit writes a template bundle into a new directory.
func (e *env) cmdInit(_ context.Context, args []string) error {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	fset.SetOutput(e.stderr)
	names := strings.Join(templates.Names(), ", ")
	name := fset.String("template", "hello", "template to start from: "+names)
	example := fset.String("example", "", "same as --template")
	list := fset.Bool("list", false, "list the templates")
	asJSON := fset.Bool("json", false, "with --list: print the templates as JSON")
	var mc modelChoice
	fset.StringVar(&mc.model, "model", "", "OpenRouter model instead of the template's (a routing model such as typesafe/jev-router picks the model per request)")
	fset.StringVar(&mc.tier, "tier", "", "default cost tier for a routing model: low, medium, high (xhigh, max for openrouter/auto)")
	fset.StringVar(&mc.maxTier, "max-tier", "", "highest cost tier a task may ask for (routing models)")
	fset.StringVar(&mc.fallback, "fallback", "", `comma-separated models to try when the model keeps failing ("none" clears them)`)
	pos, err := parse(fset, args)
	if err != nil {
		return err
	}
	if *list {
		all, err := templates.List()
		if err != nil {
			return err
		}
		if *asJSON {
			type entry struct {
				templates.Template
				Tools   []string `json:"tools,omitempty"`
				Skills  []string `json:"skills,omitempty"`
				Secrets []string `json:"secrets,omitempty"`
			}
			out := struct {
				Templates []entry `json:"templates"`
			}{}
			for _, t := range all {
				out.Templates = append(out.Templates, entry{t, t.Tools, t.Skills, t.Secrets})
			}
			enc := json.NewEncoder(e.stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		w := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tCATEGORY\tTOOLS\tSKILLS\tSECRETS\tDESCRIPTION")
		for _, t := range all {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, t.Category, orDash(t.Tools), orDash(t.Skills), orDash(t.Secrets), t.Description)
		}
		return w.Flush()
	}
	if len(pos) > 1 {
		return usageErr("agen init [dir] [--template NAME] [--model M] [--tier T] [--max-tier T] [--fallback M,...] | agen init --list [--json]")
	}
	if *example != "" {
		*name = *example
	}
	src, err := templates.FS(*name)
	if err != nil {
		return fmt.Errorf("no template %q; templates: %s", *name, names)
	}
	files := map[string][]byte{}
	err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files[p], err = fs.ReadFile(src, p)
		return err
	})
	if err != nil {
		return err
	}
	if !mc.empty() {
		if err := applyModel(files, mc); err != nil {
			return err
		}
		if _, err := bundle.Parse(files); err != nil {
			return err
		}
	}
	dir := *name
	if len(pos) == 1 {
		dir = pos[0]
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s already exists and is not empty", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for p, b := range files {
		target := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.stdout, "wrote the %s template to %s\nnext: agen deploy %s --replicas 1\n", *name, dir, dir)
	return nil
}

// applyModel rewrites the template's harness.json for the chosen model. A
// template with scripted replies becomes an OpenRouter one, reading
// OPENROUTER_API_KEY from the environment.
func applyModel(files map[string][]byte, mc modelChoice) error {
	var h map[string]any
	if err := json.Unmarshal(files["x-agen/harness.json"], &h); err != nil {
		return fmt.Errorf("template harness.json: %w", err)
	}
	if mc.model != "" && mc.model != h["model"] {
		if h["provider"] != "openrouter" {
			h["provider"] = "openrouter"
			if script, ok := h["script"].(string); ok {
				delete(files, script)
			}
			delete(h, "script")
			var secrets map[string]any
			if b, ok := files["x-agen/secrets.json"]; ok {
				if err := json.Unmarshal(b, &secrets); err != nil {
					return fmt.Errorf("template secrets.json: %w", err)
				}
			}
			if secrets == nil {
				secrets = map[string]any{}
			}
			if _, ok := secrets["OPENROUTER_API_KEY"]; !ok {
				secrets["OPENROUTER_API_KEY"] = map[string]any{"source": "env"}
			}
			b, err := json.MarshalIndent(secrets, "", "  ")
			if err != nil {
				return err
			}
			files["x-agen/secrets.json"] = append(b, '\n')
		}
		h["model"] = mc.model
		// Router settings belong to the template's routing model.
		delete(h, "router")
	}
	if mc.tier != "" || mc.maxTier != "" {
		router, _ := h["router"].(map[string]any)
		if router == nil {
			router = map[string]any{}
		}
		if mc.tier != "" {
			router["tier"] = mc.tier
		}
		if mc.maxTier != "" {
			router["maxTier"] = mc.maxTier
		}
		h["router"] = router
	}
	switch mc.fallback {
	case "":
		// A fallback that is now the primary model would only repeat it.
		if fb, ok := h["fallbackModels"].([]any); ok {
			keep := []any{}
			for _, m := range fb {
				if m != h["model"] {
					keep = append(keep, m)
				}
			}
			h["fallbackModels"] = keep
			if len(keep) == 0 {
				delete(h, "fallbackModels")
			}
		}
	case "none":
		delete(h, "fallbackModels")
	default:
		var fb []any
		for _, m := range strings.Split(mc.fallback, ",") {
			if m = strings.TrimSpace(m); m != "" {
				fb = append(fb, m)
			}
		}
		h["fallbackModels"] = fb
	}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	files["x-agen/harness.json"] = append(b, '\n')
	return nil
}

func orDash(list []string) string {
	if len(list) == 0 {
		return "-"
	}
	return strings.Join(list, ",")
}
