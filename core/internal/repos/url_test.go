package repos

import "testing"

func TestHostAndOwnerRepo(t *testing.T) {
	for u, want := range map[string][2]string{
		"https://GitHub.com/MohammadBnei/Drop.git": {"github.com", "mohammadbnei/drop"},
		"https://git.bnei.dev/MohammadBnei/drop/":  {"git.bnei.dev", "mohammadbnei/drop"},
		"git@github.com:x/stray.git":               {"github.com", "x/stray"},
		"/tmp/origin":                              {"", "tmp/origin"},
	} {
		if h, p := hostOf(u), ownerRepo(u); h != want[0] || p != want[1] {
			t.Errorf("%s: host %q, owner/repo %q; want %q", u, h, p, want)
		}
	}
}
