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
// aimed at a reserved subject: its literal head, up to the first wildcard,
// starts with one or could still become one. A glob with no literal head
// ("*", "?x") matches everything like an empty subject list and is not
// refused: the reserved types are the fence, this only refuses a filter that
// targets those threads.
func SubjectReaches(g string) bool {
	head, wild := g, false
	if i := strings.IndexAny(g, "*?"); i >= 0 {
		head, wild = g[:i], true
	}
	if head == "" {
		return false
	}
	for _, r := range ReservedSubjects {
		if strings.HasPrefix(head, r) || (wild && strings.HasPrefix(r, head)) {
			return true
		}
	}
	return false
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
