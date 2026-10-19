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
	cases := []struct {
		name string
		kv   map[string]string
		want string
	}{
		{"valid", nil, ""},
		// widening at the effective spec
		{"mode widened to auto", map[string]string{"mode": "{ default: default, allow: [default, auto] }"}, `"auto" skips canUseTool`},
		{"acceptEdits by name", map[string]string{"mode": "{ default: default, allow: [default, acceptEdits] }"}, `"acceptEdits" skips canUseTool`},
		{"bypassPermissions by name", map[string]string{"mode": "{ default: default, allow: [bypassPermissions, default] }"}, `"bypassPermissions" skips canUseTool`},
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
		{"credentials ok", map[string]string{"credentials": `{ vault: valid-child, services: [{ host: github.com, paths: ["/o/r/*", "/o/r.git/*"], secret: A }, { host: api.github.com, paths: ["/repos/o/r/*"], secret: A }, { host: api.anthropic.com, secret: B }] }`}, ""},
		// owns / subscribes: shape only
		{"owns empty string", map[string]string{"owns": `{ repos: [""] }`}, "owns.repos"},
		{"owns duplicate", map[string]string{"owns": "{ repos: [a, a] }"}, "owns.repos"},
		{"owns non-string", map[string]string{"owns": "{ repos: [{ a: b }] }"}, "cannot unmarshal"},
		{"subscribes glob ok", map[string]string{"subscribes": `{ types: [dev.bnei.github.pull_request], subjects: ["github/opus-tech-hq/*", "x?"] }`}, ""},
		{"subscribes bad glob", map[string]string{"subscribes": `{ subjects: ["github/[ab]"] }`}, "subscribes.subjects"},
		{"subscribes type glob", map[string]string{"subscribes": `{ types: ["github.*"] }`}, "subscribes.types"},
		{"subscribes duplicate", map[string]string{"subscribes": "{ types: [a, a] }"}, "subscribes.types"},
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
		"definitions/c.yaml": childYAML("c", map[string]string{
			"tools": "{ allow: [Read, Grep] }",
			"mode":  "{ default: plan, allow: [plan] }",
		}),
	})
	r := got["c"]
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
		got, _ := load(t, map[string]string{"definitions/c.yaml": childYAML("c", map[string]string{"extends": "nope"})})
		if !strings.Contains(errText(got["c"].Err), `extends unknown definition "nope"`) {
			t.Fatal(got["c"].Err)
		}
	})
	t.Run("self cycle", func(t *testing.T) {
		got, _ := load(t, map[string]string{"definitions/c.yaml": childYAML("c", map[string]string{"extends": "c"})})
		if !strings.Contains(errText(got["c"].Err), "extends cycle: c → c") {
			t.Fatal(got["c"].Err)
		}
	})
	t.Run("three cycle", func(t *testing.T) {
		got, _ := load(t, map[string]string{
			"definitions/a.yaml": childYAML("a", map[string]string{"extends": "b"}),
			"definitions/b.yaml": childYAML("b", map[string]string{"extends": "c"}),
			"definitions/c.yaml": childYAML("c", map[string]string{"extends": "a"}),
			"definitions/d.yaml": childYAML("d", map[string]string{"extends": "a"}),
		})
		for _, n := range []string{"a", "b", "c"} {
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
		bad := strings.Replace(baseYAML, "allow: [default, plan]", "allow: [default, auto]", 1)
		got, fileErrs := load(t, map[string]string{
			"definitions/base.yaml": bad,
			"definitions/c.yaml":    childYAML("c", nil),
		})
		if !strings.Contains(errText(got["c"].Err), `parent "base"`) {
			t.Fatal(got["c"].Err)
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
	valid := childYAML("c", nil)
	cases := []struct {
		name, path, body, want string
	}{
		{"unknown top-level key", "definitions/c.yaml", valid + "extra: 1\n", "field extra not found"},
		{"unknown spec key", "definitions/c.yaml", strings.Replace(valid, "spec:\n", "spec:\n  colour: red\n", 1), "field colour not found"},
		{"two documents", "definitions/c.yaml", valid + "---\n" + valid, "one YAML document"},
		{"wrong apiVersion", "definitions/c.yaml", strings.Replace(valid, "v1alpha1", "v1", 1), "apiVersion"},
		{"wrong kind", "definitions/c.yaml", strings.Replace(valid, "AgentDefinition", "AgentClient", 1), "kind"},
		{"name not stem", "definitions/d.yaml", valid, `metadata.name "c" must equal the file stem "d"`},
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
	invoked := childYAML("c", map[string]string{"invokers": "[bridge]"})
	cases := []struct {
		name         string
		client, def  string
		wantFile     string // substring in fileErrs
		wantDefError string // substring in c's error
	}{
		{"agreement", clientWith("c", "", ""), invoked, "", ""},
		{"empty stub", clientWith("", "", ""), childYAML("c", nil), "", ""},
		{"invoker missing", clientWith("c", "", ""), childYAML("c", nil), `does not list client "bridge" in invokers`, ""},
		{"client does not list it back", clientWith("", "", ""), invoked, "", `client "bridge" does not list it`},
		{"unknown client", clientWith("", "", ""), childYAML("c", map[string]string{"invokers": "[ci]"}), "", `unknown client "ci"`},
		{"unknown definition", clientWith("c, ghost", "", ""), invoked, `unknown definition "ghost"`, ""},
		{"bad tokenRef", clientWith("c", "secret-value", ""), invoked, "tokenRef", ""},
		{"empty actors", clientWith("c", "", "[]"), invoked, "actors.allow", ""},
		{"bad actor", clientWith("c", "", "[not a login]"), invoked, "actors.allow", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fileErrs := load(t, map[string]string{
				"definitions/base.yaml":           baseYAML,
				"definitions/c.yaml":              tc.def,
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
			msg := errText(got["c"].Err)
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
func TestValidate_RejectsAutoOnAStoredSpec(t *testing.T) {
	got, _ := load(t, map[string]string{"definitions/base.yaml": baseYAML, "definitions/c.yaml": childYAML("c", nil)})
	s := *got["c"].Effective
	s.Mode = &Mode{Default: "auto", Allow: []string{"auto"}}
	if err := validate("c", &s); err == nil || !strings.Contains(err.Error(), "skips canUseTool") {
		t.Fatalf("want auto rejected, got %v", err)
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
	got, fileErrs := load(t, map[string]string{"definitions/base.yaml": bare, "definitions/c.yaml": childYAML("c", nil)})
	if len(fileErrs) != 0 {
		t.Fatalf("an abstract base may leave them out: %v", fileErrs)
	}
	msg := errText(got["c"].Err)
	for _, want := range []string{"tools is required", "interaction is required", "lifecycle is required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("want %q in %q", want, msg)
		}
	}
}

// The KEY-name checks exist to catch a pasted secret, so they never echo it.
func TestLoad_SecretValuesNeverEchoed(t *testing.T) {
	const leaked = "ghp_PASTEDtokenVALUE123"
	got, fileErrs := load(t, map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/c.yaml": childYAML("c", map[string]string{
			"credentials": "{ vault: c, services: [{ host: api.anthropic.com, secret: " + leaked + " }] }",
		}),
		"definitions/clients/bridge.yaml": clientWith("", leaked, ""),
	})
	all := errText(got["c"].Err)
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
