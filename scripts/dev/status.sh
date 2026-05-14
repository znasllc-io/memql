#!/usr/bin/env bash
#
# scripts/dev/status.sh
# =====================
#
# Quick snapshot of the dev environment: docker daemon? memQL gRPC
# reachable? what containers are running? Useful when 'dev-refresh'
# didn't behave as expected and you want to see where things stand
# without scanning logs by hand.
#
# Per repo convention (CLAUDE.md): function-based structure.
# Intentionally does NOT 'set -e' -- we want to keep printing
# sections even if individual checks fail.
set -uo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

function main() {
    section_docker
    section_grpc
    section_containers
    section_urls
}

main "$@"
