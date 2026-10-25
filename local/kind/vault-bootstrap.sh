#!/usr/bin/env bash
# The local vault's bootstrap (docs/adr/0080 §1), the local counterpart of
# infra-bootstrap's gitops/scripts/agent-vault-bootstrap.sh, with no Infisical:
# register the owner on first run, close registration, mint the provisioner's
# member token, and write Secret provisioner-agent-vault with every file the
# provisioner reads from /var/run/agent-vault. Re-runnable: it rotates the token.
#
# Usage: local/kind/vault-bootstrap.sh <model-token-file>
# The model token is the only real secret, and it stays in the vault. Use a
# separate, revocable token for local runs.
set -euo pipefail
NS=agent-fleet
MODEL_TOKEN_FILE=${1:?usage: vault-bootstrap.sh <model-token-file>}
[ -s "$MODEL_TOKEN_FILE" ] || { echo "vault-bootstrap: $MODEL_TOKEN_FILE is empty" >&2; exit 1; }

OWN=$(kubectl -n "$NS" get secret agent-vault-local -o jsonpath='{.data.OWNER_PASSWORD}' | base64 -d)
[ -n "$OWN" ] || { echo "vault-bootstrap: Secret agent-vault-local has no OWNER_PASSWORD" >&2; exit 1; }

TOKEN=$({
  printf 'OWN=%s\n' "$OWN"
  cat <<'SH'
set -eu
A=http://127.0.0.1:14321
E=agent-vault-owner@local
export HOME="$(mktemp -d)"
S="$HOME/.agent-vault/session.json"
trap 'rm -rf "$HOME"' EXIT
tok() { sed -n 's/.*"token":"\([^"]*\)".*/\1/p' "$S" 2>/dev/null || true; }
st=$(wget -qO- "$A/v1/status")
case "$st" in
  *'"initialized":false'*)
    printf %s "$OWN" | agent-vault auth register --address "$A" --email "$E" --password-stdin >/dev/null ;;
esac
[ -s "$S" ] || printf %s "$OWN" | agent-vault auth login --address "$A" --email "$E" --password-stdin >/dev/null
agent-vault owner config set --invite-only >/dev/null
if wget -qO- --header "Authorization: Bearer $(tok)" "$A/v1/agents/fleet-provisioner" >/dev/null 2>&1; then
  agent-vault agent rotate fleet-provisioner --token-only
else
  agent-vault agent create fleet-provisioner --role member --token-only
fi
SH
} | kubectl -n "$NS" exec -i deploy/agent-vault-local -- sh -s)
case "$TOKEN" in av_agt_*) ;; *) echo "vault-bootstrap: the vault printed no agent token" >&2; exit 1 ;; esac

# GH_<VAULT> per game definition and GH_OWNER_<OWNER> for repo sessions'
# owner vaults: the files a vault falls back to with no App mounted.
# Placeholders the forge accepts; non-empty, or the reconcile refuses them.
kubectl -n "$NS" create secret generic provisioner-agent-vault \
  --from-literal=AGENT_VAULT_TOKEN="$TOKEN" \
  --from-file=CLAUDE_CODE_OAUTH_TOKEN="$MODEL_TOKEN_FILE" \
  --from-literal=GH_GAME_UKUBI_GAME=local-placeholder \
  --from-literal=GH_OWNER_MOHAMMADBNEI=local-placeholder \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
echo "vault-bootstrap: provisioner-agent-vault written"
# The rotation revoked the old token at once; a running provisioner reads the
# mounted file, which the kubelet refreshes only after its sync period.
if kubectl -n "$NS" get deploy/provisioner >/dev/null 2>&1; then
  kubectl -n "$NS" rollout restart deploy/provisioner >/dev/null
  echo "vault-bootstrap: provisioner restarted onto the new token"
fi
