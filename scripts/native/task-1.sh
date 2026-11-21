#!/usr/bin/env bash
set -e
# A relative HUB_CONFIG is relative to the caller's directory, not apps/server.
if [[ -n "${HUB_CONFIG:-}" && "$HUB_CONFIG" != /* && -e "$HUB_CONFIG" ]]; then
  HUB_CONFIG="$(cd "$(dirname "$HUB_CONFIG")" && pwd)/$(basename "$HUB_CONFIG")"
fi
cd "$(dirname "$0")/../.."
cd apps/server && go run ./cmd/server -config "${HUB_CONFIG:-config/example.yaml}"
