#!/usr/bin/env bash
#
# scripts/deploy/bff-bluegreen-cutover.sh
# =======================================
#
# WIP (znasllc-io/memql#616): blue/green BFF cutover. DRAFT -- the cutover
# sequence + drain semantics are under owner review; do NOT run against staging
# until the open questions in PR #<this> are resolved and bff-bluegreen.yaml is
# wired into kustomization.yaml.
#
# WHAT THIS DOES (distinct from #615 graceful-drain, which already shipped):
#   #615 makes a SINGLE pool's pods drain cleanly when they're killed on a roll.
#   #616 runs TWO colors at once so connected users STAY on their current
#   version until they disconnect, while NEW logins go to the new version:
#
#     1. Bring up the NEW color at the new image, wait Ready.
#     2. FLIP the user-facing entry Services (bff-active + bff-external) selector
#        memql/color: OLD -> NEW. From here NEW logins land on NEW; existing
#        streams stay pinned to their OLD-color pods (already-established
#        connections are not re-steered by a selector change).
#     3. DRAIN: poll the OLD color's pods' /healthz activeStreams (the #616
#        primitive) until every OLD pod reports 0 active streams, or the
#        --max-drain deadline hits.
#     4. TEARDOWN the OLD color (scale to 0 / delete). Its pods take the #615
#        graceful-drain path (preStop + GOAWAY + terminationGracePeriod) for any
#        residual streams at the deadline.
#
# ROLLBACK: before teardown (step 4), rollback is just flipping the selector
# back to OLD -- both colors are still up, so new logins return to OLD and the
# NEW color drains instead. After teardown it's a normal redeploy of the prior
# color/image.
#
# Per repo + global Skills+Scripts convention (CLAUDE.md): function-based, one
# responsibility per function, main() at the bottom. set -uo pipefail (no -e --
# a single pod poll failing must not abort the drain loop). --help + --dry-run.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

NAMESPACE="memql"

# The user-facing entry Services whose selector we flip. The in-mesh `bff`
# Service is deliberately NOT here: it stays color-agnostic so cross-node
# forwards reach whichever color holds the user's stream.
readonly ENTRY_SERVICES=(bff-active bff-external)

COLOR_LABEL="memql/color"

# Drain poll cadence + ceiling. Existing streams are long-lived (a session can
# last hours), so the default ceiling is generous; past it the OLD color is torn
# down and residual streams take the #615 graceful path.
DRAIN_POLL_INTERVAL="${DRAIN_POLL_INTERVAL:-15}"   # seconds between polls
DEFAULT_MAX_DRAIN="3600"                            # seconds (1h) hard ceiling

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function section() { echo ""; echo "===== $* ====="; }
function info()    { echo "INFO: $*"; }
function warn()    { echo "WARNING: $*"; }
function plan()    { echo "  [plan] $*"; }

function run_or_plan() {
    if [ "$DRY_RUN" = true ]; then plan "$*"; return 0; fi
    "$@"
}

#=============================================================================
# ARGS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 --to=<blue|green> [options]

Blue/green BFF cutover (#616): bring up the target color, flip the user-facing
entry Services to it so new logins land on the new version, drain the old color
until its existing streams close, then tear the old color down.

Options:
    --to=COLOR        Target (NEW) color to cut over TO: blue|green. Required.
    --version=X.Y.Z   Image tag for the NEW color (set on bff-<color>). If
                      omitted, the NEW color uses its manifest-pinned tag.
    --max-drain=SECS  Max seconds to wait for the OLD color to reach 0 active
                      streams before forced teardown. Default: $DEFAULT_MAX_DRAIN.
    --no-teardown     Flip + drain only; leave the OLD color running (manual
                      teardown later). Useful for a watched first cutover.
    --dry-run         Print the full plan and mutate nothing.
    --help            Show this help.

Cutover sequence:
    1. Set NEW color image (if --version) + wait Ready.
    2. Flip ${ENTRY_SERVICES[*]} selector $COLOR_LABEL -> NEW.
    3. Poll OLD color pods /healthz activeStreams until 0 or --max-drain.
    4. Scale OLD color to 0 (unless --no-teardown).

Rollback (pre-teardown): re-run with --to=<OLD color> to flip back.
EOF
}

function parse_arguments() {
    TO_COLOR=""
    VERSION=""
    MAX_DRAIN="$DEFAULT_MAX_DRAIN"
    NO_TEARDOWN=false
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            --to=*)        TO_COLOR="${1#*=}"; shift ;;
            --version=*)   VERSION="${1#*=}"; shift ;;
            --max-drain=*) MAX_DRAIN="${1#*=}"; shift ;;
            --no-teardown) NO_TEARDOWN=true; shift ;;
            --dry-run)     DRY_RUN=true; shift ;;
            --help)        show_help; exit 0 ;;
            *) echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

function validate_arguments() {
    case "$TO_COLOR" in
        blue)  FROM_COLOR="green" ;;
        green) FROM_COLOR="blue" ;;
        *) echo "ERROR: --to must be 'blue' or 'green'"; show_help; exit 1 ;;
    esac
}

function check_prerequisites() {
    if ! command -v kubectl &> /dev/null; then
        echo "ERROR: kubectl is not installed"; exit 1
    fi
    if [ "$DRY_RUN" = false ] && ! kubectl cluster-info &> /dev/null; then
        echo "ERROR: no reachable Kubernetes cluster in the current context."; exit 1
    fi
}

#=============================================================================
# 1. BRING UP NEW COLOR
#=============================================================================

