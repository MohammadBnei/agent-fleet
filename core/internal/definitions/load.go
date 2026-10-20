package definitions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// The fleet ceiling (docs/adr/0062 §1). A definition can only narrow within
// these; every rule is judged on the EFFECTIVE spec, so a child cannot widen
// what its parent was allowed.
var (
	knownGroups = []string{"platform-admins", "shared-notes"}
	// ModelCeiling must stay a subset of the console's validModels
	// (dashboard/ceiling_test.go), or a definition could name a model the
	// console would refuse.
	ModelCeiling = []string{"claude-opus-5-5", "claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"}
	// ModeCeiling: the modes that keep canUseTool in the loop. auto is the
	// fleet's auto, not the SDK's: the worker runs a definition session in
	// auto with the SDK in default, so every call that is not auto-safe still
	// reaches canUseTool (docs/adr/0076 §1). Not dontAsk (docs/adr/0067 §4):
	// it never calls canUseTool, and with allowedTools holding only sidecar
	// tools it would deny everything tools.allow lists.
	ModeCeiling = []string{"auto", "default", "plan"}
	// skipsCanUseTool is rejected BY NAME, so the error says why.
	skipsCanUseTool = []string{"acceptEdits", "bypassPermissions"}
	// SDK 0.3.301 EffortLevel.
	effortLevels   = []string{"low", "medium", "high", "xhigh", "max"}
	toolCeiling    = []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash", "TodoWrite"}
	sidecarCeiling = []string{"AskUserQuestion", "send_message", "wait_for_messages", "set_session_meta"}
	serviceHosts   = []string{"github.com", "api.github.com", "api.anthropic.com"}
	interactions   = []string{"ask", "deny"}
	lifecycles     = []string{"resident", "single-shot"}
)

const (
	maxSystemPrompt  = 16 << 10
	maxMilliCPU      = 4000 // = limitRange.max in k8s/core.yaml (repo_test.go)
	maxMemoryMi      = 4096
	maxTurns         = 500
	maxTimeout       = 12 * time.Hour
	maxStartupBudget = 15 * time.Minute
)

