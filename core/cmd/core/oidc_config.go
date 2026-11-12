package main

import (
	"errors"
	"fmt"
	"net"

	"github.com/MohammadBnei/agent-fleet/core/internal/config"
)

// requireOIDC is the console gate's configuration that auth.New does not check itself: it checks the session keys
// and runs discovery against the issuer, and nothing else. With the gate on, an empty DASHBOARD_PUBLIC_URL gives a
// relative redirect URI ("/auth/callback") and an empty accepted origin, an empty OIDC_CLIENT_ID makes every ID token
// fail its audience check, and an empty OIDC_CLIENT_SECRET fails the token exchange against authentik's confidential
// client (k8s/core.yaml provisions all three, the secret from fleet-oidc's client_secret): the console is up and
// nobody can sign in. So each missing one refuses the start (adr/0056 §4: unset OIDC config makes core refuse),
// every one named, unless FLEET_AUTH_DISABLED=1. A malformed DASHBOARD_PUBLIC_URL is config.Load's.
func requireOIDC(cfg config.Config) error {
	if cfg.AuthDisabled {
		return nil
	}
	var errs []error
	for _, v := range []struct{ key, val string }{
		{"DASHBOARD_PUBLIC_URL", cfg.DashboardPublicURL},
		{"OIDC_CLIENT_ID", cfg.OIDCClientID},
		{"OIDC_CLIENT_SECRET", cfg.OIDCClientSecret},
	} {
		if v.val == "" {
			errs = append(errs, fmt.Errorf("%s is not set: the console gate is on, and sign-in cannot work without it", v.key))
		}
	}
	return errors.Join(errs...)
}

// requireLoopback keeps FLEET_AUTH_DISABLED=1 off every surface but a port-forward. In a cluster, port 8080 is behind
// fleet.bnei.dev's public IngressRoute, so with the gate off anyone on the internet would hold the console as the dev
// identity. There the flag refuses the start unless CORE_HTTP_HOST is a loopback address, which only kubectl
// port-forward reaches. Outside a cluster (a laptop, /dashboard-e2e) nothing changes.
func requireLoopback(cfg config.Config) error {
	if !cfg.AuthDisabled || !cfg.InCluster {
		return nil
	}
	if h := cfg.HTTPHost; h == "localhost" || net.ParseIP(h).IsLoopback() {
		return nil
	}
	return fmt.Errorf("FLEET_AUTH_DISABLED=1 inside a cluster needs CORE_HTTP_HOST=127.0.0.1 (got %q): the console port is public through the IngressRoute; reach the break-glass core with kubectl port-forward", cfg.HTTPHost)
}
