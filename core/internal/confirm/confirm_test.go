package confirm

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

const fleet = "https://fleet.bnei.dev"

var now = time.Unix(1_800_000_000, 0)

type fake struct {
	calls   int
	err     error
	reason  string
	gotOwn  string
	version int64
}

func setup() (Config, *fake) {
	f := &fake{}
	card := &agentfleetv1.Card{Ask: "Close it?", WriteOwn: true, Subject: &agentfleetv1.CardSubject{Repo: "o/r", Kind: "pr", Number: 7}, Options: []*agentfleetv1.CardOption{
		{Id: "1", Label: "keep", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
		{Id: "2", Label: "close all", Reversible: "no", Effect: &agentfleetv1.CardEffect{Verb: "close_threads"}},
	}}
	perm := &agentfleetv1.Card{Ask: "Run Bash?", Detail: `{"command":"<script>alert(1)</script>"}`, Options: []*agentfleetv1.CardOption{
		{Id: "allow", Label: "Allow once", Reversible: "no", Effect: &agentfleetv1.CardEffect{Verb: "grant"}},
		{Id: "deny", Label: "Deny", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "deny"}},
	}}
	return Config{
		Fleet: fleet, Key: []byte("k"), AdminOrigins: []string{"https://game.bnei.dev"}, Now: func() time.Time { return now },
		Load: func(_ context.Context, id string) (decisions.Record, error) {
			switch id {
			case "d1":
				return decisions.Record{ID: "d1", Kind: decisions.Card, State: decisions.Open, Version: 3}, nil
			case "p1":
				return decisions.Record{ID: "p1", Kind: decisions.Permission, State: decisions.Open, Version: 3}, nil
			}
			return decisions.Record{}, decisions.ErrNotFound
		},
		Card: func(r decisions.Record) *agentfleetv1.Card {
			if r.Kind == decisions.Permission {
				return perm
			}
			return card
		},
		Answer: func(_ context.Context, _ auth.Identity, _, _, own string, v int64) (bool, string, error) {
			f.calls++
			f.gotOwn, f.version = own, v
			return f.reason == "", f.reason, f.err
		},
	}, f
}

var owner = auth.Identity{Sub: "u1", Groups: []string{auth.RequiredGroup}, AuthAt: now.Unix()}

func do(c Config, id auth.Identity, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	Handler(c).ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
	return w
}

func get(c Config, id auth.Identity, q string) *httptest.ResponseRecorder {
	return do(c, id, httptest.NewRequest(http.MethodGet, Path+"?"+q, nil))
}

var hidden = regexp.MustCompile(`name="(\w+)" value="([^"]*)"`)

// form is the hidden fields a good GET rendered.
func form(t *testing.T, c Config, q string) url.Values {
	t.Helper()
	w := get(c, owner, q)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<form") {
		t.Fatalf("GET %s = %d %s", q, w.Code, w.Body)
	}
	v := url.Values{}
	for _, m := range hidden.FindAllStringSubmatch(w.Body.String(), -1) {
		v.Set(m[1], html.UnescapeString(m[2]))
	}
	return v
}

func post(c Config, id auth.Identity, v url.Values, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", origin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return do(c, id, r)
}

func TestGet(t *testing.T) {
	c, f := setup()
	stale := owner
	stale.AuthAt = now.Add(-11 * time.Minute).Unix()
	for _, tc := range []struct {
		name, q  string
		id       auth.Identity
		code     int
		contains string
	}{
		{"non-admin", "id=d1&option=2&v=3", auth.Identity{Sub: "u1", AuthAt: now.Unix()}, 403, "Only the owner"},
		{"yes option refused", "id=d1&option=1&v=3", owner, 400, "reversible"},
		{"unknown option", "id=d1&option=9&v=3", owner, 400, "No such option"},
		{"stale version", "id=d1&option=2&v=2", owner, 409, "Nothing was done"},
		{"unknown card", "id=x&option=2&v=3", owner, 404, "No such card"},
		{"bad v", "id=d1&option=2&v=x", owner, 400, "Bad link"},
		{"not fresh", "id=d1&option=2&v=3", stale, 200, `target="_top"`},
		{"effect from the stored card", "id=d1&option=2&v=3", owner, 200, "Closes every thread on o/r pr #7 (irreversible)"},
		{"own words", "id=d1&option=&v=3", owner, 200, "<textarea"},
	} {
		w := get(c, tc.id, tc.q)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.contains) {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
		}
	}
	if f.calls != 0 {
		t.Errorf("GET answered %d times", f.calls)
	}
	// The re-auth link comes back here, return_to and all.
	w := get(c, stale, "id=d1&option=2&v=3&return_to=https://game.bnei.dev/town")
	if !strings.Contains(w.Body.String(), `href="/auth/login?reauth=1&amp;return_to=%2Fdecisions%2Fconfirm%3Fid%3Dd1%26option%3D2%26return_to%3Dhttps%253A%252F%252Fgame.bnei.dev%252Ftown%26v%3D3"`) {
		t.Errorf("re-auth link: %s", w.Body)
	}
}

