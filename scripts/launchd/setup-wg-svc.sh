#!/usr/bin/env bash
# One-shot installer for the polar-wg-svc launchd auto-start. Run on
# the deploy box; idempotent.
#
#   bash scripts/launchd/setup-wg-svc.sh
#
# Steps:
#   1. Copy the wrapper to ~/.local/bin/polar-wg-svc-launch.sh
#   2. Copy the plist to ~/Library/LaunchAgents/polar.wg-svc.plist
#   3. Bootstrap ~/wg-svc.env from the sample IF it doesn't exist
#      yet (operator fills in real values + chmod 600)
#   4. launchctl load -w to start + auto-start at login
#
# Prerequisites:
#   - Plugin row provisioned via /admin-plugins.html on dock (capture
#     the one-time plaintext into POLAR_PLUGIN_TOKEN in the env file)
#   - polar_wg database created + scripts/migrate/wg-schema.sql applied
#   - wg-svc binary present at $HOME/.local/bin/wg-svc

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
LOCAL_BIN="$HOME/.local/bin"
LAUNCH_AGENTS="$HOME/Library/LaunchAgents"
ENV_FILE="$HOME/wg-svc.env"

mkdir -p "$LOCAL_BIN" "$LAUNCH_AGENTS"

echo "→ installing wrapper at $LOCAL_BIN/polar-wg-svc-launch.sh"
install -m 0755 "$SCRIPT_DIR/polar-wg-svc-launch.sh" "$LOCAL_BIN/polar-wg-svc-launch.sh"

echo "→ installing plist at $LAUNCH_AGENTS/polar.wg-svc.plist"
install -m 0644 "$SCRIPT_DIR/polar.wg-svc.plist" "$LAUNCH_AGENTS/polar.wg-svc.plist"
plutil -lint "$LAUNCH_AGENTS/polar.wg-svc.plist"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "→ $ENV_FILE not found — bootstrapping from sample (you must fill in real values + chmod 600)"
  install -m 0600 "$SCRIPT_DIR/wg-svc.env.sample" "$ENV_FILE"
  echo ""
  echo "  ! Provision a plugin row via /admin-plugins.html and paste"
  echo "    the printed plaintext into POLAR_PLUGIN_TOKEN in $ENV_FILE."
  echo "    Re-use dock's METRICS_TOKEN for POLAR_WG_METRICS_TOKEN if you"
  echo "    want Prometheus to scrape both with one bearer."
  echo ""
else
  echo "→ $ENV_FILE already present, leaving untouched"
fi

echo "→ launchctl unload + reload (idempotent)"
launchctl unload "$LAUNCH_AGENTS/polar.wg-svc.plist" 2>/dev/null || true
launchctl load -w "$LAUNCH_AGENTS/polar.wg-svc.plist"

sleep 3
if pgrep -fl wg-svc >/dev/null; then
  echo "✓ wg-svc is running under launchd."
  echo "  log: tail -f /tmp/polar.wg-svc.log"
  echo "  smoke: curl -s http://127.0.0.1:8090/healthz"
else
  echo "✗ wg-svc did not start. Check /tmp/polar.wg-svc.log + ~/wg-svc.env."
  exit 1
fi
