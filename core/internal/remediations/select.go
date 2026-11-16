// Package remediations is docs/adr/0086 §3's catalogue in core: a responder's typed selection is schema-checked,
// denylist-checked and rate-limited here, then written as a single-use row the remediation executor pulls by id.
// Core holds no cluster RBAC; the checks only the cluster can answer (object exists, selfHeal) are the executor's.
package remediations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The catalogue. No cordon, no rollback (ADR-0086 §3). Patch is the free-form fix, low-stakes namespaces only.
const (
	ActionRestart   = "restart"
	ActionScale     = "scale"
	ActionDeletePod = "delete_pod"
	ActionSyncApp   = "sync_app"
	ActionPatch     = "patch"
)

var (
	ErrInvalid      = errors.New("remediation selection is invalid")
	ErrDenied       = errors.New("remediation target is on the denylist")
	ErrNotLowStakes = errors.New("a free-form fix needs a low-stakes namespace")
	ErrRateLimited  = errors.New("remediation rate limit reached")
	ErrNotFound     = errors.New("remediation not found or already used")
)

const maxScaleReplicas = 50

// Selection is the responder's typed output: an action plus its parameters. Unknown fields are refused.
type Selection struct {
	Action    string `json:"action"`
	Kind      string `json:"kind,omitempty"` // restart, scale, patch: deployment or statefulset (scale: deployment)
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// scale, and patch field "replicas"
	Replicas *int `json:"replicas,omitempty"`
	// patch only: one field of the allowlist, and its typed value.
	Field     string `json:"field,omitempty"` // replicas | resources | env | restart
	Container string `json:"container,omitempty"`
	EnvName   string `json:"envName,omitempty"`
	EnvValue  string `json:"envValue,omitempty"`
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
}

// Bounds is the definition's replica range for scale (ADR-0086 §3), set by the definition, never by the responder.
type Bounds struct{ Min, Max int }

// Parse decodes a selection, refusing unknown fields and trailing data.
func Parse(raw []byte) (Selection, error) {
	var s Selection
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return Selection{}, fmt.Errorf("%w: unreadable", ErrInvalid)
	}
	return s, nil
}

var (
	dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	quant   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|Mi|Gi|Ki)?$`)
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// An env value is not a secret's home: the name must not look like one, and the value stays short and plain.
	secretish = regexp.MustCompile(`(?i)(key|token|secret|pass|credential|cert|auth|dsn|url)`)
)

func (s Selection) target() string {
	if s.Action == ActionSyncApp {
		return "application/" + s.Name
	}
	return s.Namespace + "/" + s.Name
}

func invalid(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }

// Validate is the schema check: per action, exactly the fields it takes. lowStakes says the namespace is on the
// reviewed list; it alone admits patch.
func (s Selection) Validate(b Bounds, lowStakes bool) error {
	if !dns1123.MatchString(s.Name) {
		return invalid("name")
	}
	if s.Action == ActionSyncApp {
		if s.Namespace != "" || s.Kind != "" || s.Replicas != nil || s.Field != "" {
			return invalid("sync_app takes only a name")
		}
		return nil
	}
	if !dns1123.MatchString(s.Namespace) {
		return invalid("namespace")
	}
	workload := s.Kind == "deployment" || s.Kind == "statefulset"
	switch s.Action {
	case ActionRestart:
		if !workload || s.Replicas != nil || s.Field != "" {
			return invalid("restart takes a deployment or statefulset")
		}
	case ActionScale:
		if s.Kind != "deployment" || s.Replicas == nil || s.Field != "" {
			return invalid("scale takes a deployment and replicas")
		}
		if b.Min < 0 || b.Max < b.Min || b.Max > maxScaleReplicas || *s.Replicas < b.Min || *s.Replicas > b.Max {
			return invalid("replicas outside the definition's bounds")
		}
	case ActionDeletePod:
		if s.Kind != "" || s.Replicas != nil || s.Field != "" {
			return invalid("delete_pod takes only a namespace and name")
		}
	case ActionPatch:
		if !lowStakes {
			return ErrNotLowStakes
		}
		if !workload {
			return invalid("patch takes a deployment or statefulset")
		}
		return s.validatePatch(b)
	default:
		return invalid("action %q is not in the catalogue", s.Action)
	}
	return nil
}

// validatePatch is the field allowlist: replicas, container resources, a non-secret env value, the restart annotation.
func (s Selection) validatePatch(b Bounds) error {
	switch s.Field {
	case "replicas":
		if s.Replicas == nil || b.Min < 0 || b.Max < b.Min || b.Max > maxScaleReplicas || *s.Replicas < b.Min || *s.Replicas > b.Max {
			return invalid("replicas outside the definition's bounds")
		}
	case "resources":
		if !dns1123.MatchString(s.Container) || !quant.MatchString(s.CPU) || !quant.MatchString(s.Memory) {
			return invalid("resources take a container, cpu and memory quantities")
		}
	case "env":
		if !dns1123.MatchString(s.Container) || !envName.MatchString(s.EnvName) || secretish.MatchString(s.EnvName) ||
			len(s.EnvValue) > 128 || strings.ContainsAny(s.EnvValue, "\n\r\x00") || secretish.MatchString(s.EnvValue) {
			return invalid("env takes a container and a non-secret name and value")
		}
	case "restart":
	default:
		return invalid("field %q is not on the allowlist", s.Field)
	}
	return nil
}

// ADR-0086 §3's denylist: core, the provisioner, Agent Vault, authentik, the databases, the ArgoCD controller and the
// executor itself. By namespace (what each lives in) and by name (a database or component in any namespace).
var (
	denyNamespaces = map[string]bool{
		"agent-fleet": true, "agent-vault": true, "authentik": true, "argocd": true, "kube-system": true,
		"infisical": true, "longhorn-system": true, "thot": true,
	}
	denyNameTokens = []string{
		"postgres", "mysql", "mariadb", "redis", "mongo", "cnpg", "etcd",
		"provisioner", "agent-vault", "authentik", "argocd", "remediation", "thot-executor",
	}
)

// Denied reports whether the target is one remediation never touches. Applications are named by app, so a denied
// component's own Application is refused by name.
func (s Selection) Denied() bool {
	if s.Action != ActionSyncApp && denyNamespaces[s.Namespace] {
		return true
	}
	if s.Action == ActionSyncApp && (denyNamespaces[s.Name] || s.Name == "agent-fleet") {
		return true
	}
	for _, t := range denyNameTokens {
		if strings.Contains(s.Name, t) {
			return true
		}
	}
	return false
}
