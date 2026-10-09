package hub

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"errors"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/calltoken"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Call-token key rotation publishes the new key before signing with it,
// keeps the previous key valid for verification, and keeps two keys.
func TestTokenKeyRotation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	defer func(g, ttl time.Duration) { keyGrace, keyringTTL = g, ttl }(keyGrace, keyringTTL)
	keyringTTL = 0 // re-read the Store on every use

	kid := func(k ed25519.PrivateKey) string { return calltoken.KeyID(k.Public().(ed25519.PublicKey)) }
	a, err := e.hub.tokenKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	signedByA, _ := calltoken.Sign(a, calltoken.Claims{Sub: "user:token:bob", Aud: "default/hello"}, time.Minute, time.Now())
	rotate := func() {
		seed, _ := newTokenSeed()
		if err := e.hub.Store.RotateTokenKey(ctx, seed, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Rotating while the current key is younger than an agent token's
	// lifetime is refused (it could retire a key live tokens rely on).
	if seed, _ := newTokenSeed(); !errors.Is(e.hub.Store.RotateTokenKey(ctx, seed, minRotationAge), store.ErrTooSoon) {
		t.Fatal("rotation of a fresh key was not refused")
	}

	// B is published at once but A still signs during the grace period.
	rotate()
	kr, _ := e.hub.keyring(ctx)
	if len(kr.public) != 2 || kid(kr.signing) != kid(a) {
		t.Fatalf("after rotation: %d keys, signing %s (A %s)", len(kr.public), kid(kr.signing), kid(a))
	}
	_, nest := e.enroll(t, "n", 1, nil)
	got, err := nest.GetTokenKeys(ctx, connect.NewRequest(&agenv1.GetTokenKeysRequest{NestId: firstNestID(t, e)}))
	if err != nil || len(got.Msg.Keys) != 2 {
		t.Fatalf("published keys: %v %v", got, err)
	}

	// After the grace period B signs; A still verifies what it signed.
	keyGrace = 0
	b, _ := e.hub.tokenKey(ctx)
	if kid(b) == kid(a) {
		t.Fatal("new key not used after the grace period")
	}
	if user := e.hub.tokenUser(ctx, signedByA, "default/hello"); user != "token:bob" {
		t.Fatalf("token signed with the previous key: %q", user)
	}

	// Another rotation retires A: no longer published (B and C are), but
	// still able to verify stored requester records it signed.
	rotate()
	kr, _ = e.hub.keyring(ctx)
	if len(kr.public) != 2 {
		t.Fatalf("published keys after second rotation: %d", len(kr.public))
	}
	if _, ok := kr.public[kid(a)]; ok {
		t.Fatal("the oldest key is still published after two rotations")
	}
	if user := e.hub.tokenUser(ctx, signedByA, "default/hello"); user != "token:bob" {
		t.Fatalf("requester record signed by a retired key: %q", user)
	}
}

func firstNestID(t *testing.T, e env) string {
	t.Helper()
	nests, err := e.hub.Store.ListNests(context.Background())
	if err != nil || len(nests) == 0 {
		t.Fatalf("nests: %v %v", nests, err)
	}
	return nests[0].ID
}
