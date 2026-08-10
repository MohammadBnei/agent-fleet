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
exec bun run src/index.ts
