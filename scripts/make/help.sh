#!/usr/bin/env bash
set -euo pipefail

# Script: help.sh
# Purpose: Render `make help` from the Makefile itself, so EVERY documented
#          target shows up automatically. The old help was a hand-maintained
#          echo block that silently drifted out of sync with the targets (a new
#          target -- e.g. vscode-install -- was added without being listed).
#          Deriving the menu from the source of truth makes that drift structurally
#          impossible: document a target and it appears; there is no second list
#          to forget.
#
#          `--check` gates the other half of that guarantee: it exits non-zero if
#          any target LACKS a doc comment (and so would be silently dropped from
#          the menu). A Go test (scripts/make/help_test.go) runs this on every
#          `make test`, so "every command shows up" is enforced, not just hoped for.
#
# Convention parsed from the Makefile:
#   ##@ Section        -> a section header
#   ##> free-form note -> an indented note line (context, not a target)
#   ## Description     -> the summary for the target on the NEXT line. Only the
#                         FIRST '## ' line of a contiguous block is used; the rest
#                         are the Makefile's own longer-form source docs and are
#                         intentionally ignored here.
#   target:            -> printed with its pending description (render), or flagged
#                         when it has none (check).

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MAKEFILE="$REPO_ROOT/Makefile"
VERSION="$(cat "$REPO_ROOT/VERSION" 2>/dev/null || echo dev)"

function show_help() {
    cat <<EOF
Usage: $0 [--check]

  (no args)  Render the grouped \`make help\` menu.
  --check    Exit non-zero if any Makefile target lacks a '## ' doc comment
             (such a target would be silently missing from the menu).
EOF
}

# scan runs the shared Makefile parser in one of two modes:
#   render -> print the grouped menu
#   check  -> print (to stdout) the name of every target with no '## ' summary
# Keeping ONE parser means render and check can never disagree about what counts
# as a target or where a description comes from.
function scan() {
    local mode="$1"
    awk -v mode="$mode" '
        # Section header.
        /^##@/  { desc = ""; if (mode != "check") printf "\n%s\n", substr($0, 5); next }
        # Free-form note line under the current section.
        /^##> / { if (mode != "check") printf "    %s\n", substr($0, 5); next }
        # First "## " line of a block is the summary; ignore continuation lines.
        /^## /  { if (desc == "") desc = substr($0, 4); next }
        # Any other "##" line (a bare "##" paragraph separator, "##x") is still a
        # doc-comment continuation: skip it WITHOUT clearing the pending summary,
        # or a blank line mid-block would drop the summary before its target.
        /^##/   { next }
        # Variable assignments (FOO := bar, FOO=bar, FOO?=bar, FOO+=bar, incl. the
        # space-less form) are NOT targets -- skip so one under a doc comment can
        # never render as a phantom target.
        /^[A-Za-z_][A-Za-z0-9_.-]*[ \t]*[:+?]?=/ { desc = ""; next }
        # A real target line. Leading [a-zA-Z] already excludes .PHONY and dotted
        # special targets.
        /^[a-zA-Z][a-zA-Z0-9_.-]*:/ {
            name = substr($1, 1, index($1, ":") - 1)
            if (mode == "check") {
                if (desc == "") print name
            } else if (desc != "") {
                printf "  %-22s %s\n", name, desc
            }
            desc = ""
            next
        }
        # Anything else (blank line, recipe body, .PHONY) clears a pending
        # summary so it never leaks onto an unrelated target.
        { desc = "" }
    ' "$MAKEFILE"
}

function render() {
    echo "memQL Makefile -- v${VERSION}"
    echo "Common: make up | make dev | make test | make vscode-install"
    scan render
}

# check prints the undocumented targets and exits non-zero if there are any.
function check() {
    local missing
    missing="$(scan check)"
    if [[ -n "$missing" ]]; then
        {
            echo "ERROR: these Makefile targets have no '## ' doc comment and would be missing from \`make help\`:"
            echo "$missing" | sed 's/^/  - /'
            echo "Fix: add a '## <one-line summary>' comment on the line directly above each target."
        } >&2
        return 1
    fi
}

function main() {
    case "${1:-}" in
        --check) check ;;
        --help|-h) show_help ;;
        "") render ;;
        *) echo "ERROR: unknown option: $1" >&2; show_help >&2; exit 2 ;;
    esac
}

main "$@"
