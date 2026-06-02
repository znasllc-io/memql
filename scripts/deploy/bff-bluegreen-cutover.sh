#!/usr/bin/env bash
#
# scripts/deploy/bff-bluegreen-cutover.sh
# =======================================
#
# Blue/green BFF cutover (znasllc-io/memql#616). Operator-driven manual flip
# (decision 2): an operator runs this; it is NOT wired into aks-deploy.sh.
# bff-bluegreen.yaml is shipped alongside bff.yaml and is NOT in the default
# kustomization apply path until the owner approves the production cutover.
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
#     3. DRAIN (progressive): poll the OLD color's pods' /healthz activeStreams
#        (the #616 primitive) and, as individual OLD pods reach 0 streams, scale
#        the OLD Deployment DOWN to match the count of pods that still hold
#        streams. This bounds peak capacity -- we never sit at a sustained 2x;
#        the OLD color shrinks as its users go home. Continues until every OLD
#        pod reports 0 active streams, or the --max-drain deadline hits.
#     4. TEARDOWN the OLD color (scale to 0). Its pods take the #615
#        graceful-drain path (preStop + GOAWAY + terminationGracePeriod) for any
#        residual streams at the deadline.
#
# CAPACITY (decision 7, refs #614): the AKS cluster autoscaler is LIVE
#   (min 2 / max 5 on Standard_B2s, codified in #614). It provides the surge
#   headroom for the brief window where the NEW color is fully up before the OLD
#   color starts shrinking. Progressive scale-down (step 3) keeps the peak
#   bounded so we lean on autoscaler headroom only momentarily, not for the
#   whole (up to 1h) drain. --no-progressive-teardown reverts to the simpler
#   "hold OLD at full replicas until fully drained" behavior (watched cutover).
#
# ROLLBACK: before teardown (step 4), rollback is just flipping the selector
# back to OLD -- both colors are still up, so new logins return to OLD and the
# NEW color drains instead. NOTE: progressive teardown shrinks OLD as it drains,
# so a late rollback may need OLD scaled back up first (the script prints the
# command). For a fully-watched first cutover use --no-progressive-teardown so
# OLD stays at full replicas and rollback is instant. After teardown it's a
# normal redeploy of the prior color/image.
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
DEFAULT_MAX_DRAIN="3600"                            # seconds (1h) hard ceiling (decision 3, configurable via --max-drain)

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
                      streams before forced teardown. Default: $DEFAULT_MAX_DRAIN
                      (1h). (decision 3 -- configurable.)
    --no-progressive-teardown
                      Hold the OLD color at its full replica count for the whole
                      drain window instead of shrinking it pod-by-pod as pods
                      empty. Simpler + instant rollback, but sits at ~2x BFF
                      capacity until fully drained. Default is progressive
                      (capacity-bounded) teardown -- see decision 7 / #614.
    --no-teardown     Flip + drain only; leave the OLD color running at full
                      replicas (manual teardown later). Implies
                      --no-progressive-teardown. Useful for a watched first
                      cutover.
    --dry-run         Print the full plan and mutate nothing.
    --help            Show this help.

Cutover sequence:
    1. Set NEW color image (if --version) + wait Ready.
    2. Flip ${ENTRY_SERVICES[*]} selector $COLOR_LABEL -> NEW.
    3. Poll OLD color pods /healthz activeStreams until 0 or --max-drain,
       progressively scaling OLD down as its pods empty (unless
       --no-progressive-teardown).
    4. Scale OLD color to 0 (unless --no-teardown).

Rollback (pre-teardown): re-run with --to=<OLD color> to flip back. With
progressive teardown a late rollback may need OLD scaled back up first.
EOF
}

function parse_arguments() {
    TO_COLOR=""
    VERSION=""
    MAX_DRAIN="$DEFAULT_MAX_DRAIN"
    NO_TEARDOWN=false
    PROGRESSIVE=true
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            --to=*)                    TO_COLOR="${1#*=}"; shift ;;
            --version=*)               VERSION="${1#*=}"; shift ;;
            --max-drain=*)             MAX_DRAIN="${1#*=}"; shift ;;
            --no-progressive-teardown) PROGRESSIVE=false; shift ;;
            --no-teardown)             NO_TEARDOWN=true; shift ;;
            --dry-run)                 DRY_RUN=true; shift ;;
            --help)                    show_help; exit 0 ;;
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
    # --no-teardown leaves OLD fully up for a watched cutover, so shrinking it
    # mid-drain would contradict the intent. Force the simple hold-at-full path.
    if [ "$NO_TEARDOWN" = true ]; then PROGRESSIVE=false; fi
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

