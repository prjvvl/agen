package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/prjvvl/agen/platform/internal/templates"
)

// cmdInit writes a template bundle into a new directory.
func (e *env) cmdInit(_ context.Context, args []string) error {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	fset.SetOutput(e.stderr)
	names := strings.Join(templates.Names(), ", ")
	name := fset.String("template", "hello", "template to start from: "+names)
	example := fset.String("example", "", "same as --template")
	list := fset.Bool("list", false, "list the templates")
	pos, err := parse(fset, args)
	if err != nil {
		return err
	}
	if *list {
		all, err := templates.List()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tCATEGORY\tTOOLS\tSKILLS\tSECRETS\tDESCRIPTION")
		for _, t := range all {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, t.Category, orDash(t.Tools), orDash(t.Skills), orDash(t.Secrets), t.Description)
		}
		return w.Flush()
	}
	if len(pos) > 1 {
		return usageErr("agen init [dir] [--template NAME] | agen init --list")
	}
	if *example != "" {
		*name = *example
	}
	src, err := templates.FS(*name)
	if err != nil {
		return fmt.Errorf("no template %q; templates: %s", *name, names)
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
	err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "wrote the %s template to %s\nnext: agen deploy %s --replicas 1\n", *name, dir, dir)
	return nil
}

func orDash(list []string) string {
	if len(list) == 0 {
		return "-"
	}
	return strings.Join(list, ",")
}
