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
#   4. PROMOTE + GATE: promote the bff blue/green Rollout (#1520) so traffic
#      flips off the old color, then run the FUNCTIONAL post-deploy gate
#      (#1519, scripts/deploy/post-deploy-gate.sh) -- bff-promoted + JWKS
#      coherent + a real BFF->agent authenticated round-trip -- as the authority
#      that stamps a release validated (the digest-drift guard alone went green
#      on the broken 0.9.60 deploy). Then the live-front-door smoke test.
#
# Engine images built here (memQL repo, per-node build tags):
#   identity cognition voice agent planner workbench
# The bff node runs the CoPresent BFF CARRIER (memql-bff-copresent) and the
# SPA runs the copresent image -- both are built + version-pinned from their
# OWN sibling repos under the BFF->memQL pin model (#491), NOT here. Their
# tags stay as pinned in deploy/k8s/{bff,copresent}.yaml.
#
# deployment-v2 Phase 1 (#699): the committed kustomize overlay
# (deploy/k8s/overlays/<env>) is the SINGLE image authority -- every image is
# pinned there by @sha256: DIGEST. This script applies that overlay; it no
# longer mutates the cluster out-of-band. There is NO `kubectl set image` and
# NO `kubectl rollout undo`: a new version is a digest change in the overlay
# (committed + applied/reconciled), and ROLLBACK = `git revert` of that change
# (re-apply the overlay at the prior commit). This makes #684 (manifest tag !=
# live image; rollback reverts to the wrong tag) structurally impossible.
# `--version` is retained only for the engine BUILD step (az acr build tag);
# it no longer pins live images -- the overlay digest does.
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
# Phase 1 (#699): base = raw manifests; the per-env overlay pins images by
# digest and is what gets applied/reconciled. OVERLAY_DIR is resolved from $ENV
# at apply time (see apply_ordered / check_prerequisites).
BASE_DIR="$K8S_DIR/base"

NAMESPACE="memql"
SECRET_NAME="memql-secrets"

# Shared ACR (Basic). LOGIN_SERVER is the registry host the manifests use.
ACR_NAME="${ACR_NAME:-acrmemql}"
ACR_LOGIN_SERVER="${ACR_LOGIN_SERVER:-${ACR_NAME}.azurecr.io}"

# Node-types built here (each its own build-tag image). bff (carrier) +
# copresent (SPA) come from sibling repos -- not built here.
#
# Architecture (#1053): nodes that execute CoPresent DSL (agentReply etc.) MUST
# be CARRIER-built (memql-bff-copresent/Dockerfile + BUILD_TAGS=<type>, context =
# workspace parent) so they carry the CoPresent DSL -- same as the bff carrier
# and the local k3d cluster (scripts/k3d/dev.sh builds the carrier set the same
# way). voice (CGO voice-runtime,
# transport-only), identity (auth), and mcp (remote MCP head, memql#1550) have
# no CoPresent refs and stay engine-built.
readonly ENGINE_NODE_TYPES=(identity cognition voice agent planner workbench mcp)
# Subset of ENGINE_NODE_TYPES that must be carrier-built (CoPresent DSL).
readonly CARRIER_NODE_TYPES=(cognition agent planner workbench)
# Carrier build context = the workspace parent (memql + memql-bff-copresent
# siblings, per the `replace ../memql` directive) + its Dockerfile.
readonly WORKSPACE_ROOT="$(cd "$REPO_ROOT/.." && pwd)"
readonly CARRIER_DOCKERFILE="$WORKSPACE_ROOT/memql-bff-copresent/Dockerfile"
# Shared carrier-base image repo (build-speed #1507). The carrier node types
# differ ONLY by the final `go build -tags <nt>`, so the expensive prefix
# (deps + tailwind + source + templ + build-css) is built ONCE as this image
# and every carrier compile reuses it via the Dockerfile's CARRIER_BASE arg --
# turning "N full carrier builds" into "1 base build + N tag-only compiles".
readonly CARRIER_BASE_REPO="memql-carrier-base"
# Engine node types that do NOT depend on the carrier base (no CoPresent DSL),
# so they can build concurrently with the base (#1512 wave A). This is exactly
# ENGINE_NODE_TYPES minus CARRIER_NODE_TYPES.
readonly ENGINE_ONLY_NODE_TYPES=(identity voice mcp)
# Poll interval (seconds) for the async `az acr build --no-wait` runs (#1512).
ACR_POLL_INTERVAL="${ACR_POLL_INTERVAL:-15}"

# Every Deployment this script rolls -- the rollback target set when the
# post-deploy smoke gate fails (engine nodes + the bff carrier + the SPA).
readonly ALL_DEPLOYMENTS=(identity bff cognition voice agent planner workbench copresent mcp)

