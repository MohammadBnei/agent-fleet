#!/usr/bin/env bash
# The local stack's read-only definitions mirror (docs/adr/0080 §2, §5): a bare
# repo on the workspace hostPath, outside every pod's mounts, whose `game`
# branch is the COMMITTED branch under test with definitions/ replaced by
# local/kind/definitions/. The provisioner reads definitions, packs and
# fleet-shared from it (FLEET_SHARED_REPO_URL). Re-run after each commit.
set -euo pipefail
repo=$(git rev-parse --show-toplevel)
mirror=$repo/local/kind/workspace-data/local-mirror/agent-fleet.git
branch=$(git -C "$repo" rev-parse --abbrev-ref HEAD)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# One commit holding the tree, no history: the provisioner reads files at the
# ref, and a shallow checkout could not push its history anyway.
git clone -q --depth 1 --branch "$branch" "file://$repo" "$work"
git -C "$work" checkout -q --orphan game
rm -rf "$work/definitions"
cp -R "$repo/local/kind/definitions" "$work/definitions"
git -C "$work" add -A
git -C "$work" -c user.name=kind-local -c user.email=kind-local@localhost \
  commit -q -m "kind-local: $branch ($(git -C "$repo" rev-parse --short HEAD)) with local/kind/definitions"
[ -d "$mirror" ] || git init -q --bare "$mirror"
git -C "$work" -c push.negotiate=false push -q --force "$mirror" HEAD:refs/heads/game
echo "mirror: $mirror (game = $branch + local/kind/definitions)"
