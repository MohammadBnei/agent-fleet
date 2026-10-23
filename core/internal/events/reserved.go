// Package events holds the event-bridge rules core and its loader share
// (infra-bootstrap ADR-0066 §3-4, docs/adr/0078): the human-facing types and
// subjects nothing an agent runs may read or publish.
package events

import "strings"

// ReservedTypes are the human-facing type prefixes: a session waiting on a
// person, and mountain's game. An agent woken by its own wait would loop, and
// a game event's text would become a prompt.
var ReservedTypes = []string{"dev.bnei.fleet.session.", "dev.bnei.game."}

// ReservedSubjects are the subject prefixes those types live on.
var ReservedSubjects = []string{"fleet/", "game/"}

// TypeReaches reports whether type prefix p (as a subscription filter) can
// match a reserved type: it is a prefix of one, or starts with one. "" and
// "dev.bnei." reach both.
func TypeReaches(p string) bool {
	for _, r := range ReservedTypes {
		if strings.HasPrefix(p, r) || strings.HasPrefix(r, p) {
			return true
		}
	}
	return false
}

// SubjectReaches reports whether subject glob g (the bridge's * and ?) is
// aimed at a reserved subject: it can match some subject that starts with one.
// A glob of only '*' matches everything, like an empty subject list, and is
// not refused: the reserved types are the fence; this refuses a filter that
// targets those threads.
func SubjectReaches(g string) bool {
	if strings.Trim(g, "*") == "" {
		return false
	}
	for _, r := range ReservedSubjects {
		if canStartWith(g, r) {
			return true
		}
	}
	return false
}

// canStartWith: some string matching glob g starts with prefix p. Walking p,
// '*' can absorb the rest, '?' takes any one byte, a literal must be equal;
// running out of glob first means every match is shorter than p.
func canStartWith(g, p string) bool {
	for i := 0; i < len(p); i++ {
		if len(g) == 0 {
			return false
		}
		switch g[0] {
		case '*':
			return true
		case '?':
		default:
			if g[0] != p[i] {
				return false
			}
		}
		g = g[1:]
	}
	return true
}

// Reserved reports whether a concrete event type or subject is human-facing
// (for PublishEvent, which publishes one exact type on one exact subject).
func Reserved(typ, subject string) bool {
	for _, r := range ReservedTypes {
		if strings.HasPrefix(typ, r) {
			return true
		}
	}
	for _, r := range ReservedSubjects {
		if strings.HasPrefix(subject, r) {
			return true
		}
	}
	return false
}