function ensure_new_color_ready() {
    section "1. Bring up NEW color (bff-${TO_COLOR})"
    if [ -n "$VERSION" ]; then
        info "pinning bff-${TO_COLOR} to image tag ${VERSION}..."
        run_or_plan kubectl set image "deployment/bff-${TO_COLOR}" \
            "bff=acrmemql.azurecr.io/memql-bff-copresent:${VERSION}" -n "$NAMESPACE"
    else
        info "no --version; bff-${TO_COLOR} uses its manifest-pinned tag."
    fi
    if [ "$DRY_RUN" = true ]; then
        plan "kubectl rollout status deployment/bff-${TO_COLOR} -n $NAMESPACE --timeout=180s"
        return 0
    fi
    if ! kubectl rollout status "deployment/bff-${TO_COLOR}" -n "$NAMESPACE" --timeout=180s; then
        echo "ERROR: NEW color bff-${TO_COLOR} did not become Ready; aborting BEFORE the selector flip." >&2
        exit 1
    fi
}

#=============================================================================
# 2. FLIP THE SELECTOR (new logins -> new color)
#=============================================================================

function flip_entry_selector() {
    section "2. Flip user-facing entry to NEW color (${FROM_COLOR} -> ${TO_COLOR})"
    local svc
    for svc in "${ENTRY_SERVICES[@]}"; do
        info "patching Service/${svc} ${COLOR_LABEL} -> ${TO_COLOR}..."
        run_or_plan kubectl patch "service/${svc}" -n "$NAMESPACE" --type merge \
            -p "{\"spec\":{\"selector\":{\"app.kubernetes.io/name\":\"bff\",\"${COLOR_LABEL}\":\"${TO_COLOR}\"}}}"
    done
    info "NEW logins now land on ${TO_COLOR}; existing streams stay on ${FROM_COLOR}."
}

#=============================================================================
# 3. DRAIN THE OLD COLOR (poll activeStreams -> 0)
#=============================================================================

# Sum activeStreams across all OLD-color pods by curling each pod's /healthz
# from inside the cluster. Prints the integer total (0 if none/unreachable-as-0
# is NOT assumed -- unreachable pods are skipped with a warning).
function old_color_active_streams() {
    local pods total=0 pod streams
    pods="$(kubectl get pods -n "$NAMESPACE" \
        -l "app.kubernetes.io/name=bff,${COLOR_LABEL}=${FROM_COLOR}" \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)"
    if [ -z "$pods" ]; then echo 0; return 0; fi
    for pod in $pods; do
        # kubectl exec into the pod and read its own /healthz (localhost). This
        # avoids needing the activeStreams field on the color-pinned Service.
        streams="$(kubectl exec -n "$NAMESPACE" "$pod" -c bff -- \
            sh -c 'wget -qO- http://127.0.0.1:8085/healthz 2>/dev/null || curl -s http://127.0.0.1:8085/healthz' 2>/dev/null \
            | grep -o '"activeStreams":[0-9]*' | head -1 | grep -o '[0-9]*')"
        if [ -z "$streams" ]; then
            warn "could not read activeStreams from pod ${pod}; treating as NON-zero (will keep draining)."
            streams=1
        fi
        total=$(( total + streams ))
    done
    echo "$total"
}

function drain_old_color() {
    section "3. Drain OLD color (bff-${FROM_COLOR}) until 0 active streams"
    if [ "$DRY_RUN" = true ]; then
        plan "poll bff-${FROM_COLOR} pods /healthz activeStreams every ${DRAIN_POLL_INTERVAL}s until 0 or ${MAX_DRAIN}s"
        return 0
    fi
    local waited=0 total
    while :; do
        total="$(old_color_active_streams)"
        info "OLD color (${FROM_COLOR}) active streams: ${total} (waited ${waited}s / ${MAX_DRAIN}s)"
        if [ "$total" -le 0 ] 2>/dev/null; then
            info "OLD color drained to 0 active streams."
            return 0
        fi
        if [ "$waited" -ge "$MAX_DRAIN" ]; then
            warn "max-drain (${MAX_DRAIN}s) reached with ${total} stream(s) still open on ${FROM_COLOR}."
            warn "proceeding to teardown -- residual streams take the #615 graceful path (GOAWAY + grace period)."
            return 0
        fi
        sleep "$DRAIN_POLL_INTERVAL"
        waited=$(( waited + DRAIN_POLL_INTERVAL ))
    done
}

#=============================================================================
# 4. TEARDOWN OLD COLOR
#=============================================================================

function teardown_old_color() {
    section "4. Teardown OLD color (bff-${FROM_COLOR})"
    if [ "$NO_TEARDOWN" = true ]; then
        info "--no-teardown set; leaving bff-${FROM_COLOR} running. Scale it down manually when satisfied:"
        info "  kubectl scale deployment/bff-${FROM_COLOR} -n $NAMESPACE --replicas=0"
        return 0
    fi
    info "scaling bff-${FROM_COLOR} to 0..."
    run_or_plan kubectl scale "deployment/bff-${FROM_COLOR}" -n "$NAMESPACE" --replicas=0
    info "OLD color scaled to 0. (Kept as a 0-replica Deployment so the next cutover can flip back to it.)"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"
    validate_arguments
    check_prerequisites

    echo "========================================="
    echo "BFF blue/green cutover (#616) -- WIP DRAFT"
    echo "  from=${FROM_COLOR}  to=${TO_COLOR}  version=${VERSION:-<manifest-pinned>}"
    echo "  max-drain=${MAX_DRAIN}s  no-teardown=${NO_TEARDOWN}  dry-run=${DRY_RUN}"
    echo "========================================="

    ensure_new_color_ready
    flip_entry_selector
    drain_old_color
    teardown_old_color

    section "Cutover complete"
    echo "  Active color: ${TO_COLOR}"
    echo "  Rollback (pre/post): re-run with --to=${FROM_COLOR} (bring it back up first if torn down)."
}

main "$@"
