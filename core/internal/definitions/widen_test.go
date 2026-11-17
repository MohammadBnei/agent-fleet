package definitions

import "testing"

func TestWidens(t *testing.T) {
	base := func() *Spec {
		return &Spec{
			Tools:        &Tools{Allow: []string{"Read", "Bash(git push:*)"}, Deny: []string{"WebFetch"}},
			SidecarTools: []string{"send_message"},
			Credentials:  &Credentials{Vault: "v", Services: []Service{{Host: "api.github.com", Paths: []string{"/repos/a/b"}, Secret: "GH"}}},
		}
	}
	for _, c := range []struct {
		name            string
		edit            func(*Spec)
		ceiling, vaultW bool
	}{
		{"identical", func(*Spec) {}, false, false},
		{"unrelated field", func(s *Spec) { s.Model = &Model{Default: "x"} }, false, false},
		{"tool added", func(s *Spec) { s.Tools.Allow = append(s.Tools.Allow, "Edit") }, true, false},
		{"tool removed", func(s *Spec) { s.Tools.Allow = []string{"Read"} }, false, false},
		{"deny removed", func(s *Spec) { s.Tools.Deny = nil }, true, false},
		{"deny added", func(s *Spec) { s.Tools.Deny = append(s.Tools.Deny, "Edit") }, false, false},
		{"sidecar tool added", func(s *Spec) { s.SidecarTools = append(s.SidecarTools, "draft_card") }, true, false},
		{"cluster read on", func(s *Spec) { s.Cluster = &Cluster{Read: true} }, true, false},
		{"path added", func(s *Spec) { s.Credentials.Services[0].Paths = append(s.Credentials.Services[0].Paths, "/repos/c/d") }, false, true},
		{"service added", func(s *Spec) {
			s.Credentials.Services = append(s.Credentials.Services, Service{Host: "h", Secret: "S"})
		}, false, true},
		{"vault renamed", func(s *Spec) { s.Credentials.Vault = "w" }, false, true},
		{"whole host narrowed to a path", func(s *Spec) { s.Credentials.Services[0].Paths = nil }, false, true},
		{"service removed", func(s *Spec) { s.Credentials.Services = nil }, false, false},
	} {
		next := base()
		c.edit(next)
		ceiling, vault := Widens(base(), next)
		if ceiling != c.ceiling || vault != c.vaultW {
			t.Errorf("%s: ceiling %v vault %v, want %v %v", c.name, ceiling, vault, c.ceiling, c.vaultW)
		}
	}
	// A path-less old service covers any path of its host and secret.
	whole, one := base(), base()
	whole.Credentials.Services[0].Paths = nil
	if _, v := Widens(whole, one); v {
		t.Error("a path under a whole-host service is a narrowing")
	}
	if c, v := Widens(nil, base()); !c || !v {
		t.Error("an unreadable old version must count as widening")
	}
}
