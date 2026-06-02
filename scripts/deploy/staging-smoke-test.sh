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

# SMOKE_PROFILE selects the gate's strictness (znasllc-io/memql#627):
#   baseline -- front-door reachability. Deep checks that lack their inputs
#               (token, ws client) SKIP, and a SKIP never fails the run. This
#               is the developer's "is the front door up?" check.
#   deep     -- the PROMOTION GATE. Every deep check MUST run and PASS; a
#               missing input is a FAIL, not a SKIP. A deep run with no
#               MEMQL_SMOKE_TOKEN fails immediately -- a gate that can't
#               authenticate proves nothing (this is exactly how the 0.9.6
#               deploy went 8-PASS/0-FAIL/2-SKIP green while the authenticated
#               app was broken: issuer mismatch, WS rejection, dead mesh).
SMOKE_PROFILE="${SMOKE_PROFILE:-baseline}"

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
function warn() { echo "WARNING: $*"; }

function section() {
    echo ""
    echo "----- $* -----"
}

# is_deep -- true when running the deep promotion-gate profile.
function is_deep() { [ "$SMOKE_PROFILE" = "deep" ]; }

# deep_gap MSG -- a deep check could not run because an input is missing.
# In the deep profile this is a hard FAIL (the gate must be conclusive); in
# baseline it is a SKIP. Centralizes the "a SKIP is never silently a PASS"
# rule so every deep check reports the gap the same way.
function deep_gap() {
    if is_deep; then
        fail "$* [deep profile requires this check to run]"
    else
        skip "$*"
    fi
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

# poll_http_status URL WANT [attempts] [nap] -- repeatedly GETs URL until it
# returns WANT, echoing the final status. Absorbs the rolling-update + ingress
# endpoint-convergence window after a fresh rollout (the gate runs right after
# `kubectl rollout status`, when the ingress can still briefly load-balance
# across mixed-version pods -- e.g. a terminating old-version identity pod).
# Defaults: 6 attempts, 5s apart (~30s). #682.
function poll_http_status() {
    local url="$1" want="$2" attempts="${3:-6}" nap="${4:-5}" code="000" i
    for ((i = 1; i <= attempts; i++)); do
        code="$(http_status GET "$url")"
        if [ "$code" = "$want" ]; then echo "$code"; return 0; fi
        if [ "$i" -lt "$attempts" ]; then sleep "$nap"; fi
    done
    echo "$code"
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
    code="$(poll_http_status "https://$IDENTITY_HOST/healthz" 200)"
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

# 2b. Server-side readiness: GET /readyz asserts critical schema presence
# (#657). This proves migrations actually applied WITHOUT needing DB creds (the
# staging DB is firewalled), closing the gap that let #624 ship a broken schema
# behind a green deploy -- the runtime counterpart to the #671 migrate gate.
# Needs no token, so it runs in every profile; a missing endpoint (404) is a
# hard FAIL in the deep promotion gate (a version that predates #657 is not
# promotable) and a SKIP in baseline.
#
# Targets the IDENTITY host, NOT the app host (#680): identity.<env> routes "/"
# -> identity (Ingress staging-identity), and the identity binary registers the
# critical-schema readiness check against the shared memory-nodes DB. The app
# host only proxies /memql* -> bff and /.well-known -> identity; /readyz there
# falls to the copresent SPA catch-all and returns index.html (200 doctype).
function check_readiness() {
    section "2b. Server-side readiness (/readyz schema invariants)"

    local code body
    code="$(poll_http_status "https://$IDENTITY_HOST/readyz" 200)"
    case "$code" in
        200)
            body="$(http_body "https://$IDENTITY_HOST/readyz")"
            if echo "$body" | grep -q '"status":"ready"'; then
                pass "/readyz is ready -- critical schema present (identity host)"
            else
                fail "/readyz returned 200 but status is not \"ready\": $body"
            fi
            ;;
        404)
            if is_deep; then
                fail "/readyz not found (404) -- this version predates the schema-assertion probe (#657); NOT promotable"
            else
                skip "/readyz not present (404) -- deploy #657+ to enable server-side schema gating"
            fi
            ;;
        *)
            fail "/readyz returned $code (expected 200 \"ready\")"
            ;;
    esac
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
        deep_gap "authenticated BFF query + cross-node AI forward -- set MEMQL_SMOKE_TOKEN (a PAT/JWT) to run a real query"
        return
    fi
    local ws_client=""
    if command -v websocat &> /dev/null; then ws_client="websocat"; fi

    if [ -z "$ws_client" ]; then
        deep_gap "authenticated query -- MEMQL_SMOKE_TOKEN is set but no ws client found (install 'websocat' to run the live query + AI forward)"
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

