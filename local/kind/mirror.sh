#!/usr/bin/env bash
# The local stack's definitions mirror (docs/adr/0080 §2, §5): a bare repo on
# the workspace hostPath, outside every worker pod's mounts, whose `game` branch
# is the COMMITTED tree of HEAD with definitions/ replaced by its own
# local/kind/definitions/. The provisioner reads definitions, packs and
# fleet-shared from it (FLEET_SHARED_REPO_URL). Re-run after each commit.
set -euo pipefail
repo=$(git rev-parse --show-toplevel)
mirror=$repo/local/kind/workspace-data/local-mirror/agent-fleet.git
head=$(git -C "$repo" rev-parse --short HEAD)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The committed tree only (a detached HEAD works too: agent-fleet is usually a
# submodule), as one commit with no history: the provisioner reads files at the
# ref.
git -C "$repo" archive HEAD | tar -x -C "$work"
rm -rf "$work/definitions"
mv "$work/local/kind/definitions" "$work/definitions"
git -C "$work" init -q -b game
git -C "$work" add -A
git -C "$work" -c user.name=kind-local -c user.email=kind-local@localhost \
  commit -q -m "kind-local: $head with local/kind/definitions"
[ -d "$mirror" ] || git init -q --bare "$mirror"
git -C "$work" -c push.negotiate=false push -q --force "$mirror" game
echo "mirror: $mirror (game = $head + local/kind/definitions)"
