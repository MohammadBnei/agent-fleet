package decisions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/MohammadBnei/agent-fleet/core/internal/remediations"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Core-run card effect verbs (docs/adr/0086 §4). Only core files a card that carries one, and
// answering runs exactly the bound payload.
const (
	VerbCatalogue    = "catalogue"
	VerbOpenPR       = "open_pr"
	VerbOpenSession  = "open_session"
	VerbExecutorStep = "executor_step"
)

// IsCoreVerb: the verb carries a bound payload that core runs itself.
func IsCoreVerb(v string) bool {
	return v == VerbCatalogue || v == VerbOpenPR || v == VerbOpenSession || v == VerbExecutorStep
}

// PRTarget is open_pr's payload: a branch a B.C session pushed, and the commit the owner approved.
type PRTarget struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	HeadSHA string `json:"headSha"`
	Title   string `json:"title"` // agent claim, at most 80 characters
}

// Brief is open_session's payload. Repo is the fleet's repo name, as the console starts a session on it. Every field but Title is typed: a repo, enums and numbers core checks.
type Brief struct {
	Repo     string         `json:"repo"`
	Title    string         `json:"title"` // agent claim, at most 80 characters
	Findings []BriefFinding `json:"findings,omitempty"`
}

type BriefFinding struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

var (
	fleetRepo   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	headSHA     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	branchName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	findingName = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)
)

// ValidBranch: a branch name git accepts (git check-ref-format's rules for what we allow): no "..", "//",
// "./" segment starting with ".", trailing "/" or ".", or ".lock" suffix.
func ValidBranch(b string) bool {
	if !branchName.MatchString(b) || strings.Contains(b, "..") || strings.Contains(b, "//") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") {
		return false
	}
	for _, seg := range strings.Split(b, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
	}
	return true
}

// PayloadHash binds an approval to the verb and its payload.
func PayloadHash(verb, payload string) string {
	h := sha256.Sum256([]byte(verb + "\n" + payload))
	return hex.EncodeToString(h[:])
}

// bindEffect checks a core verb's payload, rewrites it canonical and sets its hash. A payload that does
// not parse as its verb's typed form is refused, never passed through.
func bindEffect(e *agentfleetv1.CardEffect) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInvalid, e.GetVerb(), fmt.Sprintf(format, a...))
	}
	var canon any
	switch e.GetVerb() {
	case VerbExecutorStep:
		return fmt.Errorf("%w: executor_step is declared but refused until ADR-0072's executor exists", ErrInvalid)
	case VerbCatalogue:
		sel, err := remediations.Parse([]byte(e.GetPayload()))
		if err != nil {
			return bad("%v", err)
		}
		if sel.Denied() {
			return bad("%v", remediations.ErrDenied)
		}
		canon = sel
	case VerbOpenPR:
		var t PRTarget
		if err := strictJSON(e.GetPayload(), &t); err != nil {
			return bad("%v", err)
		}
		if err := checkRepo(t.Repo); err != nil {
			return bad("%v", err)
		}
		if !ValidBranch(t.Branch) || !headSHA.MatchString(t.HeadSHA) {
			return bad("a branch name and a 40 hex head sha")
		}
		if err := text("title", t.Title, 80, false); err != nil {
			return err
		}
		canon = t
	case VerbOpenSession:
		var b Brief
		if err := strictJSON(e.GetPayload(), &b); err != nil {
			return bad("%v", err)
		}
		if !fleetRepo.MatchString(b.Repo) {
			return bad("repo %q is not a fleet repo name", b.Repo)
		}
		if err := text("title", b.Title, 80, false); err != nil {
			return err
		}
		if len(b.Findings) > 8 {
			return bad("at most 8 findings")
		}
		for _, f := range b.Findings {
			if !findingName.MatchString(f.Name) || f.Value < 0 || f.Value > 1e9 {
				return bad("finding %q", f.Name)
			}
		}
		canon = b
	default:
		return nil
	}
	b, _ := json.Marshal(canon)
	e.Payload, e.PayloadHash = string(b), PayloadHash(e.GetVerb(), string(b))
	return nil
}

// DiffURL is where the owner reads open_pr's branch at the approved commit.
func DiffURL(repo, headSHA string) string {
	return "https://github.com/" + repo + "/compare/" + headSHA
}

// EffectLines is a core verb's canonical payload as the owner reads it, one "field: value" per line, so
// an approval runs bytes that were shown: the title is labelled an agent claim, open_pr adds its diff.
func EffectLines(e *agentfleetv1.CardEffect) []string {
	var m map[string]any
	if !IsCoreVerb(e.GetVerb()) || json.Unmarshal([]byte(e.GetPayload()), &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		label := k
		if k == "title" {
			label = "title (agent claim)"
		}
		if fs, ok := m[k].([]any); ok {
			for _, f := range fs {
				if fm, ok := f.(map[string]any); ok {
					out = append(out, fmt.Sprintf("%s: %v = %v", k, fm["name"], fm["value"]))
				}
			}
			continue
		}
		out = append(out, fmt.Sprintf("%s: %v", label, m[k]))
	}
	if e.GetVerb() == VerbOpenPR {
		var t PRTarget
		if json.Unmarshal([]byte(e.GetPayload()), &t) == nil {
			out = append(out, "diff: "+DiffURL(t.Repo, t.HeadSHA))
		}
	}
	return out
}

// VerifyEffect is the check before an effect runs: the stored payload still hashes to what the card
// was filed with, so an approval runs the bytes the owner was shown.
func VerifyEffect(e *agentfleetv1.CardEffect) error {
	if e.GetPayloadHash() == "" || e.GetPayloadHash() != PayloadHash(e.GetVerb(), e.GetPayload()) {
		return fmt.Errorf("%w: the %s payload no longer matches its hash", ErrStale, e.GetVerb())
	}
	return nil
}

func strictJSON(raw string, v any) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		return fmt.Errorf("payload is unreadable")
	}
	return nil
}

func checkRepo(repo string) error {
	if !repoName.MatchString(repo) {
		return fmt.Errorf("repo %q is not owner/name", repo)
	}
	return nil
}
