// Package testutil holds helpers shared by platform tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// RepoRoot is the repository root.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// HostBin returns the agen-host binary: AGEN_HOST_BIN, or the workspace debug
// build (built on demand with cargo). Without cargo the test is skipped,
// unless AGEN_REQUIRE_HOST=1.
func HostBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AGEN_HOST_BIN"); p != "" {
		return p
	}
	name := "agen-host"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(RepoRoot(), "target", "debug", name)
	if _, err := exec.LookPath("cargo"); err != nil {
		if _, statErr := os.Stat(p); statErr == nil {
			return p
		}
		if os.Getenv("AGEN_REQUIRE_HOST") == "1" {
			t.Fatal("AGEN_REQUIRE_HOST=1 but agen-host is not built and cargo is not available")
		}
		t.Skip("agen-host is not built and cargo is not available (set AGEN_HOST_BIN)")
	}
	cmd := exec.Command("cargo", "build", "-q", "-p", "agen-host")
	cmd.Dir = RepoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build agen-host: %v\n%s", err, out)
	}
	return p
}

// BundleFiles reads examples/bundles/<name> as path -> bytes.
func BundleFiles(t *testing.T, name string) map[string][]byte {
	t.Helper()
	root := filepath.Join(RepoRoot(), "examples", "bundles", name)
	files := map[string][]byte{}
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}
