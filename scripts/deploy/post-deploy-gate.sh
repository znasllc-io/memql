#!/usr/bin/env bash
#
# scripts/deploy/post-deploy-gate.sh
# ==================================
#
# The FUNCTIONAL post-deploy gate (znasllc-io/memql#1519) + the bff
# auto-promote step (#1520). The authority that stamps a release VALIDATED --
# augmenting the digest-drift-only signal (drift-check.sh), which today went
# green on a deploy that was actually broken in two silent ways:
#
#   1. The 0.9.60 bff blue/green Rollout (autoPromotionEnabled:false) parked at
#      BlueGreenPause and was never promoted: bff-active's selector stayed on
#      the OLD rollouts-pod-template-hash (a stale color with no agent peer ->
#      `siSuggest: no connected agent node available`). drift-check (digest
#      match) + `kubectl rollout status` BOTH reported the deploy healthy.
#   2. Identity JWKS diverged across replicas (per-pod keys) -> ~50% of token
#      verifications failed. Completely silent.
#
# A digest-only check can see NEITHER. This gate is functional: it promotes the
# bff Rollout, then proves the release is actually serving correctly across
# THREE conditions, failing LOUDLY (non-zero) if any does not hold:
#
#   1. bff promoted    -- `kubectl argo rollouts status bff` == Healthy AND the
#                         bff-active Service selector's rollouts-pod-template-hash
#                         == the Rollout's stable (release) color. Catches the
#                         false-green unpromoted-Rollout case (#1).
#   2. JWKS coherent   -- poll the LB /.well-known/jwks.json N times AND every
#                         identity pod directly; FAIL if the served kid set
#                         diverges across replicas (or across polls). Catches
#                         per-pod-key divergence (#2).
#   3. functional auth -- a real authenticated round-trip that exercises
#                         BFF -> agent forwarding (the in-cluster
#                         class="service_account" deploy-gate query, or an
#                         authed query with MEMQL_SMOKE_TOKEN). Catches
#                         "no connected agent node available".
#
# PROMOTE (#1520). promote_bff() folds the bff blue/green promotion into the
# flow: after the new color is Ready and its prePromotionAnalysis (deploy-gate
# AnalysisRun) is green, it runs `kubectl argo rollouts promote bff` (handling
# the documented "may need promoting twice" case), then asserts bff-active ==
# the release color. If the AnalysisRun FAILED it does NOT promote (the old
# color stays active) and fails LOUDLY -- a failed analysis must never flip
# traffic.
#
# Wired into scripts/deploy/aks-deploy.sh (promote_bff before the health gate;
# run_functional_gate inside health_gate as the validation authority). Also
# runnable standalone for re-validation:  post-deploy-gate.sh --env=staging
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -euo pipefail.
# Idempotent + re-runnable: a promote on an already-promoted Rollout is a
# no-op, and the checks are pure reads.

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

NAMESPACE="${MEMQL_NAMESPACE:-memql}"
ROLLOUT_NAME="${BFF_ROLLOUT_NAME:-bff}"
ACTIVE_SERVICE="${BFF_ACTIVE_SERVICE:-bff-active}"

# JWKS coherence: how many times to poll the LB JWKS (each poll may land on a
# different identity replica behind the Service), and the per-request timeout.
JWKS_POLLS="${JWKS_POLLS:-8}"
JWKS_CURL_TIMEOUT="${JWKS_CURL_TIMEOUT:-10}"

# JWKS sources. The LB front door (same-origin proxy on the app host) plus the
# identity host itself; both must serve a coherent, replica-agnostic key set.
APP_HOST="${APP_HOST:-app.staging.copresent.ai}"
IDENTITY_HOST="${IDENTITY_HOST:-identity.staging.copresent.ai}"
# Identity pods serve JWKS over HTTPS on :8085 at /.well-known/jwks.json
# (deploy/k8s/base/identity.yaml). Polled per-pod via `kubectl exec`.
IDENTITY_POD_PORT="${IDENTITY_POD_PORT:-8085}"
IDENTITY_SELECTOR="${IDENTITY_SELECTOR:-app.kubernetes.io/name=identity}"