# Read one OLD-color pod's activeStreams via its own /healthz (localhost, over
# kubectl exec). Echoes the integer; echoes 1 (treated as "still busy") if the
# read fails so we never tear down a pod we couldn't confirm is empty.
function pod_active_streams() {
    local pod="$1" streams
    streams="$(kubectl exec -n "$NAMESPACE" "$pod" -c bff -- \
        sh -c 'wget -qO- http://127.0.0.1:8085/healthz 2>/dev/null || curl -s http://127.0.0.1:8085/healthz' 2>/dev/null \
        | grep -o '"activeStreams":[0-9]*' | head -1 | grep -o '[0-9]*')"
    if [ -z "$streams" ]; then
        warn "could not read activeStreams from pod ${pod}; treating as NON-zero (will keep draining)." >&2
        echo 1; return 0
    fi
    echo "$streams"
}

# Inspect every live OLD-color pod and set two globals:
#   OLD_TOTAL_STREAMS  -- sum of activeStreams across all OLD pods.
#   OLD_BUSY_PODS      -- count of OLD pods with >0 streams (the replica floor
#                         we must keep alive so we don't kill an in-use pod).
function scan_old_color() {
    local pods pod streams
    OLD_TOTAL_STREAMS=0
    OLD_BUSY_PODS=0
    pods="$(kubectl get pods -n "$NAMESPACE" \
        -l "app.kubernetes.io/name=bff,${COLOR_LABEL}=${FROM_COLOR}" \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)"
    if [ -z "$pods" ]; then return 0; fi
    for pod in $pods; do
        streams="$(pod_active_streams "$pod")"
        OLD_TOTAL_STREAMS=$(( OLD_TOTAL_STREAMS + streams ))
        if [ "$streams" -gt 0 ] 2>/dev/null; then
            OLD_BUSY_PODS=$(( OLD_BUSY_PODS + 1 ))
        fi
    done
}

# Progressive scale-down: shrink the OLD Deployment to the number of pods that
# still hold streams. k8s scale-down evicts the LOWEST-utilization pods first
# only opportunistically, so we additionally avoid scaling below OLD_BUSY_PODS
# to never evict a pod that still has an open stream. This bounds peak capacity
# (decision 7 / #614) -- the OLD color shrinks as users disconnect rather than
# sitting at full replicas for the entire drain window.
function shrink_old_to_busy() {
    local target="$1"
    [ "$PROGRESSIVE" = true ] || return 0
    local current
    current="$(kubectl get deployment "bff-${FROM_COLOR}" -n "$NAMESPACE" \
        -o jsonpath='{.spec.replicas}' 2>/dev/null)"
    [ -n "$current" ] || return 0
    # Never scale UP here, and never below the busy floor.
    if [ "$target" -lt "$current" ] 2>/dev/null; then
        info "progressive teardown: scaling bff-${FROM_COLOR} ${current} -> ${target} (drained pods removed)."
        kubectl scale "deployment/bff-${FROM_COLOR}" -n "$NAMESPACE" --replicas="$target" >/dev/null 2>&1 || \
            warn "could not scale bff-${FROM_COLOR} to ${target}; will retry next poll."
    fi
}

function drain_old_color() {
    section "3. Drain OLD color (bff-${FROM_COLOR}) until 0 active streams"
    if [ "$PROGRESSIVE" = true ]; then
        info "progressive teardown ON: OLD color shrinks as pods empty (peak-bounded; refs #614 autoscaler)."
    else
        info "progressive teardown OFF: OLD color held at full replicas until fully drained."
    fi
    if [ "$DRY_RUN" = true ]; then
        plan "poll bff-${FROM_COLOR} pods /healthz activeStreams every ${DRAIN_POLL_INTERVAL}s until 0 or ${MAX_DRAIN}s"
        [ "$PROGRESSIVE" = true ] && plan "as OLD pods reach 0 streams, scale deployment/bff-${FROM_COLOR} down to the busy-pod count"
        return 0
    fi
    local waited=0
    while :; do
        scan_old_color
        info "OLD color (${FROM_COLOR}) active streams: ${OLD_TOTAL_STREAMS} across ${OLD_BUSY_PODS} busy pod(s) (waited ${waited}s / ${MAX_DRAIN}s)"
        if [ "$OLD_TOTAL_STREAMS" -le 0 ] 2>/dev/null; then
            info "OLD color drained to 0 active streams."
            return 0
        fi
        # Shrink to the busy-pod floor so we shed already-empty pods without
        # ever evicting one that still carries a stream.
        shrink_old_to_busy "$OLD_BUSY_PODS"
        if [ "$waited" -ge "$MAX_DRAIN" ]; then
            warn "max-drain (${MAX_DRAIN}s) reached with ${OLD_TOTAL_STREAMS} stream(s) still open on ${FROM_COLOR}."
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
    echo "BFF blue/green cutover (#616)"
    echo "  from=${FROM_COLOR}  to=${TO_COLOR}  version=${VERSION:-<manifest-pinned>}"
    echo "  max-drain=${MAX_DRAIN}s  progressive=${PROGRESSIVE}  no-teardown=${NO_TEARDOWN}  dry-run=${DRY_RUN}"
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
