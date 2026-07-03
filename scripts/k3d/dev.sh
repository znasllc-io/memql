#!/usr/bin/env bash
#
# scripts/k3d/dev.sh
# ==================
#
# Capability: k3d.dev -- inner-loop dev command: build one or more node images
# locally, import them into the k3d cluster, and restart the relevant
# Deployments.
#
# Design
# ------
# The local overlay pins images to a ':local' tag (not @sha256 digests)
# with imagePullPolicy: IfNotPresent. k3d pre-pulls images into its
# containerd runtime at import time, so pods always get the imported
# image rather than pulling from a registry.
#
# Because the manifest tag (':local') is stable, ArgoCD sees no diff
# after a rebuild -- the change is the image CONTENT, not the reference.
# We therefore trigger a rolling restart on the affected Deployment(s)
# after import, which causes kubelet to start fresh containers using the
# newly-imported image.
#
# This is NOT a manifest bypass: ArgoCD owns the Deployment spec;
# 'kubectl rollout restart' only touches the pod template's restart
# annotation. ArgoCD's selfHeal will not revert this because the restart
# annotation value is not part of the desired manifest. (selfHeal ignores
# pod restarts.)
#
# Supported node types and their Docker build context
# ---------------------------------------------------
#   ENGINE nodes (built from THIS repo's Dockerfile, --target <type>):
#     identity    engine identity binary (no CoPresent DSL)
#     voice       engine voice binary    (CGO, requires libopus)
#     mcp         engine mcp binary      (no CoPresent DSL)
#
#   CARRIER nodes (built from memql-bff-copresent/Dockerfile, BUILD_TAGS=<type>):
#     bff         carrier bff
#     cognition   carrier cognition
#     agent       carrier agent
#     planner     carrier planner
#     workbench   carrier workbench
#
#   LOCAL infra (pulled from public registries, not rebuilt here):
#     postgres    timescale/timescaledb -- pull + import
#     azurite     mcr.microsoft.com/azure-storage/azurite -- pull + import
#     redis       redis -- pull + import
#     livekit     livekit/livekit-server -- pull + import
#
# Usage
# -----
#   make dev                          # rebuild + restart all app nodes
#   make dev NODE=bff                 # rebuild + restart one node
#   make dev NODE=bff,cognition       # comma-separated list
#   make dev PULL_INFRA=1            # pull + re-import infra images
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Idempotent: each invocation rebuilds + re-imports the requested images and
# rolls the Deployments; safe to re-run.
#
# Exit codes: 0 ok | 2 bad param (unknown node type) | 4 prerequisite missing
#             (docker/k3d/kubectl absent, cluster not running, carrier repo
#             missing)
#
# Refs: #2066 #2061 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/engine_build_args.sh
source "${SCRIPT_DIR}/../lib/engine_build_args.sh"

cap_init "k3d.dev" "Build node image(s) locally, import into k3d, and restart Deployments."
cap_spec_param "node"       "node type(s) to rebuild, comma-separated (default: all app nodes)" ""
cap_spec_param "pull-infra" "pull + import infra images (flag)"                                 ""
cap_spec_param "cluster"    "k3d cluster name"
cap_spec_param "namespace"  "k8s namespace"
cap_spec_param "no-wait"    "skip rollout status wait (flag)"                                   ""

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
LOCAL_TAG="local"

# The bff-copresent sibling repo is expected one directory up (workspace layout).
BFF_REPO="${MEMQL_BFF_COPRESENT_REPO:-${REPO_ROOT}/../memql-bff-copresent}"

# Engine node types (built from this repo's Dockerfile)
ENGINE_NODES=(identity voice mcp)

# Carrier node types (built from memql-bff-copresent/Dockerfile)
CARRIER_NODES=(bff cognition agent planner workbench)

# All app node types
ALL_APP_NODES=("${ENGINE_NODES[@]}" "${CARRIER_NODES[@]}")

# Infra images (pull from upstream, import into k3d)
INFRA_IMAGES=(
    "timescale/timescaledb:2.19.1-pg16"
    "mcr.microsoft.com/azure-storage/azurite:3.34.0"
    "redis:7-alpine"
    "livekit/livekit-server:v1.8"
)

# Outcome tracking (result envelope + idempotency reporting).
REBUILT_COUNT=0
RESTARTED=false
INFRA_PULLED=false

#=============================================================================
# OUTPUT HELPERS -- delegate to the capability runtime (all logs to STDERR)
#=============================================================================

function info()  { cap_info  "$*"; }
function warn()  { cap_warn  "$*"; }
function error() { cap_error "$*"; }

