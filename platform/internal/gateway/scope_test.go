package gateway

import "testing"

func TestA2ATaskIDsAreScopedToTheDeployment(t *testing.T) {
	a := a2aTaskID("default", "writer", "", "m1")
	if a != a2aTaskID("default", "writer", "", "m1") {
		t.Fatal("not stable")
	}
	for _, other := range []string{a2aTaskID("default", "boss", "", "m1"), a2aTaskID("team", "writer", "", "m1"), a2aTaskID("default", "writer", "", "m2"),
		a2aTaskID("default", "writer", "user:token:bob", "m1")} {
		if other == a {
			t.Fatalf("collision: %s", a)
		}
	}
	p := taskParams{Metadata: map[string]any{"messageId": "m1"}}
	if p.taskID("default", "writer", caller{}) != a {
		t.Fatal("cancel by messageId must name the same task")
	}
}
