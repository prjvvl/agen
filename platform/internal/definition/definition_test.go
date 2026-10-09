package definition

import "testing"

// Same vector as agen-engine bundle::tests::digest_golden_vector_matches_go.
func TestDigestGoldenVector(t *testing.T) {
	files := map[string][]byte{
		"plugin.json":       []byte("{}\n"),
		"a-b/x":             []byte("1"),
		"a/b":               []byte("2"),
		"skills/s/SKILL.md": []byte("h\u00e9llo"),
	}
	const want = "sha256:0a2a61b2870af2778739855371faf070f54a1497fa8408b338764d6fa24f3bb1"
	if got := Digest(files); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	files[".git/HEAD"] = []byte("ref")
	files["x-agen/agent.md~"] = []byte("backup")
	if got := Digest(files); got != want {
		t.Fatalf("ignored files changed digest: %s", got)
	}
}

func TestIgnored(t *testing.T) {
	for p, want := range map[string]bool{"a/.git/config": true, "x.pyc": true, "skills/git/SKILL.md": false, "plugin.json": false} {
		if Ignored(p) != want {
			t.Errorf("Ignored(%q) != %v", p, want)
		}
	}
}
