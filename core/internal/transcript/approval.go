package transcript

import "regexp"

// Direct port of worker/src/session.ts's isApproval/isAbort — exact regex
// parity matters here: this is the human approval gate (see docs/adr/0005),
// and a behavior drift silently breaks it. The `type` field (only ever set
// by the Discord bot's relay, never by the LLM-issued send_message tool
// call) short-circuits before falling back to regex-matching free text.

var (
	approveRe = regexp.MustCompile(`(?i)\bapprove(d)?\b|\blgtm\b|\bship it\b|\bgo ahead\b`)
	abortRe   = regexp.MustCompile(`(?i)\b(stop|abort|cancel|kill)\b`)
)

// IsApproval reports whether an entry should be treated as human approval.
func IsApproval(e Entry) bool {
	switch e.Type {
	case "approve":
		return true
	case "abort":
		return false
	}
	return approveRe.MatchString(e.Text)
}

// IsAbort reports whether an entry should be treated as a human abort/stop.
func IsAbort(e Entry) bool {
	switch e.Type {
	case "abort":
		return true
	case "approve":
		return false
	}
	return abortRe.MatchString(e.Text)
}
