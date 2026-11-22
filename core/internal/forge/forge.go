// Package forge names the fleet's forge (Forgejo at git.bnei.dev, build plan P4.4) the way the bridge's
// /webhook/forgejo route does: thread keys forge/<owner>/<repo>/pr|issue|run/<n>, types dev.bnei.forge.<event>.<action>,
// source git.bnei.dev/<owner>/<repo>. GitHub's github/ keys and dev.bnei.github. types are not read any more.
package forge

import (
	"net/url"
	"strconv"
	"strings"
)

// URL is the forge's web base, FORGE_URL: links, and the host RepoURL binds a definition's repo to. cmd/core sets it once from config before anything reads it.
// ponytail: a package var, not a field threaded into dispatch, decisions and coreserver; it never changes at runtime.
var URL = "https://git.bnei.dev"

// Base is raw as FORGE_URL must be, https://<its host, lowercased>; "" when raw is not an https URL with a host alone
// (no port, path, userinfo, query or anything else), the check the provisioner refuses its start on (build plan P4.1).
// Core's config refuses its start on it too.
func Base(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	base := "https://" + strings.ToLower(u.Hostname())
	if !strings.EqualFold(raw, base) {
		return ""
	}
	return base
}

// RepoURL is repo's (<owner>/<repo>) web URL on the forge, Base(URL)/<owner>/<repo>; "" when URL has no Base: a
// caller binding a credential to the result then refuses.
func RepoURL(repo string) string {
	if b := Base(URL); b != "" && repo != "" {
		return b + "/" + repo
	}
	return ""
}

// SourceHost is the host the bridge puts in every forge event's source. The bridge hardcodes it
// (internal/forgejo Normalize), so it is not FORGE_URL's: a local or ported FORGE_URL must not stop intake.
const SourceHost = "git.bnei.dev"

// Type is the prefix of every forge event type.
const Type = "dev.bnei.forge."

// FromForge: source is the bridge's for a forge event, git.bnei.dev/<owner>/<repo>. A PublishEvent hop never has one.
func FromForge(source string) bool { return strings.HasPrefix(source, SourceHost+"/") }

// RepoOf reads forge/<owner>/<repo>/... as owner/repo and repo; "" for any other subject.
func RepoOf(subject string) (full, name string) {
	parts := strings.SplitN(subject, "/", 4)
	if len(parts) < 3 || parts[0] != "forge" || parts[1] == "" || parts[2] == "" {
		return "", ""
	}
	return parts[1] + "/" + parts[2], parts[2]
}

// PR is a PR's thread key, forge/<owner>/<repo>/pr/<n>.
func PR(repo string, n int64) string { return "forge/" + repo + "/pr/" + strconv.FormatInt(n, 10) }

// PROf reads PR's key back.
func PROf(subject string) (repo string, n int64, ok bool) {
	parts := strings.Split(subject, "/")
	if len(parts) != 5 || parts[0] != "forge" || parts[1] == "" || parts[2] == "" || parts[3] != "pr" {
		return "", 0, false
	}
	n, err := strconv.ParseInt(parts[4], 10, 64)
	return parts[1] + "/" + parts[2], n, err == nil && n > 0
}
