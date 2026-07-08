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
# Node images and the carrier override
# ------------------------------------
# By default every app node type builds from THIS repo's Dockerfile
# (--build-arg BUILD_TAGS=<type>) and is tagged memql-<type>:local:
#     identity  voice  mcp  cognition  agent  planner  workbench
#
# A downstream product repo that ships its own DSL/integrations on top of
# the engine (a "carrier" image) reuses this script and overrides a subset
# of node types via the carrier hook:
#     --carrier-repo=PATH      the carrier repo (its Dockerfile is used)
#     --carrier-nodes=a,b,c    node types built from the carrier Dockerfile
#     --carrier-context=PATH   docker build context (default: the carrier
#                              repo's parent directory, i.e. the workspace
#                              root, so the carrier Dockerfile can mount
#                              both source trees at compile time)
# Carrier builds pass --build-arg BUILD_TAGS=<type> and tag the image
# memql-<type>:local -- the SAME name the overlay pins -- so overriding a
# node is transparent to the manifests. The engine repo itself never sets
# these; they exist for downstream repos' Makefiles.
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
#   make dev NODE=cognition           # rebuild + restart one node
#   make dev NODE=mcp,cognition       # comma-separated list
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
cap_spec_param "node"            "node type(s) to rebuild, comma-separated (default: all app nodes)" ""
cap_spec_param "pull-infra"      "pull + import infra images (flag)"                                 ""
cap_spec_param "cluster"         "k3d cluster name"
cap_spec_param "namespace"       "k8s namespace"
cap_spec_param "no-wait"         "skip rollout status wait (flag)"                                   ""
cap_spec_param "carrier-repo"    "downstream carrier repo whose Dockerfile builds the carrier nodes" ""
cap_spec_param "carrier-nodes"   "node types built from the carrier Dockerfile, comma-separated"     ""
cap_spec_param "carrier-context" "docker build context for carrier builds (default: carrier repo's parent)" ""

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
LOCAL_TAG="local"

# App node types buildable from this repo's Dockerfile. The default `make dev`
# set matches the Deployments in deploy/k8s/overlays/local.
DEFAULT_APP_NODES=(identity bff voice mcp cognition agent planner workbench)

# Every node type this script can address (superset: node types a downstream
# carrier overlay may add, e.g. bff, are valid targets too).
VALID_NODES=(identity voice mcp bff cognition agent planner workbench)

# Carrier override (resolved from params in main; empty = engine-only).
CARRIER_REPO=""
CARRIER_CONTEXT=""
CARRIER_NODES=()

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

function is_valid_node() {
    local node="$1"
    for n in "${VALID_NODES[@]}"; do
        [[ "$n" == "$node" ]] && return 0
    done
    return 1
}

function is_carrier_node() {
    local node="$1"
    for n in "${CARRIER_NODES[@]+"${CARRIER_NODES[@]}"}"; do
        [[ "$n" == "$node" ]] && return 0
    done
    return 1
}

function deployment_name_for_node() {
    # Node type == k8s Deployment name (matches base/ manifest names).
    local node="$1"
    if ! is_valid_node "$node"; then
        cap_fail 2 "unknown node type: $node"
    fi
    echo "$node"
}

function image_name_for_node() {
    # Map node type to the in-cluster image ref the local overlay's pods
    # pull. These MUST match the `newName`s in the local overlay's
    # `images:` block -- k3d imports under exactly this name so the
    # kubelet resolves it locally instead of trying to pull from a
    # registry. Uniform for engine and carrier builds, so a carrier
    # override is transparent to the manifests.
    local node="$1"
    echo "memql-${node}:${LOCAL_TAG}"
}

#=============================================================================
# BUILD ENGINE IMAGE (from this repo's Dockerfile)
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
# BUILD CARRIER IMAGE (from the downstream carrier repo's Dockerfile)
#=============================================================================

function build_carrier_node() {
    local node="$1"
    local image
    image="$(image_name_for_node "$node")"

    section "Building carrier image: ${node} -> ${image}"

    if [ ! -d "${CARRIER_REPO}" ]; then
        error "Carrier repo not found at ${CARRIER_REPO}."
        error "Carrier node builds (--carrier-nodes=${CARRIER_NODES[*]}) need the"
        error "carrier repo checked out. Pass --carrier-repo=<path> or set"
        error "MEMQL_CARRIER_REPO to its location."
        cap_fail 4 "carrier repo not found at ${CARRIER_REPO}"
    fi

    docker build \
        --build-arg BUILD_TAGS="${node}" \
        --build-arg CGO_ENABLED=0 \
        --tag "${image}" \
        --file "${CARRIER_REPO}/Dockerfile" \
        "${CARRIER_CONTEXT}" >&2

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

    if ! kubectl get deployment "${deployment}" -n "${NAMESPACE}" &>/dev/null; then
        info "Deployment '${deployment}' not present in namespace '${NAMESPACE}' -- image imported; skipping restart."
        return 0
    fi

    info "Rolling restart of Deployment '${deployment}' in namespace '${NAMESPACE}'..."
    kubectl rollout restart deployment/"${deployment}" -n "${NAMESPACE}" >&2
    info "Restart initiated. Watch: kubectl rollout status deployment/${deployment} -n ${NAMESPACE}"
}

