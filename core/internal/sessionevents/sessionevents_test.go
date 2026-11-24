package sessionevents

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func sample() session {
	arch := at.Add(time.Hour)
	return session{id: "s1", repo: "acme/web", title: "fix login", definition: "ops-lead", lifecycle: "standing",
		threadKey: "main", createdAt: at, archivedAt: &arch}
}

func TestNoTextInData(t *testing.T) {
	// A result body full of text, paths and a prompt: only two numbers may leave.
	body := `{"durationMs":1200,"isError":false,"result":"SECRET reply","prompt":"SECRET prompt","cwd":"/home/x/SECRET","modelUsage":{"m":{}}}`
	s := sample()
	for _, ev := range []interface{ GetData() []byte }{openedEvent(s), turnEvent(s, turn{seq: 7, threadKey: "main", at: at, text: body}), endedEvent(s)} {
		if strings.Contains(string(ev.GetData()), "SECRET") || strings.Contains(string(ev.GetData()), "/home") {
			t.Fatalf("text leaked: %s", ev.GetData())
		}
	}
	var d map[string]any
	_ = json.Unmarshal(turnEvent(s, turn{seq: 7, threadKey: "main", at: at, text: body}).Data, &d)
	allowed := map[string]bool{"session_id": true, "kind": true, "repo": true, "definition": true, "title": true, "thread_key": true,
		"seq": true, "duration_ms": true, "is_error": true, "occurred_at": true}
	for k := range d {
		if !allowed[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
	if d["duration_ms"] != float64(1200) || d["kind"] != "character" {
		t.Errorf("data %v", d)
	}
}

func TestCeIDStable(t *testing.T) {
	s := sample()
	tr := turn{seq: 7, at: at, text: `{}`}
	if a, b := turnEvent(s, tr).Id, turnEvent(s, tr).Id; a != b || a != "session/s1/turn/7" {
		t.Errorf("turn id %q %q", a, b)
	}
	if a, b := openedEvent(s).Id, openedEvent(s).Id; a != b || a != "session/s1/opened" {
		t.Errorf("opened id %q %q", a, b)
	}
	want := "session/s1/ended/" + itoa(s.archivedAt.Unix())
	if a, b := endedEvent(s).Id, endedEvent(s).Id; a != b || a != want {
		t.Errorf("ended id %q %q want %q", a, b, want)
	}
	if openedEvent(s).Subject != "fleet/session/s1" || endedEvent(s).Type != "dev.bnei.fleet.session.ended" {
		t.Error("subject or type")
	}
}

func TestKind(t *testing.T) {
	s := session{}
	if kind(s) != "repo" {
		t.Error("repo")
	}
	s.definition = "d"
	if kind(s) != "definition" {
		t.Error("definition")
	}
	s.lifecycle = "standing"
	if kind(s) != "character" {
		t.Error("character")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