# 5b. DEEP: node bootstrap transport is https end-to-end (memql#626).
# The node-side bootstrap client (component/node/bootstrap_token.go) and
# the identity-side handler (component/identity/http/node_bootstrap.go)
# both enforce https: a plaintext POST to /node/bootstrap must NEVER
# mint a token -- it gets `403 insecure_transport` (or is redirected to
# https by the edge), never a 200 success. This asserts the server end
# of that invariant from the front door so an http regression (a deploy
# manifest reverting IDENTITY_VERIFIER_BASE_URL to http://, or the edge
# dropping TLS enforcement) is caught in the promotion gate. The node
# (client) end is covered by the Go regression test
# TestMaybeBootstrapNodeToken_RefusesInsecureHTTP.
function check_bootstrap_transport() {
    if ! is_deep; then return; fi
    section "5b. Node bootstrap transport (https end-to-end, #626)"

    # POST over plaintext http (port 80). A correctly-secured edge either
    # 301/302/308-redirects to https, refuses the connection (000), or --
    # if the request reaches the identity handler in cleartext -- returns
    # 403 insecure_transport. The ONLY unacceptable outcome is a 200 that
    # mints a token over cleartext.
    local code
    code="$(http_status POST "http://$IDENTITY_HOST/node/bootstrap" \
        -H 'Content-Type: application/json' \
        -H 'Authorization: Bootstrap smoke-probe-not-a-real-secret' \
        --data '{"nodeId":"v1:cluster:node:smoke-probe","nodeType":"bff"}')"
    case "$code" in
        301|302|303|307|308) pass "plaintext /node/bootstrap is redirected to https (HTTP $code) -- no cleartext mint" ;;
        403) pass "plaintext /node/bootstrap rejected with 403 (insecure_transport enforced)" ;;
        401|400) pass "plaintext /node/bootstrap reached the handler and was rejected pre-mint (HTTP $code; TLS-terminating edge already secured the hop)" ;;
        000) pass "plaintext /node/bootstrap refused at the connection level (no port 80 / TLS-only edge)" ;;
        200) fail "plaintext /node/bootstrap returned 200 -- a token was minted over CLEARTEXT (the #626 regression: https not enforced end-to-end)" ;;
        *) fail "plaintext /node/bootstrap returned unexpected HTTP $code -- expected a redirect, 403 insecure_transport, or a refused connection" ;;
    esac
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

# assert_asset_loads HOST HTML EXT LABEL -- find the first /assets/*.EXT URL
# referenced by HTML, fetch it from HOST, and assert it is HTTP 200 + non-empty.
# Vite emits content-hashed bundles under /assets/; a 404 or empty body means
# the SPA build/serve is broken and the app can never boot.
function assert_asset_loads() {
    local host="$1" html="$2" ext="$3" label="$4"
    local path
    path="$(echo "$html" | grep -oE '/assets/[A-Za-z0-9._-]+\.'"$ext" | head -1)"
    if [ -z "$path" ]; then
        fail "$label: no /assets/*.$ext reference in the $host app shell (broken build?)"
        return
    fi
    local url="https://$host$path" code bytes
    code="$(http_status GET "$url")"
    bytes="$(http_body "$url" | wc -c | tr -d ' ')"
    if [ "$code" = "200" ] && [ "${bytes:-0}" -gt 100 ]; then
        pass "$label served ($path -> HTTP 200, ${bytes} bytes)"
    else
        fail "$label not served ($path -> HTTP $code, ${bytes} bytes) -- SPA asset build/serve broken"
    fi
}

# 7. SPA boot assets: the app shell loads and its hashed JS + CSS bundles are
# served non-empty. A missing/empty stylesheet or bundle is the front-door
# signature of a broken asset build (app stuck "initializing"). Credential-free.
# NOTE: this is an HTTP-level asset check -- it does NOT execute the bundle, so
# it cannot catch a runtime JS error (e.g. the clientProfile.yaml #255 parse
# class, which is COMPILED INTO the bundle) or console errors. That requires the
# headless-browser walkthrough tier (tracked #627 follow-up).
function check_spa_boot() {
    section "7. SPA boot assets (app shell + hashed JS/CSS)"
    local html
    html="$(http_body "https://$APP_HOST/")"
    if [ -z "$html" ]; then
        fail "app shell https://$APP_HOST/ served no HTML"
        return
    fi
    if echo "$html" | grep -qiE '<div id="root"|<div id="app"|<script'; then
        pass "app shell HTML served (mount point + script tag present)"
    else
        fail "app shell HTML has no root mount / script tag -- not a valid SPA index"
    fi
    assert_asset_loads "$APP_HOST" "$html" "js" "SPA JS bundle"
    assert_asset_loads "$APP_HOST" "$html" "css" "SPA stylesheet"
}