var (
	// Also a valid Kubernetes label value: the pod profile labels by it.
	nameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	originRe   = regexp.MustCompile(`^https://([a-z0-9-]+(\.[a-z0-9-]+)*)(:[0-9]{1,5})?$`)
	githubPath = regexp.MustCompile(`^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(\.git)?/\*$`)
	apiPath    = regexp.MustCompile(`^/repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\*$`)
	envKeyRe   = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	repoRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
	skillRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	toolAllow  = regexp.MustCompile(`^([A-Za-z]+)(\((.+)\))?$`)
	toolDeny   = regexp.MustCompile(`^([A-Za-z_]+)(\((.+)\))?$`)
	cpuRe      = regexp.MustCompile(`^([0-9]+)(m?)$`)
	memoryRe   = regexp.MustCompile(`^([0-9]+)(Mi|Gi)$`)
	loginRe    = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`)
)

// Result is one AgentDefinition file after load.
type Result struct {
	Name      string          // metadata.name; the file stem when undecodable
	Own       json.RawMessage // the file's own spec as JSON; nil when undecodable
	Effective *Spec           // nil when Err != nil
	Abstract  bool
	Err       error
}

type parsed struct {
	path string
	raw  map[string]any // the spec node, for the top-level merge
	spec Spec           // the file's own spec
	err  error
}

// Load is pure: Registry.Sync and the CI test both call it. fileErrs holds
// every error with no definition row to live on (a bad path, an abstract
// definition's own error, a client error), so none of them is log-only.
func Load(files map[string][]byte) (results []Result, fileErrs []error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	defs := map[string]*parsed{}
	clients := map[string]string{} // name → path
	clientFiles := map[string]clientFile{}
	clientErrs := map[string]error{}
	for _, p := range paths {
		// packs/ rides along in the same listing (docs/adr/0063).
		if strings.HasSuffix(p, ".md") || strings.HasPrefix(p, "packs/") {
			continue
		}
		// A git path may be any bytes; a name column (and a proto string)
		// may not, and one bad name would fail the whole sync.
		if !utf8.ValidString(p) {
			fileErrs = append(fileErrs, fmt.Errorf("%q: not a definition path (not UTF-8)", p))
			continue
		}
		dir, base := path.Split(p)
		stem, isYAML := strings.CutSuffix(base, ".yaml")
		switch {
		case isYAML && dir == "definitions/":
			defs[stem] = parseDefinition(p, stem, files[p])
		case isYAML && dir == "definitions/clients/":
			var cf clientFile
			err := decodeStrict(files[p], &cf)
			if err == nil {
				err = checkHeader(cf.File, kindClient, stem)
			}
			clients[stem], clientFiles[stem], clientErrs[stem] = p, cf, err
		default:
			fileErrs = append(fileErrs, fmt.Errorf("%s: not a definition path (want definitions/<name>.yaml or definitions/clients/<name>.yaml; *.md is ignored)", p))
		}
	}

	type resolved struct {
		merged map[string]any
		eff    *Spec
		err    error
	}
	memo := map[string]resolved{}
	var resolve func(name string, chain []string) resolved
	resolve = func(name string, chain []string) resolved {
		if r, ok := memo[name]; ok {
			return r
		}
		p := defs[name]
		r := resolved{err: p.err}
		if r.err == nil {
			merged := map[string]any{}
			chain = append(slices.Clone(chain), name)
			if ext := p.spec.Extends; ext != "" {
				if i := slices.Index(chain, ext); i >= 0 {
					r.err = errors.New("extends cycle: " + strings.Join(append(chain[i:], ext), " → "))
				} else if defs[ext] == nil {
					r.err = fmt.Errorf("extends unknown definition %q", ext)
				} else if pr := resolve(ext, chain); pr.err != nil {
					r.err = fmt.Errorf("parent %q: %w", ext, pr.err)
				} else {
					// Top-level key replace: nothing below this level is merged.
					for k, v := range pr.merged {
						if k != "extends" && k != "abstract" {
							merged[k] = v
						}
					}
				}
			}
			if r.err == nil {
				for k, v := range p.raw {
					merged[k] = v
				}
				r.merged = merged
				r.eff, r.err = toSpec(merged)
				if r.err == nil {
					r.err = validate(name, r.eff)
				}
			}
		}
		memo[name] = r
		return r
	}

	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := defs[n]
		r := resolve(n, nil)
		res := Result{Name: n, Abstract: p.spec.Abstract, Effective: r.eff, Err: r.err}
		if p.err == nil {
			if b, err := json.Marshal(p.raw); err == nil {
				res.Own = b
			}
		}
		// Two-sided agreement, the definition's half: every invoker is a
		// loaded client that lists this definition back.
		if res.Err == nil {
			var errs []error
			for _, inv := range res.Effective.Invokers {
				cf, ok := clientFiles[inv]
				switch {
				case !ok || clientErrs[inv] != nil:
					errs = append(errs, fmt.Errorf("invokers: unknown client %q", inv))
				case !slices.Contains(cf.Spec.Definitions, n):
					errs = append(errs, fmt.Errorf("invokers: client %q does not list it in its definitions", inv))
				}
			}
			if len(errs) > 0 {
				res.Err = errors.Join(errs...)
			}
		}
		if res.Err != nil {
			res.Effective = nil
			if res.Abstract {
				fileErrs = append(fileErrs, fmt.Errorf("%s: %w", p.path, res.Err))
			}
		}
		results = append(results, res)
	}

	clientNames := make([]string, 0, len(clients))
	for n := range clients {
		clientNames = append(clientNames, n)
	}
	sort.Strings(clientNames)
	for _, n := range clientNames {
		if err := clientErrs[n]; err != nil {
			fileErrs = append(fileErrs, fmt.Errorf("%s: %w", clients[n], err))
			continue
		}
		if err := validateClient(n, clientFiles[n].Spec, defs, results); err != nil {
			fileErrs = append(fileErrs, fmt.Errorf("%s: %w", clients[n], err))
		}
	}
	return results, fileErrs
}

func parseDefinition(p, stem string, b []byte) *parsed {
	out := &parsed{path: p}
	// Postgres text and jsonb cannot hold U+0000, and yaml.v3 errors echo
	// keys and values verbatim. A NUL would fail the row's upsert and with it
	// the whole sync transaction, so it is refused here, as an ordinary
	// invalid definition that CI also catches. Checked before the typed
	// decode so no error below can carry one.
	var doc any
	if yaml.Unmarshal(b, &doc) == nil && hasNUL(doc) {
		out.err = errors.New(`a NUL character (\0) is not allowed anywhere in a definition`)
		return out
	}
	var df definitionFile
	if err := decodeStrict(b, &df); err != nil {
		out.err = err
		return out
	}
	if err := checkHeader(df.File, kindDefinition, stem); err != nil {
		out.err = err
		return out
	}
	// Decoded a second time, loosely, for the top-level merge. Strictness was
	// already enforced on the typed decode above.
	var node struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &node); err != nil {
		out.err = err
		return out
	}
	// An explicit null would DELETE the parent's value in the top-level
	// merge (`tools: ~` dropping the parent's bound and its deny list), so a
	// child could widen by erasing. Omit a key to inherit it.
	for k, v := range node.Spec {
		if v == nil {
			out.err = fmt.Errorf("spec.%s: null is not allowed (omit the key to inherit the parent's)", k)
			return out
		}
	}
	out.spec, out.raw = df.Spec, node.Spec
	return out
}

// hasNUL reports a NUL in any key or string of a decoded YAML value.
func hasNUL(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case []any:
		return slices.ContainsFunc(v, hasNUL)
	case map[string]any:
		for k, x := range v {
			if hasNUL(k) || hasNUL(x) {
				return true
			}
		}
	case map[any]any:
		for k, x := range v {
			if hasNUL(k) || hasNUL(x) {
				return true
			}
		}
	}
	return false
}

// decodeStrict rejects an unknown key at any depth and a second document.
func decodeStrict(b []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty file")
		}
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("a definitions file must hold exactly one YAML document")
	}
	return nil
}

func checkHeader(f File, kind, stem string) error {
	var errs []error
	if f.APIVersion != apiVersion {
		errs = append(errs, fmt.Errorf("apiVersion %q: want %q", f.APIVersion, apiVersion))
	}
	if f.Kind != kind {
		errs = append(errs, fmt.Errorf("kind %q: want %q in this directory", f.Kind, kind))
	}
	if !nameRe.MatchString(f.Metadata.Name) {
		errs = append(errs, fmt.Errorf("metadata.name %q must match %s", f.Metadata.Name, nameRe))
	}
	if f.Metadata.Name != stem {
		errs = append(errs, fmt.Errorf("metadata.name %q must equal the file stem %q", f.Metadata.Name, stem))
	}
	return errors.Join(errs...)
}

func toSpec(merged map[string]any) (*Spec, error) {
	b, err := yaml.Marshal(merged)
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := decodeStrict(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func validateClient(name string, s ClientSpec, defs map[string]*parsed, results []Result) error {
	var errs []error
	if !envKeyRe.MatchString(s.TokenRef) {
		// Never echo it: this check exists to catch a pasted value.
		errs = append(errs, fmt.Errorf("tokenRef must be a secret KEY name matching %s, never a value (value not shown)", envKeyRe))
	}
	if len(s.Actors.Allow) == 0 {
		errs = append(errs, errors.New("actors.allow must not be empty"))
	}
	for _, a := range s.Actors.Allow {
		if !loginRe.MatchString(a) {
			errs = append(errs, fmt.Errorf("actors.allow: %q is not a GitHub login", a))
		}
	}
	for _, d := range s.Definitions {
		i := slices.IndexFunc(results, func(r Result) bool { return r.Name == d })
		if i < 0 {
			errs = append(errs, fmt.Errorf("definitions: unknown definition %q", d))
			continue
		}
		r := results[i]
		if r.Abstract {
			errs = append(errs, fmt.Errorf("definitions: %q is abstract", d))
			continue
		}
		invokers := defs[d].spec.Invokers
		if r.Effective != nil {
			invokers = r.Effective.Invokers
		}
		if !slices.Contains(invokers, name) {
			errs = append(errs, fmt.Errorf("definitions: definition %q does not list client %q in invokers", d, name))
		}
	}
	return errors.Join(errs...)
}

// validate checks one spec against the fleet ceiling. The registry re-runs it
// on stored rows every sync, so a tightened ceiling revokes old versions.
// Required fields apply only to a non-abstract spec; every other rule applies
// to whatever is present.
func validate(name string, s *Spec) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	need := func(present bool, field string) bool {
		if !present && !s.Abstract {
			bad("%s is required", field)
		}
		return present
	}

	if need(s.Audience != nil, "audience") && !slices.Contains(knownGroups, s.Audience.Group) {
		bad("audience.group %q is not a known group %v", s.Audience.Group, knownGroups)
	}
	for _, inv := range s.Invokers {
		if !nameRe.MatchString(inv) {
			bad("invokers: %q is not a client name", inv)
		}
	}
	if s.Embed != nil {
		seen := map[string]bool{}
		for _, o := range s.Embed.Origins {
			m := originRe.FindStringSubmatch(o)
			if m == nil || (m[1] != "bnei.dev" && !strings.HasSuffix(m[1], ".bnei.dev")) {
				bad("embed.origins: %q is not an exact same-site https:// origin (no path, no trailing /, no *, host bnei.dev or *.bnei.dev)", o)
			}
			if seen[o] {
				bad("embed.origins: duplicate %q", o)
			}
			seen[o] = true
		}
	}
	if c := s.Credentials; c != nil {
		if c.Vault != name {
			bad("credentials.vault %q must equal metadata.name %q", c.Vault, name)
		}
		// docs/adr/0065: the vault's own slug minimum, and the bridge
		// executor's reserved prefix (the provisioner's prune skips it).
		if len(c.Vault) < 3 {
			bad("credentials.vault %q must be at least 3 characters (a vault name)", c.Vault)
		}
		if strings.HasPrefix(c.Vault, "executor-") {
			bad("credentials.vault %q: the executor- prefix is reserved for the event bridge's executor", c.Vault)
		}
		if len(c.Services) == 0 {
			bad("credentials.services must not be empty")
		}
		seen := map[string]bool{}
		for _, svc := range c.Services {
			if !slices.Contains(serviceHosts, svc.Host) {
				bad("credentials.services: host %q is not in %v", svc.Host, serviceHosts)
			}
			if seen[svc.Host] {
				bad("credentials.services: duplicate host %q", svc.Host)
			}
			seen[svc.Host] = true
			if !envKeyRe.MatchString(svc.Secret) {
				bad("credentials.services[%s].secret must be a secret KEY name matching %s (value not shown)", svc.Host, envKeyRe)
			} else if strings.HasPrefix(svc.Secret, "AGENT_VAULT_") || strings.HasPrefix(svc.Secret, "FLEET_VAULT_") || strings.HasPrefix(svc.Secret, "GITHUB_APP_") {
				bad("credentials.services[%s].secret %q is the provisioner's own key, never a definition's", svc.Host, svc.Secret)
			}
			switch svc.Host {
			case "api.anthropic.com":
				if len(svc.Paths) > 0 {
					bad("credentials.services: host api.anthropic.com takes no paths")
				}
				if strings.HasPrefix(svc.Secret, "GH_") {
					bad("credentials.services[api.anthropic.com].secret %q: a GH_ key is a GitHub credential", svc.Secret)
				}
			case "github.com", "api.github.com":
				// docs/adr/0065 §1: a GitHub key is bound to its vault by name,
				// so no definition can name another's PAT (the provisioner
				// re-checks this).
				if want := "GH_" + strings.ToUpper(strings.ReplaceAll(c.Vault, "-", "_")); svc.Secret != want {
					bad("credentials.services[%s].secret must be %s (GH_ + the vault name), not %q", svc.Host, want, svc.Secret)
				}
				if len(svc.Paths) == 0 {
					bad("credentials.services: host %s needs at least one path", svc.Host)
				}
				for _, p := range svc.Paths {
					if !validServicePath(svc.Host, p) {
						bad("credentials.services[%s].paths: %q must be exactly /<owner>/<repo>/* or /<owner>/<repo>.git/* (api.github.com: /repos/<owner>/<repo>/*)", svc.Host, p)
					}
				}
			}
		}
	}
	// docs/adr/0065: after P3 a definition pod's model credential is the
	// vault's, a push credential exists only in push mode, and every GitHub
	// path names the one repo the warm binds repos.url to.
	if !s.Abstract {
		if need(s.Credentials != nil, "credentials") && !slices.ContainsFunc(s.Credentials.Services, func(x Service) bool { return x.Host == "api.anthropic.com" }) {
			bad("credentials need an api.anthropic.com service: a definition pod's model credential is the vault's")
		}
		var gh, git bool
		if s.Credentials != nil {
			for _, x := range s.Credentials.Services {
				gh = gh || x.Host == "github.com" || x.Host == "api.github.com"
				git = git || x.Host == "github.com"
			}
			if len(repoSet(s.Credentials)) > 1 {
				bad("credentials: every github.com / api.github.com path must name one <owner>/<repo>")
			}
		}
		if s.Git != nil {
			if gh && s.Git.Mode != "push" {
				bad("credentials: github.com / api.github.com services need git.mode push (a pr-mode pod holds no push credential)")
			}
			if s.Git.Mode == "push" && !git {
				bad("git.mode push needs a github.com credentials service")
			}
		}
	}
	if need(s.Workspace != nil, "workspace") {
		if s.Workspace.Kind != "repo" {
			bad(`workspace.kind %q: only "repo" is supported`, s.Workspace.Kind)
		}
		if !repoRe.MatchString(s.Workspace.Repo) {
			bad("workspace.repo %q must match %s", s.Workspace.Repo, repoRe)
		}
	}
	if need(s.Git != nil, "git") {
		g := s.Git
		if g.Mode != "pr" && g.Mode != "push" {
			bad(`git.mode %q: want "pr" or "push"`, g.Mode)
		}
		if g.Mode == "push" && g.Branch == "" {
			bad("git.branch is required when git.mode is push")
		}
		if g.Identity == nil {
			bad("git.identity is required")
		} else {
			if g.Identity.Name == "" || strings.IndexFunc(g.Identity.Name, unicode.IsControl) >= 0 {
				bad("git.identity.name must be non-empty with no control characters")
			}
			if !emailRe.MatchString(g.Identity.Email) {
				bad("git.identity.email %q is not an email address", g.Identity.Email)
			}
		}
	}
	if need(s.Model != nil, "model") {
		checkAllow(&errs, "model", s.Model.Default, s.Model.Allow, ModelCeiling, nil)
	}
	if need(s.Thinking != nil, "thinking") && !slices.Contains(effortLevels, s.Thinking.Default) {
		bad("thinking.default %q is not an effort level %v", s.Thinking.Default, effortLevels)
	}
	if need(s.Mode != nil, "mode") {
		checkAllow(&errs, "mode", s.Mode.Default, s.Mode.Allow, ModeCeiling, skipsCanUseTool)
	}
	if s.SystemPrompt != nil && len(s.SystemPrompt.Append) > maxSystemPrompt {
		bad("systemPrompt.append is %d bytes, over %d", len(s.SystemPrompt.Append), maxSystemPrompt)
	}
	seenSkill := map[string]bool{}
	for _, sk := range s.Skills {
		if !skillRe.MatchString(sk) || seenSkill[sk] {
			bad("skills: %q must match %s, once", sk, skillRe)
		}
		seenSkill[sk] = true
	}
	// tools, interaction and lifecycle are required: an absent bound is no
	// bound (nil tools would mean the SDK's whole default tool set).
	if need(s.Tools != nil, "tools") {
		if len(s.Tools.Allow) == 0 && !s.Abstract {
			bad("tools.allow must not be empty")
		}
		for _, t := range s.Tools.Allow {
			m := toolAllow.FindStringSubmatch(t)
			switch {
			case m == nil:
				bad("tools.allow: %q is not Tool or Tool(specifier)", t)
			case !slices.Contains(toolCeiling, m[1]):
				bad("tools.allow: %q is not in the tool ceiling %v", t, toolCeiling)
			case m[1] == "Bash" && !validBashSpecifier(m[3]):
				bad("tools.allow: %q: Bash needs a literal command prefix (`Bash(git add:*)`): no other *, no leading space", t)
			}
		}
		for _, t := range s.Tools.Deny {
			if !toolDeny.MatchString(t) {
				bad("tools.deny: %q is not Tool or Tool(specifier)", t)
			}
		}
	}
	if need(len(s.SidecarTools) > 0, "sidecarTools") {
		for _, t := range s.SidecarTools {
			if !slices.Contains(sidecarCeiling, t) {
				bad("sidecarTools: %q is not in %v", t, sidecarCeiling)
			}
		}
	}
	if need(s.Interaction != nil, "interaction") {
		checkList(&errs, "interaction", s.Interaction.Allow, interactions)
	}
	if need(s.Lifecycle != nil, "lifecycle") {
		checkList(&errs, "lifecycle", s.Lifecycle.Allow, lifecycles)
	}
	if o := s.Owns; o != nil {
		shapeList(&errs, "owns.repos", o.Repos, "")
		shapeList(&errs, "owns.components", o.Components, "")
		shapeList(&errs, "owns.subjects", o.Subjects, "")
		shapeList(&errs, "owns.types", o.Types, "")
	}
	if sub := s.Subscribes; sub != nil {
		// Subjects are the bridge's globs (* and ?), types are prefixes.
		shapeList(&errs, "subscribes.subjects", sub.Subjects, "[]{}\\")
		shapeList(&errs, "subscribes.types", sub.Types, "*?[]{}\\")
	}
	if r := s.Resources; r != nil {
		if m := cpuRe.FindStringSubmatch(r.CPU); m == nil {
			bad("resources.cpu %q must be cores or millicores", r.CPU)
		} else if n, _ := strconv.Atoi(m[1]); (m[2] == "" && n*1000 > maxMilliCPU) || (m[2] == "m" && n > maxMilliCPU) {
			bad("resources.cpu %q is over %dm", r.CPU, maxMilliCPU)
		}
		if m := memoryRe.FindStringSubmatch(r.Memory); m == nil {
			bad("resources.memory %q must be <n>Mi or <n>Gi", r.Memory)
		} else if n, _ := strconv.Atoi(m[1]); (m[2] == "Gi" && n*1024 > maxMemoryMi) || (m[2] == "Mi" && n > maxMemoryMi) {
			bad("resources.memory %q is over %dMi", r.Memory, maxMemoryMi)
		}
	}
	if need(s.Limits != nil, "limits") {
		l := s.Limits
		if l.MaxTurns < 1 || l.MaxTurns > maxTurns {
			bad("limits.maxTurns %d must be 1-%d", l.MaxTurns, maxTurns)
		}
		if d, err := time.ParseDuration(l.Timeout); err != nil || d <= 0 || d > maxTimeout {
			bad("limits.timeout %q must be a duration in (0, %s]", l.Timeout, maxTimeout)
		}
		if d, err := time.ParseDuration(l.StartupBudget); err != nil || d <= 0 || d > maxStartupBudget {
			bad("limits.startupBudget %q must be a duration in (0, %s]", l.StartupBudget, maxStartupBudget)
		}
		if l.Concurrency < 1 {
			bad("limits.concurrency %d must be >= 1", l.Concurrency)
		}
	}
	return errors.Join(errs...)
}

func checkAllow(errs *[]error, field, def string, allow, ceiling, byName []string) {
	if len(allow) == 0 {
		*errs = append(*errs, fmt.Errorf("%s.allow must not be empty", field))
	}
	for _, v := range allow {
		switch {
		case slices.Contains(byName, v):
			*errs = append(*errs, fmt.Errorf("%s.allow: %q skips canUseTool and is never allowed for a definition", field, v))
		case !slices.Contains(ceiling, v):
			*errs = append(*errs, fmt.Errorf("%s.allow: %q is not in the fleet ceiling %v", field, v, ceiling))
		}
	}
	if !slices.Contains(allow, def) {
		*errs = append(*errs, fmt.Errorf("%s.default %q is not in %s.allow", field, def, field))
	}
}

func checkList(errs *[]error, field string, allow, ceiling []string) {
	if len(allow) == 0 {
		*errs = append(*errs, fmt.Errorf("%s.allow must not be empty", field))
	}
	for _, v := range allow {
		if !slices.Contains(ceiling, v) {
			*errs = append(*errs, fmt.Errorf("%s.allow: %q is not in %v", field, v, ceiling))
		}
	}
}

// validBashSpecifier: a literal command, optionally ending in one ":*". Any
// other * (`Bash(**)`, `Bash(*:*)`) or a leading space (`Bash( *)`) is
// unscoped Bash by another name.
//
// ponytail: shape only, a typo guard and not a sandbox. `Bash(sh:*)`,
// `Bash(make:*)` and `Bash(git push:*)` (`--receive-pack`) all pass, and no
// denylist of command words could be complete. The gate is canUseTool, which
// asks on every Bash match until P4's sidecar allowlist and argument check
// exist (docs/adr/0062 §7).
func validBashSpecifier(spec string) bool {
	cmd := strings.TrimSuffix(spec, ":*")
	return cmd != "" && cmd == strings.TrimSpace(cmd) && !strings.Contains(cmd, "*")
}

// shapeList: non-empty strings, no duplicates, none of the forbidden runes or
// whitespace.
func shapeList(errs *[]error, field string, list []string, forbidden string) {
	seen := map[string]bool{}
	for _, v := range list {
		switch {
		case v == "":
			*errs = append(*errs, fmt.Errorf("%s: empty string", field))
		case seen[v]:
			*errs = append(*errs, fmt.Errorf("%s: duplicate %q", field, v))
		case strings.ContainsAny(v, forbidden) || strings.IndexFunc(v, unicode.IsSpace) >= 0:
			*errs = append(*errs, fmt.Errorf("%s: %q contains a character outside the allowed pattern syntax", field, v))
		}
		seen[v] = true
	}
}

// validServicePath enforces spike 3: the vault matches by prefix with no
// normalisation, so a path must be anchored, end in exactly one "/*", and
// carry no "." / ".." segment and no "%".
func validServicePath(host, p string) bool {
	re := githubPath
	if host == "api.github.com" {
		re = apiPath
	}
	if !re.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// repoSet is every "<owner>/<repo>" a spec's well-shaped GitHub paths name.
func repoSet(c *Credentials) map[string]bool {
	out := map[string]bool{}
	if c == nil {
		return out
	}
	for _, svc := range c.Services {
		for _, p := range svc.Paths {
			if !validServicePath(svc.Host, p) {
				continue
			}
			seg := strings.Split(p, "/") // "", owner, repo[.git], "*"
			switch svc.Host {
			case "api.github.com":
				seg = seg[1:] // "", "repos", owner, repo, "*"
			case "github.com":
			default:
				continue
			}
			out[seg[1]+"/"+strings.TrimSuffix(seg[2], ".git")] = true
		}
	}
	return out
}

// GitHubRepo is the one "<owner>/<repo>" a valid spec's GitHub credentials
// name, "" when it has none (docs/adr/0065 §6: the warm binds repos.url to it).
func GitHubRepo(c *Credentials) string {
	for r := range repoSet(c) {
		return r
	}
	return ""
}
