#!/usr/bin/env bash
# Shared helpers for voice-agent shell scripts.
# Sourced by install.sh / run.sh / test.sh. Do not run directly.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VOICE_AGENT_DIR="${REPO_ROOT}/voice-agent"
PROTO_FILE="${REPO_ROOT}/component/grpc/memql.proto"
PROTO_OUT_DIR="${VOICE_AGENT_DIR}/voice_agent/proto"
VENV_DIR="${VOICE_AGENT_DIR}/.venv"

function require_python() {
    if ! command -v python3 >/dev/null 2>&1; then
        echo "ERROR: python3 not found in PATH"
        exit 1
    fi
    local version
    version="$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"
    case "$version" in
        3.11|3.12|3.13)
            ;;
        *)
            echo "ERROR: python3 ${version} found; voice-agent requires >= 3.11"
            exit 1
            ;;
    esac
}

function ensure_venv() {
    if [ ! -d "${VENV_DIR}" ]; then
        echo "[voice-agent] creating venv at ${VENV_DIR}"
        python3 -m venv "${VENV_DIR}"
    fi
}

function activate_venv() {
    # shellcheck disable=SC1091
    source "${VENV_DIR}/bin/activate"
}

function generate_protos() {
    if [ ! -f "${PROTO_FILE}" ]; then
        echo "ERROR: proto file not found at ${PROTO_FILE}"
        exit 1
    fi
    mkdir -p "${PROTO_OUT_DIR}"
    python -m grpc_tools.protoc \
        -I"${REPO_ROOT}/component/grpc" \
        --python_out="${PROTO_OUT_DIR}" \
        --grpc_python_out="${PROTO_OUT_DIR}" \
        "${PROTO_FILE}"
    # Rewrite the grpc stub's bare `import memql_pb2` so it resolves
    # under the voice_agent.proto package instead of going looking for
    # a top-level memql_pb2 on sys.path. macOS sed needs the '' arg.
    local memql_grpc_py="${PROTO_OUT_DIR}/memql_pb2_grpc.py"
    if [ -f "${memql_grpc_py}" ]; then
        if [[ "$(uname)" == "Darwin" ]]; then
            sed -i '' 's/^import memql_pb2 as memql__pb2/from voice_agent.proto import memql_pb2 as memql__pb2/' "${memql_grpc_py}"
        else
            sed -i 's/^import memql_pb2 as memql__pb2/from voice_agent.proto import memql_pb2 as memql__pb2/' "${memql_grpc_py}"
        fi
    fi
    echo "[voice-agent] protos generated -> ${PROTO_OUT_DIR}"
}
