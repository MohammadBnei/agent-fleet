// Package confirm is core's own top-level confirm page for a card answer from
// another of our app surfaces (docs/adr/0082 §7): a script injected into the
// game can click whatever the game draws, so an option that is not reversible
// "yes", and any answer in the owner's own words, passes this page. It is
// Go-rendered at one exact path, with no script and no links out, so nothing
// framed or injected can drive it, and only a POST carrying the token its own
// GET minted answers.
package confirm

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Path is the page's one route.
const Path = "/decisions/confirm"

// maxOwn is agentsvc's validMessage cap on the owner's own words.
const maxOwn = 2000

// maxCarry caps the escaped own words a re-auth link carries.
const maxCarry = 1024

// lf makes line breaks LF: a browser posts every form value with CRLF, the
// game's textarea sends LF, and the token must sign the same text both ways.
func lf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// tokenTTL bounds how long a rendered page can be submitted.
const tokenTTL = 5 * time.Minute

// Config is the page's dependencies.
type Config struct {
	// Fleet is core's own origin (https://fleet.bnei.dev): a POST's Origin
	// must equal it.
	Fleet string
	// Key signs the form token.
	Key  []byte
	Load func(ctx context.Context, id string) (decisions.Record, error)
	// Card reads a record's stored card; nil when it is not one.
	Card func(decisions.Record) *agentfleetv1.Card
	// Answer is agentsvc's ConfirmCard: whether the agent will read it, and
	// why not.
	Answer       func(ctx context.Context, id auth.Identity, decisionID, optionID, ownText string, version int64) (bool, string, error)
	AdminOrigins []string
	Now          func() time.Time
}

// claim is what the token signs: the page as GET rendered it, for this
// owner, until Exp.
type claim struct {
	Sub    string `json:"s"`
	ID     string `json:"i"`
	Option string `json:"o"`
	V      int64  `json:"v"`
	Return string `json:"r"`
	// Text is the surface's own words, shown and answered exactly.
	Text string `json:"t"`
	Exp  int64  `json:"x"`
}

func (c Config) sign(cl claim) string {
	b, _ := json.Marshal(cl)
	m := hmac.New(sha256.New, c.Key)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}

// Handler serves GET (render) and POST (answer) at Path. The gate runs
// before it, so the caller's identity is in the context.
func Handler(c Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// form-action also covers the 303 back to an admin origin (exact
		// origins from config); same-origin keeps the POST's Origin, which
		// no-referrer would send as "null".
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+strings.Join(append([]string{"'self'"}, c.AdminOrigins...), " ")+"; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if r.URL.Path != Path {
			http.NotFound(w, r)
			return
		}
		id, ok := auth.FromContext(r.Context())
		if !ok || !auth.AdminOnly(id) {
			page(w, http.StatusForbidden, view{Msg: "Only the owner answers a card."})
			return
		}
		switch r.Method {
		case http.MethodGet:
			c.get(w, r, id)
		case http.MethodPost:
			c.post(w, r, id)
		default:
			w.Header().Set("Allow", "GET, POST")
			page(w, http.StatusMethodNotAllowed, view{Msg: "Method not allowed."})
		}
	})
}

