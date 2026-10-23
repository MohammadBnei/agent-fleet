package definitions

import (
	"sort"
	"strings"
	"testing"
)

const baseYAML = `apiVersion: fleet.bnei.dev/v1alpha1
kind: AgentDefinition
metadata: { name: base }
spec:
  abstract: true
  model: { default: claude-opus-5, allow: [claude-opus-5] }
  thinking: { default: medium }
  mode: { default: default, allow: [default, plan] }
  tools: { allow: [Read], deny: ["Edit(.git/**)"] }
  sidecarTools: [AskUserQuestion]
  interaction: { allow: [ask] }
  lifecycle: { allow: [resident] }
  limits: { maxTurns: 10, timeout: 30m, concurrency: 1, startupBudget: 3m }
`

// childYAML renders a definition extending base. kv overrides a top-level
// spec key; an empty value drops it. Keys are rendered once each, because
// YAML itself rejects a duplicate mapping key.
func childYAML(name string, kv map[string]string) string {
	spec := map[string]string{
		"extends":   "base",
		"audience":  "{ group: platform-admins }",
		"workspace": "{ kind: repo, repo: dream-analyst }",
		"git":       "{ mode: pr, identity: { name: a, email: a@b.c } }",
		// docs/adr/0065: the model credential is the vault's.
		"credentials": "{ vault: " + name + ", services: [{ host: api.anthropic.com, secret: CLAUDE_CODE_OAUTH_TOKEN }] }",
	}
	for k, v := range kv {
		spec[k] = v
	}
	keys := make([]string, 0, len(spec))
	for k := range spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("apiVersion: fleet.bnei.dev/v1alpha1\nkind: AgentDefinition\nmetadata: { name: " + name + " }\nspec:\n")
	for _, k := range keys {
		if spec[k] != "" {
			b.WriteString("  " + k + ": " + spec[k] + "\n")
		}
	}
	return b.String()
}

