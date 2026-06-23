#!/usr/bin/env bash
#
# scripts/k3d/dev.sh
# ==================
#
# Inner-loop dev command: build one or more node images locally,
# import them into the k3d cluster, and restart the relevant Deployments.
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
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -euo pipefail.
#
# Refs: #2066 #2061

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
LOCAL_TAG="local"

# The bff-copresent sibling repo is expected one directory up (workspace layout).
BFF_REPO="${MEMQL_BFF_COPRESENT_REPO:-${REPO_ROOT}/../memql-bff-copresent}"

# ACR image base (carrier images reference the carrier repo image base).
# For local builds we override to 'memql' as the image name prefix.
ENGINE_IMAGE_BASE="${MEMQL_ENGINE_IMAGE_BASE:-acrmemql.azurecr.io/memql}"
CARRIER_IMAGE_BASE="${MEMQL_CARRIER_IMAGE_BASE:-acrmemql.azurecr.io/memql-carrier}"

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

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { echo "INFO:  $*"; }
function warn()  { echo "WARN:  $*"; }
function error() { echo "ERROR: $*" >&2; }

function section() {
    echo ""
    echo "------------------------------------------------------------"
    echo "  $*"
    echo "------------------------------------------------------------"
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
        exit 1
    fi

    if ! k3d cluster list 2>/dev/null | grep -q "^${CLUSTER_NAME}[[:space:]]"; then
        error "Cluster '${CLUSTER_NAME}' is not running. Run 'make up' first."
        exit 1
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
            error "Unknown node type: $node"
            exit 1
            ;;
    esac
}

#=============================================================================
# BUILD ENGINE IMAGE (identity / voice / mcp)
#=============================================================================

function build_engine_node() {
    local node="$1"
    local image="${ENGINE_IMAGE_BASE}-${node}:${LOCAL_TAG}"

    section "Building engine image: ${node} -> ${image}"

    # voice requires CGO for LibOpus; other engine nodes are CGO-free.
    local build_args=()
    if [[ "$node" == "voice" ]]; then
        build_args+=(--build-arg CGO_ENABLED=1)
        warn "voice node requires libopus headers -- building from repo Dockerfile."
        warn "If the build fails with 'opus.h not found', see docs/public/build/build-tags.md."
    fi

    docker build \
        "${build_args[@]}" \
        --target "${node}-runtime" \
        --tag "${image}" \
        --file "${REPO_ROOT}/Dockerfile" \
        "${REPO_ROOT}"

    info "Built ${image}."
}

#=============================================================================
# BUILD CARRIER IMAGE (bff / cognition / agent / planner / workbench)
#=============================================================================

function build_carrier_node() {
    local node="$1"
    local image="${CARRIER_IMAGE_BASE}-${node}:${LOCAL_TAG}"

    section "Building carrier image: ${node} -> ${image}"

    if [ ! -d "${BFF_REPO}" ]; then
        error "memql-bff-copresent repo not found at ${BFF_REPO}."
        error "Carrier nodes (bff/cognition/agent/planner/workbench) require the"
        error "sibling checkout at the workspace level. Clone it first:"
        error "  git clone git@github.com:znasllc-io/memql-bff-copresent.git ${BFF_REPO}"
        error "Or set MEMQL_BFF_COPRESENT_REPO to its location."
        exit 1
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
        "${workspace_root}"

    info "Built ${image}."
}

#=============================================================================
# IMPORT IMAGE INTO K3D
#=============================================================================

function import_image() {
    local image="$1"

    info "Importing ${image} into k3d cluster '${CLUSTER_NAME}'..."
    k3d image import "${image}" --cluster "${CLUSTER_NAME}"
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
    kubectl rollout restart deployment/"${deployment}" -n "${NAMESPACE}"
    info "Restart initiated. Watch: kubectl rollout status deployment/${deployment} -n ${NAMESPACE}"
}

#=============================================================================
# PROCESS ONE NODE
#=============================================================================

