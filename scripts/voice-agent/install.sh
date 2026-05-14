#!/usr/bin/env bash
# Install voice-agent dependencies + generate proto stubs.
#
# Idempotent. Run after editing pyproject.toml or memql.proto.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

function install_deps() {
    cd "${VOICE_AGENT_DIR}"
    pip install --upgrade pip setuptools wheel >/dev/null
    # Anam by default. Override with: AVATAR=simli make voice-agent
    local avatar="${AVATAR:-anam}"
    case "${avatar}" in
        anam|simli|none)
            ;;
        *)
            echo "ERROR: AVATAR=${avatar} must be anam | simli | none"
            exit 1
            ;;
    esac
    if [ "${avatar}" = "none" ]; then
        pip install -e ".[dev]"
    else
        pip install -e ".[${avatar},dev]"
    fi
}

function main() {
    require_python
    ensure_venv
    activate_venv
    install_deps
    generate_protos
    echo "[voice-agent] install complete"
    echo "[voice-agent] activate the venv with:"
    echo "  source ${VENV_DIR}/bin/activate"
}

main "$@"
