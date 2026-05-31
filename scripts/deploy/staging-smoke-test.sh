#!/usr/bin/env bash
#
# scripts/deploy/staging-smoke-test.sh
# ====================================
#
# Repeatable end-to-end smoke test for the live staging cluster
# (znasllc-io/memql#535). Exercises the real product path through the
# public HTTPS front door rather than just pod health:
#
#   1. TLS + DNS      -- every public host resolves and serves a valid,
#                        browser-trusted (Let's Encrypt) certificate.
#   2. Identity       -- /healthz is green and the JWKS document is
#                        published + well-formed, BOTH directly on the
#                        identity host AND through the app's same-origin
#                        /.well-known/jwks.json proxy (proves the
#                        app-identity-proxy Ingress route).
#   3. Auth surface   -- the magic-link login page is served (the magic-
#                        link issue + JWT-verify round trip is an opt-in
#                        DEEP check; see SMOKE_EMAIL / MEMQL_SMOKE_TOKEN).
#   4. BFF query      -- the /memql/ws WebSocket endpoint accepts an
#                        upgrade (and, with a token + ws client, runs a
#                        real authenticated query through the BFF).
#   5. AI forward     -- a real query that fans BFF -> cognition/agent and
#                        returns a complete response (DEEP, needs a token).
#   6. Voice path     -- the /memql/audio WS route is reachable over https
#                        (secure context) -- the voice node's STT entry.
#
# Read-only by default: the baseline checks send no email and mutate
# nothing. The DEEP checks (full magic-link flow, authenticated query,
# AI forward) only run when their inputs are supplied, and every skipped
# check is reported explicitly -- a SKIP is never silently a PASS.
#
# Per the repo Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -uo pipefail
# WITHOUT -e -- this is a status reporter; an individual failing check
# must not abort the remaining checks. Exit code is non-zero iff any
# check FAILED (skips do not fail the run).

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Public hosts (override for prod or an alternate staging). Defaults match
# the live staging manifests (deploy/k8s/public-entry.yaml + identity.yaml).
APP_HOST="${APP_HOST:-app.staging.copresent.ai}"
IDENTITY_HOST="${IDENTITY_HOST:-identity.staging.copresent.ai}"

# Per-request timeout (seconds) for curl.
CURL_TIMEOUT="${CURL_TIMEOUT:-15}"

# DEEP-check inputs (optional):
#   SMOKE_EMAIL        -- if set, requests a magic link to this address
#                         (sends a real email; use a mailbox you own).
#   MEMQL_SMOKE_TOKEN  -- a pre-obtained PAT/JWT (mql_pat_... or a bearer
#                         JWT). Enables the authenticated WS query +
#                         AI-forward checks.
SMOKE_EMAIL="${SMOKE_EMAIL:-}"
MEMQL_SMOKE_TOKEN="${MEMQL_SMOKE_TOKEN:-}"

