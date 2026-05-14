#!/bin/bash
set -e

# teardown.sh — Remove Polyphon GKE deployment
#
# Usage:
#   ./infra/polyphon/teardown.sh [--delete-cluster] [--dry-run]
#
# By default, only removes K8s resources (pods, services, deployments).
# Use --delete-cluster to also delete the entire GKE cluster.

#=============================================================================
# CONFIGURATION
#=============================================================================

PROJECT_ID="fast-fire-486523-f3"
REGION="us-central1"
CLUSTER_NAME="polyphon"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
K8S_DIR="$SCRIPT_DIR/k8s"

DELETE_CLUSTER=false
DRY_RUN=false

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Removes Polyphon services from GKE.

Options:
    --delete-cluster  Also delete the entire GKE cluster (destructive)
    --dry-run         Show what would happen without executing
    --help            Show this help message
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --delete-cluster)
                DELETE_CLUSTER=true
                shift
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            --help)
                show_help
                exit 0
                ;;
            *)
                echo "ERROR: Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
    done
}

function get_credentials() {
    gcloud config set project "$PROJECT_ID" --quiet
    gcloud container clusters get-credentials "$CLUSTER_NAME" --region="$REGION" 2>/dev/null || true
}

function delete_resources() {
    echo "[INFO] Removing Polyphon K8s resources..."

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] kubectl delete -f $K8S_DIR/ --recursive --ignore-not-found"
        return
    fi

    # Delete all resources.
    kubectl delete -f "$K8S_DIR/" --recursive --ignore-not-found 2>/dev/null || true

    # Delete namespace.
    kubectl delete namespace polyphon --ignore-not-found 2>/dev/null || true

    echo "[OK] K8s resources removed"
}

function delete_cluster() {
    echo ""
    echo "[WARNING] Deleting GKE cluster: $CLUSTER_NAME"
    echo "[WARNING] This is IRREVERSIBLE."
    echo ""

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] gcloud container clusters delete $CLUSTER_NAME --region=$REGION --quiet"
        return
    fi

    gcloud container clusters delete "$CLUSTER_NAME" --region="$REGION" --quiet

    echo "[OK] Cluster deleted"
}

function main() {
    parse_arguments "$@"

    echo "========================================="
    echo "  Polyphon Teardown"
    echo "========================================="
    echo ""

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN MODE] No changes will be made"
        echo ""
    fi

    get_credentials
    delete_resources

    if [ "$DELETE_CLUSTER" = true ]; then
        delete_cluster
    else
        echo ""
        echo "[INFO] Cluster '$CLUSTER_NAME' preserved."
        echo "[INFO] To delete the cluster: $0 --delete-cluster"
    fi

    echo ""
    echo "[OK] Teardown complete."
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
