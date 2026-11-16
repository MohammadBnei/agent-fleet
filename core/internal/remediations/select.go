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
	"strconv"
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
	// A namespace and a container name are DNS labels: no dots, 63 at most.
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	cpuQty   = regexp.MustCompile(`^([1-9][0-9]{0,5})(m?)$`)
	memQty   = regexp.MustCompile(`^([1-9][0-9]{0,5})(Mi|Gi)$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// An env value is not a secret's home: the name must not look like one, and the value stays short and plain.
	secretish = regexp.MustCompile(`(?i)(key|token|secret|pass|credential|cert|auth|dsn|url)`)
)

// Resource floors and caps: a patch cannot starve a pod (0, 1m, 1Mi) or ask for a node (100000 cores).
const (
	minMilliCPU, maxMilliCPU = 10, 16000
	minMiB, maxMiB           = 16, 65536
)

func validCPU(q string) bool {
	m := cpuQty.FindStringSubmatch(q)
	if m == nil {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "" {
		n *= 1000
	}
	return n >= minMilliCPU && n <= maxMilliCPU
}

func validMemory(q string) bool {
	m := memQty.FindStringSubmatch(q)
	if m == nil {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "Gi" {
		n *= 1024
	}
	return n >= minMiB && n <= maxMiB
}

// inBounds: the definition must have set a range (Max >= 1), and never admits zero replicas, which is an outage.
func inBounds(b Bounds, n int) bool {
	return b.Min >= 1 && b.Max >= b.Min && b.Max <= maxScaleReplicas && n >= b.Min && n <= b.Max
}

// only keeps the named fields of s (and action and name); a selection that differs carried a parameter its
// action does not take.
func only(s Selection, fields ...string) Selection {
	k := Selection{Action: s.Action, Name: s.Name}
	for _, f := range fields {
		switch f {
		case "kind":
			k.Kind = s.Kind
		case "namespace":
			k.Namespace = s.Namespace
		case "replicas":
			k.Replicas = s.Replicas
		case "field":
			k.Field = s.Field
		case "container":
			k.Container = s.Container
		case "envName":
			k.EnvName = s.EnvName
		case "envValue":
			k.EnvValue = s.EnvValue
		case "cpu":
			k.CPU = s.CPU
		case "memory":
			k.Memory = s.Memory
		}
	}
	return k
}

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
		if s != only(s) {
			return invalid("sync_app takes only a name")
		}
		return nil
	}
	if !dnsLabel.MatchString(s.Namespace) || len(s.Namespace) > 63 {
		return invalid("namespace")
	}
	workload := s.Kind == "deployment" || s.Kind == "statefulset"
	switch s.Action {
	case ActionRestart:
		if !workload || s != only(s, "kind", "namespace") {
			return invalid("restart takes a deployment or statefulset")
		}
	case ActionScale:
		if s.Kind != "deployment" || s.Replicas == nil || s != only(s, "kind", "namespace", "replicas") {
			return invalid("scale takes a deployment and replicas")
		}
		if !inBounds(b, *s.Replicas) {
			return invalid("replicas outside the definition's bounds")
		}
	case ActionDeletePod:
		if s != only(s, "namespace") {
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
	base := []string{"kind", "namespace", "field"}
	switch s.Field {
	case "replicas":
		if s.Replicas == nil || !inBounds(b, *s.Replicas) || s != only(s, append(base, "replicas")...) {
			return invalid("replicas outside the definition's bounds")
		}
	case "resources":
		if !dnsLabel.MatchString(s.Container) || !validCPU(s.CPU) || !validMemory(s.Memory) ||
			s != only(s, append(base, "container", "cpu", "memory")...) {
			return invalid("resources take a container, cpu and memory quantities")
		}
	case "env":
		if !dnsLabel.MatchString(s.Container) || !envName.MatchString(s.EnvName) || secretish.MatchString(s.EnvName) ||
			len(s.EnvValue) > 128 || strings.ContainsAny(s.EnvValue, "\n\r\x00") || secretish.MatchString(s.EnvValue) ||
			s != only(s, append(base, "container", "envName", "envValue")...) {
			return invalid("env takes a container and a non-secret name and value")
		}
	case "restart":
		if s != only(s, base...) {
			return invalid("restart takes no parameters")
		}
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
		"provisioner", "agent-vault", "authentik", "argocd", "remediation", "thot",
		// Core's own Application is agent-fleet-core; the app-of-apps is bootstrap.
		"agent-fleet", "bootstrap",
	}
)

// Denied reports whether the target is one remediation never touches. A sync_app names an Application, not a namespace,
// so a denied component's own Application is refused by name token (agent-fleet-core, platform-thot, bootstrap).
func (s Selection) Denied() bool {
	if s.Action != ActionSyncApp && denyNamespaces[s.Namespace] {
		return true
	}
	if s.Action == ActionSyncApp && denyNamespaces[s.Name] {
		return true
	}
	for _, t := range denyNameTokens {
		if strings.Contains(s.Name, t) {
			return true
		}
	}
	return false
}