# Functional auth probe. Reuses the in-cluster deploy-gate-check binary (the
# class="service_account" path, #691) shipped as the deploy-gate image and run
# as a one-shot Job against bff-active -- exactly the authenticated BFF->agent
# round-trip the AnalysisTemplate runs. Image + Secret are the same artifacts
# the Rollout's prePromotionAnalysis already uses.
DEPLOY_GATE_IMAGE="${DEPLOY_GATE_IMAGE:-}"   # resolved from the AnalysisTemplate if empty
DEPLOY_GATE_JWT_SECRET="${DEPLOY_GATE_JWT_SECRET:-deploy-gate-jwt}"
GATE_JOB_NAME="${GATE_JOB_NAME:-post-deploy-functional-auth}"
GATE_JOB_TIMEOUT="${GATE_JOB_TIMEOUT:-120s}"

# Promote can need a second nudge while the controller settles
# (Argo Rollouts "may need promoting twice"). How many times to retry and how
# long to wait for the active selector to flip to the release color.
PROMOTE_RETRIES="${PROMOTE_RETRIES:-3}"
PROMOTE_SETTLE_SECONDS="${PROMOTE_SETTLE_SECONDS:-10}"
ROLLOUT_HEALTHY_TIMEOUT="${ROLLOUT_HEALTHY_TIMEOUT:-180s}"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function section() { echo ""; echo "===== $* ====="; }
function info()    { echo "INFO: $*"; }
function warn()    { echo "WARNING: $*"; }
function pass()    { echo "PASS: $*"; }
function fail()    { echo "FAIL: $*"; }
function plan()    { echo "  [plan] $*"; }

#=============================================================================
# ARGS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

The FUNCTIONAL post-deploy gate (#1519) + bff auto-promote (#1520). Promotes
the bff blue/green Rollout, then validates a release across THREE conditions:
bff-promoted, jwks-coherence, and functional-auth (a real BFF->agent
round-trip). A failure on any blocks/flags the release loudly (non-zero exit).

Options:
    --env=ENV         Environment label for log context (staging|production).
                      The target cluster is the current kubectl context.
                      Default: staging.
    --promote-only    Run ONLY the bff promote step (#1520); skip the gate.
    --gate-only       Run ONLY the 3-condition gate (#1519); skip the promote.
    --no-promote      Run the gate but do not promote the bff Rollout (assumes
                      it is already promoted / auto-promotion is on).
    --dry-run         Print the plan and mutate nothing (no cluster writes).
    --help            Show this help.

The three gate conditions (#1519):
    1. bff promoted    rollout status Healthy AND $ACTIVE_SERVICE selector hash
                       == the Rollout's release (stable) color.
    2. JWKS coherent   the kid set served by the LB /.well-known/jwks.json and
                       by every identity pod is IDENTICAL across replicas.
    3. functional auth an authenticated BFF->agent round-trip succeeds (the
                       in-cluster service_account deploy-gate query, or an
                       authed query via MEMQL_SMOKE_TOKEN).

Env knobs: APP_HOST, IDENTITY_HOST, JWKS_POLLS ($JWKS_POLLS), DEPLOY_GATE_IMAGE,
DEPLOY_GATE_JWT_SECRET ($DEPLOY_GATE_JWT_SECRET), MEMQL_SMOKE_TOKEN.

Examples:
    $0 --env=staging            # promote bff, then run the full gate
    $0 --gate-only              # validate an already-promoted release
    $0 --promote-only           # just promote the bff Rollout (#1520)
EOF
}

function parse_arguments() {
    ENV="staging"
    DO_PROMOTE=true
    DO_GATE=true
    NO_PROMOTE=false
    DRY_RUN=false
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)       ENV="${1#*=}"; shift ;;
            --promote-only) DO_GATE=false; shift ;;
            --gate-only)    DO_PROMOTE=false; shift ;;
            --no-promote)   NO_PROMOTE=true; DO_PROMOTE=false; shift ;;
            --dry-run)      DRY_RUN=true; shift ;;
            --help)         show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 2 ;;
        esac
    done
}

