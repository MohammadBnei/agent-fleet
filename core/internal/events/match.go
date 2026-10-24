package events

import "strings"

// TypeMatches is the bridge's type filter: an empty list matches every type,
// otherwise one prefix must.
func TypeMatches(prefixes []string, typ string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}

// SubjectMatches is the bridge's subject filter: an empty list matches every
// subject, otherwise one glob must ('*' any run of bytes, '/' included; '?'
// one byte).
func SubjectMatches(globs []string, subject string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if Glob(g, subject) {
			return true
		}
	}
	return false
}

// Glob matches s against g with the bridge's '*' and '?'. Iterative, with
// one backtrack point, so a glob of many stars stays linear.
func Glob(g, s string) bool {
	gi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case gi < len(g) && (g[gi] == '?' || g[gi] == s[si]):
			gi++
			si++
		case gi < len(g) && g[gi] == '*':
			star, mark = gi, si
			gi++
		case star >= 0:
			gi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for gi < len(g) && g[gi] == '*' {
		gi++
	}
	return gi == len(g)
}
