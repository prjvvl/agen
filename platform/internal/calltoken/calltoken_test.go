package calltoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]ed25519.PublicKey{KeyID(pub): pub}
	now := time.Unix(1_800_000_000, 0)
	tok, exp := Sign(priv, Claims{Sub: "agent:default/boss", Inst: "i1", Aud: "default/writer"}, time.Hour, now)
	if exp != now.Add(time.Hour) {
		t.Fatal(exp)
	}
	c, err := Verify(tok, keys, "default/writer", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ns, dep, ok := c.Agent(); !ok || ns != "default" || dep != "boss" || c.Inst != "i1" {
		t.Fatalf("%+v", c)
	}
	if _, _, ok := (Claims{Sub: "user:tok1"}).Agent(); ok {
		t.Fatal("user treated as agent")
	}

	// Wrong audience, expired, unknown key, tampered payload, garbage.
	if _, err := Verify(tok, keys, "default/other", now); !errors.Is(err, ErrAudience) {
		t.Fatal(err)
	}
	if _, err := Verify(tok, keys, "default/writer", now.Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(tok, map[string]ed25519.PublicKey{KeyID(other): other}, "default/writer", now); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	forged, _ := Sign(priv, Claims{Sub: "agent:default/admin", Aud: "default/writer"}, time.Hour, now)
	tampered := parts[0] + "." + strings.Split(forged, ".")[1] + "." + parts[2]
	if _, err := Verify(tampered, keys, "default/writer", now); !errors.Is(err, ErrSignature) {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "x", "agen1.a.b", "agen2." + parts[1] + "." + parts[2]} {
		if _, err := Verify(bad, keys, "default/writer", now); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
