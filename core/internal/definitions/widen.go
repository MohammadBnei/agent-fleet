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
	vault = !vaultCovers(old.Credentials, next.Credentials)
	return ceiling, vault
}

// vaultCovers: everything next names, old already did. A service with no path names the whole host, so it covers
// any path of the same host and secret.
func vaultCovers(old, next *Credentials) bool {
	if next == nil {
		return true
	}
	if old == nil || old.Vault != next.Vault {
		return false
	}
	for _, n := range next.Services {
		ok := false
		for _, o := range old.Services {
			if o.Host == n.Host && o.Secret == n.Secret && (len(o.Paths) == 0 || (len(n.Paths) > 0 && subset(n.Paths, o.Paths))) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
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
