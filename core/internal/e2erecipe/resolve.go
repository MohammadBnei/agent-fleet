// Package e2erecipe resolves which environment recipe an e2e sandbox is built
// from, for the two callers that need to: CoreService.RequestE2EEnv (the
// agent's path) and DashboardService.StartE2e (a human clicking Start).
//
// It exists as a package rather than a method on either server because the
// resolution order is load-bearing and easy to get subtly wrong — it already
// was. Core hardcoded the profile name "e2e", which silently gave agent-fleet
// a sandbox with none of its toolchain, because agent-fleet's recipe is named
// "lint" (docs/adr/0044). The dashboard's own GetE2EStatus had a second copy
// of that same hardcoded lookup and so reported a profile the pod wasn't
// built from. Two copies, two different bugs; hence one function.
//
// Same reasoning core/cmd/core/run.go already applies to warmIfIdle: a second
// implementation of a rule with real consequences is a second thing to keep
// in sync, and it will not stay in sync.
package e2erecipe

import (
	"context"
	"fmt"

	"github.com/MohammadBnei/agent-fleet/core/internal/repoprofiles"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
)

// DefaultProfileName is the convention a repo falls back to when it has no
// e2e_profile column set. A repo with no profile under this name is not an
// error — it yields an empty recipe, which builds a sandbox with the base
// image's toolchain and no app (docs/adr/0044).
const DefaultProfileName = "e2e"

// Recipe is the resolved answer: what the pod should be built from.
type Recipe struct {
	// ProfileName is always set, even when Profile is nil — the caller needs
	// to be able to say "resolved to 'lint', which doesn't exist for this
	// repo" rather than reporting an empty string.
	ProfileName string
	StartCmd    string
	ToolKeys    []string
	Services    []repoprofiles.ServiceIngredient
}

// RepoGetter/ProfileGetter are the narrow slices of repos.Store and
// repoprofiles.Store this package needs — same test-seam convention the
// reconcile and mcpserver packages already use, so resolution order is
// unit-testable without a Postgres.
type RepoGetter interface {
	Get(ctx context.Context, name string) (*repos.Repo, error)
}

type ProfileGetter interface {
	Get(ctx context.Context, repo, name string) (*repoprofiles.Profile, error)
}

// Resolve determines the recipe for repo, in this order:
//
//  1. override — an explicitly requested profile name. Only the agent supplies
//     this, and only after a human approved it in the sidecar, so ADR-0034's
//     rule holds: a task branch cannot silently change what the provisioner
//     builds.
//  2. the repo's own e2e_profile column, maintained by a human in the
//     dashboard.
//  3. DefaultProfileName.
//
// A missing profile row is not an error — it means an empty recipe.
func Resolve(ctx context.Context, repoStore RepoGetter, profileStore ProfileGetter, repo, override string) (Recipe, error) {
	name := override
	if name == "" {
		r, err := repoStore.Get(ctx, repo)
		if err != nil {
			return Recipe{}, fmt.Errorf("repo lookup: %w", err)
		}
		if r != nil {
			name = r.E2eProfile
		}
	}
	if name == "" {
		name = DefaultProfileName
	}

	out := Recipe{ProfileName: name}
	profile, err := profileStore.Get(ctx, repo, name)
	if err != nil {
		return Recipe{}, fmt.Errorf("repo profile lookup: %w", err)
	}
	if profile != nil {
		out.StartCmd = profile.StartCmd
		out.ToolKeys = profile.Tools
		out.Services = profile.Services
	}
	return out, nil
}