function check_prerequisites() {
    if ! command -v kubectl &> /dev/null; then
        echo "ERROR: kubectl is not installed" >&2; exit 2
    fi
    if [ "$DRY_RUN" = false ] && ! kubectl cluster-info &> /dev/null; then
        echo "ERROR: no reachable Kubernetes cluster in the current context." >&2
        exit 2
    fi
}

#=============================================================================
# ROLLOUT COLOR HELPERS
#=============================================================================

# rollout_status -- echo the Argo Rollouts phase (Healthy|Degraded|Paused|...).
# Uses the kubectl-argo-rollouts plugin's `status` subcommand. The plugin
# blocks until terminal unless --watch=false, so we read the field directly.
function rollout_status() {
    kubectl argo rollouts status "$ROLLOUT_NAME" -n "$NAMESPACE" --watch=false 2>/dev/null \
        | head -n1 | tr -d '[:space:]'
}

# release_color -- the rollouts-pod-template-hash of the Rollout's STABLE (post-
# promotion release) ReplicaSet. This is the color bff-active MUST select once
# the release is promoted. Read from the Rollout status' stableRS field.
function release_color() {
    kubectl get rollout "$ROLLOUT_NAME" -n "$NAMESPACE" \
        -o jsonpath='{.status.stableRS}' 2>/dev/null
}

# active_color -- the rollouts-pod-template-hash the bff-active Service currently
# selects. After a successful promote this MUST equal release_color.
function active_color() {
    kubectl get service "$ACTIVE_SERVICE" -n "$NAMESPACE" \
        -o jsonpath='{.spec.selector.rollouts-pod-template-hash}' 2>/dev/null
}

#=============================================================================
# 0. PROMOTE (#1520)
#=============================================================================

# promote_bff -- fold the bff blue/green promotion into the deploy flow. After
# the new color is Ready and its prePromotionAnalysis (the deploy-gate
# AnalysisRun) is green, run `kubectl argo rollouts promote bff` (retrying for
# the "may need promoting twice" case), then assert bff-active flips to the
# release color. If the AnalysisRun FAILED, do NOT promote -- fail loudly and
# leave the old color active.
function promote_bff() {
    section "0. Promote bff blue/green Rollout (#1520)"

    if [ "$DRY_RUN" = true ]; then
        plan "verify prePromotionAnalysis (deploy-gate AnalysisRun) for $ROLLOUT_NAME is green"
        plan "kubectl argo rollouts promote $ROLLOUT_NAME -n $NAMESPACE  (retry up to $PROMOTE_RETRIES for 'promote twice')"
        plan "assert $ACTIVE_SERVICE selector hash == Rollout stable color"
        return 0
    fi

    if ! kubectl get rollout "$ROLLOUT_NAME" -n "$NAMESPACE" &> /dev/null; then
        warn "no Rollout '$ROLLOUT_NAME' in '$NAMESPACE' -- bff is not yet converted to a blue/green Rollout (deploy/rollouts/bff-rollout.yaml). Skipping promote; the gate's bff-promoted check will treat a plain Deployment as already-active."
        return 0
    fi

    # If a prePromotionAnalysis AnalysisRun FAILED, refuse to promote.
    if analysis_failed; then
        fail "the bff prePromotionAnalysis (deploy-gate) is FAILED -- NOT promoting. The old color stays active. Investigate the AnalysisRun before retrying."
        kubectl argo rollouts get rollout "$ROLLOUT_NAME" -n "$NAMESPACE" 2>/dev/null | head -n 40 || true
        return 1
    fi

    local want; want="$(release_color)"
    local have; have="$(active_color)"
    if [ -n "$want" ] && [ "$want" = "$have" ]; then
        info "bff-active already selects the release color ($want) -- promote is a no-op (idempotent)."
        return 0
    fi

    local attempt=1
    while [ "$attempt" -le "$PROMOTE_RETRIES" ]; do
        info "promoting $ROLLOUT_NAME (attempt $attempt/$PROMOTE_RETRIES)..."
        # `promote` exits 0 even when a further nudge is needed; we re-check the
        # active selector below rather than trusting the exit code alone.
        kubectl argo rollouts promote "$ROLLOUT_NAME" -n "$NAMESPACE" 2>&1 | sed 's/^/  promote: /' || true
        sleep "$PROMOTE_SETTLE_SECONDS"
        want="$(release_color)"
        have="$(active_color)"
        if [ -n "$want" ] && [ "$want" = "$have" ]; then
            pass "bff promoted: $ACTIVE_SERVICE now selects the release color ($want)."
            return 0
        fi
        info "active color ($have) != release color ($want) yet; the controller may need promoting twice. Retrying..."
        attempt=$((attempt + 1))
    done

    fail "bff did NOT promote: after $PROMOTE_RETRIES attempts $ACTIVE_SERVICE selects '$have' but the release color is '$want'. The Rollout may be paused (BlueGreenPause) or the analysis is still running."
    return 1
}

