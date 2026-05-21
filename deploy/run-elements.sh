#!/bin/bash
#
# Self-updating wrapper for `ietf-elements extract-all`. Called by
# launchd in place of invoking the binary directly. Each fire of the
# launchd job does:
#
#   1. git pull --ff-only main. Failure → continue with existing tree
#      (offline run still works against last-known-good source).
#   2. `go install` cmd/ietf-elements into $GOBIN. Failure → continue
#      with existing $GOBIN binary (broken commit on main shouldn't
#      take down the bot).
#   3. Run extract-all three times — once per standards-track status
#      (INTERNET STANDARD, DRAFT STANDARD, PROPOSED STANDARD). Each
#      sweep auto-skips RFCs that already have elements, so weekly
#      maintenance is a no-op for the bulk of the corpus and only
#      processes newly-published standards-track RFCs since last run.
#
# The job is update-proof: any merge to main lands in the next
# launchd fire automatically, without manual rebuild/reinstall.
#
# claude -p reads its OAuth token from the macOS keychain, which is
# only unlocked in the user's GUI Aqua session. This must run as a
# user LaunchAgent (under ~/Library/LaunchAgents/), NOT a system
# LaunchDaemon — daemons can't open the keychain. The plist
# enforces this by living under the user's home directory.
#
# Logs go to launchd's Standard{Out,Error}Path (set in the plist).

set -u

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$SCRIPT_DIR/.." && pwd)

: "${GOBIN:=$HOME/go/bin}"
export GOBIN
BINARY="$GOBIN/ietf-elements"
PARALLEL="${PARALLEL:-3}"

cd "$REPO" || { echo "wrapper: cannot cd to $REPO; abort" >&2; exit 1; }

stamp() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] wrapper:" "$@"; }

# 1. Recover to main + pull.
current_branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null)
if [[ "$current_branch" != "main" ]]; then
    stamp "recovering from branch '$current_branch' → main"
    git checkout --quiet main 2>&1 || stamp "checkout main failed (continuing)"
fi
stamp "pulling latest"
if git pull --ff-only --quiet origin main 2>&1; then
    stamp "pull ok @ $(git rev-parse --short HEAD)"
else
    stamp "pull FAILED; using existing tree @ $(git rev-parse --short HEAD)"
fi

# 2. Install ietf-elements via `go install`.
stamp "installing ietf-elements → $GOBIN"
install_log=$(mktemp -t ietf-elements-install)
if go install ./cmd/ietf-elements/ 2>"$install_log"; then
    stamp "install ok"
    rm -f "$install_log"
else
    stamp "install FAILED — using existing binary if present"
    sed 's/^/    /' "$install_log"
    rm -f "$install_log"
fi

if [[ ! -x "$BINARY" ]]; then
    stamp "no executable binary at $BINARY; aborting"
    exit 1
fi

# 3. Run the three standards-track sweeps in sequence. extract-all
# auto-skips RFCs that already have elements, so re-running is cheap
# (a directory scan, no LLM cost) for the steady-state path.
#
# Order: INTERNET STANDARD first (smallest, most settled, highest
# signal per call) → DRAFT STANDARD → PROPOSED STANDARD (largest).
# If the job is interrupted mid-PROPOSED-STANDARD, the next fire
# resumes from where it left off via the same auto-skip mechanism.
for status in "INTERNET STANDARD" "DRAFT STANDARD" "PROPOSED STANDARD"; do
    stamp "=== sweep: status='$status' ==="
    "$BINARY" extract-all --corpus "$REPO" --status "$status" --parallel "$PARALLEL" || \
        stamp "sweep '$status' returned nonzero (continuing)"
done

stamp "=== all sweeps complete ==="
