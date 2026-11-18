#!/usr/bin/env bash
#
# scripts/media/local-down.sh
# ===========================
#
# Capability: media.localDown -- stop the local calls media server that
# scripts/media/local-up.sh started (make media-down), and remove the configuration it
# rendered.
#
# The containers are removed by Compose PROJECT NAME (memql-media), with no compose file:
# the file's values are required at interpolation (deploy/media/compose.yaml), and a
# teardown must not depend on the rendered files still being there. It runs from / so no
# compose file in the caller's directory is picked up instead.
#
# THE RENDERED FILES GO TOO: livekit.yaml carries both key pairs, and a server that is
# not running has no use for them. Exactly the three files local-up.sh writes are
# removed, and the directory only when nothing else is in it. make media-up renders them
# again from the cluster.
#
# Stopping the server does not touch the cluster: the bff keeps its media server row and
# its configuration, and a call placed while the server is down fails to reach it.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single JSON
# result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (Docker, its daemon or
#             Compose) | 5 op failed (docker compose down, or a file it could not remove)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "media.localDown" "Stop the local calls media server and remove its rendered configuration."
cap_spec_param "state-dir" "where make media-up rendered the configuration (default: ~/.memql/media)"
cap_spec_param "docker"    "the docker CLI (default: docker on PATH)"

PROJECT="memql-media"
# The files local-up.sh renders, and nothing else.
RENDERED_FILES=("livekit.yaml" "Caddyfile" "front-door-ca.pem")

STATE_DIR=""
DOCKER=""

function info() { cap_info "$*"; }

function require_docker() {
    if ! command -v "$DOCKER" >/dev/null 2>&1; then
        cap_fail 4 "Docker is required but '${DOCKER}' was not found. Install Docker Desktop or Docker Engine first."
    fi
    if ! "$DOCKER" info >/dev/null 2>&1; then
        cap_fail 4 "the Docker daemon is not running, or '${DOCKER}' cannot reach it. Start Docker first."
    fi
    if ! "$DOCKER" compose version >/dev/null 2>&1; then
        cap_fail 4 "the Docker Compose plugin is required ('${DOCKER} compose version' failed). Install it first."
    fi
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    STATE_DIR="$(cap_param state-dir "${HOME:-}/.memql/media")"
    DOCKER="$(cap_param docker "docker")"
    cap_require state-dir "$STATE_DIR"

    require_docker

    local running removed="" name
    running="$(cd / && "$DOCKER" compose --project-name "$PROJECT" ps --all --quiet 2>/dev/null || true)"
    info "stopping ${PROJECT}..."
    if ! (cd / && "$DOCKER" compose --project-name "$PROJECT" down --remove-orphans >&2); then
        cap_fail 5 "docker compose could not take ${PROJECT} down; its output is above"
    fi
    if [[ -n "$running" ]]; then
        cap_changed
    fi

    for name in "${RENDERED_FILES[@]}"; do
        if [[ -e "${STATE_DIR}/${name}" ]]; then
            rm -f "${STATE_DIR}/${name}" || cap_fail 5 "could not remove ${STATE_DIR}/${name}"
            removed+="${removed:+,}\"$(cap_json_escape "$name")\""
            cap_changed
        fi
    done
    # Only when empty: anything else in it is not this script's.
    rmdir "$STATE_DIR" 2>/dev/null || true
    info "${PROJECT} is stopped and its rendered configuration removed."

    cap_result_set     project "$PROJECT"
    cap_result_set     stateDir "$STATE_DIR"
    cap_result_set_raw stopped true
    cap_result_set_raw removedFiles "[${removed}]"
    cap_ok
}

main "$@"
