#!/usr/bin/env bash
#
# scripts/k3d/seed-secrets.sh
# ============================
#
# Seed the k8s Secrets that the local k3d overlay requires.
# Replaces the cloud paths that staging/prod use (ESO + Azure Key Vault):
#
#   memql-secrets          -- main app envelope (MEMQL_MASTER_KEY,
#                             MEMQL_GENESIS_B64, DATABASE_DSN, ...)
#   livekit-secrets        -- LiveKit API key + secret for local livekit
#   memql-local-db-creds   -- Postgres credentials for the in-cluster DB
#
# Called by `make k3d-secrets` and by `make up` on first boot.
# Safe to re-run: uses `kubectl apply` (idempotent, creates or updates).
#
# PREREQUISITES
#   - kubectl context points at the k3d cluster (k3d-memql or equivalent).
#   - The memql namespace already exists (created by ArgoCD / `make up`).
#   - ~/.memql/genesis.znas exists (the sealed genesis envelope) -- OR
#     MEMQL_GENESIS_B64 is already in the environment.
#   - MEMQL_MASTER_KEY is in the environment (or falls back to a dev default).
#
# The Azurite connection string is always the well-known Azurite dev constant
# (account: devstoreaccount1, key: Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq
# /K1SZFPTOtr/KBHBeksoGMGw==).  It is not secret but lives in memql-secrets
# so the blob integration reads it via the existing genesis envelope path.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -euo pipefail.

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"

# Azurite well-known dev account + key (not secret; standard Azurite default).
AZURITE_ACCOUNT="devstoreaccount1"
AZURITE_KEY="Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
AZURITE_CONN="DefaultEndpointsProtocol=http;AccountName=${AZURITE_ACCOUNT};AccountKey=${AZURITE_KEY};BlobEndpoint=http://azurite:10000/${AZURITE_ACCOUNT};"

# Default dev DB credentials (override via env for security-conscious setups).
LOCAL_DB_USER="${MEMQL_LOCAL_DB_USER:-memql}"
LOCAL_DB_PASSWORD="${MEMQL_LOCAL_DB_PASSWORD:-memql_dev}"
LOCAL_DB_NAME="${MEMQL_LOCAL_DB_NAME:-memql}"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { echo "INFO:  $*"; }
function warn()  { echo "WARN:  $*"; }
function error() { echo "ERROR: $*" >&2; }

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v kubectl &> /dev/null; then
        error "kubectl is required but not found. Install kubectl first."
        exit 1
    fi
    if ! kubectl cluster-info &> /dev/null; then
        error "kubectl cannot reach the cluster. Run 'make up' first."
        exit 1
    fi
    # Ensure the namespace exists before creating secrets.
    if ! kubectl get namespace "$NAMESPACE" &> /dev/null; then
        error "namespace $NAMESPACE does not exist. Run 'make up' first."
        exit 1
    fi
}

#=============================================================================
# RESOLVE GENESIS B64
#=============================================================================

function resolve_genesis_b64() {
    if [ -n "${MEMQL_GENESIS_B64:-}" ]; then
        echo "$MEMQL_GENESIS_B64"
        return
    fi
    local genesis_file="${MEMQL_GENESIS_FILE:-$HOME/.memql/genesis.znas}"
    if [ -f "$genesis_file" ]; then
        base64 < "$genesis_file"
        return
    fi
    warn "no genesis file at $genesis_file and MEMQL_GENESIS_B64 is unset"
    warn "memql-secrets will be seeded WITHOUT the genesis envelope."
    warn "identity will fail to start without MEMQL_GENESIS_B64."
    echo ""
}

#=============================================================================
# RESOLVE MASTER KEY
#=============================================================================

function resolve_master_key() {
    if [ -n "${MEMQL_MASTER_KEY:-}" ]; then
        echo "$MEMQL_MASTER_KEY"
        return
    fi
    warn "MEMQL_MASTER_KEY is unset; using insecure dev placeholder."
    warn "Override by setting MEMQL_MASTER_KEY in your environment."
    echo "local-dev-placeholder-not-for-production"
}

#=============================================================================
# LOCAL DB CREDENTIALS SECRET
#=============================================================================

function seed_db_creds() {
    info "seeding memql-local-db-creds (Postgres credentials for in-cluster DB)..."
    kubectl create secret generic memql-local-db-creds \
        --namespace="$NAMESPACE" \
        --from-literal="POSTGRES_USER=$LOCAL_DB_USER" \
        --from-literal="POSTGRES_PASSWORD=$LOCAL_DB_PASSWORD" \
        --from-literal="POSTGRES_DB=$LOCAL_DB_NAME" \
        --dry-run=client -o yaml \
        | kubectl apply -f -
    info "memql-local-db-creds seeded."
}

#=============================================================================
# MAIN MEMQL SECRET
#=============================================================================

