package sessions

import "testing"

func TestEffectKey_RoundTripsAndRefusesOthers(t *testing.T) {
	key := EffectKey("sess-1", "topic-a", 42, 3)
	if key != "eff:sess-1:topic-a:42:3" {
		t.Fatalf("key = %q", key)
	}
	if th, seq, ok := ParseEffectKey("sess-1", key); !ok || th != "topic-a" || seq != 42 {
		t.Fatalf("parse = %q %d %v", th, seq, ok)
	}
	for _, bad := range []string{"", "plain", "eff:sess-2:topic-a:42:3", "eff:sess-1:topic-a:42", "eff:sess-1:topic-a:x:3", "eff:sess-1:a:1:2:3"} {
		if _, _, ok := ParseEffectKey("sess-1", bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
