package version

import "testing"

func TestString(t *testing.T) {
	if got := String("agen"); got != "agen "+Version {
		t.Fatalf("got %q", got)
	}
}