func load(t *testing.T, files map[string]string) (map[string]Result, []error) {
	t.Helper()
	in := map[string][]byte{}
	for p, c := range files {
		in[p] = []byte(c)
	}
	results, fileErrs := Load(in)
	out := map[string]Result{}
	for _, r := range results {
		out[r.Name] = r
	}
	return out, fileErrs
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// One child of a valid base per row; want "" means valid, else a substring of
// the child's error. Every rule is judged on the EFFECTIVE spec, so a widening
// in a child of a valid parent must still fail.
func TestLoad_ChildRules(t *testing.T) {
	const push = "{ mode: push, branch: main, identity: { name: a, email: a@b.c } }"
	cases := []struct {
		name string
		kv   map[string]string
		want string
	}{
		{"valid", nil, ""},
		// widening at the effective spec
		// docs/adr/0076 §1: auto is the fleet's, the worker runs the SDK in default.
		{"auto is accepted", map[string]string{"mode": "{ default: auto, allow: [auto, default, plan] }"}, ""},
		{"acceptEdits by name", map[string]string{"mode": "{ default: default, allow: [default, acceptEdits] }"}, `"acceptEdits" skips canUseTool`},
		{"bypassPermissions by name", map[string]string{"mode": "{ default: default, allow: [bypassPermissions, default] }"}, `"bypassPermissions" skips canUseTool`},
		{"dontAsk outside the ceiling", map[string]string{"mode": "{ default: default, allow: [default, dontAsk] }"}, `"dontAsk" is not in the fleet ceiling`},
		{"mode default not allowed", map[string]string{"mode": "{ default: plan, allow: [default] }"}, "mode.default"},
		{"tool outside ceiling", map[string]string{"tools": "{ allow: [WebFetch] }"}, "WebFetch"},
		{"bare Bash", map[string]string{"tools": "{ allow: [Bash] }"}, "literal command prefix"},
		{"Bash star", map[string]string{"tools": `{ allow: ["Bash(*)"] }`}, "literal command prefix"},
		{"Bash colon star", map[string]string{"tools": `{ allow: ["Bash(:*)"] }`}, "literal command prefix"},
		{"Bash with prefix ok", map[string]string{"tools": `{ allow: ["Bash(git add:*)"] }`}, ""},
		{"Bash double star", map[string]string{"tools": `{ allow: ["Bash(**)"] }`}, "literal command prefix"},
		{"Bash star colon star", map[string]string{"tools": `{ allow: ["Bash(*:*)"] }`}, "literal command prefix"},
		{"Bash leading space", map[string]string{"tools": `{ allow: ["Bash( *)"] }`}, "literal command prefix"},
		{"Bash inner star", map[string]string{"tools": `{ allow: ["Bash(git * push:*)"] }`}, "literal command prefix"},
		{"Bash exact command ok", map[string]string{"tools": `{ allow: ["Bash(git status)"] }`}, ""},
		// erasing a parent's bound: null and absence both fail
		{"tools null", map[string]string{"tools": "~"}, "spec.tools: null is not allowed"},
		{"interaction null", map[string]string{"interaction": "~"}, "spec.interaction: null"},
		{"lifecycle null", map[string]string{"lifecycle": "null"}, "spec.lifecycle: null"},
		{"resources null", map[string]string{"resources": "~"}, "spec.resources: null"},
		{"tools empty allow", map[string]string{"tools": "{ allow: [] }"}, "tools.allow must not be empty"},
		{"sidecar tool outside ceiling", map[string]string{"sidecarTools": "[expose]"}, "expose"},
		{"thinking ultra", map[string]string{"thinking": "{ default: ultra }"}, "thinking.default"},
		{"thinking xhigh", map[string]string{"thinking": "{ default: xhigh }"}, ""},
		{"model outside ceiling", map[string]string{"model": "{ default: gpt-5, allow: [gpt-5] }"}, "gpt-5"},
		{"model default not allowed", map[string]string{"model": "{ default: claude-sonnet-5, allow: [claude-opus-5] }"}, "model.default"},
		{"unknown group", map[string]string{"audience": "{ group: everyone }"}, "audience.group"},
		{"missing audience", map[string]string{"audience": ""}, "audience is required"},
		{"missing workspace", map[string]string{"workspace": ""}, "workspace is required"},
		{"scratch workspace", map[string]string{"workspace": "{ kind: scratch, repo: x }"}, "workspace.kind"},
		{"bad repo name", map[string]string{"workspace": "{ kind: repo, repo: ../x }"}, "workspace.repo"},
		{"push without branch", map[string]string{"git": "{ mode: push, identity: { name: a, email: a@b.c } }"}, "git.branch"},
		{"bad git mode", map[string]string{"git": "{ mode: merge, identity: { name: a, email: a@b.c } }"}, "git.mode"},
		{"bad email", map[string]string{"git": "{ mode: pr, identity: { name: a, email: nope } }"}, "identity.email"},
		{"no identity", map[string]string{"git": "{ mode: pr }"}, "git.identity"},
		// embed origins
		{"origin http", map[string]string{"embed": `{ origins: ["http://opus.bnei.dev"] }`}, "embed.origins"},
		{"origin path", map[string]string{"embed": `{ origins: ["https://opus.bnei.dev/x"] }`}, "embed.origins"},
		{"origin trailing slash", map[string]string{"embed": `{ origins: ["https://opus.bnei.dev/"] }`}, "embed.origins"},
		{"origin wildcard", map[string]string{"embed": `{ origins: ["https://*.bnei.dev"] }`}, "embed.origins"},
		{"origin other site", map[string]string{"embed": `{ origins: ["https://evil.com"] }`}, "embed.origins"},
		{"origin suffix trick", map[string]string{"embed": `{ origins: ["https://evilbnei.dev"] }`}, "embed.origins"},
		{"origin userinfo", map[string]string{"embed": `{ origins: ["https://u@opus.bnei.dev"] }`}, "embed.origins"},
		{"origin duplicate", map[string]string{"embed": `{ origins: ["https://opus.bnei.dev", "https://opus.bnei.dev"] }`}, "duplicate"},
		{"origin ok", map[string]string{"embed": `{ origins: ["https://opus.bnei.dev"] }`}, ""},
		{"origin port ok", map[string]string{"embed": `{ origins: ["https://opus.bnei.dev:8443"] }`}, ""},
		// credentials
		{"vault not name", map[string]string{"credentials": "{ vault: other, services: [{ host: api.anthropic.com, secret: A }] }"}, "credentials.vault"},
		{"no services", map[string]string{"credentials": "{ vault: valid-child, services: [] }"}, "credentials.services"},
		{"wildcard host", map[string]string{"credentials": `{ vault: valid-child, services: [{ host: "*.github.com", paths: ["/o/r/*"], secret: A }] }`}, "host"},
		{"unknown host", map[string]string{"credentials": `{ vault: valid-child, services: [{ host: gitlab.com, paths: ["/o/r/*"], secret: A }] }`}, "gitlab.com"},
		{"duplicate host", map[string]string{"credentials": `{ vault: valid-child, services: [{ host: api.anthropic.com, secret: A }, { host: api.anthropic.com, secret: B }] }`}, "duplicate"},
		{"lowercase secret", map[string]string{"credentials": "{ vault: valid-child, services: [{ host: api.anthropic.com, secret: abc }] }"}, "secret"},
		{"anthropic with paths", map[string]string{"credentials": `{ vault: valid-child, services: [{ host: api.anthropic.com, paths: ["/v1/*"], secret: A }] }`}, "no paths"},
		{"github without paths", map[string]string{"credentials": "{ vault: valid-child, services: [{ host: github.com, secret: A }] }"}, "at least one path"},
		{"credentials ok", map[string]string{"git": push, "credentials": `{ vault: valid-child, services: [{ host: github.com, paths: ["/o/r/*", "/o/r.git/*"], secret: GH_VALID_CHILD }, { host: api.github.com, paths: ["/repos/o/r/*"], secret: GH_VALID_CHILD }, { host: api.anthropic.com, secret: B }] }`}, ""},
		{"another vault's PAT", map[string]string{"git": push, "credentials": `{ vault: valid-child, services: [{ host: github.com, paths: ["/o/r/*"], secret: GH_OTHER }, { host: api.anthropic.com, secret: B }] }`}, "must be GH_VALID_CHILD"},
		{"PAT to the model host", map[string]string{"credentials": "{ vault: valid-child, services: [{ host: api.anthropic.com, secret: GH_VALID_CHILD }] }"}, "GitHub credential"},
		// docs/adr/0065
		{"no credentials", map[string]string{"credentials": ""}, "credentials is required"},
		{"no anthropic", map[string]string{"git": push, "credentials": `{ vault: valid-child, services: [{ host: github.com, paths: ["/o/r/*"], secret: A }] }`}, "api.anthropic.com service"},
		{"agent token key", map[string]string{"credentials": "{ vault: valid-child, services: [{ host: api.anthropic.com, secret: AGENT_VAULT_TOKEN }] }"}, "provisioner's own"},
		{"provisioner key", map[string]string{"credentials": "{ vault: valid-child, services: [{ host: api.anthropic.com, secret: FLEET_VAULT_GIT_USERNAME }] }"}, "provisioner's own"},
		{"github app key", map[string]string{"credentials": "{ vault: valid-child, services: [{ host: api.anthropic.com, secret: GITHUB_APP_PRIVATE_KEY }] }"}, "provisioner's own"},
		{"github in pr mode", map[string]string{"credentials": `{ vault: valid-child, services: [{ host: github.com, paths: ["/o/r/*"], secret: A }, { host: api.anthropic.com, secret: B }] }`}, "git.mode push"},
		{"api.github in pr mode", map[string]string{"credentials": `{ vault: valid-child, services: [{ host: api.github.com, paths: ["/repos/o/r/*"], secret: A }, { host: api.anthropic.com, secret: B }] }`}, "git.mode push"},
		{"push without github", map[string]string{"git": push}, "git.mode push needs a github.com"},
		{"two repos", map[string]string{"git": push, "credentials": `{ vault: valid-child, services: [{ host: github.com, paths: ["/o/r/*", "/o/x.git/*"], secret: A }, { host: api.anthropic.com, secret: B }] }`}, "one <owner>/<repo>"},
		{"two repos-owners", map[string]string{"git": push, "credentials": `{ vault: valid-child, services: [{ host: github.com, paths: ["/reposx/y/*", "/reposz/y/*"], secret: A }, { host: api.anthropic.com, secret: B }] }`}, "one <owner>/<repo>"},
		{"api on another repo", map[string]string{"git": push, "credentials": `{ vault: valid-child, services: [{ host: github.com, paths: ["/o/r/*"], secret: A }, { host: api.github.com, paths: ["/repos/o/x/*"], secret: A }, { host: api.anthropic.com, secret: B }] }`}, "one <owner>/<repo>"},
		// owns / subscribes: shape only
		{"owns empty string", map[string]string{"owns": `{ repos: [""] }`}, "owns.repos"},
		{"owns duplicate", map[string]string{"owns": "{ repos: [a, a] }"}, "owns.repos"},
		{"owns non-string", map[string]string{"owns": "{ repos: [{ a: b }] }"}, "cannot unmarshal"},
		{"subscribes glob ok", map[string]string{"subscribes": `{ types: [dev.bnei.github.pull_request], subjects: ["github/opus-tech-hq/*", "x?"] }`}, ""},
		{"subscribes bad glob", map[string]string{"subscribes": `{ subjects: ["github/[ab]"] }`}, "subscribes.subjects"},
		{"subscribes type glob", map[string]string{"subscribes": `{ types: ["github.*"] }`}, "subscribes.types"},
		{"subscribes duplicate", map[string]string{"subscribes": "{ types: [a, a] }"}, "subscribes.types"},
		// human-facing events (infra-bootstrap ADR-0066 §4, docs/adr/0078 §4)
		{"subscribes no types", map[string]string{"subscribes": `{ subjects: ["github/x/*"] }`}, "subscribes.types must not be empty"},
		{"subscribes session type", map[string]string{"subscribes": `{ types: [dev.bnei.fleet.session.waiting] }`}, "human-facing types"},
		{"subscribes game type", map[string]string{"subscribes": `{ types: [dev.bnei.game.] }`}, "human-facing types"},
		{"subscribes too wide a type", map[string]string{"subscribes": `{ types: [dev.bnei.] }`}, "human-facing types"},
		{"subscribes fleet subject", map[string]string{"subscribes": `{ types: [dev.bnei.github.], subjects: ["fleet/*"] }`}, "human-facing subjects"},
		{"subscribes match-all subject ok", map[string]string{"subscribes": `{ types: [dev.bnei.github.], subjects: ["*"] }`}, ""},
		{"subscribes subject aimed at game", map[string]string{"subscribes": `{ types: [dev.bnei.github.], subjects: ["g*"] }`}, "human-facing subjects"},
		{"owns game type", map[string]string{"owns": `{ types: [dev.bnei.game.needs_you.] }`}, "owns.types"},
		{"owns fleet subject", map[string]string{"owns": `{ subjects: ["fleet/client/game/*"] }`}, "owns.subjects"},
		{"owns repos without types ok", map[string]string{"owns": `{ repos: [x] }`}, ""},
		// resources / limits / misc
		{"cpu over ceiling", map[string]string{"resources": `{ cpu: "8", memory: 1Gi }`}, "resources.cpu"},
		{"memory over ceiling", map[string]string{"resources": "{ cpu: 500m, memory: 8Gi }"}, "resources.memory"},
		{"resources ok", map[string]string{"resources": "{ cpu: 4000m, memory: 4096Mi }"}, ""},
		{"bad timeout", map[string]string{"limits": "{ maxTurns: 10, timeout: forever, concurrency: 1, startupBudget: 3m }"}, "limits.timeout"},
		{"timeout too long", map[string]string{"limits": "{ maxTurns: 10, timeout: 13h, concurrency: 1, startupBudget: 3m }"}, "limits.timeout"},
		{"startup too long", map[string]string{"limits": "{ maxTurns: 10, timeout: 1h, concurrency: 1, startupBudget: 16m }"}, "limits.startupBudget"},
		{"zero concurrency", map[string]string{"limits": "{ maxTurns: 10, timeout: 1h, concurrency: 0, startupBudget: 3m }"}, "limits.concurrency"},
		{"maxTurns too high", map[string]string{"limits": "{ maxTurns: 501, timeout: 1h, concurrency: 1, startupBudget: 3m }"}, "limits.maxTurns"},
		{"bad skill", map[string]string{"skills": "[Bad_Skill]"}, "skills"},
		{"interaction outside ceiling", map[string]string{"interaction": "{ allow: [ask, approve] }"}, "interaction"},
		{"lifecycle outside ceiling", map[string]string{"lifecycle": "{ allow: [forever] }"}, "lifecycle"},
		{"agents key unknown", map[string]string{"agents": "[x]"}, "field agents not found"},
		{"output key unknown", map[string]string{"output": "{ x: y }"}, "field output not found"},
		// NUL: Postgres text/jsonb cannot store it, so it must be an invalid
		// row, not a failed sync transaction.
		{"NUL in a value", map[string]string{"systemPrompt": `{ append: "x\0y" }`}, "NUL"},
		{"NUL in a deny entry", map[string]string{"tools": `{ allow: [Read], deny: ["Edit(\x00)"] }`}, "NUL"},
		{"NUL in a key", map[string]string{"git": `{ mode: pr, identity: { name: a, email: a@b.c, "\u0000": 1 } }`}, "NUL"},
		{"NUL echoed by a type error", map[string]string{"limits": `{ maxTurns: "\0", timeout: 1h, concurrency: 1, startupBudget: 3m }`}, "NUL"},
		{"nested unknown key", map[string]string{"git": "{ mode: pr, identity: { name: a, email: a@b.c, foo: 1 } }"}, "field foo not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fileErrs := load(t, map[string]string{
				"definitions/base.yaml":        baseYAML,
				"definitions/valid-child.yaml": childYAML("valid-child", tc.kv),
			})
			if len(fileErrs) != 0 {
				t.Fatalf("file errors: %v", fileErrs)
			}
			r := got["valid-child"]
			msg := errText(r.Err)
			if tc.want == "" {
				if r.Err != nil || r.Effective == nil {
					t.Fatalf("want valid, got %v", r.Err)
				}
				return
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("want error containing %q, got %q", tc.want, msg)
			}
			if r.Effective != nil {
				t.Fatal("an invalid definition must have no Effective spec")
			}
		})
	}
}

