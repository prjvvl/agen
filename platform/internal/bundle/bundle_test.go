package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func helloFiles(t *testing.T) map[string][]byte {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "examples", "bundles", "hello")
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestParsesHelloExample(t *testing.T) {
	b, err := Parse(helloFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "hello" || b.Kind != "pool" || b.Scale.Max != 3 || b.Scale.IdleSeconds() != 300 {
		t.Fatalf("%+v", b)
	}
	// Same digest the Rust engine computes (definition package is shared).
	if !strings.HasPrefix(b.Digest, "sha256:") || len(b.Digest) != 71 {
		t.Fatal(b.Digest)
	}
}

func TestReportsAllIssues(t *testing.T) {
	files := helloFiles(t)
	delete(files, "plugin.json")
	files["x-agen/harness.json"] = []byte(`{"provider":"nope","model":"m"}`)
	files["x-agen/secrets.json"] = []byte(`{"KEY":{"source":"env","value":"sk-1"}}`)
	files["x-agen/config.json"] = []byte(`{"kind":"singleton","scale":{"min":0,"max":3}}`)
	files["../escape"] = []byte("x")
	_, err := Parse(files)
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("got %v", err)
	}
	text := be.Error()
	for _, want := range []string{"plugin.json: required file is missing", "x-agen/harness.json: at /provider", "x-agen/secrets.json", "singleton runs at most one instance", "path escapes the bundle"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}