function seed_memql_secrets() {
    local genesis_b64 master_key db_dsn db_direct_dsn
    genesis_b64="$(resolve_genesis_b64)"
    master_key="$(resolve_master_key)"
    # Database DSN: local in-cluster Postgres.
    # The connection string uses 'disable' sslmode because the local Postgres
    # container does not have TLS configured (dev only).
    db_dsn="postgres://${LOCAL_DB_USER}:${LOCAL_DB_PASSWORD}@postgres:5432/${LOCAL_DB_NAME}?sslmode=disable"
    # For local, the direct DSN is the same as the pooler DSN (no PgBouncer).
    db_direct_dsn="$db_dsn"

    info "seeding memql-secrets (main app envelope)..."
    kubectl create secret generic memql-secrets \
        --namespace="$NAMESPACE" \
        --from-literal="MEMQL_MASTER_KEY=$master_key" \
        --from-literal="MEMQL_GENESIS_B64=$genesis_b64" \
        --from-literal="MEMORY_NODES_DATABASE_DSN=$db_dsn" \
        --from-literal="MEMORY_NODES_DATABASE_DIRECT_DSN=$db_direct_dsn" \
        --from-literal="AZURE_BLOB_CONNECTION_STRING=$AZURITE_CONN" \
        --dry-run=client -o yaml \
        | kubectl apply -f -
    info "memql-secrets seeded."
}

#=============================================================================
# LIVEKIT SECRETS
#=============================================================================

function seed_livekit_secrets() {
    # For local dev, use a fixed API key/secret pair that matches the
    # livekit.yaml config. The livekit server reads its keys from
    # LIVEKIT_KEYS="<key>:<secret>" env (set by this secret via envFrom).
    # These values are non-production dev placeholders.
    local lk_key="${LIVEKIT_API_KEY:-local-livekit-key}"
    local lk_secret="${LIVEKIT_API_SECRET:-local-livekit-secret-dev-placeholder}"

    info "seeding livekit-secrets (LiveKit API credentials for local livekit)..."
    kubectl create secret generic livekit-secrets \
        --namespace="$NAMESPACE" \
        --from-literal="POLYPHON_LIVEKIT_API_KEY=$lk_key" \
        --from-literal="POLYPHON_LIVEKIT_API_SECRET=$lk_secret" \
        --from-literal="LIVEKIT_API_KEY=$lk_key" \
        --from-literal="LIVEKIT_API_SECRET=$lk_secret" \
        --from-literal="LIVEKIT_KEYS=${lk_key}:${lk_secret}" \
        --dry-run=client -o yaml \
        | kubectl apply -f -
    info "livekit-secrets seeded."
}

#=============================================================================
# TELEPHONY SECRETS (local stub -- telephony disabled locally)
#=============================================================================

function seed_telephony_secrets() {
    # Telephony (Telnyx) is not used locally. Create a stub secret so pods
    # that mount telephony-secrets (livekit-sip via externalsecret ref) don't
    # crash on missing secret -- even though the ExternalSecret itself is
    # deleted in the local overlay (#2064), the livekit-sip Deployment
    # references the Secret directly.
    info "seeding telephony-secrets (stub -- telephony disabled locally)..."
    kubectl create secret generic telephony-secrets \
        --namespace="$NAMESPACE" \
        --from-literal="TELNYX_API_KEY=disabled" \
        --from-literal="TELNYX_CONNECTION_ID=disabled" \
        --from-literal="TELNYX_OUTBOUND_PROFILE_ID=disabled" \
        --dry-run=client -o yaml \
        | kubectl apply -f -
    info "telephony-secrets seeded (stub)."
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Seed k8s Secrets for the local k3d overlay. Safe to re-run (idempotent).

Options:
    --namespace=NS       k8s namespace to seed into (default: $NAMESPACE)
    --help               Show this help.

Environment:
    MEMQL_GENESIS_B64      Base64-encoded sealed genesis envelope (preferred).
    MEMQL_GENESIS_FILE     Path to genesis file (default: ~/.memql/genesis.znas).
    MEMQL_MASTER_KEY       Master key for genesis decryption.
    MEMQL_LOCAL_DB_USER    Postgres user (default: memql).
    MEMQL_LOCAL_DB_PASSWORD Postgres password (default: memql_dev).
    LIVEKIT_API_KEY        LiveKit API key (default: local-livekit-key).
    LIVEKIT_API_SECRET     LiveKit API secret (default: placeholder).
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --namespace=*) NAMESPACE="${1#*=}"; shift ;;
            --help)        show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 2 ;;
        esac
    done
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    seed_db_creds
    seed_memql_secrets
    seed_livekit_secrets
    seed_telephony_secrets
    echo ""
    info "All local secrets seeded. The k3d cluster can now start the memQL stack."
    info "ArgoCD reconciles automatically; check: kubectl get app memql-local -n argocd -w"
}

main "$@"