func TestLoad_ExtendsTopLevelReplace(t *testing.T) {
	got, _ := load(t, map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/cee.yaml": childYAML("cee", map[string]string{
			"tools": "{ allow: [Read, Grep] }",
			"mode":  "{ default: plan, allow: [plan] }",
		}),
	})
	r := got["cee"]
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.Effective.Tools.Deny) != 0 {
		t.Fatalf("child tools must replace the parent's whole tools, deny included: %v", r.Effective.Tools.Deny)
	}
	if got := r.Effective.Mode.Allow; len(got) != 1 || got[0] != "plan" {
		t.Fatalf("child mode must replace the parent's: %v", got)
	}
	if r.Abstract || r.Effective.Abstract {
		t.Fatal("abstract must not be inherited")
	}
	if r.Effective.Limits == nil || r.Effective.Limits.MaxTurns != 10 {
		t.Fatal("untouched keys are inherited")
	}
	if !got["base"].Abstract {
		t.Fatal("base is abstract")
	}
	if string(r.Own) == "" || strings.Contains(string(r.Own), "maxTurns") {
		t.Fatalf("Own is the file's own spec, not the merged one: %s", r.Own)
	}
}

func TestLoad_ExtendsErrors(t *testing.T) {
	t.Run("unknown parent", func(t *testing.T) {
		got, _ := load(t, map[string]string{"definitions/cee.yaml": childYAML("cee", map[string]string{"extends": "nope"})})
		if !strings.Contains(errText(got["cee"].Err), `extends unknown definition "nope"`) {
			t.Fatal(got["cee"].Err)
		}
	})
	t.Run("self cycle", func(t *testing.T) {
		got, _ := load(t, map[string]string{"definitions/cee.yaml": childYAML("cee", map[string]string{"extends": "cee"})})
		if !strings.Contains(errText(got["cee"].Err), "extends cycle: cee → cee") {
			t.Fatal(got["cee"].Err)
		}
	})
	t.Run("three cycle", func(t *testing.T) {
		got, _ := load(t, map[string]string{
			"definitions/a.yaml":   childYAML("a", map[string]string{"extends": "b"}),
			"definitions/b.yaml":   childYAML("b", map[string]string{"extends": "cee"}),
			"definitions/cee.yaml": childYAML("cee", map[string]string{"extends": "a"}),
			"definitions/d.yaml":   childYAML("d", map[string]string{"extends": "a"}),
		})
		for _, n := range []string{"a", "b", "cee"} {
			if !strings.Contains(errText(got[n].Err), "extends cycle") {
				t.Fatalf("%s: %v", n, got[n].Err)
			}
		}
		// d is not in the cycle, only extends a member: blamed as the parent's error.
		if e := errText(got["d"].Err); !strings.HasPrefix(e, `parent "a": `) || !strings.Contains(e, "extends cycle") {
			t.Fatalf("d: %v", e)
		}
	})
	t.Run("parent error propagates", func(t *testing.T) {
		bad := strings.Replace(baseYAML, "allow: [default, plan]", "allow: [default, bypassPermissions]", 1)
		got, fileErrs := load(t, map[string]string{
			"definitions/base.yaml": bad,
			"definitions/cee.yaml":  childYAML("cee", nil),
		})
		if !strings.Contains(errText(got["cee"].Err), `parent "base"`) {
			t.Fatal(got["cee"].Err)
		}
		if len(fileErrs) != 1 || !strings.Contains(fileErrs[0].Error(), "definitions/base.yaml") {
			t.Fatalf("an abstract definition's error has no row, so it must be a file error: %v", fileErrs)
		}
	})
	t.Run("abstract without required fields is fine", func(t *testing.T) {
		got, fileErrs := load(t, map[string]string{"definitions/base.yaml": baseYAML})
		if got["base"].Err != nil || len(fileErrs) != 0 {
			t.Fatal(got["base"].Err, fileErrs)
		}
	})
}

