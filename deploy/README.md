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
| `run-elements.sh` | Self-updating wrapper: `git pull`, `go install ./cmd/ietf-elements`, then runs the three standards-track sweeps (INTERNET STANDARD → DRAFT STANDARD → PROPOSED STANDARD) in sequence. Already-extracted RFCs are auto-skipped. |
| `io.lantern.ietf-elements-backfill.plist` | One-shot LaunchAgent. No schedule; runs only when explicitly started. For the initial backfill. |
| `io.lantern.ietf-elements-weekly.plist` | Recurring LaunchAgent. Fires every Sunday at 03:00 local time. Maintenance — extracts elements for newly-published standards-track RFCs since the last fire. |
| `install-launchd.sh` | One-time installer. Substitutes the right repo path into the plists and `launchctl load`s them. |

## Installing on the mini

Assuming the repo is checked out at `/Users/afisk/code/ietf-corpus`
(the conventional bot-machine path):

```bash
cd /Users/afisk/code/ietf-corpus
git pull
bash deploy/install-launchd.sh both
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

The launchd job only *extracts*. It does not commit or push. To
batch-commit the extracted YAMLs after a sweep:

```bash
cd /Users/afisk/code/ietf-corpus
git checkout -b auto-ingest/elements-backfill-$(date +%F)
git add corpus/elements/
git commit -m "elements: backfill from standards-track sweep $(date +%F)"
gh pr create --fill
```

(A `scripts/elements-backfill-pr.sh` analogue to the circumvention-
corpus one will land when there's a routine for it.)

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
