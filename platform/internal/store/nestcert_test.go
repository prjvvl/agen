package store

import (
	"context"
	"errors"
	"testing"
)

// Certificate rotation is a compare-and-swap on the presented
// certificate; the previous one works until the new one is used (a lost
// renewal reply is retried with it); revoked Nests can neither renew nor
// authenticate.
func TestNestCertRotation(t *testing.T) {
	ctx := context.Background()
	each(t, func(t *testing.T, s *Store) {
		jt, err := s.CreateJoinToken(ctx, 60_000)
		must(t, err)
		n, _, err := s.EnrollNest(ctx, jt, Nest{Name: uniq("rot"), Backend: "native"}, func(string) (string, error) { return "fp-A-" + NewID(), nil })
		must(t, err)
		a := n.CertFingerprint
		b, c, d, e := "fp-B-"+NewID(), "fp-C-"+NewID(), "fp-D-"+NewID(), "fp-E-"+NewID()
		by := func(fp string) error { _, err := s.NestByCert(ctx, fp); return err }

		must(t, s.RotateNestCert(ctx, n.ID, a, b))
		must(t, by(a)) // previous: still valid until B is used
		must(t, by(b)) // first use of B retires A
		if err := by(a); !errors.Is(err, ErrNotFound) {
			t.Fatalf("previous certificate after the new one was used: %v", err)
		}
		if err := s.RotateNestCert(ctx, n.ID, a, c); !errors.Is(err, ErrConflict) {
			t.Fatalf("renewal with a retired certificate: %v", err)
		}

		// Lost reply: C is issued but never reaches the Nest, which retries
		// with B (now the previous certificate).
		must(t, s.RotateNestCert(ctx, n.ID, b, c))
		must(t, by(b))
		must(t, s.RotateNestCert(ctx, n.ID, b, d))
		must(t, by(d))

		// Revoked: no renewal, no authentication.
		must(t, s.RevokeNest(ctx, n.ID))
		if err := s.RotateNestCert(ctx, n.ID, d, e); !errors.Is(err, ErrConflict) {
			t.Fatalf("revoked nest renewed: %v", err)
		}
		for _, fp := range []string{b, d, e} {
			if err := by(fp); !errors.Is(err, ErrNotFound) {
				t.Fatalf("revoked nest authenticated with %s: %v", fp, err)
			}
		}
	})
}
