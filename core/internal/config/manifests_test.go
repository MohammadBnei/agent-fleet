package config

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type envEntry struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// manifestEnv is the literal env of core's manifests: k8s/core.yaml's top-level env (a values file for
// common-app-chart) and local/kind/30-core.yaml's Deployment container. A secretKeyRef entry has no literal and is
// left out: its value is not in git.
func manifestEnv(t *testing.T, path string, pick func(doc map[string]any) []envEntry) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if es := pick(doc); es != nil {
			m := map[string]string{}
			for _, e := range es {
				if e.Value != "" {
					m[e.Name] = e.Value
				}
			}
			return m
		}
	}
	t.Fatalf("%s: no env found", path)
	return nil
}

func decodeEnv(t *testing.T, v any) []envEntry {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var es []envEntry
	if err := yaml.Unmarshal(b, &es); err != nil {
		t.Fatal(err)
	}
	return es
}

// TestManifestsLoad: what the repo's manifests set for core is accepted by Load. A literal that would now refuse is
// a deploy that crash-loops, so it fails here instead (go.yml's unfiltered docs-check job runs this test, so a
// manifest-only or docs-only PR meets it too).
func TestManifestsLoad(t *testing.T) {
	core := manifestEnv(t, "../../../k8s/core.yaml", func(doc map[string]any) []envEntry {
		if e, ok := doc["env"]; ok {
			return decodeEnv(t, e)
		}
		return nil
	})
	kind := manifestEnv(t, "../../../local/kind/30-core.yaml", func(doc map[string]any) []envEntry {
		if doc["kind"] != "Deployment" {
			return nil
		}
		spec, _ := doc["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		pod, _ := tpl["spec"].(map[string]any)
		cs, _ := pod["containers"].([]any)
		if len(cs) == 0 {
			return nil
		}
		c, _ := cs[0].(map[string]any)
		return decodeEnv(t, c["env"])
	})
	for name, m := range map[string]map[string]string{"k8s/core.yaml": core, "local/kind/30-core.yaml": kind} {
		if len(m) < 4 {
			t.Fatalf("%s: read only %d literal values: %v", name, len(m), m)
		}
		// The provisioner and core tokens are secrets in production; a local literal is not what is checked here.
		if _, err := Load(env(m)); err != nil {
			t.Errorf("%s: Load refuses what the manifest sets: %v", name, err)
		}
	}
	// The positive control: the reader sees a value, and Load refuses a bad one in the same map.
	bad := map[string]string{}
	for k, v := range core {
		bad[k] = v
	}
	bad["SESSION_RETENTION"] = "6 days"
	if _, err := Load(env(bad)); err == nil || !strings.Contains(err.Error(), "SESSION_RETENTION=") {
		t.Errorf("a bad value in the manifest's map: %v", err)
	}
	// The reader found the keys, whatever their values are tuned to (k8s/core.yaml is edited as gitops, not a release).
	if core["SESSION_RETENTION"] == "" || kind["MAX_IN_FLIGHT_TASKS"] == "" {
		t.Errorf("the reader missed known keys: SESSION_RETENTION=%q, MAX_IN_FLIGHT_TASKS=%q", core["SESSION_RETENTION"], kind["MAX_IN_FLIGHT_TASKS"])
	}
}
