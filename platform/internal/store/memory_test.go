package store

import (
	"context"
	"testing"
)

func TestMemoryBlockSetAndList(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("mb")

		// Never written: not found.
		_, err := s.GetMemoryBlock(ctx, ns, "notes")
		if err != ErrNotFound {
			t.Fatalf("unwritten block: %v", err)
		}

		b, err := s.SetMemoryBlock(ctx, MemoryBlock{Namespace: ns, ID: "notes", Label: "Notes", Description: "scratch", Value: "hello"})
		must(t, err)
		if b.Value != "hello" || b.Version != 1 {
			t.Fatalf("set: %+v", b)
		}

		// A second SetMemoryBlock replaces the value outright and bumps the version.
		b2, err := s.SetMemoryBlock(ctx, MemoryBlock{Namespace: ns, ID: "notes", Label: "Notes v2", Value: "world"})
		must(t, err)
		if b2.Value != "world" || b2.Version != 2 || b2.Label != "Notes v2" {
			t.Fatalf("replace: %+v", b2)
		}

		got, err := s.GetMemoryBlock(ctx, ns, "notes")
		must(t, err)
		if got.Value != "world" {
			t.Fatalf("get: %+v", got)
		}

		_, err = s.SetMemoryBlock(ctx, MemoryBlock{Namespace: ns, ID: "other", Value: "x"})
		must(t, err)

		all, err := s.ListMemoryBlocks(ctx, ns)
		must(t, err)
		if len(all) != 2 {
			t.Fatalf("list: %d", len(all))
		}
		ids := map[string]bool{}
		for _, b := range all {
			ids[b.ID] = true
		}
		if !ids["notes"] || !ids["other"] {
			t.Fatalf("list: %+v", all)
		}

		otherNs, err := s.ListMemoryBlocks(ctx, uniq("none"))
		must(t, err)
		if len(otherNs) != 0 {
			t.Fatalf("a different namespace sees nothing: %d", len(otherNs))
		}
	})
}