# Per-Deployment rollout wait.
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"

# Pre-deploy headroom guard (#614). A RollingUpdate surges one extra pod per
# Deployment (every engine + carrier + SPA manifest pins maxSurge=1), so a roll
# transiently needs free CPU for that many surge pods on top of steady state.
# Each surge pod requests ~200m CPU (the manifests' per-node CPU request); the
# SPA is lighter but we use the conservative engine figure as a single knob.
# If the nodepool's free allocatable CPU can't cover the surge, the roll can
# deadlock with Pending surge pods (the #614 incident). With the cluster
# autoscaler enabled (aks-autoscaler.sh) this self-heals; until then we WARN,
# or FAIL when --gate-headroom is set so CI/operators catch it pre-roll.
SURGE_POD_CPU_MILLI="${SURGE_POD_CPU_MILLI:-200}"

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
    --version=X.Y.Z   Engine image tag to BUILD + push (az acr build). It no
                      longer pins live images -- the digest-pinned overlay
                      deploy/k8s/overlays/<env> is the single image authority
                      (deployment-v2 Phase 1 #699). Default: --skip-build.
    --env=ENV         Environment label for log context (staging|production).
                      The target cluster is the current kubectl context.
                      Default: staging.
    --skip-build      Do not build/push images; apply already-pushed tags.
    --skip-tls        Do not (re)generate the internal CA + identity cert.
    --skip-migrate    Do not run the gated pre-deploy DB migration Job.
    --no-smoke        Skip the post-deploy smoke test (the functional gate
                      #1519 + version-skew guard still run).
    --no-gate         Run the checks (version-skew guard, bff promote #1520,
                      functional gate #1519, smoke) but do NOT auto-rollback on
                      failure -- and downgrade the functional-auth condition
                      that needs a headless credential to a warning (useful when
                      a failure is environmental, e.g. DNS/cert propagation lag
                      or a missing deploy-gate-jwt Secret). Default: gate ON.
    --gate-headroom   FAIL the deploy if the nodepool lacks free CPU for the
                      rolling-update surge (#614). Default: WARN only.
    --skip-headroom   Skip the pre-deploy nodepool headroom check entirely.
    --allow-overwrite Permit re-cutting an engine tag that already exists in ACR.
                      Default OFF: release tags are immutable (the promotion gate
                      rests on a validated tag never being re-cut in place). Use
                      only to deliberately rebuild an UNVALIDATED tag.
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
      --from-literal=MEMQL_DATABASE_DSN="\$(tiger db connection-string xahn9ru4v6 --with-password)"

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
    SKIP_MIGRATE=false
    NO_SMOKE=false
    GATE=true
    DRY_RUN=false
    ALLOW_OVERWRITE=false
    GATE_HEADROOM=false
    SKIP_HEADROOM=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            --version=*) VERSION="${1#*=}"; shift ;;
            --env=*)     ENV="${1#*=}"; shift ;;
            --skip-build) SKIP_BUILD=true; shift ;;
            --skip-tls)   SKIP_TLS=true; shift ;;
            --skip-migrate) SKIP_MIGRATE=true; shift ;;
            --no-smoke)   NO_SMOKE=true; shift ;;
            --no-gate)    GATE=false; shift ;;
            --gate-headroom) GATE_HEADROOM=true; shift ;;
            --skip-headroom) SKIP_HEADROOM=true; shift ;;
            --allow-overwrite) ALLOW_OVERWRITE=true; shift ;;
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
    local overlay="$K8S_DIR/overlays/$ENV"
    if [ ! -f "$overlay/kustomization.yaml" ]; then
        echo "ERROR: no overlay kustomization at $overlay (deployment-v2 Phase 1 #699). Known overlays:"; ls "$K8S_DIR/overlays" 2>/dev/null; exit 1
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
    info "rendering $overlay (digest-pinned overlay)..."
    if ! kubectl kustomize "$overlay" > /dev/null 2>&1; then
        echo "ERROR: kustomize render failed for $overlay"; exit 1
    fi
}

#=============================================================================
# 0b. PRE-DEPLOY HEADROOM GUARD (#614)
#=============================================================================

