#!/usr/bin/env bash
#
# scripts/db-image/build.sh
# =========================
#
# Capability: db.buildImage -- build the memQL database operand image
# (PostgreSQL + TimescaleDB Community + pgvector) locally and, optionally,
# import it into the local k3d cluster.
#
# Epic memql#3842, task memql#3844. This is the LOCAL DEVELOPMENT path only.
# Deployable images are built on the GitHub build server
# (.github/workflows/build-db-image.yml), never on an operator machine --
# CLAUDE.md, "Image builds: LOCAL Docker for dev, BUILD SERVER for deploys".
# Both paths drive the SAME Dockerfile and the SAME smoke test, which is what
# makes the local image a faithful stand-in rather than a lookalike.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 docker/k3d absent | 5 build, smoke, or import failed
#
# Refs: #3844 #3842 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && cd .. && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "db.buildImage" "Build the memQL database operand image for local development."
cap_spec_param "tag"        "image tag to build (default: dev)"
cap_spec_param "pgMajor"    "PostgreSQL major version"
cap_spec_param "timescaledb" "TimescaleDB minor to install as CURRENT"
cap_spec_param "timescaledbPrevious" "TimescaleDB minor to also carry as N-1"
cap_spec_param "smokeTest"  "run the smoke test against the built image"
cap_spec_param "import"     "import the built image into the local k3d cluster"
cap_spec_param "cluster"    "target k3d cluster name (import only)"

# Accepts the spellings an operator actually types. `make db-image SMOKE=0`
# is the natural way to ask for a fast rebuild, and a bare `[[ "$v" != "false" ]]`
# would have silently run the smoke test anyway -- a flag that looks honoured
# and is not is worse than one that errors.
function is_false() {
    case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        false | 0 | no | off | "") return 0 ;;
        *) return 1 ;;
    esac
}

function ensure_docker() {
    command -v docker &>/dev/null || cap_fail 4 "docker is not installed"
}

function build_image() {
    local image="$1" pg="$2" ts="$3" tsPrev="$4"
    cap_info "Building ${image} (pg${pg}, timescaledb ${tsPrev} -> ${ts})..."
    docker build \
        -f "${REPO_ROOT}/deploy/db-image/Dockerfile" \
        --build-arg "PG_MAJOR=${pg}" \
        --build-arg "TIMESCALEDB_VERSION=${ts}" \
        --build-arg "TIMESCALEDB_PREVIOUS_VERSION=${tsPrev}" \
        -t "$image" \
        "${REPO_ROOT}/deploy/db-image" >&2 \
        || cap_fail 5 "docker build of ${image} failed"
}

# The same script CI runs. A local image that skipped it would be exactly the
# image whose Apache-vs-Community regression surfaces later as a migration
# error naming a missing function.
function smoke_test_image() {
    local image="$1" pg="$2" ts="$3" tsPrev="$4"
    cap_info "Smoke-testing ${image}..."
    "${REPO_ROOT}/deploy/db-image/smoke-test.sh" "$image" "$pg" "$ts" "$tsPrev" \
        || cap_fail 5 "smoke test of ${image} failed"
}

function import_image() {
    local image="$1" cluster="$2"
    command -v k3d &>/dev/null || cap_fail 4 "k3d is not installed"
    cap_info "Importing ${image} into k3d cluster ${cluster}..."
    k3d image import "$image" -c "$cluster" >&2 \
        || cap_fail 5 "k3d image import of ${image} failed"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local tag pg ts tsPrev smoke doImport cluster image
    tag="$(cap_param tag "dev")"
    pg="$(cap_param pgMajor "16")"
    ts="$(cap_param timescaledb "2.29.1")"
    tsPrev="$(cap_param timescaledbPrevious "2.28.3")"
    smoke="$(cap_param smokeTest "true")"
    doImport="$(cap_param import "false")"
    cluster="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"

    image="memql-db:${tag}"

    ensure_docker
    build_image "$image" "$pg" "$ts" "$tsPrev"
    cap_changed

    if is_false "$smoke"; then
        cap_result_set_raw smokeTested false
    else
        smoke_test_image "$image" "$pg" "$ts" "$tsPrev"
        cap_result_set_raw smokeTested true
    fi

    if is_false "$doImport"; then
        cap_result_set_raw imported false
    else
        import_image "$image" "$cluster"
        cap_result_set_raw imported true
        cap_result_set cluster "$cluster"
    fi

    cap_result_set image "$image"
    cap_result_set timescaledb "$ts"
    cap_result_set timescaledbPrevious "$tsPrev"
    cap_ok
}

main "$@"
