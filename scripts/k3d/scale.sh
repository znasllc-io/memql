#!/usr/bin/env bash
#
# scripts/k3d/scale.sh
# ====================
#
# Capability: k3d.scale -- scale all memQL app Deployments in ONE environment to
# a target replica count.
#
# Why not use ArgoCD for this?
# ----------------------------
# Replica count is a runtime dimension as well as a committed value. Every
# overlay pins the count a fresh reconcile establishes; scaling away from it --
# up to 2 for cross-node mesh testing, down to 0 to park staging -- is a runtime
# override, not a manifest change. Each environment's ArgoCD Application
# excludes /spec/replicas from drift detection
# (deploy/argocd/apps/memql-{prod,staging}.yaml), so selfHeal will NOT revert
# what this writes and no manual repair follows a scale-to-zero.
#
# The environment names the namespace (epic memql#3748 / #3766)
# -------------------------------------------------------------
# One cluster now carries two environments in two namespaces, so "which
# Deployments" is no longer answerable without saying which environment:
#
#     local    -> memql             the default; the k3d cluster `make up` brings up
#     staging  -> memql-staging     `make scale N=0 ENV=staging` parks it
#     prod     -> memql-prod        `make scale N=2 ENV=prod`
#
# --env DEFAULTS TO local -- see the CONFIGURATION block below for why the
# direction of that default is the safety property rather than a convenience.
# Naming a REMOTE environment is explicit (`ENV=staging` / `ENV=prod`), and an
# environment absent from the current kubectl context fails with exit 4 naming
# both the environment and the namespace, so the two can never be confused
# silently.
#
# --namespace still overrides, for a cluster whose namespaces are named
# something else. MEMQL_K3D_NAMESPACE is honoured for `local` ONLY, because it
# is the knob `make up` itself uses to decide what to create -- letting it reach
# the cloud environments would mean an exported shell variable could redirect a
# production scale, which is exactly the silent confusion above.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   make scale N=2                          # 2 replicas per Deployment in prod
#   make scale N=1 ENV=staging              # bring staging back up
#   make scale N=0 ENV=staging              # park staging (costs storage only)
#   make scale N=2 ENV=local                # local cross-node mesh testing
#   scripts/k3d/scale.sh --replicas=2 --env=staging
#   scripts/k3d/scale.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (kubectl/ns absent)
#
# Refs: #2067 #2061 #2221 #3766

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.scale" "Scale all memQL app Deployments in one environment to a target replica count."
cap_spec_param "replicas"  "replica count per Deployment (0 parks the environment)"
cap_spec_param "env"       "environment to scale: local | staging | prod (default: local)"
cap_spec_param "cluster"   "k3d cluster name"
cap_spec_param "namespace" "k8s namespace; overrides the one --env resolves to"
#=============================================================================
# CONFIGURATION
#=============================================================================

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"

# DEFAULTS TO local, and the direction of that default is the safety property.
# `make scale N=2` is the inner-loop command a developer types from memory
# (CLAUDE.md documents it for cross-node mesh testing), so the habit is already
# formed -- and a default of `prod` would silently point that habit at
# production. Naming a remote environment is one word; mistyping nothing and
# hitting prod is unrecoverable.
#
# memql#3766 originally specified `prod` as the default. Reversed on the repo
# owner's call while that issue was landing, for the reason above.
ENVIRONMENT="local"
NAMESPACE=""

# All Deployments the mesh runs, in every environment. Excludes postgres +
# azurite, which are local-only infrastructure and not part of the mesh.
APP_DEPLOYMENTS=(
    identity
    voice
    mcp
    bff
    cognition
    agent
    planner
    workbench
    voice-agent
    edge
)

SCALED_COUNT=0

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { cap_info "$*"; }
function warn()  { cap_warn "$*"; }
function error() { cap_error "$*"; }

#=============================================================================
# ENVIRONMENT -> NAMESPACE
#=============================================================================