# Verify the cluster has enough free CPU to absorb the rolling-update surge
# BEFORE we roll. A RollingUpdate brings up maxSurge extra pods per Deployment
# while the old ones still run; if the nodepool is packed (staging ran B2s at
# 91-93% CPU requests), those surge pods go Pending and the rollout deadlocks
# -- exactly the #614 incident. We sum requestable surge CPU across the
# Deployments we roll and compare it to the cluster's free allocatable CPU
# (allocatable minus already-requested, summed over Ready nodes).
#
# Outcome:
#   - enough headroom            -> INFO, proceed.
#   - short on headroom          -> WARN (default) and proceed, OR FAIL when
#                                   --gate-headroom is set.
#   - can't measure (no metrics) -> WARN and proceed (never block on a missing
#                                   read; the smoke gate still guards the roll).
# With the cluster autoscaler enabled (scripts/deploy/aks-autoscaler.sh, the
# #614 IaC) a shortfall self-heals as the autoscaler adds a node; this check is
# the belt-and-suspenders signal until/when that is live.
function check_nodepool_headroom() {
    section "0b. Pre-deploy headroom guard (rolling-update surge, #614)"

    if [ "$SKIP_HEADROOM" = true ]; then
        info "--skip-headroom set; skipping the surge headroom check."
        return 0
    fi

    # Surge pods = one per Deployment we roll (every manifest pins maxSurge=1).
    local surge_pods="${#ALL_DEPLOYMENTS[@]}"
    local need_milli=$((surge_pods * SURGE_POD_CPU_MILLI))

    if [ "$DRY_RUN" = true ]; then
        plan "compute free allocatable CPU across Ready nodes and compare to surge need (${surge_pods} pods x ${SURGE_POD_CPU_MILLI}m = ${need_milli}m)"
        return 0
    fi

    # Sum allocatable CPU (millicores) over Ready nodes.
    local alloc_milli requested_milli free_milli
    alloc_milli="$(kubectl get nodes \
        -o jsonpath='{range .items[?(@.status.allocatable.cpu)]}{.status.allocatable.cpu}{"\n"}{end}' 2>/dev/null \
        | awk '{ if ($1 ~ /m$/) { sub(/m$/,"",$1); s+=$1 } else { s+=$1*1000 } } END { print s+0 }')"

    # Sum already-requested CPU across all non-terminal pods cluster-wide.
    requested_milli="$(kubectl get pods --all-namespaces \
        -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.resources.requests.cpu}{"\n"}{end}{end}' 2>/dev/null \
        | awk 'NF { if ($1 ~ /m$/) { sub(/m$/,"",$1); s+=$1 } else { s+=$1*1000 } } END { print s+0 }')"

    if [ -z "$alloc_milli" ] || [ "$alloc_milli" -eq 0 ]; then
        warn "could not read node allocatable CPU; skipping the headroom check (the smoke gate still guards the roll)."
        return 0
    fi

    free_milli=$((alloc_milli - requested_milli))
    info "cluster allocatable CPU : ${alloc_milli}m"
    info "already requested       : ${requested_milli}m"
    info "free for surge          : ${free_milli}m"
    info "rolling-update surge need: ${need_milli}m (${surge_pods} pods x ${SURGE_POD_CPU_MILLI}m)"

    if [ "$free_milli" -ge "$need_milli" ]; then
        info "headroom OK -- the rolling-update surge fits."
        return 0
    fi

    warn "INSUFFICIENT headroom: free ${free_milli}m < surge need ${need_milli}m."
    warn "A rolling update may strand surge pods in Pending and stall the rollout (#614)."
    warn "Enable the cluster autoscaler so a roll can surge automatically:"
    warn "  bash scripts/deploy/aks-autoscaler.sh        # converges nodepool1 to min 2 / max 5 (owner-gated)"
    warn "or scale the nodepool up manually before deploying."
    if [ "$GATE_HEADROOM" = true ]; then
        echo "ERROR: headroom gate armed (--gate-headroom) and the surge does not fit; aborting BEFORE rolling the mesh." >&2
        return 1
    fi
    return 0
}

#=============================================================================
# 1. BUILD + PUSH
#=============================================================================

# Build + push one engine node image via az acr build. voice is the CGO
# exception: it needs CGO_ENABLED=1 and the debian-based voice-runtime stage
# (distroless can't host the libopus shared libs).
# ensure_tag_immutable -- refuse to overwrite an existing engine tag in ACR.
# Release tags are immutable so a version is a reproducible, trustworthy
# artifact: the whole promotion gate (#628) rests on "a validated tag is never
# re-cut in place." This is exactly what failed for 0.9.6 (identity + copresent
# images were rebuilt over the same tag during the incident, so :0.9.6 stopped
# meaning one thing). Mirrors the carrier's release.sh ensure_tag_immutable.
# Bypass with --allow-overwrite ONLY to deliberately re-cut an unvalidated tag.
function ensure_tag_immutable() {
    local nt="$1" tag="$2"
    [ "$ALLOW_OVERWRITE" = true ] && return 0
    [ "$DRY_RUN" = true ] && { plan "verify memql-${nt}:${tag} does not already exist in $ACR_NAME (immutability guard)"; return 0; }
    if az acr repository show --name "$ACR_NAME" --image "memql-${nt}:${tag}" >/dev/null 2>&1; then
        echo "ERROR: memql-${nt}:${tag} already exists in $ACR_NAME -- release tags are immutable." >&2
        echo "       Bump --version to cut a new artifact, or pass --allow-overwrite to" >&2
        echo "       deliberately re-cut an UNVALIDATED tag (never a promoted one)." >&2
        exit 1
    fi
}

