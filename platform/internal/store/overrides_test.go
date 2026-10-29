package store

import (
	"context"
	"testing"
)

func TestTaskOverridesJSONRoundTrips(t *testing.T) {
	o := &TaskOverrides{
		Tools:          []string{"read.*"},
		Skills:         []string{"coding"},
		SystemAddendum: "Be terse.",
		Permissions:    []OverrideRule{{Tool: "write.delete", Action: "deny"}},
		Budget:         &RunBudget{MaxTokens: 5000, MaxUsd: 0.1, MaxTurns: 5, MaxDuration: "10m"},
	}
	got, err := ParseOverrides(o.JSON())
	must(t, err)
	if got.SystemAddendum != "Be terse." || got.Budget.MaxTurns != 5 || got.Permissions[0].Action != "deny" {
		t.Fatalf("round trip: %+v", got)
	}
	// Nil and empty both decode to nothing stored / nothing parsed.
	if (*TaskOverrides)(nil).JSON() != "" {
		t.Fatal("nil overrides must store as empty")
	}
	none, err := ParseOverrides("")
	must(t, err)
	if none != nil {
		t.Fatalf("empty string must parse to nil, got %+v", none)
	}
}

func TestCombineOverridesOnlyNarrows(t *testing.T) {
	parent := &TaskOverrides{
		Tools:  []string{"read.*", "write.*"},
		Skills: []string{"coding", "review"},
		Budget: &RunBudget{MaxTokens: 10_000, MaxDuration: "30m"},
	}
	child := &TaskOverrides{
		Tools:  []string{"write.*", "exec.*"},
		Skills: []string{"coding"},
		Budget: &RunBudget{MaxTokens: 2_000, MaxDuration: "1h"},
	}
	c := CombineOverrides(parent, child)
	if len(c.Tools) != 1 || c.Tools[0] != "write.*" {
		t.Fatalf("tools must be the intersection: %v", c.Tools)
	}
	if len(c.Skills) != 1 || c.Skills[0] != "coding" {
		t.Fatalf("skills must be the intersection: %v", c.Skills)
	}
	if c.Budget.MaxTokens != 2_000 {
		t.Fatalf("budget must take the smaller value: %+v", c.Budget)
	}
	if c.Budget.MaxDuration != "30m" {
		t.Fatalf("duration must take the smaller value: %+v", c.Budget)
	}
	// nil on either side just passes the other through.
	if CombineOverrides(nil, child) != child {
		t.Fatal("nil parent: child unchanged")
	}
	if CombineOverrides(parent, nil) != parent {
		t.Fatal("nil child: parent unchanged")
	}
	if CombineOverrides(nil, nil) != nil {
		t.Fatal("both nil: nil")
	}
	// An empty allowlist on one side never widens past the other's.
	narrow := CombineOverrides(&TaskOverrides{Tools: []string{"a"}}, &TaskOverrides{})
	if len(narrow.Tools) != 1 || narrow.Tools[0] != "a" {
		t.Fatalf("an empty override list keeps the parent's: %v", narrow.Tools)
	}
	// Permissions concatenate (parent's rules first), never dropped.
	p := &TaskOverrides{Permissions: []OverrideRule{{Tool: "x", Action: "deny"}}}
	ch := &TaskOverrides{Permissions: []OverrideRule{{Tool: "y", Action: "ask"}}}
	comb := CombineOverrides(p, ch)
	if len(comb.Permissions) != 2 || comb.Permissions[0].Tool != "x" || comb.Permissions[1].Tool != "y" {
		t.Fatalf("permissions must concatenate: %+v", comb.Permissions)
	}
}

// A task's overrides round-trip through SubmitTask/GetTask storage.
func TestSubmitTaskStoresAndCombinesOverrides(t *testing.T) {
	each(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		ns := uniq("ov")
		plain, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x"})
		must(t, err)
		if plain.Overrides != "" {
			t.Fatalf("no overrides given: %q", plain.Overrides)
		}
		withOv, err := s.SubmitTask(ctx, Task{Namespace: ns, Deployment: "d", Input: "x",
			Overrides: (&TaskOverrides{Tools: []string{"read.*"}, Budget: &RunBudget{MaxTokens: 10_000}}).JSON()})
		must(t, err)
		got, err := s.GetTask(ctx, withOv.ID)
		must(t, err)
		parsed, err := ParseOverrides(got.Overrides)
		must(t, err)
		if parsed.Tools[0] != "read.*" || parsed.Budget.MaxTokens != 10_000 {
			t.Fatalf("stored overrides: %+v", parsed)
		}
	})
}
