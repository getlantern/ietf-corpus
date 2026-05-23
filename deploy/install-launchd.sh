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
#       Installs all four: backfill (one-shot), weekly, commit, mcp-serve.
#
#   bash deploy/install-launchd.sh backfill
#       Just the one-shot backfill plist.
#
#   bash deploy/install-launchd.sh weekly
#       Just the weekly maintenance plist.
#
#   bash deploy/install-launchd.sh commit
#       Just the auto-commit plist.
#
#   bash deploy/install-launchd.sh mcp-serve
#       Just the HTTP serve mode for the /ask + /mcp endpoints.
#
#   REPO=~/code/ietf-corpus bash deploy/install-launchd.sh
#       Use a custom repo path.
#
# Token file: ietf-mcp-serve requires IETF_ASK_TOKEN. If
# $HOME/.config/lantern/ietf-ask-token (or $REPO/deploy/.ietf-ask-token,
# or $TOKEN_FILE) exists, this script injects the token into the
# rendered plist's EnvironmentVariables. Same pattern as the
# circumvention-corpus install-launchd.sh.
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

# Default: install all four.
DEFAULTS=("io.lantern.ietf-elements-backfill" "io.lantern.ietf-elements-weekly" "io.lantern.ietf-elements-commit" "io.lantern.ietf-mcp-serve")
plists=("${DEFAULTS[@]}")
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
            mcp-serve|serve|ask)
                plists+=("io.lantern.ietf-mcp-serve") ;;
            all)
                plists=("${DEFAULTS[@]}") ;;
            *)
                echo "unknown plist: $arg (want: backfill | weekly | commit | mcp-serve | all)" >&2
                exit 1
                ;;
        esac
    done
fi

# Two path substitutions at install:
#   /Users/afisk/code/ietf-corpus → $REPO
#   /Users/afisk/go/bin           → $GOBIN
SED_REPO='/Users/afisk/code/ietf-corpus'
SED_GOBIN='/Users/afisk/go/bin'
: "${GOBIN:=$HOME/go/bin}"

# IETF_ASK_TOKEN injection for ietf-mcp-serve. The token is a
# per-machine secret that mustn't enter git. If a token file exists
# at the standard location (or $TOKEN_FILE), inject it into the
# rendered plist via plutil. Without it the LaunchAgent fails to
# start with "--auth-token (or env IETF_ASK_TOKEN) is required".
: "${TOKEN_FILE:=}"
if [[ -z "$TOKEN_FILE" ]]; then
    for candidate in "$HOME/.config/lantern/ietf-ask-token" "$REPO/deploy/.ietf-ask-token"; do
        if [[ -f "$candidate" ]]; then
            TOKEN_FILE="$candidate"
            break
        fi
    done
fi
TOKEN_VALUE=""
if [[ -n "$TOKEN_FILE" && -f "$TOKEN_FILE" ]]; then
    TOKEN_VALUE="$(head -n 1 "$TOKEN_FILE" | tr -d '[:space:]')"
    if [[ -n "$TOKEN_VALUE" ]]; then
        echo "  (token file found: $TOKEN_FILE — will inject into ietf-mcp-serve)"
    fi
fi

for label in "${plists[@]}"; do
    src="$REPO/deploy/$label.plist"
    dst="$AGENTS_DIR/$label.plist"
    if [[ ! -f "$src" ]]; then
        echo "skipping $label: $src not in repo" >&2
        continue
    fi
    echo "installing $label → $dst (REPO=$REPO, GOBIN=$GOBIN)"
    # Unload first if already loaded — silent on missing.
    launchctl unload "$dst" 2>/dev/null || true
    sed -e "s|$SED_REPO|$REPO|g" \
        -e "s|$SED_GOBIN|$GOBIN|g" \
        "$src" > "$dst"
    # Inject IETF_ASK_TOKEN for mcp-serve only.
    if [[ "$label" == "io.lantern.ietf-mcp-serve" && -n "$TOKEN_VALUE" ]]; then
        if ! /usr/bin/plutil -insert EnvironmentVariables.IETF_ASK_TOKEN -string "$TOKEN_VALUE" "$dst" 2>/dev/null; then
            /usr/bin/plutil -replace EnvironmentVariables.IETF_ASK_TOKEN -string "$TOKEN_VALUE" "$dst"
        fi
        echo "  (IETF_ASK_TOKEN baked into plist)"
    fi
    # Ad-hoc codesign $GOBIN/ietf-mcp at install time so the first
    # launch doesn't fail with Launch Constraint Violation. The
    # weekly run-elements.sh keeps it signed afterwards.
    if [[ "$label" == "io.lantern.ietf-mcp-serve" && -x "$GOBIN/ietf-mcp" ]]; then
        codesign --sign - --force --timestamp=none "$GOBIN/ietf-mcp" 2>/dev/null \
            && echo "  (ad-hoc codesigned $GOBIN/ietf-mcp)" \
            || echo "  (WARNING: codesign failed; LaunchAgent may not spawn)"
    fi
    launchctl load "$dst"
    echo "  loaded: $(launchctl list "$label" 2>/dev/null | head -1 || echo '(not visible)')"
done

if [[ " ${plists[*]} " == *" io.lantern.ietf-mcp-serve "* && -z "$TOKEN_VALUE" ]]; then
    echo
    echo "WARNING: ietf-mcp-serve was installed without an IETF_ASK_TOKEN."
    echo "         It will fail at startup with --auth-token errors."
    echo "         Fix:"
    echo "             mkdir -p ~/.config/lantern"
    echo "             echo '<token>' > ~/.config/lantern/ietf-ask-token"
    echo "             chmod 600 ~/.config/lantern/ietf-ask-token"
    echo "         then re-run this script. Generate a token with:"
    echo "             openssl rand -hex 24"
fi

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
        *mcp-serve*)
            echo "  launchctl start $label    # serve mode auto-starts via RunAtLoad; smoke-test:"
            echo "      curl http://127.0.0.1:8789/healthz"
            ;;
    esac
done
echo
echo "Logs:"
echo "  tail -f /tmp/ietf-elements-backfill.err.log"
echo "  tail -f /tmp/ietf-elements-weekly.err.log"
echo "  tail -f /tmp/ietf-elements-commit.err.log"
echo "  tail -f /tmp/ietf-mcp-serve.err.log"
