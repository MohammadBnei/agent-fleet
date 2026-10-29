package decisions

import "testing"

func TestCanon(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// Keys sorted at every depth, space dropped.
		{`{ "b": 1, "a": {"d": [3, {"z": 1, "y": 2}], "c": null} }`, `{"a":{"c":null,"d":[3,{"y":2,"z":1}]},"b":1}`},
		// Numbers as written: no float64 rounding, no exponent rewrite.
		{`{"n": 12345678901234567890, "f": 1.50, "e": 1e3}`, `{"e":1e3,"f":1.50,"n":12345678901234567890}`},
		{`"ls -la"`, `"ls -la"`},
		{`  [ ]  `, `[]`},
	} {
		got, err := Canon([]byte(c.in))
		if err != nil || got != c.want {
			t.Errorf("Canon(%s) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	// Two spellings of one input are one canon: what a grant matches on.
	a, _ := Canon([]byte(`{"command":"ls","timeout":5}`))
	b, _ := Canon([]byte("{\n  \"timeout\": 5,\n  \"command\": \"ls\"\n}"))
	if a != b {
		t.Errorf("%q != %q", a, b)
	}
	for _, bad := range []string{``, `{`, `{"a":1} {"b":2}`, `nope`} {
		if got, err := Canon([]byte(bad)); err == nil {
			t.Errorf("Canon(%q) = %q, want an error", bad, got)
		}
	}
}

func TestRead(t *testing.T) {
	for _, c := range []struct {
		typ, text string
		kind      Kind
		ask       string
	}{
		{"question", `{"questions":[{"question":"which?"},{"question":"and?"}]}`, Question, "which?"},
		{"question", `?`, Question, ""},
		{"permission_request", `{"tool":"Bash","input":{"command":"ls"}}`, Permission, "Bash"},
		{"permission_request", `{"tool":"ExitPlanMode","input":{"plan":"do x"}}`, Plan, "do x"},
		{"permission_request", `not json`, Permission, ""},
	} {
		d := read(c.typ, c.text)
		if d == nil || d.Kind() != c.kind || d.Ask() != c.ask {
			t.Errorf("read(%s, %s) = %#v", c.typ, c.text, d)
		}
	}
	for _, typ := range []string{"answer", "permission_response", "discussion", ""} {
		if d := read(typ, `{}`); d != nil {
			t.Errorf("read(%s) = %#v, want no decision", typ, d)
		}
	}
}