func (c Config) get(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	q := r.URL.Query()
	cl := claim{Sub: id.Sub, ID: q.Get("id"), Option: q.Get("option"), Return: c.returnTo(q.Get("return_to"))}
	v, err := strconv.ParseInt(q.Get("v"), 10, 64)
	if err != nil {
		page(w, http.StatusBadRequest, view{Msg: "Bad link. Nothing was done."})
		return
	}
	cl.V = v
	rec, err := c.Load(r.Context(), cl.ID)
	if err != nil {
		page(w, http.StatusNotFound, view{Msg: "No such card. Nothing was done."})
		return
	}
	card := c.Card(rec)
	if rec.Kind != decisions.Card || card == nil || rec.State != decisions.Open || rec.Version != v {
		page(w, http.StatusConflict, view{Msg: "This card is closed or changed since. Nothing was done."})
		return
	}
	vw := view{Ask: card.GetAsk(), Own: cl.Option == ""}
	if vw.Own {
		if !card.GetWriteOwn() {
			page(w, http.StatusBadRequest, view{Msg: "This card takes no answer in your own words. Nothing was done."})
			return
		}
		// A surface's own words are shown and answered exactly (agentsvc
		// caps them); without any, the owner types them here.
		if cl.Text = lf(q.Get("own_text")); len(cl.Text) > maxOwn {
			page(w, http.StatusBadRequest, view{Msg: "Bad link. Nothing was done."})
			return
		}
	} else {
		i := slices.IndexFunc(card.GetOptions(), func(o *agentfleetv1.CardOption) bool { return o.GetId() == cl.Option })
		if i < 0 {
			page(w, http.StatusBadRequest, view{Msg: "No such option. Nothing was done."})
			return
		}
		o := card.GetOptions()[i]
		// A reversible option is answered in place; this page is for the rest.
		if o.GetReversible() == "yes" {
			page(w, http.StatusBadRequest, view{Msg: "This option is reversible: answer it where you are. Nothing was done."})
			return
		}
		vw.Label, vw.Effect, vw.Reversible = o.GetLabel(), effectText(o, card.GetSubject()), reversibleText[o.GetReversible()]
	}
	// docs/adr/0082 §7: a fresh sign-in, at top level (authentik refuses
	// framing), and back here.
	if !auth.Fresh(id, c.Now()) {
		bq := url.Values{"id": {cl.ID}, "option": {cl.Option}, "v": {q.Get("v")}, "return_to": {cl.Return}}
		msg := "Sign in again to confirm this answer."
		// The link rides login's state cookie (~4 KB, JSON then base64), so
		// long own words stay out of it and the owner types them again.
		// ponytail: a server-side pending store if retyping gets common.
		if e := url.QueryEscape(cl.Text); len(e) > maxCarry {
			msg += " Your answer is too long to carry through sign-in: you will type it again."
		} else if cl.Text != "" {
			bq.Set("own_text", cl.Text)
		}
		back := Path + "?" + bq.Encode()
		page(w, http.StatusOK, view{Msg: msg, Login: "/auth/login?reauth=1&return_to=" + url.QueryEscape(back)})
		return
	}
	cl.Exp = c.Now().Add(tokenTTL).Unix()
	vw.Form = &cl
	vw.Token = c.sign(cl)
	page(w, http.StatusOK, vw)
}

