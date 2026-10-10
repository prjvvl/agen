package cli

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// The example bundles of the repository (examples/bundles, copied in by
// scripts/gen.sh), so a release install can start without a clone.
//
//go:embed examples
var exampleBundles embed.FS

func exampleNames() []string {
	entries, _ := exampleBundles.ReadDir("examples")
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// cmdInit writes an example bundle into a new directory.
func (e *env) cmdInit(_ context.Context, args []string) error {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	fset.SetOutput(e.stderr)
	example := fset.String("example", "hello", "example to start from: "+strings.Join(exampleNames(), ", "))
	pos, err := parse(fset, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("agen init [dir] [--example NAME]")
	}
	src, err := fs.Sub(exampleBundles, path.Join("examples", *example))
	if err != nil || !isDir(src) {
		return fmt.Errorf("no example %q; examples: %s", *example, strings.Join(exampleNames(), ", "))
	}
	dir := *example
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
	fmt.Fprintf(e.stdout, "wrote the %s example to %s\nnext: agen deploy %s --replicas 1\n", *example, dir, dir)
	return nil
}

func isDir(f fs.FS) bool {
	st, err := fs.Stat(f, ".")
	return err == nil && st.IsDir()
}
