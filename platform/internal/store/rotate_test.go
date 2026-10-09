package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Rotating the KEK re-seals every Hub secret under the new key; the old
// key no longer opens them.
func TestRotateKEK(t *testing.T) {
	ctx := context.Background()
	eachIsolated(t, func(t *testing.T, s *Store) {
		t.Cleanup(func() { s.SetKEK("") })
		s.SetKEK("old-kek")
		_, err := s.EnsureCA(ctx, func() (CA, error) { return CA{CertPEM: "CERT", KeyPEM: "CA-KEY"}, nil })
		must(t, err)
		_, err = s.EnsureTokenKey(ctx, func() (string, error) { return "TOKEN-SEED", nil })
		must(t, err)
		must(t, s.SetPlatformSecret(ctx, "ns-rot", "API_KEY", "sk-rotate-me"))
		n, err := s.RotateKEK(ctx, "new-kek")
		if err != nil || n != 3 {
			t.Fatalf("rotate: %d %v", n, err)
		}
		// This Store now uses the new KEK.
		if ca, err := s.EnsureCA(ctx, nil); err != nil || ca.KeyPEM != "CA-KEY" {
			t.Fatalf("CA after rotation: %+v %v", ca, err)
		}
		if v, err := s.GetPlatformSecret(ctx, "ns-rot", "API_KEY"); err != nil || v != "sk-rotate-me" {
			t.Fatalf("secret after rotation: %q %v", v, err)
		}
		// The old KEK no longer opens anything.
		s.SetKEK("old-kek")
		if _, err := s.EnsureTokenKey(ctx, nil); err == nil || !strings.Contains(err.Error(), "differs") {
			t.Fatalf("old KEK after rotation: %v", err)
		}
		s.SetKEK("")
		if _, err := s.GetPlatformSecret(ctx, "ns-rot", "API_KEY"); !errors.Is(err, ErrNoKEK) {
			t.Fatalf("no KEK: %v", err)
		}
		// During a rotation, Hubs that already run the new KEK still open
		// values an old-KEK Hub wrote meanwhile (AGEN_HUB_KEK_PREVIOUS).
		s.SetKEK("old-kek")
		must(t, s.SetPlatformSecret(ctx, "ns-rot", "LATE", "sk-written-by-old-hub", "d"))
		s.SetKEK("new-kek")
		if _, err := s.GetPlatformSecret(ctx, "ns-rot", "LATE"); err == nil {
			t.Fatal("new KEK alone opened an old-KEK value")
		}
		s.SetPreviousKEK("old-kek")
		if v, err := s.GetPlatformSecret(ctx, "ns-rot", "LATE"); err != nil || v != "sk-written-by-old-hub" {
			t.Fatalf("previous-KEK fallback: %q %v", v, err)
		}
		s.SetPreviousKEK("")
	})
}

// Token-key rotation refuses a too-recent key, keeps the previous one
// live and retires (never deletes) older ones.
func TestRotateTokenKeyStore(t *testing.T) {
	ctx := context.Background()
	eachIsolated(t, func(t *testing.T, s *Store) {
		_, err := s.EnsureTokenKey(ctx, func() (string, error) { return "SEED-A", nil })
		must(t, err)
		if err := s.RotateTokenKey(ctx, "SEED-B", time.Hour); !errors.Is(err, ErrTooSoon) {
			t.Fatalf("too-soon rotation: %v", err)
		}
		must(t, s.RotateTokenKey(ctx, "SEED-B", 0))
		must(t, s.RotateTokenKey(ctx, "SEED-C", 0))
		keys, err := s.TokenKeys(ctx)
		must(t, err)
		if len(keys) != 3 || keys[0].RetiredMs == 0 || keys[1].RetiredMs != 0 || keys[2].RetiredMs != 0 {
			t.Fatalf("keys after two rotations: %+v", keys)
		}
		if newest, _ := s.EnsureTokenKey(ctx, nil); newest != "SEED-C" {
			t.Fatalf("newest key: %q", newest)
		}
	})
}
