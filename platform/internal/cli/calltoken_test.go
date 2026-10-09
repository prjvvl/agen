package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/prjvvl/agen/platform/internal/calltoken"
	"github.com/prjvvl/agen/platform/internal/store"
)

// signCallToken signs a call token with the Hub key of the store in home
// (what the Hub would issue to that caller).
func signCallToken(t *testing.T, home, sub, aud string) string {
	t.Helper()
	st, err := store.Open(context.Background(), defaultStoreURL(home))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seed64, err := st.EnsureTokenKey(context.Background(), func() (string, error) { t.Fatal("no hub token key yet"); return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := base64.StdEncoding.DecodeString(seed64)
	tok, _ := calltoken.Sign(ed25519.NewKeyFromSeed(seed), calltoken.Claims{Sub: sub, Aud: aud}, time.Minute, time.Now())
	return tok
}