func TestLoad_FileStrictness(t *testing.T) {
	valid := childYAML("cee", nil)
	cases := []struct {
		name, path, body, want string
	}{
		{"unknown top-level key", "definitions/cee.yaml", valid + "extra: 1\n", "field extra not found"},
		{"unknown spec key", "definitions/cee.yaml", strings.Replace(valid, "spec:\n", "spec:\n  colour: red\n", 1), "field colour not found"},
		{"two documents", "definitions/cee.yaml", valid + "---\n" + valid, "one YAML document"},
		{"wrong apiVersion", "definitions/cee.yaml", strings.Replace(valid, "v1alpha1", "v1", 1), "apiVersion"},
		{"wrong kind", "definitions/cee.yaml", strings.Replace(valid, "AgentDefinition", "AgentClient", 1), "kind"},
		{"name not stem", "definitions/d.yaml", valid, `metadata.name "cee" must equal the file stem "d"`},
		{"bad name", "definitions/C_1.yaml", strings.Replace(valid, "name: c }", "name: C_1 }", 1), "metadata.name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"definitions/base.yaml": baseYAML, tc.path: tc.body}
			got, fileErrs := load(t, files)
			var msgs []string
			for _, r := range got {
				msgs = append(msgs, errText(r.Err))
			}
			for _, e := range fileErrs {
				msgs = append(msgs, e.Error())
			}
			if !strings.Contains(strings.Join(msgs, "\n"), tc.want) {
				t.Fatalf("want %q in %q", tc.want, msgs)
			}
		})
	}
}

