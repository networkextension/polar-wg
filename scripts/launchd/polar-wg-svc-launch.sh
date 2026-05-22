#!/usr/bin/env bash
# Wrapper that launchd's polar.wg-svc plist invokes. Sources per-box
# secrets from ~/wg-svc.env, then exec's the wg-svc binary. Keep
# this committable — no secrets, no per-box paths.
#
# See scripts/launchd/wg-svc.env.sample for the env file shape.

set -euo pipefail

ENV_FILE="${POLAR_WG_SVC_ENV:-$HOME/wg-svc.env}"
if [[ -f "$ENV_FILE" ]]; then
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  # Re-export so the child sees them — `source` makes them shell vars
  # but not env vars in non-interactive runs.
  set -a
  source "$ENV_FILE"
  set +a
fi

BIN="${POLAR_WG_SVC_BIN:-$HOME/.local/bin/wg-svc}"
cd "${POLAR_WG_SVC_WORKDIR:-$HOME}"
exec "$BIN"
