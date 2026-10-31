package errkeys

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"testing"

	"connectrpc.com/connect"
)

// TestLocationOf: Connect records the line that called it, trimmed at core/, through wrapping; anything not built by
// Connect, or built with no cause, has none. Its words and cause are TestRewriteWire's.
func TestLocationOf(t *testing.T) {
	_, _, line, _ := runtime.Caller(0)
	keyed := Connect(connect.CodeNotFound, KeyInternal, errors.New("gone")) // line+1
	want := "internal/errkeys/located_test.go:" + strconv.Itoa(line+1)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"errkeys.Connect", keyed, want},
		{"wrapped", fmt.Errorf("stop: %w", keyed), want},
		{"connect.NewError", connect.NewError(connect.CodeNotFound, errors.New("gone")), ""},
		{"plain", errors.New("gone"), ""},
		{"nil", nil, ""},
		{"nil cause", Connect(connect.CodeInternal, KeyInternal, nil), ""},
	}
	for _, c := range cases {
		if got := LocationOf(c.err); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
