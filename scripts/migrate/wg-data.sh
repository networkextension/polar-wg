#!/usr/bin/env bash
# wg-data.sh — copy wg_* tables from `ideamesh` (dock's DB) into
# `polar_wg` (wg-svc's DB). Run AFTER scripts/migrate/wg-schema.sql
# has provisioned the target tables.
#
# This script is read-only on the source: pg_dump just snapshots the
# data, dock keeps running. The cutover (when dock stops writing the
# wg_* tables) happens in Phase 1-D via POLAR_WG_REMOTE=true; this
# script is the one-time "seed polar_wg with current state" step.
#
# Usage:
#   SRC_DSN="postgres://ideamesh:test123456@127.0.0.1:5432/ideamesh"
#   DST_DSN="postgres://ideamesh:test123456@127.0.0.1:5432/polar_wg"
#   ./scripts/migrate/wg-data.sh                # dry-run: prints counts
#   ./scripts/migrate/wg-data.sh --apply        # actually copies
#
# Idempotent: each table is TRUNCATE'd in polar_wg before the dump is
# loaded, so re-running this picks up new rows in ideamesh. The
# sequences are also reset so future INSERTs in polar_wg pick up where
# ideamesh left off.

set -euo pipefail

SRC_DSN="${SRC_DSN:-postgres://ideamesh:test123456@127.0.0.1:5432/ideamesh}"
DST_DSN="${DST_DSN:-postgres://ideamesh:test123456@127.0.0.1:5432/polar_wg}"
PSQL="${PSQL:-/Applications/Postgres.app/Contents/Versions/latest/bin/psql}"
PG_DUMP="${PG_DUMP:-/Applications/Postgres.app/Contents/Versions/latest/bin/pg_dump}"

# Apply mode flag — default is dry-run.
APPLY=0
if [[ "${1:-}" == "--apply" ]]; then
    APPLY=1
fi

# Order matters — parents before children so FKs resolve.
TABLES=(
    wg_hubs
    wg_sites
    wg_tokens
    wg_devices
    wg_bundles
    wg_heartbeats
)

echo "=== wg-data.sh — $(if [[ $APPLY -eq 1 ]]; then echo APPLY; else echo DRY-RUN; fi) ==="
echo "source: $SRC_DSN"
echo "target: $DST_DSN"
echo

echo "--- source row counts ---"
for t in "${TABLES[@]}"; do
    n=$("$PSQL" "$SRC_DSN" -At -c "SELECT COUNT(*) FROM $t;" 2>/dev/null || echo "ERR")
    printf "  %-15s %s\n" "$t" "$n"
done
echo

if [[ $APPLY -eq 0 ]]; then
    echo "Dry run — pass --apply to perform the copy."
    echo "Pre-flight: ensure scripts/migrate/wg-schema.sql is applied to target."
    exit 0
fi

# pg_dump --data-only --inserts so we can pipe directly into psql on
# the target. --column-inserts is slower but tolerates schema column
# additions on the target (polar_wg may grow columns wg-svc adds).
TMPDIR=$(mktemp -d -t wgmigrate)
trap 'rm -rf "$TMPDIR"' EXIT

echo "--- dumping source data ---"
DUMP="$TMPDIR/wg-data.sql"
"$PG_DUMP" "$SRC_DSN" \
    --data-only \
    --column-inserts \
    --no-owner \
    --no-privileges \
    $(printf -- '--table=%s ' "${TABLES[@]}") \
    > "$DUMP"
echo "wrote $(wc -l < "$DUMP") lines to $DUMP"
echo

echo "--- truncating target + loading ---"
{
    # TRUNCATE CASCADE gives us a clean idempotent reset. The dump
    # carries its own setval() calls for every sequence (pg_dump
    # default behavior), so we don't need to re-sync sequences
    # afterwards. Earlier versions of this script did and tripped
    # over pg_dump's `SET search_path = ''` — left to its own dump,
    # everything resolves through pg_catalog.
    echo "BEGIN;"
    for t in "${TABLES[@]}"; do
        echo "TRUNCATE $t RESTART IDENTITY CASCADE;"
    done
    cat "$DUMP"
    echo "COMMIT;"
} | "$PSQL" "$DST_DSN" -v ON_ERROR_STOP=1

echo
echo "--- target row counts (post-load) ---"
for t in "${TABLES[@]}"; do
    n=$("$PSQL" "$DST_DSN" -At -c "SELECT COUNT(*) FROM $t;")
    printf "  %-15s %s\n" "$t" "$n"
done
echo
echo "Done. wg-svc can now point POLAR_WG_DB_DSN at $DST_DSN."