# is_carrier_node -- true when this node-type must be carrier-built (#1053).
function is_carrier_node() {
    local nt="$1" c
    for c in "${CARRIER_NODE_TYPES[@]}"; do [ "$c" = "$nt" ] && return 0; done
    return 1
}

# queue_carrier_base TAG -- queue the shared carrier-base build with
# `az acr build --no-wait` and echo its ACR run-id (build-speed #1507 + #1512).
# The base is built ONCE so each carrier compile reuses it via --build-arg
# CARRIER_BASE instead of re-running the expensive prefix; `--target
# carrier-base` stops at the prefix stage (no compile). The base is an internal
# build accelerator (no manifest pins it, nothing deploys from it), so it is NOT
# subject to the release-tag immutability guard. All human output goes to
# stderr; STDOUT carries ONLY the run-id so the caller can capture + poll it.
function queue_carrier_base() {
    local tag="$1"
    local image="${CARRIER_BASE_REPO}:${tag}"
    local -a args=(acr build --registry "$ACR_NAME" --image "$image"
        --platform linux/amd64 --target carrier-base -f "$CARRIER_DOCKERFILE"
        --no-wait --query runId --output tsv "$WORKSPACE_ROOT")
    info "queuing shared carrier base ${ACR_LOGIN_SERVER}/${image} (once for the ${#CARRIER_NODE_TYPES[@]} carrier nodes)..." >&2
    if [ "$DRY_RUN" = true ]; then
        plan "az ${args[*]}" >&2
        echo "dryrun-carrier-base"
        return 0
    fi
    local run_id
    run_id="$(az "${args[@]}")"
    if [ -z "$run_id" ]; then
        echo "ERROR: failed to queue carrier-base build (no run-id returned)" >&2
        exit 1
    fi
    echo "$run_id"
}

# queue_build NT TAG -- queue one node image build with `az acr build --no-wait`
# and echo its ACR run-id (build-speed #1512). Mirrors the per-node build args:
# engine nodes build from this repo's Dockerfile, carrier nodes from the carrier
# Dockerfile (workspace-parent context) reusing the shared base via CARRIER_BASE
# (#1507); voice adds the CGO voice-runtime. All human output goes to stderr;
# STDOUT carries ONLY the run-id. In --dry-run it prints the plan + echoes a
# placeholder id so the wave plumbing stays exercised.
function queue_build() {
    local nt="$1" tag="$2"
    ensure_tag_immutable "$nt" "$tag" >&2
    local image="memql-${nt}:${tag}"
    local dockerfile="$REPO_ROOT/Dockerfile" context="$REPO_ROOT" kind="engine"
    if is_carrier_node "$nt"; then
        dockerfile="$CARRIER_DOCKERFILE"; context="$WORKSPACE_ROOT"; kind="carrier"
    fi
    local -a args=(acr build --registry "$ACR_NAME" --image "$image"
        --platform linux/amd64 --build-arg "BUILD_TAGS=${nt}" -f "$dockerfile")
    if is_carrier_node "$nt"; then
        # Reuse the pre-built shared base (#1507): the compile stage builds
        # FROM this image, so only the tag-specific `go build` runs here.
        args+=(--build-arg "CARRIER_BASE=${ACR_LOGIN_SERVER}/${CARRIER_BASE_REPO}:${tag}")
    fi
    if [ "$nt" = "voice" ]; then
        args+=(--build-arg "CGO_ENABLED=1" --target voice-runtime)
    fi
    args+=(--no-wait --query runId --output tsv "$context")
    info "queuing ${ACR_LOGIN_SERVER}/${image} (BUILD_TAGS=${nt}, ${kind})..." >&2
    if [ "$DRY_RUN" = true ]; then
        plan "az ${args[*]}" >&2
        echo "dryrun-${nt}"
        return 0
    fi
    local run_id
    run_id="$(az "${args[@]}")"
    if [ -z "$run_id" ]; then
        echo "ERROR: failed to queue build for ${image} (no run-id returned)" >&2
        exit 1
    fi
    echo "$run_id"
}

