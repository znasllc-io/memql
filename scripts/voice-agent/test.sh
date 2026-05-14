#!/usr/bin/env bash
# Run the voice-agent's pytest suite.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

function main() {
    require_python
    if [ ! -d "${VENV_DIR}" ]; then
        echo "ERROR: venv missing -- run 'make voice-agent' first"
        exit 1
    fi
    activate_venv
    cd "${VOICE_AGENT_DIR}"
    exec pytest "$@"
}

main "$@"
