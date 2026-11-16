// Package definitions loads, validates and syncs defined agents
// (docs/adr/0062): reviewed `definitions/*.yaml` files that fix a session's
// repo, model, mode and limits ahead of time, judged on their EFFECTIVE spec
// (after `extends`) against a ceiling the fleet owns.
package definitions

// apiVersion is the only apiVersion a definition or client file may carry.
const apiVersion = "fleet.bnei.dev/v1alpha1"

const (
	kindDefinition = "AgentDefinition"
	kindClient     = "AgentClient"
)

// File is the header every definitions/ file shares.
type File struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type definitionFile struct {
	File `yaml:",inline"`
	Spec Spec `yaml:"spec"`
}

type clientFile struct {
	File `yaml:",inline"`
	Spec ClientSpec `yaml:"spec"`
}

// Spec is one definition's spec. yaml and json tags are identical: the
// `effective` jsonb column is json.Marshal(Spec), and SQL reads
// `limits.concurrency` and `mode.allow` out of it (sessions.ReserveSlot,
// sessions.SetPermissionMode).
//
// ponytail: no Agents field in P1. Strict decode rejects `agents:` as an
// unknown key. `output:` joined with docs/adr/0070 §3.
type Spec struct {
	Extends      string        `yaml:"extends,omitempty"      json:"extends,omitempty"`
	Abstract     bool          `yaml:"abstract,omitempty"     json:"abstract,omitempty"` // never inherited
	Audience     *Audience     `yaml:"audience,omitempty"     json:"audience,omitempty"`
	Invokers     []string      `yaml:"invokers,omitempty"     json:"invokers,omitempty"`
	Embed        *Embed        `yaml:"embed,omitempty"        json:"embed,omitempty"`
	Credentials  *Credentials  `yaml:"credentials,omitempty"  json:"credentials,omitempty"`
	Workspace    *Workspace    `yaml:"workspace,omitempty"    json:"workspace,omitempty"`
	Git          *Git          `yaml:"git,omitempty"          json:"git,omitempty"`
	Model        *Model        `yaml:"model,omitempty"        json:"model,omitempty"`
	Thinking     *Thinking     `yaml:"thinking,omitempty"     json:"thinking,omitempty"`
	Mode         *Mode         `yaml:"mode,omitempty"         json:"mode,omitempty"`
	SystemPrompt *SystemPrompt `yaml:"systemPrompt,omitempty" json:"systemPrompt,omitempty"`
	Skills       []string      `yaml:"skills,omitempty"       json:"skills,omitempty"`
	Tools        *Tools        `yaml:"tools,omitempty"        json:"tools,omitempty"`
	SidecarTools []string      `yaml:"sidecarTools,omitempty" json:"sidecarTools,omitempty"`
	Lifecycle    *AllowList    `yaml:"lifecycle,omitempty"    json:"lifecycle,omitempty"`
	Output       *Output       `yaml:"output,omitempty"       json:"output,omitempty"`
	Owns         *Owns         `yaml:"owns,omitempty"         json:"owns,omitempty"`
	Subscribes   *Subscribes   `yaml:"subscribes,omitempty"   json:"subscribes,omitempty"`
	Resources    *Resources    `yaml:"resources,omitempty"    json:"resources,omitempty"`
	Limits       *Limits       `yaml:"limits,omitempty"       json:"limits,omitempty"`
	Trust        *Trust        `yaml:"trust,omitempty"        json:"trust,omitempty"`
}

// Trust is the definition's half of docs/adr/0086 §2. Untrusted names the one letter (B or C) a session keeps
// beside A when an untrusted trigger starts it; empty means untrusted input cannot start it. B makes the
// definition a first responder (§3): single-shot, no GitHub write, its `subscribes` the alerts it answers.
type Trust struct {
	Untrusted string `yaml:"untrusted,omitempty" json:"untrusted,omitempty"`
	// Scale is the replica range a responder's scale selection must stay in (§3); core checks it.
	Scale *ScaleBounds `yaml:"scale,omitempty" json:"scale,omitempty"`
}

type ScaleBounds struct {
	Min int `yaml:"min" json:"min"`
	Max int `yaml:"max" json:"max"`
}

// Responder reports whether s is a first responder.
func (s *Spec) Responder() bool { return s != nil && s.Trust != nil && s.Trust.Untrusted == "B" }

type Audience struct {
	Group string `yaml:"group" json:"group"`
}

type Embed struct {
	Origins []string `yaml:"origins" json:"origins"`
}

type Credentials struct {
	Vault    string    `yaml:"vault"    json:"vault"`
	Services []Service `yaml:"services" json:"services"`
}

type Service struct {
	Host   string   `yaml:"host"            json:"host"`
	Paths  []string `yaml:"paths,omitempty" json:"paths,omitempty"`
	Secret string   `yaml:"secret"          json:"secret"`
}

