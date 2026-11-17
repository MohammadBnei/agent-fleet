package definitions

import "slices"

// Widens reports whether next gives a pod more than old did (docs/adr/0084 §3), the test a re-pin in place must
// pass. ceiling: a tool or sidecar tool old lacked, a deny removed, or cluster.read newly on. vault: a service, path or
// vault the old one did not name. `spawns.allow` joins the ceiling when the spec has it (slice 8).
func Widens(old, next *Spec) (ceiling, vault bool) {
	if old == nil || next == nil {
		return true, true
	}
	var oldTools, newTools Tools
	if old.Tools != nil {
		oldTools = *old.Tools
	}
	if next.Tools != nil {
		newTools = *next.Tools
	}
	ceiling = !subset(newTools.Allow, oldTools.Allow) || !subset(oldTools.Deny, newTools.Deny) ||
		!subset(next.SidecarTools, old.SidecarTools) ||
		(next.Cluster != nil && next.Cluster.Read && (old.Cluster == nil || !old.Cluster.Read))
	vault = !subset(serviceKeys(next.Credentials), serviceKeys(old.Credentials))
	return ceiling, vault
}

// serviceKeys names a vault and each of its service paths; a service with no path names the whole host.
func serviceKeys(c *Credentials) []string {
	if c == nil {
		return nil
	}
	out := []string{"vault " + c.Vault}
	for _, s := range c.Services {
		paths := s.Paths
		if len(paths) == 0 {
			paths = []string{""}
		}
		for _, p := range paths {
			out = append(out, s.Host+" "+p+" "+s.Secret)
		}
	}
	return out
}

// subset: every element of a is in b.
func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}
