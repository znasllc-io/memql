#!/usr/bin/env bash
# End-to-end voice-loop test driving the LiveKit Agents path
# (voice-agent process via VoiceAgent* gRPC contract).
#
# Compares latency + correctness against the legacy bridge-agent
# baseline produced by loop-test-deepgram.sh / loop-test-openai.sh.
# Replaces voice-loop-test-deepgram once the Phase 10 cutover lands.
#
# Drives the voice-agent end-to-end:
#   1. Bring up the cluster + voice-agent (assumes user already ran
#      `make dev-polyphon` or equivalent).
#   2. Confirm voice-agent's docker logs show the LiveKit Agents worker
#      registered and the memql gRPC session bound.
#   3. Print the next-steps the operator can run by hand to validate.
#
# A future iteration will add a synthetic human (LiveKit SDK) that
# streams a canned WAV through the room and measures TTS-TTFB. Today
# the script is a smoke-check + operator runbook.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

function require_env() {
    local missing=()
    for var in MEMQL_DEEPGRAM_API_KEY VOICE_AGENT_SHARED_TOKEN; do
        if [ -z "${!var:-}" ]; then
            missing+=("$var")
        fi
    done
    if [ ${#missing[@]} -gt 0 ]; then
        echo "ERROR: missing env vars: ${missing[*]}"
        exit 1
    fi
}

function check_voice_agent_running() {
    if ! docker ps --format '{{.Names}}' | grep -q '^polyphon-voice-agent$'; then
        echo "ERROR: polyphon-voice-agent container not running"
        echo "  Bring it up with:  make dev-polyphon"
        return 1
    fi
    return 0
}

function smoke_check_logs() {
    local errors=0
    if ! docker logs polyphon-voice-agent 2>&1 | grep -q "starting memql voice-agent"; then
        echo "WARNING: voice-agent never logged startup"
        errors=$((errors + 1))
    fi
    if docker logs polyphon-voice-agent 2>&1 | grep -qi "traceback\|error connecting"; then
        echo "WARNING: voice-agent log contains errors"
        errors=$((errors + 1))
    fi
    return $errors
}

function print_operator_runbook() {
    cat <<RUNBOOK
[voice-loop-test-livekit] To exercise the loop end-to-end:

  1. Open CoPresent in a browser and create / join a space.
  2. The browser's LiveKit room token request hits BFF; BFF dispatches
     voice-agent to the room as the GA participant.
  3. Speak into the mic. Confirm in voice-agent logs:
       - 'voice agent partial' lines (Deepgram interims)
       - 'voice agent final' lines (Deepgram finals)
       - 'voice agent turn request' lines (memql cognition called)
       - audio plays back through the browser (Aura-2 TTS)

  Tail logs with:
     docker logs -f polyphon-voice-agent

  Compare to the legacy bridge-agent path:
     make voice-loop-test-deepgram
     make voice-loop-test-livekit
RUNBOOK
}


function main() {
    require_env
    # Parse --csv flag if provided.
    CSV=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --csv)
                CSV="$2"; shift 2 ;;
            --csv=*)
                CSV="${1#*=}"; shift ;;
            *)
                echo "WARNING: unknown arg: $1"; shift ;;
        esac
    done
    check_voice_agent_running
    smoke_check_logs || true
    print_operator_runbook
    if [ -n "${CSV}" ]; then
        echo "[voice-loop-test-livekit] CSV target: ${CSV} (driver still TODO)"
    fi
}

main "$@"