# wait_for_acr_runs LABEL=RUNID ... -- poll the given ACR runs to terminal
# status, failing the cut on the first non-Succeeded run. ACR executes queued
# `--no-wait` runs concurrently (~2-3 at a time), so queuing all builds up front
# then polling overlaps them -- wall-clock ~= the slowest single image instead
# of the sum of serial foreground builds. bash 3.2 has no associative arrays, so
# runs are tracked as "label=runid" tokens in an indexed array.
function wait_for_acr_runs() {
    [ "$#" -eq 0 ] && return 0
    if [ "$DRY_RUN" = true ]; then
        info "(dry-run) would poll $# ACR run(s) to completion: $*"
        return 0
    fi
    local -a pending=("$@")
    while [ "${#pending[@]}" -gt 0 ]; do
        local -a still=()
        local pair label run_id status
        for pair in "${pending[@]}"; do
            label="${pair%%=*}"; run_id="${pair#*=}"
            status="$(az acr task show-run --registry "$ACR_NAME" --run-id "$run_id" --query status --output tsv 2>/dev/null || true)"
            case "$status" in
                Succeeded)
                    info "  [done] ${label} (run ${run_id})"
                    ;;
                Failed|Canceled|Error|Timeout)
                    echo "ERROR: ACR build for ${label} (run ${run_id}) ended: ${status}" >&2
                    echo "       Inspect: az acr task logs --registry ${ACR_NAME} --run-id ${run_id}" >&2
                    exit 1
                    ;;
                *)
                    # Queued / Running / Started / empty -> keep polling.
                    still+=("${pair}")
                    ;;
            esac
        done
        # Reassign without tripping `set -u` on an empty array (bash 3.2).
        pending=("${still[@]:-}")
        if [ "${#pending[@]}" -eq 1 ] && [ -z "${pending[0]}" ]; then
            pending=()
        fi
        [ "${#pending[@]}" -gt 0 ] && sleep "$ACR_POLL_INTERVAL"
    done
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
    # Queue every build with `az acr build --no-wait` + poll, so ACR runs them
    # concurrently server-side (#1512) instead of serial foreground builds.
    # TWO waves because the carrier compiles build FROM the shared carrier base
    # (#1507):
    #   wave A = the base + the engine-only nodes (identity, voice) -- neither
    #            depends on the base, so they overlap its build;
    #   wave B = the carriers, queued only after the base run has SUCCEEDED
    #            (and been pushed), so their `FROM .../memql-carrier-base:<tag>`
    #            can pull it.
    local nt
    local -a waveA=("carrier-base=$(queue_carrier_base "$VERSION")")
    for nt in "${ENGINE_ONLY_NODE_TYPES[@]}"; do
        waveA+=("${nt}=$(queue_build "$nt" "$VERSION")")
    done
    info "wave A queued (carrier base + ${ENGINE_ONLY_NODE_TYPES[*]}); polling to completion..."
    wait_for_acr_runs "${waveA[@]}"

    local -a waveB=()
    for nt in "${CARRIER_NODE_TYPES[@]}"; do
        waveB+=("${nt}=$(queue_build "$nt" "$VERSION")")
    done
    info "wave B queued (carriers: ${CARRIER_NODE_TYPES[*]}); polling to completion..."
    wait_for_acr_runs "${waveB[@]}"

    info "all builds complete (1 base + ${#ENGINE_ONLY_NODE_TYPES[@]} engine + ${#CARRIER_NODE_TYPES[@]} carrier)."
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
    local gen="$BASE_DIR/tls/gen-internal-ca.sh"
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
# 2c. GATED DB MIGRATION (pre-rollout)
#=============================================================================