#=============================================================================
# PROCESS ONE NODE
#=============================================================================

function process_node() {
    local node="$1"

    if ! is_valid_node "$node"; then
        error "Unknown node type: '${node}'. Valid values: ${VALID_NODES[*]}"
        cap_fail 2 "unknown node type: ${node}"
    fi

    if is_carrier_node "$node"; then
        build_carrier_node "$node"
    else
        build_engine_node "$node"
    fi
    import_image "$(image_name_for_node "$node")"
    restart_deployment "$node"

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
    local nodes_arg pull_infra wait_flag carrier_nodes_arg
    nodes_arg="$(cap_param node "")"
    pull_infra="$(cap_flag pull-infra)"
    wait_flag="$(cap_flag no-wait)"
    CARRIER_REPO="$(cap_param carrier-repo "${MEMQL_CARRIER_REPO:-}")"
    carrier_nodes_arg="$(cap_param carrier-nodes "${MEMQL_CARRIER_NODES:-}")"
    CARRIER_CONTEXT="$(cap_param carrier-context "${MEMQL_CARRIER_CONTEXT:-}")"

    cap_require cluster "$CLUSTER_NAME"
    cap_require namespace "$NAMESPACE"

    if [ -n "${carrier_nodes_arg}" ]; then
        IFS=',' read -ra CARRIER_NODES <<< "${carrier_nodes_arg}"
        if [ -z "${CARRIER_REPO}" ]; then
            cap_fail 2 "--carrier-nodes requires --carrier-repo (or MEMQL_CARRIER_REPO)"
        fi
        # Validate the carrier repo up front (before the default-context cd
        # below, which would otherwise abort raw under set -e) so a typo'd or
        # not-yet-cloned path fails honestly as a missing prerequisite.
        if [ ! -d "${CARRIER_REPO}" ]; then
            error "Carrier repo not found at ${CARRIER_REPO}."
            error "Pass --carrier-repo=<path> or set MEMQL_CARRIER_REPO to its location."
            cap_fail 4 "carrier repo not found at ${CARRIER_REPO}"
        fi
        # Default carrier build context: the carrier repo's parent (workspace
        # root), so its Dockerfile can mount both source trees.
        if [ -z "${CARRIER_CONTEXT}" ]; then
            CARRIER_CONTEXT="$(cd "${CARRIER_REPO}/.." && pwd)"
        fi
    fi

    check_prerequisites

    # Resolve node list: explicit --node wins; otherwise all default app
    # nodes plus any carrier-only node types the override adds (e.g. bff).
    local nodes_to_build=()
    if [ -n "${nodes_arg}" ]; then
        IFS=',' read -ra nodes_to_build <<< "${nodes_arg}"
    else
        nodes_to_build=("${DEFAULT_APP_NODES[@]}")
        for cn in "${CARRIER_NODES[@]+"${CARRIER_NODES[@]}"}"; do
            local seen=false
            for n in "${nodes_to_build[@]}"; do
                [[ "$n" == "$cn" ]] && seen=true && break
            done
            [[ "$seen" == false ]] && nodes_to_build+=("$cn")
        done
    fi

    if [ -n "${pull_infra}" ]; then
        pull_and_import_infra
    fi

    if [ ${#nodes_to_build[@]} -gt 0 ]; then
        info "Nodes to build: ${nodes_to_build[*]}"
        if [ ${#CARRIER_NODES[@]} -gt 0 ]; then
            info "Carrier override: ${CARRIER_NODES[*]} (from ${CARRIER_REPO})"
        fi
        for node in "${nodes_to_build[@]}"; do
            process_node "$node"
        done

        if [ -z "${wait_flag}" ]; then
            wait_for_rollouts "${nodes_to_build[@]}"
        fi
    fi

    section "Done"
    info "Cluster '${CLUSTER_NAME}' is running the latest local build."
    info "Pod status: kubectl get pods -n ${NAMESPACE}"

    cap_result_set     cluster     "$CLUSTER_NAME"
    cap_result_set     namespace   "$NAMESPACE"
    cap_result_set     nodes       "${nodes_to_build[*]}"
    cap_result_set_raw rebuilt     "$REBUILT_COUNT"
    cap_result_set_raw restarted   "$RESTARTED"
    cap_result_set_raw infraPulled "$INFRA_PULLED"
    cap_ok
}

main "$@"
