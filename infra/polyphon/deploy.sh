#!/bin/bash
set -e

# deploy.sh — Deploy Polyphon services to GKE
#
# Applies all Kubernetes manifests to the Polyphon cluster. Stage 1
# deploys only CPU services (LiveKit, Redis, Bridge Agent); ASR/TTS
# runs against a cloud API (OpenAI), so no GPU manifests get applied.
#
# Usage:
#   ./infra/polyphon/deploy.sh [--dry-run]

#=============================================================================
# CONFIGURATION
#=============================================================================

PROJECT_ID="fast-fire-486523-f3"
REGION="us-central1"
CLUSTER_NAME="polyphon"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
K8S_DIR="$SCRIPT_DIR/k8s"

DRY_RUN=false

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Deploys Polyphon services to the GKE cluster.

Options:
    --dry-run     Show what would be applied without executing
    --help        Show this help message

Services deployed:
    CPU (always-on):  LiveKit, Redis, Bridge Agent
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case $1 in
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
    echo "[INFO] Getting cluster credentials..."
    gcloud config set project "$PROJECT_ID" --quiet
    gcloud container clusters get-credentials "$CLUSTER_NAME" --region="$REGION"
}

function apply_manifests() {
    local dir=$1
    local name=$2
    echo "[INFO] Deploying $name..."

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] kubectl apply -f $dir/"
        return
    fi

    kubectl apply -f "$dir/"
}

function main() {
    parse_arguments "$@"

    echo "========================================="
    echo "  Polyphon GKE Deployment"
    echo "========================================="
    echo ""

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN MODE] No changes will be made"
        echo ""
    fi

    get_credentials

    # Apply namespace first.
    echo "[INFO] Creating namespace..."
    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] kubectl apply -f $K8S_DIR/namespace.yaml"
    else
        kubectl apply -f "$K8S_DIR/namespace.yaml"
    fi

    # Deploy CPU services (always-on).
    apply_manifests "$K8S_DIR/redis" "Redis"
    apply_manifests "$K8S_DIR/livekit" "LiveKit"
    apply_manifests "$K8S_DIR/bridge-agent" "Bridge Agent"

    echo ""
    echo "========================================="
    echo "  Deployment Complete"
    echo "========================================="
    echo ""
    echo "  Check status: ./infra/polyphon/status.sh"
    echo ""
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
