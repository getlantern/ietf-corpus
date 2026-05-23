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
# parallel=1 default per the May 2026 quota-burn incident (see
# runClaude doc in cmd/ietf-elements/main.go). Override via env if
# you're sure the machine isn't running competing claude work.
PARALLEL="${PARALLEL:-1}"
# Hard cap so even a malfunctioning extractor can't burn the
# subscription. 60/hour ≈ ~1500/day at parallel=1, well within
# subscription headroom while still finishing the standards-track
# backlog in ~3 days on the happy path.
MAX_PER_HOUR="${MAX_PER_HOUR:-60}"
# Abort if N back-to-back claude calls fail. The previous incident
# burned 17 unattended hours at no cap; 10 is conservative.
MAX_CONSECUTIVE_FAILS="${MAX_CONSECUTIVE_FAILS:-10}"

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

# 2. Install ietf-elements + ietf-mcp via `go install`.
#    ietf-elements: the extractor itself (CLI, no launchd direct invocation).
#    ietf-mcp:      the MCP server. The HTTP serve mode is invoked
#                   directly by io.lantern.ietf-mcp-serve.plist, so the
#                   $GOBIN binary needs to stay codesigned (next step)
#                   on every fire — `go install` strips signatures.
stamp "installing ietf-elements + ietf-mcp → $GOBIN"
install_log=$(mktemp -t ietf-elements-install)
if go install ./cmd/ietf-elements/ ./cmd/ietf-mcp/ 2>"$install_log"; then
    stamp "install ok"
    rm -f "$install_log"
    # Ad-hoc codesign so macOS 26.2+ launchd will spawn the binaries.
    # Same lesson as the 2026-05-22 circumvention-corpus outage:
    # unsigned Go binaries get killed by launchd's CODESIGNING gate.
    for bin in ietf-elements ietf-mcp; do
        if codesign --sign - --force --timestamp=none "$GOBIN/$bin" 2>/dev/null; then
            stamp "codesign ok: $bin"
        else
            stamp "codesign FAILED: $bin (LaunchAgent may not spawn it)"
        fi
    done
else
    stamp "install FAILED — using existing binary if present"
    sed 's/^/    /' "$install_log"
    rm -f "$install_log"
fi

if [[ ! -x "$BINARY" ]]; then
    stamp "no executable binary at $BINARY; aborting"
    exit 1
fi

# 3. SELFTEST GATE. Run three canary extractions before kicking off
# the long sweep. If selftest fails, bail without consuming subscription
# quota on what would just be a torrent of retries. The May 2026
# incident would have caught itself in ~10 minutes instead of 17 hours
# with this gate in place.
stamp "=== selftest gate ==="
if ! "$BINARY" selftest --corpus "$REPO"; then
    stamp "selftest FAILED — aborting sweep. Diagnose claude / network / quota before re-triggering."
    exit 1
fi

# 4. Run the three standards-track sweeps in sequence. extract-all
# auto-skips RFCs that already have elements, so re-running is cheap
# (a directory scan, no LLM cost) for the steady-state path.
#
# Order: INTERNET STANDARD first (smallest, most settled, highest
# signal per call) → DRAFT STANDARD → PROPOSED STANDARD (largest).
# If the job is interrupted mid-PROPOSED-STANDARD, the next fire
# resumes from where it left off via the same auto-skip mechanism.
# The extractor's own --max-consecutive-fails guard tears down the
# sweep early if claude starts failing in a streak, so an unattended
# 3-day run can't silently burn quota.
for status in "INTERNET STANDARD" "DRAFT STANDARD" "PROPOSED STANDARD"; do
    stamp "=== sweep: status='$status' ==="
    if ! "$BINARY" extract-all \
        --corpus "$REPO" \
        --status "$status" \
        --parallel "$PARALLEL" \
        --max-per-hour "$MAX_PER_HOUR" \
        --max-consecutive-fails "$MAX_CONSECUTIVE_FAILS"; then
        stamp "sweep '$status' aborted (likely hit the consecutive-fails guard); stopping"
        exit 1
    fi
done

stamp "=== all sweeps complete ==="
