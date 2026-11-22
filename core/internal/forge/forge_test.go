package forge

import "testing"

// The bridge's subjects (ukubi-bridge internal/forgejo TestNormalize) read back; GitHub's keys do not.
func TestSubjects(t *testing.T) {
	for _, c := range []struct {
		subject, full, name, prRepo string
		pr                          int64
	}{
		{"forge/MohammadBnei/editable-blog/pr/42", "MohammadBnei/editable-blog", "editable-blog", "MohammadBnei/editable-blog", 42},
		{"forge/MohammadBnei/editable-blog/issue/9", "MohammadBnei/editable-blog", "editable-blog", "", 0},
		{"forge/MohammadBnei/editable-blog/run/117", "MohammadBnei/editable-blog", "editable-blog", "", 0},
		{"forge/o/r", "o/r", "r", "", 0},
		{"forge/o/r/pr/0", "o/r", "r", "", 0},
		{"forge/o/r/pr/x", "o/r", "r", "", 0},
		{"forge/o/r/pr/1/extra", "o/r", "r", "", 0},
		{"github/MohammadBnei/editable-blog/pr/42", "", "", "", 0},
		{"agent/x", "", "", "", 0},
		{"forge/o", "", "", "", 0},
		{"forge//r/pr/1", "", "", "", 0},
	} {
		if full, name := RepoOf(c.subject); full != c.full || name != c.name {
			t.Errorf("RepoOf(%q) = %q, %q; want %q, %q", c.subject, full, name, c.full, c.name)
		}
		repo, n, ok := PROf(c.subject)
		if ok != (c.pr > 0) || (ok && (repo != c.prRepo || n != c.pr)) {
			t.Errorf("PROf(%q) = %q, %d, %v; want %q, %d", c.subject, repo, n, ok, c.prRepo, c.pr)
		}
	}
	if s := PR("MohammadBnei/editable-blog", 42); s != "forge/MohammadBnei/editable-blog/pr/42" {
		t.Errorf("PR = %q", s)
	}
	if repo, n, ok := PROf(PR("o/r", 7)); !ok || repo != "o/r" || n != 7 {
		t.Error("PR does not read back")
	}
}

func TestFromForge(t *testing.T) {
	for source, want := range map[string]bool{
		"git.bnei.dev/MohammadBnei/editable-blog": true,
		"github.com/MohammadBnei/editable-blog":   false, // GitHub ingest is off
		"client/bridge-core":                      false, // a PublishEvent hop
		"git.bnei.devil/o/r":                      false,
		"":                                        false,
	} {
		if got := FromForge(source); got != want {
			t.Errorf("FromForge(%q) = %v, want %v", source, got, want)
		}
	}
	defer func(u string) { URL = u }(URL)
	URL = "http://localhost:3000"
	if !FromForge("git.bnei.dev/o/r") || FromForge("localhost:3000/o/r") {
		t.Error("the source host follows FORGE_URL; it is the bridge's fixed one")
	}
}

// RepoURL holds FORGE_URL to the provisioner's shape (P4.1): https and a host alone, or nothing binds.
func TestRepoURL(t *testing.T) {
	defer func(u string) { URL = u }(URL)
	for base, want := range map[string]string{
		"https://git.bnei.dev":      "https://git.bnei.dev/o/r",
		"https://Git.Bnei.Dev":      "https://git.bnei.dev/o/r",
		"https://github.com":        "https://github.com/o/r",
		"http://git.bnei.dev":       "",
		"https://git.bnei.dev:3000": "",
		"https://git.bnei.dev/sub":  "",
		"https://u@git.bnei.dev":    "",
		"https://git.bnei.dev?x":    "",
		"https://":                  "",
		"":                          "",
	} {
		URL = base
		if got := RepoURL("o/r"); got != want {
			t.Errorf("FORGE_URL %q: RepoURL = %q, want %q", base, got, want)
		}
	}
	if URL = "https://git.bnei.dev"; RepoURL("") != "" {
		t.Error("an empty repo bound")
	}
}

// Hosts is the provisioner's vault.HostsFor for the forge FORGE_URL names: the vault gets these, so a wrong one is a
// credential on another host.
func TestHosts(t *testing.T) {
	defer func(u string) { URL = u }(URL)
	for base, want := range map[string]string{
		"https://github.com":        "github.com api.github.com ",
		"https://GitHub.com":        "github.com api.github.com ",
		"https://git.bnei.dev":      "git.bnei.dev git.bnei.dev /api/v1",
		"https://git.bnei.dev:3000": "  ",
		"https://api.github.com":    "api.github.com api.github.com /api/v1",
		"":                          "  ",
	} {
		URL = base
		if git, api, prefix := Hosts(); git+" "+api+" "+prefix != want {
			t.Errorf("FORGE_URL %q: Hosts = %q %q %q, want %q", base, git, api, prefix, want)
		}
	}
}
