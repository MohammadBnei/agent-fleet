package grants

import (
	"errors"
	"testing"
)

func TestRender(t *testing.T) {
	cases := []struct {
		name, req, exact, prefix string
		bad                      bool
	}{
		{name: "edit absolute", req: `{"tool":"Edit","input":{"file_path":"/workspace/notes/a.md"}}`, exact: "Edit(notes/a.md)", prefix: "Edit(notes/**)"},
		{name: "write top level", req: `{"tool":"Write","input":{"file_path":"a.md"}}`, exact: "Write(a.md)", prefix: "Write(**)"},
		{name: "edit outside worktree", req: `{"tool":"Edit","input":{"file_path":"/etc/passwd"}}`, bad: true},
		{name: "edit dotdot", req: `{"tool":"Edit","input":{"file_path":"/workspace/a/../../x"}}`, bad: true},
		{name: "edit git dir", req: `{"tool":"Edit","input":{"file_path":"/workspace/.git/hooks/pre-commit"}}`, bad: true},
		{name: "bash two words", req: `{"tool":"Bash","input":{"command":"npm test --watch=false"}}`, exact: "Bash(npm test --watch=false)", prefix: "Bash(npm test:*)"},
		{name: "bash option second", req: `{"tool":"Bash","input":{"command":"ls -la"}}`, exact: "Bash(ls -la)", prefix: "Bash(ls:*)"},
		{name: "bash chained has no prefix", req: `{"tool":"Bash","input":{"command":"make && make install"}}`, exact: "Bash(make && make install)"},
		{name: "bash double space has no prefix", req: `{"tool":"Bash","input":{"command":"npm  test"}}`, exact: "Bash(npm  test)"},
		{name: "bash prefix-looking", req: `{"tool":"Bash","input":{"command":"git push:*"}}`, bad: true},
		{name: "bash multiline", req: `{"tool":"Bash","input":{"command":"a\nb"}}`, bad: true},
		{name: "bare tool", req: `{"tool":"TodoWrite","input":{}}`, exact: "TodoWrite"},
		{name: "outside ceiling", req: `{"tool":"WebFetch","input":{"url":"x"}}`, bad: true},
		{name: "garbage", req: `nope`, bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Render(c.req)
			if c.bad {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("want ErrInvalid, got %+v, %v", r, err)
				}
				return
			}
			if err != nil || r.Exact != c.exact || r.Prefix != c.prefix {
				t.Fatalf("got %+v, %v; want {%s %s}", r, err, c.exact, c.prefix)
			}
		})
	}
}

func TestPick(t *testing.T) {
	r := Rules{Exact: "TodoWrite"}
	if got, err := r.Pick(""); err != nil || got != "TodoWrite" {
		t.Fatalf("default form: %q, %v", got, err)
	}
	if _, err := r.Pick(FormPrefix); !errors.Is(err, ErrInvalid) {
		t.Fatalf("prefix with none offered: %v", err)
	}
	if _, err := r.Pick("wide"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown form: %v", err)
	}
}