# analysis_failed -- true iff the LATEST prePromotion AnalysisRun for the
# Rollout is in a Failed/Error/Inconclusive phase. A missing/absent AnalysisRun
# is NOT a failure (auto-promotion or a fresh Rollout may not have one yet).
function analysis_failed() {
    local phase
    phase="$(kubectl get analysisrun -n "$NAMESPACE" \
        -l "rollouts-pod-template-hash" \
        --sort-by=.metadata.creationTimestamp \
        -o jsonpath='{.items[-1:].status.phase}' 2>/dev/null || true)"
    case "$phase" in
        Failed|Error|Inconclusive) return 0 ;;
        *) return 1 ;;
    esac
}

#=============================================================================
# 1. GATE CONDITION: bff PROMOTED
#=============================================================================

# check_bff_promoted -- condition 1: the Rollout is Healthy AND bff-active
# selects the release (stable) color. This is the check that catches the
# false-green: a Rollout parked at BlueGreenPause leaves bff-active on the old
# color, which `kubectl rollout status` + drift-check both miss.
function check_bff_promoted() {
    section "Gate 1/3: bff promoted (Rollout Healthy + active selector == release color)"

    if [ "$DRY_RUN" = true ]; then
        plan "kubectl argo rollouts status $ROLLOUT_NAME == Healthy"
        plan "assert $ACTIVE_SERVICE selector hash == Rollout stable color (catches the unpromoted false-green)"
        return 0
    fi

    if ! kubectl get rollout "$ROLLOUT_NAME" -n "$NAMESPACE" &> /dev/null; then
        # No Rollout: bff is a plain Deployment. The false-green class doesn't
        # exist (no blue/green color to strand), so this condition is vacuously
        # satisfied -- but say so explicitly.
        warn "no Rollout '$ROLLOUT_NAME' (plain Deployment) -- the blue/green false-green class does not apply; condition vacuously OK."
        pass "bff promoted (n/a: not a blue/green Rollout)"
        return 0
    fi

    local status; status="$(rollout_status)"
    local want; want="$(release_color)"
    local have; have="$(active_color)"

    if [ "$status" != "Healthy" ]; then
        fail "bff Rollout status is '$status', not Healthy (paused/degraded == the false-green incident #1519)."
        return 1
    fi
    if [ -z "$want" ]; then
        fail "could not read the Rollout's release (stable) color -- cannot prove the active Service points at it."
        return 1
    fi
    if [ "$want" != "$have" ]; then
        fail "$ACTIVE_SERVICE selects color '$have' but the release (stable) color is '$want' -- the Rollout never promoted (the 0.9.60 BlueGreenPause / stale-color incident)."
        return 1
    fi
    pass "bff promoted: Rollout Healthy and $ACTIVE_SERVICE selects the release color ($want)."
    return 0
}

#=============================================================================
# 2. GATE CONDITION: JWKS COHERENT
#=============================================================================

# kid_set_from_jwks -- read a JWKS document on stdin, emit its sorted, unique
# kid set as a single space-joined line. Pure text; no jq dependency assumed
# (grep/sed/sort), so it runs in CI and on a minimal operator box.
function kid_set_from_jwks() {
    grep -oE '"kid"[[:space:]]*:[[:space:]]*"[^"]+"' \
        | sed -E 's/.*"kid"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' \
        | sort -u | tr '\n' ' ' | sed -E 's/[[:space:]]+$//'
}

