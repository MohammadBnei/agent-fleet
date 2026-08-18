package k8s

import (
	"strings"
	"testing"
)

// Preview hostnames must stay FIRST-LEVEL under the base domain: exactly one
// new DNS label, no deeper. Cloudflare's free Universal SSL covers the apex
// plus exactly one wildcard level, so *.bnei.dev secures abc-e2e.bnei.dev but
// nothing secures abc.e2e.bnei.dev — the latter gets no certificate at all
// through the proxy and fails the TLS handshake before HTTP is reached.
// Confirmed in production 2026-08-18 against dev.api.voconsteroid.com.
//
// This test exists because the obvious "tidy-up" is to turn the "-e2e" suffix
// back into its own DNS label, which silently breaks every preview.
func TestPreviewHostForStaysFirstLevel(t *testing.T) {
	const base = "bnei.dev"
	got := PreviewHostFor(base, "abc-def-123")

	suffix := "." + base
	if !strings.HasSuffix(got, suffix) {
		t.Fatalf("preview host %q must sit under %q", got, base)
	}

	label := strings.TrimSuffix(got, suffix)
	if label == "" {
		t.Fatalf("preview host %q has no label above the base domain", got)
	}
	if strings.Contains(label, ".") {
		t.Fatalf("preview host %q adds more than one DNS label (%q) — a deeper name is NOT covered by *%s", got, label, suffix)
	}
	if !strings.HasSuffix(label, "-e2e") {
		t.Fatalf("preview label %q must end in -e2e so previews stay identifiable", label)
	}

	// The wildcard each route requests must be the one Traefik's default
	// TLSStore holds, or previews trigger their own ACME orders.
	if d := PreviewDomainFor(base); d != "*."+base {
		t.Fatalf("preview domain = %q, want *.%s", d, base)
	}
}