func TestLoad_Paths(t *testing.T) {
	_, fileErrs := load(t, map[string]string{
		"definitions/README.md":         "docs are ignored",
		"definitions/clients/README.md": "so are these",
		"definitions/typo.yml":          "x",
		"definitions/nested/x.yaml":     "x",
		"definitions/\xff.yaml":         "x", // git allows it; a TEXT name column does not
	})
	if len(fileErrs) != 3 {
		t.Fatalf("want 3 path errors (.yml, nested, not UTF-8), got %v", fileErrs)
	}
}

const clientYAML = `apiVersion: fleet.bnei.dev/v1alpha1
kind: AgentClient
metadata: { name: bridge }
spec:
  tokenRef: FLEET_CLIENT_TOKEN_BRIDGE
  actors: { allow: [MohammadBnei] }
  definitions: [%s]
`

func clientWith(defs, tokenRef, actors string) string {
	s := strings.Replace(clientYAML, "%s", defs, 1)
	if tokenRef != "" {
		s = strings.Replace(s, "FLEET_CLIENT_TOKEN_BRIDGE", tokenRef, 1)
	}
	if actors != "" {
		s = strings.Replace(s, "[MohammadBnei]", actors, 1)
	}
	return s
}

func TestLoad_Clients(t *testing.T) {
	invoked := childYAML("cee", map[string]string{"invokers": "[bridge]"})
	cases := []struct {
		name         string
		client, def  string
		wantFile     string // substring in fileErrs
		wantDefError string // substring in c's error
	}{
		{"agreement", clientWith("cee", "", ""), invoked, "", ""},
		{"empty stub", clientWith("", "", ""), childYAML("cee", nil), "", ""},
		{"invoker missing", clientWith("cee", "", ""), childYAML("cee", nil), `does not list client "bridge" in invokers`, ""},
		{"client does not list it back", clientWith("", "", ""), invoked, "", `client "bridge" does not list it`},
		{"unknown client", clientWith("", "", ""), childYAML("cee", map[string]string{"invokers": "[ci]"}), "", `unknown client "ci"`},
		{"unknown definition", clientWith("cee, ghost", "", ""), invoked, `unknown definition "ghost"`, ""},
		{"bad tokenRef", clientWith("cee", "secret-value", ""), invoked, "tokenRef", ""},
		{"empty actors", clientWith("cee", "", "[]"), invoked, "actors.allow", ""},
		{"bad actor", clientWith("cee", "", "[not a login]"), invoked, "actors.allow", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fileErrs := load(t, map[string]string{
				"definitions/base.yaml":           baseYAML,
				"definitions/cee.yaml":            tc.def,
				"definitions/clients/bridge.yaml": tc.client,
			})
			var fe []string
			for _, e := range fileErrs {
				fe = append(fe, e.Error())
			}
			joined := strings.Join(fe, "\n")
			if tc.wantFile == "" && joined != "" {
				t.Fatalf("unexpected file errors: %s", joined)
			}
			if !strings.Contains(joined, tc.wantFile) {
				t.Fatalf("want %q in file errors %q", tc.wantFile, joined)
			}
			msg := errText(got["cee"].Err)
			if tc.wantDefError == "" && msg != "" {
				t.Fatalf("unexpected definition error: %s", msg)
			}
			if !strings.Contains(msg, tc.wantDefError) {
				t.Fatalf("want %q in %q", tc.wantDefError, msg)
			}
		})
	}
}

