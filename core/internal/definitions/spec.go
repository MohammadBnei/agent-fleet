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
// ponytail: no Agents / Output fields in P1. Strict decode rejects `agents:`
// and `output:` as unknown keys; P4, their only consumer, adds them.
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
	Interaction  *AllowList    `yaml:"interaction,omitempty"  json:"interaction,omitempty"`
	Lifecycle    *AllowList    `yaml:"lifecycle,omitempty"    json:"lifecycle,omitempty"`
	Owns         *Owns         `yaml:"owns,omitempty"         json:"owns,omitempty"`
	Subscribes   *Subscribes   `yaml:"subscribes,omitempty"   json:"subscribes,omitempty"`
	Resources    *Resources    `yaml:"resources,omitempty"    json:"resources,omitempty"`
	Limits       *Limits       `yaml:"limits,omitempty"       json:"limits,omitempty"`
}

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
}

type ClientSpec struct {
	TokenRef    string    `yaml:"tokenRef"    json:"tokenRef"`
	Actors      AllowList `yaml:"actors"      json:"actors"`
	Definitions []string  `yaml:"definitions" json:"definitions"`
}
