#!/usr/bin/env bash
#
# scripts/deploy/aks-deploy.sh
# ============================
#
# End-to-end AKS deploy for the memQL node mesh (znasllc-io/memql#532,
# epic #522 -- the AKS path that SUPERSEDES the ACA-era make deploy #495).
# One operator-run command takes the cluster from source to a rolled-out,
# smoke-checked deploy:
#
#   1. BUILD + PUSH the engine node images via `az acr build` -- one image
#      per memQL-repo node-type, each compiled with its build tag (voice
#      additionally CGO + the voice-runtime stage). Pushed to the shared
#      ACR as memql-<type>:<VERSION>. Skippable with --skip-build.
#   2. ENSURE SECRETS: the internal TLS CA + identity cert (identity-tls +
#      memql-ca) via deploy/k8s/tls/gen-internal-ca.sh, and a warning if
#      the out-of-band memql-secrets (genesis b64 + master key + DSN) is
#      absent (pods won't go ready without it).
#   3. APPLY (ordered): namespace, then IDENTITY FIRST -- it runs the
#      one-time DB migration and serves JWKS, so it must be Ready before
#      the workers roll (otherwise their verifiers churn). Then kustomize-
#      apply the rest and wait for every Deployment to roll out.
#   4. SMOKE: run scripts/deploy/staging-smoke-test.sh against the live
#      front door (non-fatal -- DNS/cert propagation can lag a fresh apply).
#
# Engine images built here (memQL repo, per-node build tags):
#   identity cognition voice agent planner workbench
# The bff node runs the CoPresent BFF CARRIER (memql-bff-copresent) and the
# SPA runs the copresent image -- both are built + version-pinned from their
# OWN sibling repos under the BFF->memQL pin model (#491), NOT here. Their
# tags stay as pinned in deploy/k8s/{bff,copresent}.yaml.
#
# When --version is given, all six engine Deployments are pinned to that
# single tag at apply time (kubectl set image, non-mutating to the tracked
# manifests) -- the image-version-consistency posture from #534. Without
# --version (or with --skip-build) the manifests' own pinned tags apply.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure, one responsibility per function, main() at the
# bottom. set -uo pipefail (no -e -- a non-fatal smoke step must not abort
# the run). Supports --help and a --dry-run that prints the full plan and
# mutates nothing (and never calls Azure).

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
K8S_DIR="$REPO_ROOT/deploy/k8s"

NAMESPACE="memql"
SECRET_NAME="memql-secrets"

# Shared ACR (Basic). LOGIN_SERVER is the registry host the manifests use.
ACR_NAME="${ACR_NAME:-acrmemql}"
ACR_LOGIN_SERVER="${ACR_LOGIN_SERVER:-${ACR_NAME}.azurecr.io}"

# Engine node-types built from THIS repo (each its own build-tag image).
# bff (carrier) + copresent (SPA) come from sibling repos -- not built here.
readonly ENGINE_NODE_TYPES=(identity cognition voice agent planner workbench)

# Every Deployment this script rolls -- the rollback target set when the
# post-deploy smoke gate fails (engine nodes + the bff carrier + the SPA).
readonly ALL_DEPLOYMENTS=(identity bff cognition voice agent planner workbench copresent)

# Per-Deployment rollout wait.
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function section() { echo ""; echo "===== $* ====="; }
function info()    { echo "INFO: $*"; }
function warn()    { echo "WARNING: $*"; }
function plan()    { echo "  [plan] $*"; }

# run_or_plan CMD... -- execute the command, or in --dry-run just print it.
function run_or_plan() {
    if [ "$DRY_RUN" = true ]; then
        plan "$*"
        return 0
    fi
    "$@"
}

#=============================================================================
# ARGS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

End-to-end AKS deploy for the memQL node mesh: build+push engine images,
ensure TLS secrets, apply the manifests (identity first), wait for rollout,
and smoke-test the live front door.