function section() {
    {
        echo ""
        echo "------------------------------------------------------------"
        echo "  $*"
        echo "------------------------------------------------------------"
    } >&2
}

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    local missing=()

    for tool in docker k3d kubectl; do
        if ! command -v "$tool" &>/dev/null; then
            missing+=("$tool")
        fi
    done

    if [ ${#missing[@]} -gt 0 ]; then
        error "Missing required tools: ${missing[*]}"
        cap_fail 4 "missing required tools: ${missing[*]}"
    fi

    if ! k3d cluster list 2>/dev/null | grep -q "^${CLUSTER_NAME}[[:space:]]"; then
        error "Cluster '${CLUSTER_NAME}' is not running. Run 'make up' first."
        cap_fail 4 "cluster '${CLUSTER_NAME}' is not running"
    fi

    kubectl config use-context "k3d-${CLUSTER_NAME}" &>/dev/null || true
}

#=============================================================================
# NODE TYPE CLASSIFICATION
#=============================================================================

function is_engine_node() {
    local node="$1"
    for n in "${ENGINE_NODES[@]}"; do
        [[ "$n" == "$node" ]] && return 0
    done
    return 1
}

function is_carrier_node() {
    local node="$1"
    for n in "${CARRIER_NODES[@]}"; do
        [[ "$n" == "$node" ]] && return 0
    done
    return 1
}

function deployment_name_for_node() {
    # Map node type to k8s Deployment name (matches base/ manifest names).
    local node="$1"
    case "$node" in
        identity)   echo "identity" ;;
        voice)      echo "voice" ;;
        mcp)        echo "mcp" ;;
        bff)        echo "bff" ;;
        cognition)  echo "cognition" ;;
        agent)      echo "agent" ;;
        planner)    echo "planner" ;;
        workbench)  echo "workbench" ;;
        *)
            cap_fail 2 "unknown node type: $node"
            ;;
    esac
}

function image_name_for_node() {
    # Map node type to the in-cluster image ref the local overlay's pods
    # pull. These MUST match the `newName`s in
    # deploy/k8s/overlays/local/kustomization.yaml's `images:` block --
    # k3d imports under exactly this name so the kubelet resolves it
    # locally instead of trying to pull from a registry. The bff node's
    # image is named memql-bff-copresent (carrier); every other node is
    # memql-<node>.
    local node="$1"
    case "$node" in
        bff) echo "memql-bff-copresent:${LOCAL_TAG}" ;;
        *)   echo "memql-${node}:${LOCAL_TAG}" ;;
    esac
}

#=============================================================================
# BUILD ENGINE IMAGE (identity / voice / mcp)
#=============================================================================

function build_engine_node() {
    local node="$1"
    local image
    image="$(image_name_for_node "$node")"

    section "Building engine image: ${node} -> ${image}"

    # nodeType -> build-args mapping shared with the deploy.buildImage
    # capability backend (scripts/lib/engine_build_args.sh, memql#2379) so
    # the two local build paths cannot drift.
    engine_build_args_for_node "$node"
    if [[ "$node" == "voice" ]]; then
        warn "voice node requires libopus headers -- building from repo Dockerfile."
        warn "If the build fails with 'opus.h not found', see docs/public/build/build-tags.md."
    fi

    docker build \
        "${ENGINE_BUILD_ARGS[@]}" \
        --target "${ENGINE_BUILD_TARGET}" \
        --tag "${image}" \
        --file "${REPO_ROOT}/Dockerfile" \
        "${REPO_ROOT}" >&2

    info "Built ${image}."
}

#=============================================================================
# BUILD CARRIER IMAGE (bff / cognition / agent / planner / workbench)
#=============================================================================

function build_carrier_node() {
    local node="$1"
    local image
    image="$(image_name_for_node "$node")"

    section "Building carrier image: ${node} -> ${image}"

    if [ ! -d "${BFF_REPO}" ]; then
        error "memql-bff-copresent repo not found at ${BFF_REPO}."
        error "Carrier nodes (bff/cognition/agent/planner/workbench) require the"
        error "sibling checkout at the workspace level. Clone it first:"
        error "  git clone git@github.com:znasllc-io/memql-bff-copresent.git ${BFF_REPO}"
        error "Or set MEMQL_BFF_COPRESENT_REPO to its location."
        cap_fail 4 "memql-bff-copresent repo not found at ${BFF_REPO}"
    fi

    # The carrier Dockerfile expects the workspace root as the build context
    # so it can mount the memql + memql-bff-copresent trees at compile time.
    local workspace_root
    workspace_root="$(cd "${BFF_REPO}/.." && pwd)"

    docker build \
        --build-arg BUILD_TAGS="${node}" \
        --build-arg CGO_ENABLED=0 \
        --tag "${image}" \
        --file "${BFF_REPO}/Dockerfile" \
        "${workspace_root}" >&2

    info "Built ${image}."
}

