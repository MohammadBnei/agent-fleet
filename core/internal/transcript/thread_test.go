package transcript

import (
	"context"
	"testing"
)

func TestValidThread(t *testing.T) {
	for k, want := range map[string]bool{"main": true, "a": true, "topic-1": true, "0": true, "": false, "A": false, "a b": false, "-a": false, "a_b": false} {
		if ValidThread(k) != want {
			t.Errorf("ValidThread(%q) = %v", k, !want)
		}
	}
}

func TestThreadFrom_DefaultsToMain(t *testing.T) {
	if got := ThreadFrom(context.Background()); got != MainThread {
		t.Fatalf("no thread named = %q", got)
	}
	if got := ThreadFrom(WithThread(context.Background(), "x")); got != "x" {
		t.Fatalf("named = %q", got)
	}
	if got := ThreadFrom(WithThread(context.Background(), "")); got != MainThread {
		t.Fatalf("an empty name = %q", got)
	}
}