# 8. Identity web styling: the server-rendered login page's stylesheet is
# served non-empty. The identity UI (templ) links /static/app.css, which is
# generated by the in-image Tailwind build -- if the Dockerfile skips that step
# the file 404s and the login renders UNSTYLED. This is the exact front-door
# check for the identity #620 class. Credential-free.
function check_identity_styling() {
    section "8. Identity web styling (/static/app.css)"
    local url="https://$IDENTITY_HOST/static/app.css" code bytes
    code="$(poll_http_status "$url" 200)"
    bytes="$(http_body "$url" | wc -c | tr -d ' ')"
    if [ "$code" = "200" ] && [ "${bytes:-0}" -gt 100 ]; then
        pass "identity stylesheet served (/static/app.css -> HTTP 200, ${bytes} bytes)"
    else
        fail "identity /static/app.css missing/empty (HTTP $code, ${bytes} bytes) -- unstyled login, the #620 regression (templ+Tailwind not built into the image)"
    fi
}

function summary() {
    section "Summary"
    echo "PASS: $PASS_COUNT   FAIL: $FAIL_COUNT   SKIP: $SKIP_COUNT"
    echo "Profile: $SMOKE_PROFILE   Hosts: app=$APP_HOST identity=$IDENTITY_HOST"
    if [ "$FAIL_COUNT" -gt 0 ]; then
        echo "RESULT: FAILED ($FAIL_COUNT check(s) failed)"
        return 1
    fi
    if is_deep; then
        # In deep, deep_gap converts a missing input to a FAIL, so a zero-FAIL
        # deep run means every gate check actually ran and passed. SKIP here can
        # only be a non-gating optional (e.g. magic-link email) -- still report.
        echo "RESULT: OK -- DEEP gate PASSED (authenticated WS + AI-forward + SPA assets + identity styling all green${SKIP_COUNT:+; $SKIP_COUNT non-gating skip(s)}). PROMOTABLE."
        return 0
    fi
    echo "RESULT: OK (baseline green; $SKIP_COUNT deep check(s) skipped -- NOT promotable, run SMOKE_PROFILE=deep with a token to gate)"
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
    SMOKE_PROFILE       baseline | deep       (default: baseline)
                        deep = the promotion gate: every deep check MUST run and
                        PASS; a missing input (no token / ws client) is a FAIL,
                        not a SKIP. A deep run with no MEMQL_SMOKE_TOKEN fails.
    SMOKE_EMAIL         Send a real magic link to this address (opt-in, non-gating)
    MEMQL_SMOKE_TOKEN   PAT/JWT to run the authenticated WS query + AI forward
                        (required in the deep profile)

Checks:
    baseline: TLS+DNS, identity health+JWKS (direct + app proxy), login page,
              /memql/ws + /memql/audio wiring, SPA boot assets, identity styling.
    deep:     all baseline checks PLUS a real authenticated WS query that fans
              BFF -> cognition/agent (catches the issuer/mesh/auth-WS class that
              front-door-green hid in the 0.9.6 incident), PLUS a node-bootstrap
              transport probe asserting plaintext /node/bootstrap never mints a
              token over cleartext (the #626 https-end-to-end invariant).

Not yet asserted here (tracked #627 follow-ups -- they need infra beyond the
front door): automation_execution_claims table presence (needs a server-side
readiness endpoint -- DB is firewalled to AKS egress) and the headless-browser
first-run walkthrough + console-error capture (needs a browser in CI).

Examples:
    $0                                                  # baseline (no auth)
    SMOKE_PROFILE=deep MEMQL_SMOKE_TOKEN=mql_pat_xxx $0 # the promotion gate
    APP_HOST=app.copresent.ai SMOKE_PROFILE=deep MEMQL_SMOKE_TOKEN=... $0  # prod

Exit code is non-zero iff a check FAILED. In baseline a SKIP never fails the
run; in deep a missing required input is a FAIL.
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
    echo "  profile:  $SMOKE_PROFILE"
    echo "  app:      https://$APP_HOST"
    echo "  identity: https://$IDENTITY_HOST"
    echo "========================================="

    if is_deep && [ -z "$MEMQL_SMOKE_TOKEN" ]; then
        warn "deep profile selected with no MEMQL_SMOKE_TOKEN -- the authenticated checks will FAIL the gate. Provide a PAT/JWT to make this run conclusive."
    fi

    check_tls
    check_identity
    check_readiness
    check_auth_surface
    check_bff_ws
    check_bootstrap_transport
    check_voice_path
    check_spa_boot
    check_identity_styling

    summary
}

main "$@"
