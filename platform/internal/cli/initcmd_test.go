package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readHarness(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "x-agen", "harness.json"))
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestInitWithRoutingModel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a")
	agen(t, 0, "init", dir, "--template", "hello", "--model", "typesafe/jev-router", "--tier", "low", "--max-tier", "high", "--fallback", "deepseek/deepseek-v4-flash")
	h := readHarness(t, dir)
	if h["provider"] != "openrouter" || h["model"] != "typesafe/jev-router" || h["script"] != nil {
		t.Fatalf("harness = %v", h)
	}
	r := h["router"].(map[string]any)
	if r["tier"] != "low" || r["maxTier"] != "high" {
		t.Fatalf("router = %v", r)
	}
	if fb := h["fallbackModels"].([]any); len(fb) != 1 || fb[0] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("fallbackModels = %v", fb)
	}
	if _, err := os.Stat(filepath.Join(dir, "x-agen", "fake-script.json")); !os.IsNotExist(err) {
		t.Fatalf("scripted replies left behind: %v", err)
	}
	s, _ := os.ReadFile(filepath.Join(dir, "x-agen", "secrets.json"))
	if !strings.Contains(string(s), "OPENROUTER_API_KEY") {
		t.Fatalf("secrets.json = %s", s)
	}
}

func TestInitTierOnTemplateRouter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	agen(t, 0, "init", dir, "--template", "coder", "--tier", "low")
	r := readHarness(t, dir)["router"].(map[string]any)
	if r["tier"] != "low" || r["maxTier"] != "high" {
		t.Fatalf("router = %v", r)
	}
}

func TestInitRefusesBadModelChoice(t *testing.T) {
	for name, args := range map[string][]string{
		"tier on a plain model": {"--template", "writer", "--tier", "low"},
		"tier jev lacks":        {"--template", "hello", "--model", "typesafe/jev-router", "--tier", "max"},
		"fallback repeats":      {"--template", "writer", "--fallback", "deepseek/deepseek-v4-flash"},
	} {
		dir := filepath.Join(t.TempDir(), "x")
		agen(t, 1, append([]string{"init", dir}, args...)...)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s: wrote %s anyway", name, dir)
		}
	}
}

func TestInitModelSwapDropsTemplateRouter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	agen(t, 0, "init", dir, "--template", "coder", "--model", "deepseek/deepseek-v4-flash")
	h := readHarness(t, dir)
	if h["router"] != nil || h["fallbackModels"] != nil {
		t.Fatalf("harness = %v", h)
	}
}