Options:
    --version=X.Y.Z   Engine image tag to build + roll out. When set, all
                      six engine Deployments are pinned to this single tag
                      (image-version consistency, #534). Default: the tags
                      already pinned in deploy/k8s/.
    --env=ENV         Environment label for log context (staging|production).
                      The target cluster is the current kubectl context.
                      Default: staging.
    --skip-build      Do not build/push images; apply already-pushed tags.
    --skip-tls        Do not (re)generate the internal CA + identity cert.
    --no-smoke        Skip the post-deploy smoke test (also disables the gate).
    --no-gate         Run the smoke test but do NOT auto-rollback on failure
                      (useful when a smoke failure is environmental, e.g.
                      DNS/cert propagation lag). Default: gate ON.
    --dry-run         Print the full plan and mutate nothing (no Azure calls).
    --help            Show this help.

Engine images built (memQL repo, per-node build tags):
    ${ENGINE_NODE_TYPES[*]}
The bff carrier (memql-bff-copresent) and copresent SPA are built + pinned
from their own repos; their manifest tags are used as-is.

Prerequisite (one-time, out-of-band -- REAL values, never committed):
    kubectl create secret generic $SECRET_NAME -n $NAMESPACE \\
      --from-literal=MEMQL_MASTER_KEY="\$MEMQL_MASTER_KEY" \\
      --from-literal=MEMQL_GENESIS_B64="\$(base64 < ~/.memql/genesis.znas)" \\
      --from-literal=MEMORY_NODES_DATABASE_DSN="\$(tiger db connection-string xahn9ru4v6 --with-password)"

Examples:
    $0 --version=0.9.6                 # build, push, roll out 0.9.6
    $0 --version=0.9.6 --dry-run       # full plan, no changes
    $0 --skip-build                    # apply the manifests' pinned tags
EOF
}

function parse_arguments() {
    VERSION=""
    ENV="staging"
    SKIP_BUILD=false
    SKIP_TLS=false
    NO_SMOKE=false
    GATE=true
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            --version=*) VERSION="${1#*=}"; shift ;;
            --env=*)     ENV="${1#*=}"; shift ;;
            --skip-build) SKIP_BUILD=true; shift ;;
            --skip-tls)   SKIP_TLS=true; shift ;;
            --no-smoke)   NO_SMOKE=true; shift ;;
            --no-gate)    GATE=false; shift ;;
            --dry-run)    DRY_RUN=true; shift ;;
            --help)       show_help; exit 0 ;;
            *)
                echo "ERROR: Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
    done
}

#=============================================================================
# PRE-FLIGHT
#=============================================================================

function check_prerequisites() {
    if ! command -v kubectl &> /dev/null; then
        echo "ERROR: kubectl is not installed"; exit 1
    fi
    if [ ! -f "$K8S_DIR/kustomization.yaml" ]; then
        echo "ERROR: no kustomization at $K8S_DIR"; exit 1
    fi
    # Build needs az unless we're skipping it or just planning.
    if [ "$SKIP_BUILD" = false ] && [ "$DRY_RUN" = false ] && ! command -v az &> /dev/null; then
        echo "ERROR: az (Azure CLI) is required to build images; use --skip-build to apply pushed tags"; exit 1
    fi
    if [ "$DRY_RUN" = false ] && ! kubectl cluster-info &> /dev/null; then
        echo "ERROR: no reachable Kubernetes cluster in the current context."
        echo "       Set it, e.g.: az aks get-credentials -g rg-memql-$ENV -n <cluster>"
        exit 1
    fi
    info "rendering deploy/k8s/ kustomization..."
    if ! kubectl kustomize "$K8S_DIR" > /dev/null 2>&1; then
        echo "ERROR: kustomize render failed"; exit 1
    fi
}

#=============================================================================
# 1. BUILD + PUSH
#=============================================================================

# Build + push one engine node image via az acr build. voice is the CGO
# exception: it needs CGO_ENABLED=1 and the debian-based voice-runtime stage
# (distroless can't host the libopus shared libs).
function build_one() {
    local nt="$1" tag="$2"
    local image="memql-${nt}:${tag}"
    local -a args=(acr build --registry "$ACR_NAME" --image "$image"
        --platform linux/amd64 --build-arg "BUILD_TAGS=${nt}" -f "$REPO_ROOT/Dockerfile")
    if [ "$nt" = "voice" ]; then
        args+=(--build-arg "CGO_ENABLED=1" --target voice-runtime)
    fi
    info "building ${ACR_LOGIN_SERVER}/${image} (BUILD_TAGS=${nt})..."
    run_or_plan az "${args[@]}" "$REPO_ROOT"
}

