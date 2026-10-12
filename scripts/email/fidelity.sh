#!/usr/bin/env bash
set -euo pipefail

# Script: fidelity.sh
# Purpose: Compare a directory of example emails with their renderings by the
#          email layout engine, pixel by pixel, in headless Chrome.
#
# Each example is a subdirectory of --dir holding email.html (the original
# email) and document.json (an email document written by hand to reproduce it).
# The harness renders every document, screenshots the original and the render
# at 600 pixels wide, and reports the share of the content that differs. It
# fails when any example differs by more than --max percent.
#
# The examples are a client's real emails. They live OUTSIDE this repository,
# nothing here copies one into it, and none may be committed. The harness writes
# what it makes (rendered.html, original.png, rendered.png and diff.png) next to
# each example.
#
# The comparison itself is TestFidelityAgainstExamples in
# component/compose/emaildoc/fidelity_test.go, which this runs; the rules it
# compares by are documented there.
#
# A developer tool, not a capability script: it prints for a person, emits no
# JSON envelope, and has no Makefile target.
#
# Exit status: 0 every example is within --max | 1 an example is over --max, or
# the run failed (go test's own status) | 2 bad argument | 4 go or Chrome missing
#
# Portability: this file runs under bash 3.2 too (stock macOS).

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEFAULT_MAX_PCT="2"
# Tried in this order when no Chrome is named. Keep in step with chromeCandidates
# in component/compose/emaildoc/fidelity_test.go.
CHROME_CANDIDATES=("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" "google-chrome" "chromium")

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 --dir=<examples directory> [options]

Compares each example email with its rendering by the layout engine, pixel by
pixel, in headless Chrome at 600 pixels wide, and fails when any example
differs by more than --max percent.

An example is a subdirectory of --dir holding:
    email.html      the original email
    document.json   an email document written by hand to reproduce it; the src
                    of a picture (and the icon of a social link) may be a file,
                    named relative to the example's directory

The harness writes rendered.html, original.png, rendered.png and diff.png (the
differences in red) next to each example. Keep the examples outside this
repository: they are a client's real emails and must not be committed.

Options:
    --dir=DIR       Directory of examples (default: \$MEMQL_EMAIL_FIDELITY_DIR)
    --max=PERCENT   Largest share of an email's content that may differ
                    (default: \$MEMQL_EMAIL_FIDELITY_MAX_PCT, else $DEFAULT_MAX_PCT)
    --chrome=PATH   Chrome or Chromium binary (default:
                    \$MEMQL_EMAIL_FIDELITY_CHROME, else Google Chrome on macOS,
                    else google-chrome or chromium on PATH)
    --help          Show this help message

Examples:
    $0 --dir=/path/to/examples
    $0 --dir=/path/to/examples --max=1 --chrome=/usr/bin/chromium
EOF
}

function parse_arguments() {
    DIR="${MEMQL_EMAIL_FIDELITY_DIR:-}"
    MAX_PCT="${MEMQL_EMAIL_FIDELITY_MAX_PCT:-$DEFAULT_MAX_PCT}"
    CHROME="${MEMQL_EMAIL_FIDELITY_CHROME:-}"

    while [[ $# -gt 0 ]]; do
        case $1 in
            --dir=*)
                DIR="${1#*=}"
                shift
                ;;
            --max=*)
                MAX_PCT="${1#*=}"
                shift
                ;;
            --chrome=*)
                CHROME="${1#*=}"
                shift
                ;;
            --help)
                show_help
                exit 0
                ;;
            *)
                echo "ERROR: unknown option: $1" >&2
                show_help >&2
                exit 2
                ;;
        esac
    done
}

# find_chrome prints the Chrome binary to use and fails when there is none. A
# binary that was named (--chrome, or MEMQL_EMAIL_FIDELITY_CHROME) is used as
# given and never replaced by another browser, so a mistake in it is reported.
function find_chrome() {
    local candidate
    if [ -n "$CHROME" ]; then
        command -v "$CHROME"
        return
    fi
    for candidate in "${CHROME_CANDIDATES[@]}"; do
        if command -v "$candidate" >/dev/null 2>&1; then
            command -v "$candidate"
            return 0
        fi
    done
    return 1
}

function check_prerequisites() {
    local number='^[0-9]+(\.[0-9]+)?$'
    local found

    if ! command -v go >/dev/null 2>&1; then
        echo "ERROR: go is not installed" >&2
        exit 4
    fi
    if [ -z "$DIR" ]; then
        echo "ERROR: --dir (or MEMQL_EMAIL_FIDELITY_DIR) is required" >&2
        show_help >&2
        exit 2
    fi
    if [ ! -d "$DIR" ]; then
        echo "ERROR: the examples directory does not exist: $DIR" >&2
        exit 2
    fi
    if ! [[ $MAX_PCT =~ $number ]]; then
        echo "ERROR: --max must be a percentage such as 2 or 0.5, got: $MAX_PCT" >&2
        exit 2
    fi
    if ! found="$(find_chrome)"; then
        if [ -n "$CHROME" ]; then
            echo "ERROR: Chrome binary not found or not executable: $CHROME" >&2
        else
            echo "ERROR: no Chrome found; install Google Chrome, or pass --chrome=<binary> (or set MEMQL_EMAIL_FIDELITY_CHROME)" >&2
        fi
        exit 4
    fi
    CHROME="$found"
}

function run_fidelity() {
    local dir
    local status=0

    # go test runs in the package's directory, so a relative path would be read
    # from there and not from where this was run.
    dir="$(cd "$DIR" && pwd)"
    cd "$REPO_ROOT"

    echo "INFO: examples: $dir"
    echo "INFO: chrome:   $CHROME"
    echo "INFO: allowed:  at most $MAX_PCT% of an email's content may differ"

    MEMQL_EMAIL_FIDELITY_DIR="$dir" \
    MEMQL_EMAIL_FIDELITY_MAX_PCT="$MAX_PCT" \
    MEMQL_EMAIL_FIDELITY_CHROME="$CHROME" \
        go test ./component/compose/emaildoc/ -run TestFidelityAgainstExamples -count=1 -v || status=$?

    if [ "$status" -ne 0 ]; then
        echo "ERROR: the fidelity run failed (go test exit $status); the diff.png next to an example shows where it differs" >&2
        return "$status"
    fi
    echo "SUCCESS: every example is within $MAX_PCT%; the renders, screenshots and diffs are next to the examples"
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    run_fidelity
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