function process_node() {
    local node="$1"

    if is_engine_node "$node"; then
        build_engine_node "$node"
        import_image "${ENGINE_IMAGE_BASE}-${node}:${LOCAL_TAG}"
        restart_deployment "$node"
    elif is_carrier_node "$node"; then
        build_carrier_node "$node"
        import_image "${CARRIER_IMAGE_BASE}-${node}:${LOCAL_TAG}"
        restart_deployment "$node"
    else
        error "Unknown node type: '${node}'. Valid values:"
        error "  Engine nodes:  ${ENGINE_NODES[*]}"
        error "  Carrier nodes: ${CARRIER_NODES[*]}"
        exit 1
    fi
}

#=============================================================================
# PULL + IMPORT INFRA IMAGES
#=============================================================================

function pull_and_import_infra() {
    section "Pulling and importing infra images"

    for image in "${INFRA_IMAGES[@]}"; do
        info "Pulling ${image}..."
        docker pull "${image}"
        info "Importing ${image} into k3d..."
        k3d image import "${image}" --cluster "${CLUSTER_NAME}"
        info "Done: ${image}"
    done
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
                --timeout=120s || warn "${deployment} rollout did not complete in 120s -- check 'kubectl get pods -n ${NAMESPACE}'"
        fi
    done
}

#=============================================================================
# PARSE ARGUMENTS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

Rebuild node image(s), import into k3d, and restart Deployments.

Options:
    --node=TYPE[,TYPE,...]  Node type(s) to rebuild (default: all app nodes)
                            Engine: ${ENGINE_NODES[*]}
                            Carrier: ${CARRIER_NODES[*]}
    --pull-infra            Pull + import infra images (postgres, azurite, redis, livekit)
    --cluster=NAME          k3d cluster name (default: ${CLUSTER_NAME})
    --namespace=NS          k8s namespace (default: ${NAMESPACE})
    --no-wait               Skip rollout status wait
    --help                  Show this help message

Environment overrides:
    MEMQL_K3D_CLUSTER            cluster name
    MEMQL_K3D_NAMESPACE          k8s namespace
    MEMQL_BFF_COPRESENT_REPO     path to memql-bff-copresent checkout
    MEMQL_ENGINE_IMAGE_BASE      engine image prefix (default: acrmemql.azurecr.io/memql)
    MEMQL_CARRIER_IMAGE_BASE     carrier image prefix (default: acrmemql.azurecr.io/memql-carrier)

Examples:
    $0                            # rebuild + restart all app nodes
    $0 --node=bff                 # rebuild + restart bff only
    $0 --node=bff,cognition       # rebuild + restart bff + cognition
    $0 --pull-infra               # re-import infra images (postgres/azurite/redis/livekit)
EOF
}

NODES_ARG=""
PULL_INFRA=false
WAIT=true

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --node=*)       NODES_ARG="${1#*=}";    shift ;;
            --pull-infra)   PULL_INFRA=true;        shift ;;
            --cluster=*)    CLUSTER_NAME="${1#*=}"; shift ;;
            --namespace=*)  NAMESPACE="${1#*=}";    shift ;;
            --no-wait)      WAIT=false;             shift ;;
            --help)         show_help; exit 0 ;;
            *) error "Unknown option: $1"; show_help; exit 2 ;;
        esac
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"

    check_prerequisites

    # Resolve node list.
    local nodes_to_build=()
    if [ -n "${NODES_ARG}" ]; then
        IFS=',' read -ra nodes_to_build <<< "${NODES_ARG}"
    else
        nodes_to_build=("${ALL_APP_NODES[@]}")
    fi

    if [ "${PULL_INFRA}" = true ]; then
        pull_and_import_infra
    fi

    if [ ${#nodes_to_build[@]} -gt 0 ]; then
        info "Nodes to build: ${nodes_to_build[*]}"
        for node in "${nodes_to_build[@]}"; do
            process_node "$node"
        done

        if [ "${WAIT}" = true ]; then
            wait_for_rollouts "${nodes_to_build[@]}"
        fi
    fi

    section "Done"
    info "Cluster '${CLUSTER_NAME}' is running the latest local build."
    info "ArgoCD app status: kubectl get app memql-local -n argocd"
    info "Pod status:        kubectl get pods -n ${NAMESPACE}"
}

main "$@"
