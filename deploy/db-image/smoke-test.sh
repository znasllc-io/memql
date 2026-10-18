#!/usr/bin/env bash
#
# Smoke test for the MemQL database operand image (epic memql#3842, task
# memql#3844).
#
# Run against a BUILT image, before it is pushed anywhere. One script so the
# release lane (.github/workflows/build-db-image.yml) and the local dev build
# (scripts/db-image/build.sh) assert exactly the same things -- two copies would
# drift, and the copy that drifts is the one that stops catching anything.
#
# WHAT IT ASSERTS, AND WHY EACH ONE EARNS ITS PLACE
#
#   1. Both extensions create.               The image's entire purpose.
#   2. `SHOW timescaledb.license` = timescale
#                                            The Community/TSL build is what
#                                            carries continuous aggregates,
#                                            compression, and retention. An
#                                            Apache build installs cleanly and
#                                            creates the extension happily --
#                                            it fails LATER, at the first
#                                            migration, with an error about a
#                                            function that does not exist. This
#                                            is the one check that separates
#                                            the two builds up front.
#   3. Continuous aggregate + compression + retention actually run.
#                                            Not a version string but the three
#                                            features the schema requires,
#                                            verified in
#                                            component/database/memory-nodes/migrations/.
#                                            Asserting the license alone would
#                                            still pass on a build where they
#                                            were unavailable for another reason.
#   4. N-1 -> N upgrade choreography.        The image ships two versioned
#                                            `.so` files so a rolling restart
#                                            can land before ALTER EXTENSION
#                                            runs. That is invisible until an
#                                            upgrade, when getting it wrong is
#                                            an outage rather than a bug, so it
#                                            is exercised here: create the
#                                            extension AT the previous minor,
#                                            serve a query, then UPDATE.
#
# Usage:
#   deploy/db-image/smoke-test.sh <image-ref> [pg-major] [current-version] [previous-version]

set -euo pipefail

function info() { printf 'INFO:  %s\n' "$*" >&2; }
function fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

function main() {
    IMAGE="${1:?usage: smoke-test.sh <image-ref> [pg-major] [current] [previous]}"
    PG_MAJOR="${2:-16}"
    TS_CURRENT="${3:-2.29.1}"
    TS_PREVIOUS="${4:-2.28.3}"

    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    info "smoke-testing ${IMAGE} (pg${PG_MAJOR}, timescaledb ${TS_PREVIOUS} -> ${TS_CURRENT})"
    docker run --rm -i --user 26 --entrypoint bash "$IMAGE" \
        -s -- "$PG_MAJOR" "$TS_CURRENT" "$TS_PREVIOUS" \
        < "$SCRIPT_DIR/smoke-in-container.sh" || fail "runtime smoke test failed"
    info "SUCCESS: ${IMAGE} passed every smoke check"
}

main "$@"
