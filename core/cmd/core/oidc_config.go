package main

import (
	"errors"
	"fmt"

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
