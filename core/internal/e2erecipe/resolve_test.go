package e2erecipe

import (
	"context"
	"errors"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/repoprofiles"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
)

type fakeRepos struct {
	repo *repos.Repo
	err  error
}

func (f *fakeRepos) Get(context.Context, string) (*repos.Repo, error) { return f.repo, f.err }

type fakeProfiles struct {
	// keyed by profile name, so a test can assert WHICH name was looked up
	// rather than just that something came back.
	byName    map[string]*repoprofiles.Profile
	lastAsked string
	err       error
}

func (f *fakeProfiles) Get(_ context.Context, _, name string) (*repoprofiles.Profile, error) {
	f.lastAsked = name
	if f.err != nil {
		return nil, f.err
	}
	return f.byName[name], nil
}

// The resolution order is the whole point of this package: it has already
// produced two separate bugs by being hardcoded to "e2e" in two places
// (docs/adr/0044).
func TestResolve_Order(t *testing.T) {
	profiles := map[string]*repoprofiles.Profile{
		"e2e":  {Name: "e2e", StartCmd: "bun run dev", Tools: []string{"bun-toolchain"}},
		"lint": {Name: "lint", Tools: []string{"go-toolchain", "buf"}},
	}

	tests := []struct {
		name        string
		repo        *repos.Repo
		override    string
		wantName    string
		wantCmd     string
		wantTools   []string
		description string
	}{
		{
			name:        "override wins over everything",
			repo:        &repos.Repo{Name: "agent-fleet", E2eProfile: "e2e"},
			override:    "lint",
			wantName:    "lint",
			wantTools:   []string{"go-toolchain", "buf"},
			description: "an approved agent override must beat the repo's column",
		},
		{
			name:        "repo column wins over the convention",
			repo:        &repos.Repo{Name: "agent-fleet", E2eProfile: "lint"},
			wantName:    "lint",
			wantTools:   []string{"go-toolchain", "buf"},
			description: "this is the case the old hardcoded lookup got wrong",
		},
		{
			name:      "empty column falls back to the e2e convention",
			repo:      &repos.Repo{Name: "dream-analyst", E2eProfile: ""},
			wantName:  "e2e",
			wantCmd:   "bun run dev",
			wantTools: []string{"bun-toolchain"},
		},
		{
			name:      "unknown repo still resolves to the convention",
			repo:      nil,
			wantName:  "e2e",
			wantCmd:   "bun run dev",
			wantTools: []string{"bun-toolchain"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pf := &fakeProfiles{byName: profiles}
			got, err := Resolve(context.Background(), &fakeRepos{repo: tt.repo}, pf, "agent-fleet", tt.override)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.ProfileName != tt.wantName {
				t.Errorf("ProfileName = %q, want %q — %s", got.ProfileName, tt.wantName, tt.description)
			}
			if pf.lastAsked != tt.wantName {
				t.Errorf("looked up profile %q, want %q", pf.lastAsked, tt.wantName)
			}
			if got.StartCmd != tt.wantCmd {
				t.Errorf("StartCmd = %q, want %q", got.StartCmd, tt.wantCmd)
			}
			if len(got.ToolKeys) != len(tt.wantTools) {
				t.Errorf("ToolKeys = %v, want %v", got.ToolKeys, tt.wantTools)
			}
		})
	}
}

// A repo pointing at a profile that doesn't exist is a working sandbox with
// the base image's toolchain and no app, not an error — docs/adr/0044 turned
// exactly this case from a hard failure into the default.
func TestResolve_MissingProfileIsNotAnError(t *testing.T) {
	pf := &fakeProfiles{byName: map[string]*repoprofiles.Profile{}}
	got, err := Resolve(context.Background(), &fakeRepos{repo: &repos.Repo{E2eProfile: "nope"}}, pf, "some-repo", "")
	if err != nil {
		t.Fatalf("a missing profile must not error: %v", err)
	}
	if got.ProfileName != "nope" {
		t.Errorf("ProfileName = %q — the resolved name must survive so the caller can report it", got.ProfileName)
	}
	if got.StartCmd != "" || len(got.ToolKeys) != 0 {
		t.Errorf("expected an empty recipe, got %+v", got)
	}
}

func TestResolve_StoreErrorsPropagate(t *testing.T) {
	boom := errors.New("db down")
	if _, err := Resolve(context.Background(), &fakeRepos{err: boom}, &fakeProfiles{}, "r", ""); err == nil {
		t.Error("a repo-store error must not be swallowed into the default profile")
	}
	if _, err := Resolve(context.Background(), &fakeRepos{}, &fakeProfiles{err: boom}, "r", ""); err == nil {
		t.Error("a profile-store error must not be swallowed into an empty recipe")
	}
}
