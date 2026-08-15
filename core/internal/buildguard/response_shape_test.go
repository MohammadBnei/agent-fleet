package buildguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestResponseMessagesCarryInformation guards the response-shape convention
// settled in docs/adr/0048:
//
//	An RPC returns nothing, or the one thing the caller cannot already know.
//
// Concretely it forbids the one shape that regressed twice into a mess: a
// `*Response` message whose entire body is `string status = 1`, holding a
// constant the server always sets.
//
// Why this needs a test rather than a convention:
//
// Adding `string status = 1` to a new response breaks nothing. It compiles,
// it lints, it passes every existing test, and it looks exactly like the
// response next to it. By the time anyone notices, half the responses say
// "ok" and half say `{}`, and there is no way to tell which an unfamiliar
// RPC uses without reading it. That is the state docs/adr/0048 found: 10
// ceremonial responses against 7 already-empty ones, chosen at random.
//
// The deeper reason the ceremony is wrong, and why it is worth enforcing
// rather than tidying once: a status inside a SUCCESSFUL response duplicates
// the signal Connect's error channel already carries. Two channels means
// every caller must check both — and eventually one caller checks only the
// wrong one. AskUserQuestion's `status: "answered"|"pending"` was exactly
// that, and a reviewer found a live bug sitting on it.
//
// Deliberately narrow. It does NOT ban `status` fields outright, because
// some are real: GetE2eStatusResponse.status reports live pod state, and
// StartE2eResponse.status distinguishes "requested" from "running". Those
// carry information a caller cannot compute. The rule only catches a
// response whose ENTIRE content is an acknowledgement — which an empty
// message already expresses, for free, with no second channel.
func TestResponseMessagesCarryInformation(t *testing.T) {
	protoDir := filepath.Join(repoRoot(t), "proto", "agentfleet", "v1")
	entries, err := os.ReadDir(protoDir)
	if err != nil {
		t.Fatalf("read proto dir %s: %v — this guard is worthless if it cannot find the files", protoDir, err)
	}

	var checked int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".proto") {
			continue
		}
		path := filepath.Join(protoDir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, msg := range parseMessages(string(src)) {
			if !strings.HasSuffix(msg.name, "Response") {
				continue
			}
			checked++
			if isCeremonialAck(msg.fields) {
				t.Errorf("%s: %s is an acknowledgement wearing a status field.\n"+
					"    Make it `message %s {}` — err == nil already says it worked.\n"+
					"    See docs/adr/0048: an RPC returns nothing, or the one thing\n"+
					"    the caller cannot already know.",
					e.Name(), msg.name, msg.name)
			}
		}
	}

	// A guard that silently checks nothing passes forever. If the parse
	// breaks or the protos move, fail loudly rather than report success.
	if checked < 20 {
		t.Fatalf("only found %d Response messages — the parser or the proto layout changed, "+
			"and this guard is no longer guarding anything", checked)
	}
}

type protoMessage struct {
	name   string
	fields []string
}

var (
	messageStart = regexp.MustCompile(`^message\s+(\w+)\s*\{`)
	// Matches a field line: optional label, type, name, = N;
	fieldLine = regexp.MustCompile(`^\s*(?:optional\s+|repeated\s+)?([\w.]+)\s+(\w+)\s*=\s*\d+\s*;`)
)

// parseMessages is a deliberately dumb top-level scanner: it only needs to
// see message names and their immediate field lines, and every message in
// this schema is written flat. It skips comments, blank lines and `reserved`
// declarations, and does not attempt nested messages or oneofs — a message
// containing either is, by definition, not the empty-ack shape being
// guarded against.
func parseMessages(src string) []protoMessage {
	var out []protoMessage
	var cur *protoMessage
	depth := 0

	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)

		if cur == nil {
			if m := messageStart.FindStringSubmatch(trimmed); m != nil {
				cur = &protoMessage{name: m[1]}
				depth = 1
				// A single-line `message Foo {}` closes immediately.
				if strings.HasSuffix(trimmed, "{}") {
					out = append(out, *cur)
					cur = nil
					depth = 0
				}
			}
			continue
		}

		depth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")
		if depth <= 0 {
			out = append(out, *cur)
			cur = nil
			depth = 0
			continue
		}

		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "reserved") {
			continue
		}
		if m := fieldLine.FindStringSubmatch(trimmed); m != nil {
			cur.fields = append(cur.fields, m[1]+" "+m[2])
		}
	}
	return out
}

// isCeremonialAck reports whether a message's entire content is a single
// string field named `status` — an acknowledgement dressed as data.
func isCeremonialAck(fields []string) bool {
	return len(fields) == 1 && fields[0] == "string status"
}
