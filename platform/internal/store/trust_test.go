package store

import (
	"context"
	"testing"
)

// A task's trust round-trips through SubmitTask/GetTask storage.
func TestSubmitTaskStoresTrust(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("tr")
		task, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x", Trust: "untrusted"})
		must(t, err)
		got, err := s.GetTask(ctx, task.ID)
		must(t, err)
		if got.Trust != "untrusted" {
			t.Fatalf("trust: %q", got.Trust)
		}
	})
}

// A token's trust round-trips through CreateAPIToken/LookupAPIToken; "" is
// not resolved by the store (the Hub resolves it to "trusted").
func TestAPITokenStoresTrust(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		_, secret, err := s.CreateAPIToken(ctx, uniq("tok"), []string{"operator"}, nil, 0, false, "operator")
		must(t, err)
		got, err := s.LookupAPIToken(ctx, secret)
		must(t, err)
		if got.Trust != "operator" {
			t.Fatalf("token trust: %q", got.Trust)
		}
		_, secret2, err := s.CreateAPIToken(ctx, uniq("tok2"), []string{"operator"}, nil, 0, false, "")
		must(t, err)
		got2, err := s.LookupAPIToken(ctx, secret2)
		must(t, err)
		if got2.Trust != "" {
			t.Fatalf("unset token trust must stay empty in storage: %q", got2.Trust)
		}
	})
}
