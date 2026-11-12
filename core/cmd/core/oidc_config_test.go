package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/config"
)

// TestRequireOIDC: with the console gate on, each of DASHBOARD_PUBLIC_URL, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET that is
// missing refuses, and every missing one is named; with the gate off nothing is required. The positive control is the
// full set.
func TestRequireOIDC(t *testing.T) {
	full := config.Config{DashboardPublicURL: "https://fleet.bnei.dev", OIDCClientID: "id", OIDCClientSecret: "secret"}
	if err := requireOIDC(full); err != nil {
		t.Fatalf("a complete config refused: %v", err)
	}
	for _, key := range []string{"DASHBOARD_PUBLIC_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"} {
		c := full
		switch key {
		case "DASHBOARD_PUBLIC_URL":
			c.DashboardPublicURL = ""
		case "OIDC_CLIENT_ID":
			c.OIDCClientID = ""
		case "OIDC_CLIENT_SECRET":
			c.OIDCClientSecret = ""
		}
		err := requireOIDC(c)
		if err == nil || !strings.Contains(err.Error(), key+" is not set") {
			t.Errorf("%s missing: %v; want a refusal naming it", key, err)
		}
		for _, other := range []string{"DASHBOARD_PUBLIC_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"} {
			if other != key && err != nil && strings.Contains(err.Error(), other) {
				t.Errorf("%s missing: the refusal names %s too", key, other)
			}
		}
		c.AuthDisabled = true
		if err := requireOIDC(c); err != nil {
			t.Errorf("%s missing with FLEET_AUTH_DISABLED=1: %v; want none required", key, err)
		}
	}
	err := requireOIDC(config.Config{})
	for _, key := range []string{"DASHBOARD_PUBLIC_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"} {
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("an empty config does not name %s: %v", key, err)
		}
	}
}

// TestRunChecksOIDCBeforeBuildingTheGate: run() calls requireOIDC, before auth.New, so the check cannot be dropped or
// moved behind the gate's construction without this failing (run() needs a database, so it is read, not run).
func TestRunChecksOIDCBeforeBuildingTheGate(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var require, newAuth token.Pos
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := c.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "requireOIDC" && !require.IsValid() {
					require = c.Pos()
				}
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok && x.Name == "auth" && fun.Sel.Name == "New" && !newAuth.IsValid() {
					newAuth = c.Pos()
				}
			}
			return true
		})
	}
	if !require.IsValid() || !newAuth.IsValid() || require > newAuth {
		t.Errorf("run() must call requireOIDC (at %v) before auth.New (at %v)", require, newAuth)
	}
}

// TestRequireLoopback: in a cluster, FLEET_AUTH_DISABLED=1 refuses unless the console port is bound to loopback,
// because 8080 there is behind the public IngressRoute. Off-cluster (a laptop, /dashboard-e2e) and with the gate on,
// nothing is required.
func TestRequireLoopback(t *testing.T) {
	for _, tc := range []struct {
		name              string
		disabled, cluster bool
		host              string
		refused           bool
	}{
		{"gate on in cluster", false, true, "", false},
		{"gate off on a laptop", true, false, "", false},
		{"gate off in cluster, every interface", true, true, "", true},
		{"gate off in cluster, pod IP", true, true, "10.233.64.7", true},
		{"gate off in cluster, 0.0.0.0", true, true, "0.0.0.0", true},
		{"gate off in cluster, 127.0.0.1", true, true, "127.0.0.1", false},
		{"gate off in cluster, ::1", true, true, "::1", false},
		{"gate off in cluster, localhost", true, true, "localhost", false},
	} {
		err := requireLoopback(config.Config{AuthDisabled: tc.disabled, InCluster: tc.cluster, HTTPHost: tc.host})
		if (err != nil) != tc.refused {
			t.Errorf("%s: err = %v, want refused=%v", tc.name, err, tc.refused)
		}
	}
}

// TestRunChecksLoopback: run() calls requireLoopback, so the check cannot be dropped silently (run() needs a
// database, so it is read, not run).
func TestRunChecksLoopback(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "requireLoopback" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Error("run() must call requireLoopback")
	}
}
