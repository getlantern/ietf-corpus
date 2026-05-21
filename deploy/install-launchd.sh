#!/bin/bash
#
# Install the ietf-elements launchd agents on the current machine
# (intended for the Mac mini that runs Lantern's bot jobs).
#
# Why launchd LaunchAgents specifically: claude -p reads its OAuth
# token from the macOS keychain, which is only unlocked in the user's
# GUI Aqua session. A LaunchAgent runs in that session and can read
# the keychain; a LaunchDaemon (system-wide) cannot.
#
# The plists in deploy/ hard-code /Users/afisk/code/ietf-corpus as
# the repo path (matches the mini's checkout). This script
# substitutes the right path at install time so other machines (laptops,
# fresh boxes, whoever-the-next-maintainer-is's machine) work too.
#
# Usage:
#   bash deploy/install-launchd.sh
#       Installs both backfill (one-shot) and weekly plists.
#
#   bash deploy/install-launchd.sh backfill
#       Just the one-shot backfill plist.
#
#   bash deploy/install-launchd.sh weekly
#       Just the weekly maintenance plist.
#
#   REPO=~/code/ietf-corpus bash deploy/install-launchd.sh
#       Use a custom repo path.
#
# Reload after upstream plist edits: re-run this script.

set -euo pipefail

REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
if [[ ! -x "$REPO/deploy/run-elements.sh" ]]; then
    echo "error: $REPO/deploy/run-elements.sh not found or not executable" >&2
    echo "       set REPO=... to point at your ietf-corpus checkout" >&2
    exit 1
fi

AGENTS_DIR="$HOME/Library/LaunchAgents"
mkdir -p "$AGENTS_DIR"

# Default: install all three.
plists=("io.lantern.ietf-elements-backfill" "io.lantern.ietf-elements-weekly" "io.lantern.ietf-elements-commit")
if [[ $# -gt 0 ]]; then
    plists=()
    for arg in "$@"; do
        case "$arg" in
            backfill|one-shot)
                plists+=("io.lantern.ietf-elements-backfill") ;;
            weekly|maintenance)
                plists+=("io.lantern.ietf-elements-weekly") ;;
            commit|push)
                plists+=("io.lantern.ietf-elements-commit") ;;
            all)
                plists=("io.lantern.ietf-elements-backfill" "io.lantern.ietf-elements-weekly" "io.lantern.ietf-elements-commit") ;;
            both)
                plists=("io.lantern.ietf-elements-backfill" "io.lantern.ietf-elements-weekly") ;;
            *)
                echo "unknown plist: $arg (want: backfill | weekly | commit | all)" >&2
                exit 1
                ;;
        esac
    done
fi

# /Users/afisk/code/ietf-corpus is the hard-coded path in the committed
# plists; substitute the actual REPO at install time.
SED_FROM='/Users/afisk/code/ietf-corpus'

for label in "${plists[@]}"; do
    src="$REPO/deploy/$label.plist"
    dst="$AGENTS_DIR/$label.plist"
    if [[ ! -f "$src" ]]; then
        echo "skipping $label: $src not in repo" >&2
        continue
    fi
    echo "installing $label → $dst (REPO=$REPO)"
    # Unload first if already loaded — silent on missing.
    launchctl unload "$dst" 2>/dev/null || true
    sed "s|$SED_FROM|$REPO|g" "$src" > "$dst"
    launchctl load "$dst"
    echo "  loaded: $(launchctl list "$label" 2>/dev/null | head -1 || echo '(not visible)')"
done

echo
echo "Done. Trigger jobs manually now (without waiting for their schedule):"
echo
for label in "${plists[@]}"; do
    case "$label" in
        *backfill*)
            echo "  launchctl start $label    # ~3 days for the full standards-track sweep"
            ;;
        *weekly*)
            echo "  launchctl start $label    # smoke-test the weekly job"
            ;;
        *commit*)
            echo "  launchctl start $label    # smoke-test the commit-and-push job"
            ;;
    esac
done
echo
echo "Logs:"
echo "  tail -f /tmp/ietf-elements-backfill.err.log"
echo "  tail -f /tmp/ietf-elements-weekly.err.log"
echo "  tail -f /tmp/ietf-elements-commit.err.log"