func TestHeaders(t *testing.T) {
	c, _ := setup()
	for _, w := range []*httptest.ResponseRecorder{get(c, owner, "id=d1&option=2&v=3"), get(c, auth.Identity{}, "")} {
		h := w.Header()
		// form-action names the admin origin the 303 goes to; same-origin,
		// not no-referrer, so a browser's POST carries Origin, not "null".
		if h.Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://game.bnei.dev; frame-ancestors 'none'" ||
			h.Get("Referrer-Policy") != "same-origin" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("headers: %v", h)
		}
	}
}

func TestPost(t *testing.T) {
	c, f := setup()
	good := form(t, c, "id=d1&option=2&v=3&return_to=https://game.bnei.dev/town?x=1")
	other := owner
	other.Sub = "u2"
	stale := owner
	stale.AuthAt = now.Add(-11 * time.Minute).Unix()
	tampered := url.Values{}
	for k, v := range good {
		tampered[k] = v
	}
	tampered.Set("option", "1")
	expired, _ := setup()
	expired.Now = func() time.Time { return now.Add(6 * time.Minute) }

	for _, tc := range []struct {
		name   string
		c      Config
		id     auth.Identity
		v      url.Values
		origin string
	}{
		{"cross-origin", c, owner, good, "https://game.bnei.dev"},
		{"opaque origin", c, owner, good, "null"},
		{"no origin", c, owner, good, ""},
		{"bad token", c, owner, tampered, fleet},
		{"other sub", c, other, good, fleet},
		{"expired", expired, owner, good, fleet},
		{"not fresh", c, stale, good, fleet},
	} {
		if w := post(tc.c, tc.id, tc.v, tc.origin); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(good.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", fleet)
	r.Header.Set("Sec-Fetch-Site", "same-site")
	if w := do(c, owner, r); w.Code != http.StatusForbidden {
		t.Errorf("same-site: %d", w.Code)
	}
	if f.calls != 0 {
		t.Fatalf("a refused POST answered")
	}

	w := post(c, owner, good, fleet)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "https://game.bnei.dev/town?x=1" || f.calls != 1 || f.version != 3 {
		t.Errorf("good POST: %d %q calls=%d v=%d", w.Code, w.Header().Get("Location"), f.calls, f.version)
	}

	// Answered but not delivered: said, with the link back, no redirect.
	f.reason = "swept"
	if w := post(c, owner, good, fleet); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "not delivered: swept") ||
		!strings.Contains(w.Body.String(), `href="https://game.bnei.dev/town?x=1"`) {
		t.Errorf("not delivered: %d %s", w.Code, w.Body)
	}
	f.reason = ""

	f.err = errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyDecisionStale, decisions.ErrStale)
	if w := post(c, owner, good, fleet); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Nothing was done") {
		t.Errorf("stale answer: %d %s", w.Code, w.Body)
	}
	f.err = errors.New("boom")
	if w := post(c, owner, good, fleet); w.Code != http.StatusInternalServerError {
		t.Errorf("failed answer: %d", w.Code)
	}
}

func TestPostOwnWordsAndDisallowedReturn(t *testing.T) {
	c, f := setup()
	v := form(t, c, "id=d1&option=&v=3&return_to=https://evil.example/x")
	if v.Get("return_to") != "" {
		t.Fatalf("disallowed return_to kept: %q", v.Get("return_to"))
	}
	v.Set("own_text", "do the other thing")
	w := post(c, owner, v, fleet)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Answered") || f.gotOwn != "do the other thing" {
		t.Errorf("own words: %d %s own=%q", w.Code, w.Body, f.gotOwn)
	}
}