function build_and_push() {
    section "1. Build + push engine images -> ${ACR_LOGIN_SERVER}"
    if [ "$SKIP_BUILD" = true ]; then
        info "--skip-build set; deploying already-pushed tags."
        return 0
    fi
    if [ -z "$VERSION" ]; then
        warn "no --version given; skipping build (the manifests' pinned per-node tags will apply)."
        warn "pass --version=X.Y.Z to build + roll out a single consistent tag, or --skip-build to silence this."
        SKIP_BUILD=true
        return 0
    fi
    local nt
    for nt in "${ENGINE_NODE_TYPES[@]}"; do
        build_one "$nt" "$VERSION"
    done
    info "carrier (memql-bff-copresent) + SPA (copresent) are built + pinned from their own repos -- not built here."
}

#=============================================================================
# 2. SECRETS
#=============================================================================

function ensure_tls_secrets() {
    section "2a. Internal TLS (identity-tls + memql-ca)"
    if [ "$SKIP_TLS" = true ]; then
        info "--skip-tls set; assuming identity-tls + memql-ca already exist."
        return 0
    fi
    local gen="$K8S_DIR/tls/gen-internal-ca.sh"
    if [ ! -x "$gen" ] && [ ! -f "$gen" ]; then
        warn "TLS generator not found at $gen; identity HTTPS will fail without identity-tls/memql-ca."
        return 0
    fi
    info "generating + applying internal CA and identity cert..."
    run_or_plan bash "$gen"
}

function warn_if_app_secret_missing() {
    section "2b. App secret (genesis envelope + DSN)"
    if [ "$DRY_RUN" = true ]; then
        plan "kubectl get secret $SECRET_NAME -n $NAMESPACE  (verify the out-of-band secret exists)"
        return 0
    fi
    if ! kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" &> /dev/null; then
        warn "Secret '$SECRET_NAME' not found in namespace '$NAMESPACE'."
        warn "Pods reference it via envFrom and will NOT become ready until it exists (see --help)."
    else
        info "Secret '$SECRET_NAME' present."
    fi
}

#=============================================================================
# 3. APPLY (ordered: identity first)
#=============================================================================

function apply_namespace() {
    run_or_plan kubectl apply -f "$K8S_DIR/namespace.yaml"
}

# Override an engine Deployment's image to the built VERSION (no-op unless
# we built). Deployment + container names both equal the node-type.
function set_engine_image() {
    local nt="$1"
    [ "$SKIP_BUILD" = true ] && return 0
    [ -z "$VERSION" ] && return 0
    run_or_plan kubectl set image "deployment/${nt}" \
        "${nt}=${ACR_LOGIN_SERVER}/memql-${nt}:${VERSION}" -n "$NAMESPACE"
}

function rollout_wait() {
    local nt="$1"
    if [ "$DRY_RUN" = true ]; then
        plan "kubectl rollout status deployment/${nt} -n $NAMESPACE --timeout=$ROLLOUT_TIMEOUT"
        return 0
    fi
    if ! kubectl rollout status "deployment/${nt}" -n "$NAMESPACE" --timeout="$ROLLOUT_TIMEOUT"; then
        warn "deployment/${nt} did not report Ready within $ROLLOUT_TIMEOUT (continuing; check 'kubectl get pods -n $NAMESPACE')."
    fi
}

function apply_ordered() {
    section "3. Apply (identity first, then the mesh)"
    apply_namespace

    # identity FIRST: it owns the one-time migration + JWKS. Workers' verifiers
    # need it Ready (they retry JWKS non-fatally, but Ready-first avoids churn).
    info "applying identity and waiting for it to be Ready..."
    run_or_plan kubectl apply -f "$K8S_DIR/identity.yaml"
    set_engine_image identity
    rollout_wait identity

    # The rest of the mesh + public entry.
    info "applying the full mesh (kustomize)..."
    run_or_plan kubectl apply -k "$K8S_DIR"

    local nt
    for nt in cognition voice agent planner workbench; do
        set_engine_image "$nt"
    done
    for nt in cognition voice agent planner workbench bff; do
        rollout_wait "$nt"
    done
}

