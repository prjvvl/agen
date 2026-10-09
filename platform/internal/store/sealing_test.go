package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The Hub's keys are sealed in the Store with AGEN_HUB_KEK: the Store
// alone does not reveal them, existing plaintext keys are sealed in place,
// and a Hub without (or with another) KEK fails clearly.
func TestHubKeysSealedWithKEK(t *testing.T) {
	ctx := context.Background()
	eachIsolated(t, func(t *testing.T, s *Store) {
		for _, q := range []string{"DELETE FROM hub_token_key", "DELETE FROM hub_ca"} {
			must(t, execErr(s.db.ExecContext(ctx, q)))
		}
		t.Cleanup(func() {
			s.SetKEK("")
			s.db.ExecContext(ctx, "DELETE FROM hub_token_key")
			s.db.ExecContext(ctx, "DELETE FROM hub_ca")
		})
		raw := func(q string) string {
			var v string
			must(t, s.db.QueryRowContext(ctx, q).Scan(&v))
			return v
		}

		// New key with a KEK: sealed at rest, plain to the Hub.
		s.SetKEK("kek-one")
		key, err := s.EnsureTokenKey(ctx, func() (string, error) { return "SEED-VALUE", nil })
		if err != nil || key != "SEED-VALUE" {
			t.Fatalf("token key: %q %v", key, err)
		}
		if v := raw("SELECT private_key FROM hub_token_key"); !strings.HasPrefix(v, "enc1:") || strings.Contains(v, "SEED") {
			t.Fatalf("token key stored as %q", v)
		}
		s.SetKEK("")
		if _, err := s.EnsureTokenKey(ctx, nil); !errors.Is(err, ErrNoKEK) {
			t.Fatalf("no KEK: %v", err)
		}
		s.SetKEK("kek-two")
		if _, err := s.EnsureTokenKey(ctx, nil); err == nil || !strings.Contains(err.Error(), "AGEN_HUB_KEK differs") {
			t.Fatalf("wrong KEK: %v", err)
		}

		// A CA created before any KEK is sealed in place once one is set.
		s.SetKEK("")
		ca, err := s.EnsureCA(ctx, func() (CA, error) { return CA{CertPEM: "CERT", KeyPEM: "PLAIN-CA-KEY"}, nil })
		if err != nil || ca.KeyPEM != "PLAIN-CA-KEY" || raw("SELECT key_pem FROM hub_ca") != "PLAIN-CA-KEY" {
			t.Fatalf("plain CA: %+v %v", ca, err)
		}
		s.SetKEK("kek-one")
		ca, err = s.EnsureCA(ctx, nil)
		if err != nil || ca.KeyPEM != "PLAIN-CA-KEY" || ca.CertPEM != "CERT" {
			t.Fatalf("CA after sealing: %+v %v", ca, err)
		}
		if v := raw("SELECT key_pem FROM hub_ca"); !strings.HasPrefix(v, "enc1:") {
			t.Fatalf("CA key not sealed in place: %q", v)
		}
	})
}

func execErr(_ any, err error) error { return err }

// Platform secrets are sealed like the Hub's keys, on both dialects.
func TestPlatformSecretsSealed(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		ns := uniq("ns")
		t.Cleanup(func() { s.SetKEK("") })
		s.SetKEK("kek-one")
		must(t, s.SetPlatformSecret(ctx, ns, "API_KEY", "sk-sealed-value"))
		var raw string
		must(t, s.db.QueryRowContext(ctx, "SELECT value FROM platform_secrets WHERE namespace = $1", ns).Scan(&raw))
		if !strings.HasPrefix(raw, "enc1:") || strings.Contains(raw, "sk-sealed") {
			t.Fatalf("stored as %q", raw)
		}
		if v, err := s.GetPlatformSecret(ctx, ns, "API_KEY"); err != nil || v != "sk-sealed-value" {
			t.Fatalf("get: %q %v", v, err)
		}
		s.SetKEK("")
		if _, err := s.GetPlatformSecret(ctx, ns, "API_KEY"); !errors.Is(err, ErrNoKEK) {
			t.Fatalf("no KEK: %v", err)
		}
		list, err := s.ListPlatformSecrets(ctx, ns)
		if err != nil || len(list) != 1 || list[0].Name != "API_KEY" {
			t.Fatalf("list: %v %v", list, err)
		}
		must(t, s.DeletePlatformSecret(ctx, ns, "API_KEY"))
		if _, err := s.GetPlatformSecret(ctx, ns, "API_KEY"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted: %v", err)
		}
	})
}