func TestReturnTo(t *testing.T) {
	c, _ := setup()
	for raw, want := range map[string]bool{
		"/x":                           true,
		"https://game.bnei.dev/t?a=1":  true,
		"https://game.bnei.dev:444/t":  false,
		"http://game.bnei.dev/t":       false,
		"https://u@game.bnei.dev/t":    false,
		"https://game.bnei.dev.evil/t": false,
		"//evil.example":               false,
		"/\\evil.example":              false,
		"https://game.bnei.dev/\x00":   false,
		"javascript:alert(1)":          false,
	} {
		if got := c.returnTo(raw) != ""; got != want {
			t.Errorf("returnTo(%q) = %v, want %v", raw, got, want)
		}
	}
}

// A surface's own words are shown read-only and answered exactly as signed.
func TestPostSurfaceOwnWords(t *testing.T) {
	c, f := setup()
	if w := get(c, owner, "id=d1&option=&v=3&own_text="+strings.Repeat("x", maxOwn+1)); w.Code != http.StatusBadRequest {
		t.Errorf("over-long own_text: %d", w.Code)
	}
	v := form(t, c, "id=d1&option=&v=3&own_text="+url.QueryEscape(`ship "it"`))
	tampered := url.Values{}
	for k, x := range v {
		tampered[k] = x
	}
	tampered.Set("text", "something else")
	if w := post(c, owner, tampered, fleet); w.Code != http.StatusForbidden {
		t.Errorf("tampered text: %d", w.Code)
	}
	v.Set("own_text", "typed instead")
	if w := post(c, owner, v, fleet); w.Code != http.StatusOK || f.gotOwn != `ship "it"` {
		t.Errorf("surface own words: %d own=%q", w.Code, f.gotOwn)
	}
}

// A browser posts the signed text with CRLF; the token signed LF.
func TestPostSurfaceOwnWordsCRLF(t *testing.T) {
	c, f := setup()
	v := form(t, c, "id=d1&option=&v=3&own_text="+url.QueryEscape("a\nb"))
	v.Set("text", "a\r\nb")
	if w := post(c, owner, v, fleet); w.Code != http.StatusOK || f.gotOwn != "a\nb" {
		t.Errorf("CRLF: %d own=%q", w.Code, f.gotOwn)
	}
}

// Long own words stay out of the re-auth link (login's state cookie); short
// ones ride it.
func TestReauthOwnWordsSize(t *testing.T) {
	c, _ := setup()
	stale := owner
	stale.AuthAt = now.Add(-11 * time.Minute).Unix()
	long := strings.Repeat("é\n&", maxOwn/4)
	w := get(c, stale, "id=d1&option=&v=3&own_text="+url.QueryEscape(long))
	if b := w.Body.String(); strings.Contains(b, "own_text") || !strings.Contains(b, "type it again") || len(b) > 2048 {
		t.Errorf("long: %s", b)
	}
	if w := get(c, stale, "id=d1&option=&v=3&own_text=ship"); !strings.Contains(w.Body.String(), "own_text%3Dship") {
		t.Errorf("short: %s", w.Body)
	}
}

// A permission's card view confirms like a card: its input shown escaped.
func TestGetPermission(t *testing.T) {
	c, _ := setup()
	w := get(c, owner, "id=p1&option=allow&v=3")
	if b := w.Body.String(); w.Code != http.StatusOK || strings.Contains(b, "<script>") ||
		!strings.Contains(b, `<pre style="white-space:pre-wrap">{&#34;command&#34;:&#34;&lt;script&gt;alert(1)&lt;/script&gt;&#34;}</pre>`) ||
		!strings.Contains(b, "Lets the agent run exactly this, once (irreversible)") {
		t.Errorf("allow: %d %s", w.Code, b)
	}
	if w := get(c, owner, "id=p1&option=deny&v=3"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "reversible") {
		t.Errorf("deny: %d %s", w.Code, w.Body)
	}
	if w := get(c, owner, "id=p1&option=allow&v=2"); w.Code != http.StatusConflict {
		t.Errorf("stale version: %d %s", w.Code, w.Body)
	}
}

func TestEffectText(t *testing.T) {
	s := &agentfleetv1.CardSubject{Repo: "o/r", Kind: "repo"}
	for verb, want := range map[string]string{
		"grant":         "Lets the agent run exactly this, once",
		"deny":          "Refuses; the agent is told",
		"approve_plan":  "The agent leaves plan mode and starts the work",
		"keep_planning": "The agent keeps planning",
		"choose":        "Sends your choice to the agent",
		"handoff":       "Sends your choice to the agent",
		"close_threads": "Closes every thread on o/r",
	} {
		if got := effectText(&agentfleetv1.CardOption{Effect: &agentfleetv1.CardEffect{Verb: verb}}, s); got != want {
			t.Errorf("%s: %q", verb, got)
		}
	}
}
