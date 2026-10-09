package manager

import (
	"strings"
	"testing"
)

// Hosts do not inherit the Manager's own AGEN_* settings (join/admin/nest
// tokens, store URL); everything else passes through.
func TestHostEnvironDropsManagerSettings(t *testing.T) {
	t.Setenv("AGEN_JOIN_TOKEN", "join_secret")
	t.Setenv("AGEN_TOKEN", "agen_secret")
	t.Setenv("agen_store", "sqlite:x")
	t.Setenv("DEMO_PROVIDER_KEY", "passes")
	env := strings.Join(hostEnviron(), "\n")
	for _, bad := range []string{"join_secret", "agen_secret", "sqlite:x"} {
		if strings.Contains(env, bad) {
			t.Errorf("host environment leaks %q", bad)
		}
	}
	if !strings.Contains(env, "DEMO_PROVIDER_KEY=passes") {
		t.Error("ordinary variables must pass through")
	}
}
