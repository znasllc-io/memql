#!/usr/bin/env bash
# Run the voice-agent worker locally.
#
# Reads env from voice-agent/.env if present (gitignored), then hands
# off to the memql-voice-agent console-script defined in pyproject.toml.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

function load_env() {
    local env_file="${VOICE_AGENT_DIR}/.env"
    if [ -f "${env_file}" ]; then
        echo "[voice-agent] loading env from ${env_file}"
        set -a
        # shellcheck disable=SC1090
        source "${env_file}"
        set +a
    fi
}

function ensure_protos() {
    if [ ! -f "${PROTO_OUT_DIR}/memql_pb2.py" ]; then
        echo "[voice-agent] proto stubs missing -- regenerating"
        generate_protos
    fi
}

function main() {
    require_python
    if [ ! -d "${VENV_DIR}" ]; then
        echo "ERROR: venv missing -- run 'make voice-agent' first"
        exit 1
    fi
    activate_venv
    ensure_protos
    load_env
    cd "${VOICE_AGENT_DIR}"
    exec memql-voice-agent "$@"
}

main "$@"