#=============================================================================
# 4. SMOKE
#=============================================================================

function smoke_test() {
    section "4. Smoke test (live front door)"
    if [ "$NO_SMOKE" = true ]; then
        info "--no-smoke set; skipping."
        return 0
    fi
    if [ "$DRY_RUN" = true ]; then
        plan "bash scripts/deploy/staging-smoke-test.sh   (baseline checks)"
        return 0
    fi
    local smoke="$SCRIPT_DIR/staging-smoke-test.sh"
    if [ ! -f "$smoke" ]; then
        warn "smoke script not found at $smoke; skipping."
        return 0
    fi
    info "running baseline smoke checks..."
    if ! bash "$smoke"; then
        warn "smoke test reported failures (see above)."
        return 1
    fi
    return 0
}

#=============================================================================
# 5. HEALTH GATE + ROLLBACK
#=============================================================================

# Roll every Deployment we just applied back to its previous ReplicaSet.
# `kubectl rollout undo` is a no-op for a Deployment with no prior revision
# (a first-ever deploy), so this is safe to fan across all of them.
function rollback_all() {
    warn "rolling back to the previous revision of each Deployment..."
    local nt
    for nt in "${ALL_DEPLOYMENTS[@]}"; do
        run_or_plan kubectl rollout undo "deployment/${nt}" -n "$NAMESPACE"
    done
    warn "rollback issued. Watch: kubectl get pods -n $NAMESPACE -w"
}

# The health gate: run the smoke test; if it fails and the gate is armed,
# auto-rollback and exit non-zero so the operator (or CI) sees the failure.
# --no-smoke skips the check entirely; --no-gate runs it but won't revert.
function health_gate() {
    if smoke_test; then
        return 0
    fi
    # smoke_test returns 0 for the skipped/dry-run cases, so reaching here
    # means a real failure.
    if [ "$GATE" = false ]; then
        warn "smoke gate is OFF (--no-gate); leaving the new revision in place despite the failure."
        return 1
    fi
    section "5. Health gate FAILED -> rollback"
    rollback_all
    return 1
}

#=============================================================================
# REPORT
#=============================================================================

function state_report() {
    section "Deploy summary"
    echo "  Env:        $ENV"
    echo "  Context:    $([ "$DRY_RUN" = true ] && echo '(dry-run)' || kubectl config current-context 2>/dev/null || echo unknown)"
    echo "  Namespace:  $NAMESPACE"
    echo "  Registry:   $ACR_LOGIN_SERVER"
    if [ "$SKIP_BUILD" = true ] || [ -z "$VERSION" ]; then
        echo "  Images:     manifest-pinned per-node tags (no build)"
    else
        echo "  Images:     engine nodes -> :$VERSION (built + rolled out)"
    fi
    local gate_label
    if [ "$NO_SMOKE" = true ]; then
        gate_label="off (--no-smoke)"
    elif [ "$GATE" = true ]; then
        gate_label="on (auto-rollback on failure)"
    else
        gate_label="report-only (--no-gate)"
    fi
    echo "  Smoke gate: $gate_label"
    echo "  Dry run:    $DRY_RUN"
    if [ "$DRY_RUN" = false ]; then
        echo "  Watch:      kubectl get pods -n $NAMESPACE -w"
        echo "  Rollback:   make deploy-rollback   (or: scripts/deploy/aks-rollback.sh)"
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"
    check_prerequisites

    echo "========================================="
    echo "memQL AKS deploy"
    echo "  env=$ENV  version=${VERSION:-<manifest-pinned>}  skip-build=$SKIP_BUILD  dry-run=$DRY_RUN"
    echo "========================================="

    build_and_push
    ensure_tls_secrets
    warn_if_app_secret_missing
    apply_ordered

    local gate_rc=0
    health_gate || gate_rc=$?

    state_report
    if [ "$gate_rc" -ne 0 ]; then
        echo ""
        echo "RESULT: deploy FAILED the smoke gate. $([ "$GATE" = true ] && echo 'Previous revision restored (rollback issued).' || echo 'New (failing) revision left in place per --no-gate.')"
        exit 1
    fi
}

main "$@"
