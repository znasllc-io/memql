#!/usr/bin/env bash
#
# scripts/deploy/aks-apply.sh
# ===========================
#
# Apply the memQL AKS Kubernetes manifests (deploy/k8s/) to the current
# kubectl context. Part of the ACA -> AKS pivot (epic
# znasllc-io/memql#522): ACA exposes one ingress port per app and cannot
# host the per-node multi-port mesh, so the backend cluster runs on AKS.
#
# What it does (idempotent -- safe to re-run any number of times):
#
#   1. Pre-flight: kubectl present, a cluster is reachable, the
#      deploy/k8s/ kustomization renders cleanly.
#   2. Apply the `memql` namespace.
#   3. Warn (non-fatal) if the `memql-secrets` Secret is absent -- it is
#      a one-time prerequisite created out-of-band with REAL values
#      (see deploy/k8s/secret.example.yaml). The manifests reference it
#      via envFrom/secretKeyRef; pods will not start ready without it.
#   4. kustomize-apply the namespace + 7 node Deployments + Services
#      (identity / bff / cognition / voice / agent / planner / workbench).
#
# NO database is deployed: staging/prod use the managed Tiger Cloud DB
# (xahn9ru4v6) via the MEMQL_DATABASE_DSN key in memql-secrets.
#
# Migrations run ONCE: only the identity Deployment carries
# MIGRATE_ON_START=true; the other six are false (no 7-way
# race against the shared Tiger DB).
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure -- one function per responsibility, main() at
# the bottom calls them in order. Supports --help and a --dry-run that
# runs a server-side dry-run apply and mutates nothing.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Directory holding the manifests + kustomization, resolved relative to
# this script so the target works from any cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
K8S_DIR="$REPO_ROOT/deploy/k8s"

NAMESPACE="memql"
SECRET_NAME="memql-secrets"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Apply the memQL AKS manifests (deploy/k8s/) to the current kubectl context.

Options:
    --env=ENV     Target environment label (staging|production). Used for
                  log context only; the target cluster is whatever the
                  current kubectl context points at. Default: staging.
    --dry-run     Server-side dry-run apply -- prints what WOULD change
                  and mutates nothing.
    --help        Show this help message.

Prerequisite (one-time, out-of-band -- REAL values, never committed):
    kubectl create secret generic $SECRET_NAME -n $NAMESPACE \\
      --from-literal=MEMQL_MASTER_KEY="\$MEMQL_MASTER_KEY" \\
      --from-literal=MEMQL_GENESIS_B64="\$(base64 < ~/.memql/genesis.znas)" \\
      --from-literal=MEMQL_DATABASE_DSN="\$(tiger db connection-string xahn9ru4v6 --with-password)"

Examples:
    $0 --env=staging
    $0 --env=staging --dry-run
EOF
}

function parse_arguments() {
    ENV="staging"
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            --env=*)   ENV="${1#*=}"; shift ;;
            --dry-run) DRY_RUN=true; shift ;;
            --help)    show_help; exit 0 ;;
            *)
                echo "ERROR: Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
    done
}

function check_prerequisites() {
    if ! command -v kubectl &> /dev/null; then
        echo "ERROR: kubectl is not installed"
        exit 1
    fi
    if [ ! -f "$K8S_DIR/overlays/$ENV/kustomization.yaml" ]; then
        echo "ERROR: no overlay at $K8S_DIR/overlays/$ENV (deployment-v2 Phase 1 #699)"
        exit 1
    fi
    if ! kubectl cluster-info &> /dev/null; then
        echo "ERROR: no reachable Kubernetes cluster in the current context."
        echo "       Set your context (e.g. az aks get-credentials -g rg-memql-$ENV -n <cluster>)"
        exit 1
    fi
}

function validate_manifests() {
    echo "INFO: rendering digest-pinned overlay deploy/k8s/overlays/$ENV..."
    if ! kubectl kustomize "$K8S_DIR/overlays/$ENV" > /dev/null; then
        echo "ERROR: kustomize render failed"
        exit 1
    fi
}

function warn_if_secret_missing() {
    if ! kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" &> /dev/null; then
        echo "WARNING: Secret '$SECRET_NAME' not found in namespace '$NAMESPACE'."
        echo "         Pods reference it via envFrom/secretKeyRef and will not"
        echo "         become ready until it exists. Create it first (see --help)."
    fi
}

function apply_namespace() {
    local dry_flag=""
    [ "$DRY_RUN" = true ] && dry_flag="--dry-run=server"
    echo "INFO: applying namespace '$NAMESPACE'..."
    kubectl apply -f "$K8S_DIR/base/namespace.yaml" $dry_flag
}

function apply_manifests() {
    local dry_flag=""
    [ "$DRY_RUN" = true ] && dry_flag="--dry-run=server"
    echo "INFO: applying node Deployments + Services (digest-pinned overlay)..."
    kubectl apply -k "$K8S_DIR/overlays/$ENV" $dry_flag
}

function execute() {
    echo "========================================="
    echo "memQL AKS apply"
    echo "  Env:        $ENV"
    echo "  Context:    $(kubectl config current-context 2>/dev/null || echo unknown)"
    echo "  Namespace:  $NAMESPACE"
    echo "  Dry run:    $DRY_RUN"
    echo "========================================="

    apply_namespace
    warn_if_secret_missing
    apply_manifests

    echo "INFO: done."
    if [ "$DRY_RUN" = false ]; then
        echo "INFO: watch rollout with: kubectl get pods -n $NAMESPACE -w"
    fi
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    validate_manifests
    execute
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
