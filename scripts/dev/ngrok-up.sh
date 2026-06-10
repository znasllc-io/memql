#!/usr/bin/env bash
#
# scripts/dev/ngrok-up.sh
# =======================
#
# Bring up (or refresh) an ngrok HTTPS tunnel in front of the local
# LiveKit container so external services (Anam's cloud avatar
# engine, etc.) can dial in. Calls `lib_refresh_ngrok` from lib.sh
# -- the same function `make dev-refresh` invokes mid-flow -- so
# the two scripts stay in lockstep.
#
# Use this standalone (outside a full refresh) when the ngrok URL
# rotates mid-session or you forgot to start the tunnel up-front;
# the function tears down any existing ngrok agent and starts a
# fresh tunnel, then this script restarts the affected services so
# the new URL takes hold without a full stack wipe.
#
# Prerequisites:
#   - ngrok CLI installed on the host (brew install ngrok)
#   - ngrok authtoken saved (ngrok config add-authtoken <token>)
#   - the voice/avatar overlay running on the cluster (make dev-cluster-voice),
#     which stands up the polyphon-livekit container + coturn + voice-agent.
#
# Idempotent: re-run any time to refresh.

set -euo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

#=============================================================================
# FUNCTIONS
#=============================================================================

function warn_avatar_video_caveat() {
    # The voice/avatar overlay (docker-compose.polyphon.yml) now layers on
    # the parity cluster (memql#1310). This ngrok/TURN path restores the
    # avatar relay PLUMBING -- but local cloud-avatar VIDEO does NOT render
    # (memql#1277): the cloud engine joins LiveKit signaling but its WebRTC
    # media leg to the dockerized LiveKit never connects over the free ngrok
    # TURN relay. Avatar video is validated on STAGING only. Direct avatar
    # runs audio-only locally.
    echo "NOTE: this restores the voice-agent + avatar TURN-relay plumbing."
    echo "      Local cloud-avatar VIDEO (Anam/Simli) does NOT render over this"
    echo "      relay (memql#1277) -- validated on staging only. Voice still works."
    echo ""
}

function require_livekit() {
    # The voice/avatar overlay renames the cluster's livekit container to
    # `polyphon-livekit` (so lib_refresh_turn_relay can stamp + restart it).
    # Accept the bare cluster name too in case the overlay isn't up yet.
    if ! docker ps --format '{{.Names}}' | grep -qE '^(memql|polyphon)-livekit$'; then
        echo "ERROR: no LiveKit container is running."
        echo "       Start the cluster WITH the voice/avatar overlay first:"
        echo "         make dev-cluster-voice"
        echo "       Or run a full refresh: make dev-cluster-refresh"
        exit 1
    fi
}

function refresh_tunnel() {
    # Delegates to the shared lib function. Non-zero return is a
    # known-skipped state (ngrok missing, .env.local not seeded, etc.)
    # and is logged inside the function; treat it as a hard error
    # here since the user explicitly invoked this script asking for
    # a tunnel.
    if ! lib_refresh_ngrok; then
        echo ""
        echo "ERROR: ngrok refresh failed -- see message above."
        exit 1
    fi
}

function restart_dependent_services() {
    echo ""
    echo "Restarting bff + livekit + voice-agent so they pick up the new URL..."
    # Use the cluster + voice/avatar overlay so the polyphon-livekit + the
    # voice-agent (defined in the overlay) are in scope for the restart.
    $LIB_COMPOSE $LIB_COMPOSE_FILE_POLYPHON restart bff livekit voice-agent >/dev/null
}

function print_summary() {
    local public_url
    public_url=$(grep -E '^LIVEKIT_PUBLIC_URL=' .env.local | tail -1 | cut -d= -f2-)
    cat <<EOF

========================================
ngrok tunnel up
========================================
Public LiveKit signaling: ${public_url}
Inspector UI:             http://127.0.0.1:4040
Logs:                     ${LIB_NGROK_LOG}

Next: reload the browser, click mic + video. Anam's avatar should
join via this URL.

Static-domain tip: ngrok free accounts get 1 reserved domain.
Reserve it in the ngrok dashboard, then we can extend
lib_refresh_ngrok to pin to it across restarts. Ask when you
want that.
EOF
}

function main() {
    check_docker
    warn_avatar_video_caveat
    require_livekit
    refresh_tunnel
    restart_dependent_services
    print_summary
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