# namespace_for_env prints the namespace an environment lives in, or nothing
# when the name is not one. A lookup table, not a decision: the capability
# contract forbids a script branching on environment to choose BEHAVIOUR, and
# this chooses no behaviour -- every environment gets the identical `kubectl
# scale` over the identical Deployment list.
function namespace_for_env() {
    case "$1" in
        prod)    printf 'memql-prod' ;;
        staging) printf 'memql-staging' ;;
        # The local k3d cluster keeps the plain `memql` namespace: it is ONE
        # environment and this epic deliberately did not grow it a second one.
        # MEMQL_K3D_NAMESPACE is what `make up` reads, so `--env=local` has to
        # agree with it or a customised local cluster would be unreachable here.
        local)   printf '%s' "${MEMQL_K3D_NAMESPACE:-memql}" ;;
        *)       printf '' ;;
    esac
}

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is required but not installed."
    fi
    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        cap_fail 4 "Environment '${ENVIRONMENT}' lives in namespace '${NAMESPACE}', which does not exist on the current kubectl context. Locally that namespace is created by 'make up' (and is ENV=local); on the cluster it is created by the ArgoCD Application for that environment."
    fi
}

#=============================================================================
# SCALE DEPLOYMENTS
#=============================================================================

function scale_all() {
    local n="$1"

    info "Scaling all app Deployments to replicas=${n} in environment '${ENVIRONMENT}' (namespace '${NAMESPACE}')..."

    for deployment in "${APP_DEPLOYMENTS[@]}"; do
        if kubectl get deployment "${deployment}" -n "${NAMESPACE}" &>/dev/null; then
            kubectl scale deployment "${deployment}" \
                --replicas="${n}" \
                -n "${NAMESPACE}" >&2
            info "  ${deployment}: scaled to ${n}"
            SCALED_COUNT=$((SCALED_COUNT + 1))
        else
            warn "  ${deployment}: not found (may not be deployed yet)"
        fi
    done

    info "Done. Current pod state:"
    kubectl get pods -n "${NAMESPACE}" \
        --sort-by=.metadata.name \
        -o wide 2>/dev/null | head -30 >&2 || true

    if [ "${n}" -eq 0 ]; then
        info "Environment '${ENVIRONMENT}' is parked -- it costs storage, not compute."
        info "Bring it back with 'make scale N=1 ENV=${ENVIRONMENT}'. ArgoCD ignores /spec/replicas, so nothing scales it back on its own."
    fi
    if [ "${n}" -gt 1 ]; then
        info "Multi-node mode active (replicas=${n})."
        info "Run 'make status' to verify each pod has a UNIQUE MEMQL_NODE_ID."
        info "Unique ids are required for cross-node mesh routing to work (#1388 class)."
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local replicas resolved_ns
    replicas="$(cap_param replicas "${N:-}")"
    ENVIRONMENT="$(cap_param env "$ENVIRONMENT")"
    CLUSTER_NAME="$(cap_param cluster "$CLUSTER_NAME")"

    resolved_ns="$(namespace_for_env "$ENVIRONMENT")"
    if [[ -z "$resolved_ns" ]]; then
        cap_fail 2 "Unknown environment: '${ENVIRONMENT}'. Valid values are prod, staging, local."
    fi
    # --namespace wins, for a cluster whose namespaces are named differently.
    NAMESPACE="$(cap_param namespace "$resolved_ns")"

    cap_require replicas "$replicas"
    # ZERO IS A VALID COUNT and used to be rejected here. Parking staging at 0
    # when it is idle is what makes a second environment cost storage rather
    # than compute (design memql#3748 §3.5), so the pattern admits it.
    if ! [[ "${replicas}" =~ ^(0|[1-9][0-9]*)$ ]]; then
        cap_fail 2 "Replica count must be a non-negative integer, got: '${replicas}'"
    fi

    check_prerequisites
    scale_all "${replicas}"

    cap_changed
    cap_result_set_raw replicas "$replicas"
    cap_result_set     environment "$ENVIRONMENT"
    cap_result_set     namespace "$NAMESPACE"
    cap_result_set_raw deploymentsScaled "$SCALED_COUNT"
    cap_ok
}

main "$@"