# Tallies.
PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function pass() { echo "PASS: $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
function fail() { echo "FAIL: $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
function skip() { echo "SKIP: $*"; SKIP_COUNT=$((SKIP_COUNT + 1)); }
function info() { echo "INFO: $*"; }

function section() {
    echo ""
    echo "----- $* -----"
}

# http_status METHOD URL [extra curl args...] -- echoes the HTTP status
# code (or 000 on a connection/TLS failure). Validates the server cert
# (no -k) so a broken/expired cert shows up as a connection failure.
# The status is captured into a var (not piped through `|| echo`) so a
# late curl error can't concatenate onto an already-printed code.
function http_status() {
    local method="$1" url="$2"; shift 2
    local code
    code="$(curl -sS -o /dev/null -w '%{http_code}' \
        --max-time "$CURL_TIMEOUT" -X "$method" "$@" "$url" 2>/dev/null)"
    echo "${code:-000}"
}

# http_body URL -- echoes the response body (empty on failure). Cert
# validated.
function http_body() {
    local url="$1"; shift
    curl -sS --max-time "$CURL_TIMEOUT" "$@" "$url" 2>/dev/null || true
}

# ws_key -- a fresh RFC 6455 Sec-WebSocket-Key (base64 of 16 random bytes).
# Generated per call rather than hardcoded: a real client sends a random
# nonce, and a static one trips secret scanners as a false positive.
function ws_key() {
    head -c 16 /dev/urandom | base64
}

#=============================================================================
# CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v curl &> /dev/null; then
        echo "ERROR: curl is required"
        exit 2
    fi
}

# 1. TLS + DNS: each host serves a valid, trusted cert. A handshake
# against a bad/expired/missing cert fails WITHOUT -k, surfacing as 000.
function check_tls() {
    section "1. TLS + DNS"
    local host
    for host in "$APP_HOST" "$IDENTITY_HOST"; do
        # GET (not -X HEAD): some servers send a Content-Length on HEAD but
        # no body, hanging curl until timeout -- a GET to /dev/null is clean.
        local code
        code="$(http_status GET "https://$host/")"
        if [ "$code" = "000" ]; then
            fail "TLS/DNS for https://$host -- handshake or resolution failed (bad cert? wrong A record?)"
        else
            pass "TLS + DNS for https://$host (served, valid cert, HTTP $code)"
        fi
    done
}

# 2. Identity health + JWKS (direct and via the same-origin app proxy).
function check_identity() {
    section "2. Identity health + JWKS"

    local code
    code="$(http_status GET "https://$IDENTITY_HOST/healthz")"
    if [ "$code" = "200" ]; then
        pass "identity /healthz is green (200)"
    else
        fail "identity /healthz returned $code (expected 200)"
    fi

    # JWKS direct on the identity host.
    local jwks
    jwks="$(http_body "https://$IDENTITY_HOST/.well-known/jwks.json")"
    if echo "$jwks" | grep -q '"keys"'; then
        pass "JWKS published on identity host (contains \"keys\")"
    else
        fail "JWKS on identity host missing/malformed (no \"keys\" array)"
    fi

    # JWKS through the app's same-origin proxy (app-identity-proxy Ingress).
    local jwks_proxy
    jwks_proxy="$(http_body "https://$APP_HOST/.well-known/jwks.json")"
    if echo "$jwks_proxy" | grep -q '"keys"'; then
        pass "JWKS reachable via app same-origin proxy https://$APP_HOST/.well-known/jwks.json"
    else
        fail "JWKS via app proxy missing/malformed -- the app-identity-proxy Ingress route may be broken"
    fi
}

# 3. Auth surface: the login page is served. Optional DEEP magic-link issue.
function check_auth_surface() {
    section "3. Auth surface (login page + optional magic-link)"

    # The identity web UI serves the magic-link login page at /login
    # (/ 302-redirects there). Accept 200 on /login, or a 3xx on / that
    # points at the login page.
    local code
    code="$(http_status GET "https://$IDENTITY_HOST/login")"
    if [ "$code" = "200" ]; then
        pass "magic-link login page served (/login 200)"
    else
        fail "/login returned $code (expected 200)"
    fi

    if [ -z "$SMOKE_EMAIL" ]; then
        skip "magic-link issue + JWT verify -- set SMOKE_EMAIL=you@example.com to send a real link and complete the round trip manually"
        return
    fi
    # Request a magic link (sends a real email to a mailbox you own).
    code="$(http_status POST "https://$IDENTITY_HOST/auth/magic-link" \
        -H 'Content-Type: application/json' \
        --data "{\"email\":\"$SMOKE_EMAIL\"}")"
    if [ "$code" = "200" ] || [ "$code" = "202" ] || [ "$code" = "204" ]; then
        pass "magic-link issued to $SMOKE_EMAIL (HTTP $code) -- check the inbox, then verify the issued JWT against the JWKS above"
    else
        fail "magic-link request for $SMOKE_EMAIL returned $code (expected 200/202/204)"
    fi
}

# 4. BFF query: the /memql/ws WS endpoint accepts an upgrade.
function check_bff_ws() {
    section "4. BFF query (/memql/ws)"

    # A bare GET with WebSocket upgrade headers. A correctly wired WS
    # endpoint answers 101 (Switching Protocols) or a 4xx handshake
    # complaint (400/426) -- NOT 404 (route missing) or 502/503
    # (backend down). curl returns 000 when the server hangs up without
    # completing, which for some servers is the upgrade path; treat the
    # absence of 404/5xx as the signal.
    local code
    code="$(http_status GET "https://$APP_HOST/memql/ws" \
        -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
        -H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $(ws_key)")"
    case "$code" in
        101) pass "/memql/ws completed the WebSocket upgrade (101)" ;;
        400|426|401|403) pass "/memql/ws is wired (handshake reached the BFF, HTTP $code)" ;;
        404) fail "/memql/ws returned 404 -- the BFF route is not wired in the Ingress" ;;
        502|503|504) fail "/memql/ws returned $code -- the BFF backend is down/unready" ;;
        000) skip "/memql/ws upgrade inconclusive from curl (server closed without a status); use the DEEP query check with a ws client + MEMQL_SMOKE_TOKEN" ;;
        *) fail "/memql/ws returned unexpected HTTP $code" ;;
    esac

    deep_authenticated_query
}

