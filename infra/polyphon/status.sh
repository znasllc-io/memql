#!/bin/bash
set -e

# status.sh — Check Polyphon GKE deployment status
#
# Usage:
#   ./infra/polyphon/status.sh

#=============================================================================
# CONFIGURATION
#=============================================================================

PROJECT_ID="fast-fire-486523-f3"
REGION="us-central1"
CLUSTER_NAME="polyphon"

#=============================================================================
# FUNCTIONS
#=============================================================================

function get_credentials() {
    gcloud config set project "$PROJECT_ID" --quiet 2>/dev/null
    gcloud container clusters get-credentials "$CLUSTER_NAME" --region="$REGION" 2>/dev/null
}

function main() {
    echo "========================================="
    echo "  Polyphon Infrastructure Status"
    echo "========================================="
    echo ""

    get_credentials

    # Cluster info
    echo "[CLUSTER]"
    echo "  Name:    $CLUSTER_NAME"
    echo "  Region:  $REGION"
    echo "  Project: $PROJECT_ID"
    echo ""

    # Node pools
    echo "[NODE POOLS]"
    gcloud container node-pools list \
        --cluster="$CLUSTER_NAME" \
        --region="$REGION" \
        --format="table(name, config.machineType, autoscaling.minNodeCount, autoscaling.maxNodeCount, status)" \
        2>/dev/null || echo "  Unable to fetch node pools"
    echo ""

    # Nodes
    echo "[NODES]"
    kubectl get nodes -o wide --show-labels 2>/dev/null | grep -E "polyphon|NAME" || echo "  No nodes found"
    echo ""

    # Pods
    echo "[PODS]"
    kubectl -n polyphon get pods -o wide 2>/dev/null || echo "  No pods found"
    echo ""

    # Services
    echo "[SERVICES]"
    kubectl -n polyphon get services 2>/dev/null || echo "  No services found"
    echo ""

    # Deployments
    echo "[DEPLOYMENTS]"
    kubectl -n polyphon get deployments 2>/dev/null || echo "  No deployments found"
    echo ""

    echo "========================================="
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
