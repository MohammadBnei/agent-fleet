// Package grants is docs/adr/0076 §3: a definition session's allow list
// grows by remembered approvals. Core renders each rule from the parked
// permission_request itself, never from text in the answer, so a rule is as
// wide as the answer card says and no wider.
package grants

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

var ErrInvalid = errors.New("no grant for this request")

const (
	ScopeOnce       = "once"
	ScopeSession    = "session"
	ScopeDefinition = "definition"

	FormExact  = "exact"
	FormPrefix = "prefix"
)

// The worktree every definition pod runs in (provisioner/internal/k8s's
// sessionWorkdir). Edit/Write rules are relative to it, so a definition grant
// reads the same in every session.
const worktree = "/workspace"

// Mirrors core/internal/definitions/load.go's toolCeiling: a grant names only
// these. Anything else is denied by the worker, never asked.
var ceiling = map[string]bool{"Read": true, "Edit": true, "Write": true, "Glob": true, "Grep": true, "Bash": true, "TodoWrite": true}

// Rules is what one parked request can be granted as. Prefix is empty when
// the request offers no wider form.
type Rules struct {
	Exact  string
	Prefix string
}

// Render reads a permission_request entry's text ({"tool", "input"}, as
// worker/src/session.ts posts it).
func Render(requestText string) (Rules, error) {
	var req struct {
		Tool  string         `json:"tool"`
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(requestText), &req); err != nil {
		return Rules{}, fmt.Errorf("%w: unreadable request", ErrInvalid)
	}
	if !ceiling[req.Tool] {
		return Rules{}, fmt.Errorf("%w: %s is outside the tool ceiling", ErrInvalid, req.Tool)
	}
	switch req.Tool {
	case "Edit", "Write":
		p, ok := req.Input["file_path"].(string)
		if !ok {
			return Rules{}, fmt.Errorf("%w: no file_path", ErrInvalid)
		}
		rel, err := worktreeRelative(p)
		if err != nil {
			return Rules{}, err
		}
		r := Rules{Exact: req.Tool + "(" + rel + ")"}
		// A top-level file's directory is the whole worktree: no prefix form.
		if dir := path.Dir(rel); dir != "." {
			r.Prefix = req.Tool + "(" + dir + "/**)"
		}
		return r, nil
	case "Bash":
		cmd, ok := req.Input["command"].(string)
		if !ok || cmd == "" {
			return Rules{}, fmt.Errorf("%w: no command", ErrInvalid)
		}
		// One line, and never something the worker would read as a prefix rule.
		if strings.ContainsAny(cmd, "\n\r") || strings.HasSuffix(cmd, ":*") {
			return Rules{}, fmt.Errorf("%w: command cannot be a rule", ErrInvalid)
		}
		return Rules{Exact: "Bash(" + cmd + ")", Prefix: bashPrefix(cmd)}, nil
	}
	return Rules{Exact: req.Tool}, nil
}

// Pick is the rule for one answer's rule_form.
func (r Rules) Pick(form string) (string, error) {
	switch form {
	case FormExact, "":
		return r.Exact, nil
	case FormPrefix:
		if r.Prefix == "" {
			return "", fmt.Errorf("%w: no prefix form", ErrInvalid)
		}
		return r.Prefix, nil
	}
	return "", fmt.Errorf("%w: unknown rule_form %q", ErrInvalid, form)
}

func worktreeRelative(p string) (string, error) {
	if path.IsAbs(p) {
		rel, ok := strings.CutPrefix(p, worktree+"/")
		if !ok {
			return "", fmt.Errorf("%w: %s is outside the worktree", ErrInvalid, p)
		}
		p = rel
	}
	// No glob characters: the worker reads `/**` as a directory, so a file
	// literally named `**` would turn an exact rule into a wide one.
	if p == "" || strings.ContainsAny(p, "()\n*?[") {
		return "", fmt.Errorf("%w: unusable path", ErrInvalid)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == ".git" || seg == ".claude" {
			return "", fmt.Errorf("%w: %s cannot be granted", ErrInvalid, p)
		}
	}
	return path.Clean(p), nil
}

// bashPrefix is `Bash(<w1> <w2>:*)`, or `Bash(<w1>:*)` when the second word is
// an option or missing, offered only for a plain-words command whose text
// starts with exactly those words.
func bashPrefix(cmd string) string {
	words, ok := plainWords(cmd)
	if !ok || len(words) == 0 {
		return ""
	}
	p := words[0]
	if len(words) > 1 && !strings.HasPrefix(words[1], "-") {
		p += " " + words[1]
	}
	if cmd != p && !strings.HasPrefix(cmd, p+" ") {
		return ""
	}
	return "Bash(" + p + ":*)"
}

// plainWords splits on spaces and refuses anything a shell would quote,
// expand or chain. Stricter than worker/src/session.ts's shellWords, which
// also takes quotes: a prefix rule matches the raw text, so a quoted word
// could not match anyway.
func plainWords(cmd string) ([]string, bool) {
	if strings.ContainsAny(cmd, ";&|<>()$`\\{}*?~\n\t#'\"") {
		return nil, false
	}
	return strings.Fields(cmd), true
}
