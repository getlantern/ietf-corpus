# deploy/

LaunchAgent plists + wrapper scripts that run the `ietf-elements`
extractor on a Mac (the Lantern Mac mini, in practice). All paths in
the plists assume a checkout at `/Users/afisk/code/ietf-corpus` —
adjust via the installer's `REPO=` env var or pass a different repo
path before installing.

## Why launchd LaunchAgent and not a system daemon

`ietf-elements` shells out to `claude -p`. The Claude Code CLI reads
its OAuth token from the macOS keychain, which is only unlocked in
the user's GUI Aqua session. **LaunchAgents** (per-user, under
`~/Library/LaunchAgents/`) run in that session and can read it.
**LaunchDaemons** (system-wide, under `/Library/LaunchDaemons/`)
cannot. The plists in this directory are written as LaunchAgents
specifically for this reason.

Equally, the user account that installs these must be logged in
with the keychain unlocked — typically the auto-login user on a
dedicated bot machine.

## Files

| File | Purpose |
| --- | --- |
| `run-elements.sh` | Self-updating wrapper: `git pull`, `go install ./cmd/ietf-elements ./cmd/ietf-mcp`, ad-hoc codesigns both, then runs the three standards-track sweeps (INTERNET STANDARD → DRAFT STANDARD → PROPOSED STANDARD) in sequence. Already-extracted RFCs are auto-skipped. |
| `commit-elements.sh` | Batch-commits any newly-extracted `corpus/elements/*.yaml` directly to main and pushes. Idempotent — no-op when there's nothing new. Safe because it only adds files (never edits or deletes), CI gates the integrity tests on every push, and rebases-and-retries on push rejection. |
| `io.lantern.ietf-elements-backfill.plist` | One-shot LaunchAgent. No schedule; runs only when explicitly started. For the initial backfill. |
| `io.lantern.ietf-elements-weekly.plist` | Recurring LaunchAgent. Fires every Sunday at 03:00 local time. Maintenance — extracts elements for newly-published standards-track RFCs since the last fire. |
| `io.lantern.ietf-elements-commit.plist` | Recurring LaunchAgent. Fires every 10 minutes. Calls `commit-elements.sh`. Decoupled from the extractor so it works with a sweep in progress without requiring a restart. |
| `io.lantern.ietf-mcp-serve.plist` | LaunchAgent that runs `ietf-mcp --serve 127.0.0.1:8789` with `KeepAlive=true`. Exposes `/healthz`, `/ask`, and `/mcp`. The cloudflared tunnel (`ietf-ask.lantern.io`) forwards to this. Requires `IETF_ASK_TOKEN` from a local token file. |
| `install-launchd.sh` | One-time installer. Substitutes the right repo path into the plists and `launchctl load`s them. Accepts `backfill`, `weekly`, `commit`, `mcp-serve`, or `all` (default) as args. |

## Installing on the mini

Assuming the repo is checked out at `/Users/afisk/code/ietf-corpus`
(the conventional bot-machine path):

```bash
cd /Users/afisk/code/ietf-corpus
git pull
# First-time only: drop the /ask backend token. Then install all agents.
mkdir -p ~/.config/lantern
echo "$(openssl rand -hex 24)" > ~/.config/lantern/ietf-ask-token
chmod 600 ~/.config/lantern/ietf-ask-token
bash deploy/install-launchd.sh all
```

For just the /ask backend (skip the element-extraction agents):

```bash
bash deploy/install-launchd.sh mcp-serve
```

Or for a non-standard checkout location:

```bash
REPO=$HOME/somewhere/ietf-corpus bash deploy/install-launchd.sh both
```

## Triggering the initial backfill

After install, kick off the one-shot sweep:

```bash
launchctl start io.lantern.ietf-elements-backfill
```

A full standards-track sweep against an empty `corpus/elements/`
processes ~4,648 RFCs and takes roughly 3 days at `PARALLEL=3` (the
default). To override:

```bash
PARALLEL=6 launchctl start io.lantern.ietf-elements-backfill
```

Note: raising parallelism past 3 can hit Claude rate limits on the
subscription tier. The default is set conservatively.

## Watching progress

```bash
tail -f /tmp/ietf-elements-backfill.err.log
```

Output for the weekly job goes to
`/tmp/ietf-elements-weekly.err.log`.

## Committing the extracted elements

`io.lantern.ietf-elements-commit` (loaded by default via
`install-launchd.sh all`) fires every 10 minutes and pushes any new
elements directly to `main`. The commit author is
`ietf-corpus-bot <bot@lantern.io>` so `git log --author=ietf-corpus-bot`
isolates the auto-ingest stream.

Why direct-to-main is safe:

- The job only *adds* files in `corpus/elements/`; it never edits or
  deletes anything else.
- The full CI suite (`go build`, `go vet`, `go test`, site smoke
  render) runs on every push to `main`, so any schema-breaking
  output gets caught.
- On push rejection (someone else pushed concurrently), the job
  rebases and retries once.
- The next CF Pages deploy picks up the new elements automatically
  via git integration.

If you'd rather review batches in PRs, unload the commit plist and
fall back to manual:

```bash
launchctl unload ~/Library/LaunchAgents/io.lantern.ietf-elements-commit.plist
# ... then manually:
cd /Users/afisk/code/ietf-corpus
git checkout -b auto-ingest/elements-backfill-$(date +%F)
git add corpus/elements/
git commit -m "elements: backfill from standards-track sweep $(date +%F)"
gh pr create --fill
```

## Stopping or removing

To unload a single agent without deleting the plist:

```bash
launchctl unload ~/Library/LaunchAgents/io.lantern.ietf-elements-backfill.plist
```

To stop a running invocation (without unloading the agent):

```bash
launchctl stop io.lantern.ietf-elements-backfill
```

To fully remove:

```bash
launchctl unload ~/Library/LaunchAgents/io.lantern.ietf-elements-backfill.plist
rm ~/Library/LaunchAgents/io.lantern.ietf-elements-backfill.plist
```

Same pattern for the weekly plist.
