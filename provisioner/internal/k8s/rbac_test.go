package k8s

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// TestNoFleetRoleCanRewriteRepoOverrides guards docs/adr/0074 §9: the
// fleet-repo-overrides ConfigMap grants cluster_access, so no agent-fleet
// Role may update, patch or delete a ConfigMap unless it is pinned by
// resourceNames to objects other than that one. create cannot be pinned by
// resourceNames; the provisioner's create (docs/adr/0065) can only land while
// the ConfigMap is absent, and ArgoCD owns it, so that is the named residual.
//
// Reads every manifest under k8s/, including core.yaml's extraManifests:
// run with -count=1.
func TestNoFleetRoleCanRewriteRepoOverrides(t *testing.T) {
	root := filepath.Join("..", "..", "..", "k8s")
	var docs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".yaml" {
			return err
		}
		rel, err := filepath.Rel(filepath.Join("..", "..", ".."), p)
		if err != nil {
			return err
		}
		raw := string(repoFile(t, rel))
		docs = append(docs, strings.Split(raw, "\n---")...)
		var values struct {
			ExtraManifests []string `json:"extraManifests"`
		}
		if yaml.Unmarshal([]byte(raw), &values) == nil {
			docs = append(docs, values.ExtraManifests...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	roles := 0
	for _, doc := range docs {
		var r rbacv1.Role // a ClusterRole has the same rules shape
		if yaml.Unmarshal([]byte(doc), &r) != nil || (r.Kind != "Role" && r.Kind != "ClusterRole") {
			continue
		}
		roles++
		for _, rule := range r.Rules {
			if !hasAny(rule.APIGroups, "", "*") || !hasAny(rule.Resources, "configmaps", "*") ||
				!hasAny(rule.Verbs, "update", "patch", "delete", "deletecollection", "*") {
				continue
			}
			if len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, "fleet-repo-overrides") {
				t.Errorf("%s %s can %v ConfigMaps %v: that reaches fleet-repo-overrides, which grants cluster_access (docs/adr/0074)",
					r.Kind, r.Name, rule.Verbs, rule.ResourceNames)
			}
		}
	}
	if roles == 0 {
		t.Fatal("found no Role in k8s/: this guard is worthless if it silently skips")
	}
}

func hasAny(have []string, want ...string) bool {
	for _, w := range want {
		if slices.Contains(have, w) {
			return true
		}
	}
	return false
}