type Workspace struct {
	Kind string `yaml:"kind" json:"kind"`
	Repo string `yaml:"repo" json:"repo"`
}

type Git struct {
	Mode     string    `yaml:"mode"             json:"mode"`
	Branch   string    `yaml:"branch,omitempty" json:"branch,omitempty"`
	Identity *Identity `yaml:"identity"         json:"identity"`
}

type Identity struct {
	Name  string `yaml:"name"  json:"name"`
	Email string `yaml:"email" json:"email"`
}

type Model struct {
	Default string   `yaml:"default" json:"default"`
	Allow   []string `yaml:"allow"   json:"allow"`
}

type Thinking struct {
	Default string `yaml:"default" json:"default"`
}

type Mode struct {
	Default string   `yaml:"default" json:"default"`
	Allow   []string `yaml:"allow"   json:"allow"`
}

type SystemPrompt struct {
	Append string `yaml:"append" json:"append"`
}

type Tools struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty"  json:"deny,omitempty"`
}

type AllowList struct {
	Allow []string `yaml:"allow" json:"allow"`
}

// Output is a single-shot session's result shape (docs/adr/0070 §3): a JSON
// Schema whose root is `type: object`, at most 64 KiB once encoded.
type Output struct {
	Schema map[string]any `yaml:"schema" json:"schema"`
}

type Owns struct {
	Repos      []string `yaml:"repos,omitempty"      json:"repos,omitempty"`
	Components []string `yaml:"components,omitempty" json:"components,omitempty"`
	Subjects   []string `yaml:"subjects,omitempty"   json:"subjects,omitempty"`
	Types      []string `yaml:"types,omitempty"      json:"types,omitempty"`
}

// Subscribes is passed through to the event bridge as-is: subjects are globs
// (`*`, `?`) matching its Subscribe filter, types are prefixes.
type Subscribes struct {
	Types    []string `yaml:"types,omitempty"    json:"types,omitempty"`
	Subjects []string `yaml:"subjects,omitempty" json:"subjects,omitempty"`
}

type Resources struct {
	CPU    string `yaml:"cpu"    json:"cpu"`
	Memory string `yaml:"memory" json:"memory"`
}

type Limits struct {
	MaxTurns      int    `yaml:"maxTurns"      json:"maxTurns"`
	Timeout       string `yaml:"timeout"       json:"timeout"`
	Concurrency   int    `yaml:"concurrency"   json:"concurrency"`
	StartupBudget string `yaml:"startupBudget" json:"startupBudget"`
	// MaxParks: a single-shot run that parks more than this many times ends
	// failed, PARK_LIMIT (docs/adr/0070 §6, "Re-park cap"). Unset is
	// DefaultMaxParks; sessions.Store reads it out of `effective` in SQL.
	MaxParks *int `yaml:"maxParks,omitempty" json:"maxParks,omitempty"`
	// AnswerTimeout bounds how long a parked single-shot run's decision may
	// wait (docs/adr/0070 §6); past it core ends the run expired. Unset is
	// 24h, applied where it is read (sessions.Loop), not here: filling it in
	// would change every definition's effective JSON and so move every pin.
	AnswerTimeout string `yaml:"answerTimeout,omitempty" json:"answerTimeout,omitempty"`
}

// DefaultMaxParks is limits.maxParks when a definition sets none.
const DefaultMaxParks = 10

type ClientSpec struct {
	// TokenRef names core's env var holding the client's bearer for :9091.
	// Empty is an in-process client (the bridge's dispatcher, docs/adr/0066
	// §3): it never matches a bearer, so its rights are not reachable from the
	// network.
	TokenRef    string    `yaml:"tokenRef,omitempty" json:"tokenRef,omitempty"`
	Actors      AllowList `yaml:"actors"      json:"actors"`
	Definitions []string  `yaml:"definitions" json:"definitions"`
	// AnnounceSessions: core publishes dev.bnei.fleet.session.waiting and
	// .resumed to the bridge for the sessions this client opened, under
	// fleet/client/<name>/ (docs/adr/0078, core/internal/announce). Never for
	// anyone else's sessions.
	AnnounceSessions bool `yaml:"announceSessions,omitempty" json:"announceSessions,omitempty"`
	// MaxParked caps this client's parked single-shot runs: Invoke refuses a
	// new run with PARKED_LIMIT while that many wait on a decision
	// (docs/adr/0070 §1). Unset is DefaultMaxParked.
	MaxParked *int `yaml:"maxParked,omitempty" json:"maxParked,omitempty"`
}

// DefaultMaxParked is a client's maxParked when its file sets none.
const DefaultMaxParked = 20

// ParkedCap is s.MaxParked, or DefaultMaxParked when unset.
func (s ClientSpec) ParkedCap() int {
	if s.MaxParked == nil {
		return DefaultMaxParked
	}
	return *s.MaxParked
}

// Client is a valid client registration after a sync.
type Client struct {
	Name string
	Spec ClientSpec
}
