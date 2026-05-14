#!/bin/bash
set -e

# setup-cluster.sh — Provision GKE cluster for Polyphon multi-agent voice
#
# Creates a GKE Standard cluster with one always-on CPU node pool that
# hosts LiveKit, Bridge Agent, and Redis. ASR/TTS runs against a cloud
# API (OpenAI today; Deepgram in Stage 2 of the Deepgram migration), so
# no GPU node pool is provisioned.
#
# Usage:
#   ./infra/polyphon/setup-cluster.sh [--dry-run]
#
# Prerequisites:
#   - gcloud CLI authenticated

#=============================================================================
# CONFIGURATION
#=============================================================================

PROJECT_ID="fast-fire-486523-f3"
REGION="us-central1"
ZONE="us-central1-a"
CLUSTER_NAME="polyphon"
NETWORK="default"

# CPU node pool (always-on)
CPU_POOL_NAME="cpu-pool"
CPU_MACHINE_TYPE="e2-standard-4"    # 4 vCPU, 16 GB RAM
CPU_MIN_NODES=1
CPU_MAX_NODES=3
CPU_DISK_SIZE="50"

DRY_RUN=false

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Provisions a GKE cluster for Polyphon multi-agent voice.

Options:
    --dry-run     Show what would be created without executing
    --help        Show this help message

Configuration:
    Project:      $PROJECT_ID
    Region:       $REGION
    Cluster:      $CLUSTER_NAME
    CPU pool:     $CPU_MACHINE_TYPE (${CPU_MIN_NODES}-${CPU_MAX_NODES} nodes)

Estimated cost:
    Idle:         ~\$122/month (GKE mgmt + CPU nodes)
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

function check_prerequisites() {
    if ! command -v gcloud &> /dev/null; then
        echo "ERROR: gcloud CLI is not installed"
        exit 1
    fi

    # Verify authentication
    if ! gcloud auth list --filter=status:ACTIVE --format="value(account)" 2>/dev/null | head -1 | grep -q "."; then
        echo "ERROR: Not authenticated with gcloud. Run: gcloud auth login"
        exit 1
    fi

    echo "[OK] Prerequisites verified"
}

function set_project() {
    echo "[INFO] Setting project to $PROJECT_ID"
    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] gcloud config set project $PROJECT_ID"
        return
    fi
    gcloud config set project "$PROJECT_ID"
}

function enable_apis() {
    echo "[INFO] Enabling required APIs..."

    APIS=(
        "container.googleapis.com"          # GKE
        "compute.googleapis.com"            # Compute Engine (nodes)
    )

    for api in "${APIS[@]}"; do
        if [ "$DRY_RUN" = true ]; then
            echo "[DRY RUN] gcloud services enable $api"
        else
            gcloud services enable "$api" --quiet
        fi
    done

    echo "[OK] APIs enabled"
}

function create_cluster() {
    echo "[INFO] Creating GKE cluster: $CLUSTER_NAME"
    echo "  Region:  $REGION"
    echo "  Network: $NETWORK"
    echo ""

    # Check if cluster already exists
    if gcloud container clusters describe "$CLUSTER_NAME" --region="$REGION" &>/dev/null 2>&1; then
        echo "[INFO] Cluster $CLUSTER_NAME already exists, skipping creation"
        return
    fi

    CMD=(
        gcloud container clusters create "$CLUSTER_NAME"
        --region="$REGION"
        --network="$NETWORK"
        --release-channel=regular
        --enable-ip-alias
        --num-nodes=0                       # No default pool; we create custom pools
        --logging=SYSTEM,WORKLOAD
        --monitoring=SYSTEM
        --workload-pool="${PROJECT_ID}.svc.id.goog"
    )

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] ${CMD[*]}"
    else
        "${CMD[@]}"
    fi

    echo "[OK] Cluster created"
}

function create_cpu_pool() {
    echo "[INFO] Creating CPU node pool: $CPU_POOL_NAME"
    echo "  Machine:  $CPU_MACHINE_TYPE"
    echo "  Nodes:    ${CPU_MIN_NODES}-${CPU_MAX_NODES}"
    echo ""

    CMD=(
        gcloud container node-pools create "$CPU_POOL_NAME"
        --cluster="$CLUSTER_NAME"
        --region="$REGION"
        --machine-type="$CPU_MACHINE_TYPE"
        --disk-size="${CPU_DISK_SIZE}GB"
        --num-nodes="$CPU_MIN_NODES"
        --min-nodes="$CPU_MIN_NODES"
        --max-nodes="$CPU_MAX_NODES"
        --enable-autoscaling
        --node-labels="polyphon-role=cpu"
    )

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] ${CMD[*]}"
    else
        "${CMD[@]}"
    fi

    echo "[OK] CPU node pool created"
}

function create_namespace() {
    echo "[INFO] Creating polyphon namespace..."

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN] kubectl create namespace polyphon"
    else
        gcloud container clusters get-credentials "$CLUSTER_NAME" --region="$REGION"
        kubectl create namespace polyphon --dry-run=client -o yaml | kubectl apply -f -
    fi

    echo "[OK] Namespace created"
}

function print_summary() {
    echo ""
    echo "========================================="
    echo "  Polyphon GKE Cluster Setup Complete"
    echo "========================================="
    echo ""
    echo "  Cluster:    $CLUSTER_NAME"
    echo "  Region:     $REGION"
    echo "  Project:    $PROJECT_ID"
    echo ""
    echo "  CPU Pool:   $CPU_POOL_NAME ($CPU_MACHINE_TYPE)"
    echo "              ${CPU_MIN_NODES}-${CPU_MAX_NODES} nodes (always-on)"
    echo ""
    echo "  Next steps:"
    echo "    1. Deploy services: ./infra/polyphon/deploy.sh"
    echo "    2. Check status:    ./infra/polyphon/status.sh"
    echo ""
    echo "  Connect to cluster:"
    echo "    gcloud container clusters get-credentials $CLUSTER_NAME --region=$REGION"
    echo ""
}

function main() {
    parse_arguments "$@"

    echo "========================================="
    echo "  Polyphon GKE Cluster Setup"
    echo "========================================="
    echo ""

    if [ "$DRY_RUN" = true ]; then
        echo "[DRY RUN MODE] No changes will be made"
        echo ""
    fi

    check_prerequisites
    set_project
    enable_apis
    create_cluster
    create_cpu_pool
    create_namespace
    print_summary
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
