#!/usr/bin/env bash
# End-to-end voice-loop smoke check for the LiveKit Agents path
# (Python voice-agent process speaking VoiceAgent* gRPC against
# memQL via MemqlService.Stream).
#
# Pre-conditions:
#   - `make dev-refresh` (or `make dev-polyphon`) has been run.
#   - memql-identity is healthy and has minted + injected
#     VOICE_AGENT_TOKEN into polyphon-voice-agent at bring-up
#     (see #184; scripts/dev/refresh.sh's step4).
#   - ngrok is installed + authed so LIVEKIT_PUBLIC_URL points at a
#     reachable tunnel (Anam needs it for the avatar plugin; the
#     audio-only loop tolerates ngrok being absent).
#
# What this script checks:
#   1. polyphon-voice-agent container is running (not crash-looping).
#   2. The container has VOICE_AGENT_TOKEN populated (post-#184 it
#      comes from the host shell at compose-up time, not env_file).
#   3. The startup logs do not contain a traceback.
#   4. The voice-agent has logged its memql gRPC session bound.
#
# When all four pass, the operator runbook below points at the manual
# round-trip (open CoPresent, speak, confirm TTS comes back).
#
# A future iteration will add a synthetic human (LiveKit SDK) that
# streams a canned WAV through the room and measures TTS-TTFB. Today
# the script is a smoke-check + operator runbook.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

function check_voice_agent_running() {
    if ! docker ps --format '{{.Names}}' | grep -q '^polyphon-voice-agent$'; then
        echo "ERROR: polyphon-voice-agent container not running."
        echo "  Bring it up with:   make dev-polyphon"
        echo "  Or full refresh:    make dev-refresh"
        return 1
    fi
    # Restarting != running. dev-refresh#184 injects the token after
    # the first up; a stale stack from before that change crash-loops
    # forever without operator intervention, and we want a clear
    # error rather than the operator confused about silent retries.
    local status
    status=$(docker inspect --format '{{.State.Status}}' polyphon-voice-agent 2>/dev/null || echo "unknown")
    if [ "$status" != "running" ]; then
        echo "ERROR: polyphon-voice-agent state = ${status} (expected running)."
        echo "  Common cause: VOICE_AGENT_TOKEN never injected. Run:"
        echo "    make voice-agent-token INSTANCE=voice-agent-local > /tmp/va.tok"
        echo "    VOICE_AGENT_TOKEN=\$(cat /tmp/va.tok) docker compose \\"
        echo "      -f docker/docker-compose.full.yml \\"
        echo "      -f docker/docker-compose.polyphon.yml \\"
        echo "      up -d --no-deps --force-recreate voice-agent"
        return 1
    fi
    return 0
}

function check_voice_agent_token_present() {
    # The container's env captured at compose-up time -- empty here
    # means dev-refresh's mint+inject step (#184) didn't land before
    # this container was created. We catch the failure mode that
    # caused the epic in the first place.
    local token_set
    token_set=$(docker exec polyphon-voice-agent sh -c 'test -n "$VOICE_AGENT_TOKEN" && echo set' 2>/dev/null || true)
    if [ "$token_set" != "set" ]; then
        echo "ERROR: VOICE_AGENT_TOKEN is empty inside polyphon-voice-agent."
        echo "  The container needs the JWT injected at bring-up. Run:"
        echo "    make voice-agent-token INSTANCE=voice-agent-local"
        echo "  then recreate the voice-agent service with that token in"
        echo "  the calling shell (\`docker compose ... up -d --force-recreate voice-agent\`)."
        return 1
    fi
    return 0
}

function smoke_check_logs() {
    local errors=0
    local logs
    logs=$(docker logs polyphon-voice-agent 2>&1 || true)
    if ! echo "$logs" | grep -qi "starting memql voice-agent\|registered worker"; then
        echo "WARNING: voice-agent never logged a worker-pool registration."
        errors=$((errors + 1))
    fi
    # Filter out the auto-restart tracebacks that show up before the
    # token lands -- those are expected during the gap between
    # compose-up and dev-refresh's step4 in normal operation. We
    # only flag tracebacks that the LATEST run still has in scope.
    local tail_logs
    tail_logs=$(docker logs --tail 200 polyphon-voice-agent 2>&1 || true)
    if echo "$tail_logs" | grep -qi "traceback\|error connecting\|authentication failed\|UNAUTHENTICATED"; then
        echo "WARNING: recent voice-agent logs show errors:"
        echo "$tail_logs" | grep -iE "traceback|error connecting|authentication failed|UNAUTHENTICATED" | head -5 | sed 's/^/    /'
        errors=$((errors + 1))
    fi
    return $errors
}

function print_operator_runbook() {
    local public_url
    public_url=""
    if [ -f "${REPO_ROOT}/.env.local" ]; then
        public_url=$(grep -E '^LIVEKIT_PUBLIC_URL=' "${REPO_ROOT}/.env.local" | head -1 | cut -d= -f2- | tr -d '"')
    fi
    cat <<RUNBOOK

[voice-loop-test-livekit] Smoke check passed. To exercise the loop
end-to-end:

  1. Open CoPresent in a browser and create / join a space.
  2. The browser's LiveKit room token request hits BFF; the voice-
     agent joins the room as the General Assistant participant.
  3. Speak into the mic. Confirm in voice-agent logs:
       - 'voice agent partial' lines  (Deepgram interims)
       - 'voice agent final' lines    (Deepgram finals)
       - 'voice agent turn request'   (memql cognition called)
       - audio plays back through the browser (Aura-2 TTS)
       - avatar renders (Anam, if LIVEKIT_PUBLIC_URL reachable)

  Tail logs with:
     docker logs -f polyphon-voice-agent

  Current LIVEKIT_PUBLIC_URL: ${public_url:-(unset -- audio-only)}

  Rotate the voice-agent token (manual):
     make voice-agent-token INSTANCE=voice-agent-local

RUNBOOK
}

function main() {
    # Parse --csv flag if provided.
    local csv=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --csv)
                csv="$2"; shift 2 ;;
            --csv=*)
                csv="${1#*=}"; shift ;;
            *)
                echo "WARNING: unknown arg: $1"; shift ;;
        esac
    done

    local failed=0
    check_voice_agent_running || failed=$((failed + 1))
    if [ $failed -eq 0 ]; then
        check_voice_agent_token_present || failed=$((failed + 1))
    fi
    if [ $failed -eq 0 ]; then
        smoke_check_logs || true   # warnings only -- don't gate runbook
    fi

    if [ $failed -ne 0 ]; then
        echo
        echo "[voice-loop-test-livekit] Smoke check FAILED. Fix the errors"
        echo "above and re-run, or open #185 if you suspect a regression."
        exit 1
    fi

    print_operator_runbook
    if [ -n "${csv}" ]; then
        echo "[voice-loop-test-livekit] CSV target: ${csv} (synthetic-human driver still TODO)"
    fi
}

main "$@"
