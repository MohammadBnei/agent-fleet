#!/usr/bin/env bash
# One `go test` per module, in two invocations with the same build flags (so the
# compiled test binaries are shared): the packages Go's test-result cache cannot
# judge run with -count=1, everything else may be served from the cache.
#
# Cannot judge: a test that reads files outside its module (the repo's
# .github/, k8s/, definitions/, proto/ ...), or depends on state Go does not
# track (the integration tests' database). A cached PASS survives edits to
# those, so the guards would guard nothing (core/internal/buildguard records
# verifying this live). Detected by what the test sources reference, so a new
# package that reads outside its module is covered without editing a list.
#
# Run from the module directory. Extra args go to both go test calls.
set -euo pipefail

flags=(-race -cover -tags=integration)
uncached_re='\.\./|repoRoot|runtime\.Caller|internal/dbtest|testcontainers|AGENTFLEET_TEST_'

# Package directories, relative to the module. buildguard asserts workflow and
# Dockerfile facts, so it is always uncached.
dirs=$({ grep -rlE "$uncached_re" --include='*_test.go' . | xargs -r -n1 dirname
         [ ! -d internal/buildguard ] || echo ./internal/buildguard; } | sort -u)

go list -tags=integration ./... | sort -u > "${RUNNER_TEMP:-/tmp}/all-pkgs.txt"
if [ -n "$dirs" ]; then
  # shellcheck disable=SC2086 # one dir per word is the point
  go list -tags=integration $dirs | sort -u > "${RUNNER_TEMP:-/tmp}/uncached-pkgs.txt"
else
  : > "${RUNNER_TEMP:-/tmp}/uncached-pkgs.txt"
fi

mapfile -t cached < <(comm -23 "${RUNNER_TEMP:-/tmp}/all-pkgs.txt" "${RUNNER_TEMP:-/tmp}/uncached-pkgs.txt")
mapfile -t uncached < "${RUNNER_TEMP:-/tmp}/uncached-pkgs.txt"
echo "cached packages: ${#cached[@]}, uncached (-count=1): ${#uncached[@]}"

[ ${#cached[@]} -eq 0 ] || go test "${flags[@]}" "$@" "${cached[@]}"
[ ${#uncached[@]} -eq 0 ] || go test "${flags[@]}" -count=1 "$@" "${uncached[@]}"