# poll_lb_jwks_kids -- poll the LB JWKS JWKS_POLLS times; each poll may land on
# a different identity replica. Echo one kid-set line per poll.
function poll_lb_jwks_kids() {
    local host="$1" i body kids
    for ((i = 1; i <= JWKS_POLLS; i++)); do
        body="$(curl -sS --max-time "$JWKS_CURL_TIMEOUT" \
            "https://$host/.well-known/jwks.json" 2>/dev/null || true)"
        kids="$(printf '%s' "$body" | kid_set_from_jwks)"
        echo "$kids"
    done
}

# poll_pod_jwks_kids -- query EACH identity pod directly (in-pod curl to its own
# :8085 JWKS over the internal CA), echoing one kid-set line per pod. Catches
# per-pod key divergence the LB might mask if a poll happens to miss a replica.
function poll_pod_jwks_kids() {
    local pods pod body kids
    pods="$(kubectl get pods -n "$NAMESPACE" -l "$IDENTITY_SELECTOR" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
    [ -z "$pods" ] && return 0
    for pod in $pods; do
        # In-pod fetch over the internal CA. -k is acceptable here: we trust the
        # in-cluster endpoint and care only about the kid SET it serves, not the
        # cert chain (that is asserted separately by the smoke test).
        body="$(kubectl exec -n "$NAMESPACE" "$pod" -c identity -- \
            curl -sS -k --max-time "$JWKS_CURL_TIMEOUT" \
            "https://127.0.0.1:${IDENTITY_POD_PORT}/.well-known/jwks.json" 2>/dev/null || true)"
        kids="$(printf '%s' "$body" | kid_set_from_jwks)"
        echo "${pod}=${kids}"
    done
}

# check_jwks_coherent -- condition 2: every identity replica serves the SAME kid
# set. FAIL if the LB polls or the per-pod reads disagree. Catches the silent
# per-pod-key divergence that broke ~50% of token verifications (#1519).
function check_jwks_coherent() {
    section "Gate 2/3: JWKS coherent across identity replicas"

    if [ "$DRY_RUN" = true ]; then
        plan "poll https://$APP_HOST/.well-known/jwks.json $JWKS_POLLS times (each may hit a different replica)"
        plan "poll https://$IDENTITY_HOST/.well-known/jwks.json $JWKS_POLLS times"
        plan "query each identity pod's :$IDENTITY_POD_PORT JWKS directly; FAIL if any kid set diverges"
        return 0
    fi

    local all_sets=() line ref="" diverged=0 host
    # LB polls (app same-origin proxy + identity host).
    for host in "$APP_HOST" "$IDENTITY_HOST"; do
        while IFS= read -r line; do
            [ -z "$line" ] && { fail "empty/unreachable JWKS from https://$host (a replica served no keys)"; diverged=1; continue; }
            all_sets+=("LB:$host=$line")
        done < <(poll_lb_jwks_kids "$host")
    done
    # Per-pod direct reads.
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        all_sets+=("pod:$line")
    done < <(poll_pod_jwks_kids)

    if [ "${#all_sets[@]}" -eq 0 ]; then
        fail "no JWKS could be read from any source -- cannot prove coherence."
        return 1
    fi

    # Establish a reference kid set (the value after the LAST '='), then assert
    # every observation matches it.
    local entry label kids
    for entry in "${all_sets[@]}"; do
        label="${entry%%=*}"
        kids="${entry#*=}"
        if [ -z "$ref" ]; then
            ref="$kids"
            info "reference kid set ($label): [$ref]"
            continue
        fi
        if [ "$kids" != "$ref" ]; then
            fail "JWKS DIVERGENCE: $label serves [$kids] but the reference is [$ref] -- per-replica key divergence (the silent ~50% token-verify failure, #1519)."
            diverged=1
        else
            info "coherent: $label serves the reference kid set."
        fi
    done

    if [ "$diverged" -ne 0 ]; then
        return 1
    fi
    pass "JWKS coherent: every LB poll and every identity pod serves the same kid set [$ref]."
    return 0
}

#=============================================================================
# 3. GATE CONDITION: FUNCTIONAL AUTH ROUND-TRIP
#=============================================================================