# Run `memql migrate` as a one-shot Job and WAIT for it to complete BEFORE
# any Deployment rolls (#553). Applying schema up front -- not racing it
# against the worker roll -- is the migration-safety win. Idempotent (bun
# advisory lock + mark-applied); identity's boot migration stays as a
# no-op fallback. A failed migration FAILS the deploy (we don't roll onto
# an unmigrated/half-migrated schema).
function run_migration_gate() {
    section "2c. Gated DB migration (memql migrate Job)"
    if [ "$SKIP_MIGRATE" = true ]; then
        info "--skip-migrate set; skipping the pre-deploy migration Job."
        return 0
    fi
    local job="$BASE_DIR/migrate-job.yaml"
    if [ ! -f "$job" ]; then
        warn "migrate Job manifest not found at $job; skipping."
        return 0
    fi

    # The Job runs the identity image; pin it to the deploy's --version when
    # we built one, else use the tag pinned in the manifest.
    local img=""
    if [ -n "$VERSION" ] && [ "$SKIP_BUILD" = false ]; then
        img="${ACR_LOGIN_SERVER}/memql-identity:${VERSION}"
    fi

    # The namespace must exist for the Job; apply it idempotently first.
    run_or_plan kubectl apply -f "$BASE_DIR/namespace.yaml"
    # Jobs are immutable -- delete any prior run before re-creating.
    run_or_plan kubectl delete job memql-migrate -n "$NAMESPACE" --ignore-not-found

    if [ "$DRY_RUN" = true ]; then
        if [ -n "$img" ]; then
            plan "sed 's#memql-identity:<tag>#memql-identity:${VERSION}#' $job | kubectl apply -f -"
        else
            plan "kubectl apply -f $job"
        fi
        plan "kubectl wait --for=condition=complete --timeout=300s job/memql-migrate -n $NAMESPACE"
        return 0
    fi

    if [ -n "$img" ]; then
        info "applying migrate Job (image ${img})..."
        if ! sed -E "s#image: .*/memql-identity:.*#image: ${img}#" "$job" | kubectl apply -f -; then
            warn "failed to apply the migrate Job."
            return 1
        fi
    else
        info "applying migrate Job (manifest-pinned image)..."
        if ! kubectl apply -f "$job"; then
            warn "failed to apply the migrate Job."
            return 1
        fi
    fi

    info "waiting for migrations to complete..."
    if ! kubectl wait --for=condition=complete --timeout=300s job/memql-migrate -n "$NAMESPACE"; then
        warn "migration Job did not complete successfully -- aborting BEFORE rolling the mesh."
        kubectl logs -n "$NAMESPACE" job/memql-migrate --tail=50 2>/dev/null || true
        return 1
    fi
    info "migrations complete."
    return 0
}

#=============================================================================
# 3. APPLY (ordered: identity first)
#=============================================================================

function apply_namespace() {
    run_or_plan kubectl apply -f "$BASE_DIR/namespace.yaml"
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

    local overlay="$K8S_DIR/overlays/$ENV"

    # Apply the full mesh + public entry from the digest-pinned overlay -- the
    # ONLY image authority. Because every image is pinned by @sha256: in the
    # overlay (NOT set at runtime), there is no post-apply `set image` and the
    # apply cannot leave any node on a stale manifest tag: the #613/#684 skew
    # class is structurally gone. This is also exactly how the Phase 2 reconciler
    # (Argo CD) will apply the same overlay.
    info "applying the full mesh (digest-pinned overlay: $overlay)..."
    run_or_plan kubectl apply -k "$overlay"

    # identity owns the one-time migration + JWKS; the workers' verifiers retry
    # JWKS non-fatally, so we wait on identity FIRST, then the rest.
    local nt
    rollout_wait identity
    for nt in cognition voice agent planner workbench mcp bff copresent; do
        rollout_wait "$nt"
    done
}

#=============================================================================
# 4. SMOKE
#=============================================================================

# The post-deploy smoke. Runs the DEEP promotion-gate profile (#627) when a
# MEMQL_SMOKE_TOKEN is available -- a real authenticated WS query + cross-node
# AI forward, so the gate proves the authenticated app path, not just front-door
# 200s (the 0.9.6 incident went front-door-green while the app was broken). With
# no token it falls back to baseline and LOUDLY flags the deploy as NOT
# promotable -- a validated promotion REQUIRES a green deep run.
function smoke_test() {
    section "4. Smoke test (live front door)"
    if [ "$NO_SMOKE" = true ]; then
        info "--no-smoke set; skipping."
        return 0
    fi

    local profile="baseline"
    [ -n "${MEMQL_SMOKE_TOKEN:-}" ] && profile="deep"

    if [ "$DRY_RUN" = true ]; then
        plan "SMOKE_PROFILE=$profile bash scripts/deploy/staging-smoke-test.sh"
        return 0
    fi
    local smoke="$SCRIPT_DIR/staging-smoke-test.sh"
    if [ ! -f "$smoke" ]; then
        warn "smoke script not found at $smoke; skipping."
        return 0
    fi
    if [ "$profile" = "deep" ]; then
        info "running DEEP smoke checks (promotion gate: authenticated WS + AI forward + SPA/identity assets)..."
    else
        warn "no MEMQL_SMOKE_TOKEN in the environment -- running BASELINE smoke only. This deploy is NOT promotable; a validated promotion requires a green deep run (SMOKE_PROFILE=deep MEMQL_SMOKE_TOKEN=...)."
    fi
    if ! SMOKE_PROFILE="$profile" bash "$smoke"; then
        warn "smoke test reported failures (see above)."
        return 1
    fi
    return 0
}

#=============================================================================
# 5. HEALTH GATE + ROLLBACK
#=============================================================================