// validate is what the registry re-runs on stored rows; it must reject what
// Load rejects even when handed a Spec directly.
func TestValidate_RejectsBypassOnAStoredSpec(t *testing.T) {
	got, _ := load(t, map[string]string{"definitions/base.yaml": baseYAML, "definitions/cee.yaml": childYAML("cee", nil)})
	s := *got["cee"].Effective
	s.Mode = &Mode{Default: "bypassPermissions", Allow: []string{"bypassPermissions"}}
	if err := validate("cee", &s); err == nil || !strings.Contains(err.Error(), "skips canUseTool") {
		t.Fatalf("want bypassPermissions rejected, got %v", err)
	}
	s.Mode = &Mode{Default: "auto", Allow: []string{"auto"}}
	if err := validate("cee", &s); err != nil {
		t.Fatalf("auto is in the ceiling (docs/adr/0076): %v", err)
	}
}

func TestValidServicePath(t *testing.T) {
	ok := map[string][]string{
		"github.com":     {"/o/r/*", "/o/r.git/*", "/opus-tech-hq/workspace.git/*"},
		"api.github.com": {"/repos/o/r/*"},
	}
	bad := map[string][]string{
		"github.com":     {"/o/*", "/o/r", "/o/r/**", "/o/*/r/*", "/o/../r/*", "/../r/*", "/o/r%2F/*", "/o/r/x/*", "o/r/*", "/o/r/*x"},
		"api.github.com": {"/repos/o/*", "/o/r/*", "/repos/o/r/x/*", "/repos/../r/*"},
	}
	for host, ps := range ok {
		for _, p := range ps {
			if !validServicePath(host, p) {
				t.Errorf("%s %s: want accepted", host, p)
			}
		}
	}
	for host, ps := range bad {
		for _, p := range ps {
			if validServicePath(host, p) {
				t.Errorf("%s %s: want rejected", host, p)
			}
		}
	}
}