#=============================================================================
# IMPORT IMAGE INTO K3D
#=============================================================================

function import_image() {
    local image="$1"

    info "Importing ${image} into k3d cluster '${CLUSTER_NAME}'..."
    k3d image import "${image}" --cluster "${CLUSTER_NAME}" >&2
    info "Imported ${image}."
}

#=============================================================================
# RESTART DEPLOYMENT
#=============================================================================

function restart_deployment() {
    local node="$1"
    local deployment
    deployment="$(deployment_name_for_node "$node")"

    info "Rolling restart of Deployment '${deployment}' in namespace '${NAMESPACE}'..."
    kubectl rollout restart deployment/"${deployment}" -n "${NAMESPACE}" >&2
    info "Restart initiated. Watch: kubectl rollout status deployment/${deployment} -n ${NAMESPACE}"
}

#=============================================================================
# PROCESS ONE NODE
#=============================================================================

function process_node() {
    local node="$1"

    if is_engine_node "$node"; then
        build_engine_node "$node"
        import_image "$(image_name_for_node "$node")"
        restart_deployment "$node"
    elif is_carrier_node "$node"; then
        build_carrier_node "$node"
        import_image "$(image_name_for_node "$node")"
        restart_deployment "$node"
    else
        error "Unknown node type: '${node}'. Valid values:"
        error "  Engine nodes:  ${ENGINE_NODES[*]}"
        error "  Carrier nodes: ${CARRIER_NODES[*]}"
        cap_fail 2 "unknown node type: ${node}"
    fi

    REBUILT_COUNT=$((REBUILT_COUNT + 1))
    RESTARTED=true
    cap_changed
}

#=============================================================================
# PULL + IMPORT INFRA IMAGES
#=============================================================================

function pull_and_import_infra() {
    section "Pulling and importing infra images"

    for image in "${INFRA_IMAGES[@]}"; do
        info "Pulling ${image}..."
        docker pull "${image}" >&2
        info "Importing ${image} into k3d..."
        k3d image import "${image}" --cluster "${CLUSTER_NAME}" >&2
        info "Done: ${image}"
    done

    INFRA_PULLED=true
    cap_changed
}

#=============================================================================
# WAIT FOR ROLLOUTS
#=============================================================================

function wait_for_rollouts() {
    local nodes=("$@")

    section "Waiting for rollouts to complete"

    for node in "${nodes[@]}"; do
        local deployment
        deployment="$(deployment_name_for_node "$node")"

        if kubectl get deployment "${deployment}" -n "${NAMESPACE}" &>/dev/null; then
            info "Waiting for ${deployment}..."
            kubectl rollout status deployment/"${deployment}" \
                -n "${NAMESPACE}" \
                --timeout=120s >&2 || warn "${deployment} rollout did not complete in 120s -- check 'kubectl get pods -n ${NAMESPACE}'"
        fi
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CLUSTER_NAME="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    NAMESPACE="$(cap_param namespace "${MEMQL_K3D_NAMESPACE:-memql}")"
    local nodes_arg pull_infra wait_flag
    nodes_arg="$(cap_param node "")"
    pull_infra="$(cap_flag pull-infra)"
    wait_flag="$(cap_flag no-wait)"

    cap_require cluster "$CLUSTER_NAME"
    cap_require namespace "$NAMESPACE"

    check_prerequisites

    # Resolve node list.
    local nodes_to_build=()
    if [ -n "${nodes_arg}" ]; then
        IFS=',' read -ra nodes_to_build <<< "${nodes_arg}"
    else
        nodes_to_build=("${ALL_APP_NODES[@]}")
    fi

    if [ -n "${pull_infra}" ]; then
        pull_and_import_infra
    fi

    if [ ${#nodes_to_build[@]} -gt 0 ]; then
        info "Nodes to build: ${nodes_to_build[*]}"
        for node in "${nodes_to_build[@]}"; do
            process_node "$node"
        done

        if [ -z "${wait_flag}" ]; then
            wait_for_rollouts "${nodes_to_build[@]}"
        fi
    fi

    section "Done"
    info "Cluster '${CLUSTER_NAME}' is running the latest local build."
    info "ArgoCD app status: kubectl get app memql-local -n argocd"
    info "Pod status:        kubectl get pods -n ${NAMESPACE}"

    cap_result_set     cluster     "$CLUSTER_NAME"
    cap_result_set     namespace   "$NAMESPACE"
    cap_result_set     nodes       "${nodes_to_build[*]}"
    cap_result_set_raw rebuilt     "$REBUILT_COUNT"
    cap_result_set_raw restarted   "$RESTARTED"
    cap_result_set_raw infraPulled "$INFRA_PULLED"
    cap_ok
}

main "$@"
