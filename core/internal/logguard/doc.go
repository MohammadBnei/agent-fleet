// Package logguard holds a test and nothing else: it fails when an http.Server
// built by this module would log through the log package instead of slog. See
// guard_test.go.
//
// The same file sits in core, provisioner, sidecar and executor, one per
// module, because a module cannot import another module's test. core's
// buildguard (TestLogGuardCopiesAreIdentical) fails if the four drift apart.
package logguard