// A non-abstract spec with no tool, interaction or lifecycle bound has no
// bound at all (nil tools = the SDK's default tool set), so absence fails.
func TestLoad_RequiresBounds(t *testing.T) {
	bare := strings.NewReplacer(
		"  tools: { allow: [Read], deny: [\"Edit(.git/**)\"] }\n", "",
		"  interaction: { allow: [ask] }\n", "",
		"  lifecycle: { allow: [resident] }\n", "",
	).Replace(baseYAML)
	got, fileErrs := load(t, map[string]string{"definitions/base.yaml": bare, "definitions/cee.yaml": childYAML("cee", nil)})
	if len(fileErrs) != 0 {
		t.Fatalf("an abstract base may leave them out: %v", fileErrs)
	}
	msg := errText(got["cee"].Err)
	for _, want := range []string{"tools is required", "interaction is required", "lifecycle is required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("want %q in %q", want, msg)
		}
	}
}

// docs/adr/0077: model is optional. Absent everywhere, the definition is
// valid and its effective spec carries no model.
func TestLoad_ModelOptional(t *testing.T) {
	noModel := strings.Replace(baseYAML, "  model: { default: claude-opus-5, allow: [claude-opus-5] }\n", "", 1)
	if noModel == baseYAML {
		t.Fatal("fixture did not change")
	}
	got, fileErrs := load(t, map[string]string{"definitions/base.yaml": noModel, "definitions/cee.yaml": childYAML("cee", nil)})
	if len(fileErrs) != 0 {
		t.Fatalf("file errors: %v", fileErrs)
	}
	if r := got["cee"]; r.Err != nil || r.Effective == nil || r.Effective.Model != nil {
		t.Fatalf("want valid with no model, got err %v effective %+v", r.Err, r.Effective)
	}
}

// The KEY-name checks exist to catch a pasted secret, so they never echo it.
func TestLoad_SecretValuesNeverEchoed(t *testing.T) {
	const leaked = "ghp_PASTEDtokenVALUE123"
	got, fileErrs := load(t, map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/cee.yaml": childYAML("cee", map[string]string{
			"credentials": "{ vault: cee, services: [{ host: api.anthropic.com, secret: " + leaked + " }] }",
		}),
		"definitions/clients/bridge.yaml": clientWith("", leaked, ""),
	})
	all := errText(got["cee"].Err)
	for _, e := range fileErrs {
		all += "\n" + e.Error()
	}
	if !strings.Contains(all, "tokenRef") || !strings.Contains(all, "credentials.services") {
		t.Fatalf("want both KEY-name errors, got %q", all)
	}
	if strings.Contains(all, leaked) {
		t.Fatalf("a secret value was echoed: %q", all)
	}
}

// docs/adr/0065, 0075: vault slug minimum and the executor's and owner
// vaults' reserved prefixes, which the provisioner's prune never empties.
func TestLoad_VaultNames(t *testing.T) {
	got, _ := load(t, map[string]string{
		"definitions/base.yaml":       baseYAML,
		"definitions/ab.yaml":         childYAML("ab", nil),
		"definitions/executor-x.yaml": childYAML("executor-x", nil),
		"definitions/owner-x.yaml":    childYAML("owner-x", nil),
	})
	if msg := errText(got["ab"].Err); !strings.Contains(msg, "at least 3") {
		t.Errorf("ab: %q", msg)
	}
	for _, n := range []string{"executor-x", "owner-x"} {
		if msg := errText(got[n].Err); !strings.Contains(msg, "reserved") {
			t.Errorf("%s: %q", n, msg)
		}
	}
}

