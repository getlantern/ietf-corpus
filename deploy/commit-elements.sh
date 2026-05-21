#!/bin/bash
#
# Batch-commit any new corpus/elements/*.yaml directly to main and
# push. Designed to run on a launchd timer alongside the extractor,
# so a 3-day standards-track sweep doesn't sit on tens of thousands
# of uncommitted YAMLs. Idempotent — a no-op when there's nothing new.
#
# Why this is safe to push to main without a PR:
#   - Only adds files; never edits or deletes existing ones.
#   - The extractor only writes corpus/elements/ — git add is scoped
#     to that directory, so no accidental commit of stray dirs.
#   - The full CI suite (.github/workflows/ci.yaml) runs on push,
#     gating the corpus integrity tests on every commit.
#   - The commit author is the bot identity, so `git log --author`
#     can isolate the auto-ingest stream.
#
# Race safety:
#   - The extractor writes to corpus/elements/<id>__<slug>.yaml as
#     a single os.WriteFile call (atomic on POSIX), so a partial
#     element YAML can't land in a commit.
#   - git add takes a snapshot at the moment of invocation; any
#     element written during a `git add` lands in the next fire.
#   - On `git push` rejection (non-fast-forward), we `git pull
#     --rebase` and retry once. The bot's commits only add new
#     files in corpus/elements/, so rebase is conflict-free in
#     practice.

set -u

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$SCRIPT_DIR/.." && pwd)
BOT_NAME="${BOT_NAME:-ietf-corpus-bot}"
BOT_EMAIL="${BOT_EMAIL:-bot@lantern.io}"

cd "$REPO" || { echo "commit: cannot cd to $REPO; abort" >&2; exit 1; }

stamp() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] commit:" "$@"; }

# Make sure we're on main before doing anything. If the repo got left
# on a feature branch (manual debugging, interrupted manual workflow),
# bail rather than commit there.
current_branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "?")
if [[ "$current_branch" != "main" ]]; then
    stamp "not on main (current=$current_branch); skipping this fire"
    exit 0
fi

# Set bot identity for this repo (idempotent — no-op if already set).
git config user.name "$BOT_NAME"
git config user.email "$BOT_EMAIL"

# Is there anything to commit? Check both staged and untracked in
# corpus/elements/.
new_files=$(git status --porcelain corpus/elements/ 2>/dev/null | wc -l | tr -d ' ')
if [[ "$new_files" -eq 0 ]]; then
    # No new elements — silent no-op. Don't even log on quiet runs.
    exit 0
fi

stamp "$new_files new file(s) in corpus/elements/; staging + committing"

# Stage the new files.
git add corpus/elements/

# Re-count after staging (untracked may have moved into staged).
staged=$(git diff --cached --name-only corpus/elements/ | wc -l | tr -d ' ')
if [[ "$staged" -eq 0 ]]; then
    stamp "nothing to commit after staging (filtered out?); abort"
    exit 0
fi

DATE=$(date -u +%Y-%m-%d)
msg="auto-ingest: ietf-elements backfill — $staged new elements ($DATE)"
git commit -q -m "$msg"

stamp "committed $(git rev-parse --short HEAD): $msg"

# Push. On non-fast-forward rejection, try one rebase + retry.
push_one() {
    git push --quiet origin main 2>&1
}

if ! push_one; then
    stamp "push rejected; pulling --rebase and retrying once"
    if git pull --rebase --quiet origin main 2>&1; then
        if push_one; then
            stamp "push ok after rebase"
        else
            stamp "push still failing after rebase; next fire will retry"
            exit 1
        fi
    else
        stamp "pull --rebase FAILED; manual intervention needed"
        exit 1
    fi
else
    stamp "push ok"
fi