# resolve_gate_image -- the deploy-gate image to run the functional auth probe.
# Prefer the explicit DEPLOY_GATE_IMAGE; else read the image the Rollout's
# AnalysisTemplate already uses, so the probe and the in-Rollout gate run the
# SAME validated bytes.
function resolve_gate_image() {
    if [ -n "$DEPLOY_GATE_IMAGE" ]; then
        echo "$DEPLOY_GATE_IMAGE"; return 0
    fi
    kubectl get analysistemplate deploy-gate -n "$NAMESPACE" \
        -o jsonpath='{.spec.metrics[0].provider.job.spec.template.spec.containers[0].image}' 2>/dev/null || true
}

# run_gate_job -- run the deploy-gate-check binary as a one-shot Job against
# bff-active (the release color, post-promote), authenticating with the
# deploy-gate-jwt service_account Secret. This is the SAME authenticated
# BFF->agent round-trip the prePromotionAnalysis runs, but now post-promote
# against the LIVE active color. Returns 0 only if the Job completes.
function run_gate_job() {
    local image="$1"
    kubectl delete job "$GATE_JOB_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    # Apply a minimal Job that invokes the gate entrypoint directly (the image
    # is distroless: no /bin/sh -- the #712 trap). The query exercises
    # BFF -> cognition/agent forwarding.
    kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $GATE_JOB_NAME
  namespace: $NAMESPACE
spec:
  backoffLimit: 1
  activeDeadlineSeconds: 110
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: gate
          image: $image
          args:
            - "--addr=${ACTIVE_SERVICE}:50051"
            - "--query=queryActiveSpaceIds({})"
          env:
            - name: MEMQL_SVC_JWT
              valueFrom:
                secretKeyRef:
                  name: $DEPLOY_GATE_JWT_SECRET
                  key: MEMQL_SVC_JWT
EOF
    if kubectl wait --for=condition=complete --timeout="$GATE_JOB_TIMEOUT" \
        "job/$GATE_JOB_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
        kubectl logs -n "$NAMESPACE" "job/$GATE_JOB_NAME" --tail=20 2>/dev/null | sed 's/^/  gate: /' || true
        kubectl delete job "$GATE_JOB_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
        return 0
    fi
    warn "functional-auth gate Job did not complete; logs:"
    kubectl logs -n "$NAMESPACE" "job/$GATE_JOB_NAME" --tail=40 2>/dev/null | sed 's/^/  gate: /' || true
    kubectl delete job "$GATE_JOB_NAME" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    return 1
}

