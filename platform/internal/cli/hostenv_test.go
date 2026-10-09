package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestReadHostEnvDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "OPENROUTER_API_KEY"), []byte("sk-x\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "OTHER"), []byte("v"), 0o600)
	os.Mkdir(filepath.Join(dir, "..2026_09_28"), 0o700) // mounted-secret internals
	os.WriteFile(filepath.Join(dir, "..data"), []byte("x"), 0o600)
	got, err := readHostEnvDir(dir)
	sort.Strings(got)
	if err != nil || !reflect.DeepEqual(got, []string{"OPENROUTER_API_KEY=sk-x", "OTHER=v"}) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := readHostEnvDir(filepath.Join(dir, "missing")); err != nil || got != nil {
		t.Fatalf("missing dir: %v %v", got, err)
	}
	os.WriteFile(filepath.Join(dir, "AGEN_STORE"), []byte("x"), 0o600)
	if _, err := readHostEnvDir(dir); err == nil || !strings.Contains(err.Error(), "AGEN_") {
		t.Fatalf("AGEN_ variable accepted: %v", err)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]uint64{"1048576": 1 << 20, "512MiB": 512 << 20, "2GiB": 2 << 30, "1G": 1e9, "64K": 64000} {
		if got, err := parseBytes(in); err != nil || got != want {
			t.Errorf("parseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-1GiB", "1TiB"} {
		if _, err := parseBytes(bad); err == nil {
			t.Errorf("parseBytes(%q) accepted", bad)
		}
	}
}
