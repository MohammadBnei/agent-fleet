// Package kubectl runs kubectl on behalf of pods that hold no cluster
// RBAC, and decides what an unattended caller is allowed to run.
//
// Ported from thot/src/kubectlRead.ts, which this replaces. That file's
// three layers are kept deliberately, because the RBAC layer alone is NOT
// sufficient: the ClusterRole genuinely grants `patch` on workloads and
// `delete` on pods (ADR-0032), so a "read-only" path that merely
// forwarded argv could mutate.
package kubectl

import (
	"fmt"
	"sort"
	"strings"
)

// readVerbs is every kubectl subcommand that only reads. Deliberately
// excludes some read-ish ones: `rollout` has a mutating `restart`
// subcommand, and `auth reconcile` writes. When in doubt it's left out —
// a human-gated call can still run it.
var readVerbs = map[string]bool{
	"get":           true,
	"describe":      true,
	"logs":          true,
	"top":           true,
	"events":        true,
	"explain":       true,
	"api-resources": true,
	"api-versions":  true,
	"cluster-info":  true,
	"version":       true,
	"diff":          true,
}

// deniedFlags would redirect who or what we're talking to. `--as`/
// `--as-group` matter even though the ClusterRole has no `impersonate`
// verb: defence in depth costs one line, and this process is the one
// place in the fleet where that's worth paying for.
var deniedFlags = map[string]bool{
	"--as":                 true,
	"--as-group":           true,
	"--as-uid":             true,
	"--kubeconfig":         true,
	"--server":             true,
	"--token":              true,
	"--client-key":         true,
	"--client-certificate": true,
	"--username":           true,
	"--password":           true,
}

// ValidateReadOnly returns a human-readable refusal, or "" if the args are
// an acceptable unattended read. Only applied to read_only requests —
// human-gated calls go through untouched (see executor.proto).
func ValidateReadOnly(args []string) string {
	if len(args) == 0 {
		return "no arguments given"
	}

	if !readVerbs[args[0]] {
		allowed := make([]string, 0, len(readVerbs))
		for v := range readVerbs {
			allowed = append(allowed, v)
		}
		sort.Strings(allowed)
		return fmt.Sprintf(
			"%q is not a read-only verb. Allowed: %s. A mutating command has to go through the human permission prompt instead.",
			args[0], strings.Join(allowed, ", "))
	}

	for _, arg := range args {
		// Compare on the part before '=' so --as=admin is caught
		// alongside the space-separated form.
		flag := arg
		if i := strings.Index(arg, "="); i >= 0 {
			flag = arg[:i]
		}
		if deniedFlags[flag] {
			return fmt.Sprintf("flag %q is not permitted", flag)
		}
	}
	return ""
}