func TestGitHubRepo(t *testing.T) {
	for _, tc := range []struct {
		c    *Credentials
		want string
	}{
		{nil, ""},
		{&Credentials{Services: []Service{{Host: "api.anthropic.com"}}}, ""},
		{&Credentials{Services: []Service{{Host: "github.com", Paths: []string{"/MohammadBnei/notes/*", "/MohammadBnei/notes.git/*"}}}}, "MohammadBnei/notes"},
		{&Credentials{Services: []Service{{Host: "api.github.com", Paths: []string{"/repos/o/r/*"}}}}, "o/r"},
		{&Credentials{Services: []Service{{Host: "github.com", Paths: []string{"/reposlab/notes/*", "/reposlab/notes.git/*"}}}}, "reposlab/notes"},
	} {
		if got := GitHubRepo(tc.c); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.c, got, tc.want)
		}
	}
}

// docs/adr/0068 §4: a definition sharing an island definition's workspace
// repo must list embed.origins itself, or it would write scripts without the
// worker's ask.
func TestLoad_IslandWorkspaceIsNotShared(t *testing.T) {
	embed := `{ origins: ["https://notes.bnei.dev"] }`
	got, _ := load(t, map[string]string{
		"definitions/base.yaml":   baseYAML,
		"definitions/island.yaml": childYAML("island", map[string]string{"embed": embed}),
		"definitions/plain.yaml":  childYAML("plain", nil),
		"definitions/both.yaml":   childYAML("both", map[string]string{"embed": embed}),
		"definitions/other.yaml":  childYAML("other", map[string]string{"workspace": "{ kind: repo, repo: notes }"}),
	})
	if got["island"].Err != nil || got["both"].Err != nil || got["other"].Err != nil {
		t.Fatalf("island %v, both %v, other %v", got["island"].Err, got["both"].Err, got["other"].Err)
	}
	if err := got["plain"].Err; err == nil || !strings.Contains(err.Error(), "island host") || got["plain"].Effective != nil {
		t.Fatalf("plain shares the island's repo without embed: %v", err)
	}
}

// docs/adr/0066 §3: a tokenRef is a FLEET_CLIENT_TOKEN_* name, owned by one
// client; none is an in-process client.
func TestLoad_ClientTokenRefs(t *testing.T) {
	other := strings.ReplaceAll(clientWith("", "", ""), "name: bridge", "name: other")
	_, valid, fileErrs := LoadAll(map[string][]byte{
		"definitions/base.yaml":           []byte(baseYAML),
		"definitions/clients/bridge.yaml": []byte(clientWith("", "", "")),
		"definitions/clients/other.yaml":  []byte(other),
	})
	if len(valid) != 1 || len(fileErrs) != 1 || !strings.Contains(fileErrs[0].Error(), "already client") {
		t.Fatalf("shared tokenRef: valid %v, errs %v", valid, fileErrs)
	}
	_, _, fileErrs = LoadAll(map[string][]byte{
		"definitions/base.yaml":           []byte(baseYAML),
		"definitions/clients/bridge.yaml": []byte(clientWith("", "AGENTFLEET_DB_PASSWORD", "")),
	})
	if len(fileErrs) != 1 || !strings.Contains(fileErrs[0].Error(), "FLEET_CLIENT_TOKEN_") {
		t.Fatalf("tokenRef outside the prefix: %v", fileErrs)
	}
	announce := strings.Replace(clientWith("", "", ""), "  tokenRef: FLEET_CLIENT_TOKEN_BRIDGE\n", "  tokenRef: FLEET_CLIENT_TOKEN_BRIDGE\n  announceSessions: true\n", 1)
	if _, valid, fileErrs = LoadAll(map[string][]byte{"definitions/base.yaml": []byte(baseYAML), "definitions/clients/bridge.yaml": []byte(announce)}); len(fileErrs) != 0 || len(valid) != 1 || !valid[0].Spec.AnnounceSessions {
		t.Fatalf("announceSessions: %v %v", valid, fileErrs)
	}
	noRef := strings.Replace(clientWith("", "", ""), "  tokenRef: FLEET_CLIENT_TOKEN_BRIDGE\n", "", 1)
	_, valid, fileErrs = LoadAll(map[string][]byte{"definitions/base.yaml": []byte(baseYAML), "definitions/clients/bridge.yaml": []byte(noRef)})
	if len(fileErrs) != 0 || len(valid) != 1 || valid[0].Spec.TokenRef != "" {
		t.Fatalf("in-process client: %v %v", valid, fileErrs)
	}
}