# deployment-v2 Phase 1 (#699): ROLLBACK IS `git revert`, not `kubectl rollout
# undo`. `rollout undo` reverted to the prior ReplicaSet, which carried the
# MANIFEST tag rather than the pre-deploy image -- the #684 trap. The committed
# digest overlay is now the only image authority, so a rollback is: revert the
# overlay commit and re-apply (or let the Phase 2 reconciler converge). This
# function prints that procedure instead of mutating the cluster imperatively.
function rollback_all() {
    section "ROLLBACK -- git revert (deployment-v2 #699)"
    warn "NOT issuing 'kubectl rollout undo' (it would revert to the manifest tag, #684)."
    cat <<EOF
  The committed overlay deploy/k8s/overlays/$ENV is the only image authority.
  To roll back to the previous good digest set:

      git -C "$REPO_ROOT" revert --no-edit <bad-overlay-commit>
      git -C "$REPO_ROOT" push            # Phase 2: Argo CD reconciles automatically
      # until the reconciler is live, re-apply manually:
      kubectl apply -k "$K8S_DIR/overlays/$ENV"

  The prior digests are recoverable from git history of that overlay file.
EOF
}

# Drift guard (replaces the #613 version-skew guard): every live engine pod must
# be running the EXACT digest the committed overlay pins. Because the overlay is
# the authority, any divergence means out-of-band mutation -- which v2 forbids.
# Delegates to the dedicated drift-check.sh (--live) so the same logic gates CI.
function assert_engine_versions() {
    if [ "$DRY_RUN" = true ]; then
        plan "scripts/deploy/drift-check.sh --live --env=$ENV (assert live digests == committed overlay)"
        return 0
    fi
    section "5a. Drift guard (live digests == committed overlay)"
    local drift="$SCRIPT_DIR/drift-check.sh"
    if [ ! -x "$drift" ]; then
        warn "drift-check.sh not found/executable at $drift; skipping drift guard."
        return 0
    fi
    "$drift" --live --env="$ENV"
}

# promote_bff_rollout -- #1520: fold the bff blue/green promotion into the
# deploy flow. After apply_ordered rolls the new color, the bff Rollout
# (autoPromotionEnabled:false) sits at BlueGreenPause until promoted; nothing
# used to run the promote, so every release silently parked on the OLD color
# (the 0.9.60 incident). Delegate to post-deploy-gate.sh --promote-only, which
# verifies the prePromotionAnalysis is green, runs `kubectl argo rollouts
# promote bff` (handling the "promote twice" case), and asserts bff-active flips
# to the release color. A failed analysis does NOT promote and fails loudly.
function promote_bff_rollout() {
    local gate="$SCRIPT_DIR/post-deploy-gate.sh"
    if [ ! -f "$gate" ]; then
        warn "post-deploy-gate.sh not found at $gate; skipping the bff promote (#1520)."
        return 0
    fi
    if [ "$DRY_RUN" = true ]; then
        plan "scripts/deploy/post-deploy-gate.sh --promote-only --env=$ENV (promote the bff blue/green Rollout, #1520)"
        return 0
    fi
    bash "$gate" --promote-only --env="$ENV"
}

# The FUNCTIONAL post-deploy gate (#1519): promote bff, then validate the
# release across three conditions (bff promoted + JWKS coherent + functional
# BFF->agent auth). This is the authority that stamps a release validated --
# the digest-drift guard (assert_engine_versions) + smoke alone went GREEN on
# the 0.9.60 deploy that was actually broken (unpromoted bff color, divergent
# JWKS). Delegates to post-deploy-gate.sh; a failure here is a hard gate
# failure. --no-gate downgrades the functional condition that needs a headless
# credential to a warning (GATE_FUNCTIONAL_OPTIONAL) so an environmental gap
# doesn't block, but the bff-promoted + JWKS checks still run.
function functional_post_deploy_gate() {
    local gate="$SCRIPT_DIR/post-deploy-gate.sh"
    if [ ! -f "$gate" ]; then
        warn "post-deploy-gate.sh not found at $gate; skipping the functional gate (#1519)."
        return 0
    fi
    if [ "$DRY_RUN" = true ]; then
        plan "scripts/deploy/post-deploy-gate.sh --gate-only --env=$ENV (functional gate: bff promoted + JWKS coherent + BFF->agent auth, #1519)"
        return 0
    fi
    if [ "$GATE" = false ]; then
        GATE_FUNCTIONAL_OPTIONAL=1 bash "$gate" --gate-only --env="$ENV"
    else
        bash "$gate" --gate-only --env="$ENV"
    fi
}