# check_functional_auth -- condition 3: a real authenticated round-trip that
# forwards BFF -> agent succeeds (catches "no connected agent node available").
#
# Token strategy, in preference order:
#   1. The in-cluster class="service_account" deploy-gate (the #691 machine
#      identity). Used when the deploy-gate-jwt Secret + deploy-gate image are
#      present -- the operator/CI path that needs no interactive login. This is
#      the canonical, re-runnable probe.
#   2. An authed query with MEMQL_SMOKE_TOKEN (a PAT/JWT) via the deep smoke
#      profile, when a token is provided but no in-cluster gate is wired.
#   3. DEGRADE GRACEFULLY (documented): with NEITHER, the round-trip cannot run
#      headlessly. We do NOT silently pass -- we FAIL by default so the gate is
#      conclusive, unless --no-promote / explicit GATE_FUNCTIONAL_OPTIONAL=1
#      downgrades it to a loud WARNING for an operator running a manual login
#      round-trip out of band.
function check_functional_auth() {
    section "Gate 3/3: functional auth round-trip (BFF -> agent forward)"

    if [ "$DRY_RUN" = true ]; then
        plan "run the deploy-gate-check (service_account JWT) as a Job against ${ACTIVE_SERVICE}:50051, query queryActiveSpaceIds({}) -- proves BFF accepted the token AND the engine answered (BFF->agent fan)"
        plan "fallback: authed query via MEMQL_SMOKE_TOKEN through the deep smoke profile"
        return 0
    fi

    # Path 1: in-cluster service_account gate (preferred).
    local image; image="$(resolve_gate_image)"
    local have_secret=false
    kubectl get secret "$DEPLOY_GATE_JWT_SECRET" -n "$NAMESPACE" &> /dev/null && have_secret=true
    if [ -n "$image" ] && [ "$have_secret" = true ]; then
        info "running the in-cluster service_account functional-auth probe (image $image, Secret $DEPLOY_GATE_JWT_SECRET) against $ACTIVE_SERVICE..."
        if run_gate_job "$image"; then
            pass "functional auth: the service_account JWT was accepted on $ACTIVE_SERVICE and the engine answered (BFF->agent forward live)."
            return 0
        fi
        fail "functional auth: the in-cluster service_account round-trip FAILED -- the BFF rejected the token or no agent node answered (the 'no connected agent node available' incident #1519)."
        return 1
    fi

    # Path 2: authed query via a provided smoke token (deep smoke profile).
    if [ -n "${MEMQL_SMOKE_TOKEN:-}" ]; then
        local smoke="$SCRIPT_DIR/staging-smoke-test.sh"
        if [ -x "$smoke" ] || [ -f "$smoke" ]; then
            info "running the deep smoke authenticated query (MEMQL_SMOKE_TOKEN) against the live front door (BFF->cognition/agent forward)..."
            if SMOKE_PROFILE=deep APP_HOST="$APP_HOST" IDENTITY_HOST="$IDENTITY_HOST" bash "$smoke"; then
                pass "functional auth: the deep smoke authenticated query + AI-forward succeeded."
                return 0
            fi
            fail "functional auth: the deep smoke authenticated query FAILED (token rejected or no agent node answered)."
            return 1
        fi
        warn "MEMQL_SMOKE_TOKEN is set but staging-smoke-test.sh is unavailable."
    fi

    # Path 3: degrade gracefully (documented). No headless credential available.
    if [ "${GATE_FUNCTIONAL_OPTIONAL:-0}" = "1" ]; then
        warn "functional-auth round-trip could NOT run headlessly (no deploy-gate-jwt Secret + deploy-gate image, and no MEMQL_SMOKE_TOKEN). GATE_FUNCTIONAL_OPTIONAL=1 -> downgrading to a WARNING. Run a manual authenticated login + BFF->agent query before declaring this release validated."
        return 0
    fi
    fail "functional-auth round-trip could NOT run: no in-cluster service_account gate (deploy-gate-jwt Secret + deploy-gate image) and no MEMQL_SMOKE_TOKEN. A gate that cannot authenticate proves nothing -- provision the deploy-gate-jwt Secret (see deploy/rollouts/README.md) or pass MEMQL_SMOKE_TOKEN. Set GATE_FUNCTIONAL_OPTIONAL=1 to downgrade to a warning for a manual round-trip."
    return 1
}

#=============================================================================
# GATE ORCHESTRATION
#=============================================================================

# run_functional_gate -- run all three conditions, collecting failures so the
# report lists every problem (not just the first). Returns non-zero iff any
# condition failed. This is the authority aks-deploy.sh consults to stamp a
# release validated.
function run_functional_gate() {
    section "Functional post-deploy gate (#1519) -- env=$ENV"
    local rc=0
    check_bff_promoted   || rc=1
    check_jwks_coherent  || rc=1
    check_functional_auth || rc=1

    section "Gate verdict"
    if [ "$rc" -eq 0 ]; then
        pass "ALL THREE conditions hold -- release VALIDATED (bff promoted + JWKS coherent + functional auth)."
    else
        fail "release NOT validated -- one or more functional conditions failed above. This BLOCKS the release (a digest-only check would have falsely reported green)."
    fi
    return "$rc"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"
    check_prerequisites

    local rc=0
    if [ "$DO_PROMOTE" = true ]; then
        promote_bff || rc=1
    elif [ "$NO_PROMOTE" = true ]; then
        info "--no-promote: skipping the bff promote step (assuming already promoted)."
    fi

    # If the promote failed, the gate will fail condition 1 anyway, but running
    # it still surfaces the JWKS / functional-auth state for the operator.
    if [ "$DO_GATE" = true ]; then
        run_functional_gate || rc=1
    fi

    if [ "$rc" -ne 0 ]; then
        echo ""
        echo "RESULT: post-deploy gate FAILED -- the release is NOT validated. Do not promote to prod; investigate above."
        exit 1
    fi
    echo ""
    echo "RESULT: post-deploy gate PASSED -- bff promoted and the release is functionally validated."
}

main "$@"
