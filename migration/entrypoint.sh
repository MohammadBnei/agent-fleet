#!/bin/sh
# URL-encodes AGENTFLEET_DB_* before assembling the postgres:// DSN so
# unescaped reserved characters (@, :, /, etc.) in the password can't be
# misparsed as part of the host/port by migrate's net/url-based parser.
set -eu

enc() { jq -rn --arg s "$1" '$s|@uri'; }

exec migrate \
  -path /migrations \
  -database "postgres://$(enc "$AGENTFLEET_DB_USER"):$(enc "$AGENTFLEET_DB_PASSWORD")@${AGENTFLEET_DB_HOST}:${AGENTFLEET_DB_PORT}/$(enc "$AGENTFLEET_DB_NAME")?sslmode=disable" \
  "$@"