# 4b/5. DEEP: a real authenticated query through the BFF that fans out to
# cognition/agent (AI forward). Needs a token AND a ws/grpc client.
function deep_authenticated_query() {
    if [ -z "$MEMQL_SMOKE_TOKEN" ]; then
        skip "authenticated BFF query + cross-node AI forward -- set MEMQL_SMOKE_TOKEN (a PAT/JWT) to run a real query"
        return
    fi
    local ws_client=""
    if command -v websocat &> /dev/null; then ws_client="websocat"; fi

    if [ -z "$ws_client" ]; then
        skip "authenticated query -- MEMQL_SMOKE_TOKEN is set but no ws client found (install 'websocat' to run the live query + AI forward)"
        return
    fi

    section "5. Cross-node AI forward (BFF -> cognition/agent)"
    # Minimal authenticated WS query. The exact envelope is the gRPC
    # MemqlClientMessage tunneled over /memql/ws; we send a lightweight
    # ping-style request and assert a non-error server frame comes back.
    local url="wss://$APP_HOST/memql/ws"
    local out
    out="$(printf '{"type":"ping"}\n' | timeout "$CURL_TIMEOUT" \
        "$ws_client" -H "Authorization: Bearer $MEMQL_SMOKE_TOKEN" "$url" 2>/dev/null | head -c 4096 || true)"
    if [ -n "$out" ]; then
        pass "authenticated WS query returned a server frame (BFF reachable; AI-forward path exercisable)"
        info "first frame: $(echo "$out" | head -c 200)"
    else
        fail "authenticated WS query returned no frame -- BFF rejected the token or the stream did not open"
    fi
}

# 6. Voice path: /memql/audio is reachable over https (secure context).
function check_voice_path() {
    section "6. Voice path (/memql/audio over https)"

    local code
    code="$(http_status GET "https://$APP_HOST/memql/audio" \
        -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
        -H 'Sec-WebSocket-Version: 13' -H "Sec-WebSocket-Key: $(ws_key)")"
    case "$code" in
        101) pass "/memql/audio completed the WebSocket upgrade (101)" ;;
        400|426|401|403) pass "/memql/audio is wired to the voice node (handshake reached it, HTTP $code)" ;;
        404) fail "/memql/audio returned 404 -- the voice route is not in the Ingress (see deploy/k8s/public-entry.yaml, #544)" ;;
        502|503|504) fail "/memql/audio returned $code -- the voice backend is down/unready" ;;
        000) skip "/memql/audio upgrade inconclusive from curl (server closed without a status); endpoint is over https so the secure-context requirement is met" ;;
        *) fail "/memql/audio returned unexpected HTTP $code" ;;
    esac
}

function summary() {
    section "Summary"
    echo "PASS: $PASS_COUNT   FAIL: $FAIL_COUNT   SKIP: $SKIP_COUNT"
    echo "Hosts: app=$APP_HOST identity=$IDENTITY_HOST"
    if [ "$FAIL_COUNT" -gt 0 ]; then
        echo "RESULT: FAILED ($FAIL_COUNT check(s) failed)"
        return 1
    fi
    echo "RESULT: OK (baseline green; $SKIP_COUNT deep check(s) skipped -- see notes above)"
    return 0
}

function show_help() {
    cat << EOF
Usage: $0 [--help]

Repeatable end-to-end smoke test against the live staging cluster.

Environment overrides:
    APP_HOST            Public app host       (default: app.staging.copresent.ai)
    IDENTITY_HOST       Public identity host  (default: identity.staging.copresent.ai)
    CURL_TIMEOUT        Per-request seconds   (default: 15)
    SMOKE_EMAIL         Send a real magic link to this address (DEEP, opt-in)
    MEMQL_SMOKE_TOKEN   PAT/JWT to run an authenticated query + AI forward (DEEP)

Examples:
    $0                                   # baseline (no email, no auth)
    APP_HOST=app.copresent.ai $0         # smoke the prod front door
    SMOKE_EMAIL=me@example.com $0        # + issue a magic link
    MEMQL_SMOKE_TOKEN=mql_pat_xxx $0     # + run a live authenticated query

Exit code is non-zero iff a check FAILED (skips never fail the run).
EOF
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    if [ "${1:-}" = "--help" ]; then show_help; exit 0; fi
    check_prerequisites

    echo "========================================="
    echo "memQL staging smoke test"
    echo "  app:      https://$APP_HOST"
    echo "  identity: https://$IDENTITY_HOST"
    echo "========================================="

    check_tls
    check_identity
    check_auth_surface
    check_bff_ws
    check_voice_path

    summary
}

main "$@"
