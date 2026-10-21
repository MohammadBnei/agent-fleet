#!/bin/sh
# Copies the plugins worker/Dockerfile baked into the image's default
# $HOME (/root/.claude/plugins) onto the PVC-backed $CLAUDE_CONFIG_DIR,
# once — Claude Code reads plugins from $CLAUDE_CONFIG_DIR at runtime, not
# from where the image installed them (docs/adr/0033). No network call, no
# `claude plugin add`/`install` here; the guard means only the first pod
# to touch a given PVC does the copy.
#
# ponytail: a concurrent first-boot race (two pods copying simultaneously)
# is unguarded — harmless here since both copies come from the same
# read-only image layer, so a lock isn't worth adding.
set -e
if [ -n "$CLAUDE_CONFIG_DIR" ] && [ ! -d "$CLAUDE_CONFIG_DIR/plugins" ]; then
  mkdir -p "$CLAUDE_CONFIG_DIR"
  cp -r /root/.claude/plugins "$CLAUDE_CONFIG_DIR/plugins"
fi
# docs/adr/0075: Chromium ignores the CA env vars and trusts only its NSS db,
# so the vault's root goes in there. $HOME is the container's own layer, so
# every start. Best-effort: without it only the browser fails TLS.
if [ -f /etc/agent-vault/ca.pem ] && command -v certutil >/dev/null; then
  mkdir -p "$HOME/.pki/nssdb"
  { [ -f "$HOME/.pki/nssdb/cert9.db" ] || certutil -d "sql:$HOME/.pki/nssdb" -N --empty-password; } &&
    certutil -d "sql:$HOME/.pki/nssdb" -A -t "C,," -n agent-vault -i /etc/agent-vault/ca.pem ||
    echo "entrypoint: vault CA not imported into NSS, the browser will reject TLS" >&2
fi
exec bun run src/index.ts