func (c Config) post(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	// A cross-site form post: refused before anything is read.
	if r.Header.Get("Origin") != c.Fleet || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin") {
		page(w, http.StatusForbidden, view{Msg: "Cross-site request refused. Nothing was done."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	v, err1 := strconv.ParseInt(r.PostFormValue("v"), 10, 64)
	exp, err2 := strconv.ParseInt(r.PostFormValue("exp"), 10, 64)
	cl := claim{Sub: id.Sub, ID: r.PostFormValue("id"), Option: r.PostFormValue("option"), V: v, Return: r.PostFormValue("return_to"), Text: lf(r.PostFormValue("text")), Exp: exp}
	if err1 != nil || err2 != nil || !hmac.Equal([]byte(c.sign(cl)), []byte(r.PostFormValue("token"))) {
		page(w, http.StatusForbidden, view{Msg: "Bad form. Nothing was done."})
		return
	}
	if c.Now().Unix() > cl.Exp {
		page(w, http.StatusForbidden, view{Msg: "This page expired. Nothing was done."})
		return
	}
	if !auth.Fresh(id, c.Now()) {
		page(w, http.StatusForbidden, view{Msg: "Your sign-in is no longer fresh. Nothing was done."})
		return
	}
	own := cl.Text
	if cl.Option == "" && own == "" {
		own = r.PostFormValue("own_text")
	}
	delivered, reason, err := c.Answer(r.Context(), id, cl.ID, cl.Option, own, cl.V)
	if err != nil {
		switch errkeys.Of(err) {
		case errkeys.KeyDecisionStale, errkeys.KeyDecisionExpired:
			page(w, http.StatusConflict, view{Msg: "This card expired or changed since. Nothing was done."})
		default:
			slog.ErrorContext(r.Context(), "confirm: answer failed", "decisionId", cl.ID, "error", err)
			page(w, http.StatusInternalServerError, view{Msg: "The answer failed. Try again from the console."})
		}
		return
	}
	// The token carries return_to, but it is checked again: the allowed
	// origins may have changed since the page was rendered.
	to := c.returnTo(cl.Return)
	// Not delivered (docs/adr/0082 §6, as the console shows it): said here,
	// with a link back, before the owner leaves.
	if !delivered {
		page(w, http.StatusOK, view{Msg: "Answered, but not delivered: " + reason, Back: to})
		return
	}
	if to != "" {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	page(w, http.StatusOK, view{Msg: "Answered. You can close this page."})
}

// returnTo is raw when login would also go there (auth.SafeReturnTo: a local
// path or an https URL on one of AdminOrigins exactly); "" otherwise, and the
// page then ends on its own done page.
func (c Config) returnTo(raw string) string {
	if raw != "" && auth.SafeReturnTo(raw, c.AdminOrigins) == raw {
		return raw
	}
	return ""
}

// The console's effect words (dashboard/src/components/CardBody.tsx): core
// says what an option does, beside the agent's label.
var reversibleText = map[string]string{"yes": "reversible", "partial": "partly reversible", "no": "irreversible"}

func effectText(o *agentfleetv1.CardOption, s *agentfleetv1.CardSubject) string {
	if o.GetEffect().GetVerb() != "close_threads" {
		return "Sends your choice to the agent"
	}
	if s.GetKind() == "repo" {
		return "Closes every thread on " + s.GetRepo()
	}
	return "Closes every thread on " + s.GetRepo() + " " + s.GetKind() + " #" + strconv.FormatInt(s.GetNumber(), 10)
}

type view struct {
	Msg, Login, Back               string
	Ask, Label, Effect, Reversible string
	Own                            bool
	Form                           *claim
	Token                          string
}

var tmpl = template.Must(template.New("").Parse(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Confirm answer</title>
<body style="font-family:system-ui,sans-serif;padding:24px;max-width:40em">
{{if .Msg}}<p>{{.Msg}}</p>{{if .Login}}<p><a href="{{.Login}}" target="_top">Sign in again</a></p>{{end}}{{if .Back}}<p><a href="{{.Back}}" target="_top">Back</a></p>{{end}}
{{else}}<p><small>Agent claim</small><br>{{.Ask}}</p>
<form method="post" action="/decisions/confirm">
{{if .Form.Text}}<p><small>Your answer, in your own words</small><br><textarea readonly rows="6" cols="60">{{.Form.Text}}</textarea></p>
{{else if .Own}}<p><label>Your answer, in your own words<br><textarea name="own_text" rows="6" cols="60" maxlength="2000" required></textarea></label></p>
{{else}}<p><small>Agent claim</small><br><b>{{.Label}}</b></p><p>{{.Effect}} ({{.Reversible}}).</p>{{end}}
<input type="hidden" name="id" value="{{.Form.ID}}"><input type="hidden" name="option" value="{{.Form.Option}}">
<input type="hidden" name="v" value="{{.Form.V}}"><input type="hidden" name="return_to" value="{{.Form.Return}}">
<input type="hidden" name="text" value="{{.Form.Text}}"><input type="hidden" name="exp" value="{{.Form.Exp}}"><input type="hidden" name="token" value="{{.Token}}">
<button type="submit">Confirm</button></form>{{end}}`))

func page(w http.ResponseWriter, status int, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tmpl.Execute(w, v)
}