# The health gate: assert no version skew, promote the bff Rollout (#1520), run
# the functional post-deploy gate (#1519), then the smoke test; if any fails and
# the gate is armed, auto-rollback and exit non-zero so the operator (or CI)
# sees the failure. --no-smoke skips the smoke check (the version-skew guard +
# functional gate still run); --no-gate runs the checks but won't revert.
function health_gate() {
    if ! assert_engine_versions; then
        if [ "$GATE" = false ]; then
            warn "version skew detected; gate is OFF (--no-gate), leaving the revision in place."
            return 1
        fi
        section "5. Version skew -> rollback"
        rollback_all
        return 1
    fi
    # #1520: promote the bff blue/green Rollout before validating -- otherwise
    # the functional gate's bff-promoted condition (and the live mesh) sees the
    # stale color.
    section "5b. Promote bff blue/green Rollout (#1520)"
    if ! promote_bff_rollout; then
        if [ "$GATE" = false ]; then
            warn "bff promote failed; gate is OFF (--no-gate), leaving the revision in place."
            return 1
        fi
        section "5. bff promote FAILED -> rollback"
        rollback_all
        return 1
    fi
    # #1519: the functional gate is the authority that stamps the release
    # validated -- catches the false-green the digest guard cannot see.
    section "5c. Functional post-deploy gate (#1519)"
    if ! functional_post_deploy_gate; then
        if [ "$GATE" = false ]; then
            warn "functional gate failed; gate is OFF (--no-gate), leaving the revision in place."
            return 1
        fi
        section "5. Functional gate FAILED -> rollback"
        rollback_all
        return 1
    fi
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

# record_validated_version -- on a GREEN DEEP gate, append this version + the
# per-engine running image digests to deploy/validated-versions.json. This is
# the promotion ledger (#628): a version becomes promotable ONLY after it passes
# the deep staging smoke, and prod deploys consume ONLY versions recorded here.
# A baseline (token-less) gate does NOT record -- it proves nothing about the
# authenticated path, so it is not a basis for promotion.
function record_validated_version() {
    [ -z "$VERSION" ] && return 0
    [ "$DRY_RUN" = true ] && return 0
    if [ -z "${MEMQL_SMOKE_TOKEN:-}" ] || [ "$NO_SMOKE" = true ]; then
        warn "deploy is green but the gate was BASELINE (no MEMQL_SMOKE_TOKEN / --no-smoke) -- NOT recording ${VERSION} as validated. A promotable version requires a green DEEP gate."
        return 0
    fi
    section "6. Record validated version (promotion ledger)"
    local registry="$REPO_ROOT/deploy/validated-versions.json"
    local ts nt; ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    local -a pairs=()
    for nt in "${ENGINE_NODE_TYPES[@]}"; do
        pairs+=("${nt}=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name="$nt" \
            -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2>/dev/null)")
    done
    VV_VERSION="$VERSION" VV_ENV="$ENV" VV_TS="$ts" VV_PAIRS="${pairs[*]}" \
        python3 - "$registry" <<'PY'
import json, os, sys
path = sys.argv[1]
try:
    data = json.load(open(path))
except Exception:
    data = {"validated": []}
digests = {}
for kv in os.environ["VV_PAIRS"].split():
    k, _, v = kv.partition("=")
    digests[k] = v
entry = {"version": os.environ["VV_VERSION"], "env": os.environ["VV_ENV"],
         "validatedAt": os.environ["VV_TS"], "gate": "deep-smoke", "digests": digests}
data.setdefault("validated", [])
data["validated"] = [e for e in data["validated"]
                     if not (e.get("version") == entry["version"] and e.get("env") == entry["env"])]
data["validated"].append(entry)
with open(path, "w") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
print("recorded validated version", entry["version"], "for", entry["env"])
PY
    info "wrote ${registry} -- COMMIT it so prod promotes only validated versions."
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

    # Verify the nodepool can absorb the rolling-update surge before we roll
    # (#614). Aborts here only when --gate-headroom is set; otherwise warns.
    if ! check_nodepool_headroom; then
        state_report
        echo ""
        echo "RESULT: deploy ABORTED -- insufficient nodepool headroom for the rolling-update surge (--gate-headroom). The mesh was NOT rolled."
        exit 1
    fi

    # Migrate up front and abort the deploy if it fails -- never roll the
    # mesh onto an unmigrated/half-migrated schema (#553).
    if ! run_migration_gate; then
        state_report
        echo ""
        echo "RESULT: deploy ABORTED -- pre-deploy migration failed. The mesh was NOT rolled."
        exit 1
    fi

    apply_ordered

    local gate_rc=0
    health_gate || gate_rc=$?

    if [ "$gate_rc" -eq 0 ]; then
        record_validated_version
    fi

    state_report
    if [ "$gate_rc" -ne 0 ]; then
        echo ""
        echo "RESULT: deploy FAILED the smoke gate. $([ "$GATE" = true ] && echo 'Previous revision restored (rollback issued).' || echo 'New (failing) revision left in place per --no-gate.')"
        exit 1
    fi
}

main "$@"
